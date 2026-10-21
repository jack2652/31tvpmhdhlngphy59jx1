package server

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"math/big"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
	chananalysis "option-scope-go/internal/chan"
	"option-scope-go/internal/config"
	"option-scope-go/internal/gamma"
	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/provider"
	"option-scope-go/internal/store"
	"option-scope-go/internal/valuation"
	"option-scope-go/web"
)

type cachedQuote struct {
	value model.Quote
	at    time.Time
}

type cachedChain struct {
	value model.ChainResponse
	at    time.Time
}

type intradayCacheEntry struct {
	frame      []map[string]any
	fetchedAt  time.Time
	lastAccess time.Time
}

type chanAdjustedHistoryCacheEntry struct {
	bars       []map[string]any
	fetchedAt  time.Time
	source     string
	lastAccess time.Time
}

type chanAnalysisCacheEntry struct {
	result     chananalysis.Result
	lastAccess time.Time
}

type cachedValuationRegime struct {
	regime  string
	signals map[string]any
	at      time.Time
}

type historyLockEntry struct {
	mutex sync.Mutex
	refs  int
}

type eventLog struct {
	events   []model.Event
	nextID   int64
	payloads map[string]map[string]any
}

type eventSubscription struct {
	cancel context.CancelFunc
	refs   int
}

type refreshGate struct {
	sem       chan struct{}
	refs      int
	completed refreshResult
}

type refreshResult struct {
	requestedExpiration string
	response            model.RefreshResponse
	err                 error
	completedAt         time.Time
}

type leaseRenewer struct {
	cancel     context.CancelFunc
	done       chan struct{}
	stopTicker func()
}

const durableRefreshLease = 180 * time.Second

const forbiddenPage = `<!doctype html>
<html lang="en">
<head><title>403 Forbidden</title></head>
<body>
<center><h1>403 Forbidden</h1></center>
<hr>
<center>nginx</center>
</body>
</html>
`

type leaseTicker interface {
	C() <-chan time.Time
	Stop()
}

type realLeaseTicker struct{ ticker *time.Ticker }

func (t realLeaseTicker) C() <-chan time.Time { return t.ticker.C }
func (t realLeaseTicker) Stop()               { t.ticker.Stop() }

func startLeaseRenewal(parent context.Context, renew func() (bool, error), interval time.Duration, leaseName string) (context.Context, *leaseRenewer) {
	if interval <= 0 {
		interval = durableRefreshLease / 3
	}
	return startLeaseRenewalWithTicker(parent, renew, leaseName, realLeaseTicker{ticker: time.NewTicker(interval)})
}

func startLeaseRenewalWithTicker(parent context.Context, renew func() (bool, error), leaseName string, ticker leaseTicker) (context.Context, *leaseRenewer) {
	ctx, cancel := context.WithCancel(parent)
	renewer := &leaseRenewer{cancel: cancel, done: make(chan struct{}), stopTicker: ticker.Stop}
	go func() {
		defer close(renewer.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				owned, err := renew()
				if err != nil {
					log.Printf("durable lease renewal failed for %s: %v", leaseName, err)
					cancel()
					return
				}
				if !owned {
					log.Printf("durable lease ownership lost for %s", leaseName)
					cancel()
					return
				}
			}
		}
	}()
	return ctx, renewer
}

func (r *leaseRenewer) stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.stopTicker()
	<-r.done
}

type ExpirationUnavailableError struct {
	Symbol     string
	Expiration string
}

func (e ExpirationUnavailableError) Error() string {
	return fmt.Sprintf("%s has no active expiration %s", e.Symbol, e.Expiration)
}

type MarketProvider interface {
	Quote(context.Context, string) (model.Quote, error)
	Expirations(context.Context, string) ([]string, error)
	Chain(context.Context, string, string) ([]model.OptionRow, *time.Time, error)
}

// NearestChainProvider collapses the cold-start expiration catalogue and
// nearest-chain requests into one upstream payload.
type NearestChainProvider interface {
	NearestChain(context.Context, string) ([]string, []model.OptionRow, *time.Time, error)
}

type snapshotQuoteProvider interface {
	SnapshotQuote(context.Context, string) (model.Quote, error)
}

type HistoryProvider interface {
	History(context.Context, string, string) (provider.HistoryResult, error)
}

type IntradayHistoryProvider interface {
	IntradayHistory(context.Context, string, string) (provider.HistoryResult, error)
}

type ChanAdjustedHistoryProvider interface {
	ChanAdjustedHistory(context.Context, string, string) (provider.ChanAdjustedHistoryResult, error)
}

type Service struct {
	cfg                    config.Config
	provider               MarketProvider
	store                  *store.SQLite
	mu                     sync.RWMutex
	quotes                 map[string]cachedQuote
	expires                map[string][]string
	chains                 map[string]cachedChain
	events                 eventLog
	refreshGates           map[string]*refreshGate
	subscriptions          map[string]eventSubscription
	gammaJobs              map[string]*gammaJob
	valuationJobs          map[string]bool
	valuationAttempts      map[string]time.Time
	valuationRegimes       map[string]cachedValuationRegime
	historyLocks           map[string]*historyLockEntry
	historyRefreshJobs     map[string]bool
	historyRefreshAfter    map[string]time.Time
	levelsFlights          map[string]*levelsCacheFlight
	chainReads             singleflight.Group
	analyticsPrefetch      map[string]bool
	chanHistoryJobs        map[string]bool
	chanAdjustedMu         sync.Mutex
	chanAdjustedCache      map[string]chanAdjustedHistoryCacheEntry
	chanAdjustedJobs       map[string]bool
	chanAdjustedRetryAfter map[string]time.Time
	chanAnalysisMu         sync.Mutex
	chanAnalysisCache      map[string]chanAnalysisCacheEntry
	chanAnalysisFlights    singleflight.Group
	intradayMu             sync.Mutex
	intradayHistoryCache   map[string]intradayCacheEntry
	intradayRefreshJobs    map[string]bool
	intradayRefreshAfter   map[string]time.Time
	snapshotRefreshJobs    map[string]bool
	gammaHeavyGate         chan struct{}
	background             context.Context
	stopBackground         context.CancelFunc
	backgroundWG           sync.WaitGroup
	leaseOwner             string
	cleanupOnce            sync.Once
	cleanupMu              sync.Mutex
	housekeepingLeaseHeld  bool // guarded by cleanupMu
	cleanupActive          bool // guarded by cleanupMu
}

func New(cfg config.Config) (*Service, error) {
	marketProvider, err := provider.NewDefaultHybrid(cfg.UpstreamTimeout, cfg.UpstreamConcurrency, cfg.MarketProxy, cfg.AlpacaAPIKey, cfg.AlpacaAPISecret, cfg.UpstreamWait)
	if err != nil {
		return nil, err
	}
	if cfg.LowMemory && marketProvider.Delayed != nil {
		marketProvider.Delayed.CacheLimit = 1
	}
	if history, ok := marketProvider.HistoryClient.(*provider.YFinanceHistoryClient); ok {
		history.IntradayCacheLimit = boundedIntradayCacheEntries(cfg.LowMemory)
	}
	repository, err := store.OpenWithOptions(cfg.DatabasePath, cfg.LowMemory)
	if err != nil {
		return nil, err
	}
	return NewWithProvider(cfg, repository, marketProvider), nil
}

// ValidateAlpacaCredentials exposes optional provider startup diagnostics to
// the executable without making validation part of HTTP service construction.
func (s *Service) ValidateAlpacaCredentials(ctx context.Context) string {
	if s == nil {
		return "disabled"
	}
	validator, ok := s.provider.(interface {
		ValidateAlpacaCredentials(context.Context) string
	})
	if !ok {
		return "disabled"
	}
	return validator.ValidateAlpacaCredentials(ctx)
}

func NewWithProvider(cfg config.Config, repository *store.SQLite, marketProvider MarketProvider) *Service {
	if cfg.RawRetentionDays < 1 {
		cfg.RawRetentionDays = 30
	}
	if cfg.HistoryMaxAge <= 0 {
		cfg.HistoryMaxAge = time.Hour
	}
	if cfg.ExtremesMaxAge <= 0 {
		cfg.ExtremesMaxAge = 24 * time.Hour
	}
	if cfg.InboundConcurrency < 1 {
		cfg.InboundConcurrency = 64
		if cfg.LowMemory {
			cfg.InboundConcurrency = 12
		}
	}
	background, stop := context.WithCancel(context.Background())
	var ownerBytes [16]byte
	if _, err := rand.Read(ownerBytes[:]); err != nil {
		ownerBytes[0] = byte(time.Now().UnixNano())
	}
	// A process restart cannot resume an in-flight provider walk safely. Marking
	// it failed also lets the next refresh claim a fresh durable job.
	if repository != nil {
		_, _ = repository.FailOrphanedAnalysisJobs(3 * time.Minute)
	}
	gateLimit := 4
	if cfg.LowMemory {
		gateLimit = 1
	}
	return &Service{
		cfg: cfg, provider: marketProvider, store: repository,
		quotes: make(map[string]cachedQuote), expires: make(map[string][]string),
		chains: make(map[string]cachedChain), events: eventLog{nextID: 1, payloads: make(map[string]map[string]any)},
		refreshGates: make(map[string]*refreshGate), subscriptions: make(map[string]eventSubscription),
		gammaJobs: make(map[string]*gammaJob), valuationJobs: make(map[string]bool), valuationAttempts: make(map[string]time.Time), valuationRegimes: make(map[string]cachedValuationRegime), historyLocks: make(map[string]*historyLockEntry), historyRefreshJobs: make(map[string]bool), historyRefreshAfter: make(map[string]time.Time), levelsFlights: make(map[string]*levelsCacheFlight), analyticsPrefetch: make(map[string]bool), chanHistoryJobs: make(map[string]bool), snapshotRefreshJobs: make(map[string]bool),
		intradayHistoryCache:   make(map[string]intradayCacheEntry),
		intradayRefreshJobs:    make(map[string]bool),
		intradayRefreshAfter:   make(map[string]time.Time),
		chanAdjustedCache:      make(map[string]chanAdjustedHistoryCacheEntry),
		chanAnalysisCache:      make(map[string]chanAnalysisCacheEntry),
		chanAdjustedJobs:       make(map[string]bool),
		chanAdjustedRetryAfter: make(map[string]time.Time),
		gammaHeavyGate:         make(chan struct{}, gateLimit),
		background:             background, stopBackground: stop, leaseOwner: hex.EncodeToString(ownerBytes[:]),
	}
}

func (s *Service) Close() error {
	s.mu.Lock()
	s.stopBackground()
	for key, subscription := range s.subscriptions {
		subscription.cancel()
		delete(s.subscriptions, key)
	}
	s.mu.Unlock()
	s.backgroundWG.Wait()
	var providerErr error
	if closer, ok := s.provider.(interface{ Close() error }); ok {
		providerErr = closer.Close()
	}
	return errors.Join(providerErr, s.store.Close())
}

func boundedIntradayCacheEntries(lowMemory bool) int {
	if lowMemory {
		return 1
	}
	return 2
}

func boundedAdjustedHistoryCacheEntries(lowMemory bool) int {
	if lowMemory {
		return 1
	}
	return 3
}

func boundedChanAnalysisCacheEntries(lowMemory bool) int {
	if lowMemory {
		return 1
	}
	return 4
}

func boundedChanAnalysisDurableCacheEntries(lowMemory bool) int {
	if lowMemory {
		return 4
	}
	return 24
}

const (
	quoteCacheLimit      = 512
	chainCacheLimit      = 256
	lowMemoryCacheDivide = 4
)

// trimSymbolCachesLocked bounds the per-symbol quote and option-chain caches.
// Both are pure caches backed by SQLite, so dropping the oldest entries keeps a
// long-running instance that sees many tickers from growing without limit. The
// caller must hold s.mu.
func (s *Service) trimSymbolCachesLocked() {
	quoteLimit, chainLimit := quoteCacheLimit, chainCacheLimit
	if s.cfg.LowMemory {
		quoteLimit, chainLimit = quoteLimit/lowMemoryCacheDivide, chainLimit/lowMemoryCacheDivide
	}
	// Evict a slice below the limit so the O(n) scan runs once per batch.
	if len(s.quotes) > quoteLimit {
		evictOldestHistoryCacheEntry(s.quotes, "", quoteLimit*9/10, func(entry cachedQuote) time.Time { return entry.at })
	}
	if len(s.chains) > chainLimit {
		evictOldestHistoryCacheEntry(s.chains, "", chainLimit*9/10, func(entry cachedChain) time.Time { return entry.at })
	}
}

func evictOldestHistoryCacheEntry[T any](cache map[string]T, keyToKeep string, limit int, lastAccess func(T) time.Time) {
	if limit < 1 {
		limit = 1
	}
	for len(cache) > limit {
		oldestKey := ""
		oldestAt := time.Time{}
		for key, value := range cache {
			if key == keyToKeep {
				continue
			}
			at := lastAccess(value)
			if oldestKey == "" || at.Before(oldestAt) {
				oldestKey, oldestAt = key, at
			}
		}
		if oldestKey == "" {
			return
		}
		delete(cache, oldestKey)
	}
}

func (s *Service) Router() *gin.Engine {
	if s.cfg.SchedulerEnabled {
		s.startHousekeeping()
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(normalizeJSONContentType(), requestAdmissionLimit(s.cfg.InboundConcurrency), gin.Logger(), gin.Recovery(), s.accessGuard(), noStore(), staticCacheHeaders(), responseCompression(s.cfg.LowMemory))
	router.HandleMethodNotAllowed = true
	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
	})
	router.NoMethod(func(c *gin.Context) {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"})
	})
	router.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.GET("/openapi.json", s.openAPIDocument)
	router.HEAD("/openapi.json", s.openAPIDocument)
	router.GET("/docs", s.swaggerDocs)
	router.HEAD("/docs", s.swaggerDocs)
	router.GET("/redoc", s.redocDocs)
	router.HEAD("/redoc", s.redocDocs)
	router.GET("/", s.index)
	staticRoot := func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) }
	router.GET("/static", staticRoot)
	router.HEAD("/static", staticRoot)
	router.StaticFS("/static", http.FS(web.Static()))

	api := router.Group("/api")
	api.GET("/quote/:symbol", s.quote)
	api.GET("/expirations/:symbol", s.expirations)
	api.GET("/chain/:symbol", s.chain)
	api.POST("/refresh/:symbol", s.refresh)
	api.GET("/events/:symbol", s.eventsStream)
	api.GET("/flow/:symbol", s.flow)
	api.GET("/gamma/:symbol", s.gamma)
	api.GET("/levels/:symbol", s.levels)
	api.GET("/chan/:symbol", s.chanAnalysis)
	api.GET("/chan/:symbol/intraday", s.intradayChanAnalysis)
	api.GET("/chan/:symbol/adjusted", s.chanAdjustedAnalysis)
	api.GET("/status/:symbol", s.status)
	api.POST("/cleanup", s.cleanup)
	return router
}

// queryValue follows Starlette QueryParams.get semantics: scalar query
// parameters use the last value when a key is repeated.
func queryValue(c *gin.Context, name string) (string, bool) {
	values, present := c.Request.URL.Query()[name]
	if !present || len(values) == 0 {
		return "", false
	}
	return values[len(values)-1], true
}

func noStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

// staticCacheHeaders makes the embedded assets immutable for a year. Every
// project asset URL carries ?v=<content hash> and the vendor directories are
// versioned by name, so a rebuild naturally changes the URL. Without this the
// browser revalidates Vue, Element UI, jQuery and app.js on every navigation.
func staticCacheHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && strings.HasPrefix(c.Request.URL.Path, "/static/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
			c.Header("Vary", "Accept-Encoding")
		}
		c.Next()
	}
}

// normalizeJSONContentTypeResponseWriter removes Gin's implicit charset suffix
// from JSON responses. FastAPI/Starlette returns application/json for the same
// responses, including validation errors and access-guard rejections.
type normalizeJSONContentTypeResponseWriter struct {
	gin.ResponseWriter
}

func (w *normalizeJSONContentTypeResponseWriter) WriteHeaderNow() {
	if mediaType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type")); err == nil && strings.EqualFold(mediaType, "application/json") {
		w.Header().Set("Content-Type", "application/json")
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *normalizeJSONContentTypeResponseWriter) Write(value []byte) (int, error) {
	w.WriteHeaderNow()
	return w.ResponseWriter.Write(value)
}

func (w *normalizeJSONContentTypeResponseWriter) WriteString(value string) (int, error) {
	w.WriteHeaderNow()
	return w.ResponseWriter.WriteString(value)
}

func (w *normalizeJSONContentTypeResponseWriter) Flush() {
	w.WriteHeaderNow()
	w.ResponseWriter.Flush()
}

func normalizeJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer = &normalizeJSONContentTypeResponseWriter{ResponseWriter: c.Writer}
		c.Next()
	}
}

const gzipMinimumSize = 1024

func responseCompression(lowMemory bool) gin.HandlerFunc {
	// Compression stays enabled in low-memory mode: the payloads on the first
	// render path are 0.5-1.5 MiB JSON, and removing 75-90% of the transfer
	// costs far less CPU than waiting for the network. BestSpeed keeps the
	// compressor's 32 KiB window and hash tables cheap on small machines.
	level := gzip.DefaultCompression
	if lowMemory {
		level = gzip.BestSpeed
	}
	return func(c *gin.Context) {
		if !acceptsGzip(c.Request.Header.Values("Accept-Encoding")) {
			c.Next()
			return
		}
		writer := &gzipResponseWriter{
			ResponseWriter: c.Writer,
			accepted:       true,
			method:         c.Request.Method,
			level:          level,
			buffer:         make([]byte, 0, gzipMinimumSize-1),
		}
		c.Writer = writer
		c.Next()
		writer.finish()
	}
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	accepted  bool
	method    string
	level     int
	buffer    []byte
	gzip      *gzip.Writer
	bytes     int
	finalized bool
}

