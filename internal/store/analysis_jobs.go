package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	orphanedAnalysisJobMessage = "Go 服务重启后 Gamma 任务已失效"
	analysisClaimBusyTimeout   = 50 * time.Millisecond
	analysisClaimMaxWait       = 30 * time.Second
)

// ClaimAnalysisJob atomically claims one Gamma window across processes. The
// result from the previous batch is returned as resume_result. A separate Go
// checkpoint survives a worker crash after result_json is cleared for a claim.
func (s *SQLite) ClaimAnalysisJob(symbol string, horizonDays int, cooldown, stale time.Duration) (map[string]any, bool, error) {
	return s.claimAnalysisJob(context.Background(), false, symbol, horizonDays, cooldown, stale)
}

// ClaimAnalysisJobContext allows service shutdown to cancel a claim waiting
// for SQLite's writer lock before a background worker is launched.
func (s *SQLite) ClaimAnalysisJobContext(ctx context.Context, symbol string, horizonDays int, cooldown, stale time.Duration) (map[string]any, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.claimAnalysisJob(ctx, true, symbol, horizonDays, cooldown, stale)
}

func (s *SQLite) claimAnalysisJob(ctx context.Context, cancelOnContext bool, symbol string, horizonDays int, cooldown, stale time.Duration) (map[string]any, bool, error) {
	jobID := fmt.Sprintf("%s:%d", symbol, horizonDays)
	now := time.Now().UTC()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	if cancelOnContext {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", analysisClaimBusyTimeout.Milliseconds())); err != nil {
			return nil, false, err
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=30000")
		}()
	}
	if err := beginAnalysisImmediate(ctx, conn, cancelOnContext); err != nil {
		return nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var status, startedAt string
	var finishedAt, resultJSON, errorMessage sql.NullString
	queryErr := conn.QueryRowContext(ctx, `SELECT status, started_at, finished_at, result_json, error_message
FROM analysis_refresh_jobs WHERE job_id=?`, jobID).Scan(&status, &startedAt, &finishedAt, &resultJSON, &errorMessage)
	if queryErr != nil && queryErr != sql.ErrNoRows {
		return nil, false, queryErr
	}
	var rowResult map[string]any
	if queryErr == nil && resultJSON.Valid && resultJSON.String != "" {
		_ = json.Unmarshal([]byte(resultJSON.String), &rowResult)
	}
	var checkpointJSON sql.NullString
	checkpointErr := conn.QueryRowContext(ctx, `SELECT result_json FROM go_analysis_refresh_checkpoints WHERE job_id=?`, jobID).Scan(&checkpointJSON)
	if checkpointErr != nil && checkpointErr != sql.ErrNoRows {
		return nil, false, checkpointErr
	}
	var checkpointResult map[string]any
	if checkpointJSON.Valid && checkpointJSON.String != "" {
		_ = json.Unmarshal([]byte(checkpointJSON.String), &checkpointResult)
	}
	previousResult := rowResult
	if previousResult == nil || (status == "failed" && !boolValue(rowResult, "has_more")) {
		previousResult = checkpointResult
		if previousResult == nil {
			previousResult = rowResult
		}
	}
	// Import progress written by Python (which only knows result_json), and
	// discard a stale Go checkpoint when a completed result says there is no
	// next batch. A running Python worker has result_json=NULL, so its prior
	// checkpoint remains available if that worker exits before finishing.
	if queryErr == nil && resultJSON.Valid && resultJSON.String != "" && rowResult != nil {
		if hasMore, ok := rowResult["has_more"].(bool); ok {
			if hasMore {
				if err := writeAnalysisCheckpoint(ctx, conn, jobID, rowResult, stampFromNullable(finishedAt)); err != nil {
					return nil, false, err
				}
			} else if status == "completed" {
				if _, err := conn.ExecContext(ctx, `DELETE FROM go_analysis_refresh_checkpoints WHERE job_id=?`, jobID); err != nil {
					return nil, false, err
				}
			}
		}
	}
	if queryErr == nil {
		started := parseJobTime(startedAt)
		finished := parseJobTime(finishedAt.String)
		if status == "running" && !started.IsZero() && time.Since(started) < stale {
			payload := analysisJobPayload(jobID, symbol, horizonDays, status, startedAt, finishedAt.String, previousResult, errorMessage.String, false)
			return payload, false, commitAnalysisConnContext(ctx, conn, &committed)
		}
		if (status == "completed" || status == "failed") && !finished.IsZero() && time.Since(finished) < cooldown && !boolValue(previousResult, "has_more") {
			payload := analysisJobPayload(jobID, symbol, horizonDays, status, startedAt, finishedAt.String, previousResult, errorMessage.String, false)
			return payload, false, commitAnalysisConnContext(ctx, conn, &committed)
		}
	}
	stamp := storedTimestamp(now)
	_, err = conn.ExecContext(ctx, `INSERT INTO analysis_refresh_jobs
(job_id, symbol, horizon_days, status, started_at, finished_at, result_json, error_message)
VALUES (?, ?, ?, 'running', ?, NULL, NULL, NULL)
ON CONFLICT(job_id) DO UPDATE SET symbol=excluded.symbol, horizon_days=excluded.horizon_days,
status='running', started_at=excluded.started_at, finished_at=NULL, result_json=NULL, error_message=NULL`, jobID, symbol, horizonDays, stamp)
	if err != nil {
		return nil, false, err
	}
	if err := commitAnalysisConnContext(ctx, conn, &committed); err != nil {
		return nil, false, err
	}
	return map[string]any{
		"job_id": jobID, "symbol": symbol, "horizon_days": horizonDays,
		"status": "running", "started_at": stamp, "claimed": true,
		"resume_result": previousResult,
	}, true, nil
}

