package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
	"option-scope-go/internal/config"
	"option-scope-go/internal/outboundhttp"
	"option-scope-go/internal/store"
)

const usage = "usage: option-scope-go-db {cleanup-online|cleanup-offline|backup [backup-path]|restore backup-path|stats}"

const housekeepingLeaseName = "option-database-housekeeping"
const cleanupLeaseBusyDetail = "database cleanup is already running in another process"

const (
	cleanupOnlineMaxAttempts = 4
	cleanupRetryMaxDelay     = 2 * time.Second
)

func main() {
	if err := run(os.Args[1:], config.Load(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, cfg config.Config, output io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "cleanup-online":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return cleanupOnline(cfg, output)
	case "cleanup-offline":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return cleanupOffline(cfg, output)
	case "backup":
		if len(args) > 2 {
			return errors.New(usage)
		}
		destination := ""
		if len(args) == 2 {
			destination = args[1]
		}
		return backupDatabase(cfg.DatabasePath, destination, output)
	case "restore":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return errors.New(usage)
		}
		return restoreDatabase(cfg, args[1], output)
	case "stats":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return databaseStats(cfg.DatabasePath, output)
	default:
		return errors.New(usage)
	}
}

func restoreDatabase(cfg config.Config, backup string, output io.Writer) error {
	return restoreDatabaseWithChown(cfg, backup, output, os.Chown)
}

func restoreDatabaseWithChown(cfg config.Config, backup string, output io.Writer, chown func(string, int, int) error) error {
	if err := ensureServiceStopped(cfg.Host, cfg.Port); err != nil {
		return err
	}

	backupPath, err := resolveExistingDatabaseFile(backup)
	if err != nil {
		return fmt.Errorf("resolve restore backup: %w", err)
	}
	databasePath, err := resolveDatabaseDestination(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if backupPath == databasePath {
		return errors.New("restore backup must differ from the database path")
	}
	if err := validateProjectDatabase(backupPath); err != nil {
		return fmt.Errorf("invalid restore backup: %w", err)
	}

	directory := filepath.Dir(databasePath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	stageFile, err := os.CreateTemp(directory, "."+filepath.Base(databasePath)+".restore-*.db")
	if err != nil {
		return fmt.Errorf("create restore staging path: %w", err)
	}
	stagePath := stageFile.Name()
	if err := stageFile.Close(); err != nil {
		_ = os.Remove(stagePath)
		return fmt.Errorf("close restore staging file: %w", err)
	}
	if err := os.Remove(stagePath); err != nil {
		return fmt.Errorf("prepare restore staging path: %w", err)
	}
	defer removeDatabaseFiles(stagePath)
	if err := vacuumDatabaseInto(backupPath, stagePath); err != nil {
		return fmt.Errorf("stage restore backup: %w", err)
	}
	if err := validateProjectDatabase(stagePath); err != nil {
		return fmt.Errorf("staged restore backup is invalid: %w", err)
	}

	mode := os.FileMode(0o644)
	var owner *syscall.Stat_t
	if info, err := os.Stat(databasePath); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("database destination is not a regular file")
		}
		mode = info.Mode().Perm()
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("inspect database destination owner: unsupported file metadata")
		}
		owner = stat
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect database destination: %w", err)
	} else if info, statErr := os.Stat(backupPath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(stagePath, mode); err != nil {
		return fmt.Errorf("set restored database permissions: %w", err)
	}
	if owner != nil {
		if err := chown(stagePath, int(owner.Uid), int(owner.Gid)); err != nil {
			return fmt.Errorf("preserve database owner and group before restore: %w", err)
		}
	}

	rollbackPath := databasePath + ".pre-restore-" + time.Now().Format("20060102-150405.000000000")
	moved, err := moveDatabaseFiles(databasePath, rollbackPath)
	if err != nil {
		return fmt.Errorf("preserve current database before restore: %w", err)
	}
	rollback := func(cause error) error {
		removeErr := removeDatabaseFiles(databasePath)
		if restoreErr := restoreMovedDatabaseFiles(databasePath, rollbackPath, moved); restoreErr != nil {
			return errors.Join(cause, removeErr, fmt.Errorf("restore previous database from %s: %w", rollbackPath, restoreErr))
		}
		return errors.Join(cause, removeErr)
	}
	if err := os.Rename(stagePath, databasePath); err != nil {
		return rollback(fmt.Errorf("install restored database: %w", err))
	}
	if err := validateProjectDatabase(databasePath); err != nil {
		return rollback(fmt.Errorf("restored database failed validation: %w", err))
	}
	if err := syncDirectory(directory); err != nil {
		return rollback(fmt.Errorf("sync restored database directory: %w", err))
	}
	if len(moved) == 0 {
		_, err = fmt.Fprintf(output, "数据库已从 %s 恢复；原数据库不存在，无回滚文件。\n", backupPath)
		return err
	}
	_, err = fmt.Fprintf(output, "数据库已从 %s 恢复；原数据库及 SQLite sidecar 保留在 %s。\n", backupPath, rollbackPath)
	return err
}

