package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestChanStructureSnapshotIgnoresAppendedObjectsAndRecordsRevisions(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "chan-structure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	encode := func(value string) json.RawMessage { return json.RawMessage(value) }
	first := ChanStructureSnapshot{Symbol: "qqq", Timeframe: "1d", RuleVersion: "preview.1", InputFingerprint: "input-a", BarFingerprints: map[string]string{"2026-10-15": "bar-a"}, ConfirmedStructures: map[string]json.RawMessage{"segment|L0-C0-up-a-b": encode("{\"confirmed\":true,\"end\":\"b\"}")}, UpdatedAt: now}
	if revision, err := repository.RecordChanStructureSnapshot(first); err != nil || revision != nil {
		t.Fatalf("initial snapshot revision=%+v err=%v", revision, err)
	}
	if revision, err := repository.RecordChanStructureSnapshot(first); err != nil || revision != nil {
		t.Fatalf("identical snapshot revision=%+v err=%v, want nil", revision, err)
	}

	appended := first
	appended.InputFingerprint = "input-b"
	appended.BarFingerprints = map[string]string{"2026-10-15": "bar-a", "2026-10-16": "bar-b"}
	appended.ConfirmedStructures = map[string]json.RawMessage{
		"segment|L0-C0-up-a-b":   encode("{\"confirmed\":true,\"end\":\"b\"}"),
		"segment|L0-C0-down-b-c": encode("{\"confirmed\":true,\"end\":\"c\"}"),
	}
	appended.UpdatedAt = now.Add(time.Minute)
	if revision, err := repository.RecordChanStructureSnapshot(appended); err != nil || revision != nil {
		t.Fatalf("append-only snapshot revision=%+v err=%v, want nil", revision, err)
	}

	revised := appended
	revised.InputFingerprint = "input-c"
	revised.BarFingerprints = map[string]string{"2026-10-15": "bar-a-revised", "2026-10-16": "bar-b"}
	revised.ConfirmedStructures = map[string]json.RawMessage{
		"segment|L0-C0-up-a-b":   encode("{\"confirmed\":true,\"end\":\"b2\"}"),
		"segment|L0-C0-down-b-c": encode("{\"confirmed\":true,\"end\":\"c\"}"),
	}
	revised.UpdatedAt = now.Add(2 * time.Minute)
	revision, err := repository.RecordChanStructureSnapshot(revised)
	if err != nil || revision == nil {
		t.Fatalf("source revision=%+v err=%v, want a record", revision, err)
	}
	if revision.Reason != "source_data_revision" || len(revision.ChangedBars) != 1 || revision.ChangedBars[0].Date != "2026-10-15" || len(revision.ChangedConfirmedItems) != 1 || revision.ChangedConfirmedItems[0].ID != "segment|L0-C0-up-a-b" {
		t.Fatalf("source revision details=%+v", revision)
	}

	ruleChange := revised
	ruleChange.RuleVersion = "preview.2"
	ruleChange.ConfirmedStructures = map[string]json.RawMessage{}
	ruleChange.UpdatedAt = now.Add(3 * time.Minute)
	if _, err := repository.RecordChanStructureSnapshot(ruleChange); err != nil {
		t.Fatal(err)
	}
	revisions, err := repository.LatestChanStructureRevisions("QQQ", "1d", 10)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("latest revisions=%+v err=%v", revisions, err)
	}
	if revisions[0].Reason != "rule_version_changed" || revisions[0].PreviousRuleVersion != "preview.1" || revisions[0].RuleVersion != "preview.2" || len(revisions[0].ChangedConfirmedItems) != 2 {
		t.Fatalf("rule revision=%+v", revisions[0])
	}
	var snapshots int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM chan_structure_snapshots WHERE symbol='QQQ' AND timeframe='1d'`).Scan(&snapshots); err != nil || snapshots != 2 {
		t.Fatalf("versioned snapshots=%d err=%v, want one for each rule version", snapshots, err)
	}
	if revision, err := repository.RecordChanStructureSnapshot(ruleChange); err != nil || revision != nil {
		t.Fatalf("identical new-version snapshot revision=%+v err=%v, want nil", revision, err)
	}

	// Reusing the older rule version must compare against its own snapshot,
	// rather than treating the newer version as the previous state.
	olderVersionUpdate := revised
	olderVersionUpdate.InputFingerprint = "input-d"
	olderVersionUpdate.BarFingerprints = map[string]string{"2026-10-15": "bar-a-final", "2026-10-16": "bar-b"}
	olderVersionUpdate.ConfirmedStructures = map[string]json.RawMessage{
		"segment|L0-C0-up-a-b":   encode("{\"confirmed\":true,\"end\":\"b3\"}"),
		"segment|L0-C0-down-b-c": encode("{\"confirmed\":true,\"end\":\"c\"}"),
	}
	olderVersionUpdate.UpdatedAt = now.Add(4 * time.Minute)
	olderRevision, err := repository.RecordChanStructureSnapshot(olderVersionUpdate)
	if err != nil || olderRevision == nil {
		t.Fatalf("older-version source update revision=%+v err=%v, want a record", olderRevision, err)
	}
	if olderRevision.PreviousRuleVersion != "preview.1" || olderRevision.RuleVersion != "preview.1" || olderRevision.Reason != "source_data_revision" {
		t.Fatalf("older-version source revision=%+v, want an in-version source revision", olderRevision)
	}
}

func TestChanStructureSnapshotMigrationPreservesLegacySnapshotAndVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-chan-structure.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE chan_structure_snapshots (
 symbol TEXT NOT NULL, timeframe TEXT NOT NULL, rule_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL, bar_fingerprints_json TEXT NOT NULL, confirmed_structures_json TEXT NOT NULL,
 updated_at TEXT NOT NULL, PRIMARY KEY(symbol,timeframe));
INSERT INTO chan_structure_snapshots VALUES ('QQQ','1d','preview.1','input','{}','{}','2026-10-17T12:00:00Z');`)
	if err != nil {
		legacy.Close()
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
	var ruleVersion, input string
	if err := repository.db.QueryRow(`SELECT rule_version,input_fingerprint FROM chan_structure_snapshots WHERE symbol='QQQ' AND timeframe='1d'`).Scan(&ruleVersion, &input); err != nil || ruleVersion != "preview.1" || input != "input" {
		t.Fatalf("migrated snapshot version=%q input=%q err=%v", ruleVersion, input, err)
	}
	if _, err := repository.RecordChanStructureSnapshot(ChanStructureSnapshot{Symbol: "QQQ", Timeframe: "1d", RuleVersion: "preview.2", InputFingerprint: "input-v2"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM chan_structure_snapshots WHERE symbol='QQQ' AND timeframe='1d'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("migrated version count=%d err=%v, want 2", count, err)
	}
}

func TestChanStructureSnapshotIgnoresObjectsOutsideRollingInputWindow(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "chan-structure-window.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	first := ChanStructureSnapshot{Symbol: "QQQ", Timeframe: "1d", RuleVersion: "preview.1", InputFingerprint: "prefix-a", BarFingerprints: map[string]string{"2026-10-15": "a", "2026-10-16": "b"}, ConfirmedStructures: map[string]json.RawMessage{"segment|level=0|direction=up|start=2026-10-15|end=2026-10-15": json.RawMessage("{\"confirmed\":true}")}, UpdatedAt: now}
	if _, err := repository.RecordChanStructureSnapshot(first); err != nil {
		t.Fatal(err)
	}
	rolled := ChanStructureSnapshot{Symbol: "QQQ", Timeframe: "1d", RuleVersion: "preview.1", InputFingerprint: "prefix-b", BarFingerprints: map[string]string{"2026-10-16": "b", "2026-10-17": "c"}, ConfirmedStructures: map[string]json.RawMessage{"segment|level=0|direction=down|start=2026-10-16|end=2026-10-16": json.RawMessage("{\"confirmed\":true}")}, UpdatedAt: now.Add(time.Minute)}
	if revision, err := repository.RecordChanStructureSnapshot(rolled); err != nil || revision != nil {
		t.Fatalf("rolling-window append revision=%+v err=%v, want nil", revision, err)
	}
}

