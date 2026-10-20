// Package outboundhttp provides the shared clients used for outbound requests.
// Browser requests use CycleTLS for HTTPS; HTTP requests and API profile
// requests retain the standard library transport.
package outboundhttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	cycletls "github.com/Danny-Dasilva/CycleTLS/cycletls"
	fhttp "github.com/Danny-Dasilva/fhttp"
	"golang.org/x/net/proxy"
)

// ChromeJA3 matches the Chrome fingerprint used by go-yfinance.
const ChromeJA3 = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513,29-23-24,0"

// ChromeUserAgent is kept in step with the Chrome version advertised by the
// project's Yahoo SDK profile.
const ChromeUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

const defaultUserAgent = "option-scope-go/1.0"

// net/http keeps only two idle connections per host by default. Refresh bursts
// fan out over several symbols and expirations, so the defaults force fresh
// TCP/TLS handshakes once more than two requests overlap.
const (
	maxIdleConnections        = 64
	maxIdleConnectionsPerHost = 16
	idleConnectionTimeout     = 60 * time.Second
)

// BrowserClient uses CycleTLS for HTTPS requests and supports the project's
// HTTP, HTTPS, SOCKS5, and SOCKS5H proxy schemes. The per-request CycleTLS
// transport pool leases each CycleTLS transport to one request at a time so
// its internal connection caches remain isolated; HTTP/2 connections can be
// reused while HTTP/1 connections are closed after each response.
func BrowserClient(timeout time.Duration, proxyAddress string) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := ValidateProxy(proxyAddress); err != nil {
		return nil, err
	}
	proxyURL, err := ParseProxy(proxyAddress)
	if err != nil {
		return nil, err
	}
	dialer, err := cycleDialer(proxyURL)
	if err != nil {
		return nil, err
	}
	return newBrowserClient(timeout, proxyURL, dialer), nil
}

// NewRequest builds a standard-library request for an outbound client.
func NewRequest(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, target, body)
}

// Do sends an outbound request through the configured client. A nil client
// uses the shared default API client.
func Do(client *http.Client, request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("outbound HTTP request is nil")
	}
	if client == nil {
		client = DefaultAPIClient()
	}
	if request.Header.Get("User-Agent") == "" {
		request = request.Clone(request.Context())
		request.Header = request.Header.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		userAgent := defaultUserAgent
		if ProfileOf(client.Transport) == "browser" {
			userAgent = ChromeUserAgent
		}
		request.Header.Set("User-Agent", userAgent)
	}
	response, err := client.Do(request)
	return response, err
}

// CloseIdleConnections closes idle sockets held by a configured client.
func CloseIdleConnections(client *http.Client) {
	if client != nil {
		client.CloseIdleConnections()
	}
}

// DefaultAPIClient returns an unproxied API client with standard timeouts.
func DefaultAPIClient() *http.Client {
	return APIClientWithoutProxy(30 * time.Second)
}

// DefaultBrowserClient creates an unproxied browser-profile client for
// constructors that preserve a no-error legacy API.
func DefaultBrowserClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return newBrowserClient(timeout, nil, proxy.Direct)
}

// BrowserOptions returns the shared CycleTLS JA3 and user agent settings used
// by go-yfinance clients created alongside BrowserClient.
func BrowserOptions() (ja3, userAgent string) { return ChromeJA3, ChromeUserAgent }

func newBrowserClient(timeout time.Duration, proxyURL *url.URL, dialer proxy.ContextDialer) *http.Client {
	plainTransport := standardTransport(proxyURL)
	if proxyURL != nil && (proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h") {
		plainTransport.Proxy = nil
		plainTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		}
	}
	return &http.Client{Timeout: timeout, Transport: &browserTransport{plain: plainTransport, proxy: proxyURL, dialer: dialer, cycleTrans: make(map[*pooledCycleTransport]struct{})}}
}

// APIClient creates a standard net/http client for authenticated API traffic.
// It applies the same proxy parsing and timeout defaults as BrowserClient.
func APIClient(timeout time.Duration, proxyAddress string) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	proxyURL, err := ParseProxy(proxyAddress)
	if err != nil {
		return nil, err
	}
	transport, err := apiTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// APIClientWithoutProxy creates a standard API client without proxy routing.
func APIClientWithoutProxy(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout, Transport: standardTransport(nil)}
}

