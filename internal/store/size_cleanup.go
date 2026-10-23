package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (s *SQLite) AvailableDiskSpaceMB() (int64, error) {
	path := s.path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}
	free, err := freeBytes(path)
	if err != nil {
		return 0, err
	}
	return free / 1048576, nil
}

const (
	sizeCleanupProtectHours = 24
	sizeCleanupTargetRatio  = 0.8
	sizeCleanupVacuumRatio  = 1.2
	sizeCleanupDefaultBatch = 5000
	noCleanupFloor          = "9999-12-31T23:59:59+00:00"
)

// PruneLegacyRawJSON clears raw_json payloads left by older writers. The
// payload column is write-only for the Go/Python read paths, so this update
// selects only rowids and never scans payload values into the process. Work is
// bounded by batchSize and budget, and is safe to repeat after a partial run.
func (s *SQLite) PruneLegacyRawJSON(batchSize int, budget time.Duration) (int64, error) {
	if batchSize < 1 {
		batchSize = sizeCleanupDefaultBatch
	}
	cleared, err := s.clearRawJSON(batchSize, time.Now().Add(budget))
	if err != nil {
		return cleared, err
	}
	if cleared == 0 {
		return 0, nil
	}
	_, err = s.checkpointAndVacuum()
	return cleared, err
}

