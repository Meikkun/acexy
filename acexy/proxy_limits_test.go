package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"javinator9889/acexy/lib/acexy"
)

type dummyWriter struct{}

func (d *dummyWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func TestHandleStream_RejectsWhenConnectionLimitReached(t *testing.T) {
	oldMax := maxConnections
	maxConnections = 1
	defer func() { maxConnections = oldMax }()

	backend := startMockBackend()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	streamID, _ := acexy.NewAceID("limit-test", "")
	stream, err := a.FetchStream(streamID, nil)
	if err != nil {
		t.Fatalf("FetchStream failed: %v", err)
	}
	dw := &dummyWriter{}
	if err := a.StartStream(stream, dw); err != nil {
		t.Fatalf("StartStream failed: %v", err)
	}
	defer a.StopStream(stream, dw)

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/getstream?id=other", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStream(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "Maximum connections reached") {
		t.Fatalf("expected body to contain 'Maximum connections reached', got %s", body)
	}
}

func TestHandleStream_AllowsWhenUnderLimit(t *testing.T) {
	oldMax := maxConnections
	maxConnections = 10
	defer func() { maxConnections = oldMax }()

	backend := startMockBackend()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()
	defer a.Shutdown()

	// Create 5 active clients on the same stream
	streamID, _ := acexy.NewAceID("under-limit", "")
	stream, err := a.FetchStream(streamID, nil)
	if err != nil {
		t.Fatalf("FetchStream failed: %v", err)
	}
	writers := make([]*dummyWriter, 5)
	for i := 0; i < 5; i++ {
		writers[i] = &dummyWriter{}
		if err := a.StartStream(stream, writers[i]); err != nil {
			t.Fatalf("StartStream(%d) failed: %v", i, err)
		}
		defer func(w io.Writer) { a.StopStream(stream, w) }(writers[i])
	}

	proxy := &Proxy{Acexy: a}
	// Use a short-lived context so HandleStream returns quickly
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/ace/getstream?id=other", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	proxy.HandleStream(rr, req)

	// The request should NOT be rejected due to connection limit.
	// It may fail for other reasons (e.g., backend response) in a unit test,
	// but the status must not be 503.
	if rr.Code == http.StatusServiceUnavailable {
		t.Fatalf("expected request to pass limit check, got 503")
	}
}