// UsingTransport wraps a caller supplied transport in a timeout-configured
// client. This keeps shared transport reuse inside this package's API.
func UsingTransport(transport http.RoundTripper, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if transport == nil {
		transport = standardTransport(nil)
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// ParseProxy validates and returns an optional supported proxy URL.
func ParseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil || proxyURL == nil || proxyURL.Host == "" || proxyURL.Opaque != "" || proxyURL.RawQuery != "" || proxyURL.Fragment != "" || (proxyURL.Path != "" && proxyURL.Path != "/") || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https" && proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h") {
		return nil, errors.New("invalid MARKET_PROXY URL")
	}
	if proxyURL.Hostname() == "" {
		return nil, errors.New("invalid MARKET_PROXY URL")
	}
	if port := proxyURL.Port(); port != "" {
		number, parseErr := strconv.Atoi(port)
		if parseErr != nil || number < 1 || number > 65535 {
			return nil, errors.New("invalid MARKET_PROXY URL")
		}
	} else {
		port := "1080"
		if proxyURL.Scheme == "http" {
			port = "80"
		} else if proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyURL.Host = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	return proxyURL, nil
}

// ValidateProxy checks proxy URL syntax and verifies the selected transport can
// build the required dialer before provider startup continues.
func ValidateProxy(raw string) error {
	proxyURL, err := ParseProxy(raw)
	if err != nil || proxyURL == nil {
		return err
	}
	_, err = cycleDialer(proxyURL)
	return err
}

// ProxyAddress returns the normalized proxy URL string, or an empty string
// when no proxy is configured.
func ProxyAddress(raw string) (string, error) {
	proxyURL, err := ParseProxy(raw)
	if err != nil || proxyURL == nil {
		return "", err
	}
	return proxyURL.String(), nil
}

// ProxyAddressFromTransport recovers the proxy configured on one of this
// package's clients or on a standard net/http transport.
func ProxyAddressFromTransport(transport http.RoundTripper) string {
	if provider, ok := transport.(interface{ ProxyAddress() string }); ok {
		return provider.ProxyAddress()
	}
	standard, ok := transport.(*http.Transport)
	if !ok || standard.Proxy == nil {
		return ""
	}
	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "query1.finance.yahoo.com"}}
	proxyURL, err := standard.Proxy(request)
	if err != nil || proxyURL == nil {
		return ""
	}
	return proxyURL.String()
}

func tunedTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = maxIdleConnections
	transport.MaxIdleConnsPerHost = maxIdleConnectionsPerHost
	transport.IdleConnTimeout = idleConnectionTimeout
	return transport
}

func standardTransport(proxyURL *url.URL) *http.Transport {
	transport := tunedTransport()
	if proxyURL == nil {
		transport.Proxy = nil
		return transport
	}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		return transport
	}
	proxyCopy := *proxyURL
	if proxyCopy.Scheme == "http" || proxyCopy.Scheme == "https" {
		if proxyCopy.Port() == "" {
			port := "80"
			if proxyCopy.Scheme == "https" {
				port = "443"
			}
			proxyCopy.Host = net.JoinHostPort(proxyCopy.Hostname(), port)
		}
	}
	transport.Proxy = http.ProxyURL(&proxyCopy)
	return transport
}

func apiTransport(proxyURL *url.URL) (*http.Transport, error) {
	if proxyURL == nil || (proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h") {
		return standardTransport(proxyURL), nil
	}
	dialer, err := cycleDialer(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := tunedTransport()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	return transport, nil
}

type browserTransport struct {
	plain      *http.Transport
	proxy      *url.URL
	dialer     proxy.ContextDialer
	cycleMu    sync.Mutex
	cycleIdle  []*pooledCycleTransport
	cycleTrans map[*pooledCycleTransport]struct{}
	cycleEpoch uint64
}

type pooledCycleTransport struct {
	transport fhttp.RoundTripper
	dialer    *requestContextDialer
	epoch     uint64
}

type localDNSDialer struct{ proxy.ContextDialer }

func (d localDNSDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return d.ContextDialer.DialContext(ctx, network, address)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		connection, dialErr := d.ContextDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("proxy hostname resolved to no IP addresses")
	}
	return nil, lastErr
}

func (t *browserTransport) ProxyAddress() string {
	if t == nil || t.proxy == nil {
		return ""
	}
	return t.proxy.String()
}

func (t *browserTransport) Profile() string { return "browser" }

func (t *browserTransport) CloseIdleConnections() {
	if t == nil {
		return
	}
	if t.plain != nil {
		t.plain.CloseIdleConnections()
	}
	t.cycleMu.Lock()
	t.cycleEpoch++
	transports := make([]fhttp.RoundTripper, 0, len(t.cycleIdle))
	for _, pooled := range t.cycleIdle {
		transports = append(transports, pooled.transport)
		delete(t.cycleTrans, pooled)
	}
	t.cycleIdle = nil
	t.cycleMu.Unlock()
	for _, transport := range transports {
		closeCycleTransport(transport)
	}
}