func acceptsGzip(values []string) bool {
	var gzipSeen, wildcardSeen bool
	gzipQuality, wildcardQuality := 0.0, 0.0
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			parts := strings.Split(strings.TrimSpace(item), ";")
			coding := strings.ToLower(strings.TrimSpace(parts[0]))
			if coding == "" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					quality = 0
				} else {
					quality = parsed
				}
				break
			}
			switch coding {
			case "gzip":
				gzipSeen, gzipQuality = true, quality
			case "*":
				wildcardSeen, wildcardQuality = true, quality
			}
		}
	}
	if gzipSeen {
		return gzipQuality > 0
	}
	return wildcardSeen && wildcardQuality > 0
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.Written() {
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) WriteHeaderNow() {
	if w.finalized {
		return
	}
	w.commitUncompressed()
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	w.bytes += len(data)
	if w.gzip != nil {
		return w.gzip.Write(data)
	}
	if w.finalized {
		return w.ResponseWriter.Write(data)
	}
	if !w.canCompress() {
		w.commitUncompressed()
		return w.ResponseWriter.Write(data)
	}
	if len(w.buffer)+len(data) < gzipMinimumSize {
		w.buffer = append(w.buffer, data...)
		return len(data), nil
	}
	if err := w.startCompression(); err != nil {
		return 0, err
	}
	return w.gzip.Write(data)
}

func (w *gzipResponseWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *gzipResponseWriter) Flush() {
	if w.gzip == nil {
		w.commitUncompressed()
		w.ResponseWriter.Flush()
		return
	}
	if err := w.gzip.Flush(); err != nil {
		return
	}
	w.ResponseWriter.Flush()
}

func (w *gzipResponseWriter) Status() int { return w.ResponseWriter.Status() }
func (w *gzipResponseWriter) Size() int   { return w.bytes }
func (w *gzipResponseWriter) Written() bool {
	return w.bytes > 0 || w.finalized || w.ResponseWriter.Written()
}

func (w *gzipResponseWriter) canCompress() bool {
	if !w.accepted || w.method == http.MethodHead {
		return false
	}
	status := w.ResponseWriter.Status()
	if status >= 100 && status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
		return false
	}
	header := w.Header()
	if header.Get("Content-Encoding") != "" || header.Get("Content-Range") != "" {
		return false
	}
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(header.Get("Content-Type"))), "text/event-stream")
}

func (w *gzipResponseWriter) ensureVary() {
	header := w.Header()
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Accept-Encoding") || strings.TrimSpace(token) == "*" {
				return
			}
		}
	}
	header.Add("Vary", "Accept-Encoding")
}

func (w *gzipResponseWriter) startCompression() error {
	w.ensureVary()
	header := w.Header()
	header.Del("Content-Length")
	header.Set("Content-Encoding", "gzip")
	w.ResponseWriter.WriteHeaderNow()
	w.finalized = true
	writer, err := gzip.NewWriterLevel(w.ResponseWriter, w.level)
	if err != nil {
		return err
	}
	w.gzip = writer
	if len(w.buffer) > 0 {
		if _, err := w.gzip.Write(w.buffer); err != nil {
			return err
		}
		w.buffer = w.buffer[:0]
	}
	return nil
}

func (w *gzipResponseWriter) commitUncompressed() {
	if w.finalized {
		return
	}
	w.ensureVary()
	w.ResponseWriter.WriteHeaderNow()
	w.finalized = true
	if len(w.buffer) > 0 {
		_, _ = w.ResponseWriter.Write(w.buffer)
		w.buffer = w.buffer[:0]
	}
}

func (w *gzipResponseWriter) finish() {
	if w.gzip != nil {
		_ = w.gzip.Close()
		return
	}
	w.commitUncompressed()
}

func quoteAPIValue(quote model.Quote) any {
	encoded, err := json.Marshal(quote)
	if err != nil {
		return quote
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return quote
	}
	if quote.FairValueWarning == "" {
		payload["fair_value_warning"] = nil
	}
	if quote.FairValueSource == "" {
		payload["fair_value_source"] = nil
	}
	if quote.Source == "sqlite" && quote.SnapshotID > 0 {
		payload["id"] = quote.SnapshotID
		payload["raw_json"] = quote.RawJSON
		payload["fair_value_methods_json"] = quote.FairValueMethodsJSON
		payload["fair_value_historical_valuation_percentiles_json"] = quote.FairValueHistoricalValuationJSON
	}
	return payload
}

func refreshAPIValue(response model.RefreshResponse) any {
	encoded, err := json.Marshal(response)
	if err != nil {
		return response
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return response
	}
	// FastAPI includes nullable expiration and zero rows in every refresh
	// response, including quote-only and skipped results; Go's model tags omit
	// those zero values, so restore the wire fields here.
	if _, exists := payload["expiration"]; !exists {
		payload["expiration"] = nil
	}
	if _, exists := payload["rows"]; !exists {
		payload["rows"] = 0
	}
	if response.Quote != nil {
		quote := *response.Quote
		quote.Source = "sqlite"
		payload["quote"] = quoteAPIValue(quote)
	}
	if response.Chain != nil {
		chain := response.Chain
		payload["chain"] = map[string]any{
			"symbol": chain.Symbol, "expiration": chain.Expiration, "fetched_at": chain.FetchedAt,
			"data": chainAPIOptions(chain.Data), "source": chain.Source, "iv_model": chain.IVModel, "oi_fallback": chain.OIFallback,
		}
	}
	return payload
}

func (s *Service) accessGuard() gin.HandlerFunc {
	expected := strings.TrimSpace(s.cfg.AccessKey)
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/static/") {
			c.Next()
			return
		}
		if expected == "" {
			c.Next()
			return
		}
		provided, _ := queryValue(c, "key")
		if provided == "" {
			provided = c.GetHeader("X-Access-Key")
		}
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			if strings.HasPrefix(path, "/api/") || path == "/api" {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"detail": "403 Forbidden"})
				return
			}
			c.Data(http.StatusForbidden, "text/html; charset=utf-8", []byte(forbiddenPage))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Service) index(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	assets := web.Static()
	page, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "index.html unavailable"})
		return
	}
	defaultSymbol := s.cfg.DefaultSymbol
	if len(s.cfg.DefaultSymbols) > 0 && strings.TrimSpace(s.cfg.DefaultSymbols[0]) != "" {
		defaultSymbol = s.cfg.DefaultSymbols[0]
	}
	content := strings.ReplaceAll(string(page), "__DEFAULT_SYMBOL__", defaultSymbol)
	content = strings.ReplaceAll(content, "__ACCESS_KEY_REQUIRED__", strconv.FormatBool(s.cfg.AccessKey != ""))
	versionHash := sha256.New()
	for _, asset := range []string{"common/css/styles.css", "common/js/request.js", "common/js/charts.js", "common/js/chan-client.js", "common/js/app.js", "common/img/option-scope-mark.svg", "common/img/option-scope-favicon.svg"} {
		if contents, err := fs.ReadFile(assets, asset); err == nil {
			_, _ = versionHash.Write(contents)
		}
	}
	version := hex.EncodeToString(versionHash.Sum(nil))
	content = strings.ReplaceAll(content, "__ASSET_VERSION__", version)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(content))
}

func (s *Service) quote(c *gin.Context) {
	if !s.validateQuery(c, boolQueryRule("refresh")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	refresh, err := queryBool(c, "refresh", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	cached, ok, loadErr := s.latestQuoteSnapshot(symbol)
	if loadErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": loadErr.Error()})
		return
	}
	if !refresh {
		if ok && cached.value.Price != nil {
			// Python reattaches valuation on lightweight cached-quote reads. Apply
			// the durable Go result first so ready, unavailable, and retry states do
			// not remain stuck at the old pending placeholder after a restart.
			quote := cached.value
			s.mergeCachedValuation(symbol, &quote)
			// Keep a stale Go value visible, but allow its six-hour cache entry to
			// refresh. Python-authoritative values are never replaced by this worker.
			if quote.FairValue == nil || (isGoFairValueSource(quote.FairValueSource) && quote.FairValueSource != goFairValueSource) ||
				(quote.FairValueSource == goFairValueSource && s.goValuationCacheExpired(symbol)) {
				s.scheduleValuationAfterSnapshot(symbol, nil, false)
			}
			quote.Source = "sqlite"
			// Cache reads stay on the critical path for a new symbol. Use only durable
			// SQLite inputs here and refresh stale history in the background.
			quote = s.decorateCachedQuote(quote)
			s.prefetchSymbolAnalytics(symbol, false)
			c.JSON(http.StatusOK, quoteAPIValue(quote))
			return
		}
		c.JSON(http.StatusOK, s.pendingQuoteWithCachedValuation(symbol))
		return
	}
	quote, err := s.fetchQuote(c.Request.Context(), symbol)
	if err != nil {
		cached, ok, loadErr := s.latestQuoteSnapshot(symbol)
		if loadErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": loadErr.Error()})
			return
		}
		if ok && cached.value.Price != nil {
			quote := cached.value
			s.mergeCachedValuation(symbol, &quote)
			quote.Source = "sqlite"
			quote.Warning = err.Error()
			quote = s.decorateQuote(c.Request.Context(), quote)
			c.JSON(http.StatusOK, quoteAPIValue(quote))
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"detail": err.Error()})
		return
	}
	if !validMarketPrice(quote.Price) {
		if ok && validMarketPrice(cached.value.Price) {
			quote := cached.value
			s.mergeCachedValuation(symbol, &quote)
			quote.Source = "sqlite"
			quote = s.decorateQuote(c.Request.Context(), quote)
			c.JSON(http.StatusOK, quoteAPIValue(quote))
			return
		}
		c.JSON(http.StatusOK, s.pendingQuoteWithCachedValuation(symbol))
		return
	}
	c.JSON(http.StatusOK, quoteAPIValue(quote))
}

func (s *Service) pendingQuoteWithCachedValuation(symbol string) map[string]any {
	response := pendingQuoteResponse(symbol)
	quote := model.Quote{
		Symbol: symbol, FairValueStatus: "pending", FairValuePending: true,
	}
	s.mergeCachedValuation(symbol, &quote)
	if quote.FairValue == nil || (isGoFairValueSource(quote.FairValueSource) && quote.FairValueSource != goFairValueSource) ||
		(quote.FairValueSource == goFairValueSource && s.goValuationCacheExpired(symbol)) {
		s.scheduleValuationAfterSnapshot(symbol, nil, false)
	}
	// A first-time quote request must not start history or benchmark
	// lookups before the snapshot refresh has fetched expirations and the option
	// chain. That background analytics pass is started after the snapshot commit.
	if mapped, ok := quoteAPIValue(quote).(map[string]any); ok {
		for key, value := range mapped {
			if key == "fair_value" || strings.HasPrefix(key, "fair_value_") {
				response[key] = value
			}
		}
	}
	return response
}

func (s *Service) latestQuoteSnapshot(symbol string) (cachedQuote, bool, error) {
	persisted, found, err := s.store.LatestQuote(symbol)
	if err != nil {
		return cachedQuote{}, false, err
	}
	if found {
		cached := cachedQuote{value: persisted, at: quoteTime(persisted)}
		s.mu.Lock()
		s.quotes[symbol] = cached
		s.trimSymbolCachesLocked()
		s.mu.Unlock()
		return cached, true, nil
	}
	s.mu.Lock()
	delete(s.quotes, symbol)
	s.mu.Unlock()
	return cachedQuote{}, false, nil
}

// latestChains collapses concurrent cache-miss reads for the same symbol and
// horizon. Callers share the returned map, so they must copy before appending
// to its slices.
func (s *Service) latestChains(symbol string, horizon int) (map[string]any, error) {
	value, err, _ := s.chainReads.Do(fmt.Sprintf("%s|%d", symbol, horizon), func() (any, error) {
		return s.store.LatestChains(symbol, horizon)
	})
	if err != nil {
		return nil, err
	}
	profile, _ := value.(map[string]any)
	return profile, nil
}

func (s *Service) fetchQuote(ctx context.Context, symbol string) (model.Quote, error) {
	// GET /quote mirrors Python's provider.quote path: return the enriched
	// upstream quote without writing a snapshot or publishing a quote event.
	return s.fetchQuoteForExpirationMode(ctx, symbol, "", false)
}

func (s *Service) fetchQuoteForExpiration(ctx context.Context, symbol, requestedExpiration string) (result model.Quote, resultErr error) {
	return s.fetchQuoteForExpirationMode(ctx, symbol, requestedExpiration, true)
}

func (s *Service) fetchQuoteForExpirationMode(ctx context.Context, symbol, requestedExpiration string, persist bool) (result model.Quote, resultErr error) {
	return s.fetchQuoteForExpirationModeWithSnapshotQuote(ctx, symbol, requestedExpiration, persist, false)
}

func (s *Service) fetchSnapshotQuoteForExpirationMode(ctx context.Context, symbol, requestedExpiration string, persist bool) (result model.Quote, resultErr error) {
	return s.fetchQuoteForExpirationModeWithSnapshotQuote(ctx, symbol, requestedExpiration, persist, true)
}

func (s *Service) fetchQuoteForExpirationModeWithSnapshotQuote(ctx context.Context, symbol, requestedExpiration string, persist, snapshot bool) (result model.Quote, resultErr error) {
	requestStartedAt := time.Now()
	quoteGateKey := "quote|" + symbol
	finish, previous, acquired, waited := s.acquireRefreshGate(ctx, quoteGateKey)
	if !acquired {
		return model.Quote{}, ctx.Err()
	}
	defer func() {
		response := model.RefreshResponse{}
		if resultErr == nil {
			response.Quote = &result
		}
		finish(requestedExpiration, response, resultErr)
	}()
	leaseName := "snapshot-quote:" + symbol
	leaseWait := 20 * time.Second
	if s.cfg.LowMemory {
		leaseWait = 3 * time.Second
	}
	leaseAcquired, leaseWaited, leaseErr := s.acquireDurableLease(ctx, leaseName, leaseWait)
	if leaseErr != nil {
		return model.Quote{}, leaseErr
	}
	if !leaseAcquired {
		if cached, found, loadErr := s.store.LatestQuote(symbol); loadErr == nil && found && cached.Price != nil {
			cached.Source = "sqlite"
			cached.FairValueWarning = "现货正在由其他进程刷新，本次沿用缓存"
			return s.decorateQuote(ctx, cached), nil
		}
		return model.Quote{}, fmt.Errorf("quote refresh lease wait exceeded %s", leaseWait)
	}
	ctx, renewer := startLeaseRenewal(ctx, func() (bool, error) {
		return s.store.RenewServiceLease(leaseName, s.leaseOwner)
	}, durableRefreshLease/3, leaseName)
	defer func() {
		renewer.stop()
		if err := s.store.ReleaseServiceLease(leaseName, s.leaseOwner); err != nil {
			log.Printf("release durable quote lease %s: %v", leaseName, err)
		}
	}()
	if waited && previous.completedAt.After(requestStartedAt) && time.Since(previous.completedAt) < 15*time.Second && previous.err == nil && previous.response.Quote != nil && (previous.requestedExpiration == requestedExpiration || previous.requestedExpiration == "" || requestedExpiration == "") {
		return *previous.response.Quote, nil
	}
	if leaseWaited {
		if cached, found, loadErr := s.store.LatestQuote(symbol); loadErr == nil && found && cached.Price != nil && time.Since(quoteTime(cached)) < 15*time.Second {
			cached.Source = "sqlite"
			return s.decorateQuote(ctx, cached), nil
		}
	}
	return s.fetchQuoteOnce(ctx, symbol, persist, snapshot)
}

func (s *Service) fetchQuoteOnce(ctx context.Context, symbol string, persist, snapshot bool) (model.Quote, error) {
	var quote model.Quote
	var err error
	if snapshot {
		if snapshotProvider, ok := s.provider.(snapshotQuoteProvider); ok {
			quote, err = snapshotProvider.SnapshotQuote(ctx, symbol)
		} else {
			quote, err = s.provider.Quote(ctx, symbol)
		}
	} else {
		quote, err = s.provider.Quote(ctx, symbol)
	}
	if err != nil {
		return model.Quote{}, err
	}
	if !persist {
		// Snapshot refreshes must commit quote/chain data before the browser can
		// render. Historical trend inputs are independent and can refresh in the
		// already-running analytics prefetch after the snapshot is saved.
		quote = s.decorateCachedQuote(quote)
		if preserved, preserveErr := s.store.PreserveCachedFairValue(quote); preserveErr == nil {
			quote = preserved
		}
		return quote, nil
	}
	quote = s.decorateQuote(ctx, quote)
	return s.persistQuoteSnapshot(symbol, quote)
}

func (s *Service) persistQuoteSnapshot(symbol string, quote model.Quote) (model.Quote, error) {
	if !validMarketPrice(quote.Price) {
		return model.Quote{}, fmt.Errorf("refusing to replace cached quote for %s without a positive price", symbol)
	}
	persisted, err := s.store.SaveQuotePreservingFairValue(quote)
	if err != nil {
		return model.Quote{}, fmt.Errorf("save quote snapshot: %w", err)
	}
	s.cacheQuote(symbol, persisted)
	s.publishQuoteEvent(symbol)
	return persisted, nil
}

func validMarketPrice(price *float64) bool {
	return price != nil && !math.IsNaN(*price) && !math.IsInf(*price, 0) && *price > 0
}

// quoteWasFetched distinguishes a provider quote from the SQLite fallback used
// when another worker holds the quote lease.
func quoteWasFetched(quote model.Quote) bool {
	return validMarketPrice(quote.Price) && !strings.EqualFold(strings.TrimSpace(quote.Source), "sqlite")
}

func (s *Service) cacheQuote(symbol string, quote model.Quote) {
	if !validMarketPrice(quote.Price) {
		return
	}
	s.mu.Lock()
	s.quotes[symbol] = cachedQuote{value: quote, at: time.Now()}
	s.trimSymbolCachesLocked()
	s.mu.Unlock()
}

