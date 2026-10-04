// helpers.go — small shared bits: SQL identifier/literal quoting and
// the Library SQLite open bridge (schema ownership stays with the
// component's own migration path).
package databundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library/sqlite"
	"github.com/jackc/pgx/v5"
)

// pgIdent quotes one PostgreSQL identifier.
func pgIdent(name string) string { return pgx.Identifier{name}.Sanitize() }

// sqlIdent quotes one SQLite identifier.
func sqlIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteLit renders one SQL string literal (sequence names for setval).
func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sqliteLibraryOpen creates/opens + migrates the Library SQLite file
// via the component's own Open (operating pragmas asserted, own
// ledger). The sink then re-opens a raw single-writer handle — it
// never emits DDL itself.
func sqliteLibraryOpen(ctx context.Context, path string) (*sqlite.Repo, error) {
	rep, err := sqlite.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("library sqlite open: %w", err)
	}
	return rep, nil
}
