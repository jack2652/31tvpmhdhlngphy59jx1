package store

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func quoteTestPtr(value float64) *float64 { return &value }

func TestQuoteValuationMetadataRoundTripsSQLite(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "valuation-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	at := time.Now().UTC()
	quality := 67.5
	normalizedSource := "historical EPS median"
	marketCapQuality := "consistent"
	shareholderYield := .024
	quote := model.Quote{
		Symbol: "QQQ", Provider: "test", Price: quoteTestPtr(50), FetchedAt: &at,
		FairValue: 60.0, FairValueSource: "valuation_go_v4", FairValueModel: "owner_earnings",
		FairValueMethods:   map[string]any{"owner_earnings": 60.0, "forward_eps_pe": 75.0},
		FairValueDefensive: map[string]any{"value": 48.0}, FairValueOptimistic: map[string]any{"value": 72.0},
		FairValueDataQualityScore: &quality, FairValueNormalizedEPSSource: &normalizedSource,
		FairValueMarketCapDataQuality: &marketCapQuality, FairValueShareholderReturnYield: &shareholderYield,
		FairValueRegimeSignals: map[string]any{"vix": 18.0}, Sessions: map[string]any{},
	}
	if err := repository.SaveQuote(quote); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found {
		t.Fatalf("LatestQuote found=%v err=%v", found, err)
	}
	if methods, ok := stored.FairValueMethods.(map[string]any); !ok || methods["owner_earnings"] != 60.0 || methods["forward_eps_pe"] != 75.0 {
		t.Fatalf("valuation methods failed round-trip: %#v", stored.FairValueMethods)
	}
	if scenarios, ok := stored.FairValueDefensive.(map[string]any); !ok || scenarios["value"] != 48.0 {
		t.Fatalf("defensive scenario failed round-trip: %#v", stored.FairValueDefensive)
	}
	if stored.FairValueDataQualityScore == nil || *stored.FairValueDataQualityScore != quality || stored.FairValueNormalizedEPSSource == nil || *stored.FairValueNormalizedEPSSource != normalizedSource || stored.FairValueMarketCapDataQuality == nil || *stored.FairValueMarketCapDataQuality != marketCapQuality || stored.FairValueShareholderReturnYield == nil || *stored.FairValueShareholderReturnYield != shareholderYield {
		t.Fatalf("valuation source metadata failed round-trip: %#v", stored)
	}
}

func TestLatestQuoteMatchesPythonFirstRowWhenFetchTimesTie(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "quote-tie.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, price, provider, sessions_json)
VALUES ('QQQ', ?, 100, 'older', '{"session":"old"}'), ('QQQ', ?, 101, 'newer', '{"session":"new"}')`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 100 || quote.Provider != "older" || quote.Sessions["session"] != "old" {
		t.Fatalf("latest quote should match Python fetched_at-only ordering: quote=%#v found=%v err=%v", quote, found, err)
	}
}

func TestLatestQuoteMatchesPythonLexicalOrderingForOffsetTimestamps(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "quote-offset-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	// Python selects ORDER BY fetched_at DESC, so the Z-form sorts after
	// the equivalent +00:00 form even though both denote the same instant.
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, price, provider)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 100, 'zulu'), ('QQQ', '2030-01-18T15:00:00+00:00', 101, 'offset')`); err != nil {
		t.Fatal(err)
	}
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 100 || quote.Provider != "zulu" {
		t.Fatalf("latest quote must match Python text ordering for timezone variants: quote=%#v found=%v err=%v", quote, found, err)
	}
}

func TestLatestQuoteRetainsPythonSelectStarStorageFields(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "quote-storage-fields.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rawJSON := `{"provider":"python","opaque":[1,2]}`
	methodsJSON := "{ \"owner_earnings\" : 120.5 }"
	historicalJSON := "{\"percentiles\":[10,50,90]}"
	result, err := repository.db.Exec(`INSERT INTO quote_snapshots
(symbol, fetched_at, price, provider, raw_json, fair_value_methods_json, fair_value_historical_valuation_percentiles_json)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 100, 'python', ?, ?, ?)`, rawJSON, methodsJSON, historicalJSON)
	if err != nil {
		t.Fatal(err)
	}
	wantID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found {
		t.Fatalf("LatestQuote found=%v err=%v", found, err)
	}
	if quote.SnapshotID != wantID || quote.RawJSON == nil || *quote.RawJSON != rawJSON || quote.FairValueMethodsJSON == nil || *quote.FairValueMethodsJSON != methodsJSON || quote.FairValueHistoricalValuationJSON == nil || *quote.FairValueHistoricalValuationJSON != historicalJSON {
		t.Fatalf("SQLite select-star metadata changed: %#v", quote)
	}
}