func ensureServiceStopped(host, port string) error {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	port = strings.TrimSpace(port)
	if host == "" {
		host = "0.0.0.0"
	}
	if port == "" {
		port = "8000"
	}
	hosts := []string{host}
	if ip := net.ParseIP(host); host == "0.0.0.0" || (ip != nil && ip.IsUnspecified()) {
		hosts = []string{"127.0.0.1", "::1"}
	}
	for _, probeHost := range hosts {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(probeHost, port), 400*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return errors.New("stop the application before restoring the database")
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
			continue
		}
		return fmt.Errorf("cannot confirm that the application is stopped at %s: %w", net.JoinHostPort(probeHost, port), err)
	}
	return nil
}

// resolveRuntimePath keeps maintenance operations independent from the
// caller's working directory. Relative paths are anchored beside the running
// binary, matching config.Load's .env and DATABASE_PATH resolution.
func resolveRuntimePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	executable, err := os.Executable()
	if err != nil || executable == "" {
		return filepath.Abs(path)
	}
	return filepath.Join(filepath.Dir(executable), path), nil
}

func resolveExistingDatabaseFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("backup path is empty")
	}
	abs, err := resolveRuntimePath(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("backup path is not a regular file")
	}
	return resolved, nil
}

func resolveDatabaseDestination(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "data/options.db"
	}
	abs, err := resolveRuntimePath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return abs, nil
}

func validateProjectDatabase(path string) error {
	database, err := sql.Open("sqlite", sqliteFileDSN(path, true))
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.Ping(); err != nil {
		return err
	}
	rows, err := database.Query("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	checks := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return err
		}
		checks++
		if result != "ok" {
			_ = rows.Close()
			return fmt.Errorf("SQLite integrity_check: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if checks == 0 {
		return errors.New("SQLite integrity_check returned no result")
	}
	var hasSnapshotTable int
	if err := database.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='quote_snapshots'").Scan(&hasSnapshotTable); err != nil {
		return err
	}
	if hasSnapshotTable != 1 {
		return errors.New("database is missing the quote_snapshots table")
	}
	return nil
}

func sqliteFileDSN(path string, readOnly bool) string {
	fileURL := &url.URL{Scheme: "file", Path: path}
	options := url.Values{"_pragma": {"busy_timeout(30000)", "foreign_keys(ON)"}}
	if readOnly {
		options.Set("mode", "ro")
	}
	fileURL.RawQuery = options.Encode()
	return fileURL.String()
}

func vacuumDatabaseInto(source, destination string) error {
	database, err := sql.Open("sqlite", sqliteFileDSN(source, false))
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return err
	}
	_, execErr := database.Exec("VACUUM INTO ?", destination)
	closeErr := database.Close()
	if execErr != nil {
		return execErr
	}
	return closeErr
}

func moveDatabaseFiles(source, destination string) ([]string, error) {
	var moved []string
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		sourcePath := source + suffix
		if _, err := os.Lstat(sourcePath); os.IsNotExist(err) {
			continue
		} else if err != nil {
			restoreErr := restoreMovedDatabaseFiles(source, destination, moved)
			return nil, errors.Join(fmt.Errorf("inspect %s: %w", sourcePath, err), restoreErr)
		}
		destinationPath := destination + suffix
		if _, err := os.Lstat(destinationPath); err == nil {
			restoreErr := restoreMovedDatabaseFiles(source, destination, moved)
			return nil, errors.Join(fmt.Errorf("rollback path already exists: %s", destinationPath), restoreErr)
		} else if !os.IsNotExist(err) {
			restoreErr := restoreMovedDatabaseFiles(source, destination, moved)
			return nil, errors.Join(fmt.Errorf("inspect rollback path %s: %w", destinationPath, err), restoreErr)
		}
		if err := os.Rename(sourcePath, destinationPath); err != nil {
			restoreErr := restoreMovedDatabaseFiles(source, destination, moved)
			return nil, errors.Join(fmt.Errorf("move %s: %w", sourcePath, err), restoreErr)
		}
		moved = append(moved, suffix)
	}
	return moved, nil
}

func removeDatabaseFiles(path string) error {
	var firstErr error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return firstErr
}

func restoreMovedDatabaseFiles(destination, rollback string, moved []string) error {
	var firstErr error
	for index := len(moved) - 1; index >= 0; index-- {
		suffix := moved[index]
		if err := os.Rename(rollback+suffix, destination+suffix); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("restore %s: %w", destination+suffix, err)
		}
	}
	return firstErr
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cleanupOnline(cfg config.Config, output io.Writer) error {
	port := strings.TrimSpace(cfg.Port)
	if port == "" {
		port = "8000"
	}
	timeout := 2 * time.Minute
	if cfg.SizeCleanupBudget >= timeout {
		timeout = cfg.SizeCleanupBudget + 30*time.Second
	}
	endpoint := "http://127.0.0.1:" + port + "/api/cleanup"
	client, err := outboundhttp.APIClient(timeout, "")
	if err != nil {
		return err
	}
	return cleanupOnlineWithRetry(cfg.AccessKey, endpoint, output, client, time.Sleep)
}

