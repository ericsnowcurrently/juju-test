package environs_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/dummy"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/testing"
	"launchpad.net/juju-core/version"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type ToolsSuite struct {
	env environs.Environ
	testing.LoggingSuite
	oldVarDir string
}

func (t *ToolsSuite) SetUpTest(c *C) {
	t.LoggingSuite.SetUpTest(c)
	env, err := environs.NewFromAttrs(map[string]interface{}{
		"name":            "test",
		"type":            "dummy",
		"zookeeper":       false,
		"authorized-keys": "i-am-a-key",
	})
	c.Assert(err, IsNil)
	t.env = env
	t.oldVarDir = environs.VarDir
	environs.VarDir = c.MkDir()
}

func (t *ToolsSuite) TearDownTest(c *C) {
	environs.VarDir = t.oldVarDir
	dummy.Reset()
	t.LoggingSuite.TearDownTest(c)
}

var envs *environs.Environs

func toolsStoragePath(vers string) string {
	return environs.ToolsStoragePath(version.Binary{
		Number: version.MustParse(vers),
		Series: version.Current.Series,
		Arch:   version.Current.Arch,
	})
}

var _ = Suite(&ToolsSuite{})

const urlFile = "downloaded-url.txt"

var commandTests = []struct {
	cmd    []string
	output string
}{
	{
		[]string{"juju", "arble"},
		"error: unrecognized command: juju arble\n",
	}, {
		[]string{"jujud", "arble"},
		"error: unrecognized command: jujud arble\n",
	}, {
		[]string{"jujuc"},
		"(.|\n)*error: jujuc should not be called directly\n",
	},
}

func (t *ToolsSuite) TestPutGetTools(c *C) {
	tools, err := environs.PutTools(t.env.Storage(), nil)
	c.Assert(err, IsNil)
	c.Assert(tools.Binary, Equals, version.Current)
	c.Assert(tools.URL, Not(Equals), "")

	for i, get := range []func(t *state.Tools) error{
		getTools,
		getToolsWithTar,
	} {
		c.Logf("test %d", i)
		// Unarchive the tool executables into a temp directory.
		environs.VarDir = c.MkDir()
		err = get(tools)
		c.Assert(err, IsNil)

		dir := environs.ToolsDir(version.Current)
		// Verify that each tool executes and produces some
		// characteristic output.
		for i, test := range commandTests {
			c.Logf("command test %d", i)
			out, err := exec.Command(filepath.Join(dir, test.cmd[0]), test.cmd[1:]...).CombinedOutput()
			if err != nil {
				c.Assert(err, FitsTypeOf, (*exec.ExitError)(nil))
			}
			c.Check(string(out), Matches, test.output)
		}
		data, err := ioutil.ReadFile(filepath.Join(dir, urlFile))
		c.Assert(err, IsNil)
		c.Assert(string(data), Equals, tools.URL)
	}
}

func (t *ToolsSuite) TestPutToolsAndForceVersion(c *C) {
	// This test actually tests three things:
	//   the writing of the FORCE-VERSION file;
	//   the reading of the FORCE-VERSION file by the version package;
	//   and the reading of the version from jujud.
	vers := version.Current
	vers.Patch++
	tools, err := environs.PutTools(t.env.Storage(), &vers)
	c.Assert(err, IsNil)
	c.Assert(tools.Binary, Equals, vers)
}

// Test that the upload procedure fails correctly
// when the build process fails (because of a bad Go source
// file in this case).
func (t *ToolsSuite) TestUploadBadBuild(c *C) {
	gopath := c.MkDir()
	join := append([]string{gopath, "src"}, strings.Split("launchpad.net/juju-core/cmd/broken", "/")...)
	pkgdir := filepath.Join(join...)
	err := os.MkdirAll(pkgdir, 0777)
	c.Assert(err, IsNil)

	err = ioutil.WriteFile(filepath.Join(pkgdir, "broken.go"), []byte("nope"), 0666)
	c.Assert(err, IsNil)

	defer os.Setenv("GOPATH", os.Getenv("GOPATH"))
	os.Setenv("GOPATH", gopath)

	tools, err := environs.PutTools(t.env.Storage(), nil)
	c.Assert(tools, IsNil)
	c.Assert(err, ErrorMatches, `build failed: exit status 1; can't load package:(.|\n)*`)
}

