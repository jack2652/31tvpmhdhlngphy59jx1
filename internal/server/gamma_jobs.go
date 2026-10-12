package server

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"option-scope-go/internal/model"
)

const (
	gammaRefreshFreshSeconds   = 60 * time.Second
	gammaRefreshMaxBatchSize   = 2
	gammaRefreshTimeout        = 180 * time.Second
	gammaRefreshTimeoutError   = "分析超时，已停止本轮计算"
	goOrphanedGammaJobMessage  = "Go 服务重启后 Gamma 任务已失效"
	pythonOrphanedGammaMessage = "进程重启，后台分析已中断"
	gammaFinishRetryDelay      = 50 * time.Millisecond
	gammaFinishRetryCount      = 3
	gammaTerminalJobCacheLimit = 64
)

type gammaJob struct {
	jobID          string
	durableStarted string
	symbol         string
	horizonDays    int
	status         string
	cancel         context.CancelCauseFunc
	persistError   string
	persistPending bool
	startedAt      time.Time
	finishedAt     time.Time
	warning        string
	result         map[string]any
	nextCursor     string
	hasMore        bool
	processed      int
	total          int
}

// gammaJobPayload is deliberately process-local status. The durable option
// snapshots remain the source of truth; this state only coordinates the
// asynchronous request and tells the browser whether another batch is needed.
func gammaJobPayload(job *gammaJob, claimed bool) map[string]any {
	if job == nil {
		return nil
	}
	result := map[string]any{
		"job_id": job.jobID, "symbol": job.symbol, "horizon_days": job.horizonDays,
		"status": job.status, "started_at": job.durableStarted, "finished_at": nil,
		"result": job.result, "error_message": nil, "claimed": claimed,
		"deferred": job.status == "deferred", "has_more": job.hasMore,
		"next_cursor": nil, "processed": job.processed, "total": job.total,
	}
	if job.nextCursor != "" {
		result["next_cursor"] = job.nextCursor
	}
	if !job.finishedAt.IsZero() {
		result["finished_at"] = job.finishedAt.UTC().Format(time.RFC3339Nano)
	}
	if job.warning != "" {
		result["error_message"] = job.warning
	}
	if job.persistError != "" {
		if warning, _ := result["error_message"].(string); warning != "" {
			result["error_message"] = warning + "; Gamma 状态写入 SQLite 失败：" + job.persistError
		} else {
			result["error_message"] = "Gamma 状态写入 SQLite 失败：" + job.persistError
		}
	}
	return result
}

func (s *Service) gammaJobKey(symbol string, horizonDays int) string {
	return symbol + "|" + time.Duration(horizonDays).String()
}

func (s *Service) gammaJobStatus(symbol string, horizonDays int) map[string]any {
	if persisted, found, err := s.store.AnalysisJob(symbol, horizonDays); err == nil && found {
		if persisted["status"] == "running" {
			key := s.gammaJobKey(symbol, horizonDays)
			s.mu.RLock()
			job := s.gammaJobs[key]
			if job != nil && job.persistError != "" && job.durableStarted == fmt.Sprint(persisted["started_at"]) {
				copy := *job
				s.mu.RUnlock()
				return gammaAnalysisJobPayload(gammaJobPayload(&copy, false))
			}
			s.mu.RUnlock()
		}
		return gammaAnalysisJobPayload(persisted)
	}
	key := s.gammaJobKey(symbol, horizonDays)
	s.mu.RLock()
	job := s.gammaJobs[key]
	if job != nil {
		copy := *job
		job = &copy
	}
	s.mu.RUnlock()
	return gammaAnalysisJobPayload(gammaJobPayload(job, false))
}

// Python's analysis_job response mirrors the durable table columns. Keep the
// public refresh state at that shape instead of exposing Go-only claim fields.
func gammaAnalysisJobPayload(job map[string]any) map[string]any {
	if job == nil {
		return nil
	}
	errorMessage := job["error_message"]
	// Keep the public restart message compatible with Python's analysis_job API.
	if errorMessage == goOrphanedGammaJobMessage {
		errorMessage = pythonOrphanedGammaMessage
	}
	return map[string]any{
		"job_id": job["job_id"], "symbol": job["symbol"], "horizon_days": job["horizon_days"],
		"status": job["status"], "started_at": job["started_at"], "finished_at": job["finished_at"],
		"result": job["result"], "error_message": errorMessage,
	}
}

