// Command axiom-ng serves the Go implementation of the axiom backend.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
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

	return server.New(cfg, logger).Run(ctx)
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
