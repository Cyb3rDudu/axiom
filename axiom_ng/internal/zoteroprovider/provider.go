// provider.go — Zotero behind the F06 Library ports (F07, #301).
//
// This is the compiled-in adapter the Library service is constructed
// with. Everything Zotero lives in this package (the seam the import
// lint guards); the service speaks only the port vocabulary.
//
// Port capability matrix (honest — unsupported is reported, never emulated):
//
//	CatalogReader      full pagination internally (every Zotero page
//	                   followed, no first-page shortcut); serves one
//	                   CONSISTENT snapshot per catalog walk — page tokens
//	                   carry the snapshot GENERATION, so an invalidation
//	                   (own mutation) or TTL rebuild (external edits)
//	                   mid-walk fails the NEXT page loudly (Conflict —
//	                   restart the scan) instead of silently mixing
//	                   generations; a dedup scan can never skip records
//	                   that way. Records carry their attachments and
//	                   COLLECTION KEYS as memberships; ContentHash is the
//	                   axiom-sha256 tag of imported files, "" for foreign
//	                   attachments (honest: Zotero exposes md5, not our
//	                   sha256).
//	RecordWriter      EnsureRecord via anchor ledger + axiom-imp tag;
//	               diverging fields on an anchored record are a
//	                   versioned update (PUT + If-Unmodified-Since-Version).
//	RenditionWriter   official 3-phase upload (write.go); idempotent by
//	                   (parent, sha256) anchor + axiom-sha256 tag;
//	                   EnsureMembership is a versioned item update.
//	CollectionWriter  ResolvePath parent-first over the real collection
//	                   tree; same-named siblings under one parent are a
//	                   conflict (library.ErrProviderConflict), never a
//	                   pick; creations are parent-first with readback.
//	BibliographicResolver  crossref.go / openlibrary.go (real HTTP).
//	DocumentInspector      inspector.go (PDF only; EPUB reports empty).
//
// Write discipline (the #301 versioned write contract): every mutation is
// optimistic-concurrency guarded where Zotero offers versions, followed by
// a readback that proves what the provider PERSISTED (item type, fields,
// membership, attachment parent, retrievable file with matching digest),
// followed by ONE audit row (library_write_audit — mutations : audit rows
// are 1:1). Zotero's 412 becomes *VersionConflictError (typed, retryable):
// the last line of defense after the single-writer lease — a concurrent
// writer's change is never silently overwritten.
//
// Single-writer: constructing a WRITE-capable provider ACQUIRES the
// provider-scoped writer lease (library.WriterLeaseConflict refuses a
// second instance at start, cross-process via the shared Library
// persistence) and renews it until Close. A LOST lease (renewal failed —
// taken over after silence) stops the writer for good: the write ports
// refuse with Conflict until Close (no logging exists in the provider —
// the refusal is the signal). Read-only providers may coexist (no lease).
package zoteroprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/jackc/pgx/v5"
)

// ProviderStore is the adapter's persistence surface (implemented by
// *library.Store; schema/0002). Test doubles capture the audit stream.
type ProviderStore interface {
	AcquireWriterLease(ctx context.Context, scope, owner string, ttl time.Duration) error
	RenewWriterLease(ctx context.Context, scope, owner string) error
	ReleaseWriterLease(ctx context.Context, scope, owner string) error
	AppendWriteAudit(ctx context.Context, r library.WriteAuditRow) error
	LookupProviderAnchor(ctx context.Context, scope, kind, anchor string) (string, int64, error)
	PutProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string, version int64) (string, error)
	// EvictProviderAnchor drops a row whose provider id proved DEAD (the
	// item vanished from Zotero) so the next ensure can re-anchor fresh.
	EvictProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string) error
}

// VersionConflictError reports a Zotero 412 (If-Unmodified-Since-Version
// guard): another writer changed the target since it was read. Typed and
// RETRYABLE — the saga re-reads the fresh state and re-applies; nothing of
// ours landed (the guard fired before the mutation).
type VersionConflictError struct {
	Key  string
	What string
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("zotero version conflict on %s (%s): a concurrent writer changed the item — re-read and retry", e.Key, e.What)
}

// mapWriteErr classifies adapter write errors: a Zotero 412 becomes the
// typed retryable conflict; anything classed passes through.
func mapWriteErr(err error, key, what string) error {
	if err == nil {
		return nil
	}
	if IsVersionConflict(err) {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			&VersionConflictError{Key: key, What: what}, what)
	}
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassNotFound, err, what)
	}
	return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, what)
}

// ---------------------------------------------------------------------------
// The provider

// Options configures the provider (constructed by the composition root;
// env reads live THERE, not here).
type Options struct {
	// BaseURL is the Zotero local API base INCLUDING /api (the read
	// client's shape, e.g. http://localhost:23119/api).
	BaseURL string
	// LibraryID is the read-side library prefix (users/0). The write
	// surface targets the local user library.
	LibraryID string
	// APIKey is the local-API write key; "" constructs a READ-ONLY
	// provider (write ports report Unavailable — capability-honest).
	APIKey string
	// Store carries lease, audit and anchors (required).
	Store ProviderStore
	// Owner identifies this writer in the lease (host:pid:nonce).
	Owner string
	// LeaseTTL overrides the lease TTL (tests); <=0 = library default.
	LeaseTTL time.Duration
	// HTTPClient overrides both clients (tests).
	HTTPClient *http.Client
}

// Provider implements the Library ports against the Zotero local API.
// Construct with New; ALWAYS Close a write-capable provider (releases the
// lease; the TTL covers the ungraceful exits).
type Provider struct {
	scope  string
	read   *LocalAPI
	write  *WriteClient // nil = read-only
	store  ProviderStore
	owner  string
	ttl    time.Duration
	cancel context.CancelFunc

	mu     sync.Mutex
	cat    *catalogSnapshot
	catGen uint64 // last-assigned snapshot generation (bumped per rebuild)

	lostLease atomic.Bool // set when a renewal failed: writes refuse (Conflict)
}

// catalogTTL bounds snapshot staleness from EXTERNAL edits (Zotero UI
// changes are invisible to the invalidation on our own mutations).
const catalogTTL = 30 * time.Second

// catalogPageSize is the catalog walk page size the adapter serves.
const catalogPageSize = 100