var unpackToolsBadDataTests = []struct {
	data []byte
	err  string
}{
	{
		testing.TarGz(testing.NewTarFile("bar", os.ModeDir, "")),
		"bad file type.*",
	}, {
		testing.TarGz(testing.NewTarFile("../../etc/passwd", 0755, "")),
		"bad name.*",
	}, {
		testing.TarGz(testing.NewTarFile(`\ini.sys`, 0755, "")),
		"bad name.*",
	}, {
		[]byte("x"),
		"unexpected EOF",
	}, {
		gzyesses,
		"archive/tar: invalid tar header",
	},
}

func (t *ToolsSuite) TestUnpackToolsBadData(c *C) {
	for i, test := range unpackToolsBadDataTests {
		c.Logf("test %d", i)
		tools := &state.Tools{
			URL:    "http://foo/bar",
			Binary: version.MustParseBinary("1.2.3-foo-bar"),
		}
		err := environs.UnpackTools(tools, bytes.NewReader(test.data))
		c.Assert(err, ErrorMatches, test.err)
		assertDirNames(c, toolsDir(), []string{})
	}
}

func toolsDir() string {
	return filepath.Join(environs.VarDir, "tools")
}

func (t *ToolsSuite) TestUnpackToolsContents(c *C) {
	files := []*testing.TarFile{
		testing.NewTarFile("bar", 0755, "bar contents"),
		testing.NewTarFile("foo", 0755, "foo contents"),
	}
	tools := &state.Tools{
		URL:    "http://foo/bar",
		Binary: version.MustParseBinary("1.2.3-foo-bar"),
	}

	err := environs.UnpackTools(tools, bytes.NewReader(testing.TarGz(files...)))
	c.Assert(err, IsNil)
	assertDirNames(c, toolsDir(), []string{"1.2.3-foo-bar"})
	assertToolsContents(c, tools, files)

	// Try to unpack the same version of tools again - it should succeed,
	// leaving the original version around.
	tools2 := &state.Tools{
		URL:    "http://arble",
		Binary: version.MustParseBinary("1.2.3-foo-bar"),
	}
	files2 := []*testing.TarFile{
		testing.NewTarFile("bar", 0755, "bar2 contents"),
		testing.NewTarFile("x", 0755, "x contents"),
	}
	err = environs.UnpackTools(tools2, bytes.NewReader(testing.TarGz(files2...)))
	c.Assert(err, IsNil)
	assertDirNames(c, toolsDir(), []string{"1.2.3-foo-bar"})
	assertToolsContents(c, tools, files)
}

func (t *ToolsSuite) TestReadToolsErrors(c *C) {
	vers := version.MustParseBinary("1.2.3-precise-amd64")
	tools, err := environs.ReadTools(vers)
	c.Assert(tools, IsNil)
	c.Assert(err, ErrorMatches, "cannot read URL in tools directory: .*")

	dir := environs.ToolsDir(vers)
	err = os.MkdirAll(dir, 0755)
	c.Assert(err, IsNil)

	err = ioutil.WriteFile(filepath.Join(dir, urlFile), []byte(" \t\n"), 0644)
	c.Assert(err, IsNil)

	tools, err = environs.ReadTools(vers)
	c.Assert(tools, IsNil)
	c.Assert(err, ErrorMatches, "empty URL in tools directory.*")
}

func (t *ToolsSuite) TestToolsStoragePath(c *C) {
	c.Assert(environs.ToolsStoragePath(binaryVersion("1.2.3-precise-amd64")),
		Equals, "tools/juju-1.2.3-precise-amd64.tgz")
}

func (t *ToolsSuite) TestToolsDir(c *C) {
	environs.VarDir = "/var/lib/juju"
	c.Assert(environs.ToolsDir(binaryVersion("1.2.3-precise-amd64")),
		Equals,
		"/var/lib/juju/tools/1.2.3-precise-amd64")
}

// getTools downloads and unpacks the given tools.
func getTools(tools *state.Tools) error {
	resp, err := http.Get(tools.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad http status: %v", resp.Status)
	}
	return environs.UnpackTools(tools, resp.Body)
}

// getToolsWithTar is the same as getTools but uses tar
// itself so we're not just testing the Go tar package against
// itself.
func getToolsWithTar(tools *state.Tools) error {
	resp, err := http.Get(tools.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dir := environs.ToolsDir(tools.Binary)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}

	cmd := exec.Command("tar", "xz")
	cmd.Dir = dir
	cmd.Stdin = resp.Body
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tar extract failed: %s", out)
	}
	return ioutil.WriteFile(filepath.Join(cmd.Dir, urlFile), []byte(tools.URL), 0644)
}

