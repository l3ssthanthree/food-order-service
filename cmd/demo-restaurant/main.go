package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avito-intern/kitchen/internal/config"
	"github.com/avito-intern/kitchen/internal/demorestaurant"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.LoadDemoRestaurant()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service := demorestaurant.New(demorestaurant.Config{
		KitchenBaseURL: cfg.KitchenBaseURL,
		PartnerToken:   cfg.PartnerToken,
		CallbackToken:  cfg.CallbackToken,
	}, logger)
	go service.SyncMenuUntilReady(ctx)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		logger.Info("demo restaurant started", "address", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve demo restaurant", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown demo restaurant", "error", err)
	}
}
