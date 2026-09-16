package main

import (
	"log"

	"ircapybara/internal/config"
	"ircapybara/internal/plugins"
	"ircapybara/internal/server"
	"ircapybara/internal/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	if err := store.SeedChannels(cfg.InitialChannels); err != nil {
		log.Fatalf("seed channels: %v", err)
	}

	builtins, err := plugins.Builtins()
	if err != nil {
		log.Fatalf("plugins: %v", err)
	}
	srv, err := server.New(cfg, store, builtins)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