// Python returns a compact claim result when another worker owns the job or
// the completed result is still inside its cooldown window.
func gammaUnclaimedPayload(job map[string]any) map[string]any {
	if job == nil {
		return nil
	}
	payload := map[string]any{
		"job_id": job["job_id"], "symbol": job["symbol"], "horizon_days": job["horizon_days"],
		"status": job["status"], "claimed": false,
	}
	if job["status"] == "running" {
		payload["started_at"] = job["started_at"]
	} else {
		payload["finished_at"] = job["finished_at"]
	}
	return payload
}

// claimGammaJob starts at most one incremental batch for a symbol/horizon.
// A second refresh request observes the running job instead of starting a
// second provider walk over the same expiration window.
func (s *Service) claimGammaJob(symbol string, horizonDays int) (map[string]any, bool) {
	key := s.gammaJobKey(symbol, horizonDays)
	s.mu.Lock()
	ctx := s.background
	if ctx.Err() != nil {
		s.mu.Unlock()
		return map[string]any{"status": "deferred", "claimed": false, "deferred": true, "warning": "服务正在关闭"}, false
	}
	// Add before releasing the lifecycle lock so Close cannot begin Wait while
	// this claim is between the durable transaction and goroutine startup.
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	if ctx.Err() == nil {
		s.retryPendingGammaFinish(key)
	}

	claimed, didClaim, err := s.store.ClaimAnalysisJobContext(ctx, symbol, horizonDays, 30*time.Second, gammaRefreshTimeout)
	if err != nil {
		s.backgroundWG.Done()
		return map[string]any{"status": "deferred", "claimed": false, "deferred": true, "warning": err.Error()}, false
	}
	if !didClaim {
		s.backgroundWG.Done()
		return gammaUnclaimedPayload(claimed), false
	}
	now := time.Now()
	job := &gammaJob{jobID: fmt.Sprint(claimed["job_id"]), durableStarted: fmt.Sprint(claimed["started_at"]), symbol: symbol, horizonDays: horizonDays, status: "running", startedAt: now}
	if resume, ok := claimed["resume_result"].(map[string]any); ok {
		if cursor, ok := resume["next_cursor"].(string); ok {
			job.nextCursor = cursor
		}
		if processed, ok := resume["processed"].(float64); ok {
			job.processed = int(processed)
		}
		if total, ok := resume["total"].(float64); ok {
			job.total = int(total)
		}
	}
	jobCtx, cancel := context.WithCancelCause(ctx)
	job.cancel = cancel
	s.mu.Lock()
	if previous := s.gammaJobs[key]; previous != nil && previous.status == "running" && previous.cancel != nil {
		previous.cancel(context.Canceled)
	}
	s.gammaJobs[key] = job
	s.mu.Unlock()
	go func(jobID, started string) {
		defer s.backgroundWG.Done()
		defer cancel(context.Canceled)
		s.runGammaJob(jobCtx, key, jobID, started, symbol, horizonDays)
	}(job.jobID, job.durableStarted)
	return claimed, true
}

func (s *Service) markGammaJobFinished(key, jobID, startedAt, status, warning string, result map[string]any) bool {
	nextCursor, _ := result["next_cursor"].(string)
	hasMore, _ := result["has_more"].(bool)
	processed, total := gammaInt(result, "processed"), gammaInt(result, "total")
	if _, ok := result["loaded"]; ok {
		processed = gammaInt(result, "loaded")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.gammaJobs[key]
	if job == nil || job.jobID != jobID || job.durableStarted != startedAt || job.status != "running" {
		return false
	}
	job.status = status
	job.warning = warning
	job.result = result
	job.nextCursor = nextCursor
	job.hasMore = hasMore
	job.processed = processed
	job.total = total
	job.cancel = nil
	job.persistPending = true
	job.finishedAt = time.Now()
	s.pruneGammaTerminalJobsLocked()
	return true
}

// Durable rows are authoritative after a job finishes. Keep a bounded local
// fallback for recent status reads, while retaining running jobs and terminal
// states whose SQLite write still needs retrying.
func (s *Service) pruneGammaTerminalJobsLocked() {
	type terminalEntry struct {
		key        string
		finishedAt time.Time
	}
	entries := make([]terminalEntry, 0, len(s.gammaJobs))
	for key, job := range s.gammaJobs {
		if job.status != "running" && job.persistError == "" && !job.persistPending {
			entries = append(entries, terminalEntry{key: key, finishedAt: job.finishedAt})
		}
	}
	if len(entries) <= gammaTerminalJobCacheLimit {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].finishedAt.Before(entries[j].finishedAt)
	})
	for _, entry := range entries[:len(entries)-gammaTerminalJobCacheLimit] {
		delete(s.gammaJobs, entry.key)
	}
}

