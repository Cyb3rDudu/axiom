// Package zotero — write client for the #184 fix-service loop.
//
// The RAG is the ONLY gateway to Zotero: the fix-service never sees
// credentials. Writes go through the Zotero 7.1+ LOCAL API write surface
// (same semantics as the web API, keyed by local authorization).
//
// Exactly TWO mutations exist (#184 design nail 3):
//
//	DeleteAttachmentItem — remove a (quarantined-before) broken attachment
//	CreateAttachmentWithFile — 3-phase upload of the healed PDF under a
//	  SCHEMA filename ({Autor|Institution} - {Jahr} - {Titel}); there is
//	  deliberately NO filename patch.
package zotero

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WriteClient talks to the Zotero local API with write authorization.
type WriteClient struct {
	BaseURL  string // http://localhost:23119
	ServerID string // Zotero-Server-ID header (same value the sync reader uses)
	APIKey   string // from POST /api/local/authorize (Key-Flow), env-injected
	HTTP     *http.Client
}

func NewWriteClient(baseURL, serverID, apiKey string) *WriteClient {
	return &WriteClient{BaseURL: strings.TrimRight(baseURL, "/"), ServerID: serverID, APIKey: apiKey,
		HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Authorize performs the local Key-Flow once: Zotero shows an approval
// dialog for appName; on approval the response carries the api key. Ops
// runs this interactively and puts the key into ZOTERO_WRITE_API_KEY —
// normal operation never calls this.
func (w *WriteClient) Authorize(appName string) (string, error) {
	body, _ := json.Marshal(map[string]any{"appName": appName, "auth": []string{"allow_library_metadata", "allow_file_upload"}})
	req, _ := http.NewRequest(http.MethodPost, w.BaseURL+"/api/local/authorize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Zotero-Server-ID", w.ServerID)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authorize: %d %s", resp.StatusCode, string(raw))
	}
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.APIKey == "" {
		return "", fmt.Errorf("authorize: keine apiKey in Antwort: %s", string(raw))
	}
	return out.APIKey, nil
}

func (w *WriteClient) do(method, path string, headers map[string]string, body io.Reader) ([]byte, http.Header, error) {
	req, err := http.NewRequest(method, w.BaseURL+path, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Zotero-Server-ID", w.ServerID)
	if w.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.APIKey)
	}
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
		return raw, resp.Header, &httpError{method: method, path: path, status: resp.StatusCode, body: string(raw)}
	}
	return raw, resp.Header, nil
}

type httpError struct {
	method, path string
	status       int
	body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("zotero write %s %s: %d %s", e.method, e.path, e.status, e.body)
}

func IsVersionConflict(err error) bool {
	he, ok := err.(*httpError)
	return ok && he.status == http.StatusPreconditionFailed
}