func TestChanStructureSnapshotLogsUnexpectedConfirmedChangeOnAppend(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "chan-structure-append-change.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	id := "segment|level=0|direction=up|start=2026-10-15|end=2026-10-16"
	first := ChanStructureSnapshot{Symbol: "QQQ", Timeframe: "1d", RuleVersion: "preview.1", InputFingerprint: "prefix-a", BarFingerprints: map[string]string{"2026-10-15": "a", "2026-10-16": "b"}, ConfirmedStructures: map[string]json.RawMessage{id: json.RawMessage("{\"end_price\":100}")}, UpdatedAt: now}
	if _, err := repository.RecordChanStructureSnapshot(first); err != nil {
		t.Fatal(err)
	}
	appended := first
	appended.InputFingerprint = "prefix-b"
	appended.BarFingerprints = map[string]string{"2026-10-15": "a", "2026-10-16": "b", "2026-10-17": "c"}
	appended.ConfirmedStructures = map[string]json.RawMessage{id: json.RawMessage("{\"end_price\":99}")}
	appended.UpdatedAt = now.Add(time.Minute)
	revision, err := repository.RecordChanStructureSnapshot(appended)
	if err != nil || revision == nil || revision.Reason != "confirmed_structure_changed_after_append" || len(revision.ChangedBars) != 0 || len(revision.ChangedConfirmedItems) != 1 {
		t.Fatalf("unexpected append revision=%+v err=%v", revision, err)
	}
}