func (s *Service) finishGammaJob(key, jobID, startedAt, status, warning, nextCursor string, hasMore bool, processed, total int) {
	result := map[string]any{"has_more": hasMore, "next_cursor": nextCursor, "processed": processed, "total": total}
	s.finishGammaJobAttempt(key, jobID, startedAt, status, warning, result)
}

func (s *Service) runGammaJob(ctx context.Context, key, jobID, startedAt, symbol string, horizonDays int) {
	s.runGammaJobWithTimeout(ctx, key, jobID, startedAt, gammaRefreshTimeout, func(jobCtx context.Context) {
		s.runGammaBatch(jobCtx, key, jobID, startedAt, symbol, horizonDays)
	})
}

func (s *Service) runGammaJobWithTimeout(ctx context.Context, key, jobID, startedAt string, timeout time.Duration, run func(context.Context)) {
	jobCtx, cancel := context.WithCancelCause(ctx)
	timeoutDone := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		defer close(timeoutDone)
		marked := s.markGammaJobFinished(key, jobID, startedAt, "failed", gammaRefreshTimeoutError, nil)
		cancel(fmt.Errorf("%s", gammaRefreshTimeoutError))
		if marked {
			s.persistGammaFinish(key, jobID, startedAt, "failed", gammaRefreshTimeoutError, nil)
		}
	})
	defer func() {
		if recovered := recover(); recovered != nil {
			message := fmt.Sprint(recovered)
			if s.markGammaJobFinished(key, jobID, startedAt, "failed", message, nil) {
				s.persistGammaFinish(key, jobID, startedAt, "failed", message, nil)
			}
		}
		if !timer.Stop() {
			<-timeoutDone
		}
		cancel(context.Canceled)
	}()
	run(jobCtx)
}

func gammaContextError(ctx context.Context) string {
	if cause := context.Cause(ctx); cause != nil {
		return cause.Error()
	}
	return context.Canceled.Error()
}

func (s *Service) finishGammaJobAttempt(key, jobID, startedAt, status, warning string, result map[string]any) {
	if !s.markGammaJobFinished(key, jobID, startedAt, status, warning, result) {
		return
	}
	s.persistGammaFinish(key, jobID, startedAt, status, warning, result)
}

func (s *Service) persistGammaFinish(key, jobID, startedAt, status, warning string, result map[string]any) {
	var finishErr error
	for attempt := 0; attempt < gammaFinishRetryCount; attempt++ {
		finished, err := s.store.FinishAnalysisJob(jobID, status, result, warning, startedAt)
		if err == nil {
			s.completeGammaPersistence(key, jobID, startedAt)
			if !finished {
				return
			}
			return
		}
		finishErr = err
		if attempt+1 < gammaFinishRetryCount {
			time.Sleep(gammaFinishRetryDelay)
		}
	}
	message := finishErr.Error()
	s.recordGammaPersistenceError(key, jobID, startedAt, message)
	log.Printf("persist terminal Gamma job %s failed after %d attempts: %v", jobID, gammaFinishRetryCount, finishErr)
}

func (s *Service) retryPendingGammaFinish(key string) {
	s.mu.Lock()
	job := s.gammaJobs[key]
	if job == nil || job.persistError == "" || job.status == "running" || job.persistPending {
		s.mu.Unlock()
		return
	}
	job.persistPending = true
	copy := *job
	s.mu.Unlock()
	s.persistGammaFinish(key, copy.jobID, copy.durableStarted, copy.status, copy.warning, copy.result)
}

func (s *Service) recordGammaPersistenceError(key, jobID, startedAt, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.gammaJobs[key]
	if job != nil && job.jobID == jobID && job.durableStarted == startedAt && job.status != "running" {
		job.persistError = message
		job.persistPending = false
		s.pruneGammaTerminalJobsLocked()
	}
}

func (s *Service) completeGammaPersistence(key, jobID, startedAt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.gammaJobs[key]
	if job != nil && job.jobID == jobID && job.durableStarted == startedAt && job.status != "running" {
		job.persistError = ""
		job.persistPending = false
		s.pruneGammaTerminalJobsLocked()
	}
}

