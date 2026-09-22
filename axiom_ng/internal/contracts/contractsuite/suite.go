// Package contractsuite is the behavior harness of the F03 contracts
// (#297). LibrarySuite/StoreSuite run implementation-neutral probes
// against ANY library.Library / store.Store implementation — the
// reference fakes here, the extractions F06/F09, and the local/HTTP
// bindings F11 must prove parity with.
//
// Suite contract with implementers:
//
//   - The suite assumes a FRESH (or per-run isolated) implementation:
//     probes reuse fixed idempotency keys and fixture ids.
//   - StoreSuite precondition: the implementation must resolve
//     SeedRevision.ContentTicket to SeedContent (in-process via the
//     Library binding, HTTP via the wired backend) — intake fetches its
//     bytes through that ticket.
//   - Implementations MAY additionally implement FaultControl; the suite
//     then also proves error classification under induced faults.
//
// Probes return errors (they do not own a *testing.T): LibrarySuite
// wraps each in a subtest, and the mutation-sonde tests run single
// probes against deliberately broken fakes to prove the harness has
// teeth.
package contractsuite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

// FaultControl is the optional fault-injection hook. Implementations
// that satisfy it get the classification probes: the next call to the
// named method (Go method name) fails with exactly err; pre-typed
// contracterr values pass through unmapped, anything else is what the
// implementation does to internal errors (must surface as Internal).
type FaultControl interface {
	InjectFault(method string, err error)
	ClearFaults()
}

// Canonical fixtures. One content, one bibliography, one revision —
// shared by both suites so the Library→Store bridge is probed with the
// exact same artifact both sides will see in production.
const (
	// SeedToken appears in the second paragraph of SeedContent ONLY.
	SeedToken = "Xylophonquarz"
	// DefaultCitationStyle is the style LibrarySuite expects when the
	// request leaves Style empty.
	DefaultCitationStyle = "apa-7"
)

var seedYear = 2019

// SeedContent is the canonical rendition content: three paragraphs, the
// retrieval token only in the second.
var SeedContent = []byte("Paragraph one introduces the contract fixture.\n\n" +
	"Paragraph two carries the distinctive token " + SeedToken + " for retrieval.\n\n" +
	"Paragraph three closes the fixture.")

// SeedBibliography is the canonical normalized record.
var SeedBibliography = revision.Bibliography{
	RecordID:      "rec-seed-1",
	Title:         "The Contract Fixture",
	Authors:       []string{"Ada Example"},
	Year:          &seedYear,
	Publisher:     "Fixture Press",
	Language:      "de",
	CitationClass: "citable",
}

// SeedRevision is the canonical Library→Store bridge artifact. Its
// ContentHash covers SeedContent; its ContentTicket is what StoreSuite
// implementations must resolve to SeedContent.
var SeedRevision = revision.SourceRevision{
	SourceID:     "src-seed-1",
	RevisionID:   "1",
	ContentHash:  revision.HashContent(SeedContent),
	MediaType:    revision.MediaTypePDF,
	Bibliography: SeedBibliography,
	LocatorCapabilities: revision.LocatorCapabilities{
		Page: &revision.PageCapability{Trust: revision.TrustFolioVerified},
	},
	ContentTicket: "ticket-seed-1",
}

// ---------------------------------------------------------------------------
// error-returning assertions (shared by probes)

func classIs(err error, want contracterr.Class, context string) error {
	if err == nil {
		return fmt.Errorf("%s: expected error %s, got nil", context, want)
	}
	got, ok := contracterr.ClassOf(err)
	if !ok {
		return fmt.Errorf("%s: error is not a contract error: %v", context, err)
	}
	if got != want {
		return fmt.Errorf("%s: class = %s, want %s (error: %v)", context, got, want, err)
	}
	return nil
}

func mustNoErr(err error, context string) error {
	if err != nil {
		return fmt.Errorf("%s: unexpected error: %v", context, err)
	}
	return nil
}