func (s *Service) publishQuoteEvent(symbol string) {
	event := model.Event{Symbol: symbol, Kind: "quote", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	s.mu.RLock()
	if cached, ok := s.quotes[symbol]; ok {
		event.Payload = map[string]any{"quote": quoteAPIValue(cached.value)}
	}
	s.mu.RUnlock()
	if id, eventErr := s.store.AddEvent(event); eventErr == nil {
		event.ID = id
	}
	s.mu.Lock()
	s.appendEventLocked(event)
	s.mu.Unlock()
}

func (s *Service) expirations(c *gin.Context) {
	if !s.validateQuery(c, boolQueryRule("refresh")) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	refresh, err := queryBool(c, "refresh", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	values, ok, loadErr := s.store.LatestExpirations(symbol)
	if loadErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": loadErr.Error()})
		return
	}
	cachedExpirations, cacheErr := s.store.CachedExpirations(symbol)
	if cacheErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": cacheErr.Error()})
		return
	}
	values = activeExpirations(values)
	cachedExpirations = activeExpirations(cachedExpirations)
	if len(values) > 0 {
		s.mu.Lock()
		s.expires[symbol] = values
		s.mu.Unlock()
	}
	if len(values) > 0 && !refresh {
		c.JSON(http.StatusOK, model.ExpirationsResponse{Symbol: symbol, Expirations: values, CachedExpirations: cachedExpirations, Source: "sqlite"})
		return
	}
	if !refresh && (len(values) == 0 || !ok) {
		c.JSON(http.StatusOK, model.ExpirationsResponse{Symbol: symbol, Expirations: []string{}, CachedExpirations: cachedExpirations, Source: "pending"})
		return
	}
	upstreamValues, upstreamErr := s.provider.Expirations(c.Request.Context(), symbol)
	if upstreamErr != nil {
		if len(cachedExpirations) > 0 {
			c.JSON(http.StatusOK, model.ExpirationsResponse{Symbol: symbol, Expirations: cachedExpirations, CachedExpirations: cachedExpirations, Source: "sqlite", Warning: upstreamErr.Error()})
			return
		}
		if len(values) > 0 {
			c.JSON(http.StatusOK, model.ExpirationsResponse{Symbol: symbol, Expirations: values, CachedExpirations: cachedExpirations, Source: "sqlite", Warning: upstreamErr.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"detail": upstreamErr.Error()})
		return
	}
	values = upstreamValues
	values = activeExpirations(values)
	cachedExpirations, cacheErr = s.store.CachedExpirations(symbol)
	if cacheErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": cacheErr.Error()})
		return
	}
	cachedExpirations = activeExpirations(cachedExpirations)
	if len(values) > 0 {
		if err := s.store.SaveExpirations(symbol, values); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
			return
		}
		s.mu.Lock()
		s.expires[symbol] = values
		s.mu.Unlock()
	}
	c.JSON(http.StatusOK, model.ExpirationsResponse{Symbol: symbol, Expirations: values, CachedExpirations: cachedExpirations, Source: "upstream"})
}

var expirationPattern = regexp.MustCompile(`^\p{Nd}{4}-\p{Nd}{2}-\p{Nd}{2}$`)
var apiSymbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.-]*$`)

func normalizeAPISymbol(raw string) (string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	if symbol == "" || len(symbol) > 10 || !apiSymbolPattern.MatchString(symbol) {
		return "", errors.New("标的代码格式无效")
	}
	return symbol, nil
}

func strictExpiration(raw string) (string, error) {
	if !expirationPattern.MatchString(raw) {
		return "", fmt.Errorf("expiration must use YYYY-MM-DD")
	}
	return raw, nil
}

func pendingQuoteResponse(symbol string) map[string]any {
	return map[string]any{
		"symbol": symbol, "price": nil, "change_percent": nil, "currency": "USD",
		"market_state": nil, "sessions": map[string]any{}, "provider": "upstream", "source": "pending",
		"fair_value": nil, "fair_value_low": nil, "fair_value_high": nil,
		"fair_value_buy_low": nil, "fair_value_buy_high": nil, "fair_value_source": nil,
		"fair_value_model": nil, "fair_value_forward_eps": nil, "fair_value_forward_eps_source": nil,
		"fair_value_safety_margin": nil, "fair_value_confidence": nil, "fair_value_confidence_score": nil,
		"fair_value_interest_coverage": nil, "fair_value_regime": nil, "fair_value_regime_signals": nil,
		"fair_value_model_under_regime": nil, "fair_value_defensive": nil, "fair_value_optimistic": nil,
		"fair_value_normalized_eps_source": nil, "fair_value_quarterly_momentum": nil,
		"fair_value_historical_valuation_percentiles": nil, "fair_value_shareholder_total_return_yield": nil,
		"fair_value_market_cap_data_quality": nil, "fair_value_data_quality_score": nil,
		"fair_value_owner_earnings_maintenance_ratio": nil, "fair_value_owner_earnings_ratio_source": nil,
		"fair_value_status": "pending", "fair_value_warning": nil, "fair_value_pending": true,
	}
}

func queryBool(c *gin.Context, name string, fallback bool) (bool, error) {
	raw, present := queryValue(c, name)
	if !present {
		return fallback, nil
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	switch raw {
	case "1", "true", "yes", "y", "t", "on":
		return true, nil
	case "0", "false", "no", "n", "f", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean", name)
	}
}

type queryValidationRule struct {
	name             string
	kind             string
	required         bool
	pattern          string
	minimum          float64
	hasMinimum       bool
	minimumInclusive bool
	maximum          float64
	hasMaximum       bool
	maximumInclusive bool
}

var queryIntegerPattern = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*$`)

func boolQueryRule(name string) queryValidationRule {
	return queryValidationRule{name: name, kind: "bool"}
}

func patternedStringQueryRule(name string, required bool) queryValidationRule {
	return queryValidationRule{name: name, kind: "string", required: required, pattern: `^\d{4}-\d{2}-\d{2}$`}
}

func intQueryRule(name string, required bool, minimum, maximum *float64) queryValidationRule {
	rule := queryValidationRule{name: name, kind: "int", required: required}
	if minimum != nil {
		rule.minimum, rule.hasMinimum, rule.minimumInclusive = *minimum, true, true
	}
	if maximum != nil {
		rule.maximum, rule.hasMaximum, rule.maximumInclusive = *maximum, true, true
	}
	return rule
}

func positiveFloatQueryRule(name string) queryValidationRule {
	return queryValidationRule{name: name, kind: "float", minimum: 0, hasMinimum: true}
}

func queryBound(value float64) *float64 { return &value }

func parseQueryInteger(raw string) (*big.Int, bool) {
	value := strings.TrimSpace(raw)
	if !queryIntegerPattern.MatchString(value) {
		return nil, false
	}
	value = strings.ReplaceAll(value, "_", "")
	parsed, ok := new(big.Int).SetString(value, 10)
	return parsed, ok
}

func validatedQueryInt64(c *gin.Context, name string, fallback int64) int64 {
	raw, present := queryValue(c, name)
	if !present {
		return fallback
	}
	value, ok := parseQueryInteger(raw)
	if !ok {
		return fallback
	}
	if value.IsInt64() {
		return value.Int64()
	}
	if value.Sign() < 0 {
		return math.MinInt64
	}
	return math.MaxInt64
}

func parseSSEHeaderCursor(raw string) *big.Int {
	if raw == "" {
		return nil
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return nil
		}
	}
	value, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil
	}
	return value
}

func eventCursorInt64(value *big.Int) int64 {
	if value == nil || value.Sign() <= 0 {
		return 0
	}
	if value.IsInt64() {
		return value.Int64()
	}
	return math.MaxInt64
}

func finiteFloatPointer(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

func appendQueryValidationError(errors *[]map[string]any, name, kind, message string, input any, context map[string]any) {
	detail := map[string]any{
		"type":  kind,
		"loc":   []string{"query", name},
		"msg":   message,
		"input": input,
	}
	if context != nil {
		detail["ctx"] = context
	}
	*errors = append(*errors, detail)
}

func (s *Service) validateQuery(c *gin.Context, rules ...queryValidationRule) bool {
	var details []map[string]any
	for _, rule := range rules {
		raw, present := queryValue(c, rule.name)
		if !present {
			if rule.required {
				appendQueryValidationError(&details, rule.name, "missing", "Field required", nil, nil)
			}
			continue
		}

		switch rule.kind {
		case "bool":
			switch strings.ToLower(raw) {
			case "1", "true", "on", "yes", "y", "t":
			case "0", "false", "off", "no", "n", "f":
			default:
				appendQueryValidationError(&details, rule.name, "bool_parsing", "Input should be a valid boolean, unable to interpret input", raw, nil)
			}
		case "string":
			if !expirationPattern.MatchString(raw) {
				appendQueryValidationError(&details, rule.name, "string_pattern_mismatch", "String should match pattern '"+rule.pattern+"'", raw, map[string]any{"pattern": rule.pattern})
			}
		case "int":
			value, ok := parseQueryInteger(raw)
			if !ok {
				appendQueryValidationError(&details, rule.name, "int_parsing", "Input should be a valid integer, unable to parse string as an integer", raw, nil)
				break
			}
			if rule.hasMinimum {
				bound := big.NewInt(int64(rule.minimum))
				comparison := value.Cmp(bound)
				if comparison < 0 || (comparison == 0 && !rule.minimumInclusive) {
					typ, phrase, key := "greater_than_equal", "greater than or equal to", "ge"
					if !rule.minimumInclusive {
						typ, phrase, key = "greater_than", "greater than", "gt"
					}
					appendQueryValidationError(&details, rule.name, typ, fmt.Sprintf("Input should be %s %s", phrase, formatQueryBound(rule.minimum, rule.kind)), raw, map[string]any{key: queryBoundValue(rule.minimum, rule.kind)})
					continue
				}
			}
			if rule.hasMaximum {
				bound := big.NewInt(int64(rule.maximum))
				comparison := value.Cmp(bound)
				if comparison > 0 || (comparison == 0 && !rule.maximumInclusive) {
					typ, phrase, key := "less_than_equal", "less than or equal to", "le"
					if !rule.maximumInclusive {
						typ, phrase, key = "less_than", "less than", "lt"
					}
					appendQueryValidationError(&details, rule.name, typ, fmt.Sprintf("Input should be %s %s", phrase, formatQueryBound(rule.maximum, rule.kind)), raw, map[string]any{key: queryBoundValue(rule.maximum, rule.kind)})
				}
			}
		case "float":
			value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				appendQueryValidationError(&details, rule.name, "float_parsing", "Input should be a valid number, unable to parse string as a number", raw, nil)
				break
			}
			if rule.hasMinimum && (value < rule.minimum || (value == rule.minimum && !rule.minimumInclusive) || math.IsNaN(value)) {
				typ, phrase, key := "greater_than_equal", "greater than or equal to", "ge"
				if !rule.minimumInclusive {
					typ, phrase, key = "greater_than", "greater than", "gt"
				}
				appendQueryValidationError(&details, rule.name, typ, fmt.Sprintf("Input should be %s %s", phrase, formatQueryBound(rule.minimum, rule.kind)), raw, map[string]any{key: queryBoundValue(rule.minimum, rule.kind)})
			}
		default:
			panic("unknown query validation kind: " + rule.kind)
		}
	}
	if len(details) == 0 {
		return true
	}
	c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": details})
	return false
}

func queryBoundValue(value float64, kind string) any {
	if kind == "int" {
		return int64(value)
	}
	return value
}

func formatQueryBound(value float64, kind string) string {
	if kind == "int" {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func (s *Service) chain(c *gin.Context) {
	if !s.validateQuery(c, patternedStringQueryRule("expiration", true)) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	expirationRaw, _ := queryValue(c, "expiration")
	expiration, err := strictExpiration(expirationRaw)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	// Read the persisted batch on every lightweight request. The upstream cache
	// contains raw rows (before OI restoration), and another Go/Python worker may
	// have written a newer batch since this process last refreshed the symbol.
	chain, found, loadErr := s.store.LatestChain(symbol, expiration)
	if loadErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": loadErr.Error()})
		return
	}
	if found && len(chain.Data) > 0 {
		var spot *float64
		quote, quoteFound, quoteErr := s.store.LatestQuote(symbol)
		if quoteErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": quoteErr.Error()})
			return
		}
		if quoteFound {
			spot = quote.Price
		}
		chain.IVModel = gamma.Annotate(chain.Data, spot)
		// Python always includes iv_model for a non-empty chain, even when no
		// spot is available and annotation returns an empty map. Avoid omitempty.
		c.JSON(http.StatusOK, gin.H{
			"symbol": symbol, "expiration": expiration, "fetched_at": chain.FetchedAt,
			"data": chainAPIOptions(chain.Data), "source": "sqlite", "iv_model": chain.IVModel,
			"oi_fallback": chain.OIFallback,
		})
		return
	}
	c.JSON(http.StatusOK, model.ChainResponse{Symbol: symbol, Expiration: expiration, Data: []model.OptionRow{}, Source: "pending"})
}

func chainAPIOptions(rows []model.OptionRow) []map[string]any {
	options := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var inTheMoney *bool
		if row.InTheMoneyKnown {
			value := row.InTheMoney
			inTheMoney = &value
		}
		option := map[string]any{
			"contract_symbol": row.ContractSymbol, "contract_type": row.ContractType,
			"strike": row.Strike, "last_price": row.LastPrice, "bid": row.Bid, "ask": row.Ask,
			"volume": row.Volume, "open_interest": row.OpenInterest,
			"implied_volatility": row.ImpliedVolatility, "gamma": row.Gamma,
			"in_the_money": inTheMoney, "change_percent": row.ChangePercent, "expiration": row.Expiration,
		}
		if row.ModelIV != nil {
			option["model_iv"] = row.ModelIV
			option["model_gamma"] = row.ModelGamma
		}
		options = append(options, option)
	}
	return options
}