// ItemVersion fetches an item's current version (If-Unmodified-Since-Version
// source for deletes).
func (w *WriteClient) ItemVersion(key string) (string, error) {
	_, hdr, err := w.do(http.MethodHead, "/api/users/0/items/"+key, nil, nil)
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

// Mutation 2: create an attachment item WITH a file (3-phase upload,
// web-API semantics against the local endpoint):
//  1. register the upload (md5/sha1/mtime/filename) → upload key
//  2. upload the bytes to the returned URL (local API: same host)
//  3. commit with the parent item JSON → the attachment exists
//
// parentKey is the document item the new attachment belongs to (empty =
// standalone). filename MUST come from the schema builder — callers cannot
// patch filenames because no such mutation exists.
func (w *WriteClient) CreateAttachmentWithFile(parentKey, filename string, pdf []byte) (string, error) {
	md5sum := fmt.Sprintf("%x", md5.Sum(pdf))
	sha1sum := fmt.Sprintf("%x", sha1.Sum(pdf))
	mtime := strconv.FormatInt(time.Now().UnixMilli(), 10)

	// Phase 1 — register.
	regHdr := map[string]string{
		"Content-Type":       "application/x-www-form-urlencoded",
		"If-None-Match":      "*",
		"X-Zotero-FileName":  filename,
		"X-Zotero-FileMD5":   base64.StdEncoding.EncodeToString([]byte(md5sum)),
		"X-Zotero-FileSHA1":  base64.StdEncoding.EncodeToString([]byte(sha1sum)),
		"X-Zotero-FileMtime": mtime,
	}
	raw, _, err := w.do(http.MethodPost, "/api/users/0/items/new", regHdr, strings.NewReader(""))
	if err != nil {
		return "", fmt.Errorf("register upload: %w", err)
	}
	var reg struct {
		UploadKey string `json:"uploadKey"`
		URL       string `json:"url"`
		Exists    int    `json:"exists"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return "", fmt.Errorf("register upload decode: %w (%s)", err, string(raw))
	}
	if reg.Exists == 1 {
		// File already known server-side — go straight to commit with that key.
		reg.URL = ""
		reg.UploadKey = strings.TrimSpace(strings.Trim(string(raw), `"`))
	}

	// Phase 2 — upload bytes (multipart like the web API's S3 form; the
	// local API accepts the same shape on its own host).
	if reg.URL != "" {
		if err := w.uploadBytes(reg.URL, filename, md5sum, pdf); err != nil {
			return "", fmt.Errorf("upload bytes: %w", err)
		}
	}

	// Phase 3 — commit: register the attachment item tied to the upload.
	item := map[string]any{
		"itemType":    "attachment",
		"parentItem":  parentKey,
		"linkMode":    "imported_file",
		"title":       filename,
		"contentType": "application/pdf",
		"filename":    filename,
		"tags":        []map[string]string{{"tag": "axiom-repair"}},
	}
	itemJSON, _ := json.Marshal([]any{item})
	q := url.Values{"uploadKey": []string{reg.UploadKey}}
	_, _, err = w.do(http.MethodPost, "/api/users/0/items/new/commit?"+q.Encode(),
		map[string]string{"Content-Type": "application/json"}, bytes.NewReader(itemJSON))
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return reg.UploadKey, nil
}

func (w *WriteClient) uploadBytes(uploadURL, filename, md5sum string, pdf []byte) error {
	if !strings.HasPrefix(uploadURL, "http") {
		uploadURL = w.BaseURL + uploadURL
	}
	var buf bytes.Buffer
	fw := newMultipartWriter(&buf)
	fw.writeField("md5", md5sum)
	fw.writeFile("file", filename, "application/pdf", pdf)
	fw.close()
	req, err := http.NewRequest(http.MethodPost, uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", fw.contentType())
	req.Header.Set("Zotero-Server-ID", w.ServerID)
	if w.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.APIKey)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("upload: %d %s", resp.StatusCode, string(raw))
	}
	return nil
}

// tiny multipart writer (avoids importing mime/multipart writer helpers
// twice for one call site)
type mpWriter struct {
	boundary string
	w        *bytes.Buffer
}

func newMultipartWriter(buf *bytes.Buffer) *mpWriter {
	return &mpWriter{boundary: "axiomNg" + strconv.FormatInt(time.Now().UnixNano(), 36), w: buf}
}

func (m *mpWriter) contentType() string { return "multipart/form-data; boundary=" + m.boundary }

func (m *mpWriter) partHeader(name, filename, ctype string) {
	m.w.WriteString("--" + m.boundary + "\r\n")
	if filename == "" {
		m.w.WriteString("Content-Disposition: form-data; name=\"" + name + "\"\r\n\r\n")
	} else {
		m.w.WriteString("Content-Disposition: form-data; name=\"" + name + "\"; filename=\"" + filename + "\"\r\n")
		m.w.WriteString("Content-Type: " + ctype + "\r\n\r\n")
	}
}

func (m *mpWriter) writeField(name, value string) {
	m.partHeader(name, "", "")
	m.w.WriteString(value + "\r\n")
}

func (m *mpWriter) writeFile(field, filename, ctype string, data []byte) {
	m.partHeader(field, filename, ctype)
	m.w.Write(data)
	m.w.WriteString("\r\n")
}

func (m *mpWriter) close() { m.w.WriteString("--" + m.boundary + "--\r\n") }
