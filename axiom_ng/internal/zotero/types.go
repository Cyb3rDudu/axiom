// Package zotero defines the source contract between axiom-ng and a Zotero
// library. Zotero stays the source of truth for documents, metadata, tags and
// collections; axiom-ng is the only component that talks to it. Document
// processors never interact with Zotero directly.
package zotero

import "encoding/json"

// Source is the contract for reading a Zotero library for indexing.
//
// axiom-ng runs on the same host as Zotero and resolves attachments to
// local filesystem paths, so processors receive local paths instead of a
// Zotero URL or API token. The single sync path (POST /api/zotero/sync,
// Service.Run) drives this contract directly: there is no separate legacy
// item/PDF sync path.
type Source interface {
	// ServerID returns the Zotero-Server-ID that identifies this library
	// instance. Clients partition any cached state by this ID.
	ServerID() string

	// ListCanonicalItems returns a lossless canonical batch of items changed
	// since the given library version (0 = full snapshot). A since of 0 yields
	// FullSnapshot=true so absent items can be reconciled; incremental batches
	// deliver only changed items plus explicit delete events. Deletions are
	// sourced from the trash feed and the /deleted feed when available; when
	// the /deleted feed is unsupported an incremental batch falls back to a
	// full snapshot rather than advancing over an unknown deletion state.
	ListCanonicalItems(since int64) (CanonicalBatch, error)

	// ListCanonicalCollections returns all collections as lossless mirrors with
	// their parent hierarchy.
	ListCanonicalCollections() ([]CanonicalCollection, error)
}

// DeleteEvent is a structured record of an item removed from the library.
type DeleteEvent struct {
	Key       string
	ItemType  string
	ParentKey string // only set for attachment deletions
}

// CanonicalItem is a lossless mirror of a Zotero item. Envelope is the full
// item object (key, version, library, links, meta, data); Data is the item's
// own "data" object. Both are kept as raw JSON so unknown fields survive the
// round-trip semantically intact.
type CanonicalItem struct {
	Key       string
	Version   int64
	ItemType  string
	ParentKey string
	Envelope  json.RawMessage
	Data      json.RawMessage
}

// CanonicalBatch is the result of a canonical listing. FullSnapshot is true for
// a since==0 (full) sync and signals the caller that absent items were truly
// removed and can be marked deleted. For incremental batches FullSnapshot is
// false and only DeleteEvents represent removals; the delta Items are upserted
// without implying that anything not listed disappeared.
type CanonicalBatch struct {
	FullSnapshot bool
	Items        []CanonicalItem
	DeleteEvents []DeleteEvent
	NewVersion   int64
}

// CanonicalCollection is a lossless mirror of a Zotero collection, including
// its parent hierarchy.
type CanonicalCollection struct {
	Key       string
	Name      string
	ParentKey string
	Envelope  json.RawMessage
}

// Creator is a single author/editor of an Item, including order and role.
type Creator struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	// Name is the single-field (corporate) creator display name, when present.
	Name string `json:"name,omitempty"`
	// CreatorType preserves the role/order (e.g. author, editor).
	CreatorType string `json:"creatorType,omitempty"`
}

// Tag is a label assigned to an Item.
type Tag struct {
	Tag string `json:"tag"`
}

// Attachment is a concrete file (PDF/EPUB) belonging to an Item.
type Attachment struct {
	Key         string `json:"key"`
	Version     int64  `json:"version"`
	ParentKey   string `json:"parentKey"`
	ContentType string `json:"contentType"`
	LinkMode    string `json:"linkMode"`
	Filename    string `json:"filename"`
	LocalPath   string `json:"localPath"`
}
