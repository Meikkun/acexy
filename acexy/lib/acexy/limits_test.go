package acexy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func startMockBackendForLimits() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/ace/getstream", func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		jsonResp := fmt.Sprintf(`{
			"response": {
				"playback_url": "http://%s/stream",
				"stat_url": "http://%s/stat",
				"command_url": "http://%s/command",
				"infohash": "hash",
				"playback_session_id": "session",
				"is_live": 1,
				"is_encrypted": 0,
				"client_session_id": 123
			},
			"error": null
		}`, host, host, host)
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
		w.Write([]byte(`{"response": "ok", "error": null}`))
	})

	return httptest.NewServer(mux)
}

func TestFetchStream_RejectsWhenChannelLimitReached(t *testing.T) {
	backend := startMockBackendForLimits()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &Acexy{
		Scheme:                "http",
		Host:                  backendURL.Hostname(),
		Port:                  mustParseInt(backendURL.Port()),
		Endpoint:              MPEG_TS_ENDPOINT,
		EmptyTimeout:          10 * time.Second,
		BufferSize:            1024,
		NoResponseTimeout:     2 * time.Second,
		MaxConcurrentChannels: 2,
	}
	a.Init()

	idA := AceID{id: "aaa"}
	idB := AceID{id: "bbb"}
	idC := AceID{id: "ccc"}

	_, err := a.FetchStream(idA, nil)
	if err != nil {
		t.Fatalf("FetchStream(aaa) failed: %v", err)
	}

	_, err = a.FetchStream(idB, nil)
	if err != nil {
		t.Fatalf("FetchStream(bbb) failed: %v", err)
	}

	_, err = a.FetchStream(idC, nil)
	if err == nil {
		t.Fatal("FetchStream(ccc) should have failed with channel limit reached")
	}

	limitErr, ok := err.(*ErrLimitReached)
	if !ok {
		t.Fatalf("expected *ErrLimitReached, got %T", err)
	}
	if limitErr.Message == "" {
		t.Fatal("ErrLimitReached message should not be empty")
	}

	// Reusing existing channel should succeed even at limit
	_, err = a.FetchStream(idA, nil)
	if err != nil {
		t.Fatalf("FetchStream(aaa) reuse failed: %v", err)
	}
}

func TestFetchStream_UnlimitedWhenZero(t *testing.T) {
	backend := startMockBackendForLimits()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
		// MaxConcurrentChannels defaults to 0 (unlimited)
	}
	a.Init()

	for i := 0; i < 10; i++ {
		id := AceID{id: fmt.Sprintf("stream-%d", i)}
		_, err := a.FetchStream(id, nil)
		if err != nil {
			t.Fatalf("FetchStream(%s) failed: %v", id.id, err)
		}
	}
}

func TestClientCount_Accurate(t *testing.T) {
	backend := startMockBackendForLimits()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	idA := AceID{id: "stream-a"}
	idB := AceID{id: "stream-b"}

	streamA, err := a.FetchStream(idA, nil)
	if err != nil {
		t.Fatalf("FetchStream(A) failed: %v", err)
	}
	streamB, err := a.FetchStream(idB, nil)
	if err != nil {
		t.Fatalf("FetchStream(B) failed: %v", err)
	}

	// Add 3 clients to A and 2 to B using a dummy writer
	writers := make([]*mockWriter, 5)
	for i := range writers {
		writers[i] = &mockWriter{}
	}

	for i := 0; i < 3; i++ {
		if err := a.StartStream(streamA, writers[i]); err != nil {
			t.Fatalf("StartStream(A, %d) failed: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := a.StartStream(streamB, writers[3+i]); err != nil {
			t.Fatalf("StartStream(B, %d) failed: %v", i, err)
		}
	}

	if count := a.ClientCount(); count != 5 {
		t.Fatalf("expected ClientCount=5, got %d", count)
	}

	// Stop one client from A
	a.StopStream(streamA, writers[0])

	if count := a.ClientCount(); count != 4 {
		t.Fatalf("expected ClientCount=4 after stop, got %d", count)
	}
}

func TestStreamCount_Accurate(t *testing.T) {
	backend := startMockBackendForLimits()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	if count := a.StreamCount(); count != 0 {
		t.Fatalf("expected StreamCount=0, got %d", count)
	}

	idA := AceID{id: "stream-a"}
	_, err := a.FetchStream(idA, nil)
	if err != nil {
		t.Fatalf("FetchStream(A) failed: %v", err)
	}

	if count := a.StreamCount(); count != 1 {
		t.Fatalf("expected StreamCount=1, got %d", count)
	}
}

type mockWriter struct {
	data []byte
}

func (m *mockWriter) Write(p []byte) (n int, err error) {
	m.data = append(m.data, p...)
	return len(p), nil
}