func (t *browserTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("outbound HTTP request has no URL")
	}
	if strings.EqualFold(request.URL.Scheme, "http") {
		request = request.Clone(request.Context())
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		if request.Header.Get("User-Agent") == "" {
			request.Header.Set("User-Agent", ChromeUserAgent)
		}
		return t.plain.RoundTrip(request)
	}
	if !strings.EqualFold(request.URL.Scheme, "https") {
		return nil, fmt.Errorf("unsupported outbound URL scheme %q", request.URL.Scheme)
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	cycleRequest, err := toCycleRequest(request)
	if err != nil {
		return nil, err
	}
	pooled, epoch := t.acquireCycleTransport()
	if pooled == nil {
		cycleDialer := &requestContextDialer{base: t.dialer}
		pooled = &pooledCycleTransport{transport: cycletls.NewTransportWithProxy(ChromeJA3, ChromeUserAgent, cycleDialer), dialer: cycleDialer, epoch: epoch}
		t.registerCycleTransport(pooled)
	}
	pooled.dialer.setContext(request.Context())
	cycleTransport := pooled.transport
	cycleResponse, err := cycleTransport.RoundTrip(cycleRequest)
	if err != nil {
		if cycleResponse != nil && cycleResponse.Body != nil {
			_ = cycleResponse.Body.Close()
		}
		t.discardCycleTransport(pooled)
		return nil, err
	}
	response := fromCycleResponse(cycleResponse, request)
	if response == nil || response.Body == nil {
		t.releaseCycleTransport(pooled, response != nil && response.ProtoMajor >= 2 && !response.Close)
		return response, nil
	}
	keepTransport := response.ProtoMajor >= 2 && !response.Close
	response.Body = &cycleResponseBody{ReadCloser: response.Body, release: func() { t.releaseCycleTransport(pooled, keepTransport) }}
	return response, nil
}

func (t *browserTransport) releaseCycleTransport(pooled *pooledCycleTransport, reusable bool) {
	t.cycleMu.Lock()
	if _, tracked := t.cycleTrans[pooled]; !tracked {
		t.cycleMu.Unlock()
		closeCycleTransport(pooled.transport)
		return
	}
	pooled.dialer.setContext(nil)
	if !reusable || pooled.epoch != t.cycleEpoch {
		delete(t.cycleTrans, pooled)
		t.cycleMu.Unlock()
		closeCycleTransport(pooled.transport)
		return
	}
	t.cycleIdle = append(t.cycleIdle, pooled)
	t.cycleMu.Unlock()
}

func (t *browserTransport) discardCycleTransport(pooled *pooledCycleTransport) {
	pooled.dialer.setContext(nil)
	t.cycleMu.Lock()
	delete(t.cycleTrans, pooled)
	t.cycleMu.Unlock()
	closeCycleTransport(pooled.transport)
}

func (t *browserTransport) acquireCycleTransport() (*pooledCycleTransport, uint64) {
	t.cycleMu.Lock()
	defer t.cycleMu.Unlock()
	if len(t.cycleIdle) == 0 {
		return nil, t.cycleEpoch
	}
	last := len(t.cycleIdle) - 1
	pooled := t.cycleIdle[last]
	t.cycleIdle = t.cycleIdle[:last]
	pooled.epoch = t.cycleEpoch
	return pooled, t.cycleEpoch
}

func (t *browserTransport) registerCycleTransport(pooled *pooledCycleTransport) {
	t.cycleMu.Lock()
	defer t.cycleMu.Unlock()
	t.cycleTrans[pooled] = struct{}{}
}

// ProfileOf returns the configured client profile, when it is identifiable.
func ProfileOf(transport http.RoundTripper) string {
	if profile, ok := transport.(interface{ Profile() string }); ok {
		return profile.Profile()
	}
	return "api"
}