func (s *Service) refresh(c *gin.Context) {
	if !s.validateQuery(c,
		patternedStringQueryRule("expiration", false),
		intQueryRule("max_age", false, queryBound(0), queryBound(3600)),
		boolQueryRule("background"),
	) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	maxAge := 60 * time.Second
	if _, present := queryValue(c, "max_age"); present {
		seconds := validatedQueryInt64(c, "max_age", 60)
		maxAge = time.Duration(seconds) * time.Second
	}
	requestedExpiration, hasRequestedExpiration := queryValue(c, "expiration")
	if hasRequestedExpiration {
		requestedExpiration, err = strictExpiration(requestedExpiration)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
			return
		}
	}
	background, err := queryBool(c, "background", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	response, refreshErr := s.refreshSnapshotForRequest(c.Request.Context(), symbol, requestedExpiration, maxAge, background)
	if refreshErr != nil {
		var expirationErr ExpirationUnavailableError
		if errors.As(refreshErr, &expirationErr) {
			c.JSON(http.StatusNotFound, gin.H{"detail": expirationErr.Error()})
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"detail": refreshErr.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, refreshAPIValue(response))
}

func (s *Service) acquireRefreshGate(ctx context.Context, key string) (func(string, model.RefreshResponse, error), refreshResult, bool, bool) {
	s.mu.Lock()
	gate := s.refreshGates[key]
	if gate == nil {
		gate = &refreshGate{sem: make(chan struct{}, 1)}
		s.refreshGates[key] = gate
	}
	gate.refs++
	s.mu.Unlock()
	waited := false
	select {
	case gate.sem <- struct{}{}:
	default:
		waited = true
		select {
		case gate.sem <- struct{}{}:
		case <-ctx.Done():
			s.releaseRefreshGate(key, gate)
			return nil, refreshResult{}, false, waited
		}
	}
	previous := gate.completed
	return func(requestedExpiration string, response model.RefreshResponse, err error) {
		gate.completed = refreshResult{requestedExpiration: requestedExpiration, response: response, err: err, completedAt: time.Now()}
		<-gate.sem
		s.releaseRefreshGate(key, gate)
	}, previous, true, waited
}

// tryAcquireRefreshGate lets a frontend refresh use the latest SQLite snapshot
// while another local request or SSE worker refreshes the same symbol. Waiting
// behind that work made a cache hit appear unavailable for the full upstream
// request duration, even though the browser could render the saved chain.
func (s *Service) tryAcquireRefreshGate(key string) (func(string, model.RefreshResponse, error), refreshResult, bool) {
	s.mu.Lock()
	gate := s.refreshGates[key]
	if gate == nil {
		gate = &refreshGate{sem: make(chan struct{}, 1)}
		s.refreshGates[key] = gate
	}
	gate.refs++
	s.mu.Unlock()
	select {
	case gate.sem <- struct{}{}:
		previous := gate.completed
		return func(requestedExpiration string, response model.RefreshResponse, err error) {
			gate.completed = refreshResult{requestedExpiration: requestedExpiration, response: response, err: err, completedAt: time.Now()}
			<-gate.sem
			s.releaseRefreshGate(key, gate)
		}, previous, true
	default:
		s.releaseRefreshGate(key, gate)
		return nil, refreshResult{}, false
	}
}

func (s *Service) acquireDurableLease(ctx context.Context, name string, wait time.Duration) (bool, bool, error) {
	deadline := time.Now().Add(wait)
	waited := false
	for {
		acquired, err := s.store.TryAcquireServiceLeaseContext(ctx, name, s.leaseOwner, 180*time.Second)
		if err != nil {
			return false, waited, err
		}
		if acquired {
			return true, waited, nil
		}
		waited = true
		if time.Now().After(deadline) {
			return false, true, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, waited, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) releaseRefreshGate(key string, gate *refreshGate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate.refs--
	if gate.refs == 0 && s.refreshGates[key] == gate {
		delete(s.refreshGates, key)
	}
}

func snapshotRefreshGateKey(symbol, requestedExpiration string) string {
	if requestedExpiration == "" {
		requestedExpiration = "nearest"
	}
	return "snapshot|" + symbol + "|" + requestedExpiration
}

func (s *Service) refreshSnapshot(ctx context.Context, symbol, requestedExpiration string, maxAge time.Duration) (result model.RefreshResponse, resultErr error) {
	return s.refreshSnapshotWithOverrides(ctx, symbol, requestedExpiration, maxAge, nil, nil)
}

func (s *Service) refreshSnapshotWithOverrides(ctx context.Context, symbol, requestedExpiration string, maxAge time.Duration, expirationsOverride []string, quoteOverride *model.Quote) (result model.RefreshResponse, resultErr error) {
	return s.refreshSnapshotWithOverridesMode(ctx, symbol, requestedExpiration, maxAge, expirationsOverride, quoteOverride, false)
}

func (s *Service) refreshSnapshotForRequest(ctx context.Context, symbol, requestedExpiration string, maxAge time.Duration, forceBackground bool) (result model.RefreshResponse, resultErr error) {
	return s.refreshSnapshotWithOverridesMode(ctx, symbol, requestedExpiration, maxAge, nil, nil, forceBackground)
}

func (s *Service) refreshSnapshotWithOverridesMode(ctx context.Context, symbol, requestedExpiration string, maxAge time.Duration, expirationsOverride []string, quoteOverride *model.Quote, forceBackground bool) (result model.RefreshResponse, resultErr error) {
	requestStartedAt := time.Now()
	var expirationsDuration, quoteDuration, chainDuration, snapshotWriteDuration, earlyQuotePublishDuration, refreshRunDuration, leaseSetupDuration time.Duration
	var refreshGateWait, cacheCheckDuration, leaseWaitDuration time.Duration
	var refreshSource string
	defer func() {
		if elapsed := time.Since(requestStartedAt); elapsed >= time.Second {
			log.Printf("slow snapshot refresh symbol=%s total=%s source=%s gate_wait=%s cache_check=%s lease_wait=%s expirations=%s quote=%s early_quote_publish=%s chain=%s snapshot_write=%s refresh_run=%s lease_setup=%s low_memory=%t upstream_concurrency=%d",
				symbol, elapsed.Round(time.Millisecond), refreshSource, refreshGateWait.Round(time.Millisecond), cacheCheckDuration.Round(time.Millisecond), leaseWaitDuration.Round(time.Millisecond), expirationsDuration.Round(time.Millisecond), quoteDuration.Round(time.Millisecond), earlyQuotePublishDuration.Round(time.Millisecond), chainDuration.Round(time.Millisecond), snapshotWriteDuration.Round(time.Millisecond), refreshRunDuration.Round(time.Millisecond), leaseSetupDuration.Round(time.Millisecond), s.cfg.LowMemory, s.cfg.UpstreamConcurrency)
		}
	}()
	gateKey := snapshotRefreshGateKey(symbol, requestedExpiration)
	gateStartedAt := time.Now()
	finish, previous, acquired := s.tryAcquireRefreshGate(gateKey)
	if !acquired {
		if forceBackground {
			refreshGateWait = time.Since(gateStartedAt)
			refreshSource = "local_gate_busy"
			if err := ctx.Err(); err != nil {
				return model.RefreshResponse{}, err
			}
			if cachedResponse, found, cacheErr := s.cachedSnapshotRefreshResponse(symbol, requestedExpiration); cacheErr != nil {
				return model.RefreshResponse{}, cacheErr
			} else if found {
				return cachedResponse, nil
			}
			return s.deferredSnapshotRefreshResponse(symbol, requestedExpiration), nil
		}
		finish, previous, acquired, _ = s.acquireRefreshGate(ctx, gateKey)
		refreshGateWait = time.Since(gateStartedAt)
	}
	if !acquired {
		return model.RefreshResponse{}, ctx.Err()
	}
	defer func() { finish(requestedExpiration, result, resultErr) }()
	if previous.completedAt.After(requestStartedAt) && time.Since(previous.completedAt) < 15*time.Second && previous.err == nil && previous.response.Quote != nil && previous.requestedExpiration == requestedExpiration {
		refreshSource = "local_gate_coalesced"
		coalesced := previous.response
		coalesced.Coalesced = true
		return coalesced, nil
	}
	cacheCheckStartedAt := time.Now()
	cached, ok, loadErr := s.latestQuoteSnapshot(symbol)
	if loadErr != nil {
		return model.RefreshResponse{}, fmt.Errorf("load cached quote: %w", loadErr)
	}
	chainFresh := false
	chainExpiration := requestedExpiration
	if chainExpiration == "" {
		// A recent authoritative empty catalogue outranks stale non-empty
		// catalogues retained for the UI, otherwise restart/new-worker refreshes
		// would repeatedly fetch the old chain after an empty upstream response.
		if empty, probeErr := s.store.ExpirationProbeEmpty(symbol, maxAge); probeErr == nil && empty {
			chainFresh = true
		} else if values, found, loadErr := s.store.LatestExpirations(symbol); loadErr == nil && found {
			values = activeExpirations(values)
			if len(values) > 0 {
				chainExpiration = values[0]
			}
		}
	}
	if chainExpiration != "" {
		if chain, found, loadErr := s.store.LatestChain(symbol, chainExpiration); loadErr == nil && found && chain.FetchedAt != nil {
			chainFresh = time.Since(*chain.FetchedAt) < maxAge && hasTwoSidedQuotes(chain.Data)
		}
	}
	if ok && cached.value.Price != nil && chainFresh && maxAge > 0 && time.Since(cached.at) < maxAge {
		refreshSource = "fresh_sqlite"
		cacheCheckDuration = time.Since(cacheCheckStartedAt)
		quote := cached.value
		quote.Source = "sqlite"
		quote = s.decorateCachedQuote(quote)
		response := model.RefreshResponse{Skipped: true, FetchedAt: quote.FetchedAt, AgeSeconds: time.Since(cached.at).Seconds(), Quote: &quote, Rows: 0}
		if values, found, valuesErr := s.store.LatestExpirations(symbol); valuesErr == nil && found {
			response.Expirations = activeExpirations(values)
		}
		if requestedExpiration != "" {
			response.Expiration = requestedExpiration
		} else if chainExpiration != "" {
			response.Expiration = chainExpiration
		}
		return response, nil
	}
	cacheCheckDuration = time.Since(cacheCheckStartedAt)
	if forceBackground {
		// The first page render explicitly opts into SSE delivery. Return the
		// current cache now and do slow provider work on one tracked goroutine.
		activeRefresh := s.snapshotRefreshKeyInProgress(symbol, requestedExpiration)
		if activeRefresh {
			if cachedResponse, found, cacheErr := s.cachedSnapshotRefreshResponse(symbol, requestedExpiration); cacheErr != nil {
				return model.RefreshResponse{}, cacheErr
			} else if found {
				refreshSource = "background_already_scheduled"
				return cachedResponse, nil
			}
			refreshSource = "background_already_scheduled"
			return s.deferredSnapshotRefreshResponse(symbol, requestedExpiration), nil
		}
		if cachedResponse, found, cacheErr := s.cachedSnapshotRefreshResponse(symbol, requestedExpiration); cacheErr != nil {
			return model.RefreshResponse{}, cacheErr
		} else if found {
			scheduled := s.scheduleSnapshotRefresh(symbol, requestedExpiration, maxAge)
			if scheduled {
				refreshSource = "scheduled_background"
			} else {
				refreshSource = "background_already_scheduled"
			}
			return cachedResponse, nil
		}
		if s.scheduleSnapshotRefresh(symbol, requestedExpiration, maxAge) {
			refreshSource = "scheduled_cold_background"
		} else {
			refreshSource = "cold_background_already_scheduled"
		}
		return s.deferredSnapshotRefreshResponse(symbol, requestedExpiration), nil
	}
	// Match Python's durable per-symbol/expiration lease, so separate Go/Python
	// workers sharing this SQLite file do not issue duplicate provider requests.
	// Match Python's per-symbol/expiration scope key to coordinate the two
	// implementations sharing this SQLite database without blocking other expiries.
	leaseScope := requestedExpiration
	if leaseScope == "" {
		leaseScope = "nearest"
	}
	leaseName := "snapshot-refresh:" + symbol + ":" + leaseScope
	leaseStartedAt := time.Now()
	var leaseAcquired bool
	var leaseErr error
	// Background/SSE refreshes retain cross-process coordination and wait for
	// the current owner, while respecting cancellation. HTTP calls opted into
	// background delivery already return before reaching this point.
	leaseAcquired, _, leaseErr = s.acquireDurableLease(ctx, leaseName, 30*time.Second)
	if leaseErr != nil {
		return model.RefreshResponse{}, fmt.Errorf("acquire snapshot lease: %w", leaseErr)
	}
	leaseWaitDuration = time.Since(leaseStartedAt)
	if !leaseAcquired {
		if cachedResponse, found, cacheErr := s.cachedSnapshotRefreshResponse(symbol, requestedExpiration); cacheErr != nil {
			return model.RefreshResponse{}, cacheErr
		} else if found {
			refreshSource = "durable_lease_cache"
			return cachedResponse, nil
		}
		refreshSource = "durable_lease_wait_timeout"
		if err := ctx.Err(); err != nil {
			return model.RefreshResponse{}, err
		}
		return s.deferredSnapshotRefreshResponse(symbol, requestedExpiration), nil
	}
	ctx, renewer := startLeaseRenewal(ctx, func() (bool, error) {
		return s.store.RenewServiceLease(leaseName, s.leaseOwner)
	}, durableRefreshLease/3, leaseName)
	leaseSetupDuration = time.Since(leaseStartedAt)
	defer func() {
		renewer.stop()
		if err := s.store.ReleaseServiceLease(leaseName, s.leaseOwner); err != nil {
			log.Printf("release durable snapshot lease %s: %v", leaseName, err)
		}
	}()
	refreshSource = "upstream"
	runStartedAt := time.Now()
	runID, runErr := s.store.StartRefreshRun(symbol)
	refreshRunDuration = time.Since(runStartedAt)
	if runErr != nil {
		return model.RefreshResponse{}, fmt.Errorf("start refresh run: %w", runErr)
	}
	runStatus, runMessage := "success", ""
	defer func() {
		if resultErr != nil {
			runStatus, runMessage = "failed", resultErr.Error()
		} else if result.Warning != "" {
			runStatus, runMessage = "failed", result.Warning
		}
		if finishErr := s.store.FinishRefreshRun(runID, runStatus, result.Rows, runMessage); finishErr != nil && resultErr == nil {
			resultErr = fmt.Errorf("finish refresh run: %w", finishErr)
		}
	}()
	var quote model.Quote
	var err error
	var expires []string
	var expireErr error
	var preloadedRows []model.OptionRow
	var preloadedFetched *time.Time
	var preloadedExpiration string
	nearestProvider, supportsNearest := s.provider.(NearestChainProvider)
	preloadChain := supportsNearest && expirationsOverride == nil && requestedExpiration == ""
	// Quote and the expiration catalog are independent and use the provider's
	// dedicated first-render lane, which stays available even when the shared
	// upstream gate is a single slot. Run them together in low-memory mode too:
	// the price card should never wait behind the catalog, and the heavy chain
	// fetch still serializes on the shared gate.
	parallelQuoteAndCatalog := expirationsOverride == nil
	upstreamCtx := ctx
	cancelUpstream := func() {}
	if parallelQuoteAndCatalog {
		upstreamCtx, cancelUpstream = context.WithCancel(ctx)
		defer cancelUpstream()
	}
	var upstreamRequests sync.WaitGroup
	if parallelQuoteAndCatalog {
		upstreamRequests.Add(2)
		go func() {
			defer upstreamRequests.Done()
			if quoteOverride != nil {
				// The priority quote loop already fetched and published this
				// price; reuse it while the merged catalog/chain request runs.
				quote = s.decorateCachedQuote(*quoteOverride)
				return
			}
			started := time.Now()
			quote, err = s.fetchSnapshotQuoteForExpirationMode(upstreamCtx, symbol, requestedExpiration, false)
			quoteDuration = time.Since(started)
			if err == nil {
				publishStarted := time.Now()
				s.cacheQuote(symbol, quote)
				s.publishQuoteEvent(symbol)
				earlyQuotePublishDuration = time.Since(publishStarted)
			} else {
				// A failed quote aborts this refresh; stop the sibling catalogue
				// request instead of waiting for its full upstream timeout.
				cancelUpstream()
			}
		}()
		go func() {
			defer upstreamRequests.Done()
			started := time.Now()
			if preloadChain {
				// One no-date options response carries both the catalogue and the
				// nearest chain, removing a round trip from every cold symbol.
				expires, preloadedRows, preloadedFetched, expireErr = nearestProvider.NearestChain(upstreamCtx, symbol)
			} else {
				expires, expireErr = s.provider.Expirations(upstreamCtx, symbol)
			}
			expirationsDuration = time.Since(started)
			if expireErr == nil && len(preloadedRows) > 0 {
				if active := activeExpirations(expires); len(active) > 0 {
					preloadedExpiration = active[0]
				}
			}
		}()
	} else {
		// In constrained mode, publish the live quote before asking the provider
		// for the expiry catalogue. That request can be slow and is not needed to
		// show the current premarket, regular, after-hours, or overnight price.
		if quoteOverride != nil {
			quote = s.decorateCachedQuote(*quoteOverride)
		} else {
			started := time.Now()
			quote, err = s.fetchSnapshotQuoteForExpirationMode(ctx, symbol, requestedExpiration, false)
			quoteDuration = time.Since(started)
			if err == nil {
				publishStarted := time.Now()
				s.cacheQuote(symbol, quote)
				s.publishQuoteEvent(symbol)
				earlyQuotePublishDuration = time.Since(publishStarted)
			}
		}
		if err == nil && expirationsOverride == nil {
			started := time.Now()
			expires, expireErr = s.provider.Expirations(ctx, symbol)
			expirationsDuration = time.Since(started)
		}
	}
	if parallelQuoteAndCatalog {
		upstreamRequests.Wait()
	}
	if expirationsOverride != nil {
		expires = activeExpirations(append([]string(nil), expirationsOverride...))
	}
	if expireErr != nil && (err == nil || !errors.Is(expireErr, context.Canceled)) {
		fallback, used, fallbackErr := s.staleRefreshResponse(ctx, symbol, requestedExpiration, expireErr.Error())
		if fallbackErr != nil {
			resultErr = fallbackErr
			return model.RefreshResponse{}, fallbackErr
		}
		// The option catalogue is unavailable but daily analysis is independent;
		// let the history prefetch start instead of waiting for a durable quote
		// that this failed refresh will never write.
		s.prefetchSymbolAnalytics(symbol, true)
		if used {
			result = fallback
			return result, nil
		}
		resultErr = expireErr
		return model.RefreshResponse{}, expireErr
	}
	if err != nil {
		if expirationsOverride == nil {
			expires = activeExpirations(expires)
			if len(expires) > 0 {
				if saveErr := s.store.SaveExpirations(symbol, expires); saveErr != nil {
					resultErr = saveErr
					return model.RefreshResponse{}, saveErr
				}
				s.mu.Lock()
				s.expires[symbol] = expires
				s.mu.Unlock()
			}
		}
		fallback, used, fallbackErr := s.staleRefreshResponse(ctx, symbol, requestedExpiration, err.Error())
		if fallbackErr != nil {
			resultErr = fallbackErr
			return model.RefreshResponse{}, fallbackErr
		}
		s.prefetchSymbolAnalytics(symbol, true)
		if used {
			result = fallback
			return result, nil
		}
		resultErr = err
		return model.RefreshResponse{}, err
	}
	if !validMarketPrice(quote.Price) {
		const warning = "上游返回空行情，继续使用 SQLite 缓存"
		fallback, used, fallbackErr := s.staleRefreshResponse(ctx, symbol, requestedExpiration, warning)
		if fallbackErr != nil {
			resultErr = fallbackErr
			return model.RefreshResponse{}, fallbackErr
		}
		s.prefetchSymbolAnalytics(symbol, true)
		if used {
			result = fallback
			return result, nil
		}
		resultErr = fmt.Errorf("upstream quote has no valid positive price for %s", symbol)
		return model.RefreshResponse{}, resultErr
	}
	// In normal mode valuation can use the shared gate while the chain is being
	// fetched. Low-memory mode keeps the first chain request ahead of background work.
	if !s.cfg.LowMemory {
		s.scheduleValuationWithQuote(symbol, &quote)
	}
	response := model.RefreshResponse{FetchedAt: quote.FetchedAt, Quote: &quote}
	expires = activeExpirations(expires)
	// Keep the previous useful catalog on disk, but treat a successful empty
	// upstream result as quote-only for this refresh (do not fetch stale chains).
	if len(expires) > 0 && expirationsOverride == nil {
		if saveErr := s.store.SaveExpirations(symbol, expires); saveErr != nil {
			resultErr = saveErr
			return model.RefreshResponse{}, saveErr
		}
		s.mu.Lock()
		s.expires[symbol] = expires
		s.mu.Unlock()
	}
	response.Expirations = expires
	if len(expires) == 0 {
		if probeErr := s.store.SaveExpirationProbe(symbol, true); probeErr != nil {
			response.Warning = probeErr.Error()
		}
		s.mu.Lock()
		s.expires[symbol] = []string{}
		s.mu.Unlock()
		response.QuoteOnly = true
		persisted, persistErr := s.persistQuoteSnapshot(symbol, quote)
		if persistErr != nil {
			resultErr = persistErr
			return model.RefreshResponse{}, persistErr
		}
		response.Quote = &persisted
		response.FetchedAt = persisted.FetchedAt
		s.publishSnapshotEvent(symbol, "")
		s.prefetchSymbolAnalytics(symbol, true)
		if s.cfg.LowMemory {
			s.scheduleValuationAfterSnapshot(symbol, &quote, true)
		}
		return response, nil
	}
	selectedExpiration := requestedExpiration
	if selectedExpiration != "" && !containsExpiration(expires, selectedExpiration) {
		s.prefetchSymbolAnalytics(symbol, true)
		return response, ExpirationUnavailableError{Symbol: symbol, Expiration: selectedExpiration}
	}
	if selectedExpiration == "" && len(expires) > 0 {
		selectedExpiration = expires[0]
	}
	if selectedExpiration == "" {
		response.QuoteOnly = true
		persisted, persistErr := s.persistQuoteSnapshot(symbol, quote)
		if persistErr != nil {
			resultErr = persistErr
			return model.RefreshResponse{}, persistErr
		}
		response.Quote = &persisted
		s.prefetchSymbolAnalytics(symbol, true)
		if s.cfg.LowMemory {
			s.scheduleValuationAfterSnapshot(symbol, &quote, true)
		}
		return response, nil
	}
	response.Expiration = selectedExpiration
	chainStarted := time.Now()
	var rows []model.OptionRow
	var fetched *time.Time
	var chainErr error
	if preloadedExpiration != "" && preloadedExpiration == selectedExpiration && len(preloadedRows) > 0 {
		rows, fetched = preloadedRows, preloadedFetched
	} else {
		rows, fetched, chainErr = s.provider.Chain(ctx, symbol, selectedExpiration)
	}
	chainDuration = time.Since(chainStarted)
	if chainErr != nil {
		fallback, used, fallbackErr := s.staleRefreshResponse(ctx, symbol, requestedExpiration, chainErr.Error())
		if fallbackErr != nil {
			resultErr = fallbackErr
			return model.RefreshResponse{}, fallbackErr
		}
		s.prefetchSymbolAnalytics(symbol, true)
		if used {
			result = fallback
			return result, nil
		}
		resultErr = chainErr
		return model.RefreshResponse{}, chainErr
	}
	if quoteOverride != nil && fetched != nil {
		// The Gamma window reuses one cached quote for every expiry it refreshes.
		// A market refresh can commit a newer quote while this batch fetches the
		// chain; never let the cached copy re-publish an older market price as the
		// newest snapshot, which would keep a stale price on screen until the next
		// refresh.
		if latest, latestFound, latestErr := s.store.LatestQuote(symbol); latestErr == nil && latestFound && latest.Price != nil && latest.FetchedAt != nil && quote.FetchedAt != nil && latest.FetchedAt.After(*quote.FetchedAt) {
			quote = latest
		}
	}
	if quote.Price != nil {
		expiry, expiryErr := time.Parse("2006-01-02", selectedExpiration)
		if expiryErr == nil {
			now := time.Now().UTC()
			for index := range rows {
				row := &rows[index]
				if row.Gamma != nil || row.Strike == nil || row.ImpliedVolatility == nil {
					continue
				}
				if estimate, ok := gamma.EstimateOptionGamma(*quote.Price, *row.Strike, *row.ImpliedVolatility, expiry, now); ok {
					row.Gamma = &estimate
				}
			}
		}
	}
	chain := model.ChainResponse{Symbol: symbol, Expiration: selectedExpiration, FetchedAt: fetched, Data: rows, Source: "upstream"}
	chain.IVModel = gamma.Annotate(rows, quote.Price)
	snapshotWriteStartedAt := time.Now()
	persisted, saveErr := s.store.SaveQuoteAndChainSnapshot(quote, chain)
	snapshotWriteDuration = time.Since(snapshotWriteStartedAt)
	if saveErr != nil {
		resultErr = saveErr
		return model.RefreshResponse{}, saveErr
	}
	response.Quote = &persisted
	response.FetchedAt = fetched
	response.Chain = &chain
	key := symbol + "|" + selectedExpiration
	s.mu.Lock()
	s.quotes[symbol] = cachedQuote{value: persisted, at: time.Now()}
	s.chains[key] = cachedChain{value: chain, at: time.Now()}
	s.trimSymbolCachesLocked()
	s.mu.Unlock()
	s.publishQuoteEvent(symbol)
	s.publishSnapshotEvent(symbol, selectedExpiration)
	response.Rows = len(rows)
	s.prefetchSymbolAnalytics(symbol, true)
	if s.cfg.LowMemory {
		// Fundamentals stay behind the chain and adjusted history: the delayed
		// worker re-checks the refresh gate before it can take the shared slot.
		s.scheduleValuationAfterSnapshot(symbol, &quote, true)
	}
	return response, nil
}

func (s *Service) deferredSnapshotRefreshResponse(symbol, requestedExpiration string) model.RefreshResponse {
	response := model.RefreshResponse{
		Skipped: true, Stale: true, Deferred: true, Expirations: []string{},
		Warning: "同一标的的快照正在后台刷新，页面将通过 SSE 接收结果",
	}
	if quote, found, err := s.store.LatestQuote(symbol); err == nil && found && quote.Price != nil {
		quote.Source = "sqlite"
		quote = s.decorateCachedQuote(quote)
		response.Quote = &quote
		response.FetchedAt = quote.FetchedAt
	}
	if expirations, found, err := s.store.LatestExpirations(symbol); err == nil && found {
		response.Expirations = activeExpirations(expirations)
	}
	response.Expiration = requestedExpiration
	if response.Expiration == "" && len(response.Expirations) > 0 {
		response.Expiration = response.Expirations[0]
	}
	if response.Expiration != "" {
		if chain, found, err := s.store.LatestChain(symbol, response.Expiration); err == nil && found {
			response.Chain = &chain
			response.Rows = len(chain.Data)
			if chain.FetchedAt != nil {
				response.FetchedAt = chain.FetchedAt
			}
		}
	}
	return response
}

func (s *Service) scheduleSnapshotRefresh(symbol, requestedExpiration string, maxAge time.Duration) bool {
	if s == nil || s.store == nil || strings.TrimSpace(symbol) == "" {
		return false
	}
	key := snapshotRefreshGateKey(symbol, requestedExpiration)
	s.mu.Lock()
	if s.background.Err() != nil || s.snapshotRefreshJobs[key] {
		s.mu.Unlock()
		return false
	}
	s.snapshotRefreshJobs[key] = true
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.snapshotRefreshJobs, key)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.background, 3*time.Minute)
		defer cancel()
		if _, err := s.refreshSnapshot(ctx, symbol, requestedExpiration, maxAge); err != nil && ctx.Err() == nil {
			log.Printf("background snapshot refresh failed symbol=%s expiration=%s: %v", symbol, requestedExpiration, err)
		}
	}()
	return true
}

