// Package zotero — write client for the #184 fix-service loop.
//
// The RAG is the ONLY gateway to Zotero: the fix-service never sees
// credentials. Writes go through the Zotero local API write surface
// (verified live against Zotero 10.0-beta: server_localAPI.js semantics).
//
// Exactly TWO mutations exist (#184 design nail 3):
//
//	DeleteAttachmentItem — remove a (quarantined-before) broken attachment
//	CreateAttachmentWithFile — 3-phase upload of the healed PDF under a
//	  SCHEMA filename ({Autor|Institution} - {Jahr} - {Titel}); there is
//	  deliberately NO filename patch.
//
// Live-probed protocol facts (do not "fix" without re-probing):
//   - writes need Zotero-Server-ID (428 without) + local API key
//     (Zotero-API-Key header or Authorization: Bearer; 401 without)
//   - GET carries Last-Modified-Version; HEAD does not
//   - file upload = authorize (form: md5 hex32, filename, filesize,
//     mtime in MILLISECONDS, If-None-Match: *) -> {url, uploadKey} |
//     {exists:1}; then multipart POST to url (field "file", 201); then
//     register (form: upload=<key>, If-None-Match: *, 204)
//   - local API keys are SINGLE-USE unless the operator picked
//     "Always Allow" in the authorize dialog (remember:true)
package zotero

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"errors"
)

// WriteClient talks to the Zotero local API with write authorization.
type WriteClient struct {
	BaseURL  string // http://localhost:23119
	ServerID string // Zotero-Server-ID header (same value the sync reader uses)
	APIKey   string // from POST /api/local/authorize; single-use unless remember
	HTTP     *http.Client
}