type catalogSnapshot struct {
	gen     uint64
	built   time.Time
	records []library.CatalogRecord
}

// New builds the provider. A write-capable construction (APIKey set)
// ACQUIRES the provider-scoped writer lease and refuses (typed Conflict)
// while another live writer holds it — the #301 single-writer declaration
// at component start. The heartbeat renews at TTL/3 until Close.
func New(ctx context.Context, o Options) (*Provider, error) {
	if o.Store == nil {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "zoteroprovider: store is required")
	}
	libID := o.LibraryID
	if libID == "" {
		libID = "users/0"
	}
	var readOpts []LocalAPIOption
	if o.HTTPClient != nil {
		readOpts = append(readOpts, WithHTTPClient(o.HTTPClient))
	}
	read := NewLocalAPI(o.BaseURL, libID, readOpts...)
	p := &Provider{
		scope: LeaseScope(o.BaseURL, libID),
		read:  read,
		store: o.Store,
		owner: o.Owner,
		ttl:   o.LeaseTTL,
	}
	if p.owner == "" {
		p.owner = defaultOwner()
	}
	if p.ttl <= 0 {
		p.ttl = library.DefaultWriterLeaseTTL
	}
	if o.APIKey == "" {
		return p, nil // read-only: no lease, write ports report Unavailable
	}
	// The write surface targets the local user library exclusively
	// (every write path is /api/users/0/...). A write-capable provider
	// bound to any OTHER read library would hold a lease for a scope its
	// own writes never touch — the guard must cover the WRITE target, so
	// such a construction is refused instead of silently split-brained
	// (two instances, two scopes, one physical library).
	if libID != "users/0" {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
			"zotero write surface targets users/0 only (got LibraryID "+libID+") — a write-capable provider cannot be bound to another library")
	}
	writeBase := strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(o.BaseURL, "/"), "/api"), "/")
	p.write = NewWriteClient(writeBase, read.ServerID(), o.APIKey)
	if o.HTTPClient != nil {
		p.write.HTTP = o.HTTPClient
	}
	if err := p.store.AcquireWriterLease(ctx, p.scope, p.owner, p.ttl); err != nil {
		return nil, err
	}
	hbCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.cancel = cancel
	go p.heartbeat(hbCtx)
	return p, nil
}

// Close releases the writer lease (graceful stop). Idempotent.
func (p *Provider) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.write == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.store.ReleaseWriterLease(ctx, p.scope, p.owner)
}

// heartbeat renews the lease at TTL/3 until the provider closes. A
// renewal refused with Conflict (the lease was taken over after
// silence) latches the lost-lease flag and STOPS renewing — the write
// ports refuse everything from there (writeable), and Zotero's
// optimistic versioning is the last line for any write already in
// flight. TRANSIENT renewal errors (network, deadline) keep retrying:
// a single blip must not kill a healthy writer (fail-closed only on
// the definitive signal).
func (p *Provider) heartbeat(ctx context.Context) {
	t := time.NewTicker(p.ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := p.store.RenewWriterLease(rctx, p.scope, p.owner)
			cancel()
			if err != nil && contractClassIs(err, contracterr.ClassConflict) {
				p.lostLease.Store(true)
				return
			}
		}
	}
}

// LeaseScope is the provider-scoped writer-lease identity. Trailing
// slashes normalize FIRST so base, base/ and base/api/ share one scope.
func LeaseScope(baseURL, libraryID string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/api"), "/")
	return "zotero|" + base + "|" + libraryID
}

func defaultOwner() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), time.Now().UTC().Format("20060102T150405.000"))
}

// ---------------------------------------------------------------------------
// Zotero item shape

type zotCreator struct {
	CreatorType string `json:"creatorType"`
	FirstName   string `json:"firstName,omitempty"`
	LastName    string `json:"lastName,omitempty"`
	Name        string `json:"name,omitempty"`
}

type zotTag struct {
	Tag string `json:"tag"`
}