type gammaSnapshotRefreshContextKey struct{}

func (s *Service) cachedSnapshotRefreshResponse(symbol, requestedExpiration string) (model.RefreshResponse, bool, error) {
	quote, found, err := s.latestQuoteSnapshot(symbol)
	if err != nil {
		return model.RefreshResponse{}, false, fmt.Errorf("load cached quote: %w", err)
	}
	if !found || quote.value.Price == nil {
		return model.RefreshResponse{}, false, nil
	}
	expirations, _, err := s.store.LatestExpirations(symbol)
	if err != nil {
		return model.RefreshResponse{}, false, fmt.Errorf("load cached expirations: %w", err)
	}
	expirations = activeExpirations(expirations)
	target := requestedExpiration
	if target == "" && len(expirations) > 0 {
		target = expirations[0]
	}
	response := model.RefreshResponse{
		Skipped: true, Stale: true, Deferred: true, Expirations: expirations,
		FetchedAt: quote.value.FetchedAt, Quote: &quote.value,
		Warning: "同一标的的快照正在刷新，本次先返回 SQLite 缓存",
	}
	quoteValue := quote.value
	quoteValue.Source = "sqlite"
	response.Quote = &quoteValue
	if target != "" {
		response.Expiration = target
		chain, chainFound, chainErr := s.store.LatestChain(symbol, target)
		if chainErr != nil {
			return model.RefreshResponse{}, false, fmt.Errorf("load cached option chain: %w", chainErr)
		}
		if chainFound {
			response.Chain = &chain
			response.Rows = len(chain.Data)
			if chain.FetchedAt != nil {
				response.FetchedAt = chain.FetchedAt
			}
		}
	}
	decorated := s.decorateCachedQuote(*response.Quote)
	response.Quote = &decorated
	return response, true, nil
}

// snapshotRefreshKeyInProgress checks the exact scope first, then falls back
// to the nearest-expiration scope when the client selected a specific date.
// SSE subscriptions are deliberately excluded: they remain open while idle.
func (s *Service) snapshotRefreshKeyInProgress(symbol, requestedExpiration string) bool {
	if s == nil {
		return false
	}
	keys := []string{snapshotRefreshGateKey(symbol, requestedExpiration)}
	if requestedExpiration != "" {
		keys = append(keys, snapshotRefreshGateKey(symbol, ""))
	}
	s.mu.RLock()
	active := false
	for _, key := range keys {
		gate := s.refreshGates[key]
		// The caller already owns the exact key's gate here, so only another
		// compatible request (refs > 1) proves work is active. The nearest-key
		// fallback is independent and can be checked normally.
		gateActive := gate != nil && gate.refs > 0
		if key == snapshotRefreshGateKey(symbol, requestedExpiration) && gateActive {
			gateActive = gate.refs > 1
		}
		if s.snapshotRefreshJobs[key] || gateActive {
			active = true
			break
		}
	}
	s.mu.RUnlock()
	if active {
		return true
	}
	leaseScopes := []string{requestedExpiration}
	if requestedExpiration != "" {
		leaseScopes = append(leaseScopes, "nearest")
	}
	for _, scope := range leaseScopes {
		name := "snapshot-refresh:" + symbol + ":" + scope
		leased, err := s.store.ServiceLeaseActive(name, durableRefreshLease)
		if err == nil && leased {
			return true
		}
	}
	return false
}

// snapshotRefreshActive reports whether any local snapshot refresh currently
// owns the symbol. Background prefetches call it without holding the gate, so
// unlike snapshotRefreshKeyInProgress a lone in-flight refresh already counts.
func (s *Service) snapshotRefreshActive(symbol string) bool {
	if s == nil {
		return false
	}
	prefix := "snapshot|" + symbol + "|"
	s.mu.RLock()
	active := false
	for key, gate := range s.refreshGates {
		if strings.HasPrefix(key, prefix) && gate != nil && gate.refs > 0 {
			active = true
			break
		}
	}
	if !active {
		for key := range s.snapshotRefreshJobs {
			if strings.HasPrefix(key, prefix) {
				active = true
				break
			}
		}
	}
	s.mu.RUnlock()
	if active {
		return true
	}
	if leased, err := s.store.ServiceLeaseActive("snapshot-refresh:"+symbol+":nearest", durableRefreshLease); err == nil && leased {
		return true
	}
	return false
}

// hasDurableQuote reports whether SQLite already holds a usable quote. A cold
// symbol has none until the snapshot commit, so history prefetches wait for it
// instead of racing the quote/expiry/chain requests for the shared slot.
func (s *Service) hasDurableQuote(symbol string) bool {
	if s == nil || s.store == nil {
		return false
	}
	quote, found, err := s.store.LatestQuote(symbol)
	return err == nil && found && validMarketPrice(quote.Price)
}

// prefetchSymbolAnalytics warms the durable inputs consumed by levels. It runs
// after a snapshot is committed, so the browser can render the quote and chain
// immediately while Go fetches the slower history-derived signals in parallel.
// HTTP readers call it with force=false: while a snapshot refresh owns the
// symbol they leave the upstream slot to the quote and chain instead of
// starting a multi-year history request that would delay the first paint.
func (s *Service) prefetchSymbolAnalytics(symbol string, force bool) {
	if s == nil || s.store == nil || strings.TrimSpace(symbol) == "" {
		return
	}
	if !force && (s.snapshotRefreshActive(symbol) || !s.hasDurableQuote(symbol)) {
		return
	}
	s.mu.Lock()
	if s.analyticsPrefetch[symbol] {
		s.mu.Unlock()
		return
	}
	s.analyticsPrefetch[symbol] = true
	s.mu.Unlock()
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.analyticsPrefetch, symbol)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.background, 120*time.Second)
		defer cancel()
		// The adjusted Chan job is the single history source on a cold symbol:
		// it fetches raw OHLC together with the split/dividend factors and
		// writes both caches. Starting a raw two-year request here as well would
		// queue a duplicate upstream fetch behind the same single slot.
		s.ensureChanAdjustedHistory(symbol)
		if s.chanAdjustedJobActive(symbol) {
			return
		}
		history := s.loadHistory(ctx, symbol, "2y")
		for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
			if ctx.Err() != nil {
				return
			}
			warning, _ := history["warning"].(string)
			if !strings.Contains(warning, "内存保护") && !strings.Contains(warning, "日线刷新正在进行") && !strings.Contains(warning, "history refresh already running") {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			history = s.loadHistory(ctx, symbol, "2y")
		}
		bars, _ := history["bars"].([]map[string]any)
		if ctx.Err() != nil {
			return
		}
		analyticsChanged := history["source"] == "upstream"
		var jobs sync.WaitGroup
		jobs.Add(2)
		var extremes, beta map[string]any
		go func() { defer jobs.Done(); extremes = s.loadExtremes(ctx, symbol, history) }()
		go func() { defer jobs.Done(); beta = s.loadBeta(ctx, symbol, bars) }()
		jobs.Wait()
		if ctx.Err() != nil {
			return
		}
		for _, result := range []map[string]any{extremes, beta} {
			if result["source"] == "upstream" {
				analyticsChanged = true
			}
		}
		// Low-memory mode derives extrema from the 2y cache instead of fetching a
		// second, much larger max-history window. Surface that newly persisted
		// result to clients even though its source is SQLite.
		if warning, _ := extremes["warning"].(string); strings.HasPrefix(warning, "低内存保护：历史极值按两年日线计算") {
			analyticsChanged = true
		}
		if analyticsChanged {
			s.publishAnalyticsEvent(symbol, history["fetched_at"])
		}
	}()
}

func (s *Service) publishAnalyticsEvent(symbol string, historyFetchedAt any) {
	event := model.Event{Symbol: symbol, Kind: "analytics", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if historyFetchedAt != nil {
		// Clients compare this with the adjusted payload they already hold so a
		// derived-data event does not trigger another 1.5 MiB structure download.
		event.Payload = map[string]any{"history_fetched_at": historyFetchedAt}
	}
	if id, err := s.store.AddEvent(event); err == nil {
		event.ID = id
	}
	s.mu.Lock()
	s.appendEventLocked(event)
	s.mu.Unlock()
}

func (s *Service) staleRefreshResponse(ctx context.Context, symbol, requestedExpiration, warning string) (model.RefreshResponse, bool, error) {
	cached, found, err := s.latestQuoteSnapshot(symbol)
	if err != nil {
		return model.RefreshResponse{}, false, err
	}
	hasCachedPrice := found && cached.value.Price != nil && cached.value.FetchedAt != nil
	if !hasCachedPrice {
		return model.RefreshResponse{}, false, nil
	}

	expirations, _, err := s.store.LatestExpirations(symbol)
	if err != nil {
		return model.RefreshResponse{}, false, err
	}
	expirations = activeExpirations(expirations)
	target := requestedExpiration
	if target == "" && len(expirations) > 0 {
		target = expirations[0]
	}
	if target != "" {
		chain, chainFound, loadErr := s.store.LatestChain(symbol, target)
		if loadErr != nil {
			return model.RefreshResponse{}, false, loadErr
		}
		if chainFound && len(chain.Data) > 0 {
			quote := cached.value
			quote.Source = "sqlite"
			s.mergeCachedValuation(symbol, &quote)
			quote = s.decorateCachedQuote(quote)
			age := float64(0)
			if chain.FetchedAt != nil {
				age = math.Max(0, time.Since(*chain.FetchedAt).Seconds())
				age = math.RoundToEven(age*10) / 10
			}
			return model.RefreshResponse{
				Skipped: true, Stale: true, Warning: warning, FetchedAt: chain.FetchedAt,
				AgeSeconds: age, Expiration: target, Quote: &quote, Expirations: expirations,
			}, true, nil
		}
	}

	if requestedExpiration == "" {
		quote := cached.value
		quote.Source = "sqlite"
		s.mergeCachedValuation(symbol, &quote)
		quote = s.decorateCachedQuote(quote)
		age := math.Max(0, time.Since(*quote.FetchedAt).Seconds())
		age = math.RoundToEven(age*10) / 10
		return model.RefreshResponse{
			Skipped: true, Stale: true, Warning: warning, FetchedAt: quote.FetchedAt,
			AgeSeconds: age, QuoteOnly: true, Quote: &quote, Expirations: []string{},
		}, true, nil
	}
	return model.RefreshResponse{}, false, nil
}

func (s *Service) registerSubscription(key, symbol, expiration string) func() {
	s.mu.Lock()
	if s.background.Err() != nil {
		s.mu.Unlock()
		return func() {}
	}
	subscription, exists := s.subscriptions[key]
	if exists {
		subscription.refs++
		s.subscriptions[key] = subscription
		s.mu.Unlock()
		return func() { s.releaseSubscription(key) }
	}
	ctx, cancel := context.WithCancel(s.background)
	s.subscriptions[key] = eventSubscription{cancel: cancel, refs: 1}
	s.backgroundWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.backgroundWG.Done()
		s.subscriptionRefreshLoop(ctx, key, symbol, expiration)
	}()
	return func() { s.releaseSubscription(key) }
}

func (s *Service) releaseSubscription(key string) {
	s.mu.Lock()
	subscription, exists := s.subscriptions[key]
	if !exists {
		s.mu.Unlock()
		return
	}
	subscription.refs--
	if subscription.refs <= 0 {
		delete(s.subscriptions, key)
		s.mu.Unlock()
		subscription.cancel()
		return
	}
	s.subscriptions[key] = subscription
	s.mu.Unlock()
}

// subscriptionRefreshLoop gives the live quote its own cadence and schedules
// option-chain snapshots separately. The quote runs first and never takes the
// snapshot lease, so a slow or failing chain (and its backoff) cannot delay the
// price card.
func (s *Service) subscriptionRefreshLoop(ctx context.Context, key, symbol, expiration string) {
	var chainRefresh sync.WaitGroup
	defer chainRefresh.Wait()
	for {
		cycleStartedAt := time.Now()
		cadence := marketRefreshInterval(cycleStartedAt)

		quote, quoteErr := s.refreshSubscriptionQuote(ctx, symbol)
		if quoteErr != nil && ctx.Err() == nil {
			log.Printf("priority quote refresh failed for %s: %v", symbol, quoteErr)
		}
		if ctx.Err() != nil {
			return
		}

		claimed, claimErr := s.store.ClaimPushRefresh(symbol, expiration, cadence)
		if claimErr != nil {
			if ctx.Err() == nil {
				log.Printf("durable push refresh claim failed for %s|%s: %v", symbol, expiration, claimErr)
			}
		} else if claimed {
			var quoteOverride *model.Quote
			if quoteErr == nil {
				quoteOverride = &quote
			}
			chainRefresh.Add(1)
			go func() {
				defer chainRefresh.Done()
				s.refreshSubscriptionSnapshot(ctx, symbol, expiration, cadence, quoteOverride)
			}()
		}

		// Measure the cadence from the start of the cycle so a slow chain fetch
		// does not push the next quote refresh past the market interval.
		wait := cadence - time.Since(cycleStartedAt)
		if wait < time.Second {
			wait = time.Second
		}
		if !waitForSubscriptionRefresh(ctx, wait) {
			return
		}
	}
}

// refreshSubscriptionQuote is the high-priority path: quote only, no snapshot
// lease and no option catalog. The provider adapters already reserve their
// interactive upstream slots for the current price.
func (s *Service) refreshSubscriptionQuote(ctx context.Context, symbol string) (model.Quote, error) {
	quote, err := s.fetchSnapshotQuoteForExpirationMode(ctx, symbol, "", false)
	if err != nil {
		return model.Quote{}, err
	}
	if !quoteWasFetched(quote) {
		// Another worker holds the quote lease and just produced this cached
		// copy. Do not persist it again; the next cadence retries.
		return quote, nil
	}
	return s.persistQuoteSnapshot(symbol, quote)
}

