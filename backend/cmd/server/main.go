package main

import (
	"context"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"notty/backend/internal/notty"
)

func main() {
	cfg := notty.LoadConfig()
	if cfg.PprofAddr != "" {
		go func() {
			log.Printf("pprof listening on %s", cfg.PprofAddr)
			if err := http.ListenAndServe(cfg.PprofAddr, nil); err != nil {
				log.Printf("pprof stopped: %v", err)
			}
		}()
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("NOTTY_DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		log.Fatal("NOTTY_JWT_SECRET is required")
	}
	if err := cfg.ValidateEmailConfig(); err != nil {
		log.Fatal(err)
	}
	if !cfg.MailgunConfigured() {
		log.Print("WARNING: email sender is not configured; verification and password reset emails will not be delivered. Set NOTTY_MAILGUN_DOMAIN, NOTTY_MAILGUN_API_KEY, and NOTTY_MAILGUN_FROM, or set NOTTY_REQUIRE_EMAIL=1 to fail startup when email is unavailable.")
	}
	store, err := notty.NewStore(cfg.DatabaseURL)
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
