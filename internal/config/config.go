package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config contains the small set of runtime settings needed by the Go service.
// It deliberately mirrors the deployable settings used by the Python service.
type Config struct {
	DotEnvPath           string
	DotEnvStatus         string
	PortSource           string
	Host                 string
	Port                 string
	DatabasePath         string
	AccessKey            string
	MarketProxy          string
	DefaultSymbol        string
	DefaultSymbols       []string
	RawRetentionDays     int
	DatabaseMaxMB        int64
	SchedulerEnabled     bool
	HistoryMaxAge        time.Duration
	ExtremesMaxAge       time.Duration
	UpstreamTimeout      time.Duration
	CboeTimeout          time.Duration
	UpstreamWait         time.Duration
	UpstreamConcurrency  int
	MemoryLimitBytes     int64
	MemoryAvailableBytes int64
	UpstreamMemoryBytes  int64
	GoMemoryLimitBytes   int64
	GoMemoryLimitSource  string
	InboundConcurrency   int
	LowMemoryMode        string
	LowMemory            bool
	AnalysisCacheEntries int
	AlpacaAPIKey         string
	AlpacaAPISecret      string
	SizeCleanupBudget    time.Duration
}

const (
	defaultUpstreamConcurrency = 6
	upstreamMemoryPerSlot      = 160 * 1024 * 1024
	upstreamMemoryReserve      = 64 * 1024 * 1024
	lowMemoryThreshold         = 512 * 1024 * 1024
	autoLowMemoryLimit         = 4 * 1024 * 1024 * 1024
	autoLowMemoryAvailable     = 2 * 1024 * 1024 * 1024
)

func Load() Config {
	return LoadFromDir(runtimeDirectory())
}