func TestSavedQuoteReturnsPythonResponseStorageMetadata(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "saved-quote-storage-fields.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	at := time.Now().UTC()
	quote, err := repository.SaveQuotePreservingFairValue(model.Quote{
		Symbol: "QQQ", Provider: "test", FetchedAt: &at,
		FairValueMethods:                map[string]any{"owner_earnings": 120.5},
		FairValueHistoricalValuationPct: map[string]any{"percentiles": []any{10.0, 50.0, 90.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if quote.SnapshotID <= 0 || quote.RawJSON != nil || quote.FairValueMethodsJSON == nil || *quote.FairValueMethodsJSON != `{"owner_earnings":120.5}` || quote.FairValueHistoricalValuationJSON == nil || *quote.FairValueHistoricalValuationJSON != `{"percentiles":[10,50,90]}` {
		t.Fatalf("saved quote response metadata=%#v", quote)
	}
}

func TestAnalysisCacheSharesAcrossSQLiteHandlesAndBoundsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analysis-cache.db")
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

	payload := map[string]any{
		"symbol": "QQQ",
		"levels": []any{map[string]any{"price": 400.25, "score": 0.9}},
	}
	if err := first.PutAnalysisCache("levels-v12:QQQ:test", payload, 2); err != nil {
		t.Fatal(err)
	}
	stored, found, err := second.GetAnalysisCache("levels-v12:QQQ:test")
	if err != nil || !found {
		t.Fatalf("shared cache read found=%v err=%v payload=%#v", found, err, stored)
	}
	storedMap, ok := stored.(map[string]any)
	if !ok || storedMap["symbol"] != "QQQ" {
		t.Fatalf("shared JSON payload=%#v", stored)
	}
	levels, ok := storedMap["levels"].([]any)
	if !ok || len(levels) != 1 {
		t.Fatalf("shared nested JSON payload=%#v", stored)
	}
	level, ok := levels[0].(map[string]any)
	if !ok || level["price"] != 400.25 || level["score"] != 0.9 {
		t.Fatalf("shared nested level=%#v", levels[0])
	}

	if err := second.PutAnalysisCache("levels-v12:QQQ:second", map[string]any{"value": 2}, 2); err != nil {
		t.Fatal(err)
	}
	if err := first.PutAnalysisCache("levels-v12:QQQ:third", map[string]any{"value": 3}, 2); err != nil {
		t.Fatal(err)
	}
	if _, found, err := second.GetAnalysisCache("levels-v12:QQQ:test"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("oldest cache entry survived max_entries pruning")
	}
	if _, found, err := second.GetAnalysisCache("levels-v12:QQQ:second"); err != nil || !found {
		t.Fatalf("second newest cache entry found=%v err=%v", found, err)
	}
	if _, found, err := second.GetAnalysisCache("levels-v12:QQQ:third"); err != nil || !found {
		t.Fatalf("newest cache entry found=%v err=%v", found, err)
	}
	var count int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM api_analysis_cache`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("analysis cache count=%d, want bounded count 2", count)
	}
}

func TestOpenCreatesPythonCompatibleQueryIndexes(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "schema-indexes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	want := map[string]string{
		"idx_quote_symbol_time":        "quote_snapshots",
		"idx_option_lookup":            "option_snapshots",
		"idx_option_symbol_time":       "option_snapshots",
		"idx_option_oi_fallback":       "option_snapshots",
		"idx_option_latest_expiration": "option_latest_batches",
		"idx_refresh_symbol_time":      "refresh_runs",
		"idx_api_analysis_cache_time":  "api_analysis_cache",
	}
	for name, table := range want {
		var found string
		err := repository.db.QueryRow(`SELECT tbl_name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&found)
		if err != nil || found != table {
			t.Errorf("index %s table=%q err=%v; want %q", name, found, err, table)
		}
	}
}

func TestOpenMigratesLegacyPythonSchemaAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-options.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
CREATE TABLE quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, currency TEXT, market_state TEXT, provider TEXT NOT NULL,
 raw_json TEXT, sessions_json TEXT, today_open REAL, previous_close REAL,
 fair_value REAL, fair_value_low REAL, fair_value_high REAL,
 fair_value_buy_low REAL, fair_value_buy_high REAL, fair_value_source TEXT, fair_value_model TEXT,
 fair_value_forward_eps REAL, fair_value_forward_eps_source TEXT, fair_value_safety_margin REAL,
 fair_value_confidence TEXT, fair_value_confidence_score REAL, fair_value_interest_coverage REAL,
 fair_value_regime TEXT, fair_value_regime_signals_json TEXT, fair_value_model_under_regime TEXT,
 fair_value_defensive_json TEXT, fair_value_optimistic_json TEXT, fair_value_normalized_eps_source TEXT,
 fair_value_quarterly_momentum_json TEXT, fair_value_historical_valuation_percentiles_json TEXT,
 fair_value_shareholder_total_return_yield REAL, fair_value_market_cap_data_quality TEXT,
 fair_value_data_quality_score REAL, fair_value_owner_earnings_maintenance_ratio REAL,
 fair_value_owner_earnings_ratio_source TEXT
);
CREATE TABLE option_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, expiration TEXT NOT NULL,
 fetched_at TEXT NOT NULL, contract_symbol TEXT NOT NULL, contract_type TEXT NOT NULL,
 strike REAL, last_price REAL, bid REAL, ask REAL, volume INTEGER, open_interest INTEGER,
 implied_volatility REAL, gamma REAL, in_the_money INTEGER, change_percent REAL,
 provider TEXT NOT NULL, raw_json TEXT,
 UNIQUE(symbol, expiration, fetched_at, contract_symbol)
);
CREATE TABLE option_latest_batches (
 symbol TEXT NOT NULL, expiration TEXT NOT NULL, fetched_at TEXT NOT NULL,
 PRIMARY KEY(symbol, expiration)
);
CREATE TABLE push_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, kind TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE TABLE refresh_runs (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, started_at TEXT NOT NULL,
 finished_at TEXT, status TEXT NOT NULL, rows_written INTEGER NOT NULL DEFAULT 0, error_message TEXT
);
CREATE TABLE price_history (
 symbol TEXT NOT NULL, bar_date TEXT NOT NULL, open REAL, high REAL, low REAL,
 close REAL, volume REAL, fetched_at TEXT NOT NULL, PRIMARY KEY(symbol, bar_date)
);
CREATE TABLE price_extremes (symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL);
CREATE TABLE beta_snapshots (symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL);
CREATE TABLE earnings_snapshots (symbol TEXT PRIMARY KEY, payload TEXT NOT NULL, fetched_at TEXT NOT NULL);
INSERT INTO quote_snapshots(symbol, fetched_at, price, change_percent, currency, market_state, provider, sessions_json, today_open, previous_close, fair_value)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 400, 1.5, 'USD', 'REGULAR', 'yahoo', '{"regular":{"price":400}}', 398, 394, 410);
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, strike, bid, ask, volume, open_interest, implied_volatility, gamma, in_the_money, change_percent, provider)
VALUES ('QQQ', '2030-01-18', '2030-01-18T15:00:00Z', 'QQQ300118C00400000', 'call', 400, 1, 1.2, 5, 100, 0.2, 0.01, 1, 0.5, 'yahoo-options');
`
	if _, err := db.Exec(legacySchema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy Python schema: %v", err)
	}
	defer repository.Close()
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 400 || quote.TodayOpen == nil || *quote.TodayOpen != 398 || quote.PreviousClose == nil || *quote.PreviousClose != 394 || quote.FairValue == nil {
		t.Fatalf("legacy quote lost or unreadable: quote=%#v found=%v err=%v", quote, found, err)
	}
	chain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].OpenInterest == nil || *chain.Data[0].OpenInterest != 100 {
		t.Fatalf("legacy option data lost or unreadable: chain=%#v found=%v err=%v", chain, found, err)
	}
	// Writes after migration must remain visible through the same Python-created schema.
	writeAt := time.Date(2031, time.January, 18, 15, 0, 0, 0, time.UTC)
	writtenPrice := 401.0
	if err := repository.SaveQuote(model.Quote{Symbol: "QQQ", Provider: "go-yahoo", Sessions: map[string]any{}, Price: &writtenPrice, FetchedAt: &writeAt}); err != nil {
		t.Fatalf("write to legacy schema: %v", err)
	}
	quote, found, err = repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != writtenPrice {
		t.Fatalf("Go quote write is not visible in migrated schema: quote=%#v found=%v err=%v", quote, found, err)
	}
	strike := 405.0
	if err := repository.SaveChain(model.ChainResponse{
		Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &writeAt,
		Data: []model.OptionRow{{ContractSymbol: "QQQ300118C00405000", ContractType: "call", Strike: &strike}},
	}); err != nil {
		t.Fatalf("write chain to legacy schema: %v", err)
	}
	chain, found, err = repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].ContractSymbol != "QQQ300118C00405000" {
		t.Fatalf("Go option write is not visible in migrated schema: chain=%#v found=%v err=%v", chain, found, err)
	}
}

func TestOpenMigratesOlderPythonSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-migration.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, currency TEXT, market_state TEXT, provider TEXT NOT NULL, raw_json TEXT
);
CREATE TABLE option_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, expiration TEXT NOT NULL,
 fetched_at TEXT NOT NULL, contract_symbol TEXT NOT NULL, contract_type TEXT NOT NULL,
 strike REAL, last_price REAL, bid REAL, ask REAL, volume INTEGER, open_interest INTEGER,
 implied_volatility REAL, provider TEXT NOT NULL, raw_json TEXT,
 UNIQUE(symbol, expiration, fetched_at, contract_symbol)
);
CREATE TABLE push_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, kind TEXT NOT NULL, created_at TEXT NOT NULL
);
INSERT INTO quote_snapshots(symbol, fetched_at, price, currency, provider)
VALUES ('AAPL', '2030-01-18T15:00:00Z', 200, 'USD', 'legacy');
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, strike, open_interest, provider)
VALUES ('AAPL', '2030-01-18', '2030-01-18T15:00:00Z', 'AAPL300118C00200000', 'call', 200, 25, 'legacy');
`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatalf("Open pre-migration Python schema: %v", err)
	}
	defer repository.Close()
	quote, found, err := repository.LatestQuote("AAPL")
	if err != nil || !found || quote.Price == nil || *quote.Price != 200 {
		t.Fatalf("legacy quote unavailable after migration: quote=%#v found=%v err=%v", quote, found, err)
	}
	chain, found, err := repository.LatestChain("AAPL", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Gamma != nil || chain.Data[0].OpenInterest == nil || *chain.Data[0].OpenInterest != 25 {
		t.Fatalf("legacy chain unavailable after migration: chain=%#v found=%v err=%v", chain, found, err)
	}
	if _, err := repository.AddEvent(model.Event{Symbol: "AAPL", Kind: "snapshot", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("write to migrated push_events: %v", err)
	}
}

func TestCleanupRemovesExpiredRowsAndStaleBatchIndexes(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	old := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339Nano)
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	pythonISOTime := time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02T15:04:05.999999-07:00")
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider) VALUES ('OLD', ?, 'test'), ('OLD_PYTHON_ISO', ?, 'test'), ('NEW', ?, 'test')`, old, pythonISOTime, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, provider) VALUES ('OLD', '2030-01-18', ?, 'OLD300118C00100000', 'call', 'test'), ('NEW', '2030-01-18', ?, 'NEW300118C00100000', 'call', 'test')`, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO option_latest_batches(symbol, expiration, fetched_at) VALUES ('OLD', '2030-01-18', ?), ('NEW', '2030-01-18', ?)`, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO refresh_runs(symbol, started_at, status) VALUES ('OLD', ?, 'success'), ('NEW', ?, 'success')`, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO price_history(symbol, bar_date, fetched_at) VALUES ('OLD', '2000-01-01', ?), ('NEW', '2030-01-01', ?)`, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO price_extremes(symbol, payload, fetched_at) VALUES ('OLD', '{}', ?)`, old); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.Cleanup(30)
	if err != nil {
		t.Fatal(err)
	}
	for key, got := range map[string]int64{"options": deleted["options"], "runs": deleted["runs"], "history": deleted["history"], "extremes": deleted["extremes"]} {
		if got != 1 {
			t.Errorf("deleted[%s]=%d, want 1", key, got)
		}
	}
	if deleted["quotes"] != 2 {
		t.Errorf("deleted[quotes]=%d, want two old rows including Python ISO timestamp", deleted["quotes"])
	}
	if _, exists, err := repository.LatestQuote("OLD_PYTHON_ISO"); err != nil || exists {
		t.Errorf("Python ISO timestamp quote remains after cleanup: exists=%v err=%v", exists, err)
	}
	if _, exists, err := repository.LatestChainBatchTime("OLD", "2030-01-18"); err != nil || exists {
		t.Errorf("expired batch index remains: exists=%v err=%v", exists, err)
	}
	if _, exists, err := repository.LatestChainBatchTime("NEW", "2030-01-18"); err != nil || !exists {
		t.Errorf("recent batch index missing: exists=%v err=%v", exists, err)
	}
}

