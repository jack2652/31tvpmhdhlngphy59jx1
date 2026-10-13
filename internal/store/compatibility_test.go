package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"option-scope-go/internal/model"
)

// A representative pre-Go Python database has the core quote/options/event
// tables, but lacks newer columns introduced over time. Opening it must add
// compatibility columns without losing cached rows.
func TestConcurrentOpenMigratesLegacySchemaOnceAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-legacy-options.db")
	legacy, err := sql.Open("sqlite", path+"?_busy_timeout=30000&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
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
INSERT INTO quote_snapshots(symbol,fetched_at,price,provider) VALUES ('QQQ','2030-01-18T15:00:00Z',401.25,'python');
INSERT INTO option_snapshots(symbol,expiration,fetched_at,contract_symbol,contract_type,strike,bid,ask,open_interest,provider)
VALUES ('QQQ','2099-01-16','2030-01-18T15:00:00Z','QQQ-C-400','call',400,2.1,2.3,20,'python');
INSERT INTO push_events(symbol,kind,created_at) VALUES ('QQQ','snapshot','2030-01-18T15:00:00Z');`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	const workers = 12
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			repository, openErr := Open(path)
			if openErr != nil {
				errs <- openErr
				return
			}
			if closeErr := repository.Close(); closeErr != nil {
				errs <- closeErr
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Open failed: %v", err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated legacy schema: %v", err)
	}
	defer repository.Close()
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 401.25 || quote.Provider != "python" {
		t.Fatalf("legacy quote lost after concurrent migration: quote=%#v found=%v err=%v", quote, found, err)
	}
	chain, found, err := repository.LatestChain("QQQ", "2099-01-16")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].ContractSymbol != "QQQ-C-400" {
		t.Fatalf("legacy option row lost after concurrent migration: chain=%#v found=%v err=%v", chain, found, err)
	}
	var sessionsColumn, gammaColumn, eventExpirationColumn int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('quote_snapshots') WHERE name='sessions_json'`).Scan(&sessionsColumn); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('option_snapshots') WHERE name='gamma'`).Scan(&gammaColumn); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('push_events') WHERE name='expiration'`).Scan(&eventExpirationColumn); err != nil {
		t.Fatal(err)
	}
	if sessionsColumn != 1 || gammaColumn != 1 || eventExpirationColumn != 1 {
		t.Fatalf("compatibility columns missing: sessions_json=%d gamma=%d push_events.expiration=%d", sessionsColumn, gammaColumn, eventExpirationColumn)
	}
}

