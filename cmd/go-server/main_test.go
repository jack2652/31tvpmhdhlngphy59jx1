package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeHTTPWaitsForHandlersOnShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	requests := &requestTracker{}
	httpServer := &http.Server{Handler: requests.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- serveHTTP(ctx, httpServer, listener, requests, time.Second)
	}()

	clientResult := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			response.Body.Close()
		}
		clientResult <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("server returned before active handler exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serveHTTP returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not finish after handler exited")
	}
	select {
	case err := <-clientResult:
		if err != nil {
			t.Fatalf("client request failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client request did not finish")
	}
}

type failAfterFirstAcceptListener struct {
	net.Listener
	accepts atomic.Int32
	fail    <-chan struct{}
	err     error
}

func (l *failAfterFirstAcceptListener) Accept() (net.Conn, error) {
	if l.accepts.Add(1) > 1 {
		<-l.fail
		return nil, l.err
	}
	return l.Listener.Accept()
}

func TestServeHTTPWaitsForHandlersAfterListenerError(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenerErr := errors.New("listener failed")
	failAccept := make(chan struct{})
	listener := &failAfterFirstAcceptListener{Listener: base, fail: failAccept, err: listenerErr}
	started := make(chan struct{})
	release := make(chan struct{})
	requests := &requestTracker{}
	httpServer := &http.Server{Handler: requests.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		close(failAccept)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))}
	result := make(chan error, 1)
	go func() {
		result <- serveHTTP(context.Background(), httpServer, listener, requests, time.Second)
	}()

	clientResult := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			response.Body.Close()
		}
		clientResult <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	select {
	case err := <-result:
		t.Fatalf("server returned before active handler exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, listenerErr) {
			t.Fatalf("serveHTTP error = %v, want listener error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not finish after handler exited")
	}
	select {
	case <-clientResult:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish")
	}
}
