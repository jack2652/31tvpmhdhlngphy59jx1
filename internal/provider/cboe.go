package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"option-scope-go/internal/marketcalendar"
	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"
)

const (
	defaultCboeOptionsURL = "https://cdn.cboe.com/api/global/delayed_quotes/options/{symbol}.json"
	defaultCboeCacheTTL   = 15 * time.Second
)

var (
	occContractPattern = regexp.MustCompile(`^(?P<root>.+?)(?P<date>[0-9]{6})(?P<type>[CP])(?P<strike>[0-9]{8})$`)
	cboeSymbolPattern  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,9}$`)
)

// CboeOptionDTO represents one delayed quote row returned by Cboe's public
// options endpoint. Values remain raw JSON because the endpoint can encode
// numeric fields as either JSON numbers or strings.
type CboeOptionDTO struct {
	Option         string          `json:"option"`
	LastTradePrice json.RawMessage `json:"last_trade_price"`
	Bid            json.RawMessage `json:"bid"`
	Ask            json.RawMessage `json:"ask"`
	Volume         json.RawMessage `json:"volume"`
	OpenInterest   json.RawMessage `json:"open_interest"`
	IV             json.RawMessage `json:"iv"`
	Gamma          json.RawMessage `json:"gamma"`
	PercentChange  json.RawMessage `json:"percent_change"`
}

type CboeOptionsPayloadDTO struct {
	Data *struct {
		CurrentPrice  json.RawMessage   `json:"current_price"`
		PreviousClose json.RawMessage   `json:"prev_day_close"`
		ChangePercent json.RawMessage   `json:"price_change_percent"`
		Options       []json.RawMessage `json:"options"`
	} `json:"data"`
}

// OCCOption is the normalized subset encoded in the trailing 15 characters of
// an OCC option identifier: YYMMDD + C/P + strike in thousandths.
type OCCOption struct {
	Root         string
	Expiration   string
	ContractType string
	Strike       float64
}

// ParseOCCOption parses an OCC contract code. Root padding is preserved for
// symbol matching, while the root itself is uppercased.
func ParseOCCOption(raw string) (OCCOption, error) {
	contract := strings.ToUpper(strings.TrimSpace(raw))
	matches := occContractPattern.FindStringSubmatch(contract)
	if len(matches) != 5 {
		return OCCOption{}, fmt.Errorf("invalid OCC option contract %q", raw)
	}
	// Preserve the OCC root's internal/trailing padding: Python's Cboe
	// adapter only removes periods while matching roots, so padded roots do
	// not silently collapse into a different underlying symbol.
	root := matches[1]
	if strings.TrimSpace(root) == "" {
		return OCCOption{}, fmt.Errorf("invalid OCC option contract %q: missing root", raw)
	}
	dateCode := matches[2]
	expiry, err := time.Parse("20060102", "20"+dateCode)
	if err != nil {
		return OCCOption{}, fmt.Errorf("invalid OCC option contract %q: invalid expiry", raw)
	}
	strikeUnits, err := strconv.ParseInt(matches[4], 10, 64)
	if err != nil {
		return OCCOption{}, fmt.Errorf("invalid OCC option contract %q: invalid strike", raw)
	}
	contractType := "call"
	if matches[3] == "P" {
		contractType = "put"
	}
	return OCCOption{
		Root: root, Expiration: expiry.Format("2006-01-02"), ContractType: contractType,
		Strike: float64(strikeUnits) / 1000,
	}, nil
}

// CboeOptionsClient reads Cboe delayed option snapshots. Endpoint supports a
// {symbol} token (or a single %s) so tests can target httptest servers.
type CboeOptionsClient struct {
	Client     *http.Client
	Endpoint   string
	CacheTTL   time.Duration
	CacheLimit int
	Gate       chan struct{}
	// InteractiveGate reserves one upstream slot for the delayed price fallback
	// (Quote) so a busy shared gate cannot starve the current-price path. Chain
	// and Expirations keep using Gate.
	InteractiveGate chan struct{}
	GateWait        time.Duration
	mu              sync.Mutex
	cache           map[string]cboeCacheEntry
	inFlight        map[string]*cboeFlight
	clockNow        func() time.Time
}

type cboeCacheEntry struct {
	payload CboeOptionsPayloadDTO
	at      time.Time
}

type cboeFlight struct {
	done           chan struct{}
	payload        CboeOptionsPayloadDTO
	err            error
	leaderCanceled bool
}

func NewCboeOptionsClient(client *http.Client, endpoint string) *CboeOptionsClient {
	if client == nil {
		client = outboundhttp.DefaultBrowserClient(30 * time.Second)
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultCboeOptionsURL
	}
	return &CboeOptionsClient{Client: client, Endpoint: endpoint, CacheTTL: defaultCboeCacheTTL, CacheLimit: 16, cache: make(map[string]cboeCacheEntry), inFlight: make(map[string]*cboeFlight), clockNow: time.Now}
}

func (c *CboeOptionsClient) fetch(ctx context.Context, rawSymbol string) (string, CboeOptionsPayloadDTO, error) {
	return c.fetchWithGate(ctx, rawSymbol, c.Gate)
}

// fetchWithGate runs the shared Cboe request under the provided upstream gate.
// Quote passes the interactive gate so the delayed price fallback keeps a
// reserved slot; Expirations and Chain keep the shared gate.
func (c *CboeOptionsClient) fetchWithGate(ctx context.Context, rawSymbol string, gate chan struct{}) (string, CboeOptionsPayloadDTO, error) {
	symbol, err := normalizeCboeSymbol(rawSymbol)
	if err != nil {
		return "", CboeOptionsPayloadDTO{}, err
	}
	if c == nil || c.Client == nil {
		return "", CboeOptionsPayloadDTO{}, errors.New("Cboe options client is nil")
	}
	if payload, ok := c.cachedPayload(symbol); ok {
		return symbol, payload, nil
	}
	return c.fetchSingleFlight(ctx, symbol, gate)
}

// priceGate returns the reserved upstream slot when configured, otherwise the
// shared gate.
func (c *CboeOptionsClient) priceGate() chan struct{} {
	if c != nil && c.InteractiveGate != nil {
		return c.InteractiveGate
	}
	return c.Gate
}

func (c *CboeOptionsClient) cachedPayload(symbol string) (CboeOptionsPayloadDTO, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[symbol]
	if c.CacheTTL <= 0 || !ok || c.clockNow().Sub(entry.at) >= c.CacheTTL {
		delete(c.cache, symbol)
		return CboeOptionsPayloadDTO{}, false
	}
	return entry.payload, true
}

func (c *CboeOptionsClient) fetchSingleFlight(ctx context.Context, symbol string, gate chan struct{}) (string, CboeOptionsPayloadDTO, error) {
	retriedCanceledFlight := false
	for {
		c.mu.Lock()
		if entry, ok := c.cache[symbol]; c.CacheTTL > 0 && ok && c.clockNow().Sub(entry.at) < c.CacheTTL {
			c.mu.Unlock()
			return symbol, entry.payload, nil
		}
		if flight := c.inFlight[symbol]; flight != nil {
			c.mu.Unlock()
			select {
			case <-flight.done:
				if err := ctx.Err(); err != nil {
					return "", CboeOptionsPayloadDTO{}, err
				}
				if flight.leaderCanceled && !retriedCanceledFlight {
					retriedCanceledFlight = true
					continue
				}
				return symbol, flight.payload, flight.err
			case <-ctx.Done():
				return "", CboeOptionsPayloadDTO{}, ctx.Err()
			}
		}
		flight := &cboeFlight{done: make(chan struct{})}
		c.inFlight[symbol] = flight
		c.mu.Unlock()

		payload, err := c.fetchUncached(ctx, symbol, gate)
		leaderCanceled := ctx.Err() != nil && err != nil
		c.mu.Lock()
		flight.payload, flight.err, flight.leaderCanceled = payload, err, leaderCanceled
		if err == nil && c.CacheTTL > 0 {
			limit := c.CacheLimit
			if limit > 0 {
				for len(c.cache) >= limit {
					oldestSymbol := ""
					var oldest time.Time
					for cachedSymbol, entry := range c.cache {
						if oldestSymbol == "" || entry.at.Before(oldest) {
							oldestSymbol, oldest = cachedSymbol, entry.at
						}
					}
					delete(c.cache, oldestSymbol)
				}
			}
			c.cache[symbol] = cboeCacheEntry{payload: payload, at: c.clockNow()}
		}
		delete(c.inFlight, symbol)
		close(flight.done)
		c.mu.Unlock()
		return symbol, payload, err
	}
}

func (c *CboeOptionsClient) fetchUncached(ctx context.Context, symbol string, gate chan struct{}) (CboeOptionsPayloadDTO, error) {
	release, err := acquireGateTimeout(ctx, gate, c.GateWait)
	if err != nil {
		return CboeOptionsPayloadDTO{}, err
	}
	defer release()
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		endpoint = defaultCboeOptionsURL
	}
	encoded := url.PathEscape(symbol)
	if strings.Contains(endpoint, "{symbol}") {
		endpoint = strings.ReplaceAll(endpoint, "{symbol}", encoded)
	} else if strings.Contains(endpoint, "%s") {
		endpoint = fmt.Sprintf(endpoint, encoded)
	}
	request, err := outboundhttp.NewRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return CboeOptionsPayloadDTO{}, fmt.Errorf("build Cboe request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "option-scope/1.0")
	response, err := outboundhttp.Do(c.Client, request)
	if err != nil {
		return CboeOptionsPayloadDTO{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2000))
		detail := strings.ToValidUTF8(string(body), "\uFFFD")
		if runes := []rune(detail); len(runes) > 500 {
			detail = string(runes[:500])
		}
		return CboeOptionsPayloadDTO{}, fmt.Errorf("Cboe 请求失败（HTTP %d）：%s", response.StatusCode, detail)
	}
	var payload CboeOptionsPayloadDTO
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return CboeOptionsPayloadDTO{}, fmt.Errorf("read Cboe options response: %w", err)
	}
	if !utf8.Valid(body) {
		return CboeOptionsPayloadDTO{}, errors.New("decode Cboe options response: invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return CboeOptionsPayloadDTO{}, fmt.Errorf("decode Cboe options response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return CboeOptionsPayloadDTO{}, errors.New("decode Cboe options response: multiple JSON values")
		}
		return CboeOptionsPayloadDTO{}, fmt.Errorf("decode Cboe options response: %w", err)
	}
	if payload.Data == nil || payload.Data.Options == nil {
		return CboeOptionsPayloadDTO{}, errors.New("Cboe options response is missing data.options")
	}
	return payload, nil
}

// Quote returns the delayed underlying price embedded in Cboe's option snapshot.
func (c *CboeOptionsClient) Quote(ctx context.Context, rawSymbol string) (model.Quote, error) {
	symbol, payload, err := c.fetchWithGate(ctx, rawSymbol, c.priceGate())
	if err != nil {
		return model.Quote{}, err
	}
	price := cboeNumber(payload.Data.CurrentPrice)
	previous := cboeNumber(payload.Data.PreviousClose)
	change := cboeNumber(payload.Data.ChangePercent)
	if change == nil && price != nil && previous != nil && *previous != 0 {
		value := (*price - *previous) / *previous * 100
		change = &value
	}
	fetched := time.Now().UTC()
	state := cboeMarketState(fetched)
	return model.Quote{
		Symbol: symbol, Price: price, ChangePercent: change, PreviousClose: previous,
		Currency: "USD", MarketState: state, Provider: "cboe-delayed", Source: "upstream",
		FetchedAt: &fetched, Sessions: map[string]any{}, FairValuePending: true,
	}, nil
}

// Expirations returns sorted unique expirations for contracts whose OCC root
// matches the requested underlying. It intentionally does not apply a current-
// date filter; callers can retain historical catalog semantics and filter for
// active expirations at the service layer.
func (c *CboeOptionsClient) Expirations(ctx context.Context, rawSymbol string) ([]string, error) {
	symbol, payload, err := c.fetch(ctx, rawSymbol)
	if err != nil {
		return nil, err
	}
	return cboeExpirationsFromPayload(symbol, payload)
}

// NearestChain returns the expiration catalogue and the nearest active chain
// from one delayed-options payload. The catalogue keeps historical dates, like
// Expirations; the rows belong to the first date at or after today.
func (c *CboeOptionsClient) NearestChain(ctx context.Context, rawSymbol string) ([]string, []model.OptionRow, *time.Time, error) {
	symbol, payload, err := c.fetch(ctx, rawSymbol)
	if err != nil {
		return nil, nil, nil, err
	}
	expirations, err := cboeExpirationsFromPayload(symbol, payload)
	if err != nil {
		return nil, nil, nil, err
	}
	expiration := cboeNearestActiveExpiration(expirations)
	if expiration == "" {
		return expirations, nil, nil, nil
	}
	rows := cboeChainRowsFromPayload(symbol, expiration, payload)
	if len(rows) == 0 {
		return expirations, nil, nil, nil
	}
	fetched := time.Now().UTC()
	return expirations, rows, &fetched, nil
}

func cboeExpirationsFromPayload(symbol string, payload CboeOptionsPayloadDTO) ([]string, error) {
	values := make(map[string]struct{})
	for _, raw := range payload.Data.Options {
		row, ok := decodeCboeOption(raw)
		if !ok {
			continue
		}
		contract, parseErr := ParseOCCOption(row.Option)
		if parseErr != nil || normalizeOCCRoot(contract.Root) != normalizeOCCRoot(symbol) {
			continue
		}
		values[contract.Expiration] = struct{}{}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("Cboe has no valid option expirations for %s", symbol)
	}
	result := make([]string, 0, len(values))
	for expiration := range values {
		result = append(result, expiration)
	}
	sort.Strings(result)
	return result, nil
}

func cboeNearestActiveExpiration(expirations []string) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format(time.DateOnly)
	for _, expiration := range expirations {
		if expiration >= today {
			return expiration
		}
	}
	return ""
}

// Chain maps the selected expiration to the shared OptionRow model. Cboe
// values are delayed snapshots and the returned timestamp marks local fetch
// time, not an exchange-provided quote timestamp.
func (c *CboeOptionsClient) Chain(ctx context.Context, rawSymbol, rawExpiration string) ([]model.OptionRow, *time.Time, error) {
	symbol, err := normalizeCboeSymbol(rawSymbol)
	if err != nil {
		return nil, nil, err
	}
	expiration, err := NormalizeExpiration(rawExpiration)
	if err != nil || expiration != rawExpiration {
		return nil, nil, errors.New("expiration must use YYYY-MM-DD")
	}
	_, payload, err := c.fetch(ctx, symbol)
	if err != nil {
		return nil, nil, err
	}
	rows := cboeChainRowsFromPayload(symbol, expiration, payload)
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("Cboe has no option data for %s %s", symbol, expiration)
	}
	fetched := time.Now().UTC()
	return rows, &fetched, nil
}

func cboeChainRowsFromPayload(symbol, expiration string, payload CboeOptionsPayloadDTO) []model.OptionRow {
	spot := cboeNumber(payload.Data.CurrentPrice)
	rows := make([]model.OptionRow, 0)
	for _, raw := range payload.Data.Options {
		row, ok := decodeCboeOption(raw)
		if !ok {
			continue
		}
		contract, parseErr := ParseOCCOption(row.Option)
		if parseErr != nil || contract.Expiration != expiration || normalizeOCCRoot(contract.Root) != normalizeOCCRoot(symbol) {
			continue
		}
		itm := false
		itmKnown := spot != nil
		if spot != nil {
			if contract.ContractType == "call" {
				itm = *spot >= contract.Strike
			} else {
				itm = *spot <= contract.Strike
			}
		}
		rows = append(rows, model.OptionRow{
			ContractSymbol: strings.ToUpper(strings.TrimSpace(row.Option)), Provider: "cboe-delayed",
			ContractType: contract.ContractType, Strike: floatPtr(contract.Strike),
			LastPrice: cboeNumber(row.LastTradePrice), Bid: cboeNumber(row.Bid), Ask: cboeNumber(row.Ask),
			Volume: cboeInteger(row.Volume), OpenInterest: cboeInteger(row.OpenInterest),
			ImpliedVolatility: cboeNumber(row.IV), Gamma: cboeNumber(row.Gamma),
			ChangePercent: cboeNumber(row.PercentChange), InTheMoney: itm, InTheMoneyKnown: itmKnown, Expiration: expiration,
		})
	}
	return rows
}

func decodeCboeOption(raw json.RawMessage) (CboeOptionDTO, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return CboeOptionDTO{}, false
	}
	var row CboeOptionDTO
	if err := json.Unmarshal(raw, &row); err != nil {
		return CboeOptionDTO{}, false
	}
	return row, true
}

func cboeMarketState(moment time.Time) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return "CLOSED"
	}
	local := moment.In(location)
	if isNYSERegularSession(local) {
		return "REGULAR"
	}
	if isNYSEOvernightSession(local) {
		return "OVERNIGHT"
	}
	if !isNYSETradingDay(local) {
		return "CLOSED"
	}
	minute := local.Hour()*60 + local.Minute()
	if minute >= 4*60 && minute < 9*60+30 {
		return "PRE"
	}
	postStart := marketcalendar.RegularCloseMinute(local)
	if minute >= postStart && minute < 20*60 {
		return "POST"
	}
	return "CLOSED"
}

func normalizeOCCRoot(value string) string {
	// Python matches Cboe roots with root.replace('.', '') only; it does not
	// remove embedded OCC padding spaces. Preserve those spaces here too.
	return strings.ReplaceAll(strings.ToUpper(value), ".", "")
}

func normalizeCboeSymbol(raw string) (string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	if !cboeSymbolPattern.MatchString(symbol) {
		return "", fmt.Errorf("invalid Cboe symbol: %s", raw)
	}
	return symbol, nil
}

func cboeNumber(raw json.RawMessage) *float64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		var stringValue string
		if json.Unmarshal(raw, &stringValue) != nil {
			return nil
		}
		value, err = strconv.ParseFloat(strings.TrimSpace(stringValue), 64)
		if err != nil {
			return nil
		}
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func cboeInteger(raw json.RawMessage) *int64 {
	value := cboeNumber(raw)
	if value == nil {
		return nil
	}
	if *value <= 0 {
		zero := int64(0)
		return &zero
	}
	// Python's int() keeps arbitrarily large provider counts positive. The
	// shared model and SQLite use int64, so saturate instead of wrapping them
	// into a negative count.
	if *value >= float64(math.MaxInt64) {
		maximum := int64(math.MaxInt64)
		return &maximum
	}
	integer := int64(*value)
	return &integer
}
