package outboundhttp

import (
	"net/http"
	"net/url"
	"testing"
)

func TestStandardTransportsKeepIdleConnectionsForBursts(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		transport *http.Transport
		proxied   bool
	}{
		{name: "direct", transport: standardTransport(nil)},
		{name: "proxied", transport: standardTransport(proxyURL), proxied: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.transport.MaxIdleConns != maxIdleConnections {
				t.Fatalf("MaxIdleConns = %d, want %d", testCase.transport.MaxIdleConns, maxIdleConnections)
			}
			if testCase.transport.MaxIdleConnsPerHost != maxIdleConnectionsPerHost {
				t.Fatalf("MaxIdleConnsPerHost = %d, want %d", testCase.transport.MaxIdleConnsPerHost, maxIdleConnectionsPerHost)
			}
			if testCase.transport.IdleConnTimeout != idleConnectionTimeout {
				t.Fatalf("IdleConnTimeout = %v, want %v", testCase.transport.IdleConnTimeout, idleConnectionTimeout)
			}
			if !testCase.transport.ForceAttemptHTTP2 {
				t.Fatal("ForceAttemptHTTP2 must stay enabled")
			}
			if (testCase.transport.Proxy != nil) != testCase.proxied {
				t.Fatalf("proxy configured = %v, want %v", testCase.transport.Proxy != nil, testCase.proxied)
			}
		})
	}
}
