package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	"option-scope-go/internal/model"
)

func init() {
	// Apply a default wait window to every modernc connection, including
	// callers that open the shared database directly without our DSN helper.
	// Individual bounded operations may temporarily override this pragma.
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=30000", nil)
		return err
	})
}

// SQLite is the durable repository shared with the existing Python deployment.
// The schema is intentionally compatible with the quote/option/event tables that
// already exist in data/options.db.
type SQLite struct {
	db        *sql.DB
	path      string
	lowMemory bool
}

func Open(path string) (*SQLite, error) {
	return OpenWithOptions(path, false)
}

// OpenWithOptions configures SQLite to follow the runtime's low-memory
// safeguards. The single connection keeps connection-local PRAGMAs consistent
// across the pooled database/sql API.
func OpenWithOptions(path string, lowMemory bool) (*SQLite, error) {
	if path == "" {
		path = filepath.Join(runtimeDirectory(), "data", "options.db")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	if lowMemory {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	}
	store := &SQLite{db: db, path: path, lowMemory: lowMemory}
	if lowMemory {
		for _, pragma := range []string{"PRAGMA cache_size=-256", "PRAGMA mmap_size=0", "PRAGMA temp_store=FILE", "PRAGMA wal_autocheckpoint=200"} {
			if _, err := db.Exec(pragma); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func runtimeDirectory() string {
	executable, err := os.Executable()
	if err == nil && executable != "" {
		return filepath.Dir(executable)
	}
	workingDirectory, err := os.Getwd()
	if err == nil {
		return workingDirectory
	}
	return "."
}

func sqliteDSN(path string) string {
	// The driver treats the first '?' as the start of DSN options. Encode the
	// filename as a file URI so legal path characters such as '?', '#', and '%'
	// remain part of the database path instead of being parsed as options.
	uriPath := filepath.ToSlash(path)
	if volume := filepath.VolumeName(path); volume != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath}).String()
	options := url.Values{
		"_pragma": {"busy_timeout(30000)", "foreign_keys(ON)", "journal_mode(WAL)"},
	}
	return uri + "?" + options.Encode()
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) init() error {
	// Pin initialization to one connection and acquire SQLite's cross-process
	// write reservation before inspecting or changing any schema. The connection
	// DSN sets busy_timeout, so competing process starts wait here instead of
	// racing the PRAGMA/ALTER gap.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := s.initSchema(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLite) initSchema(conn *sql.Conn) error {
	_, err := conn.ExecContext(context.Background(), `
CREATE TABLE IF NOT EXISTS quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, currency TEXT, market_state TEXT, provider TEXT NOT NULL,
 raw_json TEXT, sessions_json TEXT, today_open REAL, previous_close REAL,
 fair_value REAL, fair_value_low REAL, fair_value_high REAL, fair_value_buy_low REAL, fair_value_buy_high REAL,
 fair_value_source TEXT, fair_value_model TEXT, fair_value_methods_json TEXT, fair_value_forward_eps REAL, fair_value_forward_eps_source TEXT,
 fair_value_safety_margin REAL, fair_value_confidence TEXT, fair_value_confidence_score REAL,
 fair_value_interest_coverage REAL, fair_value_regime TEXT, fair_value_regime_signals_json TEXT,
 fair_value_model_under_regime TEXT, fair_value_defensive_json TEXT, fair_value_optimistic_json TEXT,
 fair_value_normalized_eps_source TEXT, fair_value_quarterly_momentum_json TEXT,
 fair_value_historical_valuation_percentiles_json TEXT, fair_value_shareholder_total_return_yield REAL,
 fair_value_market_cap_data_quality TEXT, fair_value_data_quality_score REAL,
 fair_value_owner_earnings_maintenance_ratio REAL, fair_value_owner_earnings_ratio_source TEXT
);
CREATE INDEX IF NOT EXISTS idx_quote_symbol_time ON quote_snapshots(symbol, fetched_at DESC);
CREATE TABLE IF NOT EXISTS option_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, expiration TEXT NOT NULL,
 fetched_at TEXT NOT NULL, contract_symbol TEXT NOT NULL, contract_type TEXT NOT NULL,
 strike REAL, last_price REAL, bid REAL, ask REAL, volume INTEGER, open_interest INTEGER,
 implied_volatility REAL, gamma REAL, in_the_money INTEGER, change_percent REAL,
 provider TEXT NOT NULL, raw_json TEXT,
 UNIQUE(symbol, expiration, fetched_at, contract_symbol)
);
CREATE INDEX IF NOT EXISTS idx_option_lookup ON option_snapshots(symbol, expiration, fetched_at DESC);
CREATE INDEX IF NOT EXISTS idx_option_symbol_time ON option_snapshots(symbol, fetched_at DESC);
CREATE INDEX IF NOT EXISTS idx_option_oi_fallback ON option_snapshots(symbol, expiration, contract_symbol, fetched_at DESC) WHERE open_interest > 0;
CREATE TABLE IF NOT EXISTS option_latest_batches (
 symbol TEXT NOT NULL, expiration TEXT NOT NULL, fetched_at TEXT NOT NULL,
 PRIMARY KEY(symbol, expiration)
);
CREATE INDEX IF NOT EXISTS idx_option_latest_expiration ON option_latest_batches(symbol, expiration);
CREATE TABLE IF NOT EXISTS expiration_catalog (
 symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS expiration_probe_status (
 symbol TEXT PRIMARY KEY, empty INTEGER NOT NULL, fetched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS push_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, kind TEXT NOT NULL,
 expiration TEXT, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_push_events_id ON push_events(id);
CREATE TABLE IF NOT EXISTS refresh_runs (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, started_at TEXT NOT NULL,
 finished_at TEXT, status TEXT NOT NULL, rows_written INTEGER NOT NULL DEFAULT 0,
 error_message TEXT
);
CREATE INDEX IF NOT EXISTS idx_refresh_symbol_time ON refresh_runs(symbol, started_at DESC);
CREATE TABLE IF NOT EXISTS price_history (
 symbol TEXT NOT NULL, bar_date TEXT NOT NULL, open REAL, high REAL, low REAL, close REAL,
 volume REAL, fetched_at TEXT NOT NULL, PRIMARY KEY(symbol, bar_date)
);
CREATE TABLE IF NOT EXISTS chan_adjusted_history (
 symbol TEXT NOT NULL, timeframe TEXT NOT NULL, adjustment_type TEXT NOT NULL, provider TEXT NOT NULL,
 bar_date TEXT NOT NULL, open REAL NOT NULL, high REAL NOT NULL, low REAL NOT NULL, close REAL NOT NULL, volume REAL NOT NULL,
 raw_open REAL NOT NULL, raw_high REAL NOT NULL, raw_low REAL NOT NULL, raw_close REAL NOT NULL, raw_volume REAL NOT NULL,
 adjustment_factor REAL NOT NULL, fetched_at TEXT NOT NULL,
 PRIMARY KEY(symbol, timeframe, adjustment_type, provider, bar_date)
);
CREATE INDEX IF NOT EXISTS idx_chan_adjusted_history_time ON chan_adjusted_history(symbol, timeframe, adjustment_type, provider, bar_date DESC);
CREATE TABLE IF NOT EXISTS chan_structure_snapshots (
 symbol TEXT NOT NULL, timeframe TEXT NOT NULL, rule_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL, bar_fingerprints_json TEXT NOT NULL, confirmed_structures_json TEXT NOT NULL,
 updated_at TEXT NOT NULL, PRIMARY KEY(symbol, timeframe, rule_version)
);
CREATE TABLE IF NOT EXISTS chan_structure_revisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, timeframe TEXT NOT NULL,
 previous_rule_version TEXT NOT NULL, rule_version TEXT NOT NULL, reason TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL, changed_bars_json TEXT NOT NULL, changed_structures_json TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chan_structure_revisions_lookup ON chan_structure_revisions(symbol, timeframe, id DESC);
CREATE TABLE IF NOT EXISTS intraday_history (
 symbol TEXT NOT NULL, interval TEXT NOT NULL, bar_time TEXT NOT NULL, open REAL, high REAL, low REAL, close REAL,
 volume REAL, fetched_at TEXT NOT NULL, PRIMARY KEY(symbol, interval, bar_time)
);
CREATE INDEX IF NOT EXISTS idx_intraday_history_time ON intraday_history(symbol, interval, bar_time DESC);
CREATE TABLE IF NOT EXISTS price_extremes (
 symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS beta_snapshots (
 symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS earnings_snapshots (
 symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS analysis_refresh_jobs (
 job_id TEXT PRIMARY KEY, symbol TEXT NOT NULL, horizon_days INTEGER NOT NULL,
 status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT,
 result_json TEXT, error_message TEXT
);
CREATE INDEX IF NOT EXISTS idx_analysis_jobs_status ON analysis_refresh_jobs(status, started_at DESC);
CREATE TABLE IF NOT EXISTS go_analysis_refresh_checkpoints (
 job_id TEXT PRIMARY KEY, result_json TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_analysis_cache (
 cache_key TEXT PRIMARY KEY, payload TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_analysis_cache_time ON api_analysis_cache(created_at ASC);
CREATE TABLE IF NOT EXISTS chan_analysis_cache (
 cache_key TEXT PRIMARY KEY, payload TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chan_analysis_cache_time ON chan_analysis_cache(created_at ASC);
CREATE TABLE IF NOT EXISTS service_leases (
 name TEXT PRIMARY KEY, owner TEXT NOT NULL, acquired_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS push_refresh_state (
 symbol TEXT NOT NULL,
 expiration TEXT NOT NULL DEFAULT '',
 started_at TEXT NOT NULL,
 retry_after TEXT,
 retry_delay_seconds INTEGER NOT NULL DEFAULT 0,
 last_interval_seconds INTEGER NOT NULL DEFAULT 0,
 lease_until TEXT,
 PRIMARY KEY(symbol, expiration)
);
`)
	if err != nil {
		return err
	}
	if err := ensureCompatibilityColumns(conn); err != nil {
		return err
	}
	if err := ensureVersionedChanStructureSnapshots(conn); err != nil {
		return err
	}
	// Older Python/Go writers can leave a subset of option_snapshots without a
	// corresponding latest-batch marker. Repair each symbol/expiration pair,
	// not only databases where the marker table is completely empty. This runs
	// under the startup migration transaction, so readers never see a partial
	// repair. Never move an existing marker backwards.
	if _, err := conn.ExecContext(context.Background(), `INSERT OR IGNORE INTO option_latest_batches(symbol, expiration, fetched_at)
SELECT symbol, expiration, MAX(fetched_at) FROM option_snapshots GROUP BY symbol, expiration`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE option_latest_batches
SET fetched_at=(SELECT MAX(snapshot.fetched_at) FROM option_snapshots snapshot
                WHERE snapshot.symbol=option_latest_batches.symbol
                  AND snapshot.expiration=option_latest_batches.expiration)
WHERE EXISTS (SELECT 1 FROM option_snapshots snapshot
              WHERE snapshot.symbol=option_latest_batches.symbol
                AND snapshot.expiration=option_latest_batches.expiration
                AND (snapshot.fetched_at > option_latest_batches.fetched_at
                     OR NOT EXISTS (SELECT 1 FROM option_snapshots current
                                    WHERE current.symbol=option_latest_batches.symbol
                                      AND current.expiration=option_latest_batches.expiration
                                      AND current.fetched_at=option_latest_batches.fetched_at)))`); err != nil {
		return err
	}
	return nil
}

// ensureVersionedChanStructureSnapshots preserves one snapshot per algorithm
// rule version. Existing databases have a (symbol, timeframe) key, so rebuild
// the table inside init's migration transaction before any readers start.
func ensureVersionedChanStructureSnapshots(conn *sql.Conn) error {
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info("chan_structure_snapshots")`)
	if err != nil {
		return err
	}
	primaryKey := map[string]int{}
	for rows.Next() {
		var cid, notNull, keyOrder int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &keyOrder); err != nil {
			rows.Close()
			return err
		}
		if keyOrder > 0 {
			primaryKey[name] = keyOrder
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(primaryKey) == 3 && primaryKey["symbol"] == 1 && primaryKey["timeframe"] == 2 && primaryKey["rule_version"] == 3 {
		return nil
	}
	if _, err := conn.ExecContext(context.Background(), `
CREATE TABLE chan_structure_snapshots_versioned (
 symbol TEXT NOT NULL, timeframe TEXT NOT NULL, rule_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL, bar_fingerprints_json TEXT NOT NULL, confirmed_structures_json TEXT NOT NULL,
 updated_at TEXT NOT NULL, PRIMARY KEY(symbol, timeframe, rule_version)
);
INSERT INTO chan_structure_snapshots_versioned(symbol,timeframe,rule_version,input_fingerprint,bar_fingerprints_json,confirmed_structures_json,updated_at)
 SELECT symbol,timeframe,rule_version,input_fingerprint,bar_fingerprints_json,confirmed_structures_json,updated_at FROM chan_structure_snapshots;
DROP TABLE chan_structure_snapshots;
ALTER TABLE chan_structure_snapshots_versioned RENAME TO chan_structure_snapshots;`); err != nil {
		return fmt.Errorf("migrate versioned Chan structure snapshots: %w", err)
	}
	return nil
}

func ensureCompatibilityColumns(conn *sql.Conn) error {
	columns := []struct {
		table, name, definition string
	}{
		{"quote_snapshots", "raw_json", "TEXT"},
		// Older Python databases may lack provider, while the current readers and
		// writers treat it as required. SQLite cannot add a NOT NULL column to a
		// populated table without a default, so backfill a neutral legacy marker.
		{"quote_snapshots", "provider", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"quote_snapshots", "sessions_json", "TEXT"},
		{"quote_snapshots", "today_open", "REAL"},
		{"quote_snapshots", "previous_close", "REAL"},
		{"quote_snapshots", "fair_value", "REAL"},
		{"quote_snapshots", "fair_value_low", "REAL"},
		{"quote_snapshots", "fair_value_high", "REAL"},
		{"quote_snapshots", "fair_value_buy_low", "REAL"},
		{"quote_snapshots", "fair_value_buy_high", "REAL"},
		{"quote_snapshots", "fair_value_source", "TEXT"},
		{"quote_snapshots", "fair_value_model", "TEXT"},
		{"quote_snapshots", "fair_value_methods_json", "TEXT"},
		{"quote_snapshots", "fair_value_forward_eps", "REAL"},
		{"quote_snapshots", "fair_value_forward_eps_source", "TEXT"},
		{"quote_snapshots", "fair_value_safety_margin", "REAL"},
		{"quote_snapshots", "fair_value_confidence", "TEXT"},
		{"quote_snapshots", "fair_value_confidence_score", "REAL"},
		{"quote_snapshots", "fair_value_interest_coverage", "REAL"},
		{"quote_snapshots", "fair_value_regime", "TEXT"},
		{"quote_snapshots", "fair_value_regime_signals_json", "TEXT"},
		{"quote_snapshots", "fair_value_model_under_regime", "TEXT"},
		{"quote_snapshots", "fair_value_defensive_json", "TEXT"},
		{"quote_snapshots", "fair_value_optimistic_json", "TEXT"},
		{"quote_snapshots", "fair_value_normalized_eps_source", "TEXT"},
		{"quote_snapshots", "fair_value_quarterly_momentum_json", "TEXT"},
		{"quote_snapshots", "fair_value_historical_valuation_percentiles_json", "TEXT"},
		{"quote_snapshots", "fair_value_shareholder_total_return_yield", "REAL"},
		{"quote_snapshots", "fair_value_market_cap_data_quality", "TEXT"},
		{"quote_snapshots", "fair_value_data_quality_score", "REAL"},
		{"quote_snapshots", "fair_value_owner_earnings_maintenance_ratio", "REAL"},
		{"quote_snapshots", "fair_value_owner_earnings_ratio_source", "TEXT"},
		{"option_snapshots", "gamma", "REAL"},
		{"option_snapshots", "in_the_money", "INTEGER"},
		{"option_snapshots", "change_percent", "REAL"},
		{"option_snapshots", "provider", "TEXT"},
		{"push_events", "expiration", "TEXT"},
		{"push_refresh_state", "retry_after", "TEXT"},
		{"push_refresh_state", "retry_delay_seconds", "INTEGER NOT NULL DEFAULT 0"},
		{"push_refresh_state", "last_interval_seconds", "INTEGER NOT NULL DEFAULT 0"},
		{"push_refresh_state", "lease_until", "TEXT"},
	}
	for _, column := range columns {
		if err := ensureColumn(conn, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func ensureColumn(conn *sql.Conn, table, column, definition string) error {
	// Identifiers cannot be passed as SQL parameters. Quote them as SQLite
	// identifiers (including escaping embedded quotes) before using PRAGMA or
	// ALTER; table/column/definition values are otherwise migration constants.
	quotedTable := quoteSQLiteIdentifier(table)
	quotedColumn := quoteSQLiteIdentifier(column)
	rows, err := conn.QueryContext(context.Background(), "PRAGMA table_info("+quotedTable+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		// SQLite column names are case-insensitive, including legacy schemas
		// created with differently-cased identifiers.
		if strings.EqualFold(name, column) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = conn.ExecContext(context.Background(), "ALTER TABLE "+quotedTable+" ADD COLUMN "+quotedColumn+" "+definition)
	return err
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

const maxPushRefreshBackoff = 5 * time.Minute
const pushRefreshLease = 180 * time.Second

// ClaimPushRefresh atomically claims the shared subscription refresh slot for a
// symbol/expiration. It follows Python's push refresh cadence, cooldown, and
// in-flight lease semantics so independent Service instances coordinate.
func (s *SQLite) ClaimPushRefresh(symbol, expiration string, interval time.Duration) (bool, error) {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	if normalized == "" {
		return false, nil
	}
	intervalSeconds := int(interval / time.Second)
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}
	interval = time.Duration(intervalSeconds) * time.Second
	now := time.Now().UTC()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var startedAt, retryAfter, leaseUntil sql.NullString
	var retryDelay, lastInterval int
	queryErr := conn.QueryRowContext(context.Background(), `SELECT started_at, retry_after, retry_delay_seconds, last_interval_seconds, lease_until
FROM push_refresh_state WHERE symbol=? AND expiration=?`, normalized, expiration).Scan(&startedAt, &retryAfter, &retryDelay, &lastInterval, &leaseUntil)
	if queryErr != nil && !errors.Is(queryErr, sql.ErrNoRows) {
		return false, queryErr
	}
	if queryErr == nil {
		if at, ok := parsePushRefreshTime(leaseUntil); ok && now.Before(at) {
			if err := commitImmediate(conn, &committed); err != nil {
				return false, err
			}
			return false, nil
		}
		if at, ok := parsePushRefreshTime(retryAfter); ok && now.Before(at) {
			if err := commitImmediate(conn, &committed); err != nil {
				return false, err
			}
			return false, nil
		}
		previousInterval := time.Duration(lastInterval) * time.Second
		if at, ok := parsePushRefreshTime(startedAt); ok && previousInterval == interval {
			cadenceWindow := interval - time.Second
			if cadenceWindow < 0 {
				cadenceWindow = 0
			}
			if now.Sub(at) < cadenceWindow {
				if err := commitImmediate(conn, &committed); err != nil {
					return false, err
				}
				return false, nil
			}
		}
	}
	leaseAt := storedTimestamp(now.Add(pushRefreshLease))
	_, err = conn.ExecContext(context.Background(), `INSERT INTO push_refresh_state(symbol, expiration, started_at, retry_after, retry_delay_seconds, last_interval_seconds, lease_until)
VALUES (?, ?, ?, NULL, 0, ?, ?)
ON CONFLICT(symbol, expiration) DO UPDATE SET started_at=excluded.started_at,
 retry_after=NULL, last_interval_seconds=excluded.last_interval_seconds, lease_until=excluded.lease_until`,
		normalized, expiration, storedTimestamp(now), int(interval/time.Second), leaseAt)
	if err != nil {
		return false, err
	}
	if err := commitImmediate(conn, &committed); err != nil {
		return false, err
	}
	return true, nil
}

// PushRefreshWait returns the durable delay before retrying a refresh that was
// claimed by another worker or is still inside its cadence/cooldown window.
func (s *SQLite) PushRefreshWait(symbol, expiration string, interval time.Duration) (time.Duration, error) {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	intervalSeconds := int(interval / time.Second)
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}
	interval = time.Duration(intervalSeconds) * time.Second
	var startedAt, retryAfter, leaseUntil sql.NullString
	var lastInterval int
	err := s.db.QueryRow(`SELECT started_at, retry_after, last_interval_seconds, lease_until FROM push_refresh_state WHERE symbol=? AND expiration=?`, normalized, expiration).Scan(&startedAt, &retryAfter, &lastInterval, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return 100 * time.Millisecond, nil
	}
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	if at, ok := parsePushRefreshTime(retryAfter); ok && now.Before(at) {
		return at.Sub(now), nil
	}
	if at, ok := parsePushRefreshTime(leaseUntil); ok && now.Before(at) {
		wait := at.Sub(now)
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		return wait, nil
	}
	if time.Duration(lastInterval)*time.Second != interval {
		return 100 * time.Millisecond, nil
	}
	started, ok := parsePushRefreshTime(startedAt)
	if !ok {
		return 100 * time.Millisecond, nil
	}
	wait := interval - time.Second - now.Sub(started)
	if wait < 100*time.Millisecond {
		wait = 100 * time.Millisecond
	}
	return wait, nil
}

func (s *SQLite) FinishPushRefresh(symbol, expiration string) error {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	_, err := s.db.Exec(`UPDATE push_refresh_state SET lease_until=NULL, retry_delay_seconds=0 WHERE symbol=? AND expiration=?`, normalized, expiration)
	return err
}

// DeferPushRefresh records an exponentially increasing shared retry delay.
func (s *SQLite) DeferPushRefresh(symbol, expiration string, base time.Duration) (time.Duration, error) {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	if base <= 0 {
		base = time.Second
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var previous int
	err = conn.QueryRowContext(context.Background(), `SELECT retry_delay_seconds FROM push_refresh_state WHERE symbol=? AND expiration=?`, normalized, expiration).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		previous = 0
	} else if err != nil {
		return 0, err
	}
	delay := base
	if doubled := time.Duration(previous*2) * time.Second; doubled > delay {
		delay = doubled
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > maxPushRefreshBackoff {
		delay = maxPushRefreshBackoff
	}
	retryAt := storedTimestamp(time.Now().UTC().Add(delay))
	result, err := conn.ExecContext(context.Background(), `UPDATE push_refresh_state SET retry_after=?, retry_delay_seconds=?, lease_until=NULL WHERE symbol=? AND expiration=?`, retryAt, int(delay/time.Second), normalized, expiration)
	if err != nil {
		return 0, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		_, err = conn.ExecContext(context.Background(), `INSERT INTO push_refresh_state(symbol, expiration, started_at, retry_after, retry_delay_seconds, last_interval_seconds, lease_until) VALUES (?, ?, ?, ?, ?, 0, NULL)`, normalized, expiration, storedTimestamp(time.Now().UTC()), retryAt, int(delay/time.Second))
		if err != nil {
			return 0, err
		}
	}
	if err := commitImmediate(conn, &committed); err != nil {
		return 0, err
	}
	return delay, nil
}

func commitImmediate(conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return err
	}
	*committed = true
	return nil
}

func parsePushRefreshTime(value sql.NullString) (time.Time, bool) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

func (s *SQLite) LatestQuote(symbol string) (model.Quote, bool, error) {
	var quote model.Quote
	var quoteID int64
	var fetched, sessions string
	var rawJSON sql.NullString
	var providerName, currency, marketState, fairSource, fairModel sql.NullString
	var epsSource, confidence, regime, modelUnderRegime, normalizedEPSSource sql.NullString
	var marketCapQuality, ownerEarningsRatioSource sql.NullString
	var regimeSignals, defensive, optimistic, quarterlyMomentum, historicalPercentiles sql.NullString
	var price, change, todayOpen, previous, fairValue, fairLow, fairHigh, fairBuyLow, fairBuyHigh sql.NullFloat64
	var methodsJSON sql.NullString
	var forwardEPS, safetyMargin, confidenceScore, interestCoverage, shareholderReturnYield sql.NullFloat64
	var dataQualityScore, ownerEarningsMaintenanceRatio sql.NullFloat64
	err := s.db.QueryRow(`WITH quote_rows AS (
 SELECT quote_snapshots.*,
  CASE WHEN provider='go-valuation' THEN COALESCE((
   SELECT source.fetched_at FROM quote_snapshots AS source
   WHERE source.symbol=quote_snapshots.symbol AND source.provider<>'go-valuation'
    AND ((source.today_open IS NULL AND quote_snapshots.today_open IS NULL) OR
     (source.today_open IS NOT NULL AND quote_snapshots.today_open IS NOT NULL AND
      ABS(source.today_open-quote_snapshots.today_open)<=MAX(0.01,ABS(quote_snapshots.today_open)*0.0001)))
    AND ((source.previous_close IS NULL AND quote_snapshots.previous_close IS NULL) OR
     (source.previous_close IS NOT NULL AND quote_snapshots.previous_close IS NOT NULL AND
      ABS(source.previous_close-quote_snapshots.previous_close)<=MAX(0.01,ABS(quote_snapshots.previous_close)*0.0001)))
    AND source.market_state IS quote_snapshots.market_state
    AND source.fetched_at<=quote_snapshots.fetched_at
   ORDER BY source.fetched_at DESC LIMIT 1
  ), fetched_at) ELSE fetched_at END AS market_fetched_at
 FROM quote_snapshots WHERE symbol=?
)
SELECT id, raw_json, market_fetched_at, price, change_percent, today_open, previous_close, currency, market_state, provider, COALESCE(sessions_json, '{}'),
 fair_value, fair_value_low, fair_value_high, fair_value_buy_low, fair_value_buy_high, fair_value_source, fair_value_model, fair_value_methods_json,
 fair_value_forward_eps, fair_value_forward_eps_source, fair_value_safety_margin, fair_value_confidence,
 fair_value_confidence_score, fair_value_interest_coverage, fair_value_regime, fair_value_regime_signals_json,
 fair_value_model_under_regime, fair_value_defensive_json, fair_value_optimistic_json,
 fair_value_normalized_eps_source, fair_value_quarterly_momentum_json,
 fair_value_historical_valuation_percentiles_json, fair_value_shareholder_total_return_yield,
 fair_value_market_cap_data_quality, fair_value_data_quality_score,
 fair_value_owner_earnings_maintenance_ratio, fair_value_owner_earnings_ratio_source
FROM quote_rows
ORDER BY CASE WHEN price IS NOT NULL AND price > 0 THEN 0 ELSE 1 END,
 market_fetched_at DESC, CASE WHEN provider='go-valuation' THEN 0 ELSE 1 END, fetched_at DESC LIMIT 1`, symbol).Scan(
		&quoteID, &rawJSON, &fetched, &price, &change, &todayOpen, &previous, &currency, &marketState, &providerName, &sessions,
		&fairValue, &fairLow, &fairHigh, &fairBuyLow, &fairBuyHigh, &fairSource, &fairModel, &methodsJSON,
		&forwardEPS, &epsSource, &safetyMargin, &confidence, &confidenceScore, &interestCoverage,
		&regime, &regimeSignals, &modelUnderRegime, &defensive, &optimistic, &normalizedEPSSource,
		&quarterlyMomentum, &historicalPercentiles, &shareholderReturnYield, &marketCapQuality,
		&dataQualityScore, &ownerEarningsMaintenanceRatio, &ownerEarningsRatioSource)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Quote{}, false, nil
	}
	if err != nil {
		return model.Quote{}, false, err
	}
	if strings.TrimSpace(sessions) == "" {
		sessions = "{}"
	}
	_ = json.Unmarshal([]byte(sessions), &quote.Sessions)
	if quote.Sessions == nil {
		quote.Sessions = map[string]any{}
	}
	when, err := time.Parse(time.RFC3339Nano, fetched)
	if err != nil {
		when, _ = time.Parse("2006-01-02T15:04:05.999999", fetched)
	}
	quote.SnapshotID = quoteID
	quote.RawJSON = nullableString(rawJSON)
	quote.FairValueMethodsJSON = nullableString(methodsJSON)
	quote.FairValueHistoricalValuationJSON = nullableString(historicalPercentiles)
	quote.Symbol, quote.Currency, quote.MarketState = symbol, currency.String, marketState.String
	quote.Provider, quote.Source, quote.FetchedAt = providerName.String, "sqlite", &when
	quote.Price, quote.ChangePercent, quote.TodayOpen, quote.PreviousClose = nullableFloat(price), nullableFloat(change), nullableFloat(todayOpen), nullableFloat(previous)
	quote.FairValue = nullableAnyFloat(fairValue)
	quote.FairValueLow, quote.FairValueHigh = nullableAnyFloat(fairLow), nullableAnyFloat(fairHigh)
	quote.FairValueBuyLow, quote.FairValueBuyHigh = nullableAnyFloat(fairBuyLow), nullableAnyFloat(fairBuyHigh)
	if fairSource.Valid {
		quote.FairValueSource = fairSource.String
	}
	if fairModel.Valid {
		quote.FairValueModel = fairModel.String
	}
	quote.FairValueMethods = decodeJSONColumn(methodsJSON)
	quote.FairValueForwardEPS = nullableFloat(forwardEPS)
	quote.FairValueForwardEPSSource = nullableString(epsSource)
	quote.FairValueSafetyMargin = nullableFloat(safetyMargin)
	quote.FairValueConfidence = nullableString(confidence)
	quote.FairValueConfidenceScore = nullableFloat(confidenceScore)
	quote.FairValueInterestCoverage = nullableFloat(interestCoverage)
	quote.FairValueRegime = nullableString(regime)
	quote.FairValueRegimeSignals = decodeJSONColumn(regimeSignals)
	quote.FairValueModelUnderRegime = nullableString(modelUnderRegime)
	quote.FairValueDefensive = decodeJSONColumn(defensive)
	quote.FairValueOptimistic = decodeJSONColumn(optimistic)
	quote.FairValueNormalizedEPSSource = nullableString(normalizedEPSSource)
	quote.FairValueQuarterlyMomentum = decodeJSONColumn(quarterlyMomentum)
	quote.FairValueHistoricalValuationPct = decodeJSONColumn(historicalPercentiles)
	quote.FairValueShareholderReturnYield = nullableFloat(shareholderReturnYield)
	quote.FairValueMarketCapDataQuality = nullableString(marketCapQuality)
	quote.FairValueDataQualityScore = nullableFloat(dataQualityScore)
	quote.FairValueOwnerEarningsMaintRatio = nullableFloat(ownerEarningsMaintenanceRatio)
	quote.FairValueOwnerEarningsRatioSource = nullableString(ownerEarningsRatioSource)
	quote.FairValueStatus = "pending"
	if fairSource.Valid && fairSource.String != "" && fairValue.Valid {
		quote.FairValueStatus, quote.FairValuePending = "ready", false
	} else {
		quote.FairValuePending = true
	}
	return quote, true, nil
}

func (s *SQLite) SaveQuote(quote model.Quote) error {
	_, err := s.SaveQuotePreservingFairValue(quote)
	return err
}

func (s *SQLite) SaveQuotePreservingFairValue(quote model.Quote) (model.Quote, error) {
	var err error
	quote, err = s.PreserveCachedFairValue(quote)
	if err != nil {
		return model.Quote{}, err
	}
	return s.saveQuoteSnapshot(quote)
}

// SaveQuoteReplacingFairValue stores an explicit replacement, including an
// intentional no-value result. Most market quote writes should use
// SaveQuotePreservingFairValue; valuation status transitions use this method
// when a newly detected instrument type invalidates an older company estimate.
func (s *SQLite) SaveQuoteReplacingFairValue(quote model.Quote) (model.Quote, error) {
	if quote.Price == nil || math.IsNaN(*quote.Price) || math.IsInf(*quote.Price, 0) || *quote.Price <= 0 {
		return model.Quote{}, errors.New("refusing to persist quote snapshot without a positive price")
	}
	return s.saveQuoteSnapshot(quote)
}

func (s *SQLite) saveQuoteSnapshot(quote model.Quote) (model.Quote, error) {
	if quote.Provider == "" {
		quote.Provider = "upstream"
	}
	if quote.FetchedAt == nil {
		now := time.Now().UTC()
		quote.FetchedAt = &now
	}
	sessions, _ := json.Marshal(quote.Sessions)
	result, err := s.db.Exec(`INSERT INTO quote_snapshots
(symbol, fetched_at, price, change_percent, today_open, currency, market_state, provider, sessions_json, previous_close,
 fair_value, fair_value_low, fair_value_high, fair_value_buy_low, fair_value_buy_high, fair_value_source, fair_value_model, fair_value_methods_json,
 fair_value_forward_eps, fair_value_forward_eps_source, fair_value_safety_margin, fair_value_confidence,
 fair_value_confidence_score, fair_value_interest_coverage, fair_value_regime, fair_value_regime_signals_json,
 fair_value_model_under_regime, fair_value_defensive_json, fair_value_optimistic_json,
 fair_value_normalized_eps_source, fair_value_quarterly_momentum_json,
 fair_value_historical_valuation_percentiles_json, fair_value_shareholder_total_return_yield,
 fair_value_market_cap_data_quality, fair_value_data_quality_score,
 fair_value_owner_earnings_maintenance_ratio, fair_value_owner_earnings_ratio_source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,

		quote.Symbol, storedTimestamp(*quote.FetchedAt), valueOrNil(quote.Price), valueOrNil(quote.ChangePercent), valueOrNil(quote.TodayOpen), quote.Currency, quote.MarketState, quote.Provider, string(sessions), valueOrNil(quote.PreviousClose),
		valueOrNilFloatAny(quote.FairValue), valueOrNilFloatAny(quote.FairValueLow), valueOrNilFloatAny(quote.FairValueHigh), valueOrNilFloatAny(quote.FairValueBuyLow), valueOrNilFloatAny(quote.FairValueBuyHigh), quote.FairValueSource, quote.FairValueModel, jsonOrEmptyObject(quote.FairValueMethods),
		valueOrNil(quote.FairValueForwardEPS), valueOrNil(quote.FairValueForwardEPSSource), valueOrNil(quote.FairValueSafetyMargin), valueOrNil(quote.FairValueConfidence),
		valueOrNil(quote.FairValueConfidenceScore), valueOrNil(quote.FairValueInterestCoverage), valueOrNil(quote.FairValueRegime), jsonOrEmptyObject(quote.FairValueRegimeSignals),
		valueOrNil(quote.FairValueModelUnderRegime), jsonOrEmptyObject(quote.FairValueDefensive), jsonOrEmptyObject(quote.FairValueOptimistic),
		valueOrNil(quote.FairValueNormalizedEPSSource), jsonOrEmptyObject(quote.FairValueQuarterlyMomentum),
		jsonOrEmptyObject(quote.FairValueHistoricalValuationPct), valueOrNil(quote.FairValueShareholderReturnYield), valueOrNil(quote.FairValueMarketCapDataQuality),
		valueOrNil(quote.FairValueDataQualityScore), valueOrNil(quote.FairValueOwnerEarningsMaintRatio), valueOrNil(quote.FairValueOwnerEarningsRatioSource))
	if err != nil {
		return model.Quote{}, err
	}
	quoteID, err := result.LastInsertId()
	if err != nil {
		return model.Quote{}, err
	}
	setQuoteSnapshotMetadata(&quote, quoteID)
	return quote, nil
}

// SaveQuoteAndChainSnapshot atomically writes a quote and its selected option chain.
// Fair-value fields omitted by the upstream quote are copied from the latest
// persisted quote before the transaction begins, matching SaveQuote's existing
// preservation behavior. The option batch marker is committed with both rows so
// readers never observe a quote without its corresponding chain (or vice versa).
func (s *SQLite) SaveQuoteAndChainSnapshot(quote model.Quote, chain model.ChainResponse) (model.Quote, error) {
	if quote.Price == nil || math.IsNaN(*quote.Price) || math.IsInf(*quote.Price, 0) || *quote.Price <= 0 {
		return model.Quote{}, errors.New("refusing to persist quote snapshot without a positive price")
	}
	var err error
	quote, err = s.PreserveCachedFairValue(quote)
	if err != nil {
		return model.Quote{}, err
	}
	if quote.FetchedAt == nil {
		now := time.Now().UTC()
		quote.FetchedAt = &now
	}
	if chain.FetchedAt == nil {
		when := *quote.FetchedAt
		chain.FetchedAt = &when
	}
	stamp := storedTimestamp(*chain.FetchedAt)
	tx, err := s.db.Begin()
	if err != nil {
		return model.Quote{}, err
	}
	defer tx.Rollback()
	sessions, _ := json.Marshal(quote.Sessions)
	quoteResult, err := tx.Exec(`INSERT INTO quote_snapshots
	(symbol, fetched_at, price, change_percent, today_open, currency, market_state, provider, sessions_json, previous_close,
	 fair_value, fair_value_low, fair_value_high, fair_value_buy_low, fair_value_buy_high, fair_value_source, fair_value_model, fair_value_methods_json,
	 fair_value_forward_eps, fair_value_forward_eps_source, fair_value_safety_margin, fair_value_confidence,
	 fair_value_confidence_score, fair_value_interest_coverage, fair_value_regime, fair_value_regime_signals_json,
	 fair_value_model_under_regime, fair_value_defensive_json, fair_value_optimistic_json,
	 fair_value_normalized_eps_source, fair_value_quarterly_momentum_json,
	 fair_value_historical_valuation_percentiles_json, fair_value_shareholder_total_return_yield,
	 fair_value_market_cap_data_quality, fair_value_data_quality_score,
	 fair_value_owner_earnings_maintenance_ratio, fair_value_owner_earnings_ratio_source)
	VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?
		)`,
		quote.Symbol, storedTimestamp(*quote.FetchedAt), valueOrNil(quote.Price), valueOrNil(quote.ChangePercent), valueOrNil(quote.TodayOpen), quote.Currency, quote.MarketState, quote.Provider, string(sessions), valueOrNil(quote.PreviousClose),
		valueOrNilFloatAny(quote.FairValue), valueOrNilFloatAny(quote.FairValueLow), valueOrNilFloatAny(quote.FairValueHigh), valueOrNilFloatAny(quote.FairValueBuyLow), valueOrNilFloatAny(quote.FairValueBuyHigh), quote.FairValueSource, quote.FairValueModel, jsonOrEmptyObject(quote.FairValueMethods),
		valueOrNil(quote.FairValueForwardEPS), valueOrNil(quote.FairValueForwardEPSSource), valueOrNil(quote.FairValueSafetyMargin), valueOrNil(quote.FairValueConfidence),
		valueOrNil(quote.FairValueConfidenceScore), valueOrNil(quote.FairValueInterestCoverage), valueOrNil(quote.FairValueRegime), jsonOrEmptyObject(quote.FairValueRegimeSignals),
		valueOrNil(quote.FairValueModelUnderRegime), jsonOrEmptyObject(quote.FairValueDefensive), jsonOrEmptyObject(quote.FairValueOptimistic),
		valueOrNil(quote.FairValueNormalizedEPSSource), jsonOrEmptyObject(quote.FairValueQuarterlyMomentum),
		jsonOrEmptyObject(quote.FairValueHistoricalValuationPct), valueOrNil(quote.FairValueShareholderReturnYield), valueOrNil(quote.FairValueMarketCapDataQuality),
		valueOrNil(quote.FairValueDataQualityScore), valueOrNil(quote.FairValueOwnerEarningsMaintRatio), valueOrNil(quote.FairValueOwnerEarningsRatioSource))
	if err != nil {
		return model.Quote{}, err
	}
	quoteID, err := quoteResult.LastInsertId()
	if err != nil {
		return model.Quote{}, err
	}
	chainProvider := strings.TrimSpace(chain.Source)
	if chainProvider == "" || chainProvider == "sqlite" || chainProvider == "upstream" {
		chainProvider = "yahoo-options"
	}
	for _, row := range chain.Data {
		rowProvider := row.Provider
		if rowProvider == "" {
			rowProvider = chainProvider
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO option_snapshots
	(symbol, expiration, fetched_at, contract_symbol, contract_type, strike, last_price, bid, ask, volume, open_interest, implied_volatility, gamma, in_the_money, change_percent, provider)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, chain.Symbol, chain.Expiration, stamp, row.ContractSymbol, row.ContractType, valueOrNil(row.Strike), valueOrNil(row.LastPrice), valueOrNil(row.Bid), valueOrNil(row.Ask), valueOrNil(row.Volume), valueOrNil(row.OpenInterest), valueOrNil(row.ImpliedVolatility), valueOrNil(row.Gamma), nullableBoolInt(row.InTheMoney, row.InTheMoneyKnown), valueOrNil(row.ChangePercent), rowProvider); err != nil {
			return model.Quote{}, err
		}
	}
	if len(chain.Data) > 0 {
		if _, err := tx.Exec(`INSERT INTO option_latest_batches(symbol, expiration, fetched_at) VALUES (?, ?, ?)
ON CONFLICT(symbol, expiration) DO UPDATE SET fetched_at=excluded.fetched_at
WHERE excluded.fetched_at >= option_latest_batches.fetched_at`, chain.Symbol, chain.Expiration, stamp); err != nil {
			return model.Quote{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Quote{}, err
	}
	setQuoteSnapshotMetadata(&quote, quoteID)
	return quote, nil
}

func setQuoteSnapshotMetadata(quote *model.Quote, id int64) {
	quote.SnapshotID = id
	quote.RawJSON = nil
	quote.FairValueMethodsJSON = stringPointer(jsonOrEmptyObject(quote.FairValueMethods))
	quote.FairValueHistoricalValuationJSON = stringPointer(jsonOrEmptyObject(quote.FairValueHistoricalValuationPct))
}

func stringPointer(value string) *string {
	return &value
}

func (s *SQLite) PreserveCachedFairValue(quote model.Quote) (model.Quote, error) {
	if quote.Symbol == "" {
		return quote, nil
	}
	cached, found, err := s.LatestQuote(quote.Symbol)
	if err != nil || !found || cached.FairValue == nil {
		return quote, err
	}
	if quote.FairValue == nil {
		quote.FairValue = cached.FairValue
	}
	if quote.FairValueLow == nil {
		quote.FairValueLow = cached.FairValueLow
	}
	if quote.FairValueHigh == nil {
		quote.FairValueHigh = cached.FairValueHigh
	}
	if quote.FairValueBuyLow == nil {
		quote.FairValueBuyLow = cached.FairValueBuyLow
	}
	if quote.FairValueBuyHigh == nil {
		quote.FairValueBuyHigh = cached.FairValueBuyHigh
	}
	if quote.FairValueSource == "" {
		quote.FairValueSource = cached.FairValueSource
	}
	if quote.FairValueModel == "" {
		quote.FairValueModel = cached.FairValueModel
	}
	if isEmptyJSONValue(quote.FairValueMethods) {
		quote.FairValueMethods = cached.FairValueMethods
	}
	if quote.FairValueForwardEPS == nil {
		quote.FairValueForwardEPS = cached.FairValueForwardEPS
	}
	if quote.FairValueForwardEPSSource == nil {
		quote.FairValueForwardEPSSource = cached.FairValueForwardEPSSource
	}
	if quote.FairValueSafetyMargin == nil {
		quote.FairValueSafetyMargin = cached.FairValueSafetyMargin
	}
	if quote.FairValueConfidence == nil {
		quote.FairValueConfidence = cached.FairValueConfidence
	}
	if quote.FairValueConfidenceScore == nil {
		quote.FairValueConfidenceScore = cached.FairValueConfidenceScore
	}
	if quote.FairValueInterestCoverage == nil {
		quote.FairValueInterestCoverage = cached.FairValueInterestCoverage
	}
	if quote.FairValueRegime == nil {
		quote.FairValueRegime = cached.FairValueRegime
	}
	if isEmptyJSONValue(quote.FairValueRegimeSignals) {
		quote.FairValueRegimeSignals = cached.FairValueRegimeSignals
	}
	if quote.FairValueModelUnderRegime == nil {
		quote.FairValueModelUnderRegime = cached.FairValueModelUnderRegime
	}
	if isEmptyJSONValue(quote.FairValueDefensive) {
		quote.FairValueDefensive = cached.FairValueDefensive
	}
	if isEmptyJSONValue(quote.FairValueOptimistic) {
		quote.FairValueOptimistic = cached.FairValueOptimistic
	}
	if quote.FairValueNormalizedEPSSource == nil {
		quote.FairValueNormalizedEPSSource = cached.FairValueNormalizedEPSSource
	}
	if isEmptyJSONValue(quote.FairValueQuarterlyMomentum) {
		quote.FairValueQuarterlyMomentum = cached.FairValueQuarterlyMomentum
	}
	if isEmptyJSONValue(quote.FairValueHistoricalValuationPct) {
		quote.FairValueHistoricalValuationPct = cached.FairValueHistoricalValuationPct
	}
	if quote.FairValueShareholderReturnYield == nil {
		quote.FairValueShareholderReturnYield = cached.FairValueShareholderReturnYield
	}
	if quote.FairValueMarketCapDataQuality == nil {
		quote.FairValueMarketCapDataQuality = cached.FairValueMarketCapDataQuality
	}
	if quote.FairValueDataQualityScore == nil {
		quote.FairValueDataQualityScore = cached.FairValueDataQualityScore
	}
	if quote.FairValueOwnerEarningsMaintRatio == nil {
		quote.FairValueOwnerEarningsMaintRatio = cached.FairValueOwnerEarningsMaintRatio
	}
	if quote.FairValueOwnerEarningsRatioSource == nil {
		quote.FairValueOwnerEarningsRatioSource = cached.FairValueOwnerEarningsRatioSource
	}
	return quote, nil
}

func isEmptyJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	case []map[string]any:
		return len(typed) == 0
	case string:
		return typed == ""
	default:
		return false
	}
}

func (s *SQLite) LatestExpirations(symbol string) ([]string, bool, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload FROM expiration_catalog WHERE symbol=?`, symbol).Scan(&payload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if err == nil {
		values := cleanExpirationCatalog(payload)
		if len(values) > 0 {
			return values, true, nil
		}
	}
	return s.latestExpirationsFromBatches(symbol)
}

// CachedExpirations lists expirations that have a readable persisted option
// batch. The provider catalog can include many expirations never downloaded
// locally, so it must not be used to infer cache availability.
func (s *SQLite) CachedExpirations(symbol string) ([]string, error) {
	rows, err := s.db.Query(`SELECT latest.expiration FROM option_latest_batches latest
WHERE latest.symbol=? AND EXISTS (
  SELECT 1 FROM option_snapshots snapshot
  WHERE snapshot.symbol=latest.symbol AND snapshot.expiration=latest.expiration
    AND snapshot.fetched_at=latest.fetched_at
) ORDER BY latest.expiration`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func cleanExpirationCatalog(payload string) []string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &object); err != nil || object == nil {
		return nil
	}
	raw, ok := object["expirations"]
	if !ok {
		return nil
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	stringsOnly := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			stringsOnly = append(stringsOnly, text)
		}
	}
	return cleanExpirationDates(stringsOnly)
}

func cleanExpirationDates(values []string) []string {
	cleaned := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil || parsed.Format("2006-01-02") != value {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

func (s *SQLite) latestExpirationsFromBatches(symbol string) ([]string, bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT expiration FROM option_latest_batches WHERE symbol=? ORDER BY expiration`, symbol)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, false, err
		}
		values = append(values, value)
	}
	return values, len(values) > 0, rows.Err()
}

func (s *SQLite) ExpirationProbeEmpty(symbol string, maxAge time.Duration) (bool, error) {
	var empty int
	var fetched string
	err := s.db.QueryRow("SELECT empty,fetched_at FROM expiration_probe_status WHERE symbol=?", symbol).Scan(&empty, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339Nano, fetched)
	if err != nil {
		return false, nil
	}
	return empty != 0 && time.Since(at) < maxAge, nil
}

func (s *SQLite) SaveExpirationProbe(symbol string, empty bool) error {
	flag := 0
	if empty {
		flag = 1
	}
	_, err := s.db.Exec(`INSERT INTO expiration_probe_status(symbol,empty,fetched_at) VALUES(?,?,?) ON CONFLICT(symbol) DO UPDATE SET empty=excluded.empty,fetched_at=excluded.fetched_at`, symbol, flag, storedTimestamp(time.Now().UTC()))
	return err
}

func (s *SQLite) SaveExpirations(symbol string, expirations []string) error {
	// Match Python write_expiration_catalog: an empty provider response is not
	// authoritative and must not erase the last known catalog.
	expirations = cleanExpirationDates(expirations)
	if len(expirations) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"expirations": expirations})
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO expiration_catalog(symbol, payload, fetched_at) VALUES (?, ?, ?)
ON CONFLICT(symbol) DO UPDATE SET payload=excluded.payload, fetched_at=excluded.fetched_at`, symbol, string(payload), storedTimestamp(time.Now().UTC())); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM expiration_probe_status WHERE symbol=?", symbol); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestChains returns the newest cached batch for each expiration in the requested window.
// It deliberately reads the existing option_latest_batches table rather than assuming
// that Go was the process which created the snapshots.
func (s *SQLite) LatestChains(symbol string, horizonDays int) (map[string]any, error) {
	if horizonDays < 1 {
		horizonDays = 45
	}
	location, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(location).Format("2006-01-02")
	end := time.Now().In(location).AddDate(0, 0, horizonDays).Format("2006-01-02")
	query := `SELECT current.contract_symbol, current.expiration, current.contract_type,
 current.strike, current.last_price, current.bid, current.ask, current.volume,
 current.open_interest, current.implied_volatility, current.gamma, current.in_the_money,
 current.change_percent, current.provider, current.fetched_at,
 (SELECT previous.open_interest FROM option_snapshots previous
   WHERE previous.symbol=current.symbol AND previous.expiration=current.expiration
     AND previous.contract_symbol=current.contract_symbol AND previous.open_interest > 0
     AND previous.fetched_at < current.fetched_at
   ORDER BY previous.fetched_at DESC LIMIT 1) AS fallback_oi,
 (SELECT previous.fetched_at FROM option_snapshots previous
   WHERE previous.symbol=current.symbol AND previous.expiration=current.expiration
     AND previous.contract_symbol=current.contract_symbol AND previous.open_interest > 0
     AND previous.fetched_at < current.fetched_at
   ORDER BY previous.fetched_at DESC LIMIT 1) AS fallback_at
FROM option_latest_batches latest
JOIN option_snapshots current
  ON current.symbol=latest.symbol AND current.expiration=latest.expiration
 AND current.fetched_at=latest.fetched_at
WHERE latest.symbol=? AND latest.expiration BETWEEN ? AND ?
ORDER BY current.expiration, current.strike, current.contract_type`
	rows, err := s.db.Query(query, symbol, today, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []model.OptionRow{}
	expirations := []string{}
	seenExpirations := map[string]bool{}
	var latestFetched string
	restored := 0
	var fallbackAsOf string
	for rows.Next() {
		var row model.OptionRow
		var fetched, fallbackAt, providerName sql.NullString
		var strike, last, bid, ask, iv, gamma, change, fallbackOI sql.NullFloat64
		var volume, oi sql.NullInt64
		var itm sql.NullInt64
		if err := rows.Scan(&row.ContractSymbol, &row.Expiration, &row.ContractType, &strike, &last, &bid, &ask, &volume, &oi, &iv, &gamma, &itm, &change, &providerName, &fetched, &fallbackOI, &fallbackAt); err != nil {
			return nil, err
		}
		row.Strike, row.LastPrice, row.Bid, row.Ask = nullableFloat(strike), nullableFloat(last), nullableFloat(bid), nullableFloat(ask)
		row.Volume, row.OpenInterest = nullableInt(volume), nullableInt(oi)
		row.ImpliedVolatility, row.Gamma, row.ChangePercent = nullableFloat(iv), nullableFloat(gamma), nullableFloat(change)
		row.Provider = providerName.String
		row.InTheMoney = itm.Valid && itm.Int64 != 0
		row.InTheMoneyKnown = itm.Valid
		if (row.OpenInterest == nil || *row.OpenInterest <= 0) && fallbackOI.Valid && fallbackOI.Float64 > 0 {
			fallback := int64(fallbackOI.Float64)
			row.OpenInterest = &fallback
			restored++
			if fallbackAt.Valid && fallbackAt.String > fallbackAsOf {
				fallbackAsOf = fallbackAt.String
			}
		}
		data = append(data, row)
		if !seenExpirations[row.Expiration] {
			seenExpirations[row.Expiration] = true
			expirations = append(expirations, row.Expiration)
		}
		if fetched.Valid && fetched.String > latestFetched {
			latestFetched = fetched.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var fetchedAt any
	if latestFetched != "" {
		if parsed, parseErr := parseStoredTime(latestFetched); parseErr == nil {
			fetchedAt = parsed
		} else {
			fetchedAt = latestFetched
		}
	}
	return map[string]any{
		"fetched_at": fetchedAt, "data": data, "expirations": expirations,
		"horizon_days": horizonDays, "oi_fallback": map[string]any{"restored": restored, "as_of": nilIfEmpty(fallbackAsOf)},
	}, nil
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *SQLite) LatestChainBatchTime(symbol, expiration string) (*time.Time, bool, error) {
	var fetched string
	err := s.db.QueryRow(`SELECT fetched_at FROM option_latest_batches WHERE symbol=? AND expiration=?`, symbol, expiration).Scan(&fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	when, err := parseStoredTime(fetched)
	if err != nil {
		return nil, false, err
	}
	return &when, true, nil
}

func (s *SQLite) LatestChain(symbol, expiration string) (model.ChainResponse, bool, error) {
	var fetched string
	err := s.db.QueryRow(`SELECT fetched_at FROM option_latest_batches WHERE symbol=? AND expiration=?`, symbol, expiration).Scan(&fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ChainResponse{}, false, nil
	}
	if err != nil {
		return model.ChainResponse{}, false, err
	}
	rows, err := s.db.Query(`SELECT current.contract_symbol, current.contract_type, current.strike, current.last_price, current.bid, current.ask, current.volume,
 current.open_interest, current.implied_volatility, current.gamma, current.in_the_money, current.change_percent, current.provider,
 (SELECT previous.open_interest FROM option_snapshots previous
   WHERE previous.symbol=current.symbol AND previous.expiration=current.expiration
     AND previous.contract_symbol=current.contract_symbol AND previous.open_interest > 0
     AND previous.fetched_at < current.fetched_at
   ORDER BY previous.fetched_at DESC LIMIT 1) AS fallback_oi,
 (SELECT previous.fetched_at FROM option_snapshots previous
   WHERE previous.symbol=current.symbol AND previous.expiration=current.expiration
     AND previous.contract_symbol=current.contract_symbol AND previous.open_interest > 0
     AND previous.fetched_at < current.fetched_at
   ORDER BY previous.fetched_at DESC LIMIT 1) AS fallback_at
FROM option_snapshots current WHERE current.symbol=? AND current.expiration=? AND current.fetched_at=? ORDER BY current.strike, current.contract_type`, symbol, expiration, fetched)
	if err != nil {
		return model.ChainResponse{}, false, err
	}
	defer rows.Close()
	data := []model.OptionRow{}
	restoredOI := 0
	fallbackAsOf := ""
	for rows.Next() {
		var row model.OptionRow
		var strike, last, bid, ask, iv, gamma, change, fallbackOI sql.NullFloat64
		var volume, oi sql.NullInt64
		var providerName, fallbackAt sql.NullString
		var itm sql.NullInt64
		if err := rows.Scan(&row.ContractSymbol, &row.ContractType, &strike, &last, &bid, &ask, &volume, &oi, &iv, &gamma, &itm, &change, &providerName, &fallbackOI, &fallbackAt); err != nil {
			return model.ChainResponse{}, false, err
		}
		row.Expiration, row.Strike, row.LastPrice, row.Bid, row.Ask = expiration, nullableFloat(strike), nullableFloat(last), nullableFloat(bid), nullableFloat(ask)
		row.Provider = providerName.String
		row.Volume, row.OpenInterest, row.ImpliedVolatility, row.Gamma, row.ChangePercent = nullableInt(volume), nullableInt(oi), nullableFloat(iv), nullableFloat(gamma), nullableFloat(change)
		row.InTheMoney = itm.Valid && itm.Int64 != 0
		row.InTheMoneyKnown = itm.Valid
		if (row.OpenInterest == nil || *row.OpenInterest <= 0) && fallbackOI.Valid && fallbackOI.Float64 > 0 {
			fallback := int64(fallbackOI.Float64)
			row.OpenInterest = &fallback
			restoredOI++
			if fallbackAt.Valid && fallbackAt.String > fallbackAsOf {
				fallbackAsOf = fallbackAt.String
			}
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		return model.ChainResponse{}, false, err
	}
	when, parseErr := parseStoredTime(fetched)
	if parseErr != nil {
		return model.ChainResponse{}, false, parseErr
	}
	if len(data) == 0 {
		return model.ChainResponse{Symbol: symbol, Expiration: expiration, FetchedAt: &when, Data: data, Source: "sqlite"}, false, nil
	}
	return model.ChainResponse{Symbol: symbol, Expiration: expiration, FetchedAt: &when, Data: data, Source: "sqlite",
		OIFallback: map[string]any{"restored": restoredOI, "as_of": nilIfEmpty(fallbackAsOf)}}, true, rows.Err()
}

func (s *SQLite) SaveChain(response model.ChainResponse) error {
	if response.FetchedAt == nil {
		now := time.Now().UTC()
		response.FetchedAt = &now
	}
	stamp := storedTimestamp(*response.FetchedAt)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range response.Data {
		providerName := strings.TrimSpace(row.Provider)
		if providerName == "" {
			providerName = strings.TrimSpace(response.Source)
		}
		if providerName == "" || providerName == "sqlite" || providerName == "upstream" {
			providerName = "yahoo-options"
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO option_snapshots
(symbol, expiration, fetched_at, contract_symbol, contract_type, strike, last_price, bid, ask, volume, open_interest, implied_volatility, gamma, in_the_money, change_percent, provider)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, response.Symbol, response.Expiration, stamp, row.ContractSymbol, row.ContractType, valueOrNil(row.Strike), valueOrNil(row.LastPrice), valueOrNil(row.Bid), valueOrNil(row.Ask), valueOrNil(row.Volume), valueOrNil(row.OpenInterest), valueOrNil(row.ImpliedVolatility), valueOrNil(row.Gamma), nullableBoolInt(row.InTheMoney, row.InTheMoneyKnown), valueOrNil(row.ChangePercent), providerName); err != nil {
			return err
		}
	}
	if len(response.Data) > 0 {
		if _, err := tx.Exec(`INSERT INTO option_latest_batches(symbol, expiration, fetched_at) VALUES (?, ?, ?)
ON CONFLICT(symbol, expiration) DO UPDATE SET fetched_at=excluded.fetched_at
WHERE excluded.fetched_at >= option_latest_batches.fetched_at`, response.Symbol, response.Expiration, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WriteChanAdjustedHistory stores adjusted Chan bars and their raw-price
// counterparts independently from price_history. The composite key isolates
// symbol, timeframe, adjustment policy, and provider cache entries.
func (s *SQLite) WriteChanAdjustedHistory(symbol, timeframe, adjustmentType, providerName string, bars []map[string]any, fetchedAt time.Time) error {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	timeframe = strings.TrimSpace(timeframe)
	adjustmentType = strings.TrimSpace(adjustmentType)
	providerName = strings.TrimSpace(providerName)
	if symbol == "" || timeframe == "" || adjustmentType == "" || providerName == "" {
		return errors.New("Chan adjusted history cache dimensions must not be empty")
	}
	if fetchedAt.IsZero() {
		return errors.New("Chan adjusted history fetched_at must not be zero")
	}
	stamp := storedTimestamp(fetchedAt)
	type preparedBar struct {
		day    string
		values [11]float64
	}
	prepared := make([]preparedBar, 0, len(bars))
	fields := []string{"open", "high", "low", "close", "volume", "raw_open", "raw_high", "raw_low", "raw_close", "raw_volume", "adjustment_factor"}
	for index, bar := range bars {
		day, ok := bar["date"].(string)
		parsed, err := time.Parse(time.DateOnly, day)
		if !ok || err != nil || parsed.Format(time.DateOnly) != day {
			return fmt.Errorf("Chan adjusted history bar %d has invalid date", index)
		}
		row := preparedBar{day: day}
		for fieldIndex, field := range fields {
			value, valueErr := finiteSQLiteNumber(bar[field])
			number, numberOK := value.(float64)
			if valueErr != nil || !numberOK {
				return fmt.Errorf("Chan adjusted history bar %d field %s must be finite", index, field)
			}
			row.values[fieldIndex] = number
		}
		for _, offset := range []int{0, 1, 2, 3, 5, 6, 7, 8, 10} {
			if row.values[offset] <= 0 {
				return fmt.Errorf("Chan adjusted history bar %d contains non-positive prices/factor", index)
			}
		}
		if row.values[4] < 0 || row.values[9] < 0 || row.values[1] < math.Max(row.values[0], row.values[3]) || row.values[2] > math.Min(row.values[0], row.values[3]) || row.values[1] < row.values[2] || row.values[6] < math.Max(row.values[5], row.values[8]) || row.values[7] > math.Min(row.values[5], row.values[8]) || row.values[6] < row.values[7] {
			return fmt.Errorf("Chan adjusted history bar %d has invalid OHLCV", index)
		}
		for priceIndex := 0; priceIndex < 4; priceIndex++ {
			expected := row.values[priceIndex+5] * row.values[10]
			if math.Abs(row.values[priceIndex]-expected) > 1e-6*math.Max(1, row.values[priceIndex]) {
				return fmt.Errorf("Chan adjusted history bar %d has mismatched adjustment factor", index)
			}
		}
		prepared = append(prepared, row)
	}
	if len(prepared) == 0 {
		return errors.New("Chan adjusted history contains no bars")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO chan_adjusted_history(symbol,timeframe,adjustment_type,provider,bar_date,open,high,low,close,volume,raw_open,raw_high,raw_low,raw_close,raw_volume,adjustment_factor,fetched_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(symbol,timeframe,adjustment_type,provider,bar_date) DO UPDATE SET
open=excluded.open,high=excluded.high,low=excluded.low,close=excluded.close,volume=excluded.volume,
raw_open=excluded.raw_open,raw_high=excluded.raw_high,raw_low=excluded.raw_low,raw_close=excluded.raw_close,raw_volume=excluded.raw_volume,
adjustment_factor=excluded.adjustment_factor,fetched_at=excluded.fetched_at
WHERE excluded.fetched_at >= chan_adjusted_history.fetched_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, bar := range prepared {
		args := []any{symbol, timeframe, adjustmentType, providerName, bar.day}
		for _, value := range bar.values {
			args = append(args, value)
		}
		args = append(args, stamp)
		if _, err := stmt.Exec(args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLite) LatestChanAdjustedHistory(symbol, timeframe, adjustmentType, providerName string, limit int) (map[string]any, error) {
	if limit < 1 || limit > 100000 {
		limit = 1500
	}
	rows, err := s.db.Query(`SELECT bar_date,open,high,low,close,volume,raw_open,raw_high,raw_low,raw_close,raw_volume,adjustment_factor,fetched_at
FROM (SELECT bar_date,open,high,low,close,volume,raw_open,raw_high,raw_low,raw_close,raw_volume,adjustment_factor,fetched_at
FROM chan_adjusted_history WHERE symbol=? AND timeframe=? AND adjustment_type=? AND provider=? ORDER BY bar_date DESC LIMIT ?) ORDER BY bar_date`,
		strings.ToUpper(strings.TrimSpace(symbol)), strings.TrimSpace(timeframe), strings.TrimSpace(adjustmentType), strings.TrimSpace(providerName), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bars := []map[string]any{}
	latestFetched := ""
	for rows.Next() {
		var day, fetched string
		values := make([]sql.NullFloat64, 11)
		args := []any{&day}
		for index := range values {
			args = append(args, &values[index])
		}
		args = append(args, &fetched)
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		bar := map[string]any{"date": day}
		for index, field := range []string{"open", "high", "low", "close", "volume", "raw_open", "raw_high", "raw_low", "raw_close", "raw_volume", "adjustment_factor"} {
			bar[field] = nullableFloat(values[index])
		}
		bar["fetched_at"] = fetched
		bars = append(bars, bar)
		if fetched > latestFetched {
			latestFetched = fetched
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"symbol": strings.ToUpper(strings.TrimSpace(symbol)), "timeframe": timeframe, "adjustment_type": adjustmentType, "provider": providerName, "bars": bars, "fetched_at": nilIfEmpty(latestFetched), "source": "sqlite"}, nil
}

// WriteHistory transactionally inserts or replaces daily bars in the shared
// Python-compatible price_history table. Nullable OHLCV values are preserved;
// non-finite numbers and malformed dates are rejected before any write begins.
func (s *SQLite) WriteHistory(symbol string, bars []map[string]any, fetchedAt time.Time) error {
	if strings.TrimSpace(symbol) == "" {
		return errors.New("history symbol must not be empty")
	}
	if fetchedAt.IsZero() {
		return errors.New("history fetched_at must not be zero")
	}
	stamp := storedTimestamp(fetchedAt)
	type preparedBar struct {
		day                            string
		open, high, low, close, volume any
	}
	prepared := make([]preparedBar, 0, len(bars))
	for index, bar := range bars {
		rawDate, ok := bar["date"].(string)
		if !ok {
			return fmt.Errorf("history bar %d date must be YYYY-MM-DD", index)
		}
		parsed, err := time.Parse(time.DateOnly, rawDate)
		if err != nil || parsed.Format(time.DateOnly) != rawDate {
			return fmt.Errorf("history bar %d has invalid date %q", index, rawDate)
		}
		values := make([]any, 5)
		for valueIndex, field := range []string{"open", "high", "low", "close", "volume"} {
			value, err := finiteSQLiteNumber(bar[field])
			if err != nil {
				return fmt.Errorf("history bar %d field %s: %w", index, field, err)
			}
			values[valueIndex] = value
		}
		// A candle without a usable close is not safe to write. Yahoo can
		// occasionally return a partially empty chart even when it reports
		// success; dropping that row also keeps it from replacing a cached close.
		closeValue, closeOK := values[3].(float64)
		if !closeOK || closeValue <= 0 {
			continue
		}
		openValue, openOK := optionalHistoryNumber(values[0])
		highValue, highOK := optionalHistoryNumber(values[1])
		lowValue, lowOK := optionalHistoryNumber(values[2])
		volumeValue, volumeOK := optionalHistoryNumber(values[4])
		if !openOK || !highOK || !lowOK || !volumeOK ||
			openValue != nil && *openValue <= 0 ||
			highValue != nil && *highValue <= 0 ||
			lowValue != nil && *lowValue <= 0 ||
			volumeValue != nil && *volumeValue < 0 ||
			highValue != nil && lowValue != nil && *highValue < *lowValue ||
			highValue != nil && *highValue < closeValue ||
			lowValue != nil && *lowValue > closeValue ||
			openValue != nil && highValue != nil && *highValue < *openValue ||
			openValue != nil && lowValue != nil && *lowValue > *openValue {
			continue
		}
		prepared = append(prepared, preparedBar{day: rawDate, open: values[0], high: values[1], low: values[2], close: values[3], volume: values[4]})
	}
	if len(prepared) == 0 {
		return errors.New("history response contains no usable closing prices")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO price_history(symbol, bar_date, open, high, low, close, volume, fetched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(symbol, bar_date) DO UPDATE SET
open=COALESCE(excluded.open, price_history.open),
high=COALESCE(excluded.high, price_history.high),
low=COALESCE(excluded.low, price_history.low),
close=COALESCE(excluded.close, price_history.close),
volume=COALESCE(excluded.volume, price_history.volume), fetched_at=excluded.fetched_at
WHERE excluded.fetched_at >= price_history.fetched_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, bar := range prepared {
		if _, err := stmt.Exec(symbol, bar.day, bar.open, bar.high, bar.low, bar.close, bar.volume, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WriteIntradayHistory transactionally stores timestamped intraday bars.
// Interval is part of the key so future intraday resolutions can coexist.
func (s *SQLite) WriteIntradayHistory(symbol, interval string, bars []map[string]any, fetchedAt time.Time) error {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	interval = strings.TrimSpace(interval)
	if symbol == "" {
		return errors.New("intraday history symbol must not be empty")
	}
	if interval != "15m" {
		return fmt.Errorf("unsupported intraday interval %q", interval)
	}
	if fetchedAt.IsZero() {
		return errors.New("intraday history fetched_at must not be zero")
	}
	stamp := storedTimestamp(fetchedAt)
	type preparedBar struct {
		at                             string
		open, high, low, close, volume any
	}
	prepared := make([]preparedBar, 0, len(bars))
	for index, bar := range bars {
		rawTime, ok := bar["date"].(string)
		if !ok {
			return fmt.Errorf("intraday history bar %d date must be YYYY-MM-DD HH:MM", index)
		}
		parsed, err := time.ParseInLocation("2006-01-02 15:04", rawTime, time.UTC)
		if err != nil || parsed.Format("2006-01-02 15:04") != rawTime {
			return fmt.Errorf("intraday history bar %d has invalid time %q", index, rawTime)
		}
		values := make([]any, 5)
		for valueIndex, field := range []string{"open", "high", "low", "close", "volume"} {
			value, err := finiteSQLiteNumber(bar[field])
			if err != nil {
				return fmt.Errorf("intraday history bar %d field %s: %w", index, field, err)
			}
			values[valueIndex] = value
		}
		closeValue, closeOK := values[3].(float64)
		if !closeOK || closeValue <= 0 {
			continue
		}
		openValue, openOK := optionalHistoryNumber(values[0])
		highValue, highOK := optionalHistoryNumber(values[1])
		lowValue, lowOK := optionalHistoryNumber(values[2])
		volumeValue, volumeOK := optionalHistoryNumber(values[4])
		if !openOK || !highOK || !lowOK || !volumeOK || openValue != nil && *openValue <= 0 || highValue != nil && *highValue <= 0 || lowValue != nil && *lowValue <= 0 || volumeValue != nil && *volumeValue < 0 || highValue != nil && lowValue != nil && *highValue < *lowValue || highValue != nil && *highValue < closeValue || lowValue != nil && *lowValue > closeValue || openValue != nil && highValue != nil && *highValue < *openValue || openValue != nil && lowValue != nil && *lowValue > *openValue {
			continue
		}
		prepared = append(prepared, preparedBar{at: rawTime, open: values[0], high: values[1], low: values[2], close: values[3], volume: values[4]})
	}
	if len(prepared) == 0 {
		return errors.New("intraday history response contains no usable closing prices")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO intraday_history(symbol, interval, bar_time, open, high, low, close, volume, fetched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(symbol, interval, bar_time) DO UPDATE SET
open=COALESCE(excluded.open, intraday_history.open), high=COALESCE(excluded.high, intraday_history.high),
low=COALESCE(excluded.low, intraday_history.low), close=COALESCE(excluded.close, intraday_history.close),
volume=COALESCE(excluded.volume, intraday_history.volume), fetched_at=excluded.fetched_at
WHERE excluded.fetched_at >= intraday_history.fetched_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, bar := range prepared {
		if _, err := stmt.Exec(symbol, interval, bar.at, bar.open, bar.high, bar.low, bar.close, bar.volume, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestIntradayHistory returns the most recent stored intraday bars in ascending order.
func (s *SQLite) LatestIntradayHistory(symbol, interval string, limit int) (map[string]any, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if interval != "15m" {
		return nil, fmt.Errorf("unsupported intraday interval %q", interval)
	}
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	rows, err := s.db.Query(`SELECT bar_time, open, high, low, close, volume, fetched_at FROM (
SELECT bar_time, open, high, low, close, volume, fetched_at FROM intraday_history
WHERE symbol=? AND interval=? AND close IS NOT NULL AND close > 0 ORDER BY bar_time DESC LIMIT ?) ORDER BY bar_time`, symbol, interval, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bars := []map[string]any{}
	latestFetched := ""
	for rows.Next() {
		var at, fetched string
		var open, high, low, close, volume sql.NullFloat64
		if err := rows.Scan(&at, &open, &high, &low, &close, &volume, &fetched); err != nil {
			return nil, err
		}
		bars = append(bars, map[string]any{"date": at, "open": nullableFloat(open), "high": nullableFloat(high), "low": nullableFloat(low), "close": nullableFloat(close), "volume": nullableFloat(volume)})
		if fetched > latestFetched {
			latestFetched = fetched
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"bars": bars, "fetched_at": nilIfEmpty(latestFetched), "source": "sqlite"}, nil
}

func optionalHistoryNumber(value any) (*float64, bool) {
	if value == nil {
		return nil, true
	}
	number, ok := value.(float64)
	if !ok {
		return nil, false
	}
	return &number, true
}

// finiteSQLiteNumber preserves NULLs and normalizes supported numeric inputs
// to float64 so SQLite never receives NaN or infinity from a JSON-shaped bar.
func finiteSQLiteNumber(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	var number float64
	switch item := value.(type) {
	case float64:
		number = item
	case float32:
		number = float64(item)
	case int:
		number = float64(item)
	case int8:
		number = float64(item)
	case int16:
		number = float64(item)
	case int32:
		number = float64(item)
	case int64:
		number = float64(item)
	case uint:
		number = float64(item)
	case uint8:
		number = float64(item)
	case uint16:
		number = float64(item)
	case uint32:
		number = float64(item)
	case uint64:
		number = float64(item)
	case json.Number:
		parsed, err := item.Float64()
		if err != nil {
			return nil, fmt.Errorf("must be a finite number")
		}
		number = parsed
	default:
		return nil, fmt.Errorf("must be a finite number or null")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, errors.New("must be a finite number")
	}
	return number, nil
}

func (s *SQLite) LatestHistory(symbol string, limit int) (map[string]any, error) {
	return s.latestHistory(symbol, limit, false)
}

// LatestHistoryWithTimestamps preserves the fetch time of each individual
// candle. The normal history API keeps its established OHLCV shape.
func (s *SQLite) LatestHistoryWithTimestamps(symbol string, limit int) (map[string]any, error) {
	return s.latestHistory(symbol, limit, true)
}

func (s *SQLite) latestHistory(symbol string, limit int, includeTimestamps bool) (map[string]any, error) {
	if limit <= 0 || limit > 3000 {
		limit = 1500
	}
	rows, err := s.db.Query(`SELECT bar_date, open, high, low, close, volume, fetched_at FROM (
 SELECT bar_date, open, high, low, close, volume, fetched_at
 FROM price_history WHERE symbol=? AND close IS NOT NULL AND close > 0 ORDER BY bar_date DESC LIMIT ?
) ORDER BY bar_date`, symbol, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bars := []map[string]any{}
	latestFetched := ""
	for rows.Next() {
		var day, fetched string
		var open, high, low, close, volume sql.NullFloat64
		if err := rows.Scan(&day, &open, &high, &low, &close, &volume, &fetched); err != nil {
			return nil, err
		}
		bars = append(bars, map[string]any{"date": day, "open": nullableFloat(open), "high": nullableFloat(high), "low": nullableFloat(low), "close": nullableFloat(close), "volume": nullableFloat(volume)})
		if includeTimestamps {
			bars[len(bars)-1]["captured_at"] = fetched
		}
		if fetched > latestFetched {
			latestFetched = fetched
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"bars": bars, "fetched_at": nilIfEmpty(latestFetched), "source": "sqlite"}, nil
}

func allowedPayloadTable(table string) bool {
	switch table {
	case "price_extremes", "beta_snapshots", "earnings_snapshots":
		return true
	default:
		return false
	}
}

// WritePayload stores raw payload data in one of the established snapshot
// tables. Wrappers such as {extremes: ..., fetched_at: ...} are added only by
// LatestPayload, matching the existing Python reader contract.
func (s *SQLite) WritePayload(table, symbol string, payload map[string]any, fetchedAt time.Time) error {
	if !allowedPayloadTable(table) {
		return fmt.Errorf("unsupported payload table %q", table)
	}
	if strings.TrimSpace(symbol) == "" {
		return errors.New("payload symbol must not be empty")
	}
	if fetchedAt.IsZero() {
		return errors.New("payload fetched_at must not be zero")
	}
	if payload == nil {
		return errors.New("payload must not be nil")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO `+table+`(symbol, payload, fetched_at) VALUES (?, ?, ?)
ON CONFLICT(symbol) DO UPDATE SET payload=excluded.payload, fetched_at=excluded.fetched_at`,
		symbol, string(encoded), storedTimestamp(fetchedAt))
	return err
}

func (s *SQLite) LatestPayload(table, symbol string) (map[string]any, error) {
	if !allowedPayloadTable(table) {
		return nil, fmt.Errorf("unsupported payload table %q", table)
	}
	var payload, fetched string
	err := s.db.QueryRow("SELECT payload, fetched_at FROM "+table+" WHERE symbol=?", symbol).Scan(&payload, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return map[string]any{}, nil
	}
	result, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	// Python's cache readers expose extremes and beta under a table-specific
	// wrapper, while earnings keeps its dates at the top level. Preserve that
	// shape so the Go raw and levels endpoints can consume Python snapshots.
	if table == "price_extremes" {
		return map[string]any{"extremes": result, "fetched_at": fetched}, nil
	}
	if table == "beta_snapshots" {
		return map[string]any{"beta": result, "fetched_at": fetched}, nil
	}
	if _, ok := result["dates"].([]any); !ok {
		return map[string]any{}, nil
	}
	result["fetched_at"] = fetched
	return result, nil
}

// GetAnalysisCache reads one JSON-encoded shared analysis result. The bool
// distinguishes a cache miss from a stored JSON null payload, matching the
// Python cache wrapper's None/missing distinction without exposing SQLite rows.
func (s *SQLite) GetAnalysisCache(cacheKey string) (any, bool, error) {
	payload, _, found, err := s.GetAnalysisCacheEntry(cacheKey)
	return payload, found, err
}

// GetAnalysisCacheEntry is the timestamped form used by TTL-sensitive callers.
// A corrupt entry is treated as a miss, just like the Python cache wrapper.
func (s *SQLite) GetAnalysisCacheEntry(cacheKey string) (any, time.Time, bool, error) {
	var encoded, createdAt string
	err := s.db.QueryRow(`SELECT payload, created_at FROM api_analysis_cache WHERE cache_key=?`, cacheKey).Scan(&encoded, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	var payload any
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return nil, time.Time{}, false, nil
	}
	stamp, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, time.Time{}, false, nil
	}
	return payload, stamp, true, nil
}

// PutAnalysisCache stores one JSON-encoded analysis result and bounds the
// shared table to maxEntries newest entries. The insert and pruning happen in
// one SQLite write transaction so concurrent workers cannot observe a partial
// update or grow the table beyond its configured bound.
func (s *SQLite) PutAnalysisCache(cacheKey string, payload any, maxEntries int) error {
	if maxEntries < 1 {
		maxEntries = 1
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode analysis cache %q: %w", cacheKey, err)
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	createdAt := storedTimestamp(time.Now().UTC())
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO api_analysis_cache(cache_key, payload, created_at)
VALUES (?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET payload=excluded.payload, created_at=excluded.created_at`, cacheKey, string(encoded), createdAt); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM api_analysis_cache
WHERE cache_key IN (
 SELECT cache_key FROM api_analysis_cache
 ORDER BY created_at DESC, cache_key DESC
 LIMIT -1 OFFSET ?
)`, maxEntries); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// GetChanAnalysisCache reads one durable Chan analysis result. The table is
// separate from api_analysis_cache because Chan payloads are substantially
// larger and use a different retention limit.
func (s *SQLite) GetChanAnalysisCache(cacheKey string) ([]byte, bool, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload FROM chan_analysis_cache WHERE cache_key=?`, cacheKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(payload), true, nil
}

// PutChanAnalysisCache stores a complete Chan result and bounds this dedicated
// cache independently from the smaller shared API analysis entries.
func (s *SQLite) PutChanAnalysisCache(cacheKey string, payload []byte, maxEntries int) error {
	if strings.TrimSpace(cacheKey) == "" || !json.Valid(payload) {
		return errors.New("Chan analysis cache key and JSON payload must be valid")
	}
	if maxEntries < 1 {
		maxEntries = 1
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	createdAt := storedTimestamp(time.Now().UTC())
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO chan_analysis_cache(cache_key, payload, created_at)
VALUES (?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET payload=excluded.payload, created_at=excluded.created_at`, cacheKey, string(payload), createdAt); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM chan_analysis_cache
WHERE cache_key IN (
 SELECT cache_key FROM chan_analysis_cache
 ORDER BY created_at DESC, cache_key DESC
 LIMIT -1 OFFSET ?
)`, maxEntries); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *SQLite) StartRefreshRun(symbol string) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO refresh_runs(symbol, started_at, status) VALUES (?, ?, 'running')`, symbol, storedTimestamp(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *SQLite) FinishRefreshRun(runID int64, status string, rowsWritten int, errorMessage string) error {
	if status != "success" && status != "failed" {
		return fmt.Errorf("invalid refresh run status %q", status)
	}
	_, err := s.db.Exec(`UPDATE refresh_runs SET finished_at=?, status=?, rows_written=?, error_message=? WHERE id=?`,
		storedTimestamp(time.Now().UTC()), status, rowsWritten, nullableMessage(errorMessage), runID)
	return err
}

func (s *SQLite) LatestRefreshRun(symbol string) (map[string]any, bool, error) {
	var id int64
	var startedAt, status string
	var finishedAt, errorMessage sql.NullString
	var rowsWritten int64
	err := s.db.QueryRow(`SELECT id, symbol, started_at, finished_at, status, rows_written, error_message
FROM refresh_runs WHERE symbol=? ORDER BY started_at DESC, id DESC LIMIT 1`, symbol).
		Scan(&id, &symbol, &startedAt, &finishedAt, &status, &rowsWritten, &errorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	result := map[string]any{"id": id, "symbol": symbol, "started_at": startedAt, "status": status, "rows_written": rowsWritten}
	if finishedAt.Valid {
		result["finished_at"] = finishedAt.String
	} else {
		result["finished_at"] = nil
	}
	if errorMessage.Valid {
		result["error_message"] = errorMessage.String
	} else {
		result["error_message"] = nil
	}
	return result, true, nil
}

func nullableMessage(message string) any {
	if message == "" {
		return nil
	}
	return message
}

func (s *SQLite) Cleanup(retentionDays int) (map[string]int64, error) {
	cutoff := pythonISOTimestamp(time.Now().UTC().AddDate(0, 0, -retentionDays))
	return s.cleanupBefore(cutoff)
}

func (s *SQLite) cleanupBefore(cutoff string) (map[string]int64, error) {
	deleted := map[string]int64{"quotes": 0, "options": 0, "runs": 0, "history": 0, "chan_adjusted_history": 0, "intraday_history": 0, "extremes": 0}
	// Python performs retention cleanup within one connection context, committing
	// only when all tables and latest-batch index cleanup succeed. Keep that same
	// atomicity here so partial failures cannot prune quotes while retaining the
	// corresponding stale option/history data.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, item := range []struct {
		table string
		key   string
		field string
	}{
		{"quote_snapshots", "quotes", "fetched_at"},
		{"option_snapshots", "options", "fetched_at"},
		{"price_history", "history", "fetched_at"},
		{"chan_adjusted_history", "chan_adjusted_history", "fetched_at"},
		{"price_extremes", "extremes", "fetched_at"},
		{"refresh_runs", "runs", "started_at"},
	} {
		for {
			result, execErr := tx.Exec("DELETE FROM "+item.table+" WHERE rowid IN (SELECT rowid FROM "+item.table+" WHERE "+item.field+" < ? LIMIT 5000)", cutoff)
			if execErr != nil {
				return nil, execErr
			}
			count, countErr := result.RowsAffected()
			if countErr != nil {
				return nil, countErr
			}
			deleted[item.key] += count
			if count < 5000 {
				break
			}
		}
	}
	// Yahoo only serves a recent rolling window of 15-minute bars. Retain a
	// little longer than that window while bounding storage independently of
	// the daily-history retention setting.
	intradayCutoff := time.Now().UTC().AddDate(0, 0, -90).Format("2006-01-02 15:04")
	for {
		result, execErr := tx.Exec(`DELETE FROM intraday_history WHERE rowid IN (
SELECT rowid FROM intraday_history WHERE bar_time < ? LIMIT 5000)`, intradayCutoff)
		if execErr != nil {
			return nil, execErr
		}
		count, countErr := result.RowsAffected()
		if countErr != nil {
			return nil, countErr
		}
		deleted["intraday_history"] += count
		if count < 5000 {
			break
		}
	}
	if _, err := tx.Exec(`DELETE FROM option_latest_batches AS latest
WHERE NOT EXISTS (
 SELECT 1 FROM option_snapshots AS current
 WHERE current.symbol=latest.symbol AND current.expiration=latest.expiration AND current.fetched_at=latest.fetched_at
)`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return deleted, nil
}

func (s *SQLite) DatabaseSizeBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(s.path + suffix)
		if err == nil {
			total += info.Size()
		} else if !os.IsNotExist(err) {
			return 0, err
		}
	}
	return total, nil
}

func (s *SQLite) DatabaseLiveBytes() (int64, error) {
	var pageCount, freePages, pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pageCount - freePages) * pageSize, nil
}

func (s *SQLite) AddEvent(event model.Event) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO push_events(symbol, kind, expiration, created_at) VALUES (?, ?, ?, ?)`, event.Symbol, event.Kind, event.Expiration, event.CreatedAt)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Match Python's bounded event log; transactions keep insertion and pruning
	// atomic so reconnect cursors never observe a partially-pruned log.
	if _, err := tx.Exec(`DELETE FROM push_events
WHERE id <= (SELECT COALESCE(MAX(id), 0) - 20000 FROM push_events)`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *SQLite) LatestEventID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM push_events`).Scan(&id)
	return id, err
}

func (s *SQLite) EventsSince(id int64, symbol, expiration string) ([]model.Event, error) {
	rows, err := s.db.Query(`SELECT id, symbol, kind, expiration, created_at FROM push_events
WHERE id>? AND symbol=? AND (
 ?='' OR expiration=? OR expiration IS NULL OR
 (COALESCE(expiration, '')='' AND kind IN ('quote','analytics','fair_value'))
) ORDER BY id LIMIT 100`, id, symbol, expiration, expiration)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Event{}
	for rows.Next() {
		var event model.Event
		var expiration sql.NullString
		if err := rows.Scan(&event.ID, &event.Symbol, &event.Kind, &expiration, &event.CreatedAt); err != nil {
			return nil, err
		}
		if expiration.Valid {
			event.Expiration = expiration.String
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

type flowRow struct {
	contractSymbol string
	contractType   string
	strike         sql.NullFloat64
	lastPrice      sql.NullFloat64
	bid            sql.NullFloat64
	ask            sql.NullFloat64
	volume         sql.NullInt64
}

type flowStrikeAggregate struct {
	volume, buyVolume, sellVolume, unknownVolume             int64
	premium, buyPremium, sellPremium, unknownPremium         float64
	firstSeen, buyFirstSeen, sellFirstSeen, unknownFirstSeen int
}

func (s *SQLite) OptionFlow(symbol, expiration string) (map[string]any, error) {
	result := map[string]any{
		"symbol": symbol, "expiration": expiration, "available": false, "source": "sqlite",
		"reason": "暂无该期限的期权快照", "current_fetched_at": nil, "previous_fetched_at": nil,
		"interval_seconds": nil, "reset_count": 0,
		"call": emptyFlowSide(), "put": emptyFlowSide(),
		"signal": map[string]any{"label": "等待下一快照", "class_name": "waiting", "detail": "暂无该期限的期权快照"},
	}
	rows, err := s.db.Query(`SELECT fetched_at FROM option_snapshots
WHERE symbol=? AND expiration=? GROUP BY fetched_at ORDER BY fetched_at DESC LIMIT 2`, symbol, expiration)
	if err != nil {
		return nil, err
	}
	var batches []string
	for rows.Next() {
		var fetched string
		if err := rows.Scan(&fetched); err != nil {
			rows.Close()
			return nil, err
		}
		batches = append(batches, fetched)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return result, nil
	}
	result["current_fetched_at"] = batches[0]
	if len(batches) < 2 {
		result["reason"] = "等待下一份相邻快照"
		result["signal"] = map[string]any{"label": "等待下一快照", "class_name": "waiting", "detail": "等待下一份相邻快照"}
		return result, nil
	}
	result["previous_fetched_at"] = batches[1]
	current, err := s.flowRows(symbol, expiration, batches[0])
	if err != nil {
		return nil, err
	}
	previous, err := s.flowRows(symbol, expiration, batches[1])
	if err != nil {
		return nil, err
	}
	type aggregate struct {
		buyVolume, sellVolume, unknownVolume          int64
		buyPremium, sellPremium, unknownPremium       float64
		buyContracts, sellContracts, unknownContracts int
	}
	groups := map[string]*aggregate{"call": {}, "put": {}}
	byStrike := map[string]map[float64]*flowStrikeAggregate{"call": {}, "put": {}}
	previousByContract := make(map[string]flowRow, len(previous))
	for _, row := range previous {
		previousByContract[row.contractSymbol] = row
	}
	resetCount := 0
	for sequence, row := range current {
		kind := strings.ToLower(row.contractType)
		group := groups[kind]
		if group == nil {
			continue
		}
		currentVolume := nullableIntValue(row.volume)
		previousVolume := nullableIntValue(previousByContract[row.contractSymbol].volume)
		delta := currentVolume - previousVolume
		if delta < 0 {
			resetCount++
			delta = currentVolume
		}
		if delta <= 0 {
			continue
		}
		direction := classifyFlow(row)
		last := nullableFloatValue(row.lastPrice)
		premium := float64(delta) * maxFloat(last, 0) * 100
		switch direction {
		case "buy":
			group.buyVolume += delta
			group.buyPremium += premium
			group.buyContracts++
		case "sell":
			group.sellVolume += delta
			group.sellPremium += premium
			group.sellContracts++
		default:
			group.unknownVolume += delta
			group.unknownPremium += premium
			group.unknownContracts++
		}
		strike, ok := nullableFloatValueOK(row.strike)
		if !ok {
			continue
		}
		if byStrike[kind][strike] == nil {
			byStrike[kind][strike] = &flowStrikeAggregate{firstSeen: sequence + 1}
		}
		bucket := byStrike[kind][strike]
		bucket.volume += delta
		bucket.premium += premium
		switch direction {
		case "buy":
			if bucket.buyFirstSeen == 0 {
				bucket.buyFirstSeen = sequence + 1
			}
			bucket.buyVolume += delta
			bucket.buyPremium += premium
		case "sell":
			if bucket.sellFirstSeen == 0 {
				bucket.sellFirstSeen = sequence + 1
			}
			bucket.sellVolume += delta
			bucket.sellPremium += premium
		default:
			if bucket.unknownFirstSeen == 0 {
				bucket.unknownFirstSeen = sequence + 1
			}
			bucket.unknownVolume += delta
			bucket.unknownPremium += premium
		}
	}
	for kind, group := range groups {
		groupMap := map[string]any{
			"buy_volume": group.buyVolume, "sell_volume": group.sellVolume, "unknown_volume": group.unknownVolume,
			"buy_premium": roundMoney(group.buyPremium), "sell_premium": roundMoney(group.sellPremium), "unknown_premium": roundMoney(group.unknownPremium),
			"buy_contracts": group.buyContracts, "sell_contracts": group.sellContracts, "unknown_contracts": group.unknownContracts,
			"net_volume": group.buyVolume - group.sellVolume, "net_premium": roundMoney(group.buyPremium - group.sellPremium),
		}
		groupMap["top_strikes"], groupMap["concentration"] = flowStrikeLists(byStrike[kind])
		result[kind] = groupMap
	}
	currentTime, currentErr := parseStoredTime(batches[0])
	previousTime, previousErr := parseStoredTime(batches[1])
	if currentErr == nil && previousErr == nil {
		result["interval_seconds"] = maxInt64(0, int64(currentTime.Sub(previousTime).Round(time.Second)/time.Second))
	}
	result["reset_count"] = resetCount
	result["available"] = true
	result["reason"] = "相邻快照成交量增量，买卖方向按成交价接近 Bid/Ask 估算"
	call, _ := result["call"].(map[string]any)
	put, _ := result["put"].(map[string]any)
	bullish := numberFromMap(call, "buy_premium") + numberFromMap(put, "sell_premium")
	bearish := numberFromMap(call, "sell_premium") + numberFromMap(put, "buy_premium")
	if bullish == 0 && bearish == 0 {
		bullish = numberFromMap(call, "buy_volume") + numberFromMap(put, "sell_volume")
		bearish = numberFromMap(call, "sell_volume") + numberFromMap(put, "buy_volume")
	}
	signal := map[string]any{"label": "多空分歧", "class_name": "neutral", "detail": "Call/Put 新增流向接近"}
	if bullish == 0 && bearish == 0 {
		signal = map[string]any{"label": "暂无新增流向", "class_name": "neutral", "detail": "相邻快照没有新增成交量"}
	} else if bullish >= bearish*1.15 {
		signal = map[string]any{"label": "偏多流向", "class_name": "bullish", "detail": "Call 买入与 Put 卖出占优"}
	} else if bearish >= bullish*1.15 {
		signal = map[string]any{"label": "偏空流向", "class_name": "bearish", "detail": "Call 卖出与 Put 买入占优"}
	}
	result["signal"] = signal
	if resetCount > 0 {
		result["warning"] = fmt.Sprintf("%d 个合约成交量出现回落，按当前量作为重置后增量估算", resetCount)
	}
	return result, nil
}

func emptyFlowSide() map[string]any {
	return map[string]any{"buy_volume": int64(0), "sell_volume": int64(0), "unknown_volume": int64(0),
		"buy_premium": 0.0, "sell_premium": 0.0, "unknown_premium": 0.0,
		"buy_contracts": 0, "sell_contracts": 0, "unknown_contracts": 0,
		"net_volume": int64(0), "net_premium": 0.0, "top_strikes": []any{}, "concentration": []any{}}
}

func (s *SQLite) flowRows(symbol, expiration, fetched string) ([]flowRow, error) {
	rows, err := s.db.Query(`SELECT contract_symbol, contract_type, strike, last_price, bid, ask, volume
FROM option_snapshots WHERE symbol=? AND expiration=? AND fetched_at=? ORDER BY rowid`, symbol, expiration, fetched)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []flowRow{}
	for rows.Next() {
		var row flowRow
		if err := rows.Scan(&row.contractSymbol, &row.contractType, &row.strike, &row.lastPrice, &row.bid, &row.ask, &row.volume); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func classifyFlow(row flowRow) string {
	last, lok := nullableFloatValueOK(row.lastPrice)
	bid, bok := nullableFloatValueOK(row.bid)
	ask, aok := nullableFloatValueOK(row.ask)
	if !lok || !bok || !aok || bid <= 0 || ask < bid || last <= 0 {
		return "unknown"
	}
	spread := ask - bid
	if spread <= 0 {
		return "unknown"
	}
	edge := spread * 0.25
	if last >= ask-edge {
		return "buy"
	}
	if last <= bid+edge {
		return "sell"
	}
	return "unknown"
}

func flowStrikeLists(values map[float64]*flowStrikeAggregate) ([]any, []any) {
	type strikeItem struct {
		strike    float64
		value     *flowStrikeAggregate
		firstSeen int
	}
	type directionItem struct {
		strike    float64
		direction string
		volume    int64
		premium   float64
		firstSeen int
	}
	strikes := make([]strikeItem, 0, len(values))
	topCandidates := make([]directionItem, 0, len(values)*3)
	for strike, value := range values {
		strikes = append(strikes, strikeItem{strike: strike, value: value, firstSeen: value.firstSeen})
		for _, candidate := range []directionItem{
			{strike: strike, direction: "buy", volume: value.buyVolume, premium: value.buyPremium, firstSeen: value.buyFirstSeen},
			{strike: strike, direction: "sell", volume: value.sellVolume, premium: value.sellPremium, firstSeen: value.sellFirstSeen},
			{strike: strike, direction: "unknown", volume: value.unknownVolume, premium: value.unknownPremium, firstSeen: value.unknownFirstSeen},
		} {
			if candidate.volume > 0 {
				topCandidates = append(topCandidates, candidate)
			}
		}
	}
	sort.Slice(strikes, func(i, j int) bool {
		if strikes[i].value.volume != strikes[j].value.volume {
			return strikes[i].value.volume > strikes[j].value.volume
		}
		return strikes[i].firstSeen < strikes[j].firstSeen
	})
	sort.Slice(topCandidates, func(i, j int) bool {
		if topCandidates[i].volume != topCandidates[j].volume {
			return topCandidates[i].volume > topCandidates[j].volume
		}
		return topCandidates[i].firstSeen < topCandidates[j].firstSeen
	})
	top := make([]any, 0, minInt(len(topCandidates), 5))
	for _, current := range topCandidates[:minInt(len(topCandidates), 5)] {
		top = append(top, map[string]any{"strike": current.strike, "direction": current.direction, "volume": current.volume, "premium": roundMoney(current.premium)})
	}
	concentration := make([]any, 0, minInt(len(strikes), 5))
	for _, current := range strikes[:minInt(len(strikes), 5)] {
		direction := "unknown"
		if current.value.buyVolume > current.value.sellVolume {
			direction = "buy"
		} else if current.value.sellVolume > current.value.buyVolume {
			direction = "sell"
		}
		concentration = append(concentration, map[string]any{
			"strike": current.strike, "volume": current.value.volume,
			"buy_volume": current.value.buyVolume, "sell_volume": current.value.sellVolume,
			"unknown_volume": current.value.unknownVolume, "premium": roundMoney(current.value.premium),
			"dominant_direction": direction,
		})
	}
	return top, concentration
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func nullableFloatValue(value sql.NullFloat64) float64 {
	if !value.Valid {
		return 0
	}
	return value.Float64
}

func nullableFloatValueOK(value sql.NullFloat64) (float64, bool) {
	return value.Float64, value.Valid
}

func nullableIntValue(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func maxFloat(value, floor float64) float64 {
	if value < floor {
		return floor
	}
	return value
}

func roundMoney(value float64) float64 {
	return math.Round(value*100) / 100
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func parseStoredTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed, nil
	}
	return time.Parse("2006-01-02T15:04:05.999999", value)
}

// storedTimestamp keeps text ordering compatible with Python's microsecond ISO timestamps.
func storedTimestamp(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

// pythonISOTimestamp matches datetime.isoformat() for UTC retention cutoffs.
// Python cleanup compares timestamp text directly, preserving microseconds.
func pythonISOTimestamp(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000+00:00")
}

func numberFromMap(values map[string]any, key string) float64 {
	value, ok := values[key]
	if !ok {
		return 0
	}
	switch number := value.(type) {
	case int:
		return float64(number)
	case int64:
		return float64(number)
	case float64:
		return number
	default:
		return 0
	}
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullableAnyFloat(value sql.NullFloat64) any {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func decodeJSONColumn(value sql.NullString) any {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value.String), &decoded); err != nil {
		return nil
	}
	return decoded
}

func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func valueOrNil[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func jsonOrNil(value any) any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return string(encoded)
}

func jsonOrEmptyObject(value any) string {
	if isEmptyJSONValue(value) {
		return "{}"
	}
	encoded := jsonOrNil(value)
	if text, ok := encoded.(string); ok {
		return text
	}
	return "{}"
}

func valueOrNilFloatAny(value any) any {
	if value == nil {
		return nil
	}
	if pointer, ok := value.(*float64); ok {
		if pointer == nil {
			return nil
		}
		return *pointer
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableBoolInt(value, known bool) any {
	if !known && !value {
		return nil
	}
	return boolInt(value)
}
