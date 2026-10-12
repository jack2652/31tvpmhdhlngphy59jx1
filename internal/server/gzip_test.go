package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func compressionTestEngine(lowMemory bool, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(responseCompression(lowMemory))
	router.GET("/", handler)
	return router
}

func jsonBodyOfSize(size int) string {
	const prefix, suffix = `{"data":"`, `"}`
	return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
}

func TestResponseCompressionThresholdAndJSON(t *testing.T) {
	for _, test := range []struct {
		size       int
		compressed bool
	}{{gzipMinimumSize - 1, false}, {gzipMinimumSize, true}} {
		t.Run(strconv.Itoa(test.size), func(t *testing.T) {
			body := jsonBodyOfSize(test.size)
			router := compressionTestEngine(false, func(c *gin.Context) {
				c.Header("Vary", "Origin")
				c.Data(http.StatusOK, "application/json", []byte(body))
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			encoded := response.Body.Bytes()
			if got := response.Header().Get("Content-Encoding"); test.compressed && got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			} else if !test.compressed && got != "" {
				t.Fatalf("Content-Encoding = %q, want no compression", got)
			}
			if test.compressed {
				reader, err := gzip.NewReader(response.Body)
				if err != nil {
					t.Fatalf("gzip.NewReader: %v", err)
				}
				decoded, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("read compressed body: %v", err)
				}
				_ = reader.Close()
				encoded = decoded
			}
			if string(encoded) != body {
				t.Fatalf("response body differs from JSON payload")
			}
			if !hasVaryToken(response.Header(), "Origin") || !hasVaryToken(response.Header(), "Accept-Encoding") {
				t.Fatalf("Vary = %v, want Origin and Accept-Encoding", response.Header().Values("Vary"))
			}
			if test.compressed && response.Header().Get("Content-Length") != "" {
				t.Fatalf("compressed response retained Content-Length: %s", response.Header().Get("Content-Length"))
			}
		})
	}
}

func TestResponseCompressionHonorsLowMemoryAndEncoding(t *testing.T) {
	body := jsonBodyOfSize(gzipMinimumSize * 2)
	for _, test := range []struct {
		name      string
		lowMemory bool
		accept    string
	}{
		{name: "low memory", lowMemory: true, accept: "gzip"},
		{name: "gzip rejected", accept: "gzip;q=0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := compressionTestEngine(test.lowMemory, func(c *gin.Context) {
				c.Data(http.StatusOK, "application/json", []byte(body))
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", test.accept)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if got := response.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want empty", got)
			}
			if response.Body.String() != body {
				t.Fatalf("body changed for uncompressed response")
			}
			if test.lowMemory && len(response.Header().Values("Vary")) != 0 {
				t.Fatalf("low-memory response unexpectedly added Vary: %v", response.Header().Values("Vary"))
			}
			if !test.lowMemory && len(response.Header().Values("Vary")) != 0 {
				t.Fatalf("rejected gzip response unexpectedly added Vary: %v", response.Header().Values("Vary"))
			}
		})
	}
}

func TestResponseCompressionDoesNotDoubleCompress(t *testing.T) {
	body := jsonBodyOfSize(gzipMinimumSize * 2)
	router := compressionTestEngine(false, func(c *gin.Context) {
		c.Header("Content-Encoding", "br")
		c.Data(http.StatusOK, "application/json", []byte(body))
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if got := response.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want existing br", got)
	}
	if response.Body.String() != body {
		t.Fatalf("pre-encoded body was modified")
	}
}

func TestResponseCompressionLeavesSSEStreaming(t *testing.T) {
	wrote := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	router := compressionTestEngine(false, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = c.Writer.Write([]byte("data: first\n\n"))
		once.Do(func() { close(wrote) })
		<-release
		_, _ = c.Writer.Write([]byte("data: second\n\n"))
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(response, request)
		close(done)
	}()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SSE handler did not finish")
		}
	}()

	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not write its first event")
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE Content-Encoding = %q, want empty", got)
	}
	if got := response.Body.String(); got != "data: first\n\n" {
		t.Fatalf("first SSE event was not streamed immediately: %q", got)
	}
}

func hasVaryToken(header http.Header, want string) bool {
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}
