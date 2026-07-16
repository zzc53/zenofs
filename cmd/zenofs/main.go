package main

import (
	"log"
	"net/http"
	"os"

	"github.com/zzc53/zenofs/internal/api"
	"github.com/zzc53/zenofs/internal/config"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
)

func main() {
	cfg := config.LoadConfig(os.Args)
	log.Printf("config loaded: %+v", cfg)

	dbManager, err := db.New(cfg.DatabaseUrl)
	if err != nil {
		log.Fatalf("failed to init database: %+v", err)
	}
	if err := dbManager.AutoMigrate(); err != nil {
		log.Fatalf("failed to migrate database: %+v", err)
	}
	defer dbManager.Close()

	ch := make(chan []db.WriteQueue, 1000)
	pm := pool.New(dbManager, ch, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	pm.StartParityWorker(100)

	r := api.NewRouter(pm, dbManager)

	addr := ":8080"
	log.Printf("zenofs API listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}
