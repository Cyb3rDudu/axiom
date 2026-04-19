// Command axiom-ng serves the Go implementation of the axiom backend.
//
// main() is intentionally thin — signal handling + process-level
// wiring — so it is exempt from the per-package coverage gate.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "axiom-ng: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv("AXIOM_NG_CONFIG_FILE"))
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	info := version.Current()
	logger.Info("axiom-ng starting",
		slog.String("version", info.Version),
		slog.String("commit", info.Commit),
		slog.String("date", info.Date),
		slog.Int("port", cfg.Port),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, gormDB, err := buildDeps(ctx, cfg, logger)
	if err != nil {
		return err
	}
	if gormDB != nil {
		defer closeDB(gormDB, logger)
	}

	srv := server.NewWithDeps(cfg, logger, deps)

	if !cfg.IngestEnabled || gormDB == nil {
		return srv.Run(ctx)
	}

	// Run HTTP server and ingest pool under the same ctx so a single
	// SIGTERM brings both down together.
	pool := ingest.New(
		repo.NewDocuments(gormDB),
		ingest.NoopProcessor{Logger: logger},
		ingest.Config{
			Size:         cfg.IngestPoolSize,
			PollInterval: cfg.IngestPollInterval,
			Logger:       logger,
		},
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error { return pool.Run(gctx) })
	return g.Wait()
}

// buildDeps constructs the concrete dependency graph. It opens the
// *gorm.DB if DatabaseURL is set; otherwise returns an empty Deps so
// the binary can still serve / and /health for smoke testing.
func buildDeps(ctx context.Context, cfg config.Config, logger *slog.Logger) (server.Deps, *gorm.DB, error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("AXIOM_NG_DATABASE_URL not set; serving bootstrap routes only")
		return server.Deps{}, nil, nil
	}

	dbCfg := db.DefaultConfig()
	dbCfg.URL = cfg.DatabaseURL
	gormDB, err := db.Open(ctx, dbCfg)
	if err != nil {
		return server.Deps{}, nil, fmt.Errorf("db open: %w", err)
	}
	if err := db.RequireSchema(ctx, gormDB); err != nil {
		closeDB(gormDB, logger)
		return server.Deps{}, nil, fmt.Errorf("schema check: %w", err)
	}

	signer, err := auth.NewSigner(secretFromEnv())
	if err != nil {
		closeDB(gormDB, logger)
		return server.Deps{}, nil, fmt.Errorf("jwt signer: %w", err)
	}

	users := repo.NewUsers(gormDB)
	langs := repo.NewLanguages(gormDB)
	sysSettings := repo.NewSystemSettings(gormDB)
	dash := repo.NewDashboard(gormDB)
	chats := repo.NewChats(gormDB)
	documents := repo.NewDocuments(gormDB)
	groups := repo.NewDocumentGroups(gormDB)
	chunks := repo.NewChunks(gormDB)

	// Single GPU client shared between /api/system/gpu-status and
	// the retriever — both just probe/embed, no persistent state.
	gpuProbe := newGPUProbe(cfg)

	// Bootstrap admin user if the table is empty, matching Python's
	// setup_first_user.py behaviour. ADMIN_USERNAME / ADMIN_PASSWORD /
	// ADMIN_EMAIL env vars override the defaults.
	if err := ensureFirstUser(ctx, users, logger); err != nil {
		logger.Warn("first-user bootstrap failed",
			slog.String("error", err.Error()))
	}

	deps := server.Deps{
		Auth: api.AuthDeps{
			Users:          users,
			SystemSettings: sysSettings,
			Signer:         signer,
		},
		Languages: api.LanguageDeps{Languages: langs},
		System: api.SystemDeps{
			Health: dbHealth{gdb: gormDB},
			GPU:    gpuProbe,
		},
		Dashboard: api.DashboardDeps{Stats: dash},
		Settings:  api.SettingsDeps{Users: users},
		Chats:     api.ChatDeps{Chats: chats},
		Documents: api.DocumentDeps{
			Documents: documents,
			Paths: api.DocumentPaths{
				MarkdownDir:       os.Getenv("AXIOM_NG_MARKDOWN_DIR"),
				LegacyMarkdownDir: os.Getenv("AXIOM_NG_LEGACY_MARKDOWN_DIR"),
				ImagesDir:         os.Getenv("AXIOM_NG_IMAGES_DIR"),
			},
		},
		DocumentGroups: api.DocumentGroupDeps{Groups: groups, Documents: documents},
		RAG:            api.RAGDeps{Chunks: chunks},
		Search:         newSearchDeps(gormDB, documents, gpuProbe, cfg, logger),
		Upload: api.UploadDeps{
			Documents:   documents,
			Groups:      groups,
			RawFilesDir: cfg.RawFilesDir,
		},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: userLookup{users: users},
		},
	}
	return deps, gormDB, nil
}