func TestOpenAddsRawJSONColumnBeforeLatestQuoteReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-without-raw-json.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, today_open REAL, previous_close REAL, currency TEXT,
 market_state TEXT, provider TEXT NOT NULL, sessions_json TEXT, fair_value REAL,
 fair_value_low REAL, fair_value_high REAL, fair_value_buy_low REAL, fair_value_buy_high REAL,
 fair_value_source TEXT, fair_value_model TEXT, fair_value_methods_json TEXT,
 fair_value_forward_eps REAL, fair_value_forward_eps_source TEXT, fair_value_safety_margin REAL,
 fair_value_confidence TEXT, fair_value_confidence_score REAL, fair_value_interest_coverage REAL,
 fair_value_regime TEXT, fair_value_regime_signals_json TEXT, fair_value_model_under_regime TEXT,
 fair_value_defensive_json TEXT, fair_value_optimistic_json TEXT,
 fair_value_normalized_eps_source TEXT, fair_value_quarterly_momentum_json TEXT,
 fair_value_historical_valuation_percentiles_json TEXT,
 fair_value_shareholder_total_return_yield REAL, fair_value_market_cap_data_quality TEXT,
 fair_value_data_quality_score REAL, fair_value_owner_earnings_maintenance_ratio REAL,
 fair_value_owner_earnings_ratio_source TEXT
);
INSERT INTO quote_snapshots(symbol, fetched_at, price, provider)
VALUES ('QQQ', '2030-01-18T15:00:00Z', 401.25, 'python');`)
	if err != nil {
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
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.SnapshotID == 0 || quote.Price == nil || *quote.Price != 401.25 || quote.RawJSON != nil {
		t.Fatalf("legacy quote did not survive raw_json migration: quote=%#v found=%v err=%v", quote, found, err)
	}
}

func TestOpenMigratesAllGoReadWriteCompatibilityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minimal-legacy-options.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE quote_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, fetched_at TEXT NOT NULL,
 price REAL, change_percent REAL, currency TEXT, market_state TEXT, raw_json TEXT
);
CREATE TABLE option_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, expiration TEXT NOT NULL,
 fetched_at TEXT NOT NULL, contract_symbol TEXT NOT NULL, contract_type TEXT NOT NULL,
 strike REAL, last_price REAL, bid REAL, ask REAL, volume INTEGER, open_interest INTEGER,
 implied_volatility REAL, raw_json TEXT,
 UNIQUE(symbol, expiration, fetched_at, contract_symbol)
);
CREATE TABLE push_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, kind TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE push_refresh_state (
 symbol TEXT NOT NULL, expiration TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL,
 PRIMARY KEY(symbol, expiration)
);
INSERT INTO push_refresh_state(symbol, expiration, started_at)
VALUES ('AAPL', '2030-01-18', '2000-01-01T00:00:00Z');
INSERT INTO quote_snapshots(symbol, fetched_at, price)
VALUES ('AAPL', '2030-01-18T15:00:00Z', 200);
INSERT INTO option_snapshots(symbol, expiration, fetched_at, contract_symbol, contract_type, strike, open_interest)
VALUES ('AAPL', '2030-01-18', '2030-01-18T15:00:00Z', 'AAPL-C-200', 'call', 200, 25);
INSERT INTO push_events(symbol, kind, created_at)
VALUES ('AAPL', 'snapshot', '2030-01-18T15:00:00Z');`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatalf("open minimal legacy schema: %v", err)
	}
	defer repository.Close()

	wantColumns := map[string][]string{
		"quote_snapshots": {
			"provider", "sessions_json", "today_open", "previous_close", "fair_value", "fair_value_low", "fair_value_high",
			"fair_value_buy_low", "fair_value_buy_high", "fair_value_source", "fair_value_model", "fair_value_methods_json",
			"fair_value_forward_eps", "fair_value_forward_eps_source", "fair_value_safety_margin", "fair_value_confidence",
			"fair_value_confidence_score", "fair_value_interest_coverage", "fair_value_regime", "fair_value_regime_signals_json",
			"fair_value_model_under_regime", "fair_value_defensive_json", "fair_value_optimistic_json",
			"fair_value_normalized_eps_source", "fair_value_quarterly_momentum_json", "fair_value_historical_valuation_percentiles_json",
			"fair_value_shareholder_total_return_yield", "fair_value_market_cap_data_quality", "fair_value_data_quality_score",
			"fair_value_owner_earnings_maintenance_ratio", "fair_value_owner_earnings_ratio_source",
		},
		"option_snapshots":   {"gamma", "in_the_money", "change_percent", "provider"},
		"push_events":        {"expiration"},
		"push_refresh_state": {"retry_after", "retry_delay_seconds", "last_interval_seconds", "lease_until"},
	}
	for table, columns := range wantColumns {
		for _, column := range columns {
			var count int
			if err := repository.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil {
				t.Fatalf("check %s.%s: %v", table, column, err)
			}
			if count != 1 {
				t.Errorf("missing compatibility column %s.%s", table, column)
			}
		}
	}

	quote, found, err := repository.LatestQuote("AAPL")
	if err != nil || !found || quote.Price == nil || *quote.Price != 200 {
		t.Fatalf("legacy quote unavailable after migration: quote=%#v found=%v err=%v", quote, found, err)
	}
	chain, found, err := repository.LatestChain("AAPL", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Provider != "" {
		t.Fatalf("legacy chain unavailable after migration: chain=%#v found=%v err=%v", chain, found, err)
	}
	if err := repository.SaveQuote(model.Quote{Symbol: "AAPL", Provider: "go", Price: quote.Price, Sessions: map[string]any{"pre": map[string]any{"price": 199.0}}}); err != nil {
		t.Fatalf("write quote after migration: %v", err)
	}
	if err := repository.SaveChain(model.ChainResponse{Symbol: "AAPL", Expiration: "2030-01-18", Source: "go", FetchedAt: func() *time.Time { now := time.Now().UTC(); return &now }(), Data: []model.OptionRow{{ContractSymbol: "AAPL-C-201", ContractType: "call", Provider: "go", Strike: func() *float64 { value := 201.0; return &value }()}}}); err != nil {
		t.Fatalf("write chain after migration: %v", err)
	}
	if _, err := repository.AddEvent(model.Event{Symbol: "AAPL", Kind: "snapshot", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("write event after migration: %v", err)
	}
}

func TestOpenUpgradesLegacyPythonSchemaWithoutLosingSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-options.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
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
INSERT INTO quote_snapshots(symbol,fetched_at,price,provider) VALUES ('QQQ','2030-01-18T15:00:00Z',401.25,'python');
INSERT INTO option_snapshots(symbol,expiration,fetched_at,contract_symbol,contract_type,strike,bid,ask,open_interest,provider)
VALUES ('QQQ','2099-01-16','2030-01-18T15:00:00Z','QQQ-C-400','call',400,2.1,2.3,20,'python');
INSERT INTO push_events(symbol,kind,created_at) VALUES ('QQQ','snapshot','2030-01-18T15:00:00Z');`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy schema: %v", err)
	}
	defer repository.Close()
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 401.25 || quote.Provider != "python" {
		t.Fatalf("legacy quote lost: quote=%#v found=%v err=%v", quote, found, err)
	}
	chain, found, err := repository.LatestChain("QQQ", "2099-01-16")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Gamma != nil || chain.Data[0].ContractSymbol != "QQQ-C-400" {
		t.Fatalf("legacy option snapshot lost or malformed: chain=%#v found=%v err=%v", chain, found, err)
	}
	cachedExpirations, err := repository.CachedExpirations("QQQ")
	if err != nil || !reflect.DeepEqual(cachedExpirations, []string{"2099-01-16"}) {
		t.Fatalf("legacy option expiration not advertised as cached: values=%#v err=%v", cachedExpirations, err)
	}
	events, err := repository.EventsSince(0, "QQQ", "")
	if err != nil || len(events) != 1 || events[0].Kind != "snapshot" {
		t.Fatalf("legacy push event lost: events=%#v err=%v", events, err)
	}
}

func TestOpenRepairsMissingBatchMarkersEvenWhenOtherSymbolsHaveMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-batch-markers.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE option_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, symbol TEXT NOT NULL, expiration TEXT NOT NULL,
 fetched_at TEXT NOT NULL, contract_symbol TEXT NOT NULL, contract_type TEXT NOT NULL,
 strike REAL, last_price REAL, bid REAL, ask REAL, volume INTEGER, open_interest INTEGER,
 implied_volatility REAL, gamma REAL, in_the_money INTEGER, change_percent REAL,
 provider TEXT NOT NULL, raw_json TEXT, UNIQUE(symbol, expiration, fetched_at, contract_symbol)
);
CREATE TABLE option_latest_batches (symbol TEXT NOT NULL, expiration TEXT NOT NULL, fetched_at TEXT NOT NULL, PRIMARY KEY(symbol, expiration));
INSERT INTO option_snapshots(symbol,expiration,fetched_at,contract_symbol,contract_type,strike,provider) VALUES
 ('AAA','2099-01-16','2030-01-18T15:00:00Z','AAA-C-1','call',1,'legacy'),
 ('BBB','2099-01-16','2030-01-18T15:01:00Z','BBB-C-1','call',1,'legacy');
INSERT INTO option_latest_batches(symbol,expiration,fetched_at) VALUES ('AAA','2099-01-16','2030-01-18T15:00:00Z');`)
	if err != nil {
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
	chain, found, err := repository.LatestChain("BBB", "2099-01-16")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].ContractSymbol != "BBB-C-1" {
		t.Fatalf("missing marker was not repaired for partially indexed DB: chain=%#v found=%v err=%v", chain, found, err)
	}
	values, err := repository.CachedExpirations("BBB")
	if err != nil || !reflect.DeepEqual(values, []string{"2099-01-16"}) {
		t.Fatalf("repaired batch not reported as cached: values=%#v err=%v", values, err)
	}
}

