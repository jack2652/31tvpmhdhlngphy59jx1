package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesDotEnvBelowProcessEnvironment(t *testing.T) {
	keys := []string{"HOST", "PORT", "DEFAULT_SYMBOLS", "ACCESS_KEY", "DATABASE_PATH", "MARKET_PROXY", "RAW_RETENTION_DAYS", "DATABASE_MAX_MB", "SIZE_CLEANUP_TIME_BUDGET_SECONDS", "SCHEDULER_ENABLED", "HISTORY_MAX_AGE_SECONDS", "EXTREMES_MAX_AGE_SECONDS", "UPSTREAM_TIMEOUT_SECONDS", "CBOE_TIMEOUT_SECONDS", "RESOURCE_MODE", "ANALYSIS_CACHE_ENTRIES", "ALPACA_API_KEY", "ALPACA_API_SECRET"}
	oldValues := make(map[string]string, len(keys))
	oldPresent := make(map[string]bool, len(keys))
	for _, key := range keys {
		oldValues[key], oldPresent[key] = os.LookupEnv(key)
		_ = os.Unsetenv(key)
	}
	t.Cleanup(func() {
		for _, key := range keys {
			if oldPresent[key] {
				_ = os.Setenv(key, oldValues[key])
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})
	temporary := t.TempDir()
	if err := os.WriteFile(filepath.Join(temporary, ".env"), []byte(strings.Join([]string{"HOST=127.0.0.1", "PORT=8001", "DEFAULT_SYMBOLS= aapl,MSFT", "ACCESS_KEY=file-key", "DATABASE_PATH=temporary.db", "MARKET_PROXY=http://proxy.example:3128", "RAW_RETENTION_DAYS=45", ""}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("PORT", "8002"); err != nil {
		t.Fatal(err)
	}
	loaded := LoadFromDir(temporary)
	if loaded.Host != "127.0.0.1" || loaded.Port != "8002" || loaded.DefaultSymbol != "AAPL" || loaded.AccessKey != "file-key" || loaded.DatabasePath != filepath.Join(temporary, "temporary.db") || loaded.MarketProxy != "http://proxy.example:3128" || loaded.RawRetentionDays != 45 || loaded.UpstreamTimeout != 12*time.Second || loaded.CboeTimeout != 15*time.Second {
		t.Fatalf("Load() did not honor dotenv values and process precedence: %#v", loaded)
	}
}

func TestLoadUsesPythonSizeCleanupBudgetDefault(t *testing.T) {
	old, had := os.LookupEnv("SIZE_CLEANUP_TIME_BUDGET_SECONDS")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("SIZE_CLEANUP_TIME_BUDGET_SECONDS", old)
		} else {
			_ = os.Unsetenv("SIZE_CLEANUP_TIME_BUDGET_SECONDS")
		}
	})
	_ = os.Unsetenv("SIZE_CLEANUP_TIME_BUDGET_SECONDS")
	if got := Load().SizeCleanupBudget; got != 120*time.Second {
		t.Fatalf("default size cleanup budget=%s want 120s (Python parity)", got)
	}
	_ = os.Setenv("SIZE_CLEANUP_TIME_BUDGET_SECONDS", "0")
	if got := Load().SizeCleanupBudget; got != 0 {
		t.Fatalf("explicit zero size cleanup budget=%s want 0", got)
	}
}

func TestInboundConcurrencyMatchesPythonLimits(t *testing.T) {
	if got := inboundConcurrency(false); got != 64 {
		t.Fatalf("normal inbound concurrency=%d, want 64", got)
	}
	if got := inboundConcurrency(true); got != 12 {
		t.Fatalf("low-memory inbound concurrency=%d, want 12", got)
	}
}

func TestLoadDefaultsEmptyDatabasePathToRuntimeDataDirectory(t *testing.T) {
	previousDatabasePath, hadDatabasePath := os.LookupEnv("DATABASE_PATH")
	_ = os.Unsetenv("DATABASE_PATH")
	t.Cleanup(func() {
		if hadDatabasePath {
			_ = os.Setenv("DATABASE_PATH", previousDatabasePath)
		} else {
			_ = os.Unsetenv("DATABASE_PATH")
		}
	})
	runtimeDir := t.TempDir()
	if got := LoadFromDir(runtimeDir).DatabasePath; got != filepath.Join(runtimeDir, "data", "options.db") {
		t.Fatalf("explicit empty DATABASE_PATH=%q, want runtime data path", got)
	}
}

func TestLoadUsesRuntimeDirectoryInsteadOfWorkingDirectory(t *testing.T) {
	runtimeDir := t.TempDir()
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, ".env"), []byte("DATABASE_PATH=data/runtime.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte("DATABASE_PATH=data/wrong.db\nHOST=127.0.0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	previousDatabasePath, hadDatabasePath := os.LookupEnv("DATABASE_PATH")
	previousHost, hadHost := os.LookupEnv("HOST")
	_ = os.Unsetenv("DATABASE_PATH")
	_ = os.Unsetenv("HOST")
	t.Cleanup(func() {
		if hadDatabasePath {
			_ = os.Setenv("DATABASE_PATH", previousDatabasePath)
		} else {
			_ = os.Unsetenv("DATABASE_PATH")
		}
		if hadHost {
			_ = os.Setenv("HOST", previousHost)
		} else {
			_ = os.Unsetenv("HOST")
		}
	})
	loaded := LoadFromDir(runtimeDir)
	if got, want := loaded.DatabasePath, filepath.Join(runtimeDir, "data", "runtime.db"); got != want {
		t.Fatalf("database path=%q, want %q", got, want)
	}
	if loaded.Host != "0.0.0.0" {
		t.Fatalf("host=%q, want runtime .env to override working-directory .env", loaded.Host)
	}
}

func TestLoadParsesDatabaseSizeUnits(t *testing.T) {
	old, had := os.LookupEnv("DATABASE_MAX_MB")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("DATABASE_MAX_MB", old)
		} else {
			_ = os.Unsetenv("DATABASE_MAX_MB")
		}
	})
	for raw, want := range map[string]int64{"256M": 256, "1G": 1024, "300MB": 300, "0": 0} {
		if err := os.Setenv("DATABASE_MAX_MB", raw); err != nil {
			t.Fatal(err)
		}
		if got := Load().DatabaseMaxMB; got != want {
			t.Errorf("DATABASE_MAX_MB=%q got %d want %d", raw, got, want)
		}
	}
	if err := os.Setenv("DATABASE_MAX_MB", "bad"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("DATABASE_MAX_MB=bad should fail configuration loading")
		}
	}()
	_ = Load()
}