func closeDB(gormDB *gorm.DB, logger *slog.Logger) {
	sqlDB, err := gormDB.DB()
	if err != nil {
		logger.Warn("close db: gorm.DB unwrap failed", slog.String("error", err.Error()))
		return
	}
	if err := sqlDB.Close(); err != nil {
		logger.Warn("close db", slog.String("error", err.Error()))
	}
}

// secretFromEnv reads the JWT signing secret honouring the Python
// backend's env-var fallback order (JWT_SECRET_KEY, then SECRET_KEY).
func secretFromEnv() string {
	if v := os.Getenv("JWT_SECRET_KEY"); v != "" {
		return v
	}
	return os.Getenv("SECRET_KEY")
}

// ensureFirstUser creates a default admin when the users table is
// empty. Matches axiom_backend/setup_first_user.py + the wiring in
// axiom_backend/main.py:186. No-op if users already exist.
func ensureFirstUser(ctx context.Context, users *repo.Users, logger *slog.Logger) error {
	count, err := users.Count(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}
	username := envOr("ADMIN_USERNAME", "admin")
	password := envOr("ADMIN_PASSWORD", "admin123")
	email := envOr("ADMIN_EMAIL", "admin@axiom.local")

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if _, err := users.Create(ctx, repo.CreateInput{
		Username:       username,
		Email:          email,
		HashedPassword: hash,
		IsAdmin:        true,
	}); err != nil {
		return fmt.Errorf("create admin: %w", err)
	}
	if password == "admin123" {
		logger.Warn("default admin bootstrapped with password 'admin123' — change it immediately",
			slog.String("username", username))
	} else {
		logger.Info("admin user bootstrapped from ADMIN_* env vars",
			slog.String("username", username))
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// newSearchDeps wires the OpenSearch client + hybrid retriever for
// /api/documents/search/fulltext and /api/search/. Missing OpenSearch
// config is fine — handlers degrade gracefully (503 / empty results).
// The caller passes in the already-constructed GPU probe so it is
// shared with /api/system/gpu-status rather than dialled twice.
func newSearchDeps(gdb *gorm.DB, docs *repo.Documents, gpu *gpuworker.Client, cfg config.Config, logger *slog.Logger) api.SearchDeps {
	var osClient *opensearch.Client
	osCfg := opensearch.FromEnv(nil)
	if cfg.OpenSearchURL != "" {
		osCfg.Enabled = true
	}
	if osCfg.Enabled {
		c, err := opensearch.NewClient(osCfg)
		if err != nil {
			logger.Warn("opensearch disabled", slog.String("error", err.Error()))
		} else {
			osClient = c
		}
	}

	ret := &retriever.Retriever{
		DB:         gdb,
		OpenSearch: osClient,
		GPU:        gpu,
	}

	return api.SearchDeps{
		OpenSearch: osClient,
		Retriever:  ret,
		UserDocs:   api.UserDocsRepoAdapter{DB: api.NewDocumentIDLister(docs)},
	}
}

// dbHealth adapts *gorm.DB to the system-handler Ping signature without
// creating an import cycle.
type dbHealth struct{ gdb *gorm.DB }

func (h dbHealth) Ping(ctx context.Context) error { return db.Ping(ctx, h.gdb) }

// newGPUProbe constructs the shared GPU probe. Honors
// AXIOM_NG_GPU_WORKER_SOCKET first, then AXIOM_GPU_WORKER_SOCKET
// (Python parity), then the Python default /tmp/axiom-gpu.sock. A
// missing socket file still returns a valid client — the handler
// surfaces ErrNoSocket as 'not_connected'.
func newGPUProbe(cfg config.Config) *gpuworker.Client {
	path := cfg.GPUWorkerSocket
	if path == "" {
		path = gpuworker.ResolveSocketPath(nil)
	}
	return gpuworker.NewClient(path)
}

// userLookup adapts *repo.Users to server.UserResolver.
type userLookup struct{ users *repo.Users }

func (u userLookup) GetUserByUsername(ctx context.Context, username string) (authctx.User, error) {
	user, err := u.users.GetByUsername(ctx, username)
	if err != nil {
		return authctx.User{}, err
	}
	return authctx.User{
		ID:       user.ID,
		Username: user.Username,
		IsAdmin:  user.IsAdmin,
		IsActive: user.IsActive,
	}, nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