// LoadFromDir loads the runtime configuration using baseDir as the deployment
// directory. The .env file and all relative filesystem settings are resolved
// from this directory rather than from the process working directory.
func LoadFromDir(baseDir string) Config {
	baseDir = cleanRuntimeDirectory(baseDir)
	_, portFromEnvironment := os.LookupEnv("PORT")
	dotEnvPath := filepath.Join(baseDir, ".env")
	dotEnvStatus := "loaded"
	if err := loadDotEnv(dotEnvPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			panic(fmt.Sprintf("load configuration %q: %v", dotEnvPath, err))
		}
		dotEnvStatus = "missing"
	}
	portSource := "default"
	if strings.TrimSpace(os.Getenv("PORT")) != "" {
		portSource = ".env"
		if portFromEnvironment {
			portSource = "environment"
		}
	} else if portFromEnvironment {
		portSource = "default (empty environment PORT)"
	}
	memoryLimit := effectiveMemoryLimitBytes()
	memoryAvailable := effectiveMemoryAvailableBytes(memoryLimit)
	upstreamMemory := memoryAvailable
	goMemoryLimit, goMemorySource := goMemoryLimitSetting(memoryLimit, memoryAvailable)
	resourceLimit := memoryLimit
	if goMemoryLimit >= 0 && goMemoryLimit < math.MaxInt64 {
		if resourceLimit <= 0 || goMemoryLimit < resourceLimit {
			resourceLimit = goMemoryLimit
		}
		if upstreamMemory < 0 || goMemoryLimit < upstreamMemory {
			upstreamMemory = goMemoryLimit
		}
	}
	lowMemoryMode := lowMemoryModeValue()
	lowMemory := lowMemoryEnabled(lowMemoryMode, resourceLimit, upstreamMemory)
	defaultSymbols := parseSymbols(env("DEFAULT_SYMBOLS", "QQQ"))
	defaultSymbol := "QQQ"
	if len(defaultSymbols) > 0 {
		defaultSymbol = defaultSymbols[0]
	}
	databasePath, ok := os.LookupEnv("DATABASE_PATH")
	if !ok || strings.TrimSpace(databasePath) == "" {
		databasePath = filepath.Join(baseDir, "data", "options.db")
	} else if !isSQLiteURI(databasePath) && !filepath.IsAbs(databasePath) {
		databasePath = filepath.Join(baseDir, databasePath)
	}
	return Config{
		DotEnvPath:           dotEnvPath,
		DotEnvStatus:         dotEnvStatus,
		PortSource:           portSource,
		Host:                 env("HOST", "0.0.0.0"),
		Port:                 env("PORT", "8000"),
		DatabasePath:         databasePath,
		AccessKey:            strings.TrimSpace(os.Getenv("ACCESS_KEY")),
		MarketProxy:          env("MARKET_PROXY", ""),
		DefaultSymbol:        defaultSymbol,
		DefaultSymbols:       defaultSymbols,
		RawRetentionDays:     envInt("RAW_RETENTION_DAYS", 30),
		DatabaseMaxMB:        envSizeMB("DATABASE_MAX_MB", 0),
		SchedulerEnabled:     envBool("SCHEDULER_ENABLED", true),
		HistoryMaxAge:        time.Duration(envInt("HISTORY_MAX_AGE_SECONDS", 3600)) * time.Second,
		ExtremesMaxAge:       time.Duration(envInt("EXTREMES_MAX_AGE_SECONDS", 86400)) * time.Second,
		UpstreamTimeout:      time.Duration(envInt("UPSTREAM_TIMEOUT_SECONDS", 12)) * time.Second,
		CboeTimeout:          time.Duration(envInt("CBOE_TIMEOUT_SECONDS", 15)) * time.Second,
		UpstreamWait:         upstreamWaitDuration(lowMemory),
		UpstreamConcurrency:  effectiveConcurrency(upstreamMemory, lowMemory),
		MemoryLimitBytes:     memoryLimit,
		MemoryAvailableBytes: memoryAvailable,
		UpstreamMemoryBytes:  upstreamMemory,
		GoMemoryLimitBytes:   goMemoryLimit,
		GoMemoryLimitSource:  goMemorySource,
		InboundConcurrency:   inboundConcurrency(lowMemory),
		LowMemoryMode:        lowMemoryMode,
		LowMemory:            lowMemory,
		AnalysisCacheEntries: effectiveAnalysisCacheEntries(lowMemory),
		AlpacaAPIKey:         strings.TrimSpace(os.Getenv("ALPACA_API_KEY")),
		AlpacaAPISecret:      strings.TrimSpace(os.Getenv("ALPACA_API_SECRET")),
		SizeCleanupBudget:    time.Duration(envIntAllowZero("SIZE_CLEANUP_TIME_BUDGET_SECONDS", 120)) * time.Second,
	}
}

func runtimeDirectory() string {
	executable, err := os.Executable()
	if err == nil && executable != "" {
		if isGoRunArtifact(executable) {
			if workingDirectory, err := os.Getwd(); err == nil {
				return workingDirectory
			}
		}
		return filepath.Dir(executable)
	}
	workingDirectory, err := os.Getwd()
	if err == nil {
		return workingDirectory
	}
	return "."
}

var (
	goRunTemporaryExecutable = regexp.MustCompile(`(?:^|/)go-build[0-9]+/b[0-9]+/exe/[^/]+$`)
	goRunCachedExecutable    = regexp.MustCompile(`(?:^|/)([0-9a-f]{2})/([0-9a-f]{64})-d/[^/]+$`)
)

func isGoRunArtifact(executable string) bool {
	path := filepath.ToSlash(filepath.Clean(executable))
	if goRunTemporaryExecutable.MatchString(path) {
		return true
	}
	// Go can execute cache hits directly from <cache>/<hash[:2]>/<hash>-d/<name>.
	// Match the artifact layout rather than its root so custom GOTMPDIR and
	// GOCACHE paths work too. A cache directory is never a deployment directory.
	match := goRunCachedExecutable.FindStringSubmatch(path)
	return len(match) == 3 && strings.HasPrefix(match[2], match[1])
}

