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
		client:    &http.Client{Timeout: 15 * time.Second},
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
// "" if it could not be obtained.
func (a *LocalAPI) ServerID() string {
	if a.serverID != "" {
		return a.serverID
	}
	resp, err := a.get(a.libraryID, nil)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	id := resp.Header.Get("Zotero-Server-ID")
	a.serverID = id
	return id
}

// zoteroItem is the flat JSON v3 representation returned by the local API.
type zoteroItem struct {
	Key         string        `json:"key"`
	Version     int64         `json:"version"`
	ItemType    string        `json:"itemType"`
	ParentKey   string        `json:"parentItem,omitempty"`
	Title       string        `json:"title,omitempty"`
	Creators    []jsonCreator `json:"creators,omitempty"`
	Tags        []jsonTag     `json:"tags,omitempty"`
	Collections []string      `json:"collections,omitempty"`
	Date        string        `json:"date,omitempty"`
	URL         string        `json:"url,omitempty"`
	LinkMode    string        `json:"linkMode,omitempty"`
	Filename    string        `json:"filename,omitempty"`
	ContentType string        `json:"contentType,omitempty"`
}

type jsonCreator struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

type jsonTag struct {
	Tag string `json:"tag"`
}

func (a *LocalAPI) getItems(query url.Values) ([]zoteroItem, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("format", "json")
	query.Set("limit", "100")
	var all []zoteroItem
	start := 0
	for {
		q := cloneValues(query)
		q.Set("start", fmt.Sprintf("%d", start))
		resp, err := a.get(a.libraryID+"/items", q)
		if err != nil {
			return nil, err
		}
		var batch []zoteroItem
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
		Key    string `json:"key"`
		Name   string `json:"name"`
		Parent string `json:"parentCollection,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("zotero collections decode: %w", err)
	}
	out := make([]Collection, 0, len(raw))
	for _, c := range raw {
		out = append(out, Collection{Key: c.Key, Name: c.Name, Parent: c.Parent})
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
	// latest library version we saw in this batch of responses
	var maxVersion int64
	// group attachments under their parent item key
	parentOf := map[string][]Attachment{}
	for _, it := range raw {
		if it.ItemType == "attachment" {
			if it.ParentKey == "" {
				continue
			}
			if it.ContentType != "application/pdf" && !strings.HasSuffix(strings.ToLower(it.Filename), ".epub") {
				continue
			}
			parentOf[it.ParentKey] = append(parentOf[it.ParentKey], Attachment{
				Key:         it.Key,
				ParentKey:   it.ParentKey,
				ContentType: it.ContentType,
				LinkMode:    it.LinkMode,
				Filename:    it.Filename,
			})
		}
		if it.Version > maxVersion {
			maxVersion = it.Version
		}
	}
	items := make([]Item, 0, len(parentOf))
	for _, it := range raw {
		if it.ItemType == "attachment" {
			continue
		}
		atts := parentOf[it.Key]
		if len(atts) == 0 {
			continue
		}
		var creat []Creator
		for _, c := range it.Creators {
			creat = append(creat, Creator{FirstName: c.FirstName, LastName: c.LastName})
		}
		var tags []Tag
		for _, t := range it.Tags {
			tags = append(tags, Tag{Tag: t.Tag})
		}
		items = append(items, Item{
			Key:         it.Key,
			Version:     it.Version,
			ItemType:    it.ItemType,
			Title:       it.Title,
			Creators:    creat,
			Tags:        tags,
			Collections: it.Collections,
			URL:         it.URL,
			Attachments: atts,
		})
	}
	return items, maxVersion, nil
}

// ResolveAttachmentPath returns the local filesystem path of an attachment via
// the Zotero /file/view/url endpoint, which returns the URI as plain text.
func (a *LocalAPI) ResolveAttachmentPath(attachmentKey string) (string, error) {
	path := fmt.Sprintf("%s/items/%s/file/view/url", a.libraryID, attachmentKey)
	resp, err := a.get(path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return "", fmt.Errorf("zotero file/view/url: %w", err)
	}
	text := strings.TrimSpace(string(b))
	if strings.HasPrefix(text, "file://") {
		return text, nil
	}
	return "", fmt.Errorf("zotero file/view/url returned non-file uri: %q", text)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
