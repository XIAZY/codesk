package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"notty/backend/internal/notty"
)

func main() {
	cfg := notty.LoadConfig()
	dataSource := cfg.DataFile
	if cfg.DatabaseURL != "" {
		dataSource = cfg.DatabaseURL
	}
	store, err := notty.NewStore(dataSource)
	if err != nil {
		log.Fatalf("create store: %v", err)
	}
	defer store.Close()

	server := notty.NewServer(cfg, store)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("notty backend listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