func TestCleanupUsesZeroDayRetentionCutoff(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "cleanup-zero-retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	stamp := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider)
VALUES ('QQQ', ?, 'test')`, stamp); err != nil {
		t.Fatal(err)
	}

	deleted, err := repository.Cleanup(0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted["quotes"] != 1 {
		t.Fatalf("zero-day retention deleted %d quotes, want 1", deleted["quotes"])
	}
	if _, found, err := repository.LatestQuote("QQQ"); err != nil || found {
		t.Fatalf("zero-day retention quote found=%v err=%v, want expired", found, err)
	}
}

func TestCleanupRollsBackAllTablesWhenOneTableDeleteFails(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "cleanup-atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	old := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO quote_snapshots(symbol, fetched_at, provider) VALUES ('OLD', ?, 'test');
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, provider) VALUES ('OLD', '2030-01-18', ?, 'OLD300118C00100000', 'call', 'test');
CREATE TRIGGER fail_option_cleanup BEFORE DELETE ON option_snapshots BEGIN SELECT RAISE(ABORT, 'synthetic cleanup failure'); END;`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cleanup(30); err == nil {
		t.Fatal("Cleanup() error = nil, want synthetic option cleanup failure")
	}
	if _, found, err := repository.LatestQuote("OLD"); err != nil || !found {
		t.Fatalf("quote deletion should roll back with failed option cleanup: found=%v err=%v", found, err)
	}
	var optionRows int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM option_snapshots WHERE symbol='OLD'`).Scan(&optionRows); err != nil || optionRows != 1 {
		t.Fatalf("option row should remain after cleanup rollback: count=%d err=%v", optionRows, err)
	}
}

func TestSaveChainPreservesCboeProvider(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "cboe-provenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	at := time.Now().UTC().Truncate(time.Second)
	strike := 450.0
	chain := model.ChainResponse{
		Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &at, Source: "upstream",
		Data: []model.OptionRow{{ContractSymbol: "QQQ300118C00450000", ContractType: "call", Provider: "cboe-delayed", Strike: &strike}},
	}
	if err := repository.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	latest, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(latest.Data) != 1 {
		t.Fatalf("LatestChain() = %#v, found=%v, err=%v", latest, found, err)
	}
	if latest.Data[0].Provider != "cboe-delayed" {
		t.Fatalf("provider provenance = %q, want cboe-delayed", latest.Data[0].Provider)
	}
}

func TestOptionFlowUsesAdjacentSnapshots(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "options.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	strike, bid, ask, last := 100.0, 1.0, 1.2, 1.2
	volumeOld, volumeNew, oi := int64(5), int64(15), int64(100)
	firstAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	secondAt := firstAt.Add(time.Minute)
	for _, snapshot := range []struct {
		at     time.Time
		volume int64
	}{{firstAt, volumeOld}, {secondAt, volumeNew}} {
		chain := model.ChainResponse{Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &snapshot.at, Data: []model.OptionRow{{
			ContractSymbol: "QQQ300118C00100000", ContractType: "call", Strike: &strike,
			Bid: &bid, Ask: &ask, LastPrice: &last, Volume: &snapshot.volume, OpenInterest: &oi,
		}}}
		if err := repository.SaveChain(chain); err != nil {
			t.Fatal(err)
		}
	}
	flow, err := repository.OptionFlow("QQQ", "2030-01-18")
	if err != nil {
		t.Fatal(err)
	}
	if flow["available"] != true {
		t.Fatalf("flow not available: %#v", flow)
	}
	call, ok := flow["call"].(map[string]any)
	if !ok || call["buy_volume"] != int64(10) || call["buy_premium"] != 1200.0 {
		t.Fatalf("unexpected flow call side: %#v", flow["call"])
	}
}

func TestOptionFlowPreservesPythonStableVolumeTieOrderFromCurrentRows(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "flow-tie-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	oldAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	newAt := oldAt.Add(time.Minute)
	saveBatch := func(symbol string, at time.Time, order []float64, volume int64) {
		data := make([]model.OptionRow, len(order))
		for index, strike := range order {
			strikeValue, volumeValue := strike, volume
			data[index] = model.OptionRow{
				ContractSymbol: []string{"C105", "C103", "C109"}[index],
				ContractType:   "call",
				Strike:         &strikeValue,
				Volume:         &volumeValue,
			}
		}
		if err := repository.SaveChain(model.ChainResponse{Symbol: symbol, Expiration: "2030-01-18", FetchedAt: &at, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	assertOrder := func(symbol string, want []float64) {
		flow, err := repository.OptionFlow(symbol, "2030-01-18")
		if err != nil {
			t.Fatal(err)
		}
		call := flow["call"].(map[string]any)
		for _, field := range []string{"top_strikes", "concentration"} {
			rows := call[field].([]any)
			for index, expected := range want {
				if got := rows[index].(map[string]any)["strike"]; got != expected {
					t.Fatalf("%s %s[%d]=%v, want %v", symbol, field, index, got, expected)
				}
			}
		}
	}

	ordered := []float64{105, 103, 109}
	saveBatch("QQQ", oldAt, ordered, 0)
	saveBatch("QQQ", newAt, ordered, 10)
	assertOrder("QQQ", ordered)

	opposite := []float64{109, 103, 105}
	saveBatch("AAPL", oldAt, opposite, 0)
	saveBatch("AAPL", newAt, opposite, 10)
	assertOrder("AAPL", opposite)
}

func TestFlowStrikeListsPreservesPythonStableVolumeTieOrder(t *testing.T) {
	ordered := map[float64]*flowStrikeAggregate{
		105: {volume: 10, buyVolume: 10, buyPremium: 1050, firstSeen: 1, buyFirstSeen: 1},
		103: {volume: 10, sellVolume: 10, sellPremium: 1030, firstSeen: 2, sellFirstSeen: 2},
		109: {volume: 10, unknownVolume: 10, unknownPremium: 1090, firstSeen: 3, unknownFirstSeen: 3},
	}
	top, concentration := flowStrikeLists(ordered)
	want := []float64{105, 103, 109}
	for index, expected := range want {
		if got := top[index].(map[string]any)["strike"]; got != expected {
			t.Fatalf("top strike order[%d]=%v, want %v: %#v", index, got, expected, top)
		}
		if got := concentration[index].(map[string]any)["strike"]; got != expected {
			t.Fatalf("concentration order[%d]=%v, want %v: %#v", index, got, expected, concentration)
		}
	}

	opposite := map[float64]*flowStrikeAggregate{
		105: {volume: 10, buyVolume: 10, firstSeen: 3, buyFirstSeen: 3},
		103: {volume: 10, sellVolume: 10, firstSeen: 2, sellFirstSeen: 2},
		109: {volume: 10, unknownVolume: 10, firstSeen: 1, unknownFirstSeen: 1},
	}
	top, concentration = flowStrikeLists(opposite)
	want = []float64{109, 103, 105}
	for index, expected := range want {
		if got := top[index].(map[string]any)["strike"]; got != expected {
			t.Fatalf("opposite top strike order[%d]=%v, want %v: %#v", index, got, expected, top)
		}
		if got := concentration[index].(map[string]any)["strike"]; got != expected {
			t.Fatalf("opposite concentration order[%d]=%v, want %v: %#v", index, got, expected, concentration)
		}
	}
}

func TestDuplicateOptionSnapshotAtSameTimestampIsIdempotent(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "options-duplicate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC().Truncate(time.Second)
	strike := 100.0
	firstVolume, duplicateVolume := int64(12), int64(99)
	for _, volume := range []int64{firstVolume, duplicateVolume} {
		if err := repository.SaveChain(model.ChainResponse{
			Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &stamp,
			Data: []model.OptionRow{{
				ContractSymbol: "QQQ300118C00100000", ContractType: "call", Strike: &strike, Volume: &volume,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	chain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Volume == nil || *chain.Data[0].Volume != firstVolume {
		t.Fatalf("duplicate snapshot write was not idempotent: chain=%#v found=%v err=%v", chain, found, err)
	}
}

func TestOlderOptionWriteDoesNotRewindLatestBatch(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "options-ordering.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	newest := time.Now().UTC().Truncate(time.Second)
	older := newest.Add(-time.Minute)
	strike := 100.0
	volumeNewest, volumeOlder := int64(25), int64(10)
	for _, snapshot := range []struct {
		at     time.Time
		volume int64
	}{{newest, volumeNewest}, {older, volumeOlder}} {
		if err := repository.SaveChain(model.ChainResponse{
			Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &snapshot.at,
			Data: []model.OptionRow{{
				ContractSymbol: "QQQ300118C00100000", ContractType: "call", Strike: &strike, Volume: &snapshot.volume,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	batchTime, exists, err := repository.LatestChainBatchTime("QQQ", "2030-01-18")
	if err != nil || !exists || batchTime == nil || !batchTime.Equal(newest) {
		t.Fatalf("latest batch regressed to older request: fetched=%v exists=%v err=%v", batchTime, exists, err)
	}
	chain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Volume == nil || *chain.Data[0].Volume != volumeNewest {
		t.Fatalf("latest chain selected stale request: chain=%#v found=%v err=%v", chain, found, err)
	}
}

func TestEmptyLatestChainDoesNotCreateBatchMarker(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "options.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fetched := time.Now().UTC().Truncate(time.Second)
	if err := repository.SaveChain(model.ChainResponse{Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &fetched, Data: []model.OptionRow{}}); err != nil {
		t.Fatal(err)
	}
	chain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || found || chain.FetchedAt != nil {
		t.Fatalf("empty latest chain should not become readable: chain=%#v found=%v err=%v", chain, found, err)
	}
	if _, exists, err := repository.LatestChainBatchTime("QQQ", "2030-01-18"); err != nil || exists {
		t.Fatalf("empty chain created an unexpected batch marker: exists=%v err=%v", exists, err)
	}
}

func TestLatestRefreshRunMatchesPythonLexicalTimestampOrdering(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "refresh-run-offset-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	// Python's latest_status orders ISO timestamp text lexically; the Z-form wins
	// over an equivalent +00:00 value, rather than collapsing both instants.
	if _, err := repository.db.Exec(`INSERT INTO refresh_runs(symbol, started_at, status)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 'success'), ('QQQ', '2030-01-18T15:00:00+00:00', 'failed')`); err != nil {
		t.Fatal(err)
	}
	status, found, err := repository.LatestRefreshRun("QQQ")
	if err != nil || !found || status["started_at"] != "2030-01-18T15:00:00Z" || status["status"] != "success" {
		t.Fatalf("latest refresh run must match Python lexical timestamp ordering: status=%#v found=%v err=%v", status, found, err)
	}
}

func TestRefreshRunLifecycleAndLatestStatus(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "refresh-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	firstID, err := repository.StartRefreshRun("QQQ")
	if err != nil {
		t.Fatal(err)
	}
	status, found, err := repository.LatestRefreshRun("QQQ")
	if err != nil || !found || status["status"] != "running" || status["finished_at"] != nil {
		t.Fatalf("running status=%#v found=%v err=%v", status, found, err)
	}
	if err := repository.FinishRefreshRun(firstID, "success", 42, ""); err != nil {
		t.Fatal(err)
	}
	secondID, err := repository.StartRefreshRun("QQQ")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishRefreshRun(secondID, "failed", 0, "upstream timeout"); err != nil {
		t.Fatal(err)
	}
	status, found, err = repository.LatestRefreshRun("QQQ")
	if err != nil || !found || status["id"] != secondID || status["status"] != "failed" || status["error_message"] != "upstream timeout" || status["finished_at"] == nil {
		t.Fatalf("latest failed status=%#v found=%v err=%v", status, found, err)
	}
	previous, found, err := repository.LatestRefreshRun("AAPL")
	if err != nil || found || previous != nil {
		t.Fatalf("unknown symbol status=%#v found=%v err=%v", previous, found, err)
	}
}

func TestLatestPayloadMatchesPythonCacheWrappers(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "payload-wrappers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fetched := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`INSERT INTO price_extremes(symbol, payload, fetched_at) VALUES ('QQQ', '{"high": 120}', ?); INSERT INTO beta_snapshots(symbol, payload, fetched_at) VALUES ('QQQ', '{"value": 1.2}', ?); INSERT INTO earnings_snapshots(symbol, payload, fetched_at) VALUES ('QQQ', '{"dates":["2030-01-18"]}', ?)`, fetched, fetched, fetched); err != nil {
		t.Fatal(err)
	}
	extremes, err := repository.LatestPayload("price_extremes", "QQQ")
	if err != nil || extremes["extremes"].(map[string]any)["high"] != float64(120) || extremes["fetched_at"] != fetched {
		t.Fatalf("extremes wrapper=%#v err=%v", extremes, err)
	}
	beta, err := repository.LatestPayload("beta_snapshots", "QQQ")
	if err != nil || beta["beta"].(map[string]any)["value"] != float64(1.2) || beta["fetched_at"] != fetched {
		t.Fatalf("beta wrapper=%#v err=%v", beta, err)
	}
	earnings, err := repository.LatestPayload("earnings_snapshots", "QQQ")
	if err != nil || earnings["dates"].([]any)[0] != "2030-01-18" || earnings["fetched_at"] != fetched {
		t.Fatalf("earnings wrapper=%#v err=%v", earnings, err)
	}
}

func TestWriteHistoryUpsertsAndReadsBars(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "write-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	firstAt := time.Date(2030, time.January, 18, 15, 0, 0, 0, time.FixedZone("test", 2*60*60))
	bars := []map[string]any{
		{"date": "2030-01-17", "open": 10, "high": 12.0, "low": 9.5, "close": 11.5, "volume": 1000},
		{"date": "2030-01-18", "open": 11.5, "high": 13.0, "low": 11.0, "close": 12.5, "volume": 1200},
	}
	if err := repository.WriteHistory("QQQ", bars, firstAt); err != nil {
		t.Fatal(err)
	}
	secondAt := firstAt.Add(time.Hour)
	if err := repository.WriteHistory("QQQ", []map[string]any{{"date": "2030-01-18", "open": 12.0, "high": 14.0, "low": 11.8, "close": 13.5, "volume": 1400}}, secondAt); err != nil {
		t.Fatal(err)
	}
	result, err := repository.LatestHistory("QQQ", 3000)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result["bars"].([]map[string]any)
	if !ok || len(got) != 2 {
		t.Fatalf("history bars=%#v, want 2 sorted bars", result["bars"])
	}
	closeValue, closeOK := got[1]["close"].(*float64)
	volumeValue, volumeOK := got[1]["volume"].(*float64)
	if got[0]["date"] != "2030-01-17" || got[1]["date"] != "2030-01-18" || !closeOK || closeValue == nil || *closeValue != 13.5 || !volumeOK || volumeValue == nil || *volumeValue != 1400 {
		t.Fatalf("history upsert result=%#v", got)
	}
	if result["fetched_at"] != storedTimestamp(secondAt) {
		t.Fatalf("latest fetched_at=%v, want %s", result["fetched_at"], storedTimestamp(secondAt))
	}
}

func TestWriteHistoryValidatesAtomically(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "write-history-validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	stamp := time.Now().UTC()
	valid := map[string]any{"date": "2030-01-18", "open": 10.0, "high": 11.0, "low": 9.0, "close": 10.5, "volume": nil}
	if err := repository.WriteHistory("QQQ", []map[string]any{valid}, stamp); err != nil {
		t.Fatalf("nullable volume rejected: %v", err)
	}
	badCases := []struct {
		name string
		bar  map[string]any
	}{
		{"invalid date", map[string]any{"date": "2030-02-30", "close": 10.0}},
		{"timestamp date", map[string]any{"date": "2030-01-18T00:00:00Z", "close": 10.0}},
		{"nan", map[string]any{"date": "2030-01-19", "close": math.NaN()}},
		{"positive infinity", map[string]any{"date": "2030-01-19", "high": math.Inf(1)}},
		{"negative infinity", map[string]any{"date": "2030-01-19", "low": math.Inf(-1)}},
		{"non-number", map[string]any{"date": "2030-01-19", "close": "11.0"}},
		{"missing date", map[string]any{"close": 10.0}},
	}
	for _, test := range badCases {
		t.Run(test.name, func(t *testing.T) {
			if err := repository.WriteHistory("QQQ", []map[string]any{{"date": "2030-01-20", "close": 20.0}, test.bar}, stamp.Add(time.Minute)); err == nil {
				t.Fatalf("invalid bar accepted: %#v", test.bar)
			}
			result, err := repository.LatestHistory("QQQ", 3000)
			if err != nil {
				t.Fatal(err)
			}
			got := result["bars"].([]map[string]any)
			if len(got) != 1 || got[0]["date"] != "2030-01-18" {
				t.Fatalf("failed write was not atomic: %#v", got)
			}
		})
	}
	if err := repository.WriteHistory(" ", nil, stamp); err == nil {
		t.Fatal("empty symbol accepted")
	}
	if err := repository.WriteHistory("QQQ", nil, time.Time{}); err == nil {
		t.Fatal("zero fetched_at accepted")
	}
}

func TestWritePayloadAllowlistUpsertAndWrappers(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "write-payload.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fetchedAt := time.Date(2030, time.January, 18, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		table string
		data  map[string]any
		key   string
		value any
	}{
		{"price_extremes", map[string]any{"high": 120.0}, "extremes", "high"},
		{"beta_snapshots", map[string]any{"value": 1.2}, "beta", "value"},
		{"earnings_snapshots", map[string]any{"dates": []string{"2030-01-18"}}, "dates", "2030-01-18"},
	}
	for _, test := range cases {
		t.Run(test.table, func(t *testing.T) {
			if err := repository.WritePayload(test.table, "QQQ", test.data, fetchedAt); err != nil {
				t.Fatal(err)
			}
			loaded, err := repository.LatestPayload(test.table, "QQQ")
			if err != nil {
				t.Fatal(err)
			}
			if loaded["fetched_at"] != storedTimestamp(fetchedAt) {
				t.Fatalf("fetched_at=%v", loaded["fetched_at"])
			}
			switch test.table {
			case "price_extremes", "beta_snapshots":
				wrapped, ok := loaded[test.key].(map[string]any)
				if !ok || wrapped[test.value.(string)] == nil {
					t.Fatalf("wrapped payload=%#v", loaded)
				}
			case "earnings_snapshots":
				dates, ok := loaded[test.key].([]any)
				if !ok || len(dates) != 1 || dates[0] != test.value {
					t.Fatalf("earnings payload=%#v", loaded)
				}
			}
		})
	}
	if err := repository.WritePayload("beta_snapshots", "QQQ", map[string]any{"value": 1.7}, fetchedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	beta, err := repository.LatestPayload("beta_snapshots", "QQQ")
	if err != nil || beta["beta"].(map[string]any)["value"] != 1.7 {
		t.Fatalf("payload upsert=%#v err=%v", beta, err)
	}
	for _, table := range []string{"quote_snapshots", "price_history; DROP TABLE quote_snapshots", ""} {
		if err := repository.WritePayload(table, "QQQ", map[string]any{"x": 1}, fetchedAt); err == nil {
			t.Errorf("unsupported table %q accepted", table)
		}
	}
	if err := repository.WritePayload("price_extremes", "", map[string]any{}, fetchedAt); err == nil {
		t.Fatal("empty symbol accepted")
	}
	if err := repository.WritePayload("price_extremes", "QQQ", map[string]any{}, time.Time{}); err == nil {
		t.Fatal("zero fetched_at accepted")
	}
	if err := repository.WritePayload("price_extremes", "QQQ", map[string]any{"bad": math.NaN()}, fetchedAt); err == nil {
		t.Fatal("non-JSON payload accepted")
	}
}

func TestAddEventPrunesOldEventsToBoundedWindow(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "event-retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repository.db.Exec(`WITH RECURSIVE ids(id) AS (
  SELECT 1 UNION ALL SELECT id+1 FROM ids WHERE id < 20005
)
INSERT INTO push_events(id, symbol, kind, created_at)
SELECT id, 'QQQ', 'snapshot', ? FROM ids`, createdAt); err != nil {
		t.Fatal(err)
	}
	newID, err := repository.AddEvent(model.Event{Symbol: "QQQ", Kind: "quote", CreatedAt: createdAt})
	if err != nil {
		t.Fatal(err)
	}
	var count, minimum, maximum int64
	if err := repository.db.QueryRow(`SELECT COUNT(*), MIN(id), MAX(id) FROM push_events`).Scan(&count, &minimum, &maximum); err != nil {
		t.Fatal(err)
	}
	if count != 20000 || minimum != 7 || maximum != newID {
		t.Fatalf("event log window count=%d min=%d max=%d, want count=20000 min=7 max=%d", count, minimum, maximum, newID)
	}
	latest, err := repository.EventsSince(newID-1, "QQQ", "")
	if err != nil || len(latest) != 1 || latest[0].ID != newID {
		t.Fatalf("latest event not retained: events=%#v err=%v", latest, err)
	}
}

func TestSQLiteRoundTripAndEventReplay(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "options.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	price, change, todayOpen, previous := 123.45, 1.25, 122.50, 121.92
	forwardEPS, safetyMargin, confidenceScore := 12.5, 0.15, 0.82
	confidence, epsSource, regime := "high", "analyst", "cyclical"
	defensive := map[string]any{"value": 400.0, "reason": "base"}
	quarterly := []map[string]any{{"quarter": "Q1", "growth": 0.1}}
	shareholderYield := 0.025
	now := time.Now().UTC().Truncate(time.Microsecond)
	quote := model.Quote{
		Symbol: "QQQ", Price: &price, ChangePercent: &change, TodayOpen: &todayOpen, PreviousClose: &previous,
		Currency: "USD", MarketState: "regular", Provider: "test", FetchedAt: &now,
		Sessions:  map[string]any{"regular": map[string]any{"price": price}},
		FairValue: 430.0, FairValueSource: "valuation_test", FairValueModel: "owner_earnings",
		FairValueForwardEPS: &forwardEPS, FairValueForwardEPSSource: &epsSource, FairValueSafetyMargin: &safetyMargin,
		FairValueConfidence: &confidence, FairValueConfidenceScore: &confidenceScore, FairValueRegime: &regime,
		FairValueDefensive: defensive, FairValueQuarterlyMomentum: quarterly, FairValueShareholderReturnYield: &shareholderYield,
	}
	if err := repository.SaveQuote(quote); err != nil {
		t.Fatal(err)
	}
	loadedQuote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found {
		t.Fatalf("LatestQuote() found=%v err=%v", found, err)
	}
	if loadedQuote.Price == nil || *loadedQuote.Price != price || loadedQuote.TodayOpen == nil || *loadedQuote.TodayOpen != todayOpen || loadedQuote.FairValueForwardEPS == nil || *loadedQuote.FairValueForwardEPS != forwardEPS || loadedQuote.FairValueConfidence == nil || *loadedQuote.FairValueConfidence != confidence || loadedQuote.FairValueDefensive == nil || loadedQuote.FairValueQuarterlyMomentum == nil || loadedQuote.FairValueShareholderReturnYield == nil || *loadedQuote.FairValueShareholderReturnYield != shareholderYield || loadedQuote.Sessions["regular"] == nil {
		t.Fatalf("quote did not round-trip: %#v", loadedQuote)
	}

	refreshedPrice, refreshedEPS := 124.0, 13.0
	refreshedAt := now.Add(time.Minute)
	refreshed, err := repository.SaveQuotePreservingFairValue(model.Quote{
		Symbol: "QQQ", Price: &refreshedPrice, Provider: "refresh", FetchedAt: &refreshedAt,
		FairValueForwardEPS: &refreshedEPS, FairValueRegimeSignals: map[string]any{},
		FairValueDefensive: map[string]any{}, FairValueQuarterlyMomentum: []any{}, Sessions: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fairValue, fairValueOK := refreshed.FairValue.(*float64)
	if !fairValueOK || *fairValue != 430.0 || refreshed.FairValueForwardEPS == nil || *refreshed.FairValueForwardEPS != refreshedEPS || refreshed.FairValueSource != "valuation_test" || refreshed.FairValueConfidence == nil || *refreshed.FairValueConfidence != confidence {
		t.Fatalf("refresh did not preserve missing valuation fields while honoring new values: %#v", refreshed)
	}
	if defensive, ok := refreshed.FairValueDefensive.(map[string]any); !ok || defensive["reason"] != "base" {
		t.Fatalf("refresh did not preserve cached valuation JSON: %#v", refreshed.FairValueDefensive)
	}
	storedAfterRefresh, found, err := repository.LatestQuote("QQQ")
	storedFairValue, storedFairValueOK := storedAfterRefresh.FairValue.(*float64)
	if err != nil || !found || storedAfterRefresh.Price == nil || *storedAfterRefresh.Price != refreshedPrice || !storedFairValueOK || *storedFairValue != 430.0 {
		t.Fatalf("refreshed quote not persisted with valuation: quote=%#v found=%v err=%v", storedAfterRefresh, found, err)
	}

	strike := 400.0
	row := model.OptionRow{ContractSymbol: "QQQ250118C00400000", ContractType: "call", Strike: &strike, Expiration: "2030-01-18"}
	chain := model.ChainResponse{Symbol: "QQQ", Expiration: row.Expiration, FetchedAt: &now, Data: []model.OptionRow{row}}
	if err := repository.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	loadedChain, found, err := repository.LatestChain("QQQ", row.Expiration)
	if err != nil || !found || len(loadedChain.Data) != 1 {
		t.Fatalf("LatestChain() found=%v err=%v response=%#v", found, err, loadedChain)
	}
	if loadedChain.Data[0].Strike == nil || *loadedChain.Data[0].Strike != strike {
		t.Fatalf("chain did not round-trip: %#v", loadedChain.Data)
	}

	eventID, err := repository.AddEvent(model.Event{Symbol: "QQQ", Kind: "snapshot", Expiration: row.Expiration, CreatedAt: now.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	events, err := repository.EventsSince(eventID-1, "QQQ", row.Expiration)
	if err != nil || len(events) != 1 || events[0].ID != eventID {
		t.Fatalf("EventsSince() = %#v, err=%v", events, err)
	}
}

func TestExpirationProbeEmptyUsesTTLAndSuccessfulCatalogClearsIt(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.SaveExpirationProbe("QQQ", true); err != nil {
		t.Fatal(err)
	}
	if empty, err := repository.ExpirationProbeEmpty("QQQ", time.Minute); err != nil || !empty {
		t.Fatalf("probe empty=%v err=%v", empty, err)
	}
	if err := repository.SaveExpirations("QQQ", []string{"2099-01-01"}); err != nil {
		t.Fatal(err)
	}
	if empty, err := repository.ExpirationProbeEmpty("QQQ", time.Minute); err != nil || empty {
		t.Fatalf("stale empty probe survived catalog update: empty=%v err=%v", empty, err)
	}
}

func TestLatestExpirationsFiltersCatalogGarbageAndFallsBackToBatches(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "expiration-catalog-robustness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	if _, err := repository.db.Exec(`INSERT INTO expiration_catalog(symbol, payload, fetched_at)
VALUES ('MIXED', '{"expirations":["2099-01-16",null,123,true,"2099-01-16","2099-02-30"]}', '2030-01-18T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	values, found, err := repository.LatestExpirations("MIXED")
	if err != nil || !found || len(values) != 1 || values[0] != "2099-01-16" {
		t.Fatalf("mixed catalog values=%#v found=%v err=%v, want one valid deduplicated expiration", values, found, err)
	}

	if _, err := repository.db.Exec(`INSERT INTO option_latest_batches(symbol, expiration, fetched_at)
VALUES ('BROKEN', '2099-01-23', '2030-01-18T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.Exec(`INSERT INTO expiration_catalog(symbol, payload, fetched_at)
VALUES ('BROKEN', '{not-json', '2030-01-18T00:00:00Z'),
       ('EMPTY', '{"expirations":[null,42]}', '2030-01-18T00:00:00Z'),
       ('UNAVAILABLE', '{not-json', '2030-01-18T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	values, found, err = repository.LatestExpirations("BROKEN")
	if err != nil || !found || len(values) != 1 || values[0] != "2099-01-23" {
		t.Fatalf("broken catalog fallback values=%#v found=%v err=%v, want latest batch expiration", values, found, err)
	}
	values, found, err = repository.LatestExpirations("EMPTY")
	if err != nil || found || len(values) != 0 {
		t.Fatalf("unusable catalog without batches values=%#v found=%v err=%v, want empty/missing for upstream fallback", values, found, err)
	}
	values, found, err = repository.LatestExpirations("UNAVAILABLE")
	if err != nil || found || len(values) != 0 {
		t.Fatalf("corrupt catalog without batches values=%#v found=%v err=%v, want empty/missing for upstream fallback", values, found, err)
	}
}

func TestSaveExpirationsFiltersInvalidAndDuplicateDatesLikePython(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "expiration-catalog-clean-write.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	if err := repository.SaveExpirations("QQQ", []string{"2099-01-16", "bad", "2099-01-16", "2099-02-30"}); err != nil {
		t.Fatal(err)
	}
	values, found, err := repository.LatestExpirations("QQQ")
	if err != nil || !found || len(values) != 1 || values[0] != "2099-01-16" {
		t.Fatalf("saved catalog values=%#v found=%v err=%v, want one valid expiration", values, found, err)
	}
	if err := repository.SaveExpirations("QQQ", []string{"bad", "2099-02-30"}); err != nil {
		t.Fatal(err)
	}
	values, found, err = repository.LatestExpirations("QQQ")
	if err != nil || !found || len(values) != 1 || values[0] != "2099-01-16" {
		t.Fatalf("invalid-only update erased catalog: values=%#v found=%v err=%v", values, found, err)
	}
}

func TestOptionInTheMoneyNullRoundTripsSQLiteAndBothReaders(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "nullable-itm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	location, _ := time.LoadLocation("America/New_York")
	expiration := time.Now().In(location).AddDate(0, 0, 7).Format("2006-01-02")
	fetched := time.Now().UTC()
	data := []model.OptionRow{
		{ContractSymbol: "UNKNOWN", ContractType: "call", Strike: quoteTestPtr(99)},
		{ContractSymbol: "OUT", ContractType: "call", Strike: quoteTestPtr(100), InTheMoneyKnown: true},
		{ContractSymbol: "IN", ContractType: "call", Strike: quoteTestPtr(101), InTheMoney: true, InTheMoneyKnown: true},
	}
	chain := model.ChainResponse{Symbol: "NULLTEST", Expiration: expiration, FetchedAt: &fetched, Data: data}
	if err := repository.SaveChain(chain); err != nil {
		t.Fatal(err)
	}

	assertITM := func(rows []model.OptionRow) {
		t.Helper()
		if len(rows) != 3 || rows[0].InTheMoneyKnown || rows[0].InTheMoney || !rows[1].InTheMoneyKnown || rows[1].InTheMoney || !rows[2].InTheMoneyKnown || !rows[2].InTheMoney {
			t.Fatalf("in-the-money values did not round-trip: %#v", rows)
		}
	}
	latest, found, err := repository.LatestChain(chain.Symbol, expiration)
	if err != nil || !found || len(latest.Data) != 3 {
		t.Fatalf("LatestChain found=%v err=%v data=%#v", found, err, latest.Data)
	}
	assertITM(latest.Data)

	all, err := repository.LatestChains(chain.Symbol, 30)
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := all["data"].([]model.OptionRow)
	if !ok {
		t.Fatalf("LatestChains data has type %T", all["data"])
	}
	assertITM(rows)
}

func TestServiceLeaseCoordinatesOwnersAndExpires(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	acquired, err := repository.TryAcquireServiceLease("history:bars:QQQ", "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("initial lease=(%v,%v)", acquired, err)
	}
	acquired, err = repository.TryAcquireServiceLease("history:bars:QQQ", "owner-b", time.Minute)
	if err != nil || acquired {
		t.Fatalf("contended lease=(%v,%v), want false", acquired, err)
	}
	if err := repository.ReleaseServiceLease("history:bars:QQQ", "owner-b"); err != nil {
		t.Fatal(err)
	}
	acquired, err = repository.TryAcquireServiceLease("history:bars:QQQ", "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("same-owner lease=(%v,%v)", acquired, err)
	}
	if err := repository.ReleaseServiceLease("history:bars:QQQ", "owner-a"); err != nil {
		t.Fatal(err)
	}
	acquired, err = repository.TryAcquireServiceLease("history:bars:QQQ", "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("released lease=(%v,%v)", acquired, err)
	}
}

func TestRenewServiceLeaseExtendsLeasePastOriginalAcquisition(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "lease-renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	const name, owner = "snapshot-refresh:QQQ:nearest", "owner-a"
	lease := 300 * time.Millisecond
	if acquired, err := repository.TryAcquireServiceLease(name, owner, lease); err != nil || !acquired {
		t.Fatalf("initial lease=(%v,%v)", acquired, err)
	}
	// Model a renewal near the original deadline without sleeping through the
	// full lease. The renewed timestamp must still be fresh after this point.
	nearExpiry := time.Now().UTC().Add(-250 * time.Millisecond).Format(time.RFC3339Nano)
	if _, err := repository.db.Exec("UPDATE service_leases SET acquired_at=? WHERE name=?", nearExpiry, name); err != nil {
		t.Fatal(err)
	}
	if renewed, err := repository.RenewServiceLease(name, owner); err != nil || !renewed {
		t.Fatalf("renew lease=(%v,%v)", renewed, err)
	}
	time.Sleep(100 * time.Millisecond) // beyond the initial lease's 300ms expiry
	acquired, err := repository.TryAcquireServiceLease(name, "owner-b", lease)
	if err != nil || acquired {
		t.Fatalf("other owner acquired immediately after renewal=(%v,%v), want false", acquired, err)
	}
	var refreshed string
	if err := repository.db.QueryRow("SELECT acquired_at FROM service_leases WHERE name=?", name).Scan(&refreshed); err != nil {
		t.Fatal(err)
	}
	refreshedAt, err := time.Parse(time.RFC3339Nano, refreshed)
	if err != nil || time.Since(refreshedAt) > time.Second || time.Since(refreshedAt) < 0 {
		t.Fatalf("renewed acquisition timestamp=%q parsed=%v err=%v", refreshed, refreshedAt, err)
	}
}

func TestRenewServiceLeaseRejectsOwnerMismatch(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "lease-renew-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	const name = "snapshot-refresh:QQQ:2026-12-18"
	if acquired, err := repository.TryAcquireServiceLease(name, "owner-a", time.Minute); err != nil || !acquired {
		t.Fatalf("initial lease=(%v,%v)", acquired, err)
	}
	var before string
	if err := repository.db.QueryRow("SELECT acquired_at FROM service_leases WHERE name=?", name).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if renewed, err := repository.RenewServiceLease(name, "owner-b"); err != nil || renewed {
		t.Fatalf("mismatched-owner renewal=(%v,%v), want false", renewed, err)
	}
	var after string
	if err := repository.db.QueryRow("SELECT acquired_at FROM service_leases WHERE name=?", name).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("mismatched-owner renewal changed timestamp from %q to %q", before, after)
	}
}