// assertToolsContents asserts that the directory for the tools
// has the given contents.
func assertToolsContents(c *C, tools *state.Tools, files []*testing.TarFile) {
	var wantNames []string
	for _, f := range files {
		wantNames = append(wantNames, f.Header.Name)
	}
	wantNames = append(wantNames, urlFile)
	dir := environs.ToolsDir(tools.Binary)
	assertDirNames(c, dir, wantNames)
	assertFileContents(c, dir, urlFile, tools.URL, 0200)
	for _, f := range files {
		assertFileContents(c, dir, f.Header.Name, f.Contents, 0400)
	}
	gotTools, err := environs.ReadTools(tools.Binary)
	c.Assert(err, IsNil)
	c.Assert(*gotTools, Equals, *tools)
}

// assertFileContents asserts that the given file in the
// given directory has the given contents.
func assertFileContents(c *C, dir, file, contents string, mode os.FileMode) {
	file = filepath.Join(dir, file)
	info, err := os.Stat(file)
	c.Assert(err, IsNil)
	c.Assert(info.Mode()&(os.ModeType|mode), Equals, mode)
	data, err := ioutil.ReadFile(file)
	c.Assert(err, IsNil)
	c.Assert(string(data), Equals, contents)
}

// assertDirNames asserts that the given directory
// holds the given file or directory names.
func assertDirNames(c *C, dir string, names []string) {
	f, err := os.Open(dir)
	c.Assert(err, IsNil)
	defer f.Close()
	dnames, err := f.Readdirnames(0)
	c.Assert(err, IsNil)
	sort.Strings(dnames)
	sort.Strings(names)
	c.Assert(dnames, DeepEquals, names)
}

func (t *ToolsSuite) TestChangeAgentTools(c *C) {
	files := []*testing.TarFile{
		testing.NewTarFile("jujuc", 0755, "juju executable"),
		testing.NewTarFile("jujud", 0755, "jujuc executable"),
	}
	tools := &state.Tools{
		URL:    "http://foo/bar1",
		Binary: version.MustParseBinary("1.2.3-foo-bar"),
	}
	err := environs.UnpackTools(tools, bytes.NewReader(testing.TarGz(files...)))
	c.Assert(err, IsNil)

	gotTools, err := environs.ChangeAgentTools("testagent", tools.Binary)
	c.Assert(err, IsNil)
	c.Assert(*gotTools, Equals, *tools)

	assertDirNames(c, toolsDir(), []string{"1.2.3-foo-bar", "testagent"})
	assertDirNames(c, environs.AgentToolsDir("testagent"), []string{"jujuc", "jujud", urlFile})

	// Upgrade again to check that the link replacement logic works ok.
	files2 := []*testing.TarFile{
		testing.NewTarFile("foo", 0755, "foo content"),
		testing.NewTarFile("bar", 0755, "bar content"),
	}
	tools2 := &state.Tools{
		URL:    "http://foo/bar2",
		Binary: version.MustParseBinary("1.2.4-foo-bar"),
	}
	err = environs.UnpackTools(tools2, bytes.NewReader(testing.TarGz(files2...)))
	c.Assert(err, IsNil)

	gotTools, err = environs.ChangeAgentTools("testagent", tools2.Binary)
	c.Assert(err, IsNil)
	c.Assert(*gotTools, Equals, *tools2)

	assertDirNames(c, toolsDir(), []string{"1.2.3-foo-bar", "1.2.4-foo-bar", "testagent"})
	assertDirNames(c, environs.AgentToolsDir("testagent"), []string{"foo", "bar", urlFile})
}

// gzyesses holds the result of running:
// yes | head -17000 | gzip
var gzyesses = []byte{
	0x1f, 0x8b, 0x08, 0x00, 0x29, 0xae, 0x1a, 0x50,
	0x00, 0x03, 0xed, 0xc2, 0x31, 0x0d, 0x00, 0x00,
	0x00, 0x02, 0xa0, 0xdf, 0xc6, 0xb6, 0xb7, 0x87,
	0x63, 0xd0, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x38, 0x31, 0x53, 0xad, 0x03,
	0x8d, 0xd0, 0x84, 0x00, 0x00,
}

type toolsSpec struct {
	version string
	os      string
	arch    string
}