// CleanupBySize bounds the SQLite database by removing storage that is not
// needed by the live readers. It intentionally follows the Python cleanup
// ladder: discard obsolete raw payloads and refresh history first, then thin
// superseded snapshots from least to most aggressively.
func (s *SQLite) CleanupBySize(maxBytes int64, batchSize int, budget time.Duration) (map[string]any, error) {
	if batchSize < 1 {
		batchSize = sizeCleanupDefaultBatch
	}
	before, err := s.DatabaseSizeBytes()
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"limit_bytes":                     maxBytes,
		"before_bytes":                    before,
		"after_bytes":                     before,
		"live_bytes":                      before,
		"raw_json_cleared":                int64(0),
		"superseded_options":              int64(0),
		"superseded_quotes":               int64(0),
		"stale_runs":                      int64(0),
		"intraday_bars_pruned":            int64(0),
		"chan_adjusted_bars_pruned":       int64(0),
		"chan_structure_revisions_pruned": int64(0),
		"vacuumed":                        false,
	}
	if maxBytes <= 0 || before <= maxBytes {
		return result, nil
	}

	deadline := time.Now().Add(budget)
	cleanupCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	target := int64(float64(maxBytes) * sizeCleanupTargetRatio)
	protectAfter := pythonISOTimestamp(time.Now().UTC().Add(-sizeCleanupProtectHours * time.Hour))
	live, err := s.databaseLiveBytesContext(cleanupCtx)
	if err != nil && cleanupCtx.Err() == nil {
		return nil, err
	}
	if cleanupCtx.Err() != nil {
		live = target + 1
	}
	if live > target {
		cleared, clearErr := s.clearRawJSON(batchSize, deadline)
		if clearErr != nil {
			return nil, clearErr
		}
		result["raw_json_cleared"] = cleared
		stale, staleErr := s.deleteStaleRuns(protectAfter, batchSize, deadline)
		if staleErr != nil {
			return nil, staleErr
		}
		result["stale_runs"] = stale

		for _, step := range []struct {
			floor       string
			bucketChars int
		}{
			{floor: protectAfter, bucketChars: 10},
			{floor: protectAfter, bucketChars: 13},
			{floor: noCleanupFloor, bucketChars: 0},
		} {
			live, err = s.databaseLiveBytesContext(cleanupCtx)
			if err != nil && cleanupCtx.Err() == nil {
				return nil, err
			}
			if cleanupCtx.Err() != nil {
				break
			}
			if live <= target || time.Now().After(deadline) {
				break
			}
			options, thinErr := s.thinSuperseded("option_snapshots", []string{"symbol", "expiration"}, step.floor, step.bucketChars, batchSize, deadline)
			if thinErr != nil {
				return nil, thinErr
			}
			quotes, thinErr := s.thinSuperseded("quote_snapshots", []string{"symbol"}, step.floor, step.bucketChars, batchSize, deadline)
			if thinErr != nil {
				return nil, thinErr
			}
			result["superseded_options"] = result["superseded_options"].(int64) + options
			result["superseded_quotes"] = result["superseded_quotes"].(int64) + quotes
		}
		// Intraday bars are all distinct observations, so they cannot be thinned
		// like superseded snapshots. If snapshots are still too large, discard
		// progressively older 15-minute history while retaining recent candles.
		for _, keepDays := range []int{210, 180, 90, 30, 7, 1} {
			live, err = s.databaseLiveBytesContext(cleanupCtx)
			if err != nil && cleanupCtx.Err() == nil {
				return nil, err
			}
			if cleanupCtx.Err() != nil || live <= target || time.Now().After(deadline) {
				break
			}
			cutoff := intradayRetentionFloor(time.Now(), keepDays)
			pruned, pruneErr := s.pruneIntradayBefore(cutoff, batchSize, deadline)
			if pruneErr != nil {
				return nil, pruneErr
			}
			result["intraday_bars_pruned"] = result["intraday_bars_pruned"].(int64) + pruned
		}
		// Adjusted Chan bars duplicate raw OHLC plus adjustment metadata. Keep a
		// recent minimum window, then prune older rows if the size limit still
		// requires space. This table is isolated from all raw-price consumers.
		for _, keepDays := range []int{730, 365, 180, 30, 1} {
			live, err = s.databaseLiveBytesContext(cleanupCtx)
			if err != nil && cleanupCtx.Err() == nil {
				return nil, err
			}
			if cleanupCtx.Err() != nil || live <= target || time.Now().After(deadline) {
				break
			}
			cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format(time.DateOnly)
			pruned, pruneErr := s.pruneChanAdjustedBefore(cutoff, batchSize, deadline)
			if pruneErr != nil {
				return nil, pruneErr
			}
			result["chan_adjusted_bars_pruned"] = result["chan_adjusted_bars_pruned"].(int64) + pruned
		}
		// Keep the latest structure snapshot for each symbol/timeframe because
		// it is needed to detect future revisions. Old revision audit records
		// are historical and can be pruned under the same database-size limit.
		for _, keepDays := range []int{1825, 1095, 730, 365, 180, 30, 1} {
			live, err = s.databaseLiveBytesContext(cleanupCtx)
			if err != nil && cleanupCtx.Err() == nil {
				return nil, err
			}
			if cleanupCtx.Err() != nil || live <= target || time.Now().After(deadline) {
				break
			}
			cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format(time.RFC3339Nano)
			pruned, pruneErr := s.pruneChanStructureRevisionsBefore(cutoff, batchSize, deadline)
			if pruneErr != nil {
				return nil, pruneErr
			}
			result["chan_structure_revisions_pruned"] = result["chan_structure_revisions_pruned"].(int64) + pruned
		}
	}

	changed := result["raw_json_cleared"].(int64) + result["superseded_options"].(int64) + result["superseded_quotes"].(int64) + result["stale_runs"].(int64) + result["intraday_bars_pruned"].(int64) + result["chan_adjusted_bars_pruned"].(int64) + result["chan_structure_revisions_pruned"].(int64)
	if changed > 0 || before > maxBytes {
		vacuumed, vacuumErr := s.checkpointAndVacuum()
		if vacuumErr != nil {
			return nil, vacuumErr
		}
		result["vacuumed"] = vacuumed
	}
	after, err := s.DatabaseSizeBytes()
	if err != nil {
		return nil, err
	}
	live, err = s.DatabaseLiveBytes()
	if err != nil {
		return nil, err
	}
	result["after_bytes"] = after
	result["live_bytes"] = live
	if after > maxBytes {
		log.Printf("database size remains above limit: %.1f MB / %.1f MB; removed %d superseded snapshot rows and %d intraday bars",
			float64(after)/1048576,
			float64(maxBytes)/1048576,
			result["superseded_options"].(int64)+result["superseded_quotes"].(int64),
			result["intraday_bars_pruned"].(int64)+result["chan_adjusted_bars_pruned"].(int64)+result["chan_structure_revisions_pruned"].(int64),
		)
	} else {
		log.Printf("database size cleanup complete: %.1f MB -> %.1f MB (limit %.1f MB, VACUUM=%t)",
			float64(before)/1048576,
			float64(after)/1048576,
			float64(maxBytes)/1048576,
			result["vacuumed"].(bool),
		)
	}
	return result, nil
}

func (s *SQLite) pruneChanStructureRevisionsBefore(cutoff string, batchSize int, deadline time.Time) (int64, error) {
	var deleted int64
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	for ctx.Err() == nil {
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return deleted, nil
			}
			return deleted, err
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM chan_structure_revisions WHERE id IN (
SELECT id FROM chan_structure_revisions WHERE created_at < ? ORDER BY created_at ASC, id ASC LIMIT ?)`, cutoff, batchSize)
		if err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return deleted, nil
			}
			return deleted, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	return deleted, nil
}

func (s *SQLite) pruneChanAdjustedBefore(cutoff string, batchSize int, deadline time.Time) (int64, error) {
	var deleted int64
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	for ctx.Err() == nil {
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return deleted, nil
			}
			return deleted, err
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM chan_adjusted_history WHERE rowid IN (SELECT rowid FROM chan_adjusted_history WHERE bar_date < ? ORDER BY bar_date ASC LIMIT ?)`, cutoff, batchSize)
		if err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return deleted, nil
			}
			return deleted, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	return deleted, nil
}