func NewWriteClient(baseURL, serverID, apiKey string) *WriteClient {
	return &WriteClient{BaseURL: strings.TrimRight(baseURL, "/"), ServerID: serverID, APIKey: apiKey,
		HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Authorize performs the local Key-Flow once: Zotero shows a dialog with
// Allow / Always Allow / Deny; on allow the response is
// {"key": ..., "remember": <bool>}. A non-remembered key is consumed by the
// FIRST successful write — ops should pick "Always Allow" for the loop and
// put the key into ZOTERO_WRITE_API_KEY; normal operation never calls this.
func (w *WriteClient) Authorize(appName string) (key string, remember bool, err error) {
	body, _ := json.Marshal(map[string]any{"appName": appName})
	req, _ := http.NewRequest(http.MethodPost, w.BaseURL+"/api/local/authorize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Zotero-Server-ID", w.ServerID)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("authorize: %d %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Key      string `json:"key"`
		Remember bool   `json:"remember"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Key == "" {
		return "", false, fmt.Errorf("authorize: keine key in Antwort: %s", string(raw))
	}
	return out.Key, out.Remember, nil
}

// localAuthHeaders stamps the shared local-API auth surface onto a request.
// It is the ONE auth path for write.go (the phase-2 upload request also goes
// through here, after its host check).
func (w *WriteClient) localAuthHeaders(req *http.Request) {
	req.Header.Set("Zotero-Server-ID", w.ServerID)
	req.Header.Set("Zotero-API-Version", "3")
	if w.APIKey != "" {
		req.Header.Set("Zotero-API-Key", w.APIKey)
	}
}

func (w *WriteClient) do(method, path string, headers map[string]string, body io.Reader) ([]byte, http.Header, error) {
	req, err := http.NewRequest(method, w.BaseURL+path, body)
	if err != nil {
		return nil, nil, err
	}
	w.localAuthHeaders(req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return raw, resp.Header, &StatusError{Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, resp.Header, nil
}

// IsVersionConflict reports a 412 from an If-Unmodified-Since-Version guard
// (concurrent modification — the caller may re-read and retry). errors.As so
// wrapped StatusErrors (fmt.Errorf %w) are still detected.
func IsVersionConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusPreconditionFailed
}

// ItemVersion fetches an item's current version. GET carries
// Last-Modified-Version; HEAD does not (live-probed).
func (w *WriteClient) ItemVersion(key string) (string, error) {
	_, hdr, err := w.do(http.MethodGet, "/api/users/0/items/"+key, nil, nil)
	if err != nil {
		return "", err
	}
	v := hdr.Get("Last-Modified-Version")
	if v == "" {
		return "", fmt.Errorf("item %s: keine Last-Modified-Version", key)
	}
	return v, nil
}

// Mutation 1: delete an attachment item (the original was quarantined by
// the caller BEFORE this call — quarantine-first is a design nail).
func (w *WriteClient) DeleteAttachmentItem(key string) error {
	ver, err := w.ItemVersion(key)
	if err != nil {
		return err
	}
	_, _, err = w.do(http.MethodDelete, "/api/users/0/items/"+key,
		map[string]string{"If-Unmodified-Since-Version": ver}, nil)
	return err
}

// Mutation 2: create an attachment item WITH a file, live-probed 3-phase
// flow. parentKey is the document item ("" = standalone). filename MUST
// come from the schema builder — callers cannot patch filenames because no
// such mutation exists. Returns the new attachment item key.
func (w *WriteClient) CreateAttachmentWithFile(parentKey, filename string, pdf []byte) (string, error) {
	// Phase 0 — the attachment item.
	item := map[string]any{
		"itemType":    "attachment",
		"linkMode":    "imported_file",
		"title":       filename,
		"contentType": "application/pdf",
		"filename":    filename,
		"tags":        []map[string]string{{"tag": "axiom-repair"}},
	}
	if parentKey != "" {
		item["parentItem"] = parentKey
	}
	itemJSON, _ := json.Marshal([]any{item})
	raw, _, err := w.do(http.MethodPost, "/api/users/0/items",
		map[string]string{"Content-Type": "application/json"}, bytes.NewReader(itemJSON))
	if err != nil {
		return "", fmt.Errorf("create attachment item: %w", err)
	}
	var created struct {
		Successful map[string]struct {
			Key string `json:"key"`
		} `json:"successful"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || len(created.Successful) == 0 {
		return "", fmt.Errorf("create attachment item: unerwartete Antwort %s", string(raw))
	}
	attKey := created.Successful["0"].Key

	// Phase 1 — authorize the upload (form-urlencoded; mtime in MILLISECONDS).
	// md5 is PLAIN HEX here AND nowhere else is a digest sent — one encoding,
	// no base64 variant.
	// TODO(#184): live-probe digest acceptance (hex vs base64) once more on a
	// Zotero 10.0-beta point release; contentType is NOT in the probed fact
	// list above and may be ignored or rejected by the local API.
	md5hex := fmt.Sprintf("%x", md5.Sum(pdf))
	// url.Values.Encode() — schema filenames carry spaces, umlauts, &, %, +
	// ({Autor} - {Jahr} - {Titel}); hand-joined forms silently truncate at
	// '&' and corrupt at '+'/'%' (review C3, empirically demonstrated).
	form := strings.NewReader((url.Values{
		"md5":         {md5hex},
		"filename":    {filename},
		"filesize":    {strconv.Itoa(len(pdf))},
		"mtime":       {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		"contentType": {"application/pdf"},
	}).Encode())
	raw, _, err = w.do(http.MethodPost, "/api/users/0/items/"+attKey+"/file",
		map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded",
			"If-None-Match": "*",
		}, form)
	if err != nil {
		return attKey, fmt.Errorf("authorize upload: %w", err)
	}
	var auth struct {
		Exists    int               `json:"exists"`
		URL       string            `json:"url"`
		UploadKey string            `json:"uploadKey"`
		Params    map[string]string `json:"params"` // web-API S3 fields; forwarded into the multipart form below
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		// Some local-API builds answer the authorize call with the upload key
		// as a BARE quoted JSON string — accept exactly that shape and nothing
		// else; never silently post a garbage body as the key.
		var bare string
		if berr := json.Unmarshal(raw, &bare); berr != nil || bare == "" {
			return attKey, fmt.Errorf("authorize upload: unerwartete Antwortform (will {url,uploadKey}|{exists:1}|\"key\"): %s", string(raw))
		}
		auth.UploadKey = bare
	}
	if auth.Exists == 1 {
		return attKey, nil // identical file already staged/synced — done
	}
	if auth.UploadKey == "" || auth.URL == "" {
		return attKey, fmt.Errorf("authorize upload: keine uploadKey/url: %s", string(raw))
	}

	// Phase 2 — transmit the bytes (multipart, field "file"; 201). Register-
	// response upload params are forwarded as leading form fields (web-API S3
	// contract; empty on the local API).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range auth.Params {
		if err := mw.WriteField(k, v); err != nil {
			return attKey, err
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return attKey, err
	}
	if _, err := fw.Write(pdf); err != nil {
		return attKey, err
	}
	if err := mw.Close(); err != nil {
		return attKey, err
	}
	upReq, err := http.NewRequest(http.MethodPost, auth.URL, &buf)
	if err != nil {
		return attKey, err
	}
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	// Credentials only travel to the LOCAL API host: an authorize response
	// pointing elsewhere (pre-signed S3-style URL) must not receive our key.
	if sameHost(w.BaseURL, auth.URL) {
		w.localAuthHeaders(upReq)
	}
	upResp, err := w.HTTP.Do(upReq)
	if err != nil {
		return attKey, fmt.Errorf("upload bytes: %w", err)
	}
	defer upResp.Body.Close()
	upBody, _ := io.ReadAll(upResp.Body)
	if upResp.StatusCode >= 300 {
		return attKey, fmt.Errorf("upload bytes: %d %s", upResp.StatusCode, string(upBody))
	}

	// Phase 3 — register the upload against the item (204).
	reg := strings.NewReader(url.Values{"upload": {auth.UploadKey}}.Encode())
	_, _, err = w.do(http.MethodPost, "/api/users/0/items/"+attKey+"/file",
		map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded",
			"If-None-Match": "*",
		}, reg)
	if err != nil {
		return attKey, fmt.Errorf("register upload: %w", err)
	}
	return attKey, nil
}

// sameHost reports whether two URL strings share scheme-insensitive host:port.
func sameHost(a, b string) bool {
	pa, err := url.Parse(a)
	if err != nil {
		return false
	}
	pb, err := url.Parse(b)
	if err != nil {
		return false
	}
	return pa.Host == pb.Host
}
