package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"option-scope-go/internal/model"
	"option-scope-go/internal/outboundhttp"
)

const (
	AlpacaDataBaseURL           = "https://data.alpaca.markets"
	alpacaSnapshotsPath         = "/v2/stocks/snapshots"
	defaultAlpacaRequestTimeout = 12 * time.Second
	AlpacaAuthCooldownDuration  = 5 * time.Minute
)

// Alpaca fetches Basic stock snapshots from Alpaca's overnight feed. It is
// intentionally limited to stock quotes; it does not request options data.
type Alpaca struct {
	apiKey   string
	secret   string
	baseURL  string
	client   *http.Client
	Gate     chan struct{}
	GateWait time.Duration

	authMu      sync.Mutex
	authRetryAt time.Time
	now         func() time.Time
}

// NewAlpaca creates an Alpaca overnight quote client. baseURL is the data API
// origin (for example, https://data.alpaca.markets); an empty value selects
// Alpaca's production data API. A nil client gets the default request timeout.
func NewAlpaca(apiKey, secret, baseURL string, client *http.Client) *Alpaca {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = AlpacaDataBaseURL
	}
	if client == nil {
		client = outboundhttp.DefaultAPIClient()
		client.Timeout = defaultAlpacaRequestTimeout
	}
	return &Alpaca{
		apiKey:  strings.TrimSpace(apiKey),
		secret:  strings.TrimSpace(secret),
		baseURL: baseURL,
		client:  client,
		now:     time.Now,
	}
}

// CanRequest reports whether the optional provider has credentials configured
// and is outside the authentication cooldown.
func (a *Alpaca) CanRequest() bool {
	if a == nil || strings.TrimSpace(a.apiKey) == "" || strings.TrimSpace(a.secret) == "" {
		return false
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if a.authRetryAt.IsZero() {
		return true
	}
	return !a.clockNowLocked().Before(a.authRetryAt)
}

// ValidateCredentials performs the same optional startup preflight as the
// Python service. Network and provider failures are diagnostic only; they do
// not prevent the main quote provider from starting.
func (a *Alpaca) ValidateCredentials(ctx context.Context) string {
	if a == nil || (a.apiKey == "" && a.secret == "") {
		return "disabled"
	}
	if a.apiKey == "" || a.secret == "" {
		return "invalid_config"
	}
	// A prior 401/403 already established that credentials need a cooldown.
	// Match the Python preflight behavior and avoid retrying during that window.
	if !a.CanRequest() {
		return "invalid_credentials"
	}
	if _, err := a.fetchSnapshot(ctx, "SPY"); err == nil {
		return "valid"
	}
	if !a.CanRequest() {
		return "invalid_credentials"
	}
	return "unavailable"
}

func (a *Alpaca) clockNowLocked() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Alpaca) markAuthFailure() {
	a.authMu.Lock()
	a.authRetryAt = a.clockNowLocked().Add(AlpacaAuthCooldownDuration)
	a.authMu.Unlock()
}

func (a *Alpaca) markAuthSuccess() {
	a.authMu.Lock()
	a.authRetryAt = time.Time{}
	a.authMu.Unlock()
}

// Quote fetches the Alpaca overnight snapshot for symbol and maps its trade,
// previous daily close, and any supplied bars into the shared quote model.
func (a *Alpaca) Quote(ctx context.Context, symbol string) (model.Quote, error) {
	if a == nil {
		return model.Quote{}, errors.New("Alpaca client is unavailable")
	}
	if a.apiKey == "" || a.secret == "" {
		return model.Quote{}, errors.New("Alpaca credentials are not configured")
	}
	if !a.CanRequest() {
		return model.Quote{}, errors.New("Alpaca credentials are in authentication cooldown")
	}
	normalized, err := NormalizeSymbol(symbol)
	if err != nil {
		return model.Quote{}, err
	}

	payload, err := a.fetchSnapshot(ctx, normalized)
	if err != nil {
		return model.Quote{}, err
	}
	snapshot, ok := object(payload[normalized])
	if !ok {
		return model.Quote{}, fmt.Errorf("Alpaca returned no snapshot for %s", normalized)
	}

	latestTrade, _ := object(snapshot["latestTrade"])
	previousDailyBar, _ := object(snapshot["prevDailyBar"])
	dailyBar, _ := object(snapshot["dailyBar"])
	minuteBar, _ := object(snapshot["minuteBar"])
	latestQuote, _ := object(snapshot["latestQuote"])

	price, valid := quoteMidpoint(latestQuote)
	priceSource := "indicative_quote"
	if !valid {
		price, valid = positiveField(latestTrade, "p")
		priceSource = "trade"
	}
	if !valid {
		// Match Python: overnightBar is not a price fallback; use minuteBar close.
		price, valid = positiveField(minuteBar, "c")
		priceSource = "minute_bar"
	}
	if !valid {
		return model.Quote{}, fmt.Errorf("Alpaca returned no finite positive overnight price for %s", normalized)
	}

	previousClose, hasPreviousClose := positiveField(previousDailyBar, "c")
	var previousClosePtr *float64
	if hasPreviousClose {
		previousClosePtr = &previousClose
	}
	todayOpen, hasTodayOpen := positiveField(dailyBar, "o")
	var todayOpenPtr *float64
	if hasTodayOpen {
		todayOpenPtr = &todayOpen
	}

	asOf := firstTimestamp(latestQuote["t"], latestTrade["t"], minuteBar["t"])
	session := map[string]any{
		"price":          price,
		"change_percent": nil,
		"as_of":          asOf,
		"volume":         finiteField(dailyBar, "v"),
		"bar_volume":     finiteField(minuteBar, "v"),
		"bid":            positiveFieldValue(latestQuote, "bp"),
		"ask":            positiveFieldValue(latestQuote, "ap"),
		"provider":       "alpaca-overnight",
		"price_source":   priceSource,
		"delayed":        true,
	}

	fetchedAt := time.Now().UTC()
	return model.Quote{
		Symbol:           normalized,
		Price:            &price,
		TodayOpen:        todayOpenPtr,
		PreviousClose:    previousClosePtr,
		Currency:         "USD",
		MarketState:      "OVERNIGHT",
		Provider:         "alpaca-overnight",
		Source:           "upstream",
		FetchedAt:        &fetchedAt,
		Sessions:         map[string]any{"overnight": session},
		FairValuePending: true,
	}, nil
}

