// selection_listing.go — the documents/selection surface's composite
// wiring (#358): the selection routes are Library-database truth (the
// mirror's selections), while the documents listing merges the Library's
// mirror rows with the Store's job/snapshot truth IN CODE (no
// cross-database SQL). Implements server.SelectionRepo over the two
// component handles.
package composition

import (
	"context"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library/mirror"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom/internal/server"
)

// selectionListing is the composite SelectionRepo: selection writes and
// reads go to the Library's mirror (the sync's source of truth); the
// documents listing merges both components' halves in code.
type selectionListing struct {
	mir   *mirror.Repo
	store *repo.Repo
}

var _ server.SelectionRepo = (*selectionListing)(nil)

func (c *selectionListing) SetSelectionBatch(ctx context.Context, docs []mirror.SelectionInput, colls []mirror.CollectionSelectionInput) error {
	return c.mir.SetSelectionBatch(ctx, docs, colls)
}

func (c *selectionListing) SelectionModes(ctx context.Context) (map[string]string, error) {
	return c.mir.SelectionModes(ctx)
}

func (c *selectionListing) CollectionSelectionModes(ctx context.Context) (map[string]string, error) {
	return c.mir.CollectionSelectionModes(ctx)
}

func (c *selectionListing) ResolveSelectionView(ctx context.Context) (*mirror.ResolvedSelection, error) {
	return c.mir.ResolveSelectionView(ctx)
}

// ListZoteroDocuments merges the Library's mirror rows with the Store's
// job/snapshot truth: two engine-local queries, one code merge (#358).
func (c *selectionListing) ListZoteroDocuments(ctx context.Context, syncState string) ([]mirror.ZoteroDocumentState, error) {
	rows, err := c.mir.ListDocumentsMirror(ctx)
	if err != nil {
		return nil, err
	}
	var attIDs, docIDs []string
	for _, z := range rows {
		if z.AttachmentID != "" {
			attIDs = append(attIDs, z.AttachmentID)
		}
		if z.DocumentID != "" {
			docIDs = append(docIDs, z.DocumentID)
		}
	}
	jobs, serving, err := c.store.DocumentJobStates(ctx, attIDs, docIDs)
	if err != nil {
		return nil, err
	}
	return mirror.DocumentListing(rows, jobs, serving, syncState), nil
}