// zotItem is the item's data object — the adapter's read/write shape.
type zotItem struct {
	Key         string       `json:"key,omitempty"`
	Version     int64        `json:"version,omitempty"`
	ItemType    string       `json:"itemType"`
	ParentItem  string       `json:"parentItem,omitempty"`
	Title       string       `json:"title,omitempty"`
	Creators    []zotCreator `json:"creators,omitempty"`
	Date        string       `json:"date,omitempty"`
	Publisher   string       `json:"publisher,omitempty"`
	Language    string       `json:"language,omitempty"`
	DOI         string       `json:"DOI,omitempty"`
	ISBN        string       `json:"ISBN,omitempty"`
	URL         string       `json:"url,omitempty"`
	AccessDate  string       `json:"accessDate,omitempty"`
	Collections []string     `json:"collections,omitempty"`
	Tags        []zotTag     `json:"tags,omitempty"`
	// Deleted: Zotero answers a trashed item's GET with 200 + data.deleted
	// (live-probed as a JSON true; the item LIST excludes trash, but
	// single-item reads do not). Decoded TOLERANTLY: some Zotero shapes
	// send integer truthiness — a strict bool decode would fail the whole
	// unmarshal and turn every trashed-item readback into an Internal
	// error instead of the heal path. Every aliveness readback must treat
	// deleted=true as absent.
	Deleted rawDeleted `json:"deleted,omitempty"`
	// attachment-only fields
	LinkMode    string `json:"linkMode,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Filename    string `json:"filename,omitempty"`
	MD5         string `json:"md5,omitempty"`
}

// rawDeleted is the tolerant wire form of a Zotero deletion flag:
// JSON true/false and any JSON number (nonzero = true) decode; anything
// else decodes false instead of failing the enclosing unmarshal.
type rawDeleted bool

func (r *rawDeleted) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "true":
		*r = true
	case s == "false", s == "null", s == "":
		*r = false
	default:
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			*r = n != 0
		} else {
			*r = false
		}
	}
	return nil
}

// renditionBrokenError marks a readback failure caused by the
// ATTACHMENT ITSELF being unusable (file-less leftover, wrong parent,
// filename/content-type divergence, digest mismatch) — as opposed to a
// transient transport failure. Callers treat it like NotFound: evict
// (anchored path) or skip adoption (tag-search path) and heal forward.
type renditionBrokenError struct{ msg string }

func (e *renditionBrokenError) Error() string { return e.msg }

func renditionBroken(format string, args ...any) error {
	return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
		&renditionBrokenError{msg: fmt.Sprintf(format, args...)}, "rendition readback")
}

func creatorsFromDraft(authors []library.Creator) []zotCreator {
	var out []zotCreator
	for _, a := range authors {
		z := zotCreator{CreatorType: firstNonEmptyStr(a.CreatorType, "author")}
		if a.Name != "" {
			z.Name = a.Name
		} else {
			z.FirstName, z.LastName = a.FirstName, a.LastName
		}
		out = append(out, z)
	}
	return out
}

func creatorsToDomain(cs []zotCreator) []library.Creator {
	var out []library.Creator
	for _, c := range cs {
		out = append(out, library.Creator{
			FirstName: c.FirstName, LastName: c.LastName, Name: c.Name,
			CreatorType: firstNonEmptyStr(c.CreatorType, "author"),
		})
	}
	return out
}

// yearOfDate extracts the leading 4-digit year from a Zotero date field
// ("2020", "2020-06", "June 2020"); 0 when none.
func yearOfDate(d string) int {
	for _, part := range strings.FieldsFunc(d, func(r rune) bool { return r == '-' || r == ' ' || r == '/' }) {
		if y, err := strconv.Atoi(part); err == nil && y >= 1000 && y <= 3000 {
			return y
		}
	}
	return 0
}

func hasTag(tags []zotTag, prefix string) (string, bool) {
	for _, t := range tags {
		if strings.HasPrefix(t.Tag, prefix) {
			return strings.TrimPrefix(t.Tag, prefix), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// CatalogReader — full pagination internally, consistent snapshot per walk

// ListRecords implements library.CatalogReader. The snapshot is rebuilt
// on the first page of a walk when invalidated (own mutations) or stale
// (TTL — external edits); every page token of a walk carries the snapshot
// GENERATION, so pages are pinned to the state the walk started from. A
// rebuild in between (own mutation elsewhere, TTL expiry) makes the next
// page of the OLD walk fail loudly — Conflict, restart the scan — never a
// silent mix of two generations (a dedup scan must not skip records).
func (p *Provider) ListRecords(ctx context.Context, pageToken string) (library.CatalogPage, error) {
	snap, err := p.snapshot(ctx)
	if err != nil {
		return library.CatalogPage{}, err
	}
	start := 0
	if pageToken != "" {
		gen, off, ok := parseCatalogToken(pageToken)
		if !ok || off < 0 {
			return library.CatalogPage{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "bad catalog page token "+pageToken)
		}
		if gen != snap.gen {
			return library.CatalogPage{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
				fmt.Sprintf("catalog snapshot changed during walk (token gen %d, current gen %d) — restart the scan", gen, snap.gen))
		}
		start = off
	}
	end := start + catalogPageSize
	if end > len(snap.records) {
		end = len(snap.records)
	}
	if start > len(snap.records) {
		start = len(snap.records)
	}
	page := library.CatalogPage{Records: append([]library.CatalogRecord(nil), snap.records[start:end]...)}
	if end < len(snap.records) {
		page.NextPageToken = strconv.FormatUint(snap.gen, 10) + ":" + strconv.Itoa(end)
	}
	return page, nil
}

// parseCatalogToken splits "<generation>:<offset>" (the walk pin).
func parseCatalogToken(tok string) (uint64, int, bool) {
	genS, offS, ok := strings.Cut(tok, ":")
	if !ok {
		return 0, 0, false
	}
	gen, err := strconv.ParseUint(genS, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	off, err := strconv.Atoi(offS)
	if err != nil {
		return 0, 0, false
	}
	return gen, off, true
}

func (p *Provider) snapshot(ctx context.Context) (*catalogSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cat != nil && time.Since(p.cat.built) < catalogTTL {
		return p.cat, nil
	}
	batch, err := p.read.ListCanonicalItems(0)
	if err != nil {
		return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, "zotero catalog snapshot")
	}
	docs := map[string]*library.CatalogRecord{}
	var order []string
	for _, it := range batch.Items {
		// Attachments fold into parents below; notes and annotations are
		// not documents (annotations multiply per PDF read — never records).
		if it.ItemType == "attachment" || it.ItemType == "note" || it.ItemType == "annotation" {
			continue
		}
		var d zotItem
		if err := json.Unmarshal(it.Data, &d); err != nil {
			return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "zotero item decode "+it.Key)
		}
		rec := &library.CatalogRecord{
			ProviderRecordID: it.Key,
			RecordID:         it.Key,
			RecordType:       d.ItemType,
			Title:            d.Title,
			Authors:          creatorsToDomain(d.Creators),
			DOI:              library.NormalizeDOI(d.DOI),
			ISBN:             library.NormalizeISBN(d.ISBN),
			Collections:      append([]string(nil), d.Collections...),
		}
		if y := yearOfDate(d.Date); y > 0 {
			yy := y
			rec.Year = &yy
		}
		docs[it.Key] = rec
		order = append(order, it.Key)
	}
	// Attachments onto their parents (orphan attachments — parent deleted
	// or not a document — are skipped: not part of any record's renditions).
	for _, it := range batch.Items {
		if it.ItemType != "attachment" {
			continue
		}
		var d zotItem
		if err := json.Unmarshal(it.Data, &d); err != nil {
			continue // an undecodable attachment must not sink the catalog
		}
		parent, ok := docs[d.ParentItem]
		if !ok {
			continue
		}
		hash, _ := hasTag(d.Tags, "axiom-sha256:")
		parent.Renditions = append(parent.Renditions, library.CatalogRendition{
			ProviderAttachmentID: it.Key,
			RenditionID:          it.Key,
			ContentHash:          hash, // "" for foreign attachments — honest
			MediaType:            d.ContentType,
			Filename:             d.Filename,
		})
	}
	records := make([]library.CatalogRecord, 0, len(order))
	for _, k := range order {
		sort.Slice(docs[k].Renditions, func(i, j int) bool {
			return docs[k].Renditions[i].ProviderAttachmentID < docs[k].Renditions[j].ProviderAttachmentID
		})
		records = append(records, *docs[k])
	}
	p.catGen++
	p.cat = &catalogSnapshot{gen: p.catGen, built: time.Now(), records: records}
	return p.cat, nil
}

// invalidate drops the snapshot after a successful own mutation.
func (p *Provider) invalidate() {
	p.mu.Lock()
	p.cat = nil
	p.mu.Unlock()
}

// ---------------------------------------------------------------------------
// RecordWriter

// writeable guards the write ports of a read-only construction and of a
// writer whose lease was lost (latched by the heartbeat — a taken-over
// lease must not write on, not even once).
func (p *Provider) writeable(what string) error {
	if p.lostLease.Load() {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"writer lease lost (renewal failed; scope "+p.scope+") — refusing "+what)
	}
	if p.write == nil {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"zotero provider is read-only (no write key): "+what+" unsupported")
	}
	return nil
}

// EnsureRecord implements library.RecordWriter: idempotent by the external
// key (anchor ledger first; the axiom-imp tag closes the crash window
// between the Zotero write and the ledger insert). A record whose fields
// diverged gets a versioned update; every path readbacks what Zotero
// persisted and appends exactly one audit row.
func (p *Provider) EnsureRecord(ctx context.Context, d library.RecordDraft) (string, error) {
	if err := p.writeable("ensure_record"); err != nil {
		return "", err
	}
	if d.ExternalKey == "" || d.Title == "" {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "ensure_record: external key and title are required")
	}
	anchor := d.ExternalKey
	tag := "axiom-imp:" + anchor

	// 1. Anchor ledger (fast path). A NotFound-class readback (the
	// anchored record was deleted externally — purged OR trashed, both
	// surface the same way) heals forward: evict the stale row and fall
	// through to tag-search/create. Eviction errors propagate — a failed
	// eviction would make the create below return the dead row's id.
	if id, _, err := p.store.LookupProviderAnchor(ctx, p.scope, "record", anchor); err == nil && id != "" {
		ok, _, rerr := p.readbackRecord(ctx, id, d)
		if rerr == nil && ok {
			if err := p.audit(ctx, "ensure_record", anchor, id, "reused", map[string]any{"title": d.Title}); err != nil {
				return "", err
			}
			return id, nil
		}
		if rerr != nil && !contractClassIs(rerr, contracterr.ClassNotFound) {
			return "", rerr
		}
		if rerr != nil {
			if eerr := p.store.EvictProviderAnchor(ctx, p.scope, "record", anchor, id); eerr != nil {
				return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, eerr,
					"evicting stale record anchor "+anchor+" — refusing to continue with a dead anchor in place")
			}
		}
		if rerr == nil && !ok {
			// Anchored but diverged/persisted differently: versioned update.
			return p.updateRecord(ctx, id, d, anchor)
		}
	} else if err != nil && !errors.Is(err, errAnchorAbsent) {
		return "", err
	}

	// 2. Tag search (crash window: Zotero write landed, ledger did not).
	if found, err := p.findTaggedRecord(ctx, tag); err != nil {
		return "", err
	} else if found != "" {
		ok, version, rerr := p.readbackRecord(ctx, found, d)
		if rerr != nil {
			return "", rerr
		}
		if !ok {
			return p.updateRecord(ctx, found, d, anchor)
		}
		if _, err := p.store.PutProviderAnchor(ctx, p.scope, "record", anchor, found, version); err != nil {
			return "", err
		}
		if err := p.audit(ctx, "ensure_record", anchor, found, "reused", map[string]any{"recovered_by": "tag_search"}); err != nil {
			return "", err
		}
		return found, nil
	}

	// 3. Create.
	item := zotItem{
		ItemType:   firstNonEmptyStr(d.RecordType, "document"),
		Title:      d.Title,
		Creators:   creatorsFromDraft(d.Authors),
		Publisher:  d.Publisher,
		Language:   d.Language,
		DOI:        library.NormalizeDOI(d.DOI),
		ISBN:       library.NormalizeISBN(d.ISBN),
		URL:        d.URL,
		AccessDate: d.AccessDate,
		Tags:       []zotTag{{Tag: tag}},
	}
	if y := yearPtrValue(d.Year); y > 0 {
		item.Date = strconv.Itoa(y)
	}
	body, _ := json.Marshal([]zotItem{item})
	raw, _, err := p.write.do(http.MethodPost, "/api/users/0/items",
		map[string]string{"Content-Type": "application/json"}, strings.NewReader(string(body)))
	if err != nil {
		return "", mapWriteErr(err, "", "ensure_record create")
	}
	key, err := createdKey(raw)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "ensure_record create response")
	}
	// Readback: what Zotero persisted must match the draft (type+title
	// + the non-empty draft fields + the anchor tag marker).
	if ok, version, rerr := p.readbackRecord(ctx, key, d); rerr != nil {
		return "", rerr
	} else if !ok {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"ensure_record readback mismatch for "+key+" — Zotero persisted different type/title")
	} else {
		surviving, err := p.store.PutProviderAnchor(ctx, p.scope, "record", anchor, key, version)
		if err != nil {
			return "", err
		}
		if surviving != key {
			// The anchor row points elsewhere while OUR fresh create is
			// verified — a ledger race the single-writer lease should make
			// impossible. Loud (mirroring the rendition race), never a
			// silent reused with an unverified id.
			return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
				fmt.Sprintf("record anchor %s raced: ledger holds %s while %s was freshly verified — manual reconcile", anchor, surviving, key))
		}
	}
	p.invalidate()
	if err := p.audit(ctx, "ensure_record", anchor, key, "created", map[string]any{"item_type": item.ItemType, "title": d.Title}); err != nil {
		return "", err
	}
	return key, nil
}

// findTaggedRecord searches items by the anchor tag. Verification is
// EXACT (t.Tag == tag): an instance that ignores the tag filter and
// returns a decoy carrying some OTHER axiom-imp:<…> tag must not be
// adopted — the anchor is the identity, a prefix is not.
func (p *Provider) findTaggedRecord(ctx context.Context, tag string) (string, error) {
	raw, err := p.read.getItems(ctx, url.Values{"tag": {tag}})
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, "ensure_record tag search")
	}
	for _, env := range raw {
		it, ok := itemFromEnvelope(env)
		if !ok || it.ItemType == "attachment" || it.ItemType == "note" {
			continue
		}
		if it.Key == "" {
			continue
		}
		for _, t := range it.Tags {
			if t.Tag == tag {
				return it.Key, nil
			}
		}
	}
	return "", nil
}

// itemFromEnvelope decodes an item envelope into the data shape; false
// when the envelope carries no decodable data object.
func itemFromEnvelope(env []byte) (zotItem, bool) {
	var holder struct {
		Key  string          `json:"key"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(env, &holder) != nil || len(holder.Data) == 0 {
		return zotItem{}, false
	}
	var it zotItem
	if json.Unmarshal(holder.Data, &it) != nil {
		return zotItem{}, false
	}
	if it.Key == "" {
		it.Key = holder.Key
	}
	return it, true
}

