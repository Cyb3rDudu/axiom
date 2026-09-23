// suite_test.go — the harness's own witnesses (#297):
//
//   - both suites run green against the reference fakes (the harness is
//     self-consistent),
//   - MUTATION SONDES per component prove the suites check behavior,
//     not types: a fake that misclassifies NotFound as Internal, one
//     that swallows the idempotency mismatch, one whose neighbors lie
//     about adjacency, ones that stamp non-UTC or sub-microsecond
//     times, one that answers a blank import ref with success — each
//     drives its probe red. (The sonde pattern of internal/baseline:
//     prove the gate has teeth in-suite; the external worktree probes
//     repeat it on real source.)
package contractsuite

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

func TestLibrarySuiteAgainstReferenceFake(t *testing.T) {
	LibrarySuite(t, NewFakeLibrary())
}

func TestStoreSuiteAgainstReferenceFake(t *testing.T) {
	StoreSuite(t, NewFakeStore(nil))
}

// runProbe runs the named probe and reports whether it FAILED — the
// sonde harness: a broken implementation must not pass.
func runProbe(probes []probe, name string) (failed bool, err error) {
	for _, p := range probes {
		if p.name != name {
			continue
		}
		if err := p.run(); err != nil {
			return true, err
		}
		return false, nil
	}
	panic("probe not found: " + name)
}

// --- sonde 1: misclassification (NotFound reported as Internal) -------

type misclassifyingLibrary struct{ *FakeLibrary }

func (m misclassifyingLibrary) GetSource(ctx context.Context, ref library.SourceRef) (library.Source, error) {
	src, err := m.FakeLibrary.GetSource(ctx, ref)
	if err != nil {
		if class, _ := contracterr.ClassOf(err); class == contracterr.ClassNotFound {
			return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "misclassified")
		}
	}
	return src, err
}

