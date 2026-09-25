// write.go — write client for the #184 fix-service loop, part of the
// Zotero adapter package since F07 (#301).
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
package zoteroprovider

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

// GetItem fetches one item's data JSON plus its current version (the
// adapter's readback primitive; absent items surface as *StatusError 404).
func (w *WriteClient) GetItem(key string) (data []byte, version string, err error) {
	raw, hdr, err := w.do(http.MethodGet, "/api/users/0/items/"+key, nil, nil)
	if err != nil {
		return nil, "", err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
		return nil, "", fmt.Errorf("item %s: undecodable envelope %.200s", key, raw)
	}
	return env.Data, hdr.Get("Last-Modified-Version"), nil
}

// PutItem replaces an item's data under the optimistic-concurrency guard
// (If-Unmodified-Since-Version): a 412 means a concurrent writer won —
// IsVersionConflict detects it, the adapter maps it to a typed retryable
// error.
func (w *WriteClient) PutItem(key string, itemJSON []byte, version string) error {
	_, _, err := w.do(http.MethodPut, "/api/users/0/items/"+key,
		map[string]string{
			"Content-Type":                "application/json",
			"If-Unmodified-Since-Version": version,
		}, bytes.NewReader(itemJSON))
	return err
}

// GetCollection fetches one collection's data (readback of a created
// collection segment).
func (w *WriteClient) GetCollection(key string) ([]byte, error) {
	raw, _, err := w.do(http.MethodGet, "/api/users/0/collections/"+key, nil, nil)
	return raw, err
}

// PostCollections creates collections (parent-first — the caller resolves
// each segment's parent before creating the child).
func (w *WriteClient) PostCollections(body []byte) ([]byte, error) {
	raw, _, err := w.do(http.MethodPost, "/api/users/0/collections",
		map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body))
	return raw, err
}

// DeleteCollection removes a collection under the version guard (the
// IT cleanup path; version-guarded like every mutation).
func (w *WriteClient) DeleteCollection(key string) error {
	_, hdr, err := w.do(http.MethodGet, "/api/users/0/collections/"+key, nil, nil)
	if err != nil {
		return err
	}
	ver := hdr.Get("Last-Modified-Version")
	if ver == "" {
		return fmt.Errorf("collection %s: keine Last-Modified-Version", key)
	}
	_, _, err = w.do(http.MethodDelete, "/api/users/0/collections/"+key,
		map[string]string{"If-Unmodified-Since-Version": ver}, nil)
	return err
}

// GetItemEnvelope fetches the FULL item envelope (data + links — the
// enclosure link carries the local storage path, the file readback's
// source: the local API answers GET /items/<key>/file with a redirect to
// a file:// URL, not with bytes).
func (w *WriteClient) GetItemEnvelope(key string) (raw []byte, version string, err error) {
	raw, hdr, err := w.do(http.MethodGet, "/api/users/0/items/"+key, nil, nil)
	return raw, hdr.Get("Last-Modified-Version"), err
}

