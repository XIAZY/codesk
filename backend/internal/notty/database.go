package notty

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Database struct {
	DB  *sql.DB
	URL string
}

func OpenDatabase(databaseURL string) (*Database, error) {
	databaseURL = strings.TrimSpace(databaseURL)
	if !isPostgresDSN(databaseURL) {
		return nil, fmt.Errorf("Postgres database URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initPostgresSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// One-time data migration: seed existing accounts' already-earned learning milestones so the
	// frontend cutover doesn't re-onboard them. The per-account guard + ON CONFLICT make it a no-op
	// for any account that already has completion rows, so it's safe to run on every boot.
	if err := backfillOnboardingLearningCompletions(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Database{DB: db, URL: databaseURL}, nil
}

func (d *Database) Close() error {
	if d == nil || d.DB == nil {
		return nil
	}
	return d.DB.Close()
}