func intradayRetentionFloor(now time.Time, keepDays int) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	return now.In(location).AddDate(0, 0, -keepDays).Format("2006-01-02 15:04")
}

func (s *SQLite) pruneIntradayBefore(cutoff string, batchSize int, deadline time.Time) (int64, error) {
	var deleted int64
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	for ctx.Err() == nil {
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return deleted, nil
			}
			return deleted, err
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM intraday_history WHERE rowid IN (
SELECT rowid FROM intraday_history WHERE bar_time < ? ORDER BY bar_time ASC LIMIT ?)`, cutoff, batchSize)
		if err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return deleted, nil
			}
			return deleted, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	return deleted, nil
}

func (s *SQLite) clearRawJSON(batchSize int, deadline time.Time) (int64, error) {
	var cleared int64
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	for _, table := range []string{"option_snapshots", "quote_snapshots"} {
		for ctx.Err() == nil {
			if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
				if ctx.Err() != nil {
					return cleared, nil
				}
				return cleared, err
			}
			result, err := conn.ExecContext(ctx, "UPDATE "+table+" SET raw_json=NULL WHERE rowid IN (SELECT rowid FROM "+table+" WHERE raw_json IS NOT NULL LIMIT ?)", batchSize)
			if err != nil {
				if cleanupBudgetExpired(ctx, deadline, err) {
					return cleared, nil
				}
				return cleared, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return cleared, err
			}
			cleared += count
			if count < int64(batchSize) {
				break
			}
		}
	}
	return cleared, nil
}

func (s *SQLite) deleteStaleRuns(floor string, batchSize int, deadline time.Time) (int64, error) {
	var deleted int64
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	for ctx.Err() == nil {
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return deleted, nil
			}
			return deleted, err
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM refresh_runs WHERE id IN
		(SELECT id FROM refresh_runs WHERE started_at < ? LIMIT ?)`, floor, batchSize)
		if err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return deleted, nil
			}
			return deleted, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	return deleted, nil
}

// thinSuperseded uses a dedicated connection because SQLite temporary tables
// belong to one connection, while database/sql otherwise may choose another
// pooled connection for the following statement.
func (s *SQLite) thinSuperseded(table string, keys []string, floor string, bucketChars, batchSize int, deadline time.Time) (int64, error) {
	if table != "option_snapshots" && table != "quote_snapshots" {
		return 0, fmt.Errorf("unsupported cleanup table %q", table)
	}
	if len(keys) == 0 {
		return 0, fmt.Errorf("cleanup keys must not be empty")
	}
	keySQL := joinColumns(keys)
	joinKeep := joinColumnsWithAlias(keys, "k", "o")
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	defer conn.Close()
	defer restoreCleanupBusyTimeout(conn)
	if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS temp.keep_group"); err != nil {
		if cleanupBudgetExpired(ctx, deadline, err) {
			return 0, nil
		}
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE keep_group AS SELECT "+keySQL+", MAX(fetched_at) AS newest FROM "+table+" GROUP BY "+keySQL); err != nil {
		if cleanupBudgetExpired(ctx, deadline, err) {
			return 0, nil
		}
		return 0, err
	}

	extra := ""
	if bucketChars > 0 {
		bucketJoin := joinColumnsWithAlias(keys, "b", "o")
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return 0, nil
			}
			return 0, err
		}
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS temp.keep_bucket"); err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return 0, nil
			}
			return 0, err
		}
		bucketSQL := "CREATE TEMP TABLE keep_bucket AS SELECT " + keySQL + ", substr(fetched_at, 1, ?) AS bucket, MAX(fetched_at) AS newest FROM " + table + " WHERE fetched_at >= ? GROUP BY " + keySQL + ", bucket"
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return 0, nil
			}
			return 0, err
		}
		if _, err := conn.ExecContext(ctx, bucketSQL, bucketChars, floor); err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return 0, nil
			}
			return 0, err
		}
		extra = " AND NOT EXISTS (SELECT 1 FROM keep_bucket b WHERE " + bucketJoin + " AND b.bucket=substr(o.fetched_at, 1, ?) AND b.newest=o.fetched_at)"
	}

	var deleted int64
	for ctx.Err() == nil {
		query := "DELETE FROM " + table + " WHERE rowid IN (SELECT o.rowid FROM " + table + " o JOIN keep_group k ON " + joinKeep + " WHERE o.fetched_at < k.newest AND o.fetched_at < ?"
		args := []any{floor}
		if bucketChars > 0 {
			query += extra
			args = append(args, bucketChars)
		}
		query += " ORDER BY o.fetched_at ASC, o.rowid ASC LIMIT ?)"
		args = append(args, batchSize)
		if err := limitCleanupBusyTimeout(conn, deadline); err != nil {
			if ctx.Err() != nil {
				return deleted, nil
			}
			return deleted, err
		}
		result, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			if cleanupBudgetExpired(ctx, deadline, err) {
				return deleted, nil
			}
			return deleted, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	return deleted, nil
}

