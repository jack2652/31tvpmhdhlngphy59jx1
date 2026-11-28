package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ChanStructureSnapshot is a deterministic view of the source bars and
// confirmed recursive structures used by one Chan analysis frame.
type ChanStructureSnapshot struct {
	Symbol              string
	Timeframe           string
	RuleVersion         string
	InputFingerprint    string
	BarFingerprints     map[string]string
	ConfirmedStructures map[string]json.RawMessage
	UpdatedAt           time.Time
}

type ChanStructureChange struct {
	ID     string          `json:"id"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

type chanStructureRevisionHeader struct {
	ID                  int64     `json:"id"`
	PreviousRuleVersion string    `json:"previous_rule_version"`
	RuleVersion         string    `json:"rule_version"`
	Reason              string    `json:"reason"`
	InputFingerprint    string    `json:"input_fingerprint"`
	CreatedAt           time.Time `json:"created_at"`
}

type ChanStructureRevision struct {
	chanStructureRevisionHeader
	Symbol                string                `json:"symbol"`
	Timeframe             string                `json:"timeframe"`
	ChangedBars           []ChanBarChange       `json:"changed_bars"`
	ChangedConfirmedItems []ChanStructureChange `json:"changed_confirmed_structures"`
}

// ChanStructureRevisionPayload 保留已存储的差异数组，供 API 直接序列化。
type ChanStructureRevisionPayload struct {
	chanStructureRevisionHeader
	ChangedBars           json.RawMessage `json:"changed_bars"`
	ChangedConfirmedItems json.RawMessage `json:"changed_confirmed_structures"`
}

type ChanBarChange struct {
	Date string `json:"date"`
	Old  string `json:"old_fingerprint,omitempty"`
	New  string `json:"new_fingerprint,omitempty"`
}

type chanStoredSnapshot struct {
	InputFingerprint    string                     `json:"input_fingerprint"`
	BarFingerprints     map[string]string          `json:"bar_fingerprints"`
	ConfirmedStructures map[string]json.RawMessage `json:"confirmed_structures"`
}

// RecordChanStructureSnapshot atomically stores the latest snapshot and, when
// a previously confirmed object changes or disappears, an auditable revision.
// Newly confirmed objects are expected as bars are appended and are not treated
// as revisions.
func (s *SQLite) RecordChanStructureSnapshot(snapshot ChanStructureSnapshot) (*ChanStructureRevision, error) {
	snapshot.Symbol = strings.ToUpper(strings.TrimSpace(snapshot.Symbol))
	snapshot.Timeframe = strings.TrimSpace(snapshot.Timeframe)
	snapshot.RuleVersion = strings.TrimSpace(snapshot.RuleVersion)
	if snapshot.Symbol == "" || snapshot.Timeframe == "" || snapshot.RuleVersion == "" || snapshot.InputFingerprint == "" {
		return nil, errors.New("Chan structure snapshot identity must not be empty")
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now().UTC()
	}
	if snapshot.BarFingerprints == nil {
		snapshot.BarFingerprints = map[string]string{}
	}
	if snapshot.ConfirmedStructures == nil {
		snapshot.ConfirmedStructures = map[string]json.RawMessage{}
	}
	barJSON, err := json.Marshal(snapshot.BarFingerprints)
	if err != nil {
		return nil, fmt.Errorf("encode Chan bar fingerprints: %w", err)
	}
	structureJSON, err := json.Marshal(snapshot.ConfirmedStructures)
	if err != nil {
		return nil, fmt.Errorf("encode confirmed Chan structures: %w", err)
	}

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var previousRule, previousInput, previousBarsJSON, previousStructuresJSON string
	err = tx.QueryRow(`SELECT rule_version,input_fingerprint,bar_fingerprints_json,confirmed_structures_json
FROM chan_structure_snapshots WHERE symbol=? AND timeframe=?
ORDER BY CASE WHEN rule_version=? THEN 0 ELSE 1 END, updated_at DESC LIMIT 1`, snapshot.Symbol, snapshot.Timeframe, snapshot.RuleVersion).
		Scan(&previousRule, &previousInput, &previousBarsJSON, &previousStructuresJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var revision *ChanStructureRevision
	if err == nil {
		// 相同快照直接复用，避免对未变化的历史重新解码。
		if previousRule == snapshot.RuleVersion && previousInput == snapshot.InputFingerprint && previousBarsJSON == string(barJSON) && previousStructuresJSON == string(structureJSON) {
			return nil, nil
		}
		previous := chanStoredSnapshot{BarFingerprints: map[string]string{}, ConfirmedStructures: map[string]json.RawMessage{}}
		if decodeErr := json.Unmarshal([]byte(previousBarsJSON), &previous.BarFingerprints); decodeErr != nil {
			return nil, fmt.Errorf("decode previous Chan bar fingerprints: %w", decodeErr)
		}
		if decodeErr := json.Unmarshal([]byte(previousStructuresJSON), &previous.ConfirmedStructures); decodeErr != nil {
			return nil, fmt.Errorf("decode previous confirmed Chan structures: %w", decodeErr)
		}
		previous.InputFingerprint = previousInput
		changedBars := compareChanBars(previous.BarFingerprints, snapshot.BarFingerprints, earliestChanBarDate(snapshot.BarFingerprints))
		includeNewStructures := previousRule != snapshot.RuleVersion || len(changedBars) > 0
		changedStructures := compareChanStructures(previous.ConfirmedStructures, snapshot.ConfirmedStructures, includeNewStructures, earliestChanBarDate(snapshot.BarFingerprints))
		if len(changedStructures) > 0 {
			reason := chanRevisionReason(previousRule, snapshot.RuleVersion, changedBars, previousInput != snapshot.InputFingerprint)
			revision = &ChanStructureRevision{
				chanStructureRevisionHeader: chanStructureRevisionHeader{
					PreviousRuleVersion: previousRule, RuleVersion: snapshot.RuleVersion, Reason: reason, InputFingerprint: snapshot.InputFingerprint, CreatedAt: snapshot.UpdatedAt.UTC(),
				},
				Symbol: snapshot.Symbol, Timeframe: snapshot.Timeframe, ChangedBars: changedBars, ChangedConfirmedItems: changedStructures,
			}
			changedBarsJSON, marshalErr := json.Marshal(changedBars)
			if marshalErr != nil {
				return nil, marshalErr
			}
			changedStructuresJSON, marshalErr := json.Marshal(changedStructures)
			if marshalErr != nil {
				return nil, marshalErr
			}
			result, execErr := tx.Exec(`INSERT INTO chan_structure_revisions(symbol,timeframe,previous_rule_version,rule_version,reason,input_fingerprint,changed_bars_json,changed_structures_json,created_at)
VALUES (?,?,?,?,?,?,?,?,?)`, snapshot.Symbol, snapshot.Timeframe, previousRule, snapshot.RuleVersion, reason, snapshot.InputFingerprint, string(changedBarsJSON), string(changedStructuresJSON), storedTimestamp(snapshot.UpdatedAt))
			if execErr != nil {
				return nil, execErr
			}
			revision.ID, _ = result.LastInsertId()
			if _, execErr = tx.Exec(`DELETE FROM chan_structure_revisions WHERE symbol=? AND timeframe=? AND id NOT IN (SELECT id FROM chan_structure_revisions WHERE symbol=? AND timeframe=? ORDER BY id DESC LIMIT 100)`, snapshot.Symbol, snapshot.Timeframe, snapshot.Symbol, snapshot.Timeframe); execErr != nil {
				return nil, execErr
			}
		}
	}
	_, err = tx.Exec(`INSERT INTO chan_structure_snapshots(symbol,timeframe,rule_version,input_fingerprint,bar_fingerprints_json,confirmed_structures_json,updated_at)
VALUES (?,?,?,?,?,?,?) ON CONFLICT(symbol,timeframe,rule_version) DO UPDATE SET input_fingerprint=excluded.input_fingerprint,bar_fingerprints_json=excluded.bar_fingerprints_json,confirmed_structures_json=excluded.confirmed_structures_json,updated_at=excluded.updated_at`,
		snapshot.Symbol, snapshot.Timeframe, snapshot.RuleVersion, snapshot.InputFingerprint, string(barJSON), string(structureJSON), storedTimestamp(snapshot.UpdatedAt))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return revision, nil
}

func (s *SQLite) LatestChanStructureRevisions(symbol, timeframe string, limit int) ([]ChanStructureRevisionPayload, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,previous_rule_version,rule_version,reason,input_fingerprint,changed_bars_json,changed_structures_json,created_at
FROM chan_structure_revisions WHERE symbol=? AND timeframe=? ORDER BY id DESC LIMIT ?`, strings.ToUpper(strings.TrimSpace(symbol)), strings.TrimSpace(timeframe), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []ChanStructureRevisionPayload
	for rows.Next() {
		var item ChanStructureRevisionPayload
		var barsJSON, structuresJSON []byte
		var createdAt string
		if err := rows.Scan(&item.ID, &item.PreviousRuleVersion, &item.RuleVersion, &item.Reason, &item.InputFingerprint, &barsJSON, &structuresJSON, &createdAt); err != nil {
			return nil, err
		}
		item.ChangedBars = barsJSON
		item.ChangedConfirmedItems = structuresJSON
		if !validChanChangeArray(item.ChangedBars) || !validChanChangeArray(item.ChangedConfirmedItems) {
			return nil, fmt.Errorf("invalid Chan revision changes for id %d", item.ID)
		}
		// 完整 API 将空差异数组表示为 null，并省略 before/after 空值。
		if bytes.Equal(item.ChangedBars, []byte("[]")) {
			item.ChangedBars = nil
		}
		if bytes.Equal(item.ChangedConfirmedItems, []byte("[]")) {
			item.ChangedConfirmedItems = nil
		} else if bytes.Contains(item.ChangedConfirmedItems, []byte(`"before":null`)) ||
			bytes.Contains(item.ChangedConfirmedItems, []byte(`"after":null`)) {
			var changes []ChanStructureChange
			if err := json.Unmarshal(item.ChangedConfirmedItems, &changes); err != nil {
				return nil, err
			}
			for index := range changes {
				if bytes.Equal(changes[index].Before, []byte("null")) {
					changes[index].Before = nil
				}
				if bytes.Equal(changes[index].After, []byte("null")) {
					changes[index].After = nil
				}
			}
			item.ChangedConfirmedItems, err = json.Marshal(changes)
			if err != nil {
				return nil, err
			}
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		revisions = append(revisions, item)
	}
	return revisions, rows.Err()
}

func validChanChangeArray(value json.RawMessage) bool {
	return bytes.Equal(value, []byte("null")) || (len(value) > 0 && value[0] == '[' && json.Valid(value))
}

func compareChanBars(previous, current map[string]string, earliestCurrentDate string) []ChanBarChange {
	dates := map[string]struct{}{}
	for date := range previous {
		dates[date] = struct{}{}
	}
	for date := range current {
		dates[date] = struct{}{}
	}
	ordered := make([]string, 0, len(dates))
	for date := range dates {
		ordered = append(ordered, date)
	}
	sort.Strings(ordered)
	changes := []ChanBarChange{}
	for _, date := range ordered {
		oldValue, oldFound := previous[date]
		newValue, newFound := current[date]
		if oldFound && newFound && oldValue != newValue || oldFound && !newFound && (earliestCurrentDate == "" || date >= earliestCurrentDate) {
			changes = append(changes, ChanBarChange{Date: date, Old: oldValue, New: newValue})
		}
	}
	return changes
}

func compareChanStructures(previous, current map[string]json.RawMessage, includeNew bool, currentEarliestDate string) []ChanStructureChange {
	ids := map[string]struct{}{}
	for id := range previous {
		ids[id] = struct{}{}
	}
	for id := range current {
		ids[id] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	changes := []ChanStructureChange{}
	for _, id := range ordered {
		before, beforeFound := previous[id]
		after, afterFound := current[id]
		if !beforeFound && !includeNew {
			continue
		}
		if beforeFound && !afterFound && currentEarliestDate != "" && structureIDEndDate(id) != "" && structureIDEndDate(id) < currentEarliestDate {
			continue
		}
		if beforeFound != afterFound || string(before) != string(after) {
			changes = append(changes, ChanStructureChange{ID: id, Before: before, After: after})
		}
	}
	return changes
}

func earliestChanBarDate(fingerprints map[string]string) string {
	earliest := ""
	for date := range fingerprints {
		if earliest == "" || date < earliest {
			earliest = date
		}
	}
	return earliest
}

func structureIDEndDate(id string) string {
	for _, part := range strings.Split(id, "|") {
		if strings.HasPrefix(part, "end=") {
			return strings.TrimPrefix(part, "end=")
		}
	}
	return ""
}

func chanRevisionReason(previousRule, currentRule string, changedBars []ChanBarChange, inputChanged bool) string {
	// Source bar revisions explain the change even when the rule version also
	// moved; the rule label alone would hide the data correction.
	for _, bar := range changedBars {
		if bar.Old != "" && bar.New != "" {
			return "source_data_revision"
		}
		if bar.Old != "" && bar.New == "" {
			return "source_history_revision"
		}
	}
	if previousRule != currentRule {
		return "rule_version_changed"
	}
	if inputChanged {
		return "confirmed_structure_changed_after_append"
	}
	return "confirmed_structure_changed_without_input_change"
}
