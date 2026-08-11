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

// zoteroObject is the common envelope of every local API object.
type zoteroObject struct {
	Key     string          `json:"key"`
	Version int64           `json:"version"`
	Links   zoteroLinks     `json:"links"`
	Meta    zoteroMeta      `json:"meta"`
	Data    json.RawMessage `json:"data"`
}

type zoteroLinks struct {
	Enclosure zoteroLink `json:"enclosure"`
}

type zoteroLink struct {
	Href string `json:"href"`
}

type zoteroMeta struct {
	NumChildren int `json:"numChildren"`
}

// zoteroItemData is the content of the "data" object of an item.
type zoteroItemData struct {
	Key         string          `json:"key"`
	Version     int64           `json:"version"`
	ItemType    string          `json:"itemType"`
	Title       string          `json:"title"`
	ParentItem  string          `json:"parentItem,omitempty"`
	LinkMode    string          `json:"linkMode,omitempty"`
	ContentType string          `json:"contentType,omitempty"`
	Filename    string          `json:"filename,omitempty"`
	Date        string          `json:"date"`
	URL         string          `json:"url,omitempty"`
	Creators    []zoteroCreator `json:"creators,omitempty"`
	Tags        []zoteroTag     `json:"tags,omitempty"`
	Collections []string        `json:"collections,omitempty"`
}

type zoteroCreator struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

type zoteroTag struct {
	Tag string `json:"tag"`
}

func (a *LocalAPI) getItems(query url.Values) ([]zoteroObject, error) {
	items, _, err := a.getItemsWithVersion(query)
	return items, err
}