var findToolsTests = []struct {
	version        version.Number // version to assume is current for the test.
	contents       []string       // names in private storage.
	publicContents []string       // names in public storage.
	expect         string         // the name we expect to find (if no error).
	err            string         // the error we expect to find (if not blank).
}{{
	// current version should be satisfied by current tools path.
	version:  version.Current.Number,
	contents: []string{environs.ToolsStoragePath(version.Current)},
	expect:   environs.ToolsStoragePath(version.Current),
}, {
	// major versions don't match.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("0.0.9"),
	},
	err: "no compatible tools found",
}, {
	// major versions don't match.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("2.0.9"),
	},
	err: "no compatible tools found",
}, {
	// fall back to public storage when nothing found in private.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("0.0.9"),
	},
	publicContents: []string{
		toolsStoragePath("1.0.0"),
	},
	expect: "public-" + toolsStoragePath("1.0.0"),
}, {
	// always use private storage in preference to public storage.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("1.0.2"),
	},
	publicContents: []string{
		toolsStoragePath("1.0.9"),
	},
	expect: toolsStoragePath("1.0.2"),
}, {
	// we'll use an earlier version if the major version number matches.
	version: version.MustParse("1.99.99"),
	contents: []string{
		toolsStoragePath("1.0.0"),
	},
	expect: toolsStoragePath("1.0.0"),
}, {
	// check that version comparing is numeric, not alphabetical.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("1.0.9"),
		toolsStoragePath("1.0.10"),
		toolsStoragePath("1.0.11"),
	},
	expect: toolsStoragePath("1.0.11"),
}, {
	// minor version wins over patch version.
	version: version.MustParse("1.0.0"),
	contents: []string{
		toolsStoragePath("1.9.11"),
		toolsStoragePath("1.10.10"),
		toolsStoragePath("1.11.9"),
	},
	expect: toolsStoragePath("1.11.9"),
}, {
	// mismatching series or architecture is ignored.
	version: version.MustParse("1.0.0"),
	contents: []string{
		environs.ToolsStoragePath(version.Binary{
			Number: version.MustParse("1.9.9"),
			Series: "foo",
			Arch:   version.Current.Arch,
		}),
		environs.ToolsStoragePath(version.Binary{
			Number: version.MustParse("1.9.9"),
			Series: version.Current.Series,
			Arch:   "foo",
		}),
		toolsStoragePath("1.0.0"),
	},
	expect: toolsStoragePath("1.0.0"),
},
}

// putNames puts a set of names into the environ's private
// and public storage. The data in the private storage is
// the name itself; in the public storage the name is preceded with "public-".
func putNames(c *C, env environs.Environ, private, public []string) {
	for _, name := range private {
		err := env.Storage().Put(name, strings.NewReader(name), int64(len(name)))
		c.Assert(err, IsNil)
	}
	// The contents of all files in the public storage is prefixed with "public-" so
	// that we can easily tell if we've got the right thing.
	for _, name := range public {
		data := "public-" + name
		err := env.PublicStorage().(environs.Storage).Put(name, strings.NewReader(data), int64(len(data)))
		c.Assert(err, IsNil)
	}
}

func (t *ToolsSuite) TestFindTools(c *C) {
	for i, tt := range findToolsTests {
		c.Logf("test %d", i)
		putNames(c, t.env, tt.contents, tt.publicContents)
		vers := version.Binary{
			Number: tt.version,
			Series: version.Current.Series,
			Arch:   version.Current.Arch,
		}
		tools, err := environs.FindTools(t.env, vers)
		if tt.err != "" {
			c.Assert(err, ErrorMatches, tt.err)
		} else {
			assertURLContents(c, tools.URL, tt.expect)
		}
		t.env.Destroy(nil)
	}
}

var setenvTests = []struct {
	set    string
	expect []string
}{
	{"foo=1", []string{"foo=1", "arble="}},
	{"foo=", []string{"foo=", "arble="}},
	{"arble=23", []string{"foo=bar", "arble=23"}},
	{"zaphod=42", []string{"foo=bar", "arble=", "zaphod=42"}},
}

func (*ToolsSuite) TestSetenv(c *C) {
	env0 := []string{"foo=bar", "arble="}
	for i, t := range setenvTests {
		c.Logf("test %d", i)
		env := make([]string, len(env0))
		copy(env, env0)
		env = environs.Setenv(env, t.set)
		c.Check(env, DeepEquals, t.expect)
	}
}