func cleanupOnlineWithRetry(accessKey, endpoint string, output io.Writer, client *http.Client, sleep func(time.Duration)) error {
	request, err := outboundhttp.NewRequest(context.Background(), http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build cleanup request: %w", err)
	}
	if accessKey != "" {
		request.Header.Set("X-Access-Key", accessKey)
	}
	for attempt := 1; attempt <= cleanupOnlineMaxAttempts; attempt++ {
		response, err := outboundhttp.Do(client, request.Clone(request.Context()))
		if err != nil {
			return fmt.Errorf("call cleanup API: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read cleanup response: %w", readErr)
		}
		if isCleanupLeaseBusyResponse(response.StatusCode, body) && attempt < cleanupOnlineMaxAttempts {
			delay := cleanupRetryDelay(response.Header.Get("Retry-After"), attempt)
			if delay > 0 && sleep != nil {
				sleep(delay)
			}
			continue
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("cleanup API returned %s: %s", response.Status, strings.TrimSpace(string(body)))
		}
		if _, err := output.Write(body); err != nil {
			return err
		}
		if len(body) == 0 || body[len(body)-1] != '\n' {
			_, err = io.WriteString(output, "\n")
		}
		return err
	}
	return errors.New("cleanup API retry limit reached")
}

func isCleanupLeaseBusyResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	var response struct {
		Detail string `json:"detail"`
	}
	return json.Unmarshal(body, &response) == nil && response.Detail == cleanupLeaseBusyDetail
}

func cleanupRetryDelay(retryAfter string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int(cleanupRetryMaxDelay/time.Second) {
			return cleanupRetryMaxDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(retryAfter); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			if delay > cleanupRetryMaxDelay {
				return cleanupRetryMaxDelay
			}
			return delay
		}
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	delay := 100 * time.Millisecond << shift
	if delay > cleanupRetryMaxDelay {
		return cleanupRetryMaxDelay
	}
	return delay
}

func cleanupOffline(cfg config.Config, output io.Writer) (resultErr error) {
	repository, err := store.OpenWithOptions(cfg.DatabasePath, cfg.LowMemory)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer repository.Close()
	owner := fmt.Sprintf("go-db-maintenance-%d-%d", os.Getpid(), time.Now().UnixNano())
	acquired, err := repository.TryAcquireServiceLease(housekeepingLeaseName, owner, 48*time.Hour)
	if err != nil {
		return fmt.Errorf("acquire database cleanup lease: %w", err)
	}
	if !acquired {
		return errors.New("database cleanup is already running in another process")
	}
	defer func() {
		if err := repository.ReleaseServiceLease(housekeepingLeaseName, owner); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("release database cleanup lease: %w", err)
		}
	}()
	pruned, err := repository.PruneLegacyRawJSON(5000, cfg.SizeCleanupBudget)
	if err != nil {
		log.Printf("prune legacy raw_json failed: %v", err)
	} else if pruned > 0 {
		log.Printf("pruned %d legacy raw_json rows", pruned)
	}
	deleted, err := repository.Cleanup(cfg.RawRetentionDays)
	if err != nil {
		return fmt.Errorf("clean expired data: %w", err)
	}
	size, err := repository.CleanupBySize(cfg.DatabaseMaxMB*1024*1024, 5000, cfg.SizeCleanupBudget)
	if err != nil {
		return fmt.Errorf("clean database size: %w", err)
	}
	if pruned > 0 {
		size["raw_json_cleared"] = size["raw_json_cleared"].(int64) + pruned
	}
	return writeJSON(output, map[string]any{"deleted": deleted, "size": size})
}

func backupDatabase(source, destination string, output io.Writer) error {
	if source == "" {
		source = "data/options.db"
	}
	if destination == "" {
		destination = filepath.Join("backup", "options-"+time.Now().Format("20060102-150405")+".db")
	}
	sourcePath, err := resolveRuntimePath(source)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	destinationPath, err := resolveRuntimePath(destination)
	if err != nil {
		return fmt.Errorf("resolve backup path: %w", err)
	}
	if sourcePath == destinationPath {
		return errors.New("backup destination must differ from the database path")
	}
	if _, err := os.Lstat(destinationPath); err == nil {
		return fmt.Errorf("backup destination already exists: %s", destinationPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	database, err := sql.Open("sqlite", sqliteFileDSN(sourcePath, false))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if _, err := database.Exec("VACUUM INTO ?", destinationPath); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	_, err = fmt.Fprintf(output, "备份文件：%s\n", destinationPath)
	return err
}

func databaseStats(path string, output io.Writer) error {
	if path == "" {
		path = "data/options.db"
	}
	absolute, err := resolveRuntimePath(path)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database read-only: %w", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("open database read-only: %w", err)
	}
	rows, err := database.QueryContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		return fmt.Errorf("list database tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("read table name: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list database tables: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close table listing: %w", err)
	}
	sort.Strings(tables)
	for _, table := range tables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		var count int64
		if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return fmt.Errorf("count rows in %s: %w", table, err)
		}
		if _, err := fmt.Fprintf(output, "  %-28s %d\n", table, count); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