func stampFromNullable(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return storedTimestamp(time.Now().UTC())
}

func writeAnalysisCheckpoint(ctx context.Context, conn *sql.Conn, jobID string, result map[string]any, updatedAt string) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO go_analysis_refresh_checkpoints(job_id, result_json, updated_at)
VALUES (?, ?, ?) ON CONFLICT(job_id) DO UPDATE SET result_json=excluded.result_json, updated_at=excluded.updated_at`, jobID, string(encoded), updatedAt)
	return err
}

func beginAnalysisImmediate(ctx context.Context, conn *sql.Conn, cancelOnContext bool) error {
	if !cancelOnContext {
		_, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		return err
	}
	deadline := time.Now().Add(analysisClaimMaxWait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil {
			return nil
		} else if !sqliteBusy(err) || !time.Now().Before(deadline) {
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

func sqliteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

func (s *SQLite) FinishAnalysisJob(jobID, status string, result map[string]any, errorMessage, startedAt string) (bool, error) {
	encoded, err := json.Marshal(result)
	if result == nil {
		encoded = nil
	}
	if err != nil {
		return false, err
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	finished := storedTimestamp(time.Now().UTC())
	response, err := conn.ExecContext(ctx, `UPDATE analysis_refresh_jobs SET status=?, finished_at=?, result_json=?, error_message=?
WHERE job_id=? AND status='running' AND started_at=?`, status, finished, nullableJSON(encoded), errorMessage, jobID, startedAt)
	if err != nil {
		return false, err
	}
	count, err := response.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	if result != nil {
		if hasMore, ok := result["has_more"].(bool); ok {
			if hasMore {
				if err := writeAnalysisCheckpoint(ctx, conn, jobID, result, finished); err != nil {
					return false, err
				}
			} else if status == "completed" {
				if _, err := conn.ExecContext(ctx, `DELETE FROM go_analysis_refresh_checkpoints WHERE job_id=?`, jobID); err != nil {
					return false, err
				}
			}
		}
	}

	if status == "completed" || status == "failed" {
		var symbol string
		if err := conn.QueryRowContext(ctx, `SELECT symbol FROM analysis_refresh_jobs WHERE job_id=?`, jobID).Scan(&symbol); err != nil {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO push_events(symbol, kind, expiration, created_at) VALUES (?, 'gamma', NULL, ?)`, symbol, finished); err != nil {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM push_events
WHERE id <= (SELECT COALESCE(MAX(id), 0) - 20000 FROM push_events)`); err != nil {
			return false, err
		}
	}

	if err := commitAnalysisConnContext(ctx, conn, &committed); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLite) AnalysisJob(symbol string, horizonDays int) (map[string]any, bool, error) {
	jobID := fmt.Sprintf("%s:%d", symbol, horizonDays)
	var status, startedAt string
	var finishedAt, resultJSON, errorMessage sql.NullString
	err := s.db.QueryRow(`SELECT status, started_at, finished_at, result_json, error_message
FROM analysis_refresh_jobs WHERE job_id=?`, jobID).Scan(&status, &startedAt, &finishedAt, &resultJSON, &errorMessage)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var result map[string]any
	if resultJSON.Valid && resultJSON.String != "" {
		_ = json.Unmarshal([]byte(resultJSON.String), &result)
	}
	return analysisJobPayload(jobID, symbol, horizonDays, status, startedAt, finishedAt.String, result, errorMessage.String, false), true, nil
}

func (s *SQLite) FailOrphanedAnalysisJobs(staleAfterValues ...time.Duration) (int64, error) {
	staleAfter := 3 * time.Minute
	if len(staleAfterValues) > 0 {
		staleAfter = staleAfterValues[0]
		if staleAfter < 0 {
			staleAfter = 0
		}
	}
	cutoff := storedTimestamp(time.Now().UTC().Add(-staleAfter))
	result, err := s.db.Exec(`UPDATE analysis_refresh_jobs SET status='failed', finished_at=?, error_message=? WHERE status='running' AND started_at < ?`, storedTimestamp(time.Now().UTC()), orphanedAnalysisJobMessage, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func analysisJobPayload(jobID, symbol string, horizonDays int, status, startedAt, finishedAt string, result map[string]any, errorMessage string, claimed bool) map[string]any {
	hasMore := boolValue(result, "has_more")
	nextCursor, _ := result["next_cursor"].(string)
	processed := numericInt(result, "processed")
	total := numericInt(result, "total")
	payload := map[string]any{
		"job_id": jobID, "symbol": symbol, "horizon_days": horizonDays, "status": status,
		"started_at": startedAt, "finished_at": nil, "result": result,
		"error_message": nil, "claimed": claimed, "deferred": status == "deferred",
		"has_more": hasMore, "next_cursor": nil, "processed": processed, "total": total,
	}
	if nextCursor != "" {
		payload["next_cursor"] = nextCursor
	}
	if finishedAt != "" {
		payload["finished_at"] = finishedAt
	}
	if errorMessage != "" {
		payload["error_message"] = errorMessage
	}
	return payload
}

func commitAnalysisConnContext(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	*committed = true
	return nil
}

func parseJobTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func boolValue(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func numericInt(values map[string]any, key string) int {
	switch value := values[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}

func nullableJSON(value []byte) any {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return string(value)
}