func TestPythonSQLiteSchemaRoundTripsThroughGoAndBack(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable; cross-language SQLite fixture requires Python")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate compatibility test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	pythonProject := pythonProjectRoot(projectRoot)
	databasePath := filepath.Join(t.TempDir(), "python-round-trip.sqlite")

	pythonCreateFixture := `
import sqlite3, sys
from app.db import Database

db = Database(sys.argv[1])
db.write_snapshot({
    "symbol": "QQQ", "price": 412.75, "change_percent": 0.75,
    "currency": "USD", "market_state": "REGULAR", "provider": "python-fixture",
    "sessions": {"pre": {"price": 413.25, "phase": "pre"}},
    "fair_value": 425.0, "fair_value_source": "valuation_test",
    "fair_value_methods": {"owner_earnings": {"value": 425.0}},
    "fair_value_regime_signals": {"vix": 19.5},
    "fair_value_defensive": {"value": 390.0},
    "fair_value_optimistic": {"value": 460.0},
    "fair_value_quarterly_momentum": {"growth": [0.1, 0.2]},
    "fair_value_historical_valuation_percentiles": {"p50": 400.0},
}, [{
    "symbol": "QQQ", "expiration": "2099-01-16",
    "contract_symbol": "QQQ300116C00400000", "contract_type": "call",
    "strike": 400.0, "last_price": 13.5, "bid": 13.4, "ask": 13.6,
    "volume": 50, "open_interest": 100, "implied_volatility": 0.21,
    "gamma": 0.01, "in_the_money": True, "change_percent": 1.5,
    "provider": "python-options",
}], "2030-01-18T15:00:00+00:00")
with db.connect() as connection:
    connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fixture_extension TEXT")
    connection.execute("CREATE INDEX fixture_extension_idx ON quote_snapshots(fixture_extension)")
    connection.execute("UPDATE quote_snapshots SET fixture_extension='preserved' WHERE symbol='QQQ'")
`
	cmd := exec.Command(python, "-c", pythonCreateFixture, databasePath)
	cmd.Dir = pythonProject
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create Python SQLite fixture: %v\n%s", err, output)
	}

	repository, err := Open(databasePath)
	if err != nil {
		t.Fatalf("open Python-created SQLite database: %v", err)
	}
	quote, found, err := repository.LatestQuote("QQQ")
	if err != nil || !found || quote.Price == nil || *quote.Price != 412.75 || quote.Provider != "python-fixture" {
		_ = repository.Close()
		t.Fatalf("read Python quote: quote=%#v found=%v err=%v", quote, found, err)
	}
	if sessions, ok := quote.Sessions["pre"].(map[string]any); !ok || sessions["price"] != 413.25 {
		_ = repository.Close()
		t.Fatalf("read Python sessions JSON: %#v", quote.Sessions)
	}
	if methods, ok := quote.FairValueMethods.(map[string]any); !ok || methods["owner_earnings"] == nil {
		_ = repository.Close()
		t.Fatalf("read Python valuation methods JSON: %#v", quote.FairValueMethods)
	}
	for name, value := range map[string]any{
		"regime signals": quote.FairValueRegimeSignals,
		"defensive":      quote.FairValueDefensive,
		"optimistic":     quote.FairValueOptimistic,
		"quarterly":      quote.FairValueQuarterlyMomentum,
	} {
		if _, ok := value.(map[string]any); !ok {
			_ = repository.Close()
			t.Fatalf("read Python %s JSON: %#v", name, value)
		}
	}
	chain, found, err := repository.LatestChain("QQQ", "2099-01-16")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].Provider != "python-options" || chain.Data[0].Gamma == nil || *chain.Data[0].Gamma != 0.01 {
		_ = repository.Close()
		t.Fatalf("read Python option batch: chain=%#v found=%v err=%v", chain, found, err)
	}
	var extension string
	if err := repository.db.QueryRow(`SELECT fixture_extension FROM quote_snapshots WHERE symbol='QQQ'`).Scan(&extension); err != nil || extension != "preserved" {
		_ = repository.Close()
		t.Fatalf("Go initialization changed an unrelated Python column: value=%q err=%v", extension, err)
	}
	var indexCount int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='fixture_extension_idx'`).Scan(&indexCount); err != nil || indexCount != 1 {
		_ = repository.Close()
		t.Fatalf("Go initialization changed an unrelated Python index: count=%d err=%v", indexCount, err)
	}

	writeAt := time.Now().UTC()
	volume, openInterest := int64(12), int64(80)
	if err := repository.SaveQuote(model.Quote{
		Symbol: "MSFT", Provider: "go-fixture", FetchedAt: &writeAt, Price: quoteTestPtr(430.25),
		Sessions: map[string]any{"post": map[string]any{"price": 431.0}},
	}); err != nil {
		_ = repository.Close()
		t.Fatalf("write Go quote to Python schema: %v", err)
	}
	if err := repository.SaveChain(model.ChainResponse{
		Symbol: "MSFT", Expiration: "2099-01-16", FetchedAt: &writeAt, Source: "go-options",
		Data: []model.OptionRow{{
			ContractSymbol: "MSFT300116P00400000", ContractType: "put", Provider: "go-options",
			Strike: quoteTestPtr(400), Bid: quoteTestPtr(12.1), Ask: quoteTestPtr(12.4),
			Volume: &volume, OpenInterest: &openInterest, Gamma: quoteTestPtr(0.02),
			InTheMoneyKnown: true, InTheMoney: false,
		}},
	}); err != nil {
		_ = repository.Close()
		t.Fatalf("write Go option batch to Python schema: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	pythonReadBack := `
