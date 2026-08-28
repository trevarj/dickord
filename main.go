package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/config/config.json", "path to non-secret JSON configuration")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("starting Dickord", "version", version)
	if err := newBridge(cfg, logger).Run(ctx); err != nil {
		logger.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("Dickord stopped")
}