func (s *Service) refreshSubscriptionSnapshot(ctx context.Context, symbol, expiration string, cadence time.Duration, quoteOverride *model.Quote) {
	// The quote was just persisted, so a 15-second window would let quote
	// freshness mask an older chain and skip every other option refresh. Keep a
	// short window: the subscription cadence, not the quote write, decides when
	// the chain refreshes.
	response, err := s.refreshSnapshotWithOverrides(ctx, symbol, expiration, 5*time.Second, nil, quoteOverride)
	finish := func() {
		if finishErr := s.store.FinishPushRefresh(symbol, expiration); finishErr != nil {
			log.Printf("durable push refresh finish failed for %s|%s: %v", symbol, expiration, finishErr)
		}
	}
	if ctx.Err() != nil {
		// Do not leave a 3-minute in-flight lease or a retry delay behind when
		// the subscription is closing.
		finish()
		return
	}
	if response.Deferred {
		// Another worker owns the snapshot lease; leave the cadence alone.
		finish()
		return
	}
	if err != nil || response.Stale || response.Warning != "" {
		warning := response.Warning
		if warning == "" && err != nil {
			warning = err.Error()
		}
		log.Printf("push snapshot refresh failed for %s|%s: %s", symbol, expiration, warning)
		if _, deferErr := s.store.DeferPushRefresh(symbol, expiration, cadence); deferErr != nil {
			log.Printf("durable push refresh defer failed for %s|%s: %v", symbol, expiration, deferErr)
		}
		return
	}
	finish()
}

func waitForSubscriptionRefresh(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func marketRefreshInterval(now time.Time) time.Duration {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	local := now.In(location)
	clock := local.Hour()*60 + local.Minute()
	day := localDate(local)
	if isNYSESession(day) {
		closeMinute := marketcalendar.RegularCloseMinute(day)
		if clock >= 9*60+30 && clock < closeMinute {
			return 20 * time.Second
		}
		if clock >= 4*60 && clock < 20*60 {
			return 60 * time.Second
		}
	}
	if clock >= 20*60 && isNYSESession(day.AddDate(0, 0, 1)) {
		return 60 * time.Second
	}
	if clock < 4*60 && isNYSESession(day) {
		return 60 * time.Second
	}
	return 5 * time.Minute
}

func localDate(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}

func isNYSESession(day time.Time) bool {
	return marketcalendar.IsTradingDay(day)
}

func isNYSEHoliday(day time.Time) bool { return !marketcalendar.IsTradingDay(day) }

func isNYSEEarlyClose(day time.Time) bool { return marketcalendar.IsEarlyClose(day) }

func (s *Service) eventsStream(c *gin.Context) {
	if !s.validateQuery(c,
		patternedStringQueryRule("expiration", false),
		intQueryRule("last_event_id", false, queryBound(0), nil),
	) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	_, hasQueryCursor := queryValue(c, "last_event_id")
	cursorValue := big.NewInt(0)
	if hasQueryCursor {
		if raw, present := queryValue(c, "last_event_id"); present {
			if parsed, ok := parseQueryInteger(raw); ok && parsed.Sign() >= 0 {
				cursorValue = parsed
			}
		}
	}
	if headerCursor := parseSSEHeaderCursor(c.GetHeader("Last-Event-ID")); headerCursor != nil {
		// Python accepts digit-only headers of arbitrary size. Preserve that cursor
		// for stream state, then clamp only at the int64-backed event-store edge.
		if headerCursor.Cmp(cursorValue) > 0 {
			cursorValue = headerCursor
		}
	}
	resumeCursor := cursorValue.Sign() > 0
	cursor := eventCursorInt64(cursorValue)
	if raw, hasExpiration := queryValue(c, "expiration"); hasExpiration {
		normalized, normalizeErr := strictExpiration(raw)
		if normalizeErr != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": normalizeErr.Error()})
			return
		}
		c.Set("expiration", normalized)
	}
	// A zero/absent cursor is a fresh connection, not a request to replay the
	// retained event log. Positive cursors are explicit reconnection points.
	if cursor == 0 {
		latest, latestErr := s.store.LatestEventID()
		if latestErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": latestErr.Error()})
			return
		}
		cursor = latest
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")
	expiration, _ := c.Get("expiration")
	expirationFilter, _ := expiration.(string)
	subscriptionKey := symbol + "|" + expirationFilter
	releaseSubscription := s.registerSubscription(subscriptionKey, symbol, expirationFilter)
	defer releaseSubscription()
	readySent := resumeCursor
	lastFrameAt := time.Now()
	c.Stream(func(w io.Writer) bool {
		if c.Request.Context().Err() != nil {
			return false
		}
		if !readySent {
			if !writeSSE(w, "", "ready", fmt.Sprintf(`{"symbol":%q,"kind":"ready"}`, symbol)) {
				return false
			}
			readySent = true
			lastFrameAt = time.Now()
			c.Writer.Flush()
		}
		pending, err := s.store.EventsSince(cursor, symbol, expirationFilter)
		if err != nil {
			return false
		}
		wroteEvent := false
		for _, event := range pending {
			event = s.attachEventPayload(event)
			data, _ := json.Marshal(eventAPIValue(event))
			if !writeSSE(w, strconv.FormatInt(event.ID, 10), "update", string(data)) {
				return false
			}
			cursor = event.ID
			wroteEvent = true
		}
		if wroteEvent {
			lastFrameAt = time.Now()
		}
		if time.Since(lastFrameAt) >= 15*time.Second {
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return false
			}
			lastFrameAt = time.Now()
		}
		c.Writer.Flush()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-c.Request.Context().Done():
			return false
		}
	})
}

func eventAPIValue(event model.Event) map[string]any {
	var expiration any
	if event.Expiration != "" {
		expiration = event.Expiration
	}
	result := map[string]any{
		"symbol": event.Symbol, "kind": event.Kind,
		"created_at": event.CreatedAt, "expiration": expiration,
	}
	if event.Payload != nil {
		result["payload"] = event.Payload
	}
	return result
}

func eventPayloadKey(symbol, expiration, createdAt string) string {
	return symbol + "|" + expiration + "|" + createdAt
}

func (s *Service) attachEventPayload(event model.Event) model.Event {
	if s == nil {
		return event
	}
	s.mu.RLock()
	if payload := s.events.payloads[eventPayloadKey(event.Symbol, event.Expiration, event.CreatedAt)]; payload != nil {
		event.Payload = payload
	}
	s.mu.RUnlock()
	return event
}

func writeSSE(w io.Writer, id, event, data string) bool {
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return false
		}
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return false
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err == nil
}

func (s *Service) publishSnapshotEvent(symbol, expiration string) {
	event := model.Event{Symbol: symbol, Kind: "snapshot", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Expiration: expiration}
	event.Payload = s.snapshotEventPayload(symbol, expiration)
	if id, err := s.store.AddEvent(event); err == nil {
		event.ID = id
	}
	s.mu.Lock()
	s.appendEventLocked(event)
	s.mu.Unlock()
}

func (s *Service) appendEventLocked(event model.Event) {
	event.ID = s.events.nextID
	s.events.nextID++
	s.events.events = append(s.events.events, event)
	if len(s.events.events) > 256 {
		s.events.events = s.events.events[len(s.events.events)-256:]
	}
	if event.Payload != nil {
		if s.events.payloads == nil {
			s.events.payloads = make(map[string]map[string]any)
		}
		key := eventPayloadKey(event.Symbol, event.Expiration, event.CreatedAt)
		s.events.payloads[key] = event.Payload
		if len(s.events.payloads) > 256 {
			for oldKey := range s.events.payloads {
				if oldKey != key {
					delete(s.events.payloads, oldKey)
					break
				}
			}
		}
	}
}

func (s *Service) snapshotEventPayload(symbol, expiration string) map[string]any {
	if s == nil || strings.TrimSpace(symbol) == "" || strings.TrimSpace(expiration) == "" {
		return nil
	}
	s.mu.RLock()
	quote, quoteOK := s.quotes[symbol]
	chain, chainOK := s.chains[symbol+"|"+expiration]
	s.mu.RUnlock()
	if !quoteOK {
		cached, found, err := s.latestQuoteSnapshot(symbol)
		if err == nil && found {
			quote, quoteOK = cached, true
		}
	}
	if !chainOK {
		cached, found, err := s.store.LatestChain(symbol, expiration)
		if err == nil && found {
			chain, chainOK = cachedChain{value: cached, at: time.Now()}, true
		}
	}
	if !quoteOK || !chainOK || len(chain.value.Data) == 0 {
		return nil
	}
	var spot *float64
	if quote.value.Price != nil {
		spot = quote.value.Price
	}
	chainValue := chain.value
	chainValue.IVModel = gamma.Annotate(chainValue.Data, spot)
	return map[string]any{
		"quote": quoteAPIValue(quote.value),
		"chain": map[string]any{
			"symbol": chainValue.Symbol, "expiration": chainValue.Expiration, "fetched_at": chainValue.FetchedAt,
			"data": chainAPIOptions(chainValue.Data), "source": "sqlite", "iv_model": chainValue.IVModel, "oi_fallback": chainValue.OIFallback,
		},
	}
}

func (s *Service) status(c *gin.Context) {
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	cached, ok, loadErr := s.latestQuoteSnapshot(symbol)
	if loadErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": loadErr.Error()})
		return
	}
	s.mu.RLock()
	refreshing := s.symbolRefreshActiveLocked(symbol)
	s.mu.RUnlock()
	var quote any
	if ok {
		// Python routes status quotes through quote_response, so stale history is
		// refreshed and attached just as it is on /quote.
		decorated := s.decorateStatusQuote(c.Request.Context(), cached.value)
		decorated.Source = "sqlite"
		quote = quoteAPIValue(decorated)
	}
	var refreshInfo any
	if latest, found, statusErr := s.store.LatestRefreshRun(symbol); statusErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": statusErr.Error()})
		return
	} else if found {
		latestInfo := gin.H{}
		for key, value := range latest {
			latestInfo[key] = value
		}
		refreshInfo = latestInfo
	}
	latestStatus := ""
	if latestInfo, ok := refreshInfo.(gin.H); ok {
		latestStatus, _ = latestInfo["status"].(string)
	}
	if refreshing && latestStatus != "running" {
		// A quote-only request may be active while the persisted record still
		// describes an older completed snapshot refresh. Do not combine the
		// transient running state with that historical run's id/timestamps.
		refreshInfo = gin.H{"status": "running"}
	}
	c.JSON(http.StatusOK, gin.H{"symbol": symbol, "quote": quote, "refresh": refreshInfo})
}

// decorateStatusQuote keeps the status-specific field normalization while
// sharing Python's quote_response enrichment, including stale-cache refreshes.
func (s *Service) decorateStatusQuote(ctx context.Context, quote model.Quote) model.Quote {
	quote = s.decorateQuote(ctx, quote)
	if quote.Sessions == nil {
		quote.Sessions = map[string]any{}
	}

	// Store readers restore persisted valuation columns, but normalize state for
	// quotes from the in-memory cache or older rows as Python does.
	if quote.FairValueSource != "" && quote.FairValue != nil {
		// A completed fair value is ready unless the Go status cache recorded a
		// newer retry/terminal attempt. mergeCachedValuation overlays that attempt
		// before this normalization step; preserve it when present.
		if quote.FairValueSource != goFairValueSource || (quote.FairValueStatus != "retry" && quote.FairValueStatus != "failed" && quote.FairValueStatus != valuation.StatusUnavailable && quote.FairValueStatus != valuation.StatusInvalid) {
			quote.FairValueStatus = "ready"
			quote.FairValuePending = false
		}
	} else if quote.FairValueStatus == "" {
		quote.FairValueStatus = "pending"
		quote.FairValuePending = true
	} else {
		quote.FairValuePending = quote.FairValueStatus == "pending" || quote.FairValueStatus == "retry"
	}

	return quote
}

// refreshGateActiveLocked reports whether a refresh owns or is waiting for a gate.
// refs includes the owner and all queued callers. The caller must hold s.mu.
func (s *Service) refreshGateActiveLocked(key string) bool {
	gate := s.refreshGates[key]
	return gate != nil && gate.refs > 0
}

func (s *Service) symbolRefreshActiveLocked(symbol string) bool {
	if s.refreshGateActiveLocked(symbol) || s.refreshGateActiveLocked("quote|"+symbol) {
		return true
	}
	prefix := "snapshot|" + symbol + "|"
	for key, gate := range s.refreshGates {
		if gate != nil && gate.refs > 0 && strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

const housekeepingLeaseName = "option-database-housekeeping"

var errCleanupLeaseBusy = errors.New("database cleanup is already running in another process")

// withCleanupLease serializes maintenance in one Service and uses the shared
// SQLite lease to exclude another process. A held housekeeping lease owned by
// this Service is borrowed, never released by a manual request.
func (s *Service) withCleanupLease(operation func() error) (operationErr error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	s.cleanupActive = true
	defer func() { s.cleanupActive = false }()
	manualLease := false
	acquired, err := s.store.TryAcquireServiceLease(housekeepingLeaseName, s.leaseOwner, 48*time.Hour)
	if err != nil {
		return fmt.Errorf("acquire database cleanup lease: %w", err)
	}
	if !acquired {
		if s.housekeepingLeaseHeld {
			s.housekeepingLeaseHeld = false
		}
		return errCleanupLeaseBusy
	}
	// Even when borrowing housekeeping's long-lived lease, reacquire it under
	// the local mutex. This renews the lease and proves we still own it before
	// allowing a manual operation to mutate SQLite.
	manualLease = !s.housekeepingLeaseHeld
	if manualLease {
		defer func() {
			if err := s.store.ReleaseServiceLease(housekeepingLeaseName, s.leaseOwner); err != nil && operationErr == nil {
				operationErr = fmt.Errorf("release database cleanup lease: %w", err)
			}
		}()
	}
	return operation()
}

func (s *Service) runCleanupPass(skipSizeWhileBusy bool) {
	_, _ = s.store.Cleanup(s.cfg.RawRetentionDays)
	if skipSizeWhileBusy && s.cfg.LowMemory && s.refreshWorkActive() {
		return
	}
	limitMB := s.cfg.DatabaseMaxMB
	if s.cfg.LowMemory {
		if free, err := s.store.AvailableDiskSpaceMB(); err == nil {
			budget := free - 96
			if budget < 32 {
				budget = 32
			}
			if limitMB <= 0 || int64(budget) < limitMB {
				limitMB = int64(budget)
			}
		} else if limitMB <= 0 {
			limitMB = 64
		}
	}
	_, _ = s.store.CleanupBySize(limitMB*1048576, 5000, s.cfg.SizeCleanupBudget)
}

// runHousekeepingPass serializes lease state and cleanup mutations. The initial
// pass claims the long-lived lease; later passes renew it before touching SQLite.
func (s *Service) runHousekeepingPass(initial bool) bool {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	s.cleanupActive = true
	defer func() { s.cleanupActive = false }()
	if initial {
		acquired, err := s.store.TryAcquireServiceLease(housekeepingLeaseName, s.leaseOwner, 48*time.Hour)
		if err != nil || !acquired {
			return false
		}
		s.housekeepingLeaseHeld = true
	} else {
		if !s.housekeepingLeaseHeld {
			return false
		}
		live, err := s.store.TryAcquireServiceLease(housekeepingLeaseName, s.leaseOwner, 48*time.Hour)
		if err != nil {
			// Keep the held flag so the goroutine's deferred owner-scoped release
			// can free the lease on exit after a transient database error.
			return false
		}
		if !live {
			s.housekeepingLeaseHeld = false
			return false
		}
	}
	if initial {
		if cleared, err := s.store.PruneLegacyRawJSON(5000, s.cfg.SizeCleanupBudget); err != nil {
			log.Printf("prune legacy raw_json failed: %v", err)
		} else if cleared > 0 {
			log.Printf("pruned %d legacy raw_json rows", cleared)
		}
	}
	s.runCleanupPass(true)
	return true
}

func (s *Service) releaseHousekeepingLease() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if !s.housekeepingLeaseHeld {
		return
	}
	if err := s.store.ReleaseServiceLease(housekeepingLeaseName, s.leaseOwner); err != nil {
		log.Printf("release housekeeping lease: %v", err)
	}
	s.housekeepingLeaseHeld = false
}

func (s *Service) startHousekeeping() {
	s.cleanupOnce.Do(func() {
		s.backgroundWG.Add(1)
		go func() {
			defer s.backgroundWG.Done()
			if !s.runHousekeepingPass(true) {
				return
			}
			defer s.releaseHousekeepingLease()
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-s.background.Done():
					return
				case <-ticker.C:
					if !s.runHousekeepingPass(false) {
						return
					}
				}
			}
		}()
	})
}

func (s *Service) refreshWorkActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.refreshGates) > 0 || len(s.gammaJobs) > 0 || len(s.historyLocks) > 0
}

func (s *Service) cleanup(c *gin.Context) {
	var body gin.H
	err := s.withCleanupLease(func() error {
		deleted, err := s.store.Cleanup(s.cfg.RawRetentionDays)
		if err != nil {
			return err
		}
		if deleted["quotes"] > 0 || deleted["options"] > 0 {
			s.mu.Lock()
			if deleted["quotes"] > 0 {
				s.quotes = make(map[string]cachedQuote)
			}
			if deleted["options"] > 0 {
				s.chains = make(map[string]cachedChain)
			}
			s.mu.Unlock()
		}
		size, err := s.store.CleanupBySize(s.cfg.DatabaseMaxMB*1048576, 5000, s.cfg.SizeCleanupBudget)
		if err != nil {
			return err
		}
		body = gin.H{"deleted": deleted, "size": size}
		return nil
	})
	writeCleanupResponse(c, body, err)
}

func writeCleanupResponse(c *gin.Context, body gin.H, err error) {
	if errors.Is(err, errCleanupLeaseBusy) {
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, body)
}

