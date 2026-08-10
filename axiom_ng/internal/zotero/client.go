package zotero

import (
	"encoding/json"
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
		return nil, fmt.Errorf("zotero %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
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
	if query == nil {
		query = url.Values{}
	}
	query.Set("format", "json")
	query.Set("limit", "100")
	var all []zoteroObject
	start := 0
	for {
		q := cloneValues(query)
		q.Set("start", fmt.Sprintf("%d", start))
		resp, err := a.get(a.libraryID+"/items", q)
		if err != nil {
			return nil, err
		}
		var batch []zoteroObject
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("zotero items decode: %w", err)
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
		start += len(batch)
	}
	return all, nil
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

// ListPDFItems returns top-level items that have at least one attachment.
// since filters items modified after the given library version (0 = full sync).
func (a *LocalAPI) ListPDFItems(since int64) ([]Item, int64, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", fmt.Sprintf("%d", since))
	}
	raw, err := a.getItems(q)
	if err != nil {
		return nil, 0, err
	}
	var maxVersion int64

	// Decode each item's data once, splitting attachments from parent items.
	type decoded struct {
		obj  zoteroObject
		item *zoteroItemData
	}
	var parents []decoded
	parentOf := map[string][]Attachment{}

	for _, obj := range raw {
		if obj.Version > maxVersion {
			maxVersion = obj.Version
		}
		var data zoteroItemData
		if err := json.Unmarshal(obj.Data, &data); err != nil {
			continue
		}
		if data.ItemType == "" {
			continue
		}
		if data.ItemType == "attachment" {
			if data.ParentItem == "" {
				continue
			}
			att := Attachment{
				Key:         data.Key,
				ParentKey:   data.ParentItem,
				ContentType: data.ContentType,
				LinkMode:    data.LinkMode,
				Filename:    data.Filename,
				LocalPath:   obj.Links.Enclosure.Href,
			}
			if att.ContentType != "application/pdf" && !strings.HasSuffix(strings.ToLower(att.Filename), ".epub") {
				continue
			}
			parentOf[data.ParentItem] = append(parentOf[data.ParentItem], att)
			continue
		}
		parents = append(parents, decoded{obj: obj, item: &data})
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
	return items, maxVersion, nil
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
