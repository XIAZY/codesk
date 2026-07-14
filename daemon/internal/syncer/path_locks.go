package syncer

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const pathLockLeaseDuration = 30 * time.Second

type pathLockStore struct {
	db        *sql.DB
	ownerID   string
	closeOnce sync.Once
	closeErr  error
}

type pathLockLease struct {
	key   string
	token string
}

type pathLockLeaseStore interface {
	cleanupExpired(time.Time) error
	lock([]string) ([]pathLockLease, error)
	release([]pathLockLease) error
	Close() error
}

type pathLockStoreOpener func(string) (pathLockLeaseStore, error)

func openPathLockStore(root string) (pathLockLeaseStore, error) {
	return newPathLockStore(root)
}

// newPathLockStore returns a closeable store owned by a workspace runtime. The
// runtime keeps it open while any WorkspaceFS user can acquire a lease and
// closes it only after those users have stopped.
func newPathLockStore(root string) (*pathLockStore, error) {
	root = filepath.Clean(root)
	if err := os.MkdirAll(filepath.Join(root, ".notty"), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".notty", "path_locks.db")
	db, err := sql.Open("sqlite3", sqliteFileDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &pathLockStore{db: db, ownerID: "daemon_" + uuid.NewString()}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *pathLockStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
	})
	return s.closeErr
}

func (s *pathLockStore) initSchema() error {
	if s == nil || s.db == nil {
		return nil
	}
	for _, stmt := range []string{
		`pragma journal_mode = wal`,
		`pragma busy_timeout = 5000`,
		`create table if not exists path_locks (
			path_key text primary key,
			workspace_path text not null,
			owner_id text not null,
			lease_token text not null,
			purpose text,
			acquired_at integer not null,
			expires_at integer not null
		)`,
		`create index if not exists path_locks_expiry on path_locks(expires_at)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *pathLockStore) cleanupExpired(now time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`delete from path_locks where expires_at <= ?`, unixNano(now))
	return err
}

func (s *pathLockStore) lock(paths []string) ([]pathLockLease, error) {
	if s == nil || len(paths) == 0 {
		return nil, nil
	}
	sort.Strings(paths)
	deadline := time.Now().Add(5 * time.Second)
	for {
		leases := make([]pathLockLease, 0, len(paths))
		acquiredAll := true
		for _, path := range paths {
			lease, ok, err := s.tryAcquire(path)
			if err != nil {
				_ = s.release(leases)
				return nil, err
			}
			if !ok {
				_ = s.release(leases)
				acquiredAll = false
				break
			}
			leases = append(leases, lease)
		}
		if acquiredAll {
			return leases, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for sqlite path lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *pathLockStore) tryAcquire(path string) (pathLockLease, bool, error) {
	now := time.Now().UTC()
	expires := now.Add(pathLockLeaseDuration)
	key := sha256Hex([]byte(filepath.ToSlash(path)))
	token := "lease_" + uuid.NewString()
	res, err := s.db.Exec(`insert into path_locks (
			path_key, workspace_path, owner_id, lease_token, purpose, acquired_at, expires_at
		) values (?, ?, ?, ?, ?, ?, ?)
		on conflict(path_key) do update set
			workspace_path = excluded.workspace_path,
			owner_id = excluded.owner_id,
			lease_token = excluded.lease_token,
			purpose = excluded.purpose,
			acquired_at = excluded.acquired_at,
			expires_at = excluded.expires_at
		where path_locks.expires_at <= ?`,
		key, path, s.ownerID, token, "workspace-fs", unixNano(now), unixNano(expires), unixNano(now))
	if err != nil {
		return pathLockLease{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return pathLockLease{}, false, err
	}
	if affected == 0 {
		return pathLockLease{}, false, nil
	}
	return pathLockLease{key: key, token: token}, true, nil
}

func (s *pathLockStore) release(leases []pathLockLease) error {
	if s == nil || len(leases) == 0 {
		return nil
	}
	for i := len(leases) - 1; i >= 0; i-- {
		if _, err := s.db.Exec(`delete from path_locks where path_key = ? and lease_token = ?`, leases[i].key, leases[i].token); err != nil {
			return err
		}
	}
	return nil
}
