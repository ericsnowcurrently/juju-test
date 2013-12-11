// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package apiserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"

	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/storage"
	"launchpad.net/juju-core/names"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api/params"
	"launchpad.net/juju-core/state/apiserver/common"
)

// charmsHandler handles charm upload through HTTPS in the API server.
type charmsHandler struct {
	state *state.State
}

// CharmsResponse is the server response to a charm upload request.
type CharmsResponse struct {
	Error    string `json:"error,omitempty"`
	CharmURL string `json:"charmUrl,omitempty"`
}

func (h *charmsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.authenticate(r); err != nil {
		h.authError(w)
		return
	}

	switch r.Method {
	case "POST":
		charmUrl, err := h.processPost(r)
		if err != nil {
			h.sendError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.sendJSON(w, http.StatusOK, &CharmsResponse{CharmURL: charmUrl.String()})
	// Possible future extensions, like GET.
	default:
		h.sendError(w, http.StatusMethodNotAllowed, fmt.Sprintf("unsupported method: %q", r.Method))
	}
}

// sendJSON sends a JSON-encoded response to the client.
func (h *charmsHandler) sendJSON(w http.ResponseWriter, statusCode int, response *CharmsResponse) error {
	w.WriteHeader(statusCode)
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	w.Write(body)
	return nil
}

// sendError sends a JSON-encoded error response.
func (h *charmsHandler) sendError(w http.ResponseWriter, statusCode int, message string) error {
	return h.sendJSON(w, statusCode, &CharmsResponse{Error: message})
}

// authenticate parses HTTP basic authentication and authorizes the
// request by looking up the provided tag and password against state.
func (h *charmsHandler) authenticate(r *http.Request) error {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || parts[0] != "Basic" {
		// Invalid header format or no header provided.
		return fmt.Errorf("invalid request format")
	}
	// Challenge is a base64-encoded "tag:pass" string.
	// See RFC 2617, Section 2.
	challenge, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("invalid request format")
	}
	tagPass := strings.SplitN(string(challenge), ":", 2)
	if len(tagPass) != 2 {
		return fmt.Errorf("invalid request format")
	}
	entity, err := checkCreds(h.state, params.Creds{
		AuthTag:  tagPass[0],
		Password: tagPass[1],
	})
	if err != nil {
		return err
	}
	// Only allow users, not agents.
	_, _, err = names.ParseTag(entity.Tag(), names.UserTagKind)
	if err != nil {
		return common.ErrBadCreds
	}
	return err
}

// authError sends an unauthorized error.
func (h *charmsHandler) authError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="juju"`)
	h.sendError(w, http.StatusUnauthorized, "unauthorized")
}

// processPost handles a charm upload POST request after authentication.
func (h *charmsHandler) processPost(r *http.Request) (*charm.URL, error) {
	query := r.URL.Query()
	series := query.Get("series")
	if series == "" {
		return nil, fmt.Errorf("expected series= URL argument")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	// Get the first (and hopefully only) uploaded part to process.
	part, err := reader.NextPart()
	if err == io.EOF {
		return nil, fmt.Errorf("expected a single uploaded file, got none")
	} else if err != nil {
		return nil, fmt.Errorf("cannot process uploaded file: %v", err)
	}
	// Make sure the content type is zip.
	contentType := part.Header.Get("Content-Type")
	if contentType != "application/zip" {
		return nil, fmt.Errorf("expected Content-Type: application/zip, got: %v", contentType)
	}
	tempFile, err := ioutil.TempFile("", "charm")
	if err != nil {
		return nil, fmt.Errorf("cannot create temp file: %v", err)
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())
	if _, err := io.Copy(tempFile, part); err != nil {
		return nil, fmt.Errorf("error processing file upload: %v", err)
	}
	if _, err := reader.NextPart(); err != io.EOF {
		return nil, fmt.Errorf("expected a single uploaded file, got more")
	}
	archive, err := charm.ReadBundle(tempFile.Name())
	if err != nil {
		return nil, fmt.Errorf("invalid charm archive: %v", err)
	}
	// We got it, now let's reserve a charm URL for it in state.
	archiveUrl := &charm.URL{
		Schema:   "local",
		Series:   series,
		Name:     archive.Meta().Name,
		Revision: archive.Revision(),
	}
	preparedUrl, err := h.state.PrepareLocalCharmUpload(archiveUrl)
	if err != nil {
		return nil, err
	}
	// Now we need to repackage it with the reserved URL, upload it to
	// provider storage and update the state.
	err = h.repackageAndUploadCharm(archive, preparedUrl)
	if err != nil {
		return nil, err
	}
	// All done.
	return preparedUrl, nil
}

// repackageAndUploadCharm expands the given charm archive to a
// temporary directoy, repackages it with the given curl's revision,
// then uploads it to providr storage, and finally updates the state.
func (h *charmsHandler) repackageAndUploadCharm(archive *charm.Bundle, curl *charm.URL) error {
	// Create a temp dir and file to use below.
	tempDir, err := ioutil.TempDir("", archive.Meta().Name)
	if err != nil {
		return fmt.Errorf("cannot create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)
	tempFile, err := ioutil.TempFile("", archive.Meta().Name)
	if err != nil {
		return fmt.Errorf("cannot create temp file: %v", err)
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	// Expand and repack it with the revision specified by curl.
	archive.SetRevision(curl.Revision)
	if err := archive.ExpandTo(tempDir); err != nil {
		return fmt.Errorf("cannot extract uploaded charm: %v", err)
	}
	charmDir, err := charm.ReadDir(tempDir)
	if err != nil {
		return fmt.Errorf("cannot read extracted charm: %v", err)
	}
	// Bundle the charm and calculate its sha256 hash at the
	// same time.
	hash := sha256.New()
	multiWriter := io.MultiWriter(hash, tempFile)
	if err := charmDir.BundleTo(multiWriter); err != nil {
		return fmt.Errorf("cannot repackage uploaded charm: %v", err)
	}
	bundleSha256 := hex.EncodeToString(hash.Sum(nil))
	size, err := tempFile.Seek(0, 2)
	if err != nil {
		return fmt.Errorf("cannot get charm file size: %v", err)
	}
	// Seek to the beginning so the subsequent Put will read
	// the whole file again.
	if _, err := tempFile.Seek(0, 0); err != nil {
		return fmt.Errorf("cannot rewind the charm file reader: %v", err)
	}

	// Now upload to provider storage.
	storage, err := getEnvironStorage(h.state)
	if err != nil {
		return fmt.Errorf("cannot access provider storage: %v", err)
	}
	name := charm.Quote(curl.String())
	if err := storage.Put(name, tempFile, size); err != nil {
		return fmt.Errorf("cannot upload charm to provider storage: %v", err)
	}
	storageUrl, err := storage.URL(name)
	if err != nil {
		return fmt.Errorf("cannot get storage URL for charm: %v", err)
	}
	bundleURL, err := url.Parse(storageUrl)
	if err != nil {
		return fmt.Errorf("cannot parse storage URL: %v", err)
	}

	// And finally, update state.
	_, err = h.state.UpdateUploadedCharm(archive, curl, bundleURL, bundleSha256)
	if err != nil {
		return fmt.Errorf("cannot update uploaded charm in state: %v", err)
	}
	return nil
}

// getEnvironStorage creates an Environ from the config in state and
// returns its storage interface.
func getEnvironStorage(st *state.State) (storage.Storage, error) {
	envConfig, err := st.EnvironConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot get environment config: %v", err)
	}
	env, err := environs.New(envConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot access environment: %v", err)
	}
	return env.Storage(), nil
}