func gammaInt(values map[string]any, key string) int {
	switch value := values[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func (s *Service) runGammaBatch(ctx context.Context, key, jobID, startedAt, symbol string, horizonDays int) {
	select {
	case s.gammaHeavyGate <- struct{}{}:
	case <-ctx.Done():
		s.finishGammaJobAttempt(key, jobID, startedAt, "failed", gammaContextError(ctx), nil)
		return
	default:
		message := "已有重任务在进行，Gamma 窗口稍后计算"
		if s.cfg.LowMemory {
			message = "内存保护：已有重任务在进行，Gamma 窗口已让路"
		}
		s.mu.RLock()
		job := s.gammaJobs[key]
		cursor, processed, total := "", 0, 0
		if job != nil && job.jobID == jobID && job.durableStarted == startedAt {
			cursor, processed, total = job.nextCursor, job.processed, job.total
		}
		s.mu.RUnlock()
		var cursorValue any
		if cursor != "" {
			cursorValue = cursor
		}
		result := map[string]any{
			"symbol": symbol, "horizon_days": horizonDays,
			"expirations": []string{}, "results": []any{}, "errors": []string{message},
			"deferred": true, "warning": message, "next_cursor": cursorValue,
			"has_more": true, "loaded": processed, "processed": processed, "total": total,
		}
		s.finishGammaJobAttempt(key, jobID, startedAt, "failed", message, result)
		return
	}
	batchSize := 0
	var values []string
	var err error
	func() {
		defer func() { <-s.gammaHeavyGate }()
		batchSize = gammaBatchSize(s.cfg.LowMemory, s.cfg.UpstreamConcurrency)
		var found bool
		values, found, err = s.store.LatestExpirations(symbol)
		if err == nil {
			values = validGammaExpirations(values)
			if !found || len(values) == 0 {
				values, err = s.provider.Expirations(ctx, symbol)
				if err == nil {
					values = activeExpirations(validGammaExpirations(values))
					if len(values) > 0 {
						err = s.store.SaveExpirations(symbol, values)
					}
				}
			} else {
				values = activeExpirations(values)
			}
		}
	}()
	if ctx.Err() != nil {
		s.finishGammaJobAttempt(key, jobID, startedAt, "failed", gammaContextError(ctx), nil)
		return
	}
	if err != nil {
		s.finishGammaJobAttempt(key, jobID, startedAt, "failed", err.Error(), nil)
		return
	}
	sort.Strings(values)
	location, locationErr := time.LoadLocation("America/New_York")
	if locationErr != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format("2006-01-02")
	cutoff := time.Now().In(location).AddDate(0, 0, horizonDays).Format("2006-01-02")
	selected := make([]string, 0, len(values))
	for _, expiration := range values {
		if expiration >= today && expiration <= cutoff {
			selected = append(selected, expiration)
		}
	}
	if len(selected) == 0 {
		result := gammaWindowResult(symbol, horizonDays, selected, []string{}, []any{}, []string{}, "", false, 0, 0, batchSize)
		s.finishGammaJobAttempt(key, jobID, startedAt, "completed", "", result)
		return
	}

	s.mu.RLock()
	previous := ""
	if old := s.gammaJobs[key]; old != nil {
		previous = old.nextCursor
	}
	s.mu.RUnlock()
	results := []any{}
	errors := []string{}
	batch := []string{}
	type gammaStep struct {
		expiration string
		batchIndex int
		done       bool
	}
	steps := []gammaStep{}
	lastProcessed := previous
	for _, expiration := range selected {
		if previous != "" && expiration <= previous {
			continue
		}
		chain, found, chainErr := s.store.LatestChain(symbol, expiration)
		if chainErr != nil {
			s.finishGammaJobAttempt(key, jobID, startedAt, "failed", chainErr.Error(), nil)
			return
		}
		fresh := false
		if found && chain.FetchedAt != nil && time.Since(*chain.FetchedAt) < gammaRefreshFreshSeconds {
			for _, row := range chain.Data {
				if row.OpenInterest != nil && *row.OpenInterest > 0 {
					fresh = true
					break
				}
			}
			if fresh {
				steps = append(steps, gammaStep{expiration: expiration, done: true})
				continue
			}
		}
		batch = append(batch, expiration)
		steps = append(steps, gammaStep{expiration: expiration, batchIndex: len(batch) - 1})
		if len(batch) >= batchSize {
			break
		}
	}
	if len(batch) > 0 {
		type refreshOutcome struct {
			result    any
			err       string
			completed bool
		}
		outcomes := make([]refreshOutcome, len(batch))
		var quoteOnce sync.Once
		var quoteOverride *model.Quote
		loadWindowQuote := func() *model.Quote {
			quoteOnce.Do(func() {
				if cachedQuote, quoteFound, quoteErr := s.store.LatestQuote(symbol); quoteErr == nil && quoteFound && cachedQuote.Price != nil {
					quoteOverride = &cachedQuote
					return
				}
				quote, quoteErr := s.fetchQuoteForExpirationMode(ctx, symbol, "", false)
				if quoteErr == nil {
					quoteOverride = &quote
				}
			})
			return quoteOverride
		}
		var workers sync.WaitGroup
		for index, expiration := range batch {
			workers.Add(1)
			go func(index int, expiration string) {
				defer workers.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						outcomes[index].err = fmt.Sprint(recovered)
						outcomes[index].completed = true
					}
				}()
				select {
				case <-ctx.Done():
					outcomes[index].err = ctx.Err().Error()
					return
				case s.gammaHeavyGate <- struct{}{}:
					defer func() { <-s.gammaHeavyGate }()
				default:
					reason := "让路给首屏历史数据"
					if s.cfg.LowMemory {
						reason = "内存保护：" + reason
					}
					outcomes[index].err = reason
					return
				}
				if ctx.Err() != nil {
					outcomes[index].err = ctx.Err().Error()
					return
				}
				refreshResult, refreshErr := s.refreshSnapshotWithOverrides(ctx, symbol, expiration, gammaRefreshFreshSeconds, values, loadWindowQuote())
				if ctx.Err() != nil {
					outcomes[index].err = ctx.Err().Error()
					return
				}
				outcomes[index].completed = true
				if refreshErr != nil {
					outcomes[index].err = refreshErr.Error()
				} else if refreshResult.Warning != "" && !refreshResult.Stale {
					outcomes[index].err = refreshResult.Warning
				} else {
					outcomes[index].result = refreshAPIValue(refreshResult)
				}
			}(index, expiration)
		}
		workers.Wait()
		if ctx.Err() != nil {
			s.finishGammaJobAttempt(key, jobID, startedAt, "failed", gammaContextError(ctx), nil)
			return
		}
		completedByBatch := make([]bool, len(batch))
		for index := range batch {
			outcome := outcomes[index]
			completedByBatch[index] = outcome.completed
			if outcome.err != "" {
				errors = append(errors, batch[index]+": "+outcome.err)
			}
			if outcome.result != nil {
				results = append(results, outcome.result)
			}
		}
		for index := range steps {
			if !steps[index].done {
				steps[index].done = completedByBatch[steps[index].batchIndex]
			}
		}
	}
	// Advance only over the ordered prefix of fresh or completed expirations.
	prefixComplete := true
	for _, step := range steps {
		if prefixComplete && step.done {
			lastProcessed = step.expiration
		} else {
			prefixComplete = false
		}
	}
	hasMore := lastProcessed == "" || lastProcessed < selected[len(selected)-1]
	nextCursor := lastProcessed
	if !hasMore {
		nextCursor = ""
	}
	result := gammaWindowResult(symbol, horizonDays, selected, batch, results, errors, nextCursor, hasMore, gammaLoaded(selected, lastProcessed), len(selected), batchSize)
	s.finishGammaJobAttempt(key, jobID, startedAt, "completed", "", result)
}

