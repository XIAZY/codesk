package main

import (
	"context"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os/signal"
	"syscall"

	"notty/daemon/internal/syncer"
)

var version = "dev"

func main() {
	log.Printf("notty daemon version %s", version)
	cfg := syncer.LoadConfig()
	if cfg.PprofAddr != "" {
		go func() {
			log.Printf("pprof listening on %s", cfg.PprofAddr)
			if err := http.ListenAndServe(cfg.PprofAddr, nil); err != nil {
				log.Printf("pprof stopped: %v", err)
			}
		}()
	}
	service, err := syncer.New(cfg)
	if err != nil {
		log.Fatalf("create daemon: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("notty daemon syncing %s to %s", cfg.BackendURL, cfg.WorkspaceDir)
	if err := service.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("run daemon: %v", err)
	}
}
