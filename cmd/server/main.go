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

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	transporthttp "github.com/anton-bystrov/webhook-telegram-proxy/internal/http"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/logging"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/telegram"
	alerttemplate "github.com/anton-bystrov/webhook-telegram-proxy/internal/template"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		slog.Error("parse configuration", "error", err)
		os.Exit(1)
	}

	logger := logging.New(cfg.LogLevel)
	metricsRegistry, err := metrics.New(version, revision)
	if err != nil {
		logger.Error("initialize metrics", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sqliteStore, err := store.New(ctx, cfg.StorePath, metricsRegistry, logger)
	if err != nil {
		logger.Error("initialize store", "error", err)
		os.Exit(1)
	}
	if count, err := sqliteStore.RequeueSending(ctx, time.Now().UTC()); err != nil {
		logger.Error("requeue sending deliveries after restart", "error", err)
		os.Exit(1)
	} else if count > 0 {
		logger.Info("requeued sending deliveries after restart", "count", count)
	}
	defer func() {
		if err := sqliteStore.Close(); err != nil {
			logger.Warn("close store", "error", err)
		}
	}()

	renderer, err := alerttemplate.Load(cfg.AlertTemplatePath, metricsRegistry)
	if err != nil {
		logger.Error("load alert template", "error", err)
		os.Exit(1)
	}

	telegramClient := telegram.NewHTTPClient(cfg.TelegramBotToken, cfg.HTTPWriteTimeout, metricsRegistry)
	deliveryService := service.NewDeliveryService(cfg, sqliteStore, renderer, telegramClient, metricsRegistry, logger)
	alertService := service.NewAlertService(sqliteStore, deliveryService, metricsRegistry, logger)

	router := transporthttp.NewRouter(cfg, alertService, sqliteStore, metricsRegistry, logger)
	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTPReadTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	go deliveryService.Run(ctx)

	go func() {
		logger.Info("server started", "address", cfg.Address(), "environment", cfg.Environment, "version", version, "revision", revision)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped unexpectedly", "error", err)
			stop()
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown http server", "error", err)
		os.Exit(1)
	}

	logger.Info("server stopped")
}