func TestWriteHistoryIsTransactionalAndLatestOrdered(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fetched := time.Now().UTC()
	good := []map[string]any{{"date": "2026-01-02", "open": 1.0, "high": 2.0, "low": 0.5, "close": 1.5, "volume": 10.0}}
	if err := repository.WriteHistory("QQQ", good, fetched); err != nil {
		t.Fatal(err)
	}
	bad := []map[string]any{good[0], {"date": "not-a-date", "open": 1.0}}
	if err := repository.WriteHistory("QQQ", bad, fetched.Add(time.Second)); err == nil {
		t.Fatal("invalid history write unexpectedly succeeded")
	}
	latest, err := repository.LatestHistory("QQQ", 3000)
	if err != nil {
		t.Fatal(err)
	}
	bars := latest["bars"].([]map[string]any)
	if len(bars) != 1 || bars[0]["date"] != "2026-01-02" {
		t.Fatalf("history after failed transaction=%#v", bars)
	}
}

func TestSaveQuoteAndChainSnapshotRollsBackQuoteOnOptionFailure(t *testing.T) {
	repository, err := Open(filepath.Join(t.TempDir(), "snapshot-atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.db.Exec(`CREATE TRIGGER fail_snapshot_options BEFORE INSERT ON option_snapshots BEGIN SELECT RAISE(ABORT, 'synthetic snapshot failure'); END;`); err != nil {
		t.Fatal(err)
	}
	price := 123.0
	fetched := time.Now().UTC().Truncate(time.Second)
	strike := 120.0
	_, err = repository.SaveQuoteAndChainSnapshot(
		model.Quote{Symbol: "QQQ", Price: &price, Provider: "test-quote", FetchedAt: &fetched, Sessions: map[string]any{}},
		model.ChainResponse{Symbol: "QQQ", Expiration: "2030-01-18", Source: "test-options", FetchedAt: &fetched, Data: []model.OptionRow{{ContractSymbol: "QQQ-C120", ContractType: "call", Strike: &strike}}},
	)
	if err == nil {
		t.Fatal("snapshot write unexpectedly succeeded with option trigger")
	}
	var quoteRows, optionRows, batchRows int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&quoteRows); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM option_snapshots WHERE symbol='QQQ'`).Scan(&optionRows); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM option_latest_batches WHERE symbol='QQQ'`).Scan(&batchRows); err != nil {
		t.Fatal(err)
	}
	if quoteRows != 0 || optionRows != 0 || batchRows != 0 {
		t.Fatalf("failed snapshot left partial rows: quotes=%d options=%d batches=%d", quoteRows, optionRows, batchRows)
	}
}

