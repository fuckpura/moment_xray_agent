package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/perfect-panel/moment/xray-agent/internal/app"
	"github.com/perfect-panel/moment/xray-agent/internal/config"
	"github.com/perfect-panel/moment/xray-agent/internal/logging"
)

func main() {
	cfg := config.Load()
	closeLog, writer, err := logging.Open(cfg.Log.AgentPath)
	if err != nil {
		slog.Error("open log failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer closeLog()

	logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	runner := app.New(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agent stopped with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
	logger.Info("agent stopped")
}
