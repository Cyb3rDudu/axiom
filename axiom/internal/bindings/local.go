// local.go — the in-process bindings (F11 #305). Direct Go calls onto
// the F06/F09 services; the wrappers exist so the composition's binding
// choice is one code shape for both topologies and so the parity suite
// runs against the SAME client type production uses.
//
// Optional capabilities (FaultControl, ContentCorruptor) are deliberately
// NOT implemented here: interface satisfaction in Go is static, so a
// wrapper with nil-guarded pass-through would ADVERTISE the capability
// even when the backing service has none — the F03 suite's fault probes
// would then run against a no-op control and misclassify normal answers
// (the CI-red lesson: local-green/CI-red). Parity tests compose the
// control surface explicitly next to the binding (the same wrapper shape
// for local and HTTP legs); production services without injection knobs
// simply run the suites without the fault probes, exactly like the
// direct-service suites always have.
package bindings

import (
	"context"
	"io"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/store"
)

// LocalLibraryClient binds library.Library in-process.
type LocalLibraryClient struct {
	Svc library.Library
}

// NewLocalLibraryClient wraps svc.
func NewLocalLibraryClient(svc library.Library) *LocalLibraryClient {
	return &LocalLibraryClient{Svc: svc}
}

func (c *LocalLibraryClient) GetSource(ctx context.Context, ref library.SourceRef) (library.Source, error) {
	return c.Svc.GetSource(ctx, ref)
}

func (c *LocalLibraryClient) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	return c.Svc.StartImport(ctx, req, content)
}

func (c *LocalLibraryClient) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	return c.Svc.GetImport(ctx, ref)
}

func (c *LocalLibraryClient) OpenRendition(ctx context.Context, ticket library.ContentTicket) (io.ReadCloser, error) {
	return c.Svc.OpenRendition(ctx, ticket)
}

func (c *LocalLibraryClient) ProjectCitation(ctx context.Context, req library.CitationRequest) (library.CitationProjection, error) {
	return c.Svc.ProjectCitation(ctx, req)
}

// LocalStoreClient binds store.Store in-process.
type LocalStoreClient struct {
	Svc store.Store
}

// NewLocalStoreClient wraps svc.
func NewLocalStoreClient(svc store.Store) *LocalStoreClient {
	return &LocalStoreClient{Svc: svc}
}

func (c *LocalStoreClient) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	return c.Svc.IngestRevision(ctx, req)
}

func (c *LocalStoreClient) Search(ctx context.Context, req store.SearchRequest) (store.SearchResult, error) {
	return c.Svc.Search(ctx, req)
}

func (c *LocalStoreClient) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	return c.Svc.GetPassage(ctx, ref)
}