func closeCycleTransport(transport fhttp.RoundTripper) {
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type cycleResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

type requestContextDialer struct {
	base proxy.ContextDialer
	mu   sync.RWMutex
	ctx  context.Context
}

func (d *requestContextDialer) setContext(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *requestContextDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *requestContextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Done() == nil {
		d.mu.RLock()
		requestContext := d.ctx
		d.mu.RUnlock()
		if requestContext != nil {
			ctx = requestContext
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.base.DialContext(ctx, network, address)
}

func (body *cycleResponseBody) Close() error {
	body.once.Do(func() {
		body.err = body.ReadCloser.Close()
		body.release()
	})
	return body.err
}

func toCycleRequest(request *http.Request) (*fhttp.Request, error) {
	cycleRequest, err := fhttp.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), request.Body)
	if err != nil {
		return nil, err
	}
	cycleRequest.Header = make(fhttp.Header, len(request.Header))
	for key, values := range request.Header {
		cycleRequest.Header[key] = append([]string(nil), values...)
	}
	cycleRequest.Host = request.Host
	if cycleRequest.Host == "" {
		cycleRequest.Host = request.URL.Host
	}
	cycleRequest.ContentLength = request.ContentLength
	if request.Body == nil || request.Body == http.NoBody {
		cycleRequest.Body = fhttp.NoBody
	}
	cycleRequest.TransferEncoding = append([]string(nil), request.TransferEncoding...)
	cycleRequest.Close = request.Close
	if len(request.Trailer) > 0 {
		cycleRequest.Trailer = make(fhttp.Header, len(request.Trailer))
		for key, values := range request.Trailer {
			cycleRequest.Trailer[key] = append([]string(nil), values...)
		}
	}
	if request.GetBody != nil {
		cycleRequest.GetBody = request.GetBody
	}
	if request.URL.User != nil && cycleRequest.Header.Get("Authorization") == "" {
		password, _ := request.URL.User.Password()
		cycleRequest.SetBasicAuth(request.URL.User.Username(), password)
	}
	return cycleRequest, nil
}

func fromCycleResponse(response *fhttp.Response, request *http.Request) *http.Response {
	if response == nil {
		return nil
	}
	header := make(http.Header, len(response.Header))
	for key, values := range response.Header {
		header[key] = append([]string(nil), values...)
	}
	return &http.Response{
		Status: response.Status, StatusCode: response.StatusCode, Proto: response.Proto,
		ProtoMajor: response.ProtoMajor, ProtoMinor: response.ProtoMinor, Header: header,
		Body: response.Body, ContentLength: response.ContentLength, TransferEncoding: append([]string(nil), response.TransferEncoding...),
		Close: response.Close, Uncompressed: response.Uncompressed, Trailer: convertCycleHeader(response.Trailer), Request: request,
	}
}

func convertCycleHeader(header fhttp.Header) http.Header {
	if len(header) == 0 {
		return nil
	}
	converted := make(http.Header, len(header))
	for key, values := range header {
		converted[key] = append([]string(nil), values...)
	}
	return converted
}

func cycleDialer(proxyURL *url.URL) (proxy.ContextDialer, error) {
	if proxyURL == nil {
		return proxy.Direct, nil
	}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		var auth *proxy.Auth
		if proxyURL.User != nil {
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: username, Password: password}
		}
		proxyAddress := proxyURL.Host
		if proxyURL.Port() == "" {
			proxyAddress = net.JoinHostPort(proxyURL.Hostname(), "1080")
		}
		var forward proxy.Dialer
		if proxyURL.Scheme == "socks5h" {
			forward = proxy.Direct
		}
		dialer, err := proxy.SOCKS5("tcp", proxyAddress, auth, forward)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 proxy dialer: %w", err)
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 proxy does not support context-aware dialing")
		}
		if proxyURL.Scheme == "socks5" {
			return localDNSDialer{ContextDialer: contextDialer}, nil
		}
		return contextDialer, nil
	}
	return &connectProxyDialer{proxyURL: proxyURL}, nil
}

type connectProxyDialer struct{ proxyURL *url.URL }

func (d *connectProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *connectProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || d.proxyURL == nil {
		return nil, errors.New("HTTP proxy is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	proxyAddress := d.proxyURL.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		port := "80"
		if d.proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddress = net.JoinHostPort(d.proxyURL.Hostname(), port)
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, network, proxyAddress)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = connection.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()
	if d.proxyURL.Scheme == "https" {
		tlsConnection := tls.Client(connection, &tls.Config{ServerName: d.proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("connect to HTTPS proxy: %w", err)
		}
		connection = tlsConnection
	}
	connectRequest := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := connectRequest.Write(connection); err != nil {
		return nil, fmt.Errorf("send proxy CONNECT request: %w", err)
	}
	buffered := bufio.NewReader(connection)
	response, err := http.ReadResponse(buffered, connectRequest)
	if err != nil {
		return nil, fmt.Errorf("read proxy CONNECT response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		return nil, fmt.Errorf("proxy CONNECT returned HTTP %d", response.StatusCode)
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	if buffered.Buffered() > 0 {
		return &bufferedConnection{Conn: connection, reader: buffered}, nil
	}
	return connection, nil
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(buffer []byte) (int, error) { return c.reader.Read(buffer) }