// readbackRecord verifies the persisted record against the draft: item
// type, title, the identifiers and every NON-EMPTY draft field the
// adapter claims to write (publisher/language/url), plus the anchor-tag
// marker. Trashed records (GET answers 200 + data.deleted) count as
// ABSENT — the NotFound error tells the caller to heal. false without
// error = the record exists but diverged (caller: versioned update).
// The returned version is what Zotero persisted (the anchor ledger's
// provider_version).
func (p *Provider) readbackRecord(ctx context.Context, key string, d library.RecordDraft) (bool, int64, error) {
	data, verStr, err := p.write.GetItem(key)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return false, 0, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
				"anchored record "+key+" vanished from Zotero (deleted externally) — recreate by dropping the anchor")
		}
		return false, 0, mapWriteErr(err, key, "ensure_record readback")
	}
	var it zotItem
	if err := json.Unmarshal(data, &it); err != nil {
		return false, 0, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "readback decode "+key)
	}
	if it.Deleted {
		return false, 0, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
			"anchored record "+key+" sits in the Zotero trash — treat as vanished")
	}
	if _, tagged := hasTag(it.Tags, "axiom-imp:"); !tagged {
		return false, versionOf(verStr), nil // ANY axiom-imp marker passes; the update re-stamps the exact one
	}
	same := it.ItemType == firstNonEmptyStr(d.RecordType, "document") &&
		it.Title == d.Title &&
		(d.DOI == "" || library.NormalizeDOI(it.DOI) == library.NormalizeDOI(d.DOI)) &&
		(d.ISBN == "" || library.NormalizeISBN(it.ISBN) == library.NormalizeISBN(d.ISBN)) &&
		(d.Publisher == "" || it.Publisher == d.Publisher) &&
		(d.Language == "" || it.Language == d.Language) &&
		(d.URL == "" || it.URL == d.URL)
	return same, versionOf(verStr), nil
}