func cleanRuntimeDirectory(directory string) string {
	if strings.TrimSpace(directory) == "" {
		directory = runtimeDirectory()
	}
	abs, err := filepath.Abs(directory)
	if err == nil {
		directory = abs
	}
	return filepath.Clean(directory)
}

func isSQLiteURI(path string) bool {
	trimmed := strings.TrimSpace(path)
	return trimmed == ":memory:" || strings.HasPrefix(trimmed, "file:")
}

func inboundConcurrency(lowMemory bool) int {
	if lowMemory {
		return 12
	}
	return 64
}

var dotenvVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		if _, exists := os.LookupEnv(name); exists {
			continue
		}
		// Strip an unquoted inline comment first, then remove surrounding
		// quotes. This accepts `KEY="value" # comment` like run.sh.
		if !(len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"'))) {
			if index := strings.Index(value, " #"); index >= 0 {
				value = strings.TrimSpace(value[:index])
			}
		}
		singleQuoted := len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\''
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if !singleQuoted {
			value = dotenvVariable.ReplaceAllStringFunc(value, func(match string) string {
				name := match[2 : len(match)-1]
				return os.Getenv(name)
			})
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("set environment variable %q: %w", name, err)
		}
	}
	return scanner.Err()
}

func parseSymbols(raw string) []string {
	seen := map[string]bool{}
	values := make([]string, 0)
	for _, candidate := range strings.Split(raw, ",") {
		value := strings.ToUpper(strings.TrimSpace(candidate))
		if value != "" && !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return []string{"QQQ"}
	}
	return values
}

func envBool(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	if strings.TrimSpace(raw) == "" {
		// Python's _bool treats an explicitly empty value as false; only an
		// unset variable uses the configured default.
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		// Python's _bool treats an unrecognized non-empty value as false.
		return false
	}
}

func lowMemoryModeValue() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOW_MEMORY"))) {
	case "1", "true", "yes", "on":
		return "true"
	case "0", "false", "no", "off":
		return "false"
	default:
		return "auto"
	}
}

func lowMemoryEnabled(mode string, memoryLimit, memoryAvailable int64) bool {
	switch mode {
	case "true":
		return true
	case "false":
		return false
	}
	if memoryLimit > 0 && memoryLimit <= autoLowMemoryLimit {
		return true
	}
	return memoryAvailable >= 0 && memoryAvailable <= autoLowMemoryAvailable
}

// effectiveMemoryLimitBytes returns the smallest visible memory limit for the
// process. Container/cgroup limits take precedence over host RAM, while MemTotal
// provides the fallback on hosts without cgroup memory controls.
func effectiveMemoryLimitBytes() int64 {
	limits := make([]int64, 0, 4)
	for _, paths := range memoryCgroupFiles() {
		value, err := os.ReadFile(paths.limit)
		if err != nil {
			continue
		}
		limit := strings.TrimSpace(string(value))
		if limit == "max" || limit == "" {
			continue
		}
		if bytes, err := strconv.ParseInt(limit, 10, 64); err == nil && bytes > 0 && bytes < 1<<60 {
			limits = append(limits, bytes)
		}
	}
	// Older hosts and non-container deployments may not expose cgroup memory
	// files. Match Python's MemTotal fallback in that case.
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				if kilobytes, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kilobytes > 0 && kilobytes <= math.MaxInt64/1024 {
					limits = append(limits, kilobytes*1024)
				}
				break
			}
		}
	}
	if len(limits) == 0 {
		return 0
	}
	limit := limits[0]
	for _, candidate := range limits[1:] {
		if candidate < limit {
			limit = candidate
		}
	}
	return limit
}

type cgroupMemoryFiles struct {
	limit string
	usage string
}

