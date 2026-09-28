// local.go — the in-process bindings (F11 #305). Direct Go calls onto
// the F06/F09 services; the wrappers exist so the composition's binding
// choice is one code shape for both topologies and so the parity suite
// runs against the SAME client type production uses. FaultControl
// passes through when the backing service offers it — the suite's
// classification probes must not thin out per binding.
package bindings

import (
	"context"
	"io"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contractsuite"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

// LocalLibraryClient binds library.Library in-process.
type LocalLibraryClient struct {
	Svc library.Library
	fc  contractsuite.FaultControl // nil unless the service offers it
}

// NewLocalLibraryClient wraps svc (nil fault control when svc has none).
func NewLocalLibraryClient(svc library.Library) *LocalLibraryClient {
	fc, _ := svc.(contractsuite.FaultControl)
	return &LocalLibraryClient{Svc: svc, fc: fc}
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

// InjectFault/ClearFaults pass through to the backing service (suite
// parity: the fault probes run against every binding identically).
func (c *LocalLibraryClient) InjectFault(method string, err error) {
	if c.fc != nil {
		c.fc.InjectFault(method, err)
	}
}

func (c *LocalLibraryClient) ClearFaults() {
	if c.fc != nil {
		c.fc.ClearFaults()
	}
}

// LocalStoreClient binds store.Store in-process.
type LocalStoreClient struct {
	Svc store.Store
	fc  contractsuite.FaultControl
	cc  contractsuite.ContentCorruptor
}

// NewLocalStoreClient wraps svc (nil fault control when svc has none).
func NewLocalStoreClient(svc store.Store) *LocalStoreClient {
	fc, _ := svc.(contractsuite.FaultControl)
	cc, _ := svc.(contractsuite.ContentCorruptor)
	return &LocalStoreClient{Svc: svc, fc: fc, cc: cc}
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

func (c *LocalStoreClient) InjectFault(method string, err error) {
	if c.fc != nil {
		c.fc.InjectFault(method, err)
	}
}

func (c *LocalStoreClient) ClearFaults() {
	if c.fc != nil {
		c.fc.ClearFaults()
	}
}

// CorruptNextContent passes through when the backing service offers the
// knob (suite parity: the hash-mismatch probe runs per binding).
func (c *LocalStoreClient) CorruptNextContent() {
	if c.cc != nil {
		c.cc.CorruptNextContent()
	}
}