// updateRecord applies diverged fields under the optimistic-versioning
// guard (PUT + If-Unmodified-Since-Version), then readbacks. The PUT
// body is the item's RAW data with ONLY the draft's fields overlaid —
// unmodeled fields (abstractNote, pages, volume, extra, …) survive; a
// typed struct round-trip would strip them.
func (p *Provider) updateRecord(ctx context.Context, key string, d library.RecordDraft, anchor string) (string, error) {
	data, version, err := p.write.GetItem(key)
	if err != nil {
		return "", mapWriteErr(err, key, "ensure_record update read")
	}
	// Merge ONLY the draft's non-empty fields onto the raw item — an
	// empty draft value (non-web intake carries no URL; a book draft may
	// carry no publisher) must never blank what the provider already
	// holds. Title is always carried (EnsureRecord requires it).
	changes := map[string]any{"title": d.Title}
	if len(d.Authors) > 0 {
		changes["creators"] = jsonValue(creatorsFromDraft(d.Authors))
	}
	if d.Publisher != "" {
		changes["publisher"] = d.Publisher
	}
	if d.Language != "" {
		changes["language"] = d.Language
	}
	if d.DOI != "" {
		changes["DOI"] = library.NormalizeDOI(d.DOI)
	}
	if d.ISBN != "" {
		changes["ISBN"] = library.NormalizeISBN(d.ISBN)
	}
	if d.URL != "" {
		changes["url"] = d.URL
	}
	if d.AccessDate != "" {
		changes["accessDate"] = d.AccessDate
	}
	if y := yearPtrValue(d.Year); y > 0 {
		changes["date"] = strconv.Itoa(y)
	}
	// Anchor marker: re-stamp when missing (user-created items adopted
	// via tag-less anchors). User tags are PRESERVED — the raw tag list
	// is decoded losslessly (type field included) and extended, never
	// replaced.
	var cur struct {
		Tags []map[string]any `json:"tags"`
	}
	if json.Unmarshal(data, &cur) == nil {
		tagged := false
		for _, t := range cur.Tags {
			if s, _ := t["tag"].(string); strings.HasPrefix(s, "axiom-imp:") {
				tagged = true
			}
		}
		if !tagged {
			cur.Tags = append(cur.Tags, map[string]any{"tag": "axiom-imp:" + anchor})
			changes["tags"] = cur.Tags
		}
	}
	body, merr := mergeItemJSON(data, changes)
	if merr != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, merr, "update merge "+key)
	}
	if err := p.write.PutItem(key, body, version); err != nil {
		return "", mapWriteErr(err, key, "ensure_record update")
	}
	ok, newVersion, rerr := p.readbackRecord(ctx, key, d)
	if rerr != nil {
		return "", rerr
	}
	if !ok {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"ensure_record update readback mismatch for "+key)
	}
	if _, err := p.store.PutProviderAnchor(ctx, p.scope, "record", anchor, key, newVersion); err != nil {
		return "", err
	}
	p.invalidate()
	if err := p.audit(ctx, "ensure_record", anchor, key, "changed", map[string]any{"fields": "versioned update"}); err != nil {
		return "", err
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// RenditionWriter

// EnsureRendition implements library.RenditionWriter: official 3-phase
// attachment upload under the parent, idempotent by (parent, sha256).
func (p *Provider) EnsureRendition(ctx context.Context, d library.RenditionDraft) (string, error) {
	if err := p.writeable("ensure_rendition"); err != nil {
		return "", err
	}
	if d.ParentProviderID == "" || d.ContentHash == "" || d.StagingPath == "" {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "ensure_rendition: parent, content hash and staging path are required")
	}
	anchor := d.ParentProviderID + "|" + d.ContentHash

	// 1. Anchor ledger (fast path) with live verification. Heal-worthy
	// readback failures (the anchored attachment VANISHED — purged,
	// trashed — or is BROKEN per *renditionBrokenError) evict the stale
	// row (guarded — only this exact row) so the re-upload below
	// re-anchors fresh instead of returning a dead id; a TRANSIENT error
	// (transport, store) propagates — evicting a healthy anchor over a
	// blip would duplicate uploads. Eviction errors PROPAGATE: with the
	// stale row still in place the fresh insert would silently lose to it
	// (ON CONFLICT DO NOTHING) and hand back the dead id — exactly the
	// state this healing exists to prevent.
	if id, _, err := p.store.LookupProviderAnchor(ctx, p.scope, "rendition", anchor); err == nil && id != "" {
		if _, _, rerr := p.readbackRendition(ctx, id, d, false); rerr == nil {
			if aerr := p.audit(ctx, "ensure_rendition", anchor, id, "reused", nil); aerr != nil {
				return "", aerr
			}
			return id, nil
		} else if healWorthy(rerr) {
			if eerr := p.store.EvictProviderAnchor(ctx, p.scope, "rendition", anchor, id); eerr != nil {
				return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, eerr,
					"evicting stale rendition anchor "+anchor+" — refusing to continue with a dead anchor in place")
			}
		} else {
			return "", rerr
		}
	} else if err != nil && !errors.Is(err, errAnchorAbsent) {
		return "", err
	}

	// 2. Tag search (crash window between upload and ledger insert). The
	// tag is stamped in phase 0 of the upload — BEFORE the file flow — so
	// a kill in between leaves a TAGGED BUT FILE-LESS attachment behind.
	// Such a leftover is adopted only when the FULL readback proves it
	// (parent, filename, retrievable file with matching digest); a
	// heal-worthy failure (absent or *renditionBrokenError) SKIPS the
	// adoption and a complete fresh attachment is created below (the
	// empty item stays for the repair track); a transient error
	// propagates instead of silently forking into a duplicate upload.
	if found, err := p.findTaggedAttachment(ctx, d.ContentHash, d.ParentProviderID); err != nil {
		return "", err
	} else if found != "" {
		if _, version, rerr := p.readbackRendition(ctx, found, d, true); rerr == nil {
			if _, err := p.store.PutProviderAnchor(ctx, p.scope, "rendition", anchor, found, version); err != nil {
				return "", err
			}
			if err := p.audit(ctx, "ensure_rendition", anchor, found, "reused", map[string]any{"recovered_by": "tag_search"}); err != nil {
				return "", err
			}
			return found, nil
		} else if !healWorthy(rerr) {
			return "", rerr
		}
	}

	// 3. The official 3-phase upload (write.go), carrying the sha anchor tag.
	content, err := readStaged(d.StagingPath)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "ensure_rendition read staged file")
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != d.ContentHash {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"staged file does not match its declared content hash — refusing to upload")
	}
	key, err := p.write.CreateAttachmentWithFile(d.ParentProviderID, d.Filename, d.MediaType, content, "axiom-sha256:"+d.ContentHash)
	if err != nil {
		return "", mapWriteErr(err, "", "ensure_rendition upload")
	}
	observed, version, err := p.readbackRendition(ctx, key, d, true)
	if err != nil {
		return "", err
	}
	surviving, err := p.store.PutProviderAnchor(ctx, p.scope, "rendition", anchor, key, version)
	if err != nil {
		return "", err
	}
	if surviving != key {
		// The anchor row points elsewhere while OUR fresh upload is
		// verified — a ledger race the single-writer lease should make
		// impossible. Loud, never a silent "reused" with an unverified id.
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			fmt.Sprintf("rendition anchor %s raced: ledger holds %s while %s was freshly verified — manual reconcile", anchor, surviving, key))
	}
	p.invalidate()
	if err := p.audit(ctx, "ensure_rendition", anchor, key, "created", map[string]any{
		"filename": d.Filename, "media_type": d.MediaType,
		"size_observed": observed, "sha256": d.ContentHash, // OBSERVED bytes, not the declared staging size
	}); err != nil {
		return "", err
	}
	return key, nil
}

