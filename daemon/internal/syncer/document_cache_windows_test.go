//go:build windows

package syncer

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

func TestSQLiteFileDSNOpensWindowsDrivePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	dsn := sqliteFileDSN(path)

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse SQLite DSN: %v", err)
	}
	wantPath := "/" + filepath.ToSlash(path)
	if parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != wantPath {
		t.Fatalf("sqliteFileDSN(%q) = %q, want file URI path %q", path, dsn, wantPath)
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open SQLite DSN: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping SQLite DSN: %v", err)
	}
}
