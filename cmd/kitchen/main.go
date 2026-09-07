package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/avito-intern/kitchen/internal/config"
	"github.com/avito-intern/kitchen/internal/kitchen"
	"github.com/avito-intern/kitchen/internal/outbox"
	"github.com/avito-intern/kitchen/internal/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.LoadKitchen()
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := pingDatabase(ctx, db); err != nil {
		logger.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	store := postgres.New(db)
	service := kitchen.NewService(store)
	dispatcher := outbox.NewDispatcher(store, cfg.CallbackToken, cfg.OutboxPollInterval, logger)
	go dispatcher.Run(ctx)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           kitchen.NewHandler(service, store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		logger.Info("kitchen service started", "address", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve kitchen", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown kitchen", "error", err)
	}
}

func pingDatabase(ctx context.Context, db *sql.DB) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = db.PingContext(checkCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return lastErr
}