func (a *Alpaca) fetchSnapshot(ctx context.Context, symbol string) (map[string]any, error) {
	endpoint, err := url.Parse(a.baseURL + alpacaSnapshotsPath)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("invalid Alpaca data base URL")
	}
	query := endpoint.Query()
	query.Set("symbols", symbol)
	query.Set("feed", "overnight")
	endpoint.RawQuery = query.Encode()

	request, err := outboundhttp.NewRequest(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("could not create Alpaca request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "option-scope/1.0")
	request.Header.Set("APCA-API-KEY-ID", a.apiKey)
	request.Header.Set("APCA-API-SECRET-KEY", a.secret)
	release, err := acquireGateTimeout(ctx, a.Gate, a.GateWait)
	if err != nil {
		return nil, err
	}
	defer release()

	response, err := outboundhttp.Do(a.client, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var networkErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkErr) && networkErr.Timeout()) {
			return nil, errors.New("Alpaca overnight request timed out")
		}
		// Do not return transport errors: custom transports may include request
		// details, including headers, in their error messages.
		return nil, errors.New("Alpaca overnight request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// Deliberately ignore the upstream body: authentication errors can
		// contain account details or credentials echoed by an intermediary.
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			a.markAuthFailure()
		}
		return nil, fmt.Errorf("Alpaca overnight request failed (HTTP %d)", response.StatusCode)
	}

	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Alpaca returned an invalid snapshot response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Alpaca returned an invalid snapshot response")
	}
	if payload == nil {
		return nil, errors.New("Alpaca returned an invalid snapshot response")
	}
	a.markAuthSuccess()
	return payload, nil
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

// quoteMidpoint follows Python's Alpaca _midpoint: valid non-crossed two-sided
// quotes use their midpoint; otherwise a valid bid takes precedence over ask.
func quoteMidpoint(quote map[string]any) (float64, bool) {
	bid, hasBid := positiveField(quote, "bp")
	ask, hasAsk := positiveField(quote, "ap")
	if hasBid && hasAsk && ask >= bid {
		return bid + (ask-bid)/2, true
	}
	if hasBid {
		return bid, true
	}
	if hasAsk {
		return ask, true
	}
	return 0, false
}

func positiveField(values map[string]any, key string) (float64, bool) {
	value, ok := finiteNumber(values[key])
	return value, ok && value > 0
}

func positiveFieldValue(values map[string]any, key string) any {
	if value, ok := positiveField(values, key); ok {
		return value
	}
	return nil
}

func finiteField(values map[string]any, key string) any {
	if value, ok := finiteNumber(values[key]); ok {
		return value
	}
	return nil
}

func finiteNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case float64:
		number = value
	case string:
		parsed, err := json.Number(value).Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func firstTimestamp(values ...any) any {
	for _, value := range values {
		text, ok := value.(string)
		if !ok || text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			for _, layout := range []string{
				"2006-01-02T15:04:05.999999999",
				"2006-01-02T15:04:05",
				"2006-01-02T15:04",
				"2006-01-02 15:04:05.999999999",
				"2006-01-02 15:04:05",
				"2006-01-02 15:04",
				"2006-01-02",
			} {
				if parsed, err = time.Parse(layout, text); err == nil {
					break
				}
			}
			if err != nil {
				return text
			}
		}
		parsed = parsed.UTC().Truncate(time.Microsecond)
		if parsed.Nanosecond() == 0 {
			return parsed.Format("2006-01-02T15:04:05-07:00")
		}
		return parsed.Format("2006-01-02T15:04:05.000000-07:00")
	}
	return nil
}
