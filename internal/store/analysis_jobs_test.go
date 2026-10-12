package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestAnalysisJobsClaimAcrossRepositoriesAndFenceLateWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analysis-jobs.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	claimed, owner, err := first.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || !owner || claimed["status"] != "running" {
		t.Fatalf("first claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	duplicate, owner, err := second.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || owner || duplicate["status"] != "running" {
		t.Fatalf("duplicate claim=%#v owner=%v err=%v", duplicate, owner, err)
	}
	started := claimed["started_at"].(string)
	if finished, err := first.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": true, "next_cursor": "2030-01-18", "processed": 1, "total": 2}, "", started); err != nil || !finished {
		t.Fatalf("finish first=%v err=%v", finished, err)
	}
	resumed, owner, err := second.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || !owner || resumed["status"] != "running" {
		t.Fatalf("resume claim=%#v owner=%v err=%v", resumed, owner, err)
	}
	resumeResult, ok := resumed["resume_result"].(map[string]any)
	if !ok || resumeResult["next_cursor"] != "2030-01-18" {
		t.Fatalf("resume result=%#v", resumed["resume_result"])
	}
	newStarted := resumed["started_at"].(string)
	if finished, err := first.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "late worker", started); err != nil || finished {
		t.Fatalf("late finish=%v err=%v, want fenced false", finished, err)
	}
	if finished, err := second.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "", newStarted); err != nil || !finished {
		t.Fatalf("current finish=%v err=%v", finished, err)
	}
	job, found, err := first.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "completed" {
		t.Fatalf("final job=%#v found=%v err=%v", job, found, err)
	}
}

func TestAnalysisJobClaimCrashRecoversDurableCheckpoint(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "analysis-checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	first, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("initial claim=%#v claimed=%v err=%v", first, claimed, err)
	}
	startedAt := first["started_at"].(string)
	progress := map[string]any{"has_more": true, "next_cursor": "2030-01-18", "processed": 1, "total": 3}
	if finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", progress, "", startedAt); err != nil || !finished {
		t.Fatalf("first batch finish=%v err=%v", finished, err)
	}

	// A worker claims the next batch and dies before it can finish. Python's
	// compatible result_json field is cleared on claim, so recovery must use the
	// Go-only checkpoint table without exposing it in the job response.
	second, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("second claim=%#v claimed=%v err=%v", second, claimed, err)
	}
	secondResume, ok := second["resume_result"].(map[string]any)
	if !ok || secondResume["next_cursor"] != "2030-01-18" {
		t.Fatalf("second batch resume=%#v", second["resume_result"])
	}
	staleStarted := time.Now().UTC().Add(-4 * time.Minute).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id='QQQ:45'`, staleStarted); err != nil {
		t.Fatal(err)
	}
	if count, err := repository.FailOrphanedAnalysisJobs(3 * time.Minute); err != nil || count != 1 {
		t.Fatalf("orphan count=%d err=%v", count, err)
	}
	recovered, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("recovery claim=%#v claimed=%v err=%v", recovered, claimed, err)
	}
	resume, ok := recovered["resume_result"].(map[string]any)
	if !ok || resume["next_cursor"] != "2030-01-18" || resume["processed"] != float64(1) {
		t.Fatalf("recovered checkpoint=%#v", recovered["resume_result"])
	}
	publicJob, found, err := repository.AnalysisJob("QQQ", 45)
	publicResult, resultOK := publicJob["result"].(map[string]any)
	if err != nil || !found || !resultOK || publicResult != nil {
		t.Fatalf("analysis job lookup=%#v found=%v err=%v, checkpoint must not leak into public result", publicJob, found, err)
	}
	var checkpointCount int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM go_analysis_refresh_checkpoints WHERE job_id='QQQ:45'`).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoint rows=%d after recovery claim, want 1", checkpointCount)
	}
	if finished, err := repository.FinishAnalysisJob("QQQ:45", "failed", map[string]any{"has_more": false}, "provider failed", recovered["started_at"].(string)); err != nil || !finished {
		t.Fatalf("failed batch finish=%v err=%v", finished, err)
	}
	resumedAfterFailure, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("resume after failed result=%#v claimed=%v err=%v", resumedAfterFailure, claimed, err)
	}
	resumeAfterFailure, ok := resumedAfterFailure["resume_result"].(map[string]any)
	if !ok || resumeAfterFailure["next_cursor"] != "2030-01-18" {
		t.Fatalf("failed result lost prior checkpoint: %#v", resumedAfterFailure["resume_result"])
	}
	if finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "", resumedAfterFailure["started_at"].(string)); err != nil || !finished {
		t.Fatalf("final finish=%v err=%v", finished, err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM go_analysis_refresh_checkpoints WHERE job_id='QQQ:45'`).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 0 {
		t.Fatalf("checkpoint rows=%d after final batch, want 0", checkpointCount)
	}
}

func TestAnalysisCheckpointLeavesPythonJobTableShapeUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-analysis-jobs.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE analysis_refresh_jobs (
job_id TEXT PRIMARY KEY, symbol TEXT NOT NULL, horizon_days INTEGER NOT NULL,
status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT,
result_json TEXT, error_message TEXT
)`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO analysis_refresh_jobs
(job_id, symbol, horizon_days, status, started_at, finished_at, result_json, error_message)
VALUES ('QQQ:45', 'QQQ', 45, 'completed', '2030-01-01T00:00:00Z', '2030-01-01T00:01:00Z', '{"has_more":true,"next_cursor":"2030-01-18"}', NULL)`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	columns, err := repository.db.Query(`PRAGMA table_info(analysis_refresh_jobs)`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for columns.Next() {
		var ordinal, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := columns.Scan(&ordinal, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = columns.Close()
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := columns.Err(); err != nil {
		_ = columns.Close()
		t.Fatal(err)
	}
	if err := columns.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"job_id", "symbol", "horizon_days", "status", "started_at", "finished_at", "result_json", "error_message"}
	if len(names) != len(want) {
		t.Fatalf("analysis_refresh_jobs columns=%v, want unchanged Python shape %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("analysis_refresh_jobs columns=%v, want unchanged Python shape %v", names, want)
		}
	}
	var status, result string
	if err := repository.db.QueryRow(`SELECT status, result_json FROM analysis_refresh_jobs WHERE job_id=?`, "QQQ:45").Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || result == "" {
		t.Fatalf("Python-compatible job row status=%q result=%q", status, result)
	}
	imported, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim Python-written progress=%#v claimed=%v err=%v", imported, claimed, err)
	}
	importedResult, ok := imported["resume_result"].(map[string]any)
	if !ok || importedResult["next_cursor"] != "2030-01-18" {
		t.Fatalf("Python-written progress was not imported: %#v", imported["resume_result"])
	}
	var checkpointTableCount int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='go_analysis_refresh_checkpoints'`).Scan(&checkpointTableCount); err != nil {
		t.Fatal(err)
	}
	if checkpointTableCount != 1 {
		t.Fatalf("Go checkpoint table count=%d, want 1", checkpointTableCount)
	}
}