// readbackRendition proves the attachment: correct parent (alive, not
// an attachment itself, not trashed), filename/content type, retrievable
// file with matching sha256 (Zotero stores md5; we verify OUR digest of
// the stored bytes — stronger). Trashed attachments count as ABSENT
// (the caller heals); a broken attachment (wrong parent, diverged
// metadata, missing/mismatched file) surfaces as *renditionBrokenError —
// heal-worthy like NotFound, unlike a transient transport failure.
// Returns the OBSERVED stored size (the audit's evidence) and the
// persisted version (the anchor ledger's provider_version).
func (p *Provider) readbackRendition(ctx context.Context, key string, d library.RenditionDraft, fresh bool) (observed, version int64, err error) {
	data, verStr, gerr := p.write.GetItem(key)
	if gerr != nil {
		if isStatus(gerr, http.StatusNotFound) {
			return 0, 0, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "rendition "+key+" vanished from Zotero")
		}
		return 0, 0, mapWriteErr(gerr, key, "ensure_rendition readback")
	}
	var it zotItem
	if jerr := json.Unmarshal(data, &it); jerr != nil {
		return 0, 0, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, jerr, "rendition readback decode "+key)
	}
	if it.Deleted {
		return 0, 0, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "rendition "+key+" sits in the Zotero trash")
	}
	if it.ParentItem != d.ParentProviderID {
		return 0, 0, renditionBroken("attachment %s sits under parent %s, want %s", key, it.ParentItem, d.ParentProviderID)
	}
	if fresh {
		// The parent must be alive (a real, un-trashed document item).
		pdata, _, perr := p.write.GetItem(d.ParentProviderID)
		if perr != nil {
			return 0, 0, mapWriteErr(perr, d.ParentProviderID, "rendition parent readback")
		}
		var parent zotItem
		if json.Unmarshal(pdata, &parent) != nil || parent.Deleted || parent.ItemType == "attachment" || parent.ItemType == "note" {
			return 0, 0, renditionBroken("parent %s is not a live document item", d.ParentProviderID)
		}
		if it.Filename != d.Filename {
			return 0, 0, renditionBroken("filename %s != %s", it.Filename, d.Filename)
		}
		if d.MediaType != "" && it.ContentType != "" && it.ContentType != d.MediaType {
			return 0, 0, renditionBroken("contentType %s != %s", it.ContentType, d.MediaType)
		}
		// File retrievable + digest/size prove. The local API redirects the
		// /file GET to the local storage path — the envelope's enclosure
		// link IS that path (same host as Zotero, the established premise);
		// read the bytes locally and compare OUR digest (stronger than the
		// stored md5).
		eraw, _, eerr := p.write.GetItemEnvelope(key)
		if eerr != nil {
			return 0, 0, mapWriteErr(eerr, key, "rendition file readback")
		}
		local := enclosurePath(eraw)
		if local == "" {
			return 0, 0, renditionBroken("attachment %s carries no local enclosure path (file-less leftover?)", key)
		}
		got, ferr := os.ReadFile(local)
		if ferr != nil {
			return 0, 0, renditionBroken("stored file unreadable: %s (%v)", local, ferr)
		}
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != d.ContentHash {
			return 0, 0, renditionBroken("stored file digest mismatch for %s (%s)", key, local)
		}
		return int64(len(got)), versionOf(verStr), nil
	}
	return 0, versionOf(verStr), nil
}

