package server

import (
	"path/filepath"
	"testing"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/store"
)

func TestGammaStatusMapsGoOrphanMessageToPythonContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamma-orphan-status.db")
	repository, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	claimed, owner, err := repository.ClaimAnalysisJob("QQQ", 45, time.Second, time.Minute)
	if err != nil || !owner {
		t.Fatalf("claim=%#v owner=%v err=%v", claimed, owner, err)
	}

	interrupted, err := repository.FailOrphanedAnalysisJobs(0)
	if err != nil || interrupted != 1 {
		t.Fatalf("orphan recovery count=%d err=%v, want 1", interrupted, err)
	}

	service := NewWithProvider(config.Config{DatabasePath: path}, repository, nil)
	defer service.Close()
	status := service.gammaJobStatus("QQQ", 45)
	if status["status"] != "failed" || status["error_message"] != pythonOrphanedGammaMessage {
		t.Fatalf("orphan status=%#v, want Python-compatible restart failure", status)
	}
	if status["job_id"] != "QQQ:45" || status["symbol"] != "QQQ" || status["horizon_days"] != 45 {
		t.Fatalf("orphan status metadata=%#v", status)
	}
}

func TestGammaStatusPreservesNonOrphanFailureMessage(t *testing.T) {
	status := gammaAnalysisJobPayload(map[string]any{
		"job_id": "QQQ:45", "symbol": "QQQ", "horizon_days": 45,
		"status": "failed", "error_message": "provider unavailable",
	})
	if status["error_message"] != "provider unavailable" {
		t.Fatalf("provider failure message=%#v, want unchanged", status["error_message"])
	}
}
