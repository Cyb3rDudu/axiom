// migratecmd.go — `axiom migrate` (#362): the deployer-side migration
// entry. Applies every pending component migration (core + store +
// library) and reports the ledger state before → after per component.
// Same idempotency as the boot path (the very same runners); nothing
// pending → "up to date", exit 0. Failure → exit 1.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
)

// cmdMigrate applies all pending component migrations against the
// configured databases (the store DSN, plus the Library DSN when the
// PostgreSQL profile is selected). The deployer runs it with the
// deployer DSN before switching services onto new binaries — runtime
// roles are DML-only (DM07) and refuse pending migrations at boot.
func cmdMigrate(name string, args []string, flags map[string]string) int {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "usage: %s migrate [--set KEY=VALUE]...\n", name)
		return exitUsage
	}
	cfg, code := loadRuntime(name, flags)
	if code != exitOK {
		return code
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintf(os.Stderr, "%s migrate: no database DSN configured (AXIOM_STORE_DATABASE_URL / AXIOM_DATABASE_URL)\n", name)
		return exitFailure
	}
	ctx := context.Background()

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, err)
		return exitFailure
	}
	defer database.Close()

	total := 0
	// core + store share the store pool (same physical database, own
	// ledgers).
	n, err := migrateComponent(ctx, "core", database.Pool(),
		"schema_migrations", db.Pending, database.Migrate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, config.RedactQueryCredentials(err.Error()))
		return exitFailure
	}
	total += n
	n, err = migrateComponent(ctx, "store", database.Pool(),
		"store_schema_migrations", storemigrations.Pending,
		func(ctx context.Context) error { return storemigrations.Migrate(ctx, database.Pool()) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, config.RedactQueryCredentials(err.Error()))
		return exitFailure
	}
	total += n

	// library: the PostgreSQL profile migrates on the Library DSN (own
	// database in the split topology). The SQLite profile migrates on
	// every open — nothing for a deployer to apply here.
	switch cfg.StorageLibraryDriver {
	case "", "postgres":
		libDSN := cfg.LibraryDatabaseURL
		if libDSN == "" {
			libDSN = cfg.DatabaseURL
		}
		libDB, err := db.Open(ctx, libDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, err)
			return exitFailure
		}
		defer libDB.Close()
		n, err := migrateComponent(ctx, "library", libDB.Pool(),
			"library_schema_migrations", pglib.Pending,
			func(ctx context.Context) error { return pglib.Migrate(ctx, libDB.Pool()) })
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, config.RedactQueryCredentials(err.Error()))
			return exitFailure
		}
		total += n
	case "sqlite":
		fmt.Println("library: sqlite profile — migrations apply on open, nothing to do here")
	}

	if total == 0 {
		fmt.Println("up to date")
	}
	return exitOK
}

// migrateComponent runs one component's ledger report + apply pass:
// reads the ledger's latest version before, applies via the component's
// own runner (the boot-path idempotency), reads it after. One stdout
// line per component; returns how many migrations were applied.
func migrateComponent(ctx context.Context, component string, pool *pgxpool.Pool,
	ledger string, pending func(context.Context, *pgxpool.Pool) ([]string, error),
	apply func(context.Context) error) (int, error) {

	pend, err := pending(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("%s: reading pending migrations: %w", component, err)
	}
	before, err := ledgerLatest(ctx, pool, ledger)
	if err != nil {
		return 0, fmt.Errorf("%s: reading ledger: %w", component, err)
	}
	if len(pend) == 0 {
		fmt.Printf("%s: up to date (%s)\n", component, before)
		return 0, nil
	}
	if err := apply(ctx); err != nil {
		return 0, fmt.Errorf("%s: %w", component, err)
	}
	after, err := ledgerLatest(ctx, pool, ledger)
	if err != nil {
		return 0, fmt.Errorf("%s: reading ledger after migrate: %w", component, err)
	}
	fmt.Printf("%s: %s -> %s (+%d: %v)\n", component, before, after, len(pend), pend)
	return len(pend), nil
}

// ledgerLatest reads the ledger's highest applied version (the doctor's
// schema probe shape); a missing or empty ledger reads as "(none)".
func ledgerLatest(ctx context.Context, pool *pgxpool.Pool, ledger string) (string, error) {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1)`, ledger,
	).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "(none)", nil
	}
	var latest *string
	if err := pool.QueryRow(ctx, "SELECT max(version) FROM "+ledger).Scan(&latest); err != nil {
		return "", err
	}
	if latest == nil {
		return "(none)", nil
	}
	return *latest, nil
}