// versionOf parses a Last-Modified-Version string (0 when absent or
// malformed — the ledger column is diagnostic, never a guard).
func versionOf(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// enclosurePath extracts links.enclosure.href from a raw item envelope,
// maps file:/// URLs onto the local path, and percent-DECODES it (the
// href arrives URL-encoded; spaces are %20). "" when absent.
func enclosurePath(raw []byte) string {
	var env struct {
		Links struct {
			Enclosure struct {
				Href string `json:"href"`
			} `json:"enclosure"`
		} `json:"links"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	href := env.Links.Enclosure.Href
	if strings.HasPrefix(href, "file://") {
		href = strings.TrimPrefix(href, "file://")
	}
	if dec, err := url.PathUnescape(href); err == nil {
		return dec
	}
	return href
}

func (p *Provider) findTaggedAttachment(ctx context.Context, contentHash, parentKey string) (string, error) {
	raw, err := p.read.getItems(ctx, url.Values{"tag": {"axiom-sha256:" + contentHash}})
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, "rendition tag search")
	}
	for _, env := range raw {
		it, ok := itemFromEnvelope(env)
		if !ok || it.ItemType != "attachment" || it.Key == "" {
			continue
		}
		// EXACT hash + parent: a decoy under the same parent carrying a
		// different axiom-sha256 tag (server ignored the filter) must not
		// be adopted — the digest is the identity.
		if it.ParentItem == parentKey {
			if sha, tagged := hasTag(it.Tags, "axiom-sha256:"); tagged && sha == contentHash {
				return it.Key, nil
			}
		}
	}
	return "", nil
}

// EnsureMembership implements library.RenditionWriter: idempotently files
// a record under a collection — a VERSIONED item update (memberships live
// on the item), guarded and readbacked. The PUT overlays ONLY the
// collections key onto the item's raw data: unmodeled fields survive.
func (p *Provider) EnsureMembership(ctx context.Context, providerRecordID, providerCollectionID string) error {
	if err := p.writeable("ensure_membership"); err != nil {
		return err
	}
	data, version, err := p.write.GetItem(providerRecordID)
	if err != nil {
		return mapWriteErr(err, providerRecordID, "ensure_membership read")
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "membership decode "+providerRecordID)
	}
	cols, _ := m["collections"].([]any)
	for _, c := range cols {
		if s, ok := c.(string); ok && s == providerCollectionID {
			return p.audit(ctx, "ensure_membership", providerRecordID+"|"+providerCollectionID, providerRecordID, "reused", nil)
		}
	}
	m["collections"] = append(cols, providerCollectionID)
	body, _ := json.Marshal(m)
	if err := p.write.PutItem(providerRecordID, body, version); err != nil {
		return mapWriteErr(err, providerRecordID, "ensure_membership update")
	}
	// Readback: the membership is observable in the provider state.
	data2, _, err := p.write.GetItem(providerRecordID)
	if err != nil {
		return mapWriteErr(err, providerRecordID, "ensure_membership readback")
	}
	var it2 zotItem
	if err := json.Unmarshal(data2, &it2); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "membership readback decode")
	}
	found := false
	for _, c := range it2.Collections {
		if c == providerCollectionID {
			found = true
		}
	}
	if !found {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"membership readback: "+providerRecordID+" not observable under "+providerCollectionID)
	}
	p.invalidate()
	return p.audit(ctx, "ensure_membership", providerRecordID+"|"+providerCollectionID, providerRecordID, "changed", nil)
}

// ---------------------------------------------------------------------------
// CollectionWriter

// ResolvePath implements library.CollectionWriter: parent-first over the
// real collection tree; same-named siblings under one parent are a
// conflict (library.ErrProviderConflict), never a pick.
func (p *Provider) ResolvePath(ctx context.Context, segments []string, createMissing bool) (string, error) {
	if err := p.writeable("resolve_path"); err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "collection path is empty")
	}
	cols, err := p.read.ListCanonicalCollections()
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, "collection listing")
	}
	children := map[string]map[string][]string{} // parentKey -> name -> keys
	for _, c := range cols {
		if children[c.ParentKey] == nil {
			children[c.ParentKey] = map[string][]string{}
		}
		children[c.ParentKey][c.Name] = append(children[c.ParentKey][c.Name], c.Key)
	}
	parent := ""
	for _, seg := range segments {
		keys := children[parent][seg]
		switch {
		case len(keys) == 0:
			if !createMissing {
				return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
					fmt.Sprintf("collection %q not found under parent (create_missing=false)", seg))
			}
			key, cerr := p.createCollection(ctx, seg, parent)
			if cerr != nil {
				return "", cerr
			}
			if children[parent] == nil {
				children[parent] = map[string][]string{}
			}
			children[parent][seg] = []string{key}
			parent = key
		case len(keys) > 1:
			return "", fmt.Errorf("%w: %d same-named sibling collections %q under one parent",
				library.ErrProviderConflict, len(keys), seg)
		default:
			// Existing segment. In create-intent mode, adopt it into the
			// anchor ledger when unanchored: a crash between the Zotero POST
			// and the ledger/audit append left a created-but-unanchored
			// segment, and this adoption re-emits the recovered audit row
			// (the 1:1 mutation↔audit contract beyond crash-freedom).
			if createMissing {
				if aerr := p.adoptCollectionAnchor(ctx, seg, parent, keys[0]); aerr != nil {
					return "", aerr
				}
			}
			parent = keys[0]
		}
	}
	return parent, nil
}

// collectionAnchor is the ledger anchor of a collection segment:
// "<parentKey>|<name>". Zotero collection KEYS are separator-free
// 8-char ids, so the leading key makes the pair injective even when a
// collection NAME contains '|' (an empty parent is the root). The
// reverse split never happens — lookups build the key, they never parse
// it.
func collectionAnchor(parent, name string) string { return parent + "|" + name }

// adoptCollectionAnchor anchors an existing collection segment when the
// ledger lacks it, appending the adoption audit row (outcome "adopted"
// — truthful whether the segment is a torn create of ours or was always
// there: the row records the LEDGER mutation, not a Zotero write).
func (p *Provider) adoptCollectionAnchor(ctx context.Context, name, parentKey, key string) error {
	anchor := collectionAnchor(parentKey, name)
	if id, _, err := p.store.LookupProviderAnchor(ctx, p.scope, "collection", anchor); err == nil && id != "" {
		return nil
	} else if err != nil && !errors.Is(err, errAnchorAbsent) {
		return err
	}
	if _, err := p.store.PutProviderAnchor(ctx, p.scope, "collection", anchor, key, 0); err != nil {
		return err
	}
	return p.audit(ctx, "create_collection", anchor, key, "adopted", map[string]any{"parent": parentKey, "recovered": true})
}

// createCollection posts one segment (parent-first: the parent key is
// resolved before the child is created), readbacks it, and anchors it
// in the ledger (the crash-torn create is adopted on retry by
// ResolvePath's existing-segment branch).
func (p *Provider) createCollection(ctx context.Context, name, parentKey string) (string, error) {
	pc := any(false)
	if parentKey != "" {
		pc = parentKey
	}
	body, _ := json.Marshal([]map[string]any{{"name": name, "parentCollection": pc}})
	raw, err := p.write.PostCollections(body)
	if err != nil {
		return "", mapWriteErr(err, "", "create_collection "+name)
	}
	key, err := createdKey(raw)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "create_collection response "+name)
	}
	// Readback: the collection persists with name + parent.
	craw, cerr := p.write.GetCollection(key)
	if cerr != nil {
		return "", mapWriteErr(cerr, key, "create_collection readback "+name)
	}
	var env struct {
		Data struct {
			Name             string          `json:"name"`
			ParentCollection json.RawMessage `json:"parentCollection"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(craw, &env); uerr != nil || env.Data.Name != name {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"create_collection readback mismatch for "+name)
	}
	gotParent := ""
	if len(env.Data.ParentCollection) > 0 && env.Data.ParentCollection[0] == '"' {
		_ = json.Unmarshal(env.Data.ParentCollection, &gotParent)
	}
	if gotParent != parentKey {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			fmt.Sprintf("create_collection readback: parent %q != %q", gotParent, parentKey))
	}
	if _, err := p.store.PutProviderAnchor(ctx, p.scope, "collection", collectionAnchor(parentKey, name), key, 0); err != nil {
		return "", err
	}
	if err := p.audit(ctx, "create_collection", collectionAnchor(parentKey, name), key, "created", map[string]any{"parent": parentKey}); err != nil {
		return "", err
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// helpers

// audit appends the ONE write-audit row of a mutation (after readback).
func (p *Provider) audit(ctx context.Context, op, anchor, ref, outcome string, readback any) error {
	return p.store.AppendWriteAudit(ctx, library.WriteAuditRow{
		Scope: p.scope, Operation: op, Anchor: anchor, ProviderRef: ref, Outcome: outcome, Readback: readback,
	})
}

// createdKey parses the Zotero multi-item write response envelope.
func createdKey(raw []byte) (string, error) {
	var out struct {
		Successful map[string]struct {
			Key string `json:"key"`
		} `json:"successful"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Successful) == 0 {
		return "", fmt.Errorf("unexpected write response %.200s", raw)
	}
	// insertion order is "0" for single writes
	if k := out.Successful["0"].Key; k != "" {
		return k, nil
	}
	for _, v := range out.Successful {
		if v.Key != "" {
			return v.Key, nil
		}
	}
	return "", fmt.Errorf("write response carries no key %.200s", raw)
}

// contractClassIs reports whether err is a classed contract error of the
// given class (test-independent mirror of the assertions).
func contractClassIs(err error, class contracterr.Class) bool {
	got, ok := contracterr.ClassOf(err)
	return ok && got == class
}

// healWorthy reports whether a rendition readback error justifies
// healing (absent/trashed item or a broken attachment) — as opposed to
// a transient transport failure, which must propagate.
func healWorthy(err error) bool {
	if contractClassIs(err, contracterr.ClassNotFound) {
		return true
	}
	var broken *renditionBrokenError
	return errors.As(err, &broken)
}

func isStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == code
}

func readStaged(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func yearPtrValue(y *int) int {
	if y == nil {
		return 0
	}
	return *y
}

func firstNonEmptyStr(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// mergeItemJSON overlays ONLY the changed keys onto the item's raw data
// JSON and re-marshals: unmodeled fields (abstractNote, pages, extra, …)
// survive a replace-semantics PUT. key/version stay exactly as Zotero
// sent them (never invented — the guard travels in the header).
func mergeItemJSON(raw []byte, changes map[string]any) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range changes {
		m[k] = v
	}
	return json.Marshal(m)
}

// jsonValue round-trips a typed value into a JSON-generic one (map/slice/
// string/…) so it can overlay into a raw item map.
func jsonValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// errAnchorAbsent is the sentinel LookupProviderAnchor surfaces for an
// absent row (pgx.ErrNoRows aliased so the adapter reads intent).
var errAnchorAbsent = pgx.ErrNoRows

// compile-time port assertions — the Provider IS the Library's Zotero
// binding (capability-honest: read-only constructions fail the write
// ports at runtime with Unavailable).
var (
	_ library.CatalogReader    = (*Provider)(nil)
	_ library.RecordWriter     = (*Provider)(nil)
	_ library.RenditionWriter  = (*Provider)(nil)
	_ library.CollectionWriter = (*Provider)(nil)
)

// LeaseScopeLabel reports the provider's writer-lease scope (diagnostics).
func (p *Provider) LeaseScopeLabel() string { return p.scope }