// waitFor polls cond until it holds or the budget expires (async
// implementations — F06/F09 pipelines — converge at their own pace).
func waitFor(what string, cond func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for {
		if last = cond(); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s: %w", what, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// LibrarySuite

type probe struct {
	name string
	run  func() error
}

// LibrarySuite runs all behavior probes against impl.
func LibrarySuite(t *testing.T, impl library.Library) {
	for _, p := range libraryProbes(impl) {
		p := p
		t.Run(p.name, func(t *testing.T) {
			if err := p.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
	runFaultProbes(t, "Library", faultTargets{
		"GetSource": func() error {
			_, err := impl.GetSource(context.Background(), library.SourceRef{SourceID: "src-seed-1"})
			return err
		},
		"StartImport": func() error {
			_, err := impl.StartImport(context.Background(), seedImportRequest("lib-fault"), bytes.NewReader(SeedContent))
			return err
		},
		"OpenRendition": func() error {
			rc, err := impl.OpenRendition(context.Background(), library.ContentTicket(SeedRevision.ContentTicket))
			if err == nil {
				rc.Close()
			}
			return err
		},
	}, impl)
}

// libraryProbes is the probe table — consumed by LibrarySuite and by
// the mutation sonde tests (single probes run against deliberately
// broken fakes in this package).
func libraryProbes(impl library.Library) []probe {
	ctx := context.Background()
	seedReq := seedImportRequest("lib-idem-ok")

	// committed imports the canonical fixture and returns the committed
	// operation (polling included — async implementations converge).
	committed := func() (library.ImportOperation, error) {
		op, err := impl.StartImport(ctx, seedReq, bytes.NewReader(SeedContent))
		if err != nil {
			return library.ImportOperation{}, err
		}
		var final library.ImportOperation
		if err := waitFor("import to reach a terminal state", func() error {
			final, err = impl.GetImport(ctx, library.ImportRef{ImportID: op.ImportID})
			if err != nil {
				return err
			}
			if !final.Status.Terminal() {
				return fmt.Errorf("status %s", final.Status)
			}
			return nil
		}); err != nil {
			return library.ImportOperation{}, err
		}
		if final.Status != library.ImportCommitted {
			return library.ImportOperation{}, fmt.Errorf("import status = %s, want committed", final.Status)
		}
		if final.Result == nil {
			return library.ImportOperation{}, errors.New("committed import has no Result")
		}
		return final, nil
	}

	return []probe{
		{"StartImport: happy path reaches committed with a valid revision", func() error {
			op, err := committed()
			if err != nil {
				return err
			}
			rev := op.Result.Revision
			if err := rev.Validate(); err != nil {
				return fmt.Errorf("published revision is invalid: %w", err)
			}
			if got := revision.HashContent(SeedContent); rev.ContentHash != got {
				return fmt.Errorf("revision content hash = %s, want %s (hash of the imported bytes)", rev.ContentHash, got)
			}
			if rev.Bibliography.RecordID != op.Result.RecordID {
				return fmt.Errorf("revision bibliography record %q != result record %q", rev.Bibliography.RecordID, op.Result.RecordID)
			}
			return nil
		}},
		{"StartImport: idempotent replay returns the same import", func() error {
			first, err := impl.StartImport(ctx, seedReq, bytes.NewReader(SeedContent))
			if err != nil {
				return err
			}
			second, err := impl.StartImport(ctx, seedReq, bytes.NewReader(SeedContent))
			if err != nil {
				return err
			}
			if first.ImportID != second.ImportID {
				return fmt.Errorf("idempotent replay returned %s, want the original %s", second.ImportID, first.ImportID)
			}
			if !reflect.DeepEqual(first, second) {
				return fmt.Errorf("idempotent replay must return the SAME operation, got %+v after %+v", second, first)
			}
			return nil
		}},
		{"StartImport: same key with different metadata is a Conflict mismatch", func() error {
			if _, err := impl.StartImport(ctx, seedReq, bytes.NewReader(SeedContent)); err != nil {
				return err
			}
			metaDiv := seedImportRequest("lib-idem-ok")
			metaDiv.MetadataHints.Title = "A Different Title"
			_, err := impl.StartImport(ctx, metaDiv, bytes.NewReader(SeedContent))
			if err == nil {
				return errors.New("metadata divergence under a reused idempotency key must fail, got success (silent divergence)")
			}
			if err := classIs(err, contracterr.ClassConflict, "idempotency metadata mismatch"); err != nil {
				return err
			}
			var mm *contracterr.IdempotencyMismatch
			if !errors.As(err, &mm) || mm.Key != seedReq.IdempotencyKey {
				return fmt.Errorf("error is not *IdempotencyMismatch with key %q: %v", seedReq.IdempotencyKey, err)
			}
			return nil
		}},
		{"StartImport: same key with different payload is a Conflict mismatch", func() error {
			if _, err := impl.StartImport(ctx, seedReq, bytes.NewReader(SeedContent)); err != nil {
				return err
			}
			_, err := impl.StartImport(ctx, seedReq, bytes.NewReader(append(append([]byte{}, SeedContent...), '!')))
			if err == nil {
				return errors.New("payload divergence under a reused idempotency key must fail, got success (silent divergence)")
			}
			if err := classIs(err, contracterr.ClassConflict, "idempotency mismatch"); err != nil {
				return err
			}
			var mm *contracterr.IdempotencyMismatch
			if !errors.As(err, &mm) || mm.Key != seedReq.IdempotencyKey {
				return fmt.Errorf("error is not *IdempotencyMismatch with key %q: %v", seedReq.IdempotencyKey, err)
			}
			if contracterr.Retryable(err) {
				return errors.New("idempotency mismatch classified retryable — retry storms live here")
			}
			return nil
		}},
		{"StartImport: blank key / record type / contradictory target are InvalidArgument", func() error {
			noKey := seedImportRequest("")
			_, err := impl.StartImport(ctx, noKey, bytes.NewReader(SeedContent))
			if err == nil {
				return errors.New("blank idempotency key accepted")
			}
			if err := classIs(err, contracterr.ClassInvalidArgument, "blank key"); err != nil {
				return err
			}
			badType := seedImportRequest("lib-invalid-1")
			badType.RecordType = ""
			_, err = impl.StartImport(ctx, badType, bytes.NewReader(SeedContent))
			if err := classIs(err, contracterr.ClassInvalidArgument, "blank record_type"); err != nil {
				return err
			}
			both := seedImportRequest("lib-invalid-2")
			both.Target.CollectionID = "col-1"
			both.Target.CollectionPath = []string{"A", "B"}
			_, err = impl.StartImport(ctx, both, bytes.NewReader(SeedContent))
			return classIs(err, contracterr.ClassInvalidArgument, "collection_id XOR collection_path")
		}},
		{"GetImport: unknown id is NotFound", func() error {
			_, err := impl.GetImport(ctx, library.ImportRef{ImportID: "imp-void"})
			return classIs(err, contracterr.ClassNotFound, "unknown import")
		}},
		{"GetSource: resolves the source of a committed import", func() error {
			op, err := committed()
			if err != nil {
				return err
			}
			src, err := impl.GetSource(ctx, library.SourceRef{SourceID: op.Result.Revision.SourceID})
			if err != nil {
				return err
			}
			if src.SourceID != op.Result.Revision.SourceID {
				return fmt.Errorf("GetSource returned %q, want %q", src.SourceID, op.Result.Revision.SourceID)
			}
			return nil
		}},
		{"GetSource: unknown is NotFound, blank is InvalidArgument", func() error {
			_, err := impl.GetSource(ctx, library.SourceRef{SourceID: "src-void"})
			if err := classIs(err, contracterr.ClassNotFound, "unknown source"); err != nil {
				return err
			}
			_, err = impl.GetSource(ctx, library.SourceRef{})
			return classIs(err, contracterr.ClassInvalidArgument, "blank source ref")
		}},
		{"OpenRendition: ticket yields the hashed content", func() error {
			op, err := committed()
			if err != nil {
				return err
			}
			rc, err := impl.OpenRendition(ctx, library.ContentTicket(op.Result.Revision.ContentTicket))
			if err != nil {
				return err
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return err
			}
			if got := revision.HashContent(b); got != op.Result.Revision.ContentHash {
				return fmt.Errorf("rendition hash %s != revision hash %s — the ticket served foreign bytes", got, op.Result.Revision.ContentHash)
			}
			return nil
		}},
		{"OpenRendition: unknown ticket is NotFound, blank is InvalidArgument", func() error {
			_, err := impl.OpenRendition(ctx, library.ContentTicket("ticket-void"))
			if err := classIs(err, contracterr.ClassNotFound, "unknown ticket"); err != nil {
				return err
			}
			_, err = impl.OpenRendition(ctx, library.ContentTicket(""))
			return classIs(err, contracterr.ClassInvalidArgument, "blank ticket")
		}},
		{"ProjectCitation: projects an in-text plus reference form", func() error {
			op, err := committed()
			if err != nil {
				return err
			}
			page := 47
			proj, err := impl.ProjectCitation(ctx, library.CitationRequest{
				RecordID: op.Result.RecordID,
				Locator: library.CitationLocator{
					Kind:       "page",
					PageStart:  &page,
					PageEnd:    &page,
					PageSource: revision.TrustFolioVerified,
				},
			})
			if err != nil {
				return err
			}
			if proj.Citation == "" || proj.Reference == "" {
				return fmt.Errorf("projection incomplete: citation %q reference %q", proj.Citation, proj.Reference)
			}
			if proj.RecordID != op.Result.RecordID {
				return fmt.Errorf("projection record %q != %q", proj.RecordID, op.Result.RecordID)
			}
			if proj.Style != DefaultCitationStyle {
				return fmt.Errorf("default style = %q, want %q", proj.Style, DefaultCitationStyle)
			}
			return nil
		}},
		{"ProjectCitation: unknown record is NotFound, unusable locator is InvalidArgument", func() error {
			_, err := impl.ProjectCitation(ctx, library.CitationRequest{RecordID: "rec-void", Locator: library.CitationLocator{Kind: "page", PageSource: revision.TrustFolioVerified}})
			if err := classIs(err, contracterr.ClassNotFound, "unknown record"); err != nil {
				return err
			}
			op, err := committed()
			if err != nil {
				return err
			}
			_, err = impl.ProjectCitation(ctx, library.CitationRequest{RecordID: op.Result.RecordID, Locator: library.CitationLocator{Kind: "no-such-kind"}})
			return classIs(err, contracterr.ClassInvalidArgument, "unknown locator kind")
		}},
	}
}

func seedImportRequest(key string) library.ImportRequest {
	crossref, openLib := true, false // one default-on omitted, one explicit — exercises both flag forms
	return library.ImportRequest{
		IdempotencyKey: key,
		RecordType:     "book",
		Target:         library.ImportTarget{LibraryID: "users/0"},
		MetadataHints:  library.MetadataHints{Title: SeedBibliography.Title},
		Enrichment:     library.EnrichmentFlags{Crossref: &crossref, OpenLibrary: &openLib},
	}
}

// ---------------------------------------------------------------------------
// StoreSuite

// StoreSuite runs all behavior probes against impl (precondition: the
// SeedRevision content ticket resolves to SeedContent).
func StoreSuite(t *testing.T, impl store.Store) {
	for _, p := range storeProbes(impl) {
		p := p
		t.Run(p.name, func(t *testing.T) {
			if err := p.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
	runFaultProbes(t, "Store", faultTargets{
		"IngestRevision": func() error {
			_, err := impl.IngestRevision(context.Background(), store.IngestRevisionRequest{IdempotencyKey: "store-fault", Revision: SeedRevision})
			return err
		},
		"Search": func() error {
			_, err := impl.Search(context.Background(), store.SearchRequest{Query: SeedToken})
			return err
		},
		"GetPassage": func() error {
			_, err := impl.GetPassage(context.Background(), store.PassageRef{ChunkID: "chk-void"})
			return err
		},
	}, impl)
}

func storeProbes(impl store.Store) []probe {
	ctx := context.Background()

	// ingest runs the canonical intake and waits until the content is
	// observable through Search (the contract has no job-status poll —
	// eventual visibility IS the terminal observable).
	ingest := func() (store.IngestJob, error) {
		job, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-idem-ok", Revision: SeedRevision})
		if err != nil {
			return store.IngestJob{}, err
		}
		if err := waitFor("ingested revision to become searchable", func() error {
			res, err := impl.Search(ctx, store.SearchRequest{Query: SeedToken})
			if err != nil {
				return err
			}
			for _, h := range res.Hits {
				if h.Source.RecordID == SeedBibliography.RecordID {
					return nil
				}
			}
			return fmt.Errorf("no hit for %q from record %s yet (%d hits)", SeedToken, SeedBibliography.RecordID, len(res.Hits))
		}); err != nil {
			return store.IngestJob{}, err
		}
		return job, nil
	}

	return []probe{
		{"IngestRevision: intake makes the revision searchable", func() error {
			job, err := ingest()
			if err != nil {
				return err
			}
			if job.RevisionID != SeedRevision.RevisionID {
				return fmt.Errorf("job revision %q != request %q", job.RevisionID, SeedRevision.RevisionID)
			}
			if job.ContentHash != SeedRevision.ContentHash {
				return fmt.Errorf("job content hash %q != request %q", job.ContentHash, SeedRevision.ContentHash)
			}
			return nil
		}},
		{"IngestRevision: idempotent replay returns the same job", func() error {
			first, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-idem-ok", Revision: SeedRevision})
			if err != nil {
				return err
			}
			second, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-idem-ok", Revision: SeedRevision})
			if err != nil {
				return err
			}
			if first.JobID != second.JobID {
				return fmt.Errorf("idempotent replay returned %s, want the original %s", second.JobID, first.JobID)
			}
			if !reflect.DeepEqual(first, second) {
				return fmt.Errorf("idempotent replay must return the SAME job, got %+v after %+v", second, first)
			}
			return nil
		}},
		{"IngestRevision: same key with different revision is a Conflict mismatch", func() error {
			if _, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-idem-div", Revision: SeedRevision}); err != nil {
				return err
			}
			diverged := SeedRevision
			diverged.RevisionID = "2"
			diverged.ContentHash = revision.HashContent([]byte("diverged"))
			_, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-idem-div", Revision: diverged})
			if err == nil {
				return errors.New("revision divergence under a reused idempotency key must fail, got success (silent divergence)")
			}
			if err := classIs(err, contracterr.ClassConflict, "idempotency mismatch"); err != nil {
				return err
			}
			var mm *contracterr.IdempotencyMismatch
			if !errors.As(err, &mm) || mm.Key != "store-idem-div" {
				return fmt.Errorf("error is not *IdempotencyMismatch with key %q: %v", "store-idem-div", err)
			}
			return nil
		}},
		{"IngestRevision: invalid revisions are InvalidArgument", func() error {
			_, err := impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-invalid-1", Revision: revision.SourceRevision{}})
			if err := classIs(err, contracterr.ClassInvalidArgument, "empty revision"); err != nil {
				return err
			}
			badHash := SeedRevision
			badHash.ContentHash = "not-a-hash"
			_, err = impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "store-invalid-2", Revision: badHash})
			if err := classIs(err, contracterr.ClassInvalidArgument, "malformed content hash"); err != nil {
				return err
			}
			_, err = impl.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "", Revision: SeedRevision})
			return classIs(err, contracterr.ClassInvalidArgument, "blank idempotency key")
		}},
		{"Search: finds the seeded record with its bibliography", func() error {
			if _, err := ingest(); err != nil {
				return err
			}
			res, err := impl.Search(ctx, store.SearchRequest{Query: SeedToken})
			if err != nil {
				return err
			}
			if res.Hits == nil {
				return errors.New("hits must be a present (possibly empty) slice — nil would marshal as null and break the frozen array shape")
			}
			var hit *store.SearchHit
			for i := range res.Hits {
				if res.Hits[i].Source.RecordID == SeedBibliography.RecordID {
					hit = &res.Hits[i]
					break
				}
			}
			if hit == nil {
				return fmt.Errorf("no hit for %q from record %s", SeedToken, SeedBibliography.RecordID)
			}
			if hit.ChunkID == "" || hit.Text == "" {
				return fmt.Errorf("hit incomplete: %+v", hit)
			}
			if hit.Source.Title != SeedBibliography.Title {
				return fmt.Errorf("hit title %q != %q — bibliography not hydrated verbatim", hit.Source.Title, SeedBibliography.Title)
			}
			if hit.Source.CitationClass != SeedBibliography.CitationClass {
				return fmt.Errorf("hit citation class %q != %q", hit.Source.CitationClass, SeedBibliography.CitationClass)
			}
			return nil
		}},
		{"Search: blank query and over-cap top_n are InvalidArgument, top_n<=0 defaults", func() error {
			_, err := impl.Search(ctx, store.SearchRequest{Query: "   "})
			if err := classIs(err, contracterr.ClassInvalidArgument, "blank query"); err != nil {
				return err
			}
			_, err = impl.Search(ctx, store.SearchRequest{Query: SeedToken, TopN: store.MaxTopN + 1})
			if err := classIs(err, contracterr.ClassInvalidArgument, "top_n over cap"); err != nil {
				return err
			}
			res, err := impl.Search(ctx, store.SearchRequest{Query: SeedToken, TopN: 0})
			if err != nil {
				return err
			}
			if res.TopN != 10 {
				return fmt.Errorf("default top_n = %d, want 10", res.TopN)
			}
			return nil
		}},
		{"GetPassage: resolves a hit's chunk consistently", func() error {
			if _, err := ingest(); err != nil {
				return err
			}
			res, err := impl.Search(ctx, store.SearchRequest{Query: SeedToken})
			if err != nil {
				return err
			}
			if len(res.Hits) == 0 {
				return errors.New("no hits to resolve a passage from")
			}
			hit := res.Hits[0]
			p, err := impl.GetPassage(ctx, store.PassageRef{ChunkID: hit.ChunkID})
			if err != nil {
				return err
			}
			if p.ChunkID != hit.ChunkID {
				return fmt.Errorf("passage chunk %q != hit %q", p.ChunkID, hit.ChunkID)
			}
			if p.Text != hit.Text {
				return errors.New("passage text diverges from hit text for the same chunk — search and passage must agree")
			}
			if p.Source.RecordID != SeedBibliography.RecordID {
				return fmt.Errorf("passage record %q != %q", p.Source.RecordID, SeedBibliography.RecordID)
			}
			if p.Neighbors == nil {
				return errors.New("neighbors must be a present (possibly empty) slice — nil breaks the frozen wire shape")
			}
			return nil
		}},
		{"GetPassage: unknown is NotFound, blank is InvalidArgument", func() error {
			_, err := impl.GetPassage(ctx, store.PassageRef{ChunkID: "chk-void"})
			if err := classIs(err, contracterr.ClassNotFound, "unknown passage"); err != nil {
				return err
			}
			_, err = impl.GetPassage(ctx, store.PassageRef{})
			return classIs(err, contracterr.ClassInvalidArgument, "blank passage ref")
		}},
	}
}

// ---------------------------------------------------------------------------
// fault probes

type faultTargets map[string]func() error

// runFaultProbes proves classification under induced faults — only if
// the implementation offers FaultControl.
func runFaultProbes(t *testing.T, component string, targets faultTargets, impl any) {
	t.Helper()
	fc, ok := impl.(FaultControl)
	if !ok {
		return
	}
	for method, call := range targets {
		method, call := method, call
		t.Run("Faults: "+method+" classification", func(t *testing.T) {
			fc.ClearFaults()
			defer fc.ClearFaults()

			// Unavailable must classify retryable — and survive wrapping.
			fc.InjectFault(method, contracterr.New(contracterr.Component(component), contracterr.ClassUnavailable, "probe"))
			err := call()
			if err := classIs(err, contracterr.ClassUnavailable, method+" unavailable"); err != nil {
				t.Fatal(err)
			}
			if !contracterr.Retryable(err) {
				t.Fatalf("%s: Unavailable not retryable: %v", method, err)
			}
			if !contracterr.Retryable(fmt.Errorf("outer: %w", err)) {
				t.Fatalf("%s: classification lost through wrapping: %v", method, err)
			}

			// A raw internal error must surface as Internal, never leak.
			fc.ClearFaults()
			fc.InjectFault(method, errors.New("database exploded"))
			if err := classIs(call(), contracterr.ClassInternal, method+" internal leak"); err != nil {
				t.Fatal(err)
			}

			// Deadline is classified but NOT auto-retryable.
			fc.ClearFaults()
			fc.InjectFault(method, contracterr.New(contracterr.Component(component), contracterr.ClassDeadline, "probe"))
			err = call()
			if err := classIs(err, contracterr.ClassDeadline, method+" deadline"); err != nil {
				t.Fatal(err)
			}
			if contracterr.Retryable(err) {
				t.Fatalf("%s: Deadline auto-retryable — same-deadline retry storms live here", method)
			}
		})
	}
}
