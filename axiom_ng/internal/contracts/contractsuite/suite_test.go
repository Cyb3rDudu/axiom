// suite_test.go — the harness's own witnesses (#297):
//
//   - both suites run green against the reference fakes (the harness is
//     self-consistent),
//   - two MUTATION SONDES per component prove the suites check behavior,
//     not types: a fake that misclassifies NotFound as Internal, and a
//     fake that swallows the idempotency mismatch, each drive their
//     probe red. (The sonde pattern of internal/baseline: prove the gate
//     has teeth in-suite; the external worktree probes repeat it on
//     real source.)
package contractsuite

import (
	"context"
	"io"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
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
