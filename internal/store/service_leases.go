package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	serviceLeaseBusyTimeout = 50 * time.Millisecond
	serviceLeaseMaxWait     = 30 * time.Second
)

// TryAcquireServiceLease atomically claims a named cross-process work lease.
// The schema matches Python's service_leases table so both backends coordinate.
func (s *SQLite) TryAcquireServiceLease(name, owner string, lease time.Duration) (bool, error) {
	return s.tryAcquireServiceLease(context.Background(), false, name, owner, lease)
}

// TryAcquireServiceLeaseContext is TryAcquireServiceLease with cancellation
// while waiting for SQLite's writer lock.
func (s *SQLite) TryAcquireServiceLeaseContext(ctx context.Context, name, owner string, lease time.Duration) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, serviceLeaseMaxWait)
	defer cancel()
	return s.tryAcquireServiceLease(waitCtx, true, name, owner, lease)
}

func (s *SQLite) tryAcquireServiceLease(ctx context.Context, cancelOnContext bool, name, owner string, lease time.Duration) (bool, error) {
	if name == "" || owner == "" {
		return false, nil
	}
	if lease <= 0 {
		lease = 120 * time.Second
	}
	deadline := time.Now().Add(serviceLeaseMaxWait)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if cancelOnContext {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", serviceLeaseBusyTimeout.Milliseconds())); err != nil {
			return false, err
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=30000")
		}()
		if err := execServiceLeaseWithRetry(ctx, conn, deadline, `CREATE TABLE IF NOT EXISTS service_leases (
 name TEXT PRIMARY KEY, owner TEXT NOT NULL, acquired_at TEXT NOT NULL
	)`); err != nil {
			return false, err
		}
	} else if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS service_leases (
 name TEXT PRIMARY KEY, owner TEXT NOT NULL, acquired_at TEXT NOT NULL
)`); err != nil {
		return false, err
	}
	if err := beginServiceLeaseImmediate(ctx, conn, cancelOnContext, deadline); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	now := time.Now().UTC()
	var existingOwner, acquired string
	queryErr := conn.QueryRowContext(ctx, "SELECT owner, acquired_at FROM service_leases WHERE name=?", name).Scan(&existingOwner, &acquired)
	if queryErr != nil && queryErr != sql.ErrNoRows {
		return false, queryErr
	}
	if queryErr == nil {
		at, parseErr := time.Parse(time.RFC3339Nano, acquired)
		if parseErr == nil && existingOwner != owner && now.Sub(at) < lease {
			if err := commitServiceLeaseContext(ctx, conn, &committed); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO service_leases(name, owner, acquired_at) VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET owner=excluded.owner, acquired_at=excluded.acquired_at`, name, owner, storedTimestamp(now)); err != nil {
		return false, err
	}
	if err := commitServiceLeaseContext(ctx, conn, &committed); err != nil {
		return false, err
	}
	return true, nil
}

func beginServiceLeaseImmediate(ctx context.Context, conn *sql.Conn, cancelOnContext bool, deadline time.Time) error {
	if !cancelOnContext {
		_, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		return err
	}
	return execServiceLeaseWithRetry(ctx, conn, deadline, "BEGIN IMMEDIATE")
}

func execServiceLeaseWithRetry(ctx context.Context, conn *sql.Conn, deadline time.Time, query string, args ...any) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, query, args...)
		if err == nil {
			return nil
		}
		if !sqliteBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RenewServiceLease advances acquired_at only while the caller still owns the
// named lease. The owner predicate prevents a stale worker from extending a
// lease that another process has already acquired.
func (s *SQLite) RenewServiceLease(name, owner string) (bool, error) {
	if name == "" || owner == "" {
		return false, nil
	}
	result, err := s.db.Exec("UPDATE service_leases SET acquired_at=? WHERE name=? AND owner=?", storedTimestamp(time.Now().UTC()), name, owner)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *SQLite) ReleaseServiceLease(name, owner string) error {
	_, err := s.db.Exec("DELETE FROM service_leases WHERE name=? AND owner=?", name, owner)
	return err
}

// ServiceLeaseActive reports whether a live lease with the given name exists.
// It is a read-only probe used by HTTP refresh requests to avoid waiting behind
// a background owner before deciding to serve the current SQLite snapshot.
func (s *SQLite) ServiceLeaseActive(name string, lease time.Duration) (bool, error) {
	if name == "" {
		return false, nil
	}
	if lease <= 0 {
		lease = 120 * time.Second
	}
	var acquired string
	err := s.db.QueryRow("SELECT acquired_at FROM service_leases WHERE name=?", name).Scan(&acquired)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339Nano, acquired)
	if err != nil {
		return false, nil
	}
	return time.Since(at) < lease, nil
}

func commitServiceLeaseContext(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	*committed = true
	return nil
}
