// Package zotero defines the source contract between axiom-ng and a Zotero
// library. Zotero stays the source of truth for documents, metadata, tags and
// collections; axiom-ng is the only component that talks to it. Document
// processors never interact with Zotero directly.
package zotero

// Source is the contract for reading a Zotero library for indexing.
//
// axiom-ng runs on the same host as Zotero and resolves attachments to
// local filesystem paths, so processors receive local paths instead of a
// Zotero URL or API token.
type Source interface {
	// ServerID returns the Zotero-Server-ID that identifies this library
	// instance. Clients partition any cached state by this ID.
	ServerID() string

	// ListCollections returns the top-level library structure (collections,
	// tags) used for workspace scoping and filters.
	ListCollections() ([]Collection, error)

	// ListPDFItems returns documents that carry at least one file attachment.
	// since is the last known Last-Modified-Version; pass 0 for a full sync.
	// It returns the items and the new library version to store.
	ListPDFItems(since int64) ([]Item, int64, error)

	// ResolveAttachmentPath maps an attachment to a local filesystem path
	// (e.g. via the Zotero /file/view/url endpoint). It returns the path and
	// the attachment's content type.
	ResolveAttachmentPath(attachmentKey string) (string, error)
}

// Collection is a named container within the library.
type Collection struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Parent string `json:"parentCollection,omitempty"`
}

// Item is a top-level document (e.g. a book or journal article) with its
// attachments resolved enough to enqueue processing work.
type Item struct {
	Key             string       `json:"key"`
	Version         int64        `json:"version"`
	ItemType        string       `json:"itemType"`
	Title           string       `json:"title"`
	Creators        []Creator    `json:"creators"`
	PublicationYear *int         `json:"publicationYear,omitempty"`
	AbstractNote    string       `json:"abstractNote,omitempty"`
	Tags            []Tag        `json:"tags"`
	Collections     []string     `json:"collections"`
	Attachments     []Attachment `json:"attachments"`
	URL             string       `json:"url,omitempty"`
}

// Creator is a single author/editor of an Item.
type Creator struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

// Tag is a label assigned to an Item.
type Tag struct {
	Tag string `json:"tag"`
}

// Attachment is a concrete file (PDF/EPUB) belonging to an Item.
type Attachment struct {
	Key         string `json:"key"`
	ParentKey   string `json:"parentKey"`
	ContentType string `json:"contentType"`
	LinkMode    string `json:"linkMode"`
	Filename    string `json:"filename"`
	LocalPath   string `json:"localPath"`
}