func (s *Service) flow(c *gin.Context) {
	if !s.validateQuery(c, patternedStringQueryRule("expiration", true)) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	expirationRaw, _ := queryValue(c, "expiration")
	expiration, err := strictExpiration(expirationRaw)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	result, err := s.store.OptionFlow(symbol, expiration)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Service) gamma(c *gin.Context) {
	if !s.validateQuery(c,
		boolQueryRule("refresh"),
		intQueryRule("horizon_days", false, queryBound(1), queryBound(365)),
		boolQueryRule("status_only"),
		boolQueryRule("include_rows"),
	) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	horizon := 45
	if _, present := queryValue(c, "horizon_days"); present {
		horizon = int(validatedQueryInt64(c, "horizon_days", 45))
	}
	statusOnly, err := queryBool(c, "status_only", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	includeRows, err := queryBool(c, "include_rows", true)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	refresh, err := queryBool(c, "refresh", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	var refreshState map[string]any
	if refresh {
		refreshState, _ = s.claimGammaJob(symbol, horizon)
	}
	if statusOnly {
		if refreshState == nil {
			refreshState = s.gammaJobStatus(symbol, horizon)
		}
		c.JSON(http.StatusOK, gin.H{"symbol": symbol, "source": "sqlite", "refresh": refreshState, "data": []any{}, "contract_count": 0, "status_only": true})
		return
	}
	if s.cfg.LowMemory {
		select {
		case s.gammaHeavyGate <- struct{}{}:
			defer func() { <-s.gammaHeavyGate }()
		default:
			if refreshState == nil {
				refreshState = s.gammaJobStatus(symbol, horizon)
			}
			c.JSON(http.StatusOK, gin.H{
				"symbol": symbol, "source": "sqlite", "refresh": refreshState,
				"data": []any{}, "contract_count": 0, "iv_model": map[string]any{},
				"zero_gamma": nil, "status_only": false, "degraded": true,
				"warning": "内存保护：已有刷新在进行，Gamma 窗口稍后计算",
			})
			return
		}
	}
	profile, err := s.latestChains(symbol, horizon)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	rows, _ := profile["data"].([]model.OptionRow)
	cached, ok, quoteErr := s.latestQuoteSnapshot(symbol)
	if quoteErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": quoteErr.Error()})
		return
	}
	var spot *float64
	if ok {
		spot = cached.value.Price
	}
	cacheKey := gammaAnalysisCacheKey(symbol, horizon, includeRows, profile["fetched_at"], profile["oi_fallback"], rows, spot)
	var analysis map[string]any
	if cachedValue, found, cacheErr := s.store.GetAnalysisCache(cacheKey); cacheErr == nil && found {
		analysis, _ = cachedValue.(map[string]any)
	}
	if analysis == nil {
		ivModel := gamma.Annotate(rows, spot)
		zero := gamma.ZeroGamma(rows, spot, horizon)
		data := any([]model.OptionRow{})
		if includeRows {
			data = rows
		}
		analysis = map[string]any{
			"iv_model": ivModel, "zero_gamma": zero, "data": data,
			"contract_count": len(rows),
		}
		// Python avoids retaining bulky full-window Gamma payloads in low-memory mode;
		// summary-only requests remain durable and reusable across workers.
		if !s.cfg.LowMemory || !includeRows {
			cacheEntries := s.cfg.AnalysisCacheEntries
			if cacheEntries <= 0 {
				cacheEntries = 128
			}
			_ = s.store.PutAnalysisCache(cacheKey, analysis, cacheEntries)
		}
	}
	if refreshState == nil {
		refreshState = s.gammaJobStatus(symbol, horizon)
	}
	response := gin.H{
		"symbol": symbol, "iv_model": analysis["iv_model"], "expirations": profile["expirations"], "horizon_days": horizon,
		"fetched_at": profile["fetched_at"], "data": analysis["data"], "contract_count": analysis["contract_count"], "source": "sqlite",
		"oi_fallback": profile["oi_fallback"], "refresh": refreshState, "zero_gamma": analysis["zero_gamma"], "status_only": false,
	}
	c.JSON(http.StatusOK, response)
}

func (s *Service) levels(c *gin.Context) {
	requestStartedAt := time.Now()
	defer func() {
		if elapsed := time.Since(requestStartedAt); elapsed >= time.Second {
			log.Printf("slow levels request symbol=%s raw=%s total=%s", c.Param("symbol"), c.Query("raw"), elapsed.Round(time.Millisecond))
		}
	}()
	if !s.validateQuery(c,
		patternedStringQueryRule("expiration", false),
		positiveFloatQueryRule("spot"),
		boolQueryRule("raw"),
		intQueryRule("raw_expirations", false, queryBound(1), queryBound(8)),
	) {
		return
	}
	symbol, err := normalizeAPISymbol(c.Param("symbol"))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	rawExpiration, hasExpiration := queryValue(c, "expiration")
	expiration := ""
	if hasExpiration {
		expiration, err = strictExpiration(rawExpiration)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "expiration must use YYYY-MM-DD"})
			return
		}
	}
	rawExpirationCount := 4
	if _, present := queryValue(c, "raw_expirations"); present {
		rawExpirationCount = int(validatedQueryInt64(c, "raw_expirations", 4))
	}
	spot := any(nil)
	if raw, hasSpot := queryValue(c, "spot"); hasSpot {
		value, parseErr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if parseErr != nil || math.IsNaN(value) || value <= 0 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "spot must be greater than 0"})
			return
		}
		spot = value
	}
	raw, err := queryBool(c, "raw", false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	if raw {
		var profile map[string]any
		var profileErr error
		var chain model.ChainResponse
		var chainFound bool
		var chainErr error
		var quote model.Quote
		var quoteFound bool
		var quoteErr error
		var history map[string]any
		var historyErr error
		var extremes, beta map[string]any
		var extremesErr, betaErr error
		if s.cfg.LowMemory || s.cfg.UpstreamConcurrency <= 1 {
			profile, profileErr = s.latestChains(symbol, 45)
			chain, chainFound, chainErr = s.store.LatestChain(symbol, expiration)
			quote, quoteFound, quoteErr = s.store.LatestQuote(symbol)
			history, historyErr = s.store.LatestHistory(symbol, 3000)
			extremes, extremesErr = s.store.LatestPayload("price_extremes", symbol)
			beta, betaErr = s.store.LatestPayload("beta_snapshots", symbol)
		} else {
			var reads sync.WaitGroup
			reads.Add(6)
			go func() { defer reads.Done(); profile, profileErr = s.latestChains(symbol, 45) }()
			go func() { defer reads.Done(); chain, chainFound, chainErr = s.store.LatestChain(symbol, expiration) }()
			go func() { defer reads.Done(); quote, quoteFound, quoteErr = s.store.LatestQuote(symbol) }()
			go func() { defer reads.Done(); history, historyErr = s.store.LatestHistory(symbol, 3000) }()
			go func() { defer reads.Done(); extremes, extremesErr = s.store.LatestPayload("price_extremes", symbol) }()
			go func() { defer reads.Done(); beta, betaErr = s.store.LatestPayload("beta_snapshots", symbol) }()
			reads.Wait()
		}
		if profileErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": profileErr.Error()})
			return
		}
		allRows, _ := profile["data"].([]model.OptionRow)
		if chainErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": chainErr.Error()})
			return
		}
		if quoteErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": quoteErr.Error()})
			return
		}
		if chainFound && !containsExpiration(profile["expirations"], expiration) && len(activeExpirations([]string{expiration})) > 0 {
			allRows = append(append([]model.OptionRow(nil), allRows...), chain.Data...)
		}
		if !quoteFound {
			quote = model.Quote{Symbol: symbol, Sessions: map[string]any{}}
		}
		var spotValue, candidateSpot *float64
		if spotNumber, ok := spot.(float64); ok && spotNumber > 0 {
			spotValue = &spotNumber
		} else if quoteFound {
			spotValue = quote.Price
		}
		if historyErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": historyErr.Error()})
			return
		}
		for _, readErr := range []error{extremesErr, betaErr} {
			if readErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"detail": readErr.Error()})
				return
			}
		}
		bars, _ := history["bars"].([]map[string]any)
		latestHistoryDay := historyLatestDate(bars)
		if quoteFound && len(bars) > 0 && historyFresh(history["fetched_at"], s.cfg.HistoryMaxAge) && historyWithinQuoteAlignmentWindow(latestHistoryDay, time.Now()) {
			if previous, ok := positiveHistoryNumber(trendMarket(bars, quote)["previous_close"]); ok {
				quote.PreviousClose = &previous
				if spotValue != nil {
					change := (*spotValue - previous) / previous * 100
					if !math.IsNaN(change) && !math.IsInf(change, 0) {
						quote.ChangePercent = &change
					}
				}
			}
		}
		if spotNumber, ok := spot.(float64); ok && spotNumber > 0 {
			spotValue = &spotNumber
		} else if quoteFound {
			spotValue = quote.Price
		}
		if quote.PreviousClose != nil && *quote.PreviousClose > 0 {
			candidateSpot = quote.PreviousClose
		} else {
			candidateSpot = spotValue
		}
		availableExpirations := make([]string, 0, len(allRows))
		seenExpirations := map[string]bool{}
		for _, row := range allRows {
			if row.Expiration != "" && !seenExpirations[row.Expiration] && len(activeExpirations([]string{row.Expiration})) > 0 {
				seenExpirations[row.Expiration] = true
				availableExpirations = append(availableExpirations, row.Expiration)
			}
		}
		sort.Strings(availableExpirations)
		rawDates := selectRawExpirations(availableExpirations, expiration, rawExpirationCount)
		rawDateSet := make(map[string]bool, len(rawDates)+1)
		for _, value := range rawDates {
			rawDateSet[value] = true
		}
		if len(activeExpirations([]string{expiration})) > 0 {
			rawDateSet[expiration] = true
		}
		rawRows := make([]model.OptionRow, 0)
		for _, row := range allRows {
			if rawDateSet[row.Expiration] {
				rawRows = append(rawRows, row)
			}
		}
		rawExpirations := make([]string, 0, len(rawDateSet))
		for value := range rawDateSet {
			for _, present := range availableExpirations {
				if value == present {
					rawExpirations = append(rawExpirations, value)
					break
				}
			}
		}
		sort.Strings(rawExpirations)
		// The first raw paint often happens before the expiration dropdown is
		// populated. Use the nearest active expiry for IV/touch probability so
		// the cards do not stay at "--" until the next request.
		levelsExpiration := expiration
		if levelsExpiration == "" {
			for _, candidate := range availableExpirations {
				if len(activeExpirations([]string{candidate})) > 0 {
					levelsExpiration = candidate
					break
				}
			}
		}
		ivModel := gamma.Annotate(rawRows, candidateSpot)
		fetchedAt := levelsOptionsTimestamp(profile, chain, expiration)
		extremesValue := extremes["extremes"]
		historySource := "none"
		historyWarning := any("本地没有日线缓存")
		if len(bars) > 0 {
			historySource, historyWarning = "sqlite", nil
		}
		extremesSource := "none"
		extremesWarning := any("本地没有极值缓存")
		if cachedPayloadAvailable(extremesValue) {
			extremesSource, extremesWarning = "sqlite", nil
		}
		betaValue := beta["beta"]
		betaSource := "none"
		betaWarning := any("本地没有 Beta 缓存")
		if cachedPayloadAvailable(betaValue) {
			betaSource, betaWarning = "sqlite", nil
		}
		spotResponse, candidateSpotResponse := finiteFloatPointer(spotValue), finiteFloatPointer(candidateSpot)
		historyMeta := gin.H{"bars": len(bars), "from": barDate(bars, 0), "to": barDate(bars, len(bars)-1), "fetched_at": history["fetched_at"], "source": historySource, "warning": historyWarning,
			"extremes_fetched_at": extremes["fetched_at"], "extremes_source": extremesSource, "extremes_warning": extremesWarning}
		// The raw endpoint is on the first-render critical path.  The inputs needed
		// by the Go levels pipeline are already loaded above, so calculate the
		// lightweight decision payload here instead of making the browser recreate
		// trend, support/resistance, trade points, and buyer structures in JavaScript.
		fastLevels := buildCachedLevelsAt(symbol, levelsExpiration, bars, allRows, quote, spotValue, candidateSpot, history["fetched_at"], extremes, beta, nil, time.Now(), ivModel)
		if fastLevels != nil {
			fastLevels = fastLevelsPayload(fastLevels)
		}
		// Once history and the server-side decision payload are available the
		// browser never reads the raw option rows: it renders fast_levels and
		// keeps the chain table from the snapshot. Skip ~0.5 MiB of row maps on
		// that common path; symbols without history still get the option fallback.
		rawOptions := make([]map[string]any, 0)
		if len(bars) == 0 || fastLevels == nil {
			rawOptions = make([]map[string]any, 0, len(rawRows))
			for _, row := range rawRows {
				item := map[string]any{
					"contract_symbol": row.ContractSymbol, "expiration": row.Expiration,
					"contract_type": row.ContractType, "strike": row.Strike,
					"last_price": row.LastPrice, "bid": row.Bid, "ask": row.Ask,
					"volume": row.Volume, "open_interest": row.OpenInterest,
					"implied_volatility": row.ImpliedVolatility, "gamma": row.Gamma,
				}
				if candidateSpot != nil && *candidateSpot > 0 {
					item["model_iv"] = row.ModelIV
					item["model_gamma"] = row.ModelGamma
				}
				if details, ok := ivModel[row.Expiration].(map[string]any); ok {
					if source, _ := details["source"].(string); source != "" {
						item["model_iv_source"] = source
					}
				}
				rawOptions = append(rawOptions, item)
			}
		}
		c.JSON(http.StatusOK, gin.H{"raw": true, "symbol": symbol, "expiration": expiration,
			"spot": spotResponse, "candidate_spot": candidateSpotResponse, "chain_fetched_at": fetchedAt, "options_fetched_at": fetchedAt,
			"options_expirations": rawExpirations, "options_window_expirations": availableExpirations,
			"options_horizon_days": 45, "options": rawOptions, "iv_model": ivModel, "generated_at": time.Now().UTC(),
			"bars": bars, "history": historyMeta,
			"extremes": extremesValue, "beta": betaValue, "beta_meta": gin.H{"fetched_at": beta["fetched_at"], "source": betaSource, "warning": betaWarning},
			"trend_market": trendMarket(bars, quote), "fast_levels": fastLevels,
		})
		return
	}
	var profile map[string]any
	var profileErr error
	var chain model.ChainResponse
	var chainFound bool
	var chainErr error
	var quote model.Quote
	var quoteFound bool
	var quoteErr error
	var history map[string]any
	var historyErr error
	// The full levels endpoint is requested after the raw endpoint has already
	// rendered the cached option data. Never put fresh Yahoo history or benchmark
	// calls on this request path: stale caches used to hold the page
	// for 13–20 seconds while the first snapshot was already visible. Read only
	// SQLite here and let the symbol analytics prefetch refresh missing inputs.
	var readStarted = time.Now()
	var inputs sync.WaitGroup
	inputs.Add(4)
	go func() { defer inputs.Done(); profile, profileErr = s.latestChains(symbol, 45) }()
	go func() { defer inputs.Done(); chain, chainFound, chainErr = s.store.LatestChain(symbol, expiration) }()
	go func() { defer inputs.Done(); quote, quoteFound, quoteErr = s.store.LatestQuote(symbol) }()
	go func() {
		defer inputs.Done()
		history, historyErr = s.store.LatestHistory(symbol, 3000)
	}()
	inputs.Wait()
	inputReadDuration := time.Since(readStarted)
	if profileErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": profileErr.Error()})
		return
	}
	rows, _ := profile["data"].([]model.OptionRow)
	if chainErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": chainErr.Error()})
		return
	}
	if chainFound && !containsExpiration(profile["expirations"], expiration) && len(activeExpirations([]string{expiration})) > 0 {
		rows = append(append([]model.OptionRow(nil), rows...), chain.Data...)
	}
	if quoteErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": quoteErr.Error()})
		return
	}
	if !quoteFound {
		quote = model.Quote{Symbol: symbol, Sessions: map[string]any{}}
	}
	if historyErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": historyErr.Error()})
		return
	}
	bars, _ := history["bars"].([]map[string]any)
	historySource := cacheSource(history["bars"], history["fetched_at"])
	historyWarning := any(nil)
	if len(bars) == 0 {
		historyWarning = "历史刷新正在进行，日线后台获取中"
	}
	history["source"], history["warning"] = historySource, historyWarning
	var extremes, beta map[string]any
	var extremesErr, betaErr error
	var cachedInputs sync.WaitGroup
	cachedInputs.Add(2)
	go func() {
		defer cachedInputs.Done()
		extremes, extremesErr = s.store.LatestPayload("price_extremes", symbol)
	}()
	go func() { defer cachedInputs.Done(); beta, betaErr = s.store.LatestPayload("beta_snapshots", symbol) }()
	cachedInputs.Wait()
	if extremesErr != nil || betaErr != nil {
		if extremesErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": extremesErr.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": betaErr.Error()})
		}
		return
	}
	if extremes == nil {
		extremes = map[string]any{}
	}
	if !cachedPayloadAvailable(extremes["extremes"]) && len(bars) > 0 {
		if calculated := calculatePriceExtremes(bars); calculated != nil {
			extremes = map[string]any{"extremes": calculated, "fetched_at": history["fetched_at"], "source": "sqlite", "warning": nil}
		}
	}
	if beta == nil {
		beta = map[string]any{}
	}
	if cachedPayloadAvailable(extremes["extremes"]) {
		extremes["source"], extremes["warning"] = "sqlite", nil
	} else {
		extremes["source"] = "none"
		extremes["warning"] = "本地没有极值缓存"
	}
	if cachedPayloadAvailable(extremes["extremes"]) && extremes["fetched_at"] != nil {
		extremes["source"], extremes["warning"] = "sqlite", nil
	}
	if cachedPayloadAvailable(beta["beta"]) {
		beta["source"], beta["warning"] = "sqlite", nil
	} else {
		beta["source"] = "none"
		beta["warning"] = "本地没有 Beta 缓存"
	}
	if !cachedPayloadAvailable(extremes["extremes"]) && len(bars) > 0 {
		if calculated := calculatePriceExtremes(bars); calculated != nil {
			extremes = map[string]any{"extremes": calculated, "fetched_at": history["fetched_at"]}
		}
	}
	if !cachedPayloadAvailable(beta["beta"]) {
		beta["warning"] = "历史刷新正在进行，Beta 后台计算中"
	}
	if !cachedPayloadAvailable(extremes["extremes"]) {
		extremes["warning"] = "历史刷新正在进行，历史极值后台计算中"
	}
	s.prefetchSymbolAnalytics(symbol, false)
	var spotValue, candidateSpot *float64
	if spotNumber, ok := spot.(float64); ok && spotNumber > 0 {
		spotValue = &spotNumber
	} else if quoteFound {
		spotValue = quote.Price
	}
	if quote.PreviousClose != nil && *quote.PreviousClose > 0 {
		candidateSpot = quote.PreviousClose
	} else {
		candidateSpot = spotValue
	}
	latestHistoryDay := historyLatestDate(bars)
	if aligned := trendMarket(bars, quote); len(bars) > 0 && historyFresh(history["fetched_at"], s.cfg.HistoryMaxAge) && historyWithinQuoteAlignmentWindow(latestHistoryDay, time.Now()) {
		if previous, ok := positiveHistoryNumber(aligned["previous_close"]); ok {
			candidateSpot = &previous
			quote.PreviousClose = &previous
			if spotValue != nil {
				change := (*spotValue - previous) / previous * 100
				if !math.IsNaN(change) && !math.IsInf(change, 0) {
					quote.ChangePercent = &change
				}
			}
		}
	}
	cacheKey := levelsAnalysisCacheKey(symbol, expiration, bars, rows, quote, spotValue, candidateSpot, history["fetched_at"], profile["fetched_at"], extremes, beta, nil)
	cacheEntries := s.cfg.AnalysisCacheEntries
	if cacheEntries <= 0 {
		cacheEntries = 128
	}
	cacheTimings := &levelsCacheTimings{}
	computed, cacheErr := s.getOrBuildLevelsTimed(c.Request.Context(), cacheKey, cacheEntries, func() map[string]any {
		return buildCachedLevels(symbol, expiration, bars, rows, quote, spotValue, candidateSpot, history["fetched_at"], extremes, beta, nil)
	}, cacheTimings)
	if elapsed := time.Since(requestStartedAt); elapsed >= time.Second {
		log.Printf("slow levels stages symbol=%s input_reads=%s analysis_cache_read=%s cache_write=%s flight_wait=%s compute=%s bars=%d rows=%d history=%s profile=%s",
			symbol, inputReadDuration.Round(time.Millisecond), cacheTimings.cacheRead.Round(time.Millisecond), cacheTimings.cacheWrite.Round(time.Millisecond), cacheTimings.flightWait.Round(time.Millisecond), cacheTimings.build.Round(time.Millisecond), len(bars), len(rows), cacheSource(history["bars"], history["fetched_at"]), cacheSource(profile["data"], profile["fetched_at"]))
	}
	if cacheErr != nil {
		return
	}
	if computed != nil {
		// The durable payload is reusable computation, not a response snapshot;
		// refresh request-local metadata and avoid returning a stale generated_at.
		computed["generated_at"] = time.Now().UTC()
		if metadata, ok := computed["history"].(map[string]any); ok {
			metadata["source"], metadata["warning"] = history["source"], history["warning"]
			metadata["extremes_fetched_at"], metadata["extremes_source"], metadata["extremes_warning"] = extremes["fetched_at"], extremes["source"], extremes["warning"]
		}
		computed["beta"], computed["beta_meta"] = beta["beta"], map[string]any{"fetched_at": beta["fetched_at"], "source": beta["source"], "warning": beta["warning"]}
		computed["extremes"] = extremes["extremes"]
		latestFetchedAt := levelsOptionsTimestamp(profile, chain, expiration)
		computed["chain_fetched_at"] = latestFetchedAt
		computed["options_fetched_at"] = latestFetchedAt
		c.JSON(http.StatusOK, computed)
		return
	}
	historyFrom, historyTo := cachedBarDates(bars)
	historyMeta := gin.H{
		"bars": len(bars), "from": historyFrom, "to": historyTo,
		"fetched_at": history["fetched_at"], "source": history["source"], "warning": history["warning"],
		"extremes_fetched_at": extremes["fetched_at"], "extremes_source": extremes["source"], "extremes_warning": extremes["warning"],
	}
	betaValue := beta["beta"]
	latestFetchedAt := levelsOptionsTimestamp(profile, chain, expiration)
	c.JSON(http.StatusOK, gin.H{
		"symbol": symbol, "expiration": expiration, "spot": nil,
		"generated_at": time.Now().UTC(), "chain_fetched_at": latestFetchedAt, "options_fetched_at": latestFetchedAt,
		"options_expirations": cachedExpirations(rows), "options_horizon_days": 45, "history_bars": len(bars),
		"history": historyMeta, "trend_market": trendMarket(bars, quote), "beta": betaValue,
		"beta_meta":      gin.H{"fetched_at": beta["fetched_at"], "source": beta["source"], "warning": beta["warning"]},
		"extremes":       extremes["extremes"],
		"options_metric": "none", "resistance": []any{}, "support": []any{},
		"trend": nil, "recommendation": gin.H{"action": "hold", "label": "继续持有", "reason": "缺少有效价格数据"},
		"trade_points": gin.H{"buy": nil, "sell": nil}, "stop_loss": nil,
		"trade_points_horizon": gin.H{"trading_days": 5, "label": "未来 5 个交易日"},
		"buyer_structures":     gin.H{"available": false, "status": "unavailable", "items": []any{}, "horizon_trading_days": 5, "horizon_label": "未来 5 个交易日", "reason": "缺少有效价格数据"},
		"plan":                 gin.H{"buy": []any{}, "add": []any{}, "sell": []any{}},
	})
}