func joinColumns(columns []string) string {
	result := columns[0]
	for _, column := range columns[1:] {
		result += ", " + column
	}
	return result
}

func joinColumnsWithAlias(columns []string, left, right string) string {
	result := left + "." + columns[0] + "=" + right + "." + columns[0]
	for _, column := range columns[1:] {
		result += " AND " + left + "." + column + "=" + right + "." + column
	}
	return result
}

func (s *SQLite) checkpointAndVacuum() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if s.lowMemory {
		busy, err := runWALCheckpoint(ctx, conn, "PASSIVE")
		if err != nil {
			// Python treats a failed passive checkpoint as a warning: cleanup has
			// already completed, and low-memory mode skips VACUUM regardless.
			log.Printf("PASSIVE checkpoint failed: %v", err)
		} else if busy != 0 {
			log.Printf("PASSIVE checkpoint could not finish because readers are active")
		}
		log.Printf("low-memory protection: skipped VACUUM after PASSIVE checkpoint")
		return false, nil
	}
	info, err := os.Stat(s.path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		free, freeErr := freeBytes(filepath.Dir(s.path))
		if freeErr != nil {
			return false, freeErr
		}
		// VACUUM needs a second database-sized temporary copy. Keep a margin
		// for SQLite/WAL metadata so low-disk deployments skip it safely.
		if vacuumSpaceInsufficient(info.Size(), free) {
			log.Printf("skipping VACUUM: insufficient free disk space (need about %.1f MB, have %.1f MB)",
				float64(info.Size())*sizeCleanupVacuumRatio/1048576,
				float64(free)/1048576,
			)
			return false, nil
		}
	}
	busy, err := runWALCheckpoint(ctx, conn, "TRUNCATE")
	if err != nil {
		return false, err
	}
	if busy != 0 {
		log.Printf("skipping VACUUM: active readers prevented WAL checkpoint truncation")
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return false, err
	}
	busy, err = runWALCheckpoint(ctx, conn, "TRUNCATE")
	if err != nil {
		return false, err
	}
	if busy != 0 {
		log.Printf("VACUUM completed but active readers prevented WAL checkpoint truncation")
		return false, nil
	}
	return true, nil
}

func runWALCheckpoint(ctx context.Context, conn *sql.Conn, mode string) (int, error) {
	var busy, logFrames, checkpointedFrames int
	err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &logFrames, &checkpointedFrames)
	if err != nil {
		return 0, err
	}
	return busy, nil
}

func freeBytes(path string) (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return availableBytes(uint64(stats.Bavail), uint64(stats.Frsize)), nil
}

func availableBytes(availableBlocks, fragmentSize uint64) int64 {
	// shutil.disk_usage(...).free reports f_bavail, excluding blocks reserved
	// for privileged users; Bfree would overstate space available to the app.
	return int64(availableBlocks) * int64(fragmentSize)
}

func vacuumSpaceInsufficient(databaseBytes, freeBytes int64) bool {
	return float64(freeBytes) < float64(databaseBytes)*sizeCleanupVacuumRatio
}

func limitCleanupBusyTimeout(conn *sql.Conn, deadline time.Time) error {
	remaining := time.Until(deadline)
	timeoutMillis := int64(remaining / time.Millisecond)
	if timeoutMillis < 0 {
		timeoutMillis = 0
	}
	_, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", timeoutMillis))
	return err
}

func cleanupBudgetExpired(ctx context.Context, deadline time.Time, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return sqliteBusy(err) && time.Until(deadline) <= 5*time.Millisecond
}

func restoreCleanupBusyTimeout(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=30000")
}

func (s *SQLite) databaseLiveBytesContext(ctx context.Context) (int64, error) {
	var pageCount, freePages, pageSize int64
	for _, query := range []struct {
		sql   string
		value *int64
	}{
		{sql: `PRAGMA page_count`, value: &pageCount},
		{sql: `PRAGMA freelist_count`, value: &freePages},
		{sql: `PRAGMA page_size`, value: &pageSize},
	} {
		if err := s.db.QueryRowContext(ctx, query.sql).Scan(query.value); err != nil {
			return 0, err
		}
	}
	return (pageCount - freePages) * pageSize, nil
}
