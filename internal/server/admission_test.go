package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestAdmissionLimitRejectsOverflowWithoutQueueing(t *testing.T) {
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	defer gin.SetMode(previousMode)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	allowHandlerToFinish := func() { releaseOnce.Do(func() { close(release) }) }
	defer allowHandlerToFinish()
	router := gin.New()
	router.Use(requestAdmissionLimit(1))
	router.GET("/", func(c *gin.Context) {
		started <- struct{}{}
		<-release
		c.Status(http.StatusOK)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	firstDone := make(chan int, 1)
	go func() {
		response, err := http.Get(server.URL)
		if err != nil {
			firstDone <- 0
			return
		}
		defer response.Body.Close()
		firstDone <- response.StatusCode
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter the handler")
	}

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("overflow request failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("overflow status=%d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if got := response.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("overflow Retry-After=%q, want 1", got)
	}

	allowHandlerToFinish()
	select {
	case status := <-firstDone:
		if status != http.StatusOK {
			t.Fatalf("first request status=%d, want %d", status, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish after release")
	}
}
