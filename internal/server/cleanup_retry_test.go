package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCleanupLeaseBusyHandlerReturnsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/cleanup", func(c *gin.Context) {
		writeCleanupResponse(c, nil, errCleanupLeaseBusy)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/cleanup", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cleanup status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After=%q, want 1", got)
	}
	var body struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Detail != errCleanupLeaseBusy.Error() {
		t.Fatalf("cleanup detail=%q, want %q", body.Detail, errCleanupLeaseBusy)
	}
}