func memoryCgroupFiles() []cgroupMemoryFiles {
	paths := []cgroupMemoryFiles{
		{limit: "/sys/fs/cgroup/memory.max", usage: "/sys/fs/cgroup/memory.current"},
		{limit: "/sys/fs/cgroup/memory/memory.limit_in_bytes", usage: "/sys/fs/cgroup/memory/memory.usage_in_bytes"},
	}
	mounts := memoryCgroupMounts()
	for _, mount := range mounts {
		limitName, usageName := "memory.limit_in_bytes", "memory.usage_in_bytes"
		if mount.v2 {
			limitName, usageName = "memory.max", "memory.current"
		}
		paths = append(paths, cgroupMemoryFiles{limit: filepath.Join(mount.directory, limitName), usage: filepath.Join(mount.directory, usageName)})
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return paths
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		v2 := parts[0] == "0" && parts[1] == ""
		if !v2 && !strings.Contains(","+parts[1]+",", ",memory,") {
			continue
		}
		for _, mount := range mounts {
			if mount.v2 != v2 {
				continue
			}
			relative, err := filepath.Rel(mount.root, filepath.Clean(parts[2]))
			if err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
				// Cgroup namespaces can report a path relative to their own
				// root while mountinfo retains the host mount root.
				relative = strings.TrimPrefix(filepath.Clean(parts[2]), string(filepath.Separator))
			}
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			limitName, usageName := "memory.limit_in_bytes", "memory.usage_in_bytes"
			if v2 {
				limitName, usageName = "memory.max", "memory.current"
			}
			// A parent's limit also constrains the process even when the leaf
			// cgroup is unlimited; consider every visible ancestor.
			for directory := filepath.Join(mount.directory, relative); ; directory = filepath.Dir(directory) {
				paths = append(paths, cgroupMemoryFiles{limit: filepath.Join(directory, limitName), usage: filepath.Join(directory, usageName)})
				if directory == mount.directory {
					break
				}
			}
		}
	}
	return paths
}

func effectiveMemoryAvailableBytes(fallbackLimit int64) int64 {
	if fallbackLimit <= 0 {
		fallbackLimit = -1
	}
	cgroupAvailable := effectiveMemoryAvailableFromPaths(memoryCgroupFiles(), fallbackLimit)
	hostAvailable := hostMemoryAvailableBytes()
	if hostAvailable < 0 {
		return cgroupAvailable
	}
	if cgroupAvailable < 0 || hostAvailable < cgroupAvailable {
		return hostAvailable
	}
	return cgroupAvailable
}

func effectiveMemoryAvailableFromPaths(paths []cgroupMemoryFiles, fallbackLimit int64) int64 {
	available := make([]int64, 0, 4)
	for _, files := range paths {
		limitData, limitErr := os.ReadFile(files.limit)
		if limitErr != nil {
			continue
		}
		limitText := strings.TrimSpace(string(limitData))
		limit, err := strconv.ParseInt(limitText, 10, 64)
		if err != nil || limit <= 0 || limit >= 1<<60 {
			continue
		}
		usageData, usageErr := os.ReadFile(files.usage)
		if usageErr != nil {
			available = append(available, limit)
			continue
		}
		usage, err := strconv.ParseInt(strings.TrimSpace(string(usageData)), 10, 64)
		if err != nil || usage < 0 {
			available = append(available, limit)
			continue
		}
		available = append(available, max(limit-usage, 0))
	}
	if len(available) == 0 {
		return fallbackLimit
	}
	budget := available[0]
	for _, candidate := range available[1:] {
		if candidate < budget {
			budget = candidate
		}
	}
	return budget
}

func hostMemoryAvailableBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	free := int64(-1)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" && fields[0] != "MemFree:" {
			continue
		}
		kilobytes, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || kilobytes < 0 || kilobytes > math.MaxInt64/1024 {
			continue
		}
		if fields[0] == "MemAvailable:" {
			return kilobytes * 1024
		}
		free = kilobytes * 1024
	}
	// Older kernels and some container meminfo views omit MemAvailable.
	return free
}

func upstreamWaitDuration(lowMemory bool) time.Duration {
	seconds := envInt("UPSTREAM_WAIT_SECONDS", 20)
	if lowMemory && seconds > 5 {
		seconds = 5
	}
	return time.Duration(seconds) * time.Second
}