import json, sys
from app.db import Database

db = Database(sys.argv[1])
quote = db.latest_quote("MSFT")
chain = db.latest_chain("MSFT", "2099-01-16")
with db.connect() as connection:
    chain_provider = connection.execute(
        "SELECT provider FROM option_snapshots WHERE symbol='MSFT' AND expiration='2099-01-16'"
    ).fetchone()[0]
structured = {}
for name, column in (
    ("regime_signals", "fair_value_regime_signals_json"),
    ("defensive", "fair_value_defensive_json"),
    ("optimistic", "fair_value_optimistic_json"),
    ("quarterly_momentum", "fair_value_quarterly_momentum_json"),
):
    raw = quote.get(column)
    structured[name] = json.loads(raw) if raw else None
quote["sessions"] = json.loads(quote.pop("sessions_json") or "{}")
print(json.dumps({"quote": quote, "structured": structured, "chain": chain, "chain_provider": chain_provider}, ensure_ascii=False))
`
	cmd = exec.Command(python, "-c", pythonReadBack, databasePath)
	cmd.Dir = pythonProject
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read Go snapshots through Python Database: %v\n%s", err, output)
	}
	var got struct {
		Quote         map[string]any `json:"quote"`
		Structured    map[string]any `json:"structured"`
		Chain         map[string]any `json:"chain"`
		ChainProvider string         `json:"chain_provider"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode Python readback %q: %v", output, err)
	}
	if got.Quote["provider"] != "go-fixture" || got.Quote["price"] != 430.25 {
		t.Errorf("Python quote reader returned Go quote %#v", got.Quote)
	}
	if sessions, ok := got.Quote["sessions"].(map[string]any); !ok || sessions["post"] == nil {
		t.Errorf("Python quote reader returned malformed Go sessions JSON: %#v", got.Quote["sessions"])
	}
	for _, name := range []string{"regime_signals", "defensive", "optimistic", "quarterly_momentum"} {
		value, ok := got.Structured[name].(map[string]any)
		if !ok || len(value) != 0 {
			t.Errorf("Python response conversion for empty %s = %#v, want empty object", name, got.Structured[name])
		}
	}
	chainData, ok := got.Chain["data"].([]any)
	if !ok || len(chainData) != 1 {
		t.Fatalf("Python chain reader returned Go batch %#v", got.Chain)
	}
	row, ok := chainData[0].(map[string]any)
	if got.ChainProvider != "go-options" || !ok || row["in_the_money"] != float64(0) || row["gamma"] != 0.02 {
		t.Errorf("Python chain reader returned malformed Go option row %#v", chainData[0])
	}
}

