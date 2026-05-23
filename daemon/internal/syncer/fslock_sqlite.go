package syncer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FSLockDB struct {
	db     *sql.DB
	holder string
}

func OpenFSLockDB(root string, holder string) (*FSLockDB, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	lockPath := filepath.Join(root, ".notty", "fslock.sqlite")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", lockPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	lock := &FSLockDB{db: db, holder: firstNonEmptyText(holder, "notty-daemon")}
	if err := lock.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return lock, nil
}

func (l *FSLockDB) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

func (l *FSLockDB) WithFilesystemLock(ctx context.Context, operation string, pathA string, pathB string, fn func() error) error {
	if l == nil || l.db == nil {
		if fn == nil {
			return nil
		}
		return fn()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	_, err = conn.ExecContext(ctx, `
		INSERT INTO lock_meta (id, holder, operation, path_a, path_b, acquired_at)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			holder = excluded.holder,
			operation = excluded.operation,
			path_a = excluded.path_a,
			path_b = excluded.path_b,
			acquired_at = excluded.acquired_at`,
		l.holder,
		strings.TrimSpace(operation),
		pathA,
		pathB,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return err
	}
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM lock_meta WHERE id = 1`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (l *FSLockDB) init(ctx context.Context) error {
	if l == nil || l.db == nil {
		return errors.New("fs lock db is required")
	}
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = NORMAL`,
		`CREATE TABLE IF NOT EXISTS lock_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			holder TEXT,
			operation TEXT,
			path_a TEXT,
			path_b TEXT,
			acquired_at TEXT
		)`,
	} {
		if _, err := l.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
