package acexy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func startMockBackendForFetch() *httptest.Server {
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

func TestFetchStream_ConcurrentSameID_NoBackendLeak(t *testing.T) {
	backend := startMockBackendForFetch()
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

	streamID := AceID{id: "concurrent-test"}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.FetchStream(streamID, nil)
			if err != nil {
				t.Errorf("FetchStream failed: %v", err)
			}
		}()
	}

	wg.Wait()

	// Give time for any duplicate close commands to fire
	time.Sleep(500 * time.Millisecond)

	// Verify only one stream exists in the map
	a.mutex.Lock()
	streamCount := len(a.streams)
	a.mutex.Unlock()

	if streamCount != 1 {
		t.Errorf("expected 1 stream in map, got %d", streamCount)
	}
}

func mustParseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}