// Python's expiration_dates filters malformed catalog entries and removes
// duplicates before a Gamma window is selected.
func validGammaExpirations(values []string) []string {
	valid := make([]string, 0, len(values))
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
		valid = append(valid, value)
	}
	return valid
}

func gammaBatchSize(lowMemory bool, upstreamConcurrency int) int {
	if lowMemory || upstreamConcurrency <= 1 {
		return 1
	}
	if upstreamConcurrency < gammaRefreshMaxBatchSize {
		return upstreamConcurrency
	}
	return gammaRefreshMaxBatchSize
}

func gammaWindowResult(symbol string, horizonDays int, expirations, batch []string, results []any, errors []string, nextCursor string, hasMore bool, loaded, total, batchSize int) map[string]any {
	if expirations == nil {
		expirations = []string{}
	}
	if batch == nil {
		batch = []string{}
	}
	if results == nil {
		results = []any{}
	}
	if errors == nil {
		errors = []string{}
	}
	var cursor any
	if nextCursor != "" {
		cursor = nextCursor
	}
	return map[string]any{
		"symbol": symbol, "horizon_days": horizonDays, "expirations": expirations,
		"results": results, "errors": errors, "batch_size": batchSize,
		"batch_expirations": batch, "next_cursor": cursor, "has_more": hasMore,
		"loaded": loaded, "processed": loaded, "total": total,
	}
}

func gammaLoaded(expirations []string, cursor string) int {
	if cursor == "" {
		return 0
	}
	loaded := 0
	for _, expiration := range expirations {
		if expiration <= cursor {
			loaded++
		}
	}
	return loaded
}
