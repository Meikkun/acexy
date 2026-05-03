package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"javinator9889/acexy/lib/acexy"
)

func startMockBackendForShutdown() (*httptest.Server, *sync.Map) {
	stops := &sync.Map{}
	mux := http.NewServeMux()

	mux.HandleFunc("/ace/getstream", func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		jsonResp := fmt.Sprintf(`{
			"response": {
				"playback_url": "http://%s/stream",
				"stat_url": "http://%s/stat",
				"command_url": "http://%s/command?id=%s",
				"infohash": "hash",
				"playback_session_id": "session",
				"is_live": 1,
				"is_encrypted": 0,
				"client_session_id": 123
			},
			"error": null
		}`, host, host, host, r.URL.Query().Get("id"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jsonResp))
	})

	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1024)
		for i := range chunk {
			chunk[i] = 'A'
		}
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})

	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("method") == "stop" {
			stops.Store(r.URL.Query().Get("id"), true)
		}
		w.Write([]byte(`{"response": "ok", "error": null}`))
	})

	return httptest.NewServer(mux), stops
}

func TestGracefulShutdown_StreamsReleasedOnSignal(t *testing.T) {
	backend, stops := startMockBackendForShutdown()
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

	streamID, err := acexy.NewAceID("shutdown-test", "")
	if err != nil {
		t.Fatalf("NewAceID failed: %v", err)
	}
	stream, err := a.FetchStream(streamID, nil)
	if err != nil {
		t.Fatalf("FetchStream failed: %v", err)
	}

	done := a.WaitStream(stream)
	if done == nil {
		t.Fatal("WaitStream should not be nil before shutdown")
	}

	status, err := a.GetStatus(nil)
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status.Streams == nil || *status.Streams == 0 {
		t.Fatal("expected at least 1 active stream before shutdown")
	}

	a.Shutdown()

	// Verify stream count is 0
	status, err = a.GetStatus(nil)
	if err != nil {
		t.Fatalf("GetStatus failed after shutdown: %v", err)
	}
	if status.Streams != nil && *status.Streams != 0 {
		t.Fatalf("expected 0 streams after shutdown, got %d", *status.Streams)
	}

	// Verify the backend received method=stop
	if _, ok := stops.Load("shutdown-test"); !ok {
		t.Fatal("expected backend to receive method=stop")
	}

	// Verify WaitStream channel is closed
	select {
	case <-done:
		// closed as expected
	default:
		t.Fatal("expected WaitStream channel to be closed after shutdown")
	}
}

func TestGracefulShutdown_EmptyServer(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              "localhost",
		Port:              6878,
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	// Should not panic or error with no active streams
	a.Shutdown()

	status, err := a.GetStatus(nil)
	if err != nil {
		t.Fatalf("GetStatus failed after shutdown: %v", err)
	}
	if status.Streams != nil && *status.Streams != 0 {
		t.Fatalf("expected 0 streams, got %d", *status.Streams)
	}
}