func TestLoadRejectsDatabaseSizeOverflow(t *testing.T) {
	t.Setenv("DATABASE_MAX_MB", "9007199254740992G")
	defer func() {
		if recover() == nil {
			t.Error("DATABASE_MAX_MB too large for int64 should fail configuration loading")
		}
	}()
	_ = Load()
}

func TestLoadDotEnvPreservesEnvironmentAndParsesValues(t *testing.T) {
	const overridden = "OPTION_SCOPE_TEST_DOTENV_OVERRIDE"
	const plain = "OPTION_SCOPE_TEST_DOTENV_PLAIN"
	const quoted = "OPTION_SCOPE_TEST_DOTENV_QUOTED"
	const exported = "OPTION_SCOPE_TEST_DOTENV_EXPORTED"
	const quotedComment = "OPTION_SCOPE_TEST_DOTENV_QUOTED_COMMENT"
	const referenced = "OPTION_SCOPE_TEST_DOTENV_REFERENCED"
	const fromProcess = "OPTION_SCOPE_TEST_DOTENV_FROM_PROCESS"
	const fromProcessRef = "OPTION_SCOPE_TEST_DOTENV_PROCESS_REFERENCE"
	for _, key := range []string{overridden, plain, quoted, exported, quotedComment, referenced, fromProcess, fromProcessRef} {
		old, had := os.LookupEnv(key)
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
		_ = os.Unsetenv(key)
	}
	if err := os.Setenv(overridden, "process-value"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(fromProcess, "host-value"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n" +
		overridden + "=file-value\n" +
		plain + "=plain value # inline comment\n" +
		quoted + "=\"quoted # value\"\n" +
		"export " + exported + "='exported value'\n" +
		quotedComment + "=\"http://proxy.example:7890\" # trailing comment\n" +
		referenced + "=${" + exported + "}/data\n" +
		fromProcessRef + "=${" + overridden + "}\n" +
		"invalid line\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loadDotEnv(path)
	checks := map[string]string{
		overridden:     "process-value",
		plain:          "plain value",
		quoted:         "quoted # value",
		exported:       "exported value",
		quotedComment:  "http://proxy.example:7890",
		referenced:     "exported value/data",
		fromProcess:    "host-value",
		fromProcessRef: "process-value",
	}
	for key, want := range checks {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLoadRejectsInvalidPositiveIntegerSettings(t *testing.T) {
	keys := []string{
		"RAW_RETENTION_DAYS", "HISTORY_MAX_AGE_SECONDS", "EXTREMES_MAX_AGE_SECONDS",
		"UPSTREAM_TIMEOUT_SECONDS", "CBOE_TIMEOUT_SECONDS", "ANALYSIS_CACHE_ENTRIES",
	}
	oldValues := make(map[string]string, len(keys))
	oldPresent := make(map[string]bool, len(keys))
	for _, key := range keys {
		oldValues[key], oldPresent[key] = os.LookupEnv(key)
	}
	t.Cleanup(func() {
		for _, key := range keys {
			if oldPresent[key] {
				_ = os.Setenv(key, oldValues[key])
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})
	// Keep every positive-int path active regardless of the host's detected
	// memory profile.
	t.Setenv("RESOURCE_MODE", "off")
	for _, key := range keys {
		for _, raw := range []string{"", "0", "-1", "not-an-int"} {
			if err := os.Setenv(key, raw); err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s=%q should reject non-positive/non-integer value", key, raw)
					}
				}()
				_ = Load()
			}()
		}
		_ = os.Unsetenv(key)
	}
}

func TestLoadParsesRuntimeControlsWithResourceModeOff(t *testing.T) {
	keys := map[string]string{
		"DEFAULT_SYMBOLS": " aapl, MSFT, aapl ", "SCHEDULER_ENABLED": "false",
		"HISTORY_MAX_AGE_SECONDS": "7200", "EXTREMES_MAX_AGE_SECONDS": "172800",
		"RESOURCE_MODE": "off", "ANALYSIS_CACHE_ENTRIES": "64",
		"ALPACA_API_KEY": "key", "ALPACA_API_SECRET": "secret",
	}
	old := map[string]string{}
	had := map[string]bool{}
	for key, value := range keys {
		old[key], had[key] = os.LookupEnv(key)
		if err := os.Setenv(key, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for key := range keys {
			if had[key] {
				_ = os.Setenv(key, old[key])
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})
	loaded := Load()
	if strings.Join(loaded.DefaultSymbols, ",") != "AAPL,MSFT" || loaded.DefaultSymbol != "AAPL" || loaded.SchedulerEnabled || loaded.HistoryMaxAge != 2*time.Hour || loaded.ExtremesMaxAge != 48*time.Hour {
		t.Fatalf("runtime settings mismatch: %#v", loaded)
	}
	if loaded.ResourceMode != "off" || loaded.LowMemory || loaded.UpstreamConcurrency != 6 || loaded.UpstreamWait != 20*time.Second || loaded.AnalysisCacheEntries != 64 {
		t.Fatalf("resource mode controls mismatch: %#v", loaded)
	}
	if loaded.GoMemoryLimitBytes != math.MaxInt64 || loaded.GoMemoryLimitSource != "off" {
		t.Fatalf("resource mode off should remove the Go soft limit: bytes=%d source=%q", loaded.GoMemoryLimitBytes, loaded.GoMemoryLimitSource)
	}
	if loaded.AlpacaAPIKey != "key" || loaded.AlpacaAPISecret != "secret" {
		t.Fatalf("provider credentials mismatch: %#v", loaded)
	}
}

func TestResourceModeRejectsUnknownValues(t *testing.T) {
	t.Setenv("RESOURCE_MODE", "manual")
	defer func() {
		if recover() == nil {
			t.Fatal("RESOURCE_MODE=manual should be rejected")
		}
	}()
	_ = Load()
}

func TestAutoResourceCalculationsFollowDetectedBudget(t *testing.T) {
	if !autoLowMemoryEnabled(256<<20, 200<<20) {
		t.Fatal("small cgroup limit should enable low-memory mode")
	}
	if autoLowMemoryEnabled(8<<30, 4<<30) {
		t.Fatal("large host budget should stay in normal mode")
	}
	if got := upstreamConcurrencyForResources(110<<20, 2, defaultUpstreamConcurrency); got != 1 {
		t.Fatalf("110 MiB budget concurrency=%d, want 1", got)
	}
	if got := upstreamConcurrencyForResources(4<<30, 8, defaultUpstreamConcurrency); got != defaultUpstreamConcurrency {
		t.Fatalf("large host concurrency=%d, want cap %d", got, defaultUpstreamConcurrency)
	}
	if got := upstreamWaitDuration(true); got != 5*time.Second {
		t.Fatalf("low-memory slot wait=%s, want 5s", got)
	}
	if got := upstreamWaitDuration(false); got != 20*time.Second {
		t.Fatalf("normal slot wait=%s, want 20s", got)
	}
}