func gammaAnalysisCacheKey(symbol string, horizon int, includeRows bool, fetchedAt, oiFallback any, rows []model.OptionRow, spot *float64) string {
	input := struct {
		Version     string            `json:"version"`
		Symbol      string            `json:"symbol"`
		Horizon     int               `json:"horizon_days"`
		IncludeRows bool              `json:"include_rows"`
		FetchedAt   any               `json:"fetched_at"`
		OIFallback  any               `json:"oi_fallback"`
		Rows        []model.OptionRow `json:"rows"`
		Spot        *float64          `json:"spot"`
	}{
		Version: "gamma-v1", Symbol: symbol, Horizon: horizon, IncludeRows: includeRows,
		FetchedAt: fetchedAt, OIFallback: oiFallback, Rows: rows, Spot: spot,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%s|%d|%t", symbol, horizon, includeRows))
	}
	digest := sha256.Sum256(encoded)
	return "gamma-v1:" + hex.EncodeToString(digest[:])
}

// Bump when level classification changes so durable SQLite analysis results
// are rebuilt instead of serving tiers produced by the previous rule set.
const levelsAnalysisCacheVersion = "levels-v25"

func levelsAnalysisCacheKey(symbol, expiration string, bars []map[string]any, rows []model.OptionRow, quote model.Quote, spot, candidateSpot *float64, historyFetchedAt, profileFetchedAt any, extremes, beta, _ map[string]any) string {
	input := struct {
		Version          string            `json:"version"`
		Symbol           string            `json:"symbol"`
		Expiration       string            `json:"expiration"`
		Bars             []map[string]any  `json:"bars"`
		Rows             []model.OptionRow `json:"rows"`
		Quote            model.Quote       `json:"quote"`
		Spot             *float64          `json:"spot"`
		CandidateSpot    *float64          `json:"candidate_spot"`
		HistoryFetchedAt any               `json:"history_fetched_at"`
		ProfileFetchedAt any               `json:"profile_fetched_at"`
		Extremes         map[string]any    `json:"extremes"`
		Beta             map[string]any    `json:"beta"`
	}{
		Version: levelsAnalysisCacheVersion, Symbol: symbol, Expiration: expiration, Bars: bars, Rows: rows,
		Quote: quote, Spot: spot, CandidateSpot: candidateSpot, HistoryFetchedAt: historyFetchedAt,
		ProfileFetchedAt: profileFetchedAt, Extremes: extremes, Beta: beta,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		encoded = []byte(symbol + "|" + expiration)
	}
	digest := sha256.Sum256(encoded)
	return levelsAnalysisCacheVersion + ":" + hex.EncodeToString(digest[:])
}

func levelsOptionsTimestamp(profile map[string]any, selected model.ChainResponse, expiration string) any {
	// Match Python's selected_chain path: only an active selected expiration
	// outside the profile contributes a second committed SQLite batch timestamp.
	fetchedAt := profile["fetched_at"]
	if selected.FetchedAt != nil && !containsExpiration(profile["expirations"], expiration) && len(activeExpirations([]string{expiration})) > 0 {
		fetchedAt = laterTimestamp(selected.FetchedAt, fetchedAt)
	}
	return fetchedAt
}

func laterTimestamp(candidate *time.Time, other any) any {
	var otherTime *time.Time
	switch value := other.(type) {
	case time.Time:
		otherTime = &value
	case *time.Time:
		otherTime = value
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			otherTime = &parsed
		}
	}
	if candidate == nil {
		return otherTime
	}
	if otherTime == nil || candidate.After(*otherTime) {
		return candidate
	}
	return otherTime
}

func maxIntValue(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func selectRawExpirations(expirations []string, selected string, limit int) []string {
	if limit <= 0 {
		limit = 4
	}
	index := sort.SearchStrings(expirations, selected)
	if index >= len(expirations) || expirations[index] != selected {
		index = 0
	}
	start := maxIntValue(0, minIntValue(index, len(expirations)-limit))
	end := minIntValue(len(expirations), start+limit)
	return append([]string(nil), expirations[start:end]...)
}

func containsExpiration(value any, expiration string) bool {
	values, _ := value.([]string)
	for _, item := range values {
		if item == expiration {
			return true
		}
	}
	return false
}

func hasTwoSidedQuotes(rows []model.OptionRow) bool {
	for _, row := range rows {
		if row.Bid != nil && row.Ask != nil && *row.Bid > 0 && *row.Ask >= *row.Bid {
			return true
		}
	}
	return false
}

func minIntValue(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func barDate(bars []map[string]any, index int) any {
	if index < 0 || index >= len(bars) {
		return nil
	}
	return bars[index]["date"]
}

func trendMarket(bars []map[string]any, quote model.Quote) map[string]any {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	return trendMarketAt(bars, quote, time.Now().In(location).Format("2006-01-02"))
}

func marketSessionState(moment time.Time) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	local := moment.In(location)
	minute := local.Hour()*60 + local.Minute()
	day := localDate(local)
	if marketcalendar.IsTradingDay(day) {
		closeMinute := marketcalendar.RegularCloseMinute(day)
		if minute >= 9*60+30 && minute < closeMinute {
			return "REGULAR"
		}
		if minute >= 4*60 && minute < 9*60+30 {
			return "PRE"
		}
		if minute >= closeMinute && minute < 20*60 {
			return "POST"
		}
	}
	if minute >= 20*60 && marketcalendar.IsTradingDay(day.AddDate(0, 0, 1)) {
		return "OVERNIGHT"
	}
	if minute < 4*60 && marketcalendar.IsTradingDay(day) {
		return "OVERNIGHT"
	}
	return "CLOSED"
}

func trendMarketAt(bars []map[string]any, quote model.Quote, today string) map[string]any {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	marketState := strings.ToUpper(strings.TrimSpace(quote.MarketState))
	if quote.FetchedAt != nil {
		capturedAt := quote.FetchedAt.In(location)
		capturedDate := capturedAt.Format(time.DateOnly)
		if quote.Source == "sqlite" {
			// SQLite can hold an older quote beside newer daily bars. Align stale
			// snapshots to the current exchange session so the previous-session
			// fields come from the most recently completed daily bar.
			current := time.Now().In(location)
			currentDate := current.Format(time.DateOnly)
			currentState := marketSessionState(current)
			if marketState == "" {
				marketState = marketSessionState(capturedAt)
			}
			staleMarketQuote := capturedDate != currentDate
			if current.Sub(capturedAt) > 15*time.Minute && marketState != currentState {
				staleMarketQuote = true
			}
			if staleMarketQuote {
				today, marketState = currentDate, currentState
			} else {
				today = capturedDate
			}
		} else {
			today = capturedDate
		}
	}
	var marketStateValue any
	if marketState != "" {
		marketStateValue = marketState
	}
	result := map[string]any{"today_open": nil, "previous_open": nil, "previous_close": quote.PreviousClose,
		"today_open_date": nil, "previous_open_date": nil, "previous_close_date": nil, "market_state": marketStateValue}
	ordered := make([]map[string]any, 0, len(bars))
	for _, bar := range bars {
		if bar == nil || !validTrendBarDate(trendBarDate(bar)) {
			continue
		}
		open, openOK := trendBarValue(bar["open"])
		close, closeOK := trendBarValue(bar["close"])
		if !openOK || !closeOK {
			continue
		}
		ordered = append(ordered, map[string]any{"date": trendBarDate(bar), "open": open, "close": close})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return trendBarDate(ordered[i]) < trendBarDate(ordered[j])
	})
	if len(ordered) > 0 && trendBarDate(ordered[len(ordered)-1]) > today {
		aligned := ordered[:0]
		for _, bar := range ordered {
			if trendBarDate(bar) <= today {
				aligned = append(aligned, bar)
			}
		}
		ordered = aligned
	}
	// A daily bar for the current calendar date may already exist in SQLite
	// from a later fetch even when this quote snapshot was captured premarket.
	// It is not a completed session yet, so exclude it when finding the last
	// completed day's open and close.
	if marketState == "PRE" && len(ordered) > 0 && trendBarDate(ordered[len(ordered)-1]) == today {
		ordered = ordered[:len(ordered)-1]
	}
	var latest, previous map[string]any
	if len(ordered) > 0 {
		latest = ordered[len(ordered)-1]
		if len(ordered) > 1 {
			previous = ordered[len(ordered)-2]
		}
		result["today_open"], result["today_open_date"] = latest["open"], latest["date"]
		result["previous_close"], result["previous_close_date"] = latest["close"], latest["date"]
		result["previous_open"], result["previous_open_date"] = latest["open"], latest["date"]
	}

	latestDate := trendBarDate(latest)
	latestIsToday := latestDate == today && (marketState == "REGULAR" || marketState == "CLOSED")
	if latestIsToday {
		if previous != nil {
			result["previous_close"], result["previous_close_date"] = previous["close"], previous["date"]
			result["previous_open"], result["previous_open_date"] = previous["open"], previous["date"]
		} else {
			result["previous_close"], result["previous_close_date"] = quote.PreviousClose, nil
			result["previous_open"], result["previous_open_date"] = nil, nil
		}
	}
	// When the exchange is closed for a weekend or holiday, show the session
	// before the most recent completed session in the "previous session" fields.
	// If history is already missing that latest session, keep its actual latest
	// bar instead of skipping a second bar.
	if marketState == "CLOSED" && previous != nil {
		if currentDate, err := time.ParseInLocation(time.DateOnly, today, location); err == nil {
			latestCompletedSession := currentDate
			for searched := 0; searched < 14 && !marketcalendar.IsTradingDay(latestCompletedSession); searched++ {
				latestCompletedSession = latestCompletedSession.AddDate(0, 0, -1)
			}
			if marketcalendar.IsTradingDay(latestCompletedSession) && latestDate == latestCompletedSession.Format(time.DateOnly) {
				result["previous_close"], result["previous_close_date"] = previous["close"], previous["date"]
				result["previous_open"], result["previous_open_date"] = previous["open"], previous["date"]
			}
		}
	}
	// Extended-session summaries can be newer than daily bars and provide the
	// authoritative regular-session close when the cached daily history lags.
	if marketState != "REGULAR" && marketState != "CLOSED" {
		var summary map[string]any
		if marketState == "PRE" {
			summary = nestedMap(quote.Sessions, "pre")
		} else {
			summary = nestedMap(quote.Sessions, "post")
			if summary == nil {
				summary = nestedMap(quote.Sessions, "overnight")
			}
		}
		asOf := stringValue(summary["as_of"])
		if len(asOf) > 10 {
			asOf = asOf[:10]
		}
		referenceClose := positiveNumeric(summary["reference_close"])
		if asOf != "" && (latestDate == "" || latestDate < asOf) && referenceClose != nil {
			result["previous_close"], result["previous_close_date"] = *referenceClose, asOf
		}
	}

	quoteOpen := positiveNumeric(quote.TodayOpen)
	if quoteOpen != nil && (marketState == "REGULAR" || marketState == "POST" || marketState == "OVERNIGHT") {
		previousOpen := numericValue(nil)
		if previous != nil {
			previousOpen = numericValue(previous["open"])
		}
		latestOpen := numericValue(nil)
		if latest != nil {
			latestOpen = numericValue(latest["open"])
		}
		dailyMissingToday := latestDate != today
		dailyOpenCopied := false
		if !dailyMissingToday && latestOpen != nil && previousOpen != nil {
			tolerance := math.Max(0.01, math.Abs(*previousOpen)*1e-4)
			dailyOpenCopied = math.Abs(*latestOpen-*previousOpen) <= tolerance
		}
		if dailyMissingToday || dailyOpenCopied {
			result["today_open"], result["today_open_date"] = *quoteOpen, today
		}
	}
	if result["today_open"] == nil {
		result["today_open"] = quote.TodayOpen
	}
	if result["previous_close"] == nil {
		result["previous_close"] = quote.PreviousClose
	}
	return result
}

func trendBarDate(bar map[string]any) string {
	if bar == nil {
		return ""
	}
	value := stringValue(bar["date"])
	if len(value) > 10 {
		return value[:10]
	}
	return value
}

func validTrendBarDate(value string) bool {
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func trendBarValue(value any) (any, bool) {
	if value == nil {
		return nil, true
	}
	if text, ok := value.(string); ok {
		number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, false
		}
		return number, true
	}
	number := numberValue(value)
	if number == nil {
		return nil, false
	}
	return *number, true
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func nestedMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	value, _ := parent[key].(map[string]any)
	return value
}

func numericValue(value any) *float64 {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

func positiveNumeric(value any) *float64 {
	if pointer, ok := value.(*float64); ok {
		if pointer == nil || math.IsNaN(*pointer) || math.IsInf(*pointer, 0) || *pointer <= 0 {
			return nil
		}
		return pointer
	}
	number := numericValue(value)
	if number == nil || *number <= 0 {
		return nil
	}
	return number
}

func activeExpirations(values []string) []string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format("2006-01-02")
	active := make([]string, 0, len(values))
	for _, value := range values {
		if value >= today {
			active = append(active, value)
		}
	}
	// ISO dates sort chronologically, so every consumer starts with the nearest
	// active expiry even when an upstream catalog changes its response order.
	sort.Strings(active)
	return active
}

func quoteTime(quote model.Quote) time.Time {
	if quote.FetchedAt != nil {
		return quote.FetchedAt.UTC()
	}
	return time.Time{}
}

func quoteTimeFromChain(chain model.ChainResponse) time.Time {
	if chain.FetchedAt != nil {
		return chain.FetchedAt.UTC()
	}
	return time.Time{}
}

func parseCursor(raw string) int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if value < 0 {
		return 0
	}
	return value
}

// upcomingNYSETradingSessions is kept for calendar utility tests and market
// date calculations; earnings dates no longer consume this window.
func upcomingNYSETradingSessions(day time.Time, count int) []time.Time {
	if count <= 0 {
		return nil
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	current := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
	sessions := make([]time.Time, 0, count)
	for searched := 0; len(sessions) < count && searched < count*4+14; searched++ {
		if isNYSESession(current) {
			sessions = append(sessions, current)
		}
		current = current.AddDate(0, 0, 1)
	}
	return sessions
}
