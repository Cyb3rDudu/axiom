package zotero

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LocalAPI is a read-only client for the Zotero Local JSON API
// (http://localhost:23119/api/). axiom-ng runs on the same host as Zotero and
// resolves attachments to local filesystem paths.
type LocalAPI struct {
	base      string
	libraryID string
	client    *http.Client
	serverID  string
}

// LocalAPIOption configures a LocalAPI client.
type LocalAPIOption func(*LocalAPI)

// NewLocalAPI builds a client against the Zotero local API. By default it
// targets the local user's library (users/0). Reading requires no API key.
func NewLocalAPI(base, libraryID string, opts ...LocalAPIOption) *LocalAPI {
	api := &LocalAPI{
		base:      strings.TrimRight(base, "/"),
		libraryID: libraryID,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(api)
	}
	return api
}

// WithHTTPClient overrides the underlying HTTP client (used in tests).
func WithHTTPClient(c *http.Client) LocalAPIOption {
	return func(a *LocalAPI) { a.client = c }
}

func (a *LocalAPI) url(path string, query url.Values) string {
	u := a.base + "/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// StatusError is an error carrying the HTTP status of a non-2xx Zotero
// response, so callers can distinguish a genuinely missing resource (404)
// from a transient failure that should abort the sync.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("zotero http %d: %s", e.Status, e.Body)
}

func (a *LocalAPI) get(path string, query url.Values) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, a.url(path, query), nil)
	req.Header.Set("Zotero-API-Version", "3")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zotero request %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp, nil
}

// ServerID returns the Zotero-Server-ID that identifies the local library, or
// "" if it could not be obtained. A bare GET /api/ is enough to read it.
func (a *LocalAPI) ServerID() string {
	if a.serverID != "" {
		return a.serverID
	}
	resp, err := a.get("", nil)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	id := resp.Header.Get("Zotero-Server-ID")
	a.serverID = id
	return id
}

// getItemsRaw paginates over /items and returns each item as its full raw JSON
// envelope (preserving all fields for the canonical lossless mirror) plus the
// Last-Modified-Version.
func (a *LocalAPI) getItemsRaw(query url.Values) ([]json.RawMessage, int64, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("format", "json")
	query.Set("limit", "100")
	var all []json.RawMessage
	var lastVersion int64
	start := 0
	for {
		q := cloneValues(query)
		q.Set("start", fmt.Sprintf("%d", start))
		resp, err := a.get(a.libraryID+"/items", q)
		if err != nil {
			return nil, lastVersion, err
		}
		if v := versionHeader(resp.Header.Get("Last-Modified-Version")); v > lastVersion {
			lastVersion = v
		}
		var batch []json.RawMessage
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, lastVersion, fmt.Errorf("zotero items decode: %w", err)
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
		start += len(batch)
	}
	return all, lastVersion, nil
}

// canonicalDims extracts the queryable dimensions of a raw item envelope.
type canonicalDims struct {
	key       string
	version   int64
	itemType  string
	parentKey string
}

