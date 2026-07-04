package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"notty/backend/internal/notty"
)

func main() {
	verifyOnly := flag.Bool("verify-only", false, "verify the Group 1 UUID migration without applying it")
	flag.Parse()

	cfg := notty.LoadConfig()
	if cfg.DatabaseURL == "" {
		log.Fatal("NOTTY_DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if *verifyOnly {
		if err := notty.VerifyUUIDGroup1Migration(ctx, db); err != nil {
			log.Fatalf("verify uuid group1 migration: %v", err)
		}
		log.Printf("uuid group1 migration verification passed")
		return
	}
	if err := notty.RunUUIDGroup1Migration(ctx, db); err != nil {
		log.Fatalf("run uuid group1 migration: %v", err)
	}
	log.Printf("uuid group1 migration completed")
}
