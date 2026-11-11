// Package store is the server's persistence layer: one SQLite database
// (modernc.org/sqlite, pure Go) with per-feature migrations, plus a
// content-addressed-on-upload blob store on disk for base images and
// snapshots.
//
// Times are stored as INTEGER unix milliseconds (see T and Time).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB. All helpers on *sql.DB are available.
type DB struct {
	*sql.DB
	Path string
}

// Open opens (creating if needed) the database file at path with WAL,
// foreign keys and a busy timeout. Transactions take the write lock up front
// (_txlock=immediate) so concurrent writers wait instead of failing.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	// Switching a fresh file to WAL fails at once with SQLITE_BUSY (the busy
	// timeout is not honoured there) when another opener is doing the same,
	// so retry the whole init.
	var err error
	for i := 0; i < 100; i++ {
		var d *DB
		if d, err = openOnce(dsn, path); err == nil {
			return d, nil
		}
		if !isBusy(err) {
			break
		}
		time.Sleep(time.Duration(10+i) * time.Millisecond)
	}
	return nil, err
}

func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

func openOnce(dsn, path string) (*DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{DB: db, Path: path}
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		feature TEXT NOT NULL, version INTEGER NOT NULL, applied_at INTEGER NOT NULL,
		PRIMARY KEY (feature, version))`); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Migration is one schema step of a feature. Versions start at 1 and are
// applied in order, each in its own transaction. Never edit a released step;
// append a new one.
type Migration struct {
	Version int
	SQL     string
	// Func runs after SQL in the same transaction (optional).
	Func func(tx *sql.Tx) error
}

var (
	regMu      sync.Mutex
	registered = map[string][]Migration{}
)

// Register adds the migrations of a feature (call from init). Features are
// applied in name order, except "core" which always runs first.
func Register(feature string, ms ...Migration) {
	regMu.Lock()
	defer regMu.Unlock()
	registered[feature] = append(registered[feature], ms...)
}

// MigrateAll applies every registered feature's pending migrations.
func (d *DB) MigrateAll(ctx context.Context) error {
	regMu.Lock()
	names := make([]string, 0, len(registered))
	for n := range registered {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "core" || names[j] == "core" {
			return names[i] == "core"
		}
		return names[i] < names[j]
	})
	all := map[string][]Migration{}
	for _, n := range names {
		all[n] = append([]Migration(nil), registered[n]...)
	}
	regMu.Unlock()
	for _, n := range names {
		if err := d.Migrate(ctx, n, all[n]...); err != nil {
			return err
		}
	}
	return nil
}

// Migrate applies the pending migrations of one feature.
func (d *DB) Migrate(ctx context.Context, feature string, ms ...Migration) error {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for _, m := range ms {
		// The check runs inside the (immediate, write-locked) transaction, so a
		// second process migrating the same file waits and then skips the step.
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE feature=? AND version=?`, feature, m.Version).Scan(&n); err != nil || n > 0 {
				return err
			}
			if m.SQL != "" {
				if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
					return err
				}
			}
			if m.Func != nil {
				if err := m.Func(tx); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(feature,version,applied_at) VALUES(?,?,?)`, feature, m.Version, Now())
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate %s v%d: %w", feature, m.Version, err)
		}
	}
	return nil
}

// Tx runs fn in a transaction (committed when fn returns nil).
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// IsNoRows reports sql.ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Now returns the current time as unix milliseconds.
func Now() int64 { return time.Now().UnixMilli() }

// T converts a time to unix milliseconds (0 for the zero time).
func T(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Time converts unix milliseconds to a UTC time (zero for 0).
func Time(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// NullTime converts a nullable millisecond column to *time.Time.
func NullTime(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := Time(v.Int64)
	return &t
}

// NullMs returns a value for a nullable millisecond column.
func NullMs(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