func TestFailOrphanedAnalysisJobs(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "analysis-orphan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	staleStarted := time.Now().UTC().Add(-4 * time.Minute).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id='QQQ:45'`, staleStarted); err != nil {
		t.Fatal(err)
	}
	count, err := repository.FailOrphanedAnalysisJobs(3 * time.Minute)
	if err != nil || count != 1 {
		t.Fatalf("failed orphan count=%d err=%v", count, err)
	}
	job, found, err := repository.AnalysisJob("QQQ", 45)
	if err != nil || !found || job["status"] != "failed" {
		t.Fatalf("orphan job=%#v found=%v err=%v", job, found, err)
	}
}

func TestFailOrphanedAnalysisJobsClampsExplicitThresholdToZero(t *testing.T) {
	for _, staleAfter := range []time.Duration{0, -time.Second} {
		t.Run(staleAfter.String(), func(t *testing.T) {
			repository, err := Open(filepath.Join(t.TempDir(), "analysis-orphan-zero.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			if _, claimed, err := repository.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute); err != nil || !claimed {
				t.Fatalf("claim=%v err=%v", claimed, err)
			}

			count, err := repository.FailOrphanedAnalysisJobs(staleAfter)
			if err != nil || count != 1 {
				t.Fatalf("failed orphan count=%d err=%v for threshold %s", count, err, staleAfter)
			}
		})
	}
}

func TestClaimAnalysisJobContextHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analysis-claim-cancel.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	claimant, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer claimant.Close()
	writer, err := owner.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(context.Background(), "ROLLBACK")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	claimDone := make(chan struct {
		claimed bool
		err     error
	}, 1)
	go func() {
		_, claimed, claimErr := claimant.ClaimAnalysisJobContext(ctx, "QQQ", 45, time.Second, time.Minute)
		claimDone <- struct {
			claimed bool
			err     error
		}{claimed: claimed, err: claimErr}
	}()
	deadline := time.Now().Add(time.Second)
	for claimant.db.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if claimant.db.Stats().InUse == 0 {
		t.Fatal("claim did not reach SQLite")
	}
	select {
	case outcome := <-claimDone:
		if outcome.err == nil || outcome.claimed || ctx.Err() == nil {
			t.Fatalf("canceled claim=(claimed=%v, err=%v, ctx=%v), want cancellation", outcome.claimed, outcome.err, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("claim did not return after its context expired")
	}
	if _, found, err := claimant.AnalysisJob("QQQ", 45); err != nil || found {
		t.Fatalf("canceled claim persisted a job: found=%v err=%v", found, err)
	}
}

func TestClaimAnalysisJobCooldownSuppressesFailedJobs(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "failed-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !owner {
		t.Fatalf("initial claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	startedAt, ok := claimed["started_at"].(string)
	if !ok || startedAt == "" {
		t.Fatalf("missing started_at in claim: %#v", claimed)
	}
	if finished, err := repository.FinishAnalysisJob("QQQ:45", "failed", map[string]any{"has_more": false}, "provider failed", startedAt); err != nil || !finished {
		t.Fatalf("finish failed=%v err=%v", finished, err)
	}

	deferred, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || owner || deferred["status"] != "failed" {
		t.Fatalf("failed cooldown claim=%#v owner=%v err=%v", deferred, owner, err)
	}
	if deferred["error_message"] != "provider failed" || deferred["claimed"] != false {
		t.Fatalf("failed cooldown payload=%#v", deferred)
	}
}

func TestFinishAnalysisJobRollsBackWhenGammaEventFails(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "analysis-event-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	firstClaim, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !owner {
		t.Fatalf("first claim=%#v owner=%v err=%v", firstClaim, owner, err)
	}
	if finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", map[string]any{
		"has_more": true, "next_cursor": "2030-01-18", "processed": 1, "total": 2,
	}, "", firstClaim["started_at"].(string)); err != nil || !finished {
		t.Fatalf("checkpoint finish=%v err=%v", finished, err)
	}
	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !owner {
		t.Fatalf("next claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	startedAt := claimed["started_at"].(string)
	if _, err := repository.db.Exec(`CREATE TRIGGER fail_gamma_event
BEFORE INSERT ON push_events WHEN NEW.kind='gamma'
BEGIN SELECT RAISE(ABORT, 'gamma event rejected'); END`); err != nil {
		t.Fatal(err)
	}

	finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "", startedAt)
	if err == nil || finished {
		t.Fatalf("finish=%v err=%v, want propagated event error", finished, err)
	}
	var status string
	var finishedAt sql.NullString
	if err := repository.db.QueryRow(`SELECT status, finished_at FROM analysis_refresh_jobs WHERE job_id='QQQ:45'`).Scan(&status, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if status != "running" || finishedAt.Valid {
		t.Fatalf("job changed despite event rollback: status=%q finished_at=%v", status, finishedAt)
	}
	var checkpointJSON string
	if err := repository.db.QueryRow(`SELECT result_json FROM go_analysis_refresh_checkpoints WHERE job_id='QQQ:45'`).Scan(&checkpointJSON); err != nil {
		t.Fatal(err)
	}
	if checkpointJSON == "" {
		t.Fatal("checkpoint was deleted despite transaction rollback")
	}
	var eventCount int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM push_events WHERE kind='gamma'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("gamma events=%d after rollback, want only the prior committed batch event", eventCount)
	}
}

func TestFinishAnalysisJobPrunesPushEventsToLatestTwentyThousand(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "analysis-event-prune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	tx, err := repository.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO push_events(symbol, kind, expiration, created_at) VALUES ('OLD', 'snapshot', NULL, '2000-01-01T00:00:00Z')`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if _, err := stmt.Exec(); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "", claimed["started_at"].(string))
	if err != nil || !finished {
		t.Fatalf("finish=%v err=%v", finished, err)
	}

	var count, minID, maxID int
	if err := repository.db.QueryRow(`SELECT COUNT(*), MIN(id), MAX(id) FROM push_events`).Scan(&count, &minID, &maxID); err != nil {
		t.Fatal(err)
	}
	if count != 20000 || minID != 2 || maxID != 20001 {
		t.Fatalf("push event retention count=%d min_id=%d max_id=%d", count, minID, maxID)
	}
	var gammaCount int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM push_events WHERE kind='gamma'`).Scan(&gammaCount); err != nil {
		t.Fatal(err)
	}
	if gammaCount != 1 {
		t.Fatalf("gamma event count=%d", gammaCount)
	}
}

func TestFinishAnalysisJobPrunesPushEventsByPythonIDWindow(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "analysis-event-id-window.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	if _, err := repository.db.Exec(`INSERT INTO push_events(symbol, kind, expiration, created_at)
VALUES ('OLD-1', 'snapshot', NULL, '2000-01-01T00:00:00Z'),
       ('OLD-2', 'snapshot', NULL, '2000-01-01T00:00:00Z'),
       ('OLD-3', 'snapshot', NULL, '2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Python retains the last 20,000 IDs, so a sequence gap can prune far fewer
	// rows than a count-based "latest 20,000 rows" policy.
	if _, err := repository.db.Exec(`UPDATE sqlite_sequence SET seq=25000 WHERE name='push_events'`); err != nil {
		t.Fatal(err)
	}
	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Minute, time.Minute)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}
	finished, err := repository.FinishAnalysisJob("QQQ:45", "completed", map[string]any{"has_more": false}, "", claimed["started_at"].(string))
	if err != nil || !finished {
		t.Fatalf("finish=%v err=%v", finished, err)
	}

	var count, minID, maxID int
	if err := repository.db.QueryRow(`SELECT COUNT(*), MIN(id), MAX(id) FROM push_events`).Scan(&count, &minID, &maxID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || minID != 25001 || maxID != 25001 {
		t.Fatalf("event ID-window retention count=%d min_id=%d max_id=%d, want only new event ID 25001", count, minID, maxID)
	}
}