func (d *canonicalDims) fromRaw(env json.RawMessage) bool {
	var top struct {
		Key     string `json:"key"`
		Version int64  `json:"version"`
		Data    struct {
			Key        string `json:"key"`
			Version    int64  `json:"version"`
			ItemType   string `json:"itemType"`
			ParentItem string `json:"parentItem,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(env, &top); err != nil {
		return false
	}
	d.key = top.Key
	if d.key == "" {
		d.key = top.Data.Key
	}
	d.version = top.Data.Version
	if d.version == 0 {
		d.version = top.Version
	}
	d.itemType = top.Data.ItemType
	d.parentKey = top.Data.ParentItem
	return d.key != ""
}

// ListCanonicalItems returns every item (parent, attachment, note, annotation)
// losslessly for the canonical mirror, honouring `since`. A since of 0 is a
// full snapshot (FullSnapshot=true); incrementals deliver only the changed
// items (FullSnapshot=false). A non-decodable item envelope aborts the sync
// rather than being silently skipped, so the mirror stays lossless.
func (a *LocalAPI) ListCanonicalItems(since int64) (CanonicalBatch, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", fmt.Sprintf("%d", since))
	}
	raw, version, err := a.getItemsRaw(q)
	if err != nil {
		return CanonicalBatch{}, err
	}
	items, err := canonicalItemsFromRaw(raw)
	if err != nil {
		return CanonicalBatch{}, err
	}

	var deletes []DeleteEvent
	deletedVersion := int64(0)
	deletedFeedSupported := true
	if since > 0 {
		allEvents, v, derr := a.ListDeletedKeys(since)
		if errors.Is(derr, errDeletionFeedUnavailable) {
			// The /deleted feed is unsupported (404/501) on this Zotero instance.
			// The deletion state is unknown for an incremental sync: silently
			// advancing would risk leaving deleted items active. Fall back to a
			// FULL item snapshot so absent items are reconciled (marked deleted)
			// by item absence, and never advance over an incomplete deletion view.
			deletedFeedSupported = false
		} else if derr != nil {
			return CanonicalBatch{}, derr
		} else {
			deletes = allEvents
			deletedVersion = v
		}
	}

	fullSnapshot := since == 0
	if since > 0 && !deletedFeedSupported {
		fullSnapshot = true
		// Re-fetch the complete item listing without a since window so missing
		// items are truly a full-snapshot signal for reconciliation.
		raw, version, err = a.getItemsRaw(url.Values{})
		if err != nil {
			return CanonicalBatch{}, err
		}
		items, err = canonicalItemsFromRaw(raw)
		if err != nil {
			return CanonicalBatch{}, err
		}
	}

	// The cursor must reflect the highest version observed across every feed we
	// read this run: items + trash/deleted. Never below the prior since.
	newVersion := since
	if version > newVersion {
		newVersion = version
	}
	if deletedVersion > newVersion {
		newVersion = deletedVersion
	}

	return CanonicalBatch{
		FullSnapshot: fullSnapshot,
		Items:        items,
		DeleteEvents: deletes,
		NewVersion:   newVersion,
	}, nil
}

func canonicalItemsFromRaw(raw []json.RawMessage) ([]CanonicalItem, error) {
	items := make([]CanonicalItem, 0, len(raw))
	for _, env := range raw {
		var dims canonicalDims
		if !dims.fromRaw(env) {
			return nil, fmt.Errorf("undecodable item envelope: %s", truncate(env, 200))
		}
		// Extract raw_data (the item's own data object).
		var data json.RawMessage
		var holder struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(env, &holder); err != nil || len(holder.Data) == 0 {
			data = env // fallback: the whole envelope
		} else {
			data = holder.Data
		}
		items = append(items, CanonicalItem{
			Key:       dims.key,
			Version:   dims.version,
			ItemType:  dims.itemType,
			ParentKey: dims.parentKey,
			Envelope:  env,
			Data:      data,
		})
	}
	return items, nil
}

func truncate(b json.RawMessage, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ListCanonicalCollections returns all collections as lossless mirrors with
// their parent hierarchy, paginated so collections beyond `limit` are also
// captured.
func (a *LocalAPI) ListCanonicalCollections() ([]CanonicalCollection, error) {
	var all []CanonicalCollection
	start := 0
	for {
		q := url.Values{"format": {"json"}, "limit": {"100"}, "start": {fmt.Sprintf("%d", start)}}
		resp, err := a.get(a.libraryID+"/collections", q)
		if err != nil {
			return nil, err
		}
		var batch []json.RawMessage
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&batch)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("zotero collections decode: %w", decodeErr)
		}
		for _, env := range batch {
			var dims struct {
				Key  string `json:"key"`
				Data struct {
					Key              string          `json:"key"`
					Name             string          `json:"name"`
					ParentCollection json.RawMessage `json:"parentCollection"`
				} `json:"data"`
			}
			if err := json.Unmarshal(env, &dims); err != nil {
				// A malformed collection envelope would silently lose data from
				// a lossless mirror: abort the sync.
				return nil, fmt.Errorf("undecodable collection envelope: %s", truncate(env, 200))
			}
			key := dims.Key
			if key == "" {
				key = dims.Data.Key
			}
			if key == "" {
				return nil, fmt.Errorf("collection envelope missing key")
			}
			parent := ""
			if len(dims.Data.ParentCollection) > 0 && dims.Data.ParentCollection[0] == '"' {
				_ = json.Unmarshal(dims.Data.ParentCollection, &parent)
			}
			all = append(all, CanonicalCollection{Key: key, Name: dims.Data.Name, ParentKey: parent, Envelope: env})
		}
		if len(batch) < 100 {
			break
		}
		start += len(batch)
	}
	return all, nil
}

// versionHeader parses an unsigned library version string, returning 0 for
// empty or malformed values.
func versionHeader(s string) int64 {
	if s == "" {
		return 0
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil || v < 0 {
		return 0
	}
	return v
}

// ListDeletedKeys returns the items Zotero reports as removed, structured so a
// single deleted attachment is distinguished from a deleted parent document. It
// merges two sources and deduplicates by key:
//   - /items/trash?since=      – items currently in the trash (rich details)
//   - /deleted?since=          – items permanently deleted (trashed and purged)
//
// Merging both guarantees a mirror can't stay active for an item that was
// trashed and purged between two syncs (it appears in neither the normal delta
// nor the live trash, but is listed by /deleted). The trash event (which has
// item type + parent) wins for the same key. Responses are paginated and decode
// errors abort the sync rather than advancing its cursor over unverifiable
// deletions.
// ListDeletedKeys merges the trash feed and the permanent /deleted feed and
// deduplicates by key (trash wins). It returns an error when the /deleted feed
// is unavailable, signalling that the deletion state is incomplete and the
// caller must not advance a cursor over it.
func (a *LocalAPI) ListDeletedKeys(since int64) ([]DeleteEvent, int64, error) {
	allEvents, trashVersion, err := a.listTrashedKeys(since)
	if err != nil {
		return nil, 0, err
	}
	deletedKeys, delVersion, derr := a.listPermanentlyDeletedKeys(since)
	if derr != nil {
		return nil, 0, derr
	}
	version := trashVersion
	if delVersion > version {
		version = delVersion
	}
	seen := map[string]bool{}
	for _, ev := range allEvents {
		seen[ev.Key] = true
	}
	for _, k := range deletedKeys {
		if seen[k] {
			continue
		}
		allEvents = append(allEvents, DeleteEvent{Key: k})
	}
	return allEvents, version, nil
}

func (a *LocalAPI) listTrashedKeys(since int64) ([]DeleteEvent, int64, error) {
	var allEvents []DeleteEvent
	version := int64(0)
	start := 0
	for {
		q := url.Values{"format": {"json"}, "limit": {"100"}, "start": {fmt.Sprintf("%d", start)}}
		if since > 0 {
			q.Set("since", fmt.Sprintf("%d", since))
		}
		path := a.libraryID + "/items/trash"
		resp, err := a.get(path, q)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusNotImplemented) {
				// The trash feed is unsupported (404/501): the deletion state is
				// incomplete, the same as a missing /deleted feed. Return the
				// shared sentinel so the caller falls back to a full snapshot
				// rather than silently treating deletions as empty.
				return nil, 0, errDeletionFeedUnavailable
			}
			return nil, 0, err
		}
		if v := versionHeader(resp.Header.Get("Last-Modified-Version")); v > version {
			version = v
		}
		var trash []struct {
			Key  string `json:"key"`
			Data struct {
				Key        string `json:"key"`
				ItemType   string `json:"itemType"`
				ParentItem string `json:"parentItem,omitempty"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&trash)
		resp.Body.Close()
		if decodeErr != nil {
			// A malformed trash response must stop the sync so deletions are not
			// silently dropped.
			return nil, 0, fmt.Errorf("zotero trash decode: %w", decodeErr)
		}
		for _, it := range trash {
			key := it.Key
			if key == "" {
				key = it.Data.Key
			}
			if key == "" {
				continue
			}
			allEvents = append(allEvents, DeleteEvent{
				Key:       key,
				ItemType:  it.Data.ItemType,
				ParentKey: it.Data.ParentItem,
			})
		}
		if len(trash) < 100 {
			break
		}
		start += len(trash)
	}
	return allEvents, version, nil
}