func TestPythonBatchSupersedesGoBatchWithinSameSecond(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable; cross-language SQLite fixture requires Python")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate compatibility test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	pythonProject := pythonProjectRoot(projectRoot)
	databasePath := filepath.Join(t.TempDir(), "python-go-ordering.sqlite")

	repository, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	goTime := time.Date(2030, time.January, 18, 15, 0, 0, 100_000_000, time.UTC)
	if err := repository.SaveChain(model.ChainResponse{
		Symbol: "QQQ", Expiration: "2030-01-18", FetchedAt: &goTime,
		Data: []model.OptionRow{{ContractSymbol: "GO-OLDER", ContractType: "call"}},
	}); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	pythonWrite := `
import sys
from app.db import Database

db = Database(sys.argv[1])
db.write_snapshot(
    {"symbol": "QQQ", "provider": "python"},
    [{"symbol": "QQQ", "expiration": "2030-01-18", "contract_symbol": "PY-NEWER", "contract_type": "call"}],
    "2030-01-18T15:00:00.100001+00:00",
)
latest = db.latest_chain("QQQ", "2030-01-18")
assert [row["contract_symbol"] for row in latest["data"]] == ["PY-NEWER"], latest
`
	cmd := exec.Command(python, "-c", pythonWrite, databasePath)
	cmd.Dir = pythonProject
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python did not select its newer option batch after Go write: %v\n%s", err, output)
	}

	repository, err = Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	chain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(chain.Data) != 1 || chain.Data[0].ContractSymbol != "PY-NEWER" {
		t.Fatalf("Go did not select Python's newer option batch: chain=%#v found=%v err=%v", chain, found, err)
	}
}