func TestPushRefreshStateCoordinatesClaimsAndBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push-refresh-state.db")
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

	var table string
	if err := first.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='push_refresh_state'`).Scan(&table); err != nil || table != "push_refresh_state" {
		t.Fatalf("push refresh state schema missing: table=%q err=%v", table, err)
	}
	interval := 20 * time.Second
	if claimed, err := first.ClaimPushRefresh("aapl", "2030-01-18", interval); err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v, want true", claimed, err)
	}
	if claimed, err := second.ClaimPushRefresh("AAPL", "2030-01-18", interval); err != nil || claimed {
		t.Fatalf("concurrent second claim=%v err=%v, want false", claimed, err)
	}
	if wait, err := second.PushRefreshWait("AAPL", "2030-01-18", interval); err != nil || wait <= 0 || wait > 5*time.Second {
		t.Fatalf("in-flight wait=%v err=%v, want bounded positive wait", wait, err)
	}
	if err := first.FinishPushRefresh("AAPL", "2030-01-18"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := second.ClaimPushRefresh("AAPL", "2030-01-18", interval); err != nil || claimed {
		t.Fatalf("cadence second claim=%v err=%v, want false", claimed, err)
	}
	firstDelay, err := first.DeferPushRefresh("AAPL", "2030-01-18", interval)
	if err != nil || firstDelay != interval {
		t.Fatalf("first defer=%v err=%v, want %v", firstDelay, err, interval)
	}
	secondDelay, err := second.DeferPushRefresh("AAPL", "2030-01-18", interval)
	if err != nil || secondDelay != 2*interval {
		t.Fatalf("second defer=%v err=%v, want %v", secondDelay, err, 2*interval)
	}
	if claimed, err := first.ClaimPushRefresh("AAPL", "2030-01-18", interval); err != nil || claimed {
		t.Fatalf("backoff claim=%v err=%v, want false", claimed, err)
	}
	var retryDelay int
	if err := first.db.QueryRow(`SELECT retry_delay_seconds FROM push_refresh_state WHERE symbol='AAPL' AND expiration='2030-01-18'`).Scan(&retryDelay); err != nil || retryDelay != int(secondDelay/time.Second) {
		t.Fatalf("persisted retry delay=%d err=%v, want %d", retryDelay, err, int(secondDelay/time.Second))
	}
}