func effectiveConcurrency(memoryBudget int64, lowMemory bool) int {
	configuredCap := defaultUpstreamConcurrency
	if raw, present := os.LookupEnv("UPSTREAM_CONCURRENCY"); present && strings.ToLower(strings.TrimSpace(raw)) != "auto" {
		configuredCap = envInt("UPSTREAM_CONCURRENCY", defaultUpstreamConcurrency)
	} else if raw, present := os.LookupEnv("UPSTREAM_CONCURRENCY"); present && strings.TrimSpace(raw) == "" {
		panic("UPSTREAM_CONCURRENCY must be auto or an integer >= 1")
	}
	if lowMemory {
		// LOW_MEMORY=true is an explicit request for the conservative resource
		// profile even when the host itself has more memory available.
		memoryBudget = min(memoryBudget, lowMemoryThreshold)
	}
	return upstreamConcurrencyForResources(memoryBudget, runtime.GOMAXPROCS(0), configuredCap)
}

// upstreamConcurrencyForResources treats UPSTREAM_CONCURRENCY as a ceiling.
// The runtime budget scales with memory available to hold concurrent upstream
// response/decode working sets, and with CPU capacity to bound decode work.
func upstreamConcurrencyForResources(memoryBytes int64, cpuCount, configuredCap int) int {
	if cpuCount < 1 {
		cpuCount = 1
	}
	if memoryBytes <= 0 {
		return 1
	}
	if memoryBytes > upstreamMemoryReserve {
		memoryBytes -= upstreamMemoryReserve
	} else {
		memoryBytes = 0
	}
	concurrency := int(memoryBytes / upstreamMemoryPerSlot)
	if concurrency < 1 {
		concurrency = 1
	}
	cpuBudget := cpuCount * 2
	if concurrency > cpuBudget {
		concurrency = cpuBudget
	}
	if configuredCap > 0 && concurrency > configuredCap {
		concurrency = configuredCap
	}
	return concurrency
}

func effectiveAnalysisCacheEntries(lowMemory bool) int {
	if lowMemory {
		return 8
	}
	return envInt("ANALYSIS_CACHE_ENTRIES", 128)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// envInt mirrors Python's _positive_int helper: an unset variable
// uses the fallback, while an explicitly configured value must parse as an
// integer at least one. In particular, empty strings and zero are invalid.
func envInt(name string, fallback int) int {
	raw, present := os.LookupEnv(name)
	if !present {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 1 {
		panic(fmt.Sprintf("%s must be an integer >= 1", name))
	}
	return value
}

func envIntAllowZero(name string, fallback int) int {
	raw, present := os.LookupEnv(name)
	if !present {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		panic(fmt.Sprintf("%s must be an integer >= 0", name))
	}
	return value
}

func envSizeMB(name string, fallback int64) int64 {
	rawValue, present := os.LookupEnv(name)
	if !present {
		return fallback
	}
	raw := strings.ToUpper(strings.TrimSpace(rawValue))
	if raw == "" {
		panic(fmt.Sprintf("%s must be an integer size such as 300M or 1G", name))
	}
	multiplier := int64(1)
	matchedSuffix := false
	for _, suffix := range []string{"GB", "G", "MB", "M"} {
		if strings.HasSuffix(raw, suffix) {
			raw = strings.TrimSpace(strings.TrimSuffix(raw, suffix))
			matchedSuffix = true
			if suffix == "G" || suffix == "GB" {
				multiplier = 1024
			}
			break
		}
	}
	if !matchedSuffix && strings.ContainsAny(raw, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		panic(fmt.Sprintf("%s must be an integer size such as 300M or 1G", name))
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		panic(fmt.Sprintf("%s must be an integer size such as 300M or 1G", name))
	}
	if multiplier > 1 && value > (1<<63-1)/multiplier {
		panic(fmt.Sprintf("%s is too large", name))
	}
	return value * multiplier
}