// listPermanentlyDeletedKeys returns the keys of items permanently deleted
// (trashed and purged) since <version>, via the documented GET /deleted feed.
// supported false means the Zotero instance does not expose the /deleted feed
// (404/501) and the caller must reconcile deletions another way (e.g. a full
// item snapshot) rather than silently assuming nothing was deleted.
func (a *LocalAPI) listPermanentlyDeletedKeys(since int64) (items []string, version int64, err error) {
	path := a.libraryID + "/deleted"
	q := url.Values{}
	if since > 0 {
		q.Set("since", fmt.Sprintf("%d", since))
	}
	resp, err := a.get(path, q)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusNotImplemented) {
			return nil, 0, errDeletionFeedUnavailable
		}
		return nil, 0, err
	}
	version = versionHeader(resp.Header.Get("Last-Modified-Version"))
	var body struct {
		Items []string `json:"items"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
	resp.Body.Close()
	if decodeErr != nil {
		return nil, 0, fmt.Errorf("zotero /deleted decode: %w", decodeErr)
	}
	return body.Items, version, nil
}

// errDeletionFeedUnavailable reports that the Zotero instance does not expose
// the trash feed and/or the /deleted incremental-deletion feed, so the
// deletion state cannot be known incrementally (the caller must reconcile by a
// full item snapshot instead of advancing the cursor).
var errDeletionFeedUnavailable = fmt.Errorf("zotero deletion feed unavailable")

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
