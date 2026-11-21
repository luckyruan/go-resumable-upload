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

	"github.com/interview/go-resumable-upload/internal/api"
	"github.com/interview/go-resumable-upload/internal/config"
	"github.com/interview/go-resumable-upload/internal/service"
	"github.com/interview/go-resumable-upload/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	startup, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	metadata, err := store.Open(startup, settings.DatabaseURL)
	if err != nil {
		// Do not expose a potentially credential-bearing connection string from a driver error.
		return errors.New("database initialization failed; check DATABASE_URL and database availability")
	}
	defer metadata.Close()
	disk, err := service.NewDisk(settings.DataDir)
	if err != nil {
		return err
	}
	uploads := service.New(metadata, disk, settings.MaxFileSize)
	server := &http.Server{
		Addr:              settings.Address,
		Handler:           api.New(uploads, settings, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       settings.UploadTimeout,
		WriteTimeout:      settings.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	logger.Info("upload server listening", "address", settings.Address)
	select {
	case err := <-failures:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
