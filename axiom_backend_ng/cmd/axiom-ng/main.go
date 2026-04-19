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

	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
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

	return server.NewWithDeps(cfg, logger, deps).Run(ctx)
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

	deps := server.Deps{
		Auth: api.AuthDeps{
			Users:          users,
			SystemSettings: sysSettings,
			Signer:         signer,
		},
		Languages: api.LanguageDeps{Languages: langs},
		System:    api.SystemDeps{Health: dbHealth{gdb: gormDB}},
		Dashboard: api.DashboardDeps{Stats: dash},
		Settings:  api.SettingsDeps{Users: users},
		Chats:     api.ChatDeps{Chats: chats},
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

// dbHealth adapts *gorm.DB to the system-handler Ping signature without
// creating an import cycle.
type dbHealth struct{ gdb *gorm.DB }

func (h dbHealth) Ping(ctx context.Context) error { return db.Ping(ctx, h.gdb) }

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
