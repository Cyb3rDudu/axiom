package store

import (
	"context"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/store/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrate applies the Store ledger (see internal/store/migrations).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrations.Migrate(ctx, pool)
}