func TestSondeLibraryMisclassificationGoesRed(t *testing.T) {
	failed, err := runProbe(libraryProbes(misclassifyingLibrary{NewFakeLibrary()}),
		"GetSource: unknown is NotFound, blank is InvalidArgument")
	if !failed {
		t.Fatalf("misclassifying fake (NotFound as Internal) passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- sonde 2: swallowed idempotency mismatch ---------------------------

type swallowingLibrary struct{ *FakeLibrary }

func (s swallowingLibrary) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	op, err := s.FakeLibrary.StartImport(ctx, req, content)
	if _, ok := err.(*contracterr.IdempotencyMismatch); ok {
		// the sin under test: return the earlier operation instead of
		// the conflict — silent divergence
		s.mu.Lock()
		prior := s.imports[s.byKey[req.IdempotencyKey]]
		s.mu.Unlock()
		return prior, nil
	}
	return op, err
}

func TestSondeLibraryIdempotencySwallowGoesRed(t *testing.T) {
	failed, err := runProbe(libraryProbes(swallowingLibrary{NewFakeLibrary()}),
		"StartImport: same key with different payload is a Conflict mismatch")
	if !failed {
		t.Fatalf("idempotency-swallowing fake passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- store-side twins ---------------------------------------------------

type misclassifyingStore struct{ *FakeStore }

func (m misclassifyingStore) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	p, err := m.FakeStore.GetPassage(ctx, ref)
	if err != nil {
		if class, _ := contracterr.ClassOf(err); class == contracterr.ClassNotFound {
			return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInternal, "misclassified")
		}
	}
	return p, err
}

func TestSondeStoreMisclassificationGoesRed(t *testing.T) {
	failed, err := runProbe(storeProbes(misclassifyingStore{NewFakeStore(nil)}),
		"GetPassage: unknown is NotFound, blank is InvalidArgument")
	if !failed {
		t.Fatalf("misclassifying store fake passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

type swallowingStore struct{ *FakeStore }

func (s swallowingStore) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	job, err := s.FakeStore.IngestRevision(ctx, req)
	if _, ok := err.(*contracterr.IdempotencyMismatch); ok {
		s.mu.Lock()
		prior := s.jobs[s.byKey[req.IdempotencyKey]]
		s.mu.Unlock()
		return prior, nil
	}
	return job, err
}

func TestSondeStoreIdempotencySwallowGoesRed(t *testing.T) {
	failed, err := runProbe(storeProbes(swallowingStore{NewFakeStore(nil)}),
		"IngestRevision: same key with different revision is a Conflict mismatch")
	if !failed {
		t.Fatalf("idempotency-swallowing store fake passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- sonde: neighbors that lie about adjacency -------------------------

type neighborLyingStore struct{ *FakeStore }

func (n neighborLyingStore) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	p, err := n.FakeStore.GetPassage(ctx, ref)
	if err != nil || len(p.Neighbors) == 0 {
		return p, err
	}
	// the sin under test: claim an index no longer ±1 (a small shift
	// could re-land on the other boundary index — 10 cannot)
	p.Neighbors[0].ChunkIndex += 10
	return p, nil
}

func TestSondeStoreNeighborLieGoesRed(t *testing.T) {
	failed, err := runProbe(storeProbes(neighborLyingStore{NewFakeStore(nil)}),
		"GetPassage: resolves a hit's chunk consistently")
	if !failed {
		t.Fatalf("neighbor-lying fake passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- sonde: producer time discipline -----------------------------------

type zonedLibrary struct{ *FakeLibrary }

func (z zonedLibrary) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	op, err := z.FakeLibrary.GetImport(ctx, ref)
	if err != nil {
		return op, err
	}
	// the sin under test: same instant, non-UTC form
	op.UpdatedAt = op.UpdatedAt.In(time.FixedZone("PROBE", 2*3600))
	return op, nil
}

func TestSondeLibraryNonUTCTimeGoesRed(t *testing.T) {
	failed, err := runProbe(libraryProbes(zonedLibrary{NewFakeLibrary()}),
		"StartImport: happy path reaches committed with a valid revision")
	if !failed {
		t.Fatalf("non-UTC timestamps passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

type subMicroStore struct{ *FakeStore }

func (s subMicroStore) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	job, err := s.FakeStore.IngestRevision(ctx, req)
	if err != nil {
		return job, err
	}
	// the sin under test: sub-microsecond precision
	job.UpdatedAt = job.UpdatedAt.Add(500 * time.Nanosecond)
	return job, nil
}

func TestSondeStoreSubMicrosecondTimeGoesRed(t *testing.T) {
	failed, err := runProbe(storeProbes(subMicroStore{NewFakeStore(nil)}),
		"IngestRevision: intake makes the revision searchable")
	if !failed {
		t.Fatalf("sub-microsecond timestamps passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- sonde: blank import ref swallowed ----------------------------------

type blankSwallowingLibrary struct{ *FakeLibrary }

func (b blankSwallowingLibrary) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	if ref.ImportID == "" {
		return library.ImportOperation{}, nil // the sin under test: blank ref answered as success
	}
	return b.FakeLibrary.GetImport(ctx, ref)
}

func TestSondeLibraryBlankRefSwallowGoesRed(t *testing.T) {
	failed, err := runProbe(libraryProbes(blankSwallowingLibrary{NewFakeLibrary()}),
		"GetImport: unknown is NotFound, blank is InvalidArgument")
	if !failed {
		t.Fatalf("blank-ref-swallowing fake passed the suite probe — the harness has no teeth (err: %v)", err)
	}
}

// --- mediaTypeFromMagic: all three branches witnessed --------------------

func TestMediaTypeFromMagic(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		fail bool // true: expect InvalidArgument and empty media type
	}{
		{"pdf magic", []byte("%PDF-1.4 body"), revision.MediaTypePDF, false},
		{"epub magic (zip local header)", []byte("PK\x03\x04mimetypeapplication/epub+zip"), revision.MediaTypeEPUB, false},
		{"foreign bytes", []byte("<html>no magic here</html>"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mediaTypeFromMagic(c.in)
			if !c.fail {
				if err != nil || got != c.want {
					t.Fatalf("mediaTypeFromMagic = %q, %v; want %q, nil", got, err, c.want)
				}
				return
			}
			if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
				t.Fatalf("foreign bytes: error class = %v (typed=%v), want InvalidArgument (err: %v)", class, ok, err)
			}
		})
	}
}
