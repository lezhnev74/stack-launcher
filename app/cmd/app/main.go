package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/httpserver"
	"stack-launcher.local/app/internal/ledger"
)

func main() {
	path := flag.String("c", "", "path to YAML configuration (required)")
	flag.Parse()
	log.Fatal(run(*path))
}

func run(path string) error {
	if path == "" {
		return errors.New("-c config path is required (usage: stack-launcher -c config.yml)")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	lock, err := ledger.LockState(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("config %q: %w", path, err)
	}
	defer func() { _ = lock.Close() }()
	db, err := ledger.Open(context.Background(), cfg.StateDir)
	if err != nil {
		return fmt.Errorf("config %q: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, err := httpserver.NewAPI(ctx, cfg, db)
	if err != nil {
		return fmt.Errorf("config %q: recovery: %w", path, err)
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return fmt.Errorf("config %q: listen %q: %w", path, cfg.Listen, server.ListenAndServe())
}