func binaryVersion(vers string) version.Binary {
	return version.MustParseBinary(vers)
}

func newTools(vers, url string) *state.Tools {
	return &state.Tools{
		Binary: binaryVersion(vers),
		URL:    url,
	}
}

func assertURLContents(c *C, url string, expect string) {
	resp, err := http.Get(url)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	c.Assert(string(data), Equals, expect)
}

var listToolsTests = []struct {
	major  int
	expect []string
}{{
	1,
	[]string{"1.2.3-precise-i386"},
}, {
	2,
	[]string{"2.2.3-precise-amd64", "2.2.3-precise-i386", "2.2.4-precise-i386"},
}, {
	3,
	[]string{"3.2.1-quantal-amd64"},
}, {
	4,
	nil,
}}

func (t *ToolsSuite) TestListTools(c *C) {
	testList := []string{
		"foo",
		"tools/.tgz",
		"tools/juju-1.2.3-precise-i386.tgz",
		"tools/juju-2.2.3-precise-amd64.tgz",
		"tools/juju-2.2.3-precise-i386.tgz",
		"tools/juju-2.2.4-precise-i386.tgz",
		"tools/juju-2.2-precise-amd64.tgz",
		"tools/juju-3.2.1-quantal-amd64.tgz",
		"xtools/juju-2.2.3-precise-amd64.tgz",
	}

	putNames(c, t.env, testList, testList)

	for i, test := range listToolsTests {
		c.Logf("test %d", i)
		toolsList, err := environs.ListTools(t.env, test.major)
		c.Assert(err, IsNil)
		c.Assert(toolsList.Private, HasLen, len(test.expect))
		c.Assert(toolsList.Public, HasLen, len(test.expect))
		for i, t := range toolsList.Private {
			vers := binaryVersion(test.expect[i])
			c.Assert(t.Binary, Equals, vers)
			assertURLContents(c, t.URL, environs.ToolsStoragePath(vers))
		}
		for i, t := range toolsList.Public {
			vers := binaryVersion(test.expect[i])
			c.Assert(t.Binary, Equals, vers)
			assertURLContents(c, t.URL, "public-"+environs.ToolsStoragePath(vers))
		}
	}
}

var bestToolsTests = []struct {
	list   *environs.ToolsList
	vers   version.Binary
	expect *state.Tools
}{{
	&environs.ToolsList{},
	binaryVersion("1.2.3-precise-amd64"),
	nil,
}, {
	&environs.ToolsList{
		Private: []*state.Tools{
			newTools("1.2.3-precise-amd64", ""),
			newTools("1.2.4-precise-amd64", ""),
			newTools("1.3.4-precise-amd64", ""),
			newTools("1.4.4-precise-i386", ""),
			newTools("1.4.5-quantal-i386", ""),
			newTools("2.2.3-precise-amd64", ""),
		},
	},
	binaryVersion("1.9.4-precise-amd64"),
	newTools("1.3.4-precise-amd64", ""),
}, {
	&environs.ToolsList{
		Private: []*state.Tools{
			newTools("1.2.3-precise-amd64", ""),
			newTools("1.2.4-precise-amd64", ""),
			newTools("1.3.4-precise-amd64", ""),
			newTools("1.4.4-precise-i386", ""),
			newTools("1.4.5-quantal-i386", ""),
			newTools("2.2.3-precise-amd64", ""),
		},
	},
	binaryVersion("2.0.0-precise-amd64"),
	newTools("2.2.3-precise-amd64", ""),
},
	{
		&environs.ToolsList{
			Private: []*state.Tools{
				newTools("1.2.3-precise-amd64", ""),
			},
			Public: []*state.Tools{
				newTools("1.2.4-precise-amd64", ""),
			},
		},
		binaryVersion("1.0.0-precise-amd64"),
		newTools("1.2.3-precise-amd64", ""),
	},
	{
		&environs.ToolsList{
			Public: []*state.Tools{
				newTools("1.2.4-precise-amd64", ""),
			},
		},
		binaryVersion("1.0.0-precise-amd64"),
		newTools("1.2.4-precise-amd64", ""),
	},
}

func (t *ToolsSuite) TestBestTools(c *C) {
	for i, t := range bestToolsTests {
		c.Logf("test %d", i)
		tools := environs.BestTools(t.list, t.vers)
		c.Assert(tools, DeepEquals, t.expect)
	}
}