// GetFile downloads an attachment's stored bytes (readback: the file must
// be retrievable and match the uploaded digest/size).
func (w *WriteClient) GetFile(key string) ([]byte, error) {
	raw, _, err := w.do(http.MethodGet, "/api/users/0/items/"+key+"/file", nil, nil)
	return raw, err
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
// such mutation exists. contentType (#220) is the attachment's MIME type —
// "" defaults to application/pdf (the pre-#220 shape); EPUB repairs pass
// application/epub+zip so the item metadata matches the uploaded artifact.
// extraTags ride on the item (the Library adapter stamps its idempotency
// anchors there, e.g. axiom-sha256:<hash> — F07 #301).
// Returns the new attachment item key.
func (w *WriteClient) CreateAttachmentWithFile(parentKey, filename, contentType string, pdf []byte, extraTags ...string) (string, error) {
	if contentType == "" {
		contentType = "application/pdf"
	}
	// Phase 0 — the attachment item. ORDERED struct, never a map: Go maps
	// marshal alphabetically, putting contentType/filename BEFORE linkMode —
	// Zotero's fromJSON then rejects with "Link mode must be set before
	// setting attachment path" (live bug, Controlling apply).
	type attachmentItem struct {
		ItemType    string              `json:"itemType"`
		LinkMode    string              `json:"linkMode"`
		ParentItem  string              `json:"parentItem,omitempty"`
		Title       string              `json:"title"`
		ContentType string              `json:"contentType"`
		Filename    string              `json:"filename"`
		Tags        []map[string]string `json:"tags"`
	}
	tags := []map[string]string{{"tag": "axiom-repair"}}
	for _, t := range extraTags {
		tags = append(tags, map[string]string{"tag": t})
	}
	item := attachmentItem{
		ItemType: "attachment", LinkMode: "imported_file", ParentItem: parentKey,
		Title: filename, ContentType: contentType, Filename: filename,
		Tags: tags,
	}
	itemJSON, _ := json.Marshal([]attachmentItem{item})
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

	// Orphan guard (review W1): the caller has already quarantined and
	// deleted the ORIGINAL — a failure after this point must not leave the
	// freshly created EMPTY imported_file item behind. Best-effort delete
	// (version-guarded like every mutation); if even that fails the key is
	// surfaced in the error so an operator can finish manually.
	cleanup := func(err error) (string, error) {
		if delErr := w.DeleteAttachmentItem(attKey); delErr != nil {
			return attKey, fmt.Errorf("%w (aufräumen fehlgeschlagen: leerer Anhang %s manuell löschen)", err, attKey)
		}
		return "", err
	}

	// Phase 1 — authorize the upload (form-urlencoded; mtime in MILLISECONDS).
	// md5 is PLAIN HEX here AND nowhere else is a digest sent — one encoding,
	// no base64 variant.
	// urlSearchParamsEncode — NOT url.Values.Encode(): the Zotero local API
	// parses the form JavaScript-style (URLSearchParams), where '+' stays a
	// literal '+' and only %XX sequences decode. Schema filenames carry
	// spaces (umlauts, &, % too) — the classic '+' encoding stored them
	// verbatim ("Habermas+-+2021", the Springer-style stems #291 documented)
	// until the F07 readback caught it. %20-for-space is correct under BOTH
	// form parsers.
	md5hex := fmt.Sprintf("%x", md5.Sum(pdf))
	form := strings.NewReader(urlSearchParamsEncode(url.Values{
		"md5":         {md5hex},
		"filename":    {filename},
		"filesize":    {strconv.Itoa(len(pdf))},
		"mtime":       {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		"contentType": {contentType},
	}))
	raw, _, err = w.do(http.MethodPost, "/api/users/0/items/"+attKey+"/file",
		map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded",
			"If-None-Match": "*",
		}, form)
	if err != nil {
		return cleanup(fmt.Errorf("authorize upload: %w", err))
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
			return cleanup(fmt.Errorf("authorize upload: unerwartete Antwortform (will {url,uploadKey}|{exists:1}|\"key\")): %s", string(raw)))
		}
		auth.UploadKey = bare
	}
	if auth.Exists == 1 {
		return attKey, nil // identical file already staged/synced — done
	}
	if auth.UploadKey == "" || auth.URL == "" {
		return cleanup(fmt.Errorf("authorize upload: keine uploadKey/url: %s", string(raw)))
	}

	// Phase 2 — transmit the bytes (multipart, field "file"; 201). Register-
	// response upload params are forwarded as leading form fields (web-API S3
	// contract; empty on the local API).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range auth.Params {
		if err := mw.WriteField(k, v); err != nil {
			return cleanup(err)
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return cleanup(err)
	}
	if _, err := fw.Write(pdf); err != nil {
		return cleanup(err)
	}
	if err := mw.Close(); err != nil {
		return cleanup(err)
	}
	upReq, err := http.NewRequest(http.MethodPost, auth.URL, &buf)
	if err != nil {
		return cleanup(err)
	}
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	// Credentials only travel to the LOCAL API host: an authorize response
	// pointing elsewhere (pre-signed S3-style URL) must not receive our key.
	if sameHost(w.BaseURL, auth.URL) {
		w.localAuthHeaders(upReq)
	}
	upResp, err := w.HTTP.Do(upReq)
	if err != nil {
		return cleanup(fmt.Errorf("upload bytes: %w", err))
	}
	defer upResp.Body.Close()
	upBody, _ := io.ReadAll(upResp.Body)
	if upResp.StatusCode >= 300 {
		return cleanup(fmt.Errorf("upload bytes: %d %s", upResp.StatusCode, string(upBody)))
	}

	// Phase 3 — register the upload against the item (204).
	reg := strings.NewReader(url.Values{"upload": {auth.UploadKey}}.Encode())
	_, _, err = w.do(http.MethodPost, "/api/users/0/items/"+attKey+"/file",
		map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded",
			"If-None-Match": "*",
		}, reg)
	if err != nil {
		return cleanup(fmt.Errorf("register upload: %w", err))
	}
	return attKey, nil
}

// urlSearchParamsEncode encodes a form the JavaScript-URLSearchParams
// way: percent-escape everything QueryEscape would, then repair the
// space handling ('+' → '%20' — URLSearchParams never decodes '+' as a
// space, classic form parsing decodes both; %20 is safe under both).
func urlSearchParamsEncode(v url.Values) string {
	return strings.ReplaceAll(v.Encode(), "+", "%20")
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