func TestPythonAndGoOptionInTheMoneyStorageSemantics(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable; cross-language SQLite fixture requires Python")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate compatibility test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	pythonProject := pythonProjectRoot(projectRoot)
	databasePath := filepath.Join(t.TempDir(), "python-go-in-the-money.sqlite")

	pythonCreate := `
import sys
from app.db import Database

db = Database(sys.argv[1])
db.write_snapshot(
    {"symbol": "QQQ", "provider": "python"},
    [{"symbol": "QQQ", "expiration": "2030-01-18", "contract_symbol": "PY-UNKNOWN", "contract_type": "call"}],
    "2030-01-18T15:00:00.000001+00:00",
)
`
	cmd := exec.Command(python, "-c", pythonCreate, databasePath)
	cmd.Dir = pythonProject
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create Python in-the-money fixture: %v\n%s", err, output)
	}

	repository, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	pythonChain, found, err := repository.LatestChain("QQQ", "2030-01-18")
	if err != nil || !found || len(pythonChain.Data) != 1 || !pythonChain.Data[0].InTheMoneyKnown || pythonChain.Data[0].InTheMoney {
		_ = repository.Close()
		t.Fatalf("Go read of Python's omitted in_the_money=%#v found=%v err=%v", pythonChain.Data, found, err)
	}
	goTime := time.Date(2030, time.January, 18, 15, 0, 0, 200_000, time.UTC)
	if err := repository.SaveChain(model.ChainResponse{
		Symbol: "SPY", Expiration: "2030-01-18", FetchedAt: &goTime,
		Data: []model.OptionRow{{ContractSymbol: "GO-UNKNOWN", ContractType: "call"}},
	}); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	pythonRead := `
import sys
from app.db import Database

db = Database(sys.argv[1])
row = db.latest_chain("SPY", "2030-01-18")["data"][0]
assert row["contract_symbol"] == "GO-UNKNOWN", row
assert row["in_the_money"] is None, row
`
	cmd = exec.Command(python, "-c", pythonRead, databasePath)
	cmd.Dir = pythonProject
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python did not read Go's nullable in_the_money: %v\n%s", err, output)
	}
}

func pythonProjectRoot(goProjectRoot string) string {
	candidate := filepath.Join(filepath.Dir(goProjectRoot), filepath.Base(goProjectRoot)+"_py")
	if _, err := os.Stat(filepath.Join(candidate, "app", "db.py")); err == nil {
		return candidate
	}
	return goProjectRoot
}