// getItemsWithVersion paginates over /items and also returns the library
// version reported in the Last-Modified-Version header of the last response.
func (a *LocalAPI) getItemsWithVersion(query url.Values) ([]zoteroObject, int64, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("format", "json")
	query.Set("limit", "100")
	var all []zoteroObject
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
		var batch []zoteroObject
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
		if errors.Is(derr, errMissingDeletedFeed) {
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

// getChildren returns all child items (attachments, notes) of a parent item so
// a delta-triggered refresh can reconstruct a complete document. Children are
// paginated over /children with start/limit so parents with more than `limit`
// children are not truncated (which would otherwise mark valid attachments as
// deleted during reconciliation).
func (a *LocalAPI) getChildren(parentKey string) ([]zoteroObject, error) {
	path := a.libraryID + "/items/" + parentKey + "/children"
	var all []zoteroObject
	start := 0
	for {
		q := url.Values{"format": {"json"}, "limit": {"100"}, "start": {fmt.Sprintf("%d", start)}}
		resp, err := a.get(path, q)
		if err != nil {
			return nil, err
		}
		var batch []zoteroObject
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("zotero children %s decode: %w", parentKey, err)
		}
		all = append(all, batch...)
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

// ListCollections returns the library's collections.
func (a *LocalAPI) ListCollections() ([]Collection, error) {
	resp, err := a.get(a.libraryID+"/collections", url.Values{"format": {"json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw []struct {
		Data struct {
			Key              string          `json:"key"`
			Name             string          `json:"name"`
			ParentCollection json.RawMessage `json:"parentCollection"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("zotero collections decode: %w", err)
	}
	out := make([]Collection, 0, len(raw))
	for _, c := range raw {
		// parentCollection is either a string (parent key) or the boolean
		// false for top-level collections in the local API.
		var parent string
		if len(c.Data.ParentCollection) > 0 && c.Data.ParentCollection[0] == '"' {
			_ = json.Unmarshal(c.Data.ParentCollection, &parent)
		}
		out = append(out, Collection{Key: c.Data.Key, Name: c.Data.Name, Parent: parent})
	}
	return out, nil
}

// ListPDFItems returns complete top-level documents that carry at least one
// file attachment.
//
// since is the last known library version: pass 0 for a full sync, or a stored
// version for an incremental sync. For incremental syncs the library is first
// queried with ?since=<since>; the affected parent keys are then reconstructed
// into complete documents (each parent item plus its current children) so a
// change to an attachment alone never loses its parent, and vice versa.
//
// The returned version is taken from the Last-Modified-Version header and is
// never allowed to fall below `since`, so a delta response that turns up no
// items still advances (or at least never rewinds) the sync cursor.
func (a *LocalAPI) ListPDFItems(since int64) (ListResult, error) {
	result, err := a.listItems(since)
	if err != nil {
		return ListResult{}, err
	}
	return result, nil
}

func (a *LocalAPI) listItems(since int64) (ListResult, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", fmt.Sprintf("%d", since))
	}
	raw, headerVersion, err := a.getItemsWithVersion(q)
	if err != nil {
		return ListResult{}, err
	}

	// Cursor: trust the server's Last-Modified-Version, and never fall below
	// the version we already committed to.
	newVersion := headerVersion
	if newVersion < since {
		newVersion = since
	}

	// Decode the raw objects and reconstruct complete documents (parents with
	// their current children) from those touched by this response. affectedKeys
	// is every parent key touched by the delta (incl. parents that now have no
	// processable attachment), used to scope reconciliation. deletedKeys is the
	// set of parents Zotero reports removed during reconstruction.
	parents, parentOf, affectedKeys, deletedKeys, err := a.completeItems(raw, since)
	if err != nil {
		return ListResult{}, err
	}

	items := make([]Item, 0, len(parents))
	for _, d := range parents {
		atts := parentOf[d.item.Key]
		if len(atts) == 0 {
			continue
		}
		var creat []Creator
		for _, c := range d.item.Creators {
			creat = append(creat, Creator{FirstName: c.FirstName, LastName: c.LastName})
		}
		var tags []Tag
		for _, t := range d.item.Tags {
			tags = append(tags, Tag{Tag: t.Tag})
		}
		items = append(items, Item{
			Key:         d.item.Key,
			Version:     d.item.Version,
			ItemType:    d.item.ItemType,
			Title:       d.item.Title,
			Creators:    creat,
			Tags:        tags,
			Collections: d.item.Collections,
			URL:         d.item.URL,
			Attachments: atts,
		})
	}
	return ListResult{
		Items:        items,
		AffectedKeys: uniqueKeys(affectedKeys),
		DeletedKeys:  uniqueKeys(deletedKeys),
		NewVersion:   newVersion,
	}, nil
}

// completeItems decodes the raw delta response and returns complete documents.
// For a full sync every parent with a file attachment is included. For an
// incremental sync (since > 0) it also re-fetches the full parent + children
// for every parent that was touched directly OR whose attachment changed, so a
// partial change still reconstructs a complete document.
type decodedItem struct {
	obj  zoteroObject
	item *zoteroItemData
}

func (a *LocalAPI) completeItems(raw []zoteroObject, since int64) ([]decodedItem, map[string][]Attachment, []string, []string, error) {
	changedParents := map[string]bool{}
	var parents []decodedItem
	parentOf := map[string][]Attachment{}
	deletedKeys := []string{}

	addAttachment := func(obj zoteroObject, data *zoteroItemData) {
		// Every attachment marks its parent as touched, including attachments
		// that are not processable (e.g. an HTML note), so a change that turns
		// the only PDF into another type still triggers a full-parent refresh
		// and lets reconciliation mark the old attachment as removed.
		if data.ParentItem == "" {
			return
		}
		changedParents[data.ParentItem] = true
	}

	for _, obj := range raw {
		var data zoteroItemData
		if err := json.Unmarshal(obj.Data, &data); err != nil {
			continue
		}
		if data.ItemType == "" {
			continue
		}
		if data.ItemType == "attachment" {
			addAttachment(obj, &data)
			continue
		}
		changedParents[data.Key] = true
	}

	// For an incremental sync we must reconstruct complete documents. Gather
	// the full item lists for each touched parent (parent item + children).
	if since > 0 {
		reconstructed, atts, aff, del, err := a.reconstructParents(changedParents, parents, parentOf)
		return reconstructed, atts, aff, del, err
	}
	// Full sync: raw already contains every parent and attachment. Build the
	// attachment map and parents list directly from the raw response. Every
	// parent key seen is affected (the whole source is reconciled).
	var rawParents []decodedItem
	rawAtts := map[string][]Attachment{}
	affected := make([]string, 0, len(rawParents))
	for _, obj := range raw {
		var data zoteroItemData
		if err := json.Unmarshal(obj.Data, &data); err != nil {
			continue
		}
		if data.ItemType == "attachment" {
			if data.ParentItem == "" {
				continue
			}
			att := Attachment{
				Key:         data.Key,
				Version:     data.Version,
				ParentKey:   data.ParentItem,
				ContentType: data.ContentType,
				LinkMode:    data.LinkMode,
				Filename:    data.Filename,
				LocalPath:   obj.Links.Enclosure.Href,
			}
			rawAtts[data.ParentItem] = append(rawAtts[data.ParentItem], att)
			affected = append(affected, data.ParentItem)
			continue
		}
		rawParents = append(rawParents, decodedItem{obj: obj, item: &data})
		affected = append(affected, data.Key)
	}
	return rawParents, rawAtts, uniqueKeys(affected), deletedKeys, nil
}

// reconstructParents loads the complete parent + children for each touched key
// from the API, so a delta that only changed an attachment still yields the
// full parent document and its current attachments. A parent that no longer
// exists (404) is skipped; any other failure is returned so the caller does
// not advance the sync cursor over an incompletely reconstructed delta.
func (a *LocalAPI) reconstructParents(changed map[string]bool, parents []decodedItem, parentOf map[string][]Attachment) ([]decodedItem, map[string][]Attachment, []string, []string, error) {
	affected := make([]string, 0, len(changed))
	deleted := []string{}
	for key := range changed {
		affected = append(affected, key)
		children, err := a.getChildren(key)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && se.Status == http.StatusNotFound {
				// Parent was deleted in Zotero; record it so the sync can mark
				// its document and attachments as removed.
				deleted = append(deleted, key)
				continue
			}
			return parents, parentOf, affected, deleted, fmt.Errorf("reconstruct children of %s: %w", key, err)
		}
		for _, obj := range children {
			var data zoteroItemData
			if err := json.Unmarshal(obj.Data, &data); err != nil {
				continue
			}
			if data.ParentItem == "" {
				continue
			}
			att := Attachment{
				Key:         data.Key,
				Version:     data.Version,
				ParentKey:   data.ParentItem,
				ContentType: data.ContentType,
				LinkMode:    data.LinkMode,
				Filename:    data.Filename,
				LocalPath:   obj.Links.Enclosure.Href,
			}
			parentOf[key] = append(parentOf[key], att)
		}
		// Also fetch the parent item itself so a parent-only change is covered.
		p, err := a.fetchItem(key)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && se.Status == http.StatusNotFound {
				deleted = append(deleted, key)
				continue
			}
			return parents, parentOf, affected, deleted, fmt.Errorf("reconstruct parent %s: %w", key, err)
		}
		if p == nil {
			// Attachment-type or empty item; nothing to enqueue, but still an
			// affected parent so reconciliation can clear its attachments.
			continue
		}
		parents = append(parents, *p)
	}
	return parents, parentOf, affected, deleted, nil
}

// fetchItem returns a single item (parent) envelope, or (nil, nil) if it is
// not a parent item (e.g. an attachment). Errors are returned so callers can
// distinguish a transient failure from a graceful skip.
func (a *LocalAPI) fetchItem(key string) (*decodedItem, error) {
	resp, err := a.get(a.libraryID+"/items/"+key, url.Values{"format": {"json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var obj zoteroObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("zotero item %s decode: %w", key, err)
	}
	var data zoteroItemData
	if err := json.Unmarshal(obj.Data, &data); err != nil {
		return nil, fmt.Errorf("zotero item %s data: %w", key, err)
	}
	if data.ItemType == "attachment" || data.ItemType == "" {
		return nil, nil
	}
	return &decodedItem{obj: obj, item: &data}, nil
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
				// Some Zotero versions do not expose a trash endpoint; treat as
				// no deletions.
				return allEvents, version, nil
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
			return nil, 0, errMissingDeletedFeed
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

// errMissingDeletedFeed reports that the Zotero instance does not expose the
// /deleted incremental-deletion feed.
var errMissingDeletedFeed = fmt.Errorf("zotero /deleted feed unavailable")

// FetchParent reconstructs a single parent document (with its current children)
// by key. It returns nil when the parent is gone or is not a parent item. Used
// to re-process a document whose preferred attachment was deleted but that was
// otherwise unchanged (so the normal preferred/hash/enqueue path can select a
// remaining sibling).
func (a *LocalAPI) FetchParent(parentKey string) (*Item, error) {
	children, err := a.getChildren(parentKey)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, nil // parent gone
		}
		return nil, err
	}
	p, err := a.fetchItem(parentKey)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	var atts []Attachment
	for _, obj := range children {
		var data zoteroItemData
		if err := json.Unmarshal(obj.Data, &data); err != nil {
			continue
		}
		if data.ItemType != "attachment" || data.ParentItem == "" {
			continue
		}
		atts = append(atts, Attachment{
			Key:         data.Key,
			Version:     data.Version,
			ParentKey:   data.ParentItem,
			ContentType: data.ContentType,
			LinkMode:    data.LinkMode,
			Filename:    data.Filename,
			LocalPath:   obj.Links.Enclosure.Href,
		})
	}
	var creat []Creator
	for _, c := range p.item.Creators {
		creat = append(creat, Creator{FirstName: c.FirstName, LastName: c.LastName})
	}
	var tags []Tag
	for _, t := range p.item.Tags {
		tags = append(tags, Tag{Tag: t.Tag})
	}
	return &Item{
		Key:         p.item.Key,
		Version:     p.item.Version,
		ItemType:    p.item.ItemType,
		Title:       p.item.Title,
		Creators:    creat,
		Tags:        tags,
		Collections: p.item.Collections,
		URL:         p.item.URL,
		Attachments: atts,
	}, nil
}

// ResolveAttachmentPath returns the local filesystem path of an attachment.
// The file URI is read from the item's enclosure link; /file/view/url is a
// fallback for attachments in responses that omit it.
func (a *LocalAPI) ResolveAttachmentPath(attachmentKey string) (string, error) {
	path := fmt.Sprintf("%s/items/%s", a.libraryID, attachmentKey)
	resp, err := a.get(path, url.Values{"format": {"json"}})
	if err != nil {
		return "", err
	}
	var obj zoteroObject
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&obj)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("zotero item decode: %w", err)
	}
	if strings.HasPrefix(obj.Links.Enclosure.Href, "file://") {
		return obj.Links.Enclosure.Href, nil
	}
	// Fallback to the explicit /file/view/url endpoint.
	vu := fmt.Sprintf("%s/items/%s/file/view/url", a.libraryID, attachmentKey)
	resp2, err := a.get(vu, nil)
	if err != nil {
		return "", err
	}
	b, rerr := io.ReadAll(io.LimitReader(resp2.Body, 16<<10))
	resp2.Body.Close()
	if rerr != nil {
		return "", fmt.Errorf("zotero file/view/url: %w", rerr)
	}
	text := strings.TrimSpace(string(b))
	if strings.HasPrefix(text, "file://") {
		return text, nil
	}
	return "", fmt.Errorf("zotero attachment %s has no local file uri", attachmentKey)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// uniqueKeys returns the input keys with duplicates removed, preserving first
// occurrence order.
func uniqueKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// DedupKeys is the exported form of uniqueKeys, used by the sync layer.
func DedupKeys(keys []string) []string { return uniqueKeys(keys) }
