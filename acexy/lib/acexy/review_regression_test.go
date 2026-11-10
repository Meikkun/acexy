package acexy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectedPlaybackUsesFreshBackendConnections(t *testing.T) {
	var mu sync.Mutex
	used := make(map[string]bool)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reused := used[r.RemoteAddr]
		used[r.RemoteAddr] = true
		mu.Unlock()
		if reused {
			<-r.Context().Done()
			return
		}
		switch r.URL.Path {
		case "/ace/getstream":
			fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
		case "/play":
			http.Redirect(w, r, "/content", http.StatusFound)
		case "/content":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := w.Write([]byte("live bytes")); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}
		case "/stop":
			fmt.Fprint(w, `{"response":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	u, _ := url.Parse(backend.URL)
	a := &Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()),
		Endpoint: MPEG_TS_ENDPOINT, NoResponseTimeout: 300 * time.Millisecond, EmptyTimeout: time.Second}
	a.Init()
	t.Cleanup(func() { a.Shutdown(); backend.Close() })
	stream, err := a.FetchStream(AceID{id: "redirected-live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &discardClient{}
	if err := a.StartStream(stream, writer); err != nil {
		t.Fatal("redirected playback did not start:", err)
	}
	eventually(t, func() bool { return writer.bytes.Load() > 0 })
	a.StopStream(stream, writer)
	a.workers.Wait()
	status, _ := a.GetStatus(nil)
	if status.PendingCleanup != 0 {
		t.Fatal("redirected playback left unresolved cleanup")
	}
}

func TestMetadataTimeoutPreservesUncertainBackendOwnership(t *testing.T) {
	t.Run("timeout", func(t *testing.T) { testUncertainMetadataOwnership(t, false) })
	t.Run("cancelled after dispatch", func(t *testing.T) { testUncertainMetadataOwnership(t, true) })
}

func testUncertainMetadataOwnership(t *testing.T, cancelWaiter bool) {
	t.Helper()
	backend := &admissionBackend{metadataGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	a.NoResponseTimeout = 50 * time.Millisecond
	reservation, err := a.ReserveAdmission(AceID{id: "created-before-response"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelWaiter {
		eventually(t, func() bool { return backend.metadata.Load() == 1 })
		cancel()
	}
	_, err = a.FetchReservedStream(ctx, reservation)
	reservation.Release()
	if err == nil {
		t.Fatal("metadata timeout unexpectedly succeeded")
	}
	a.workers.Wait()
	status, _ := a.GetStatus(nil)
	if backend.metadata.Load() != 1 || backend.stops.Load() != 0 {
		t.Fatal("fixture must dispatch creation without returning a cleanup identity")
	}
	if status.PendingCleanup != 1 {
		t.Fatalf("unacknowledged backend creation lost ownership: pending_cleanup=%d", status.PendingCleanup)
	}
	if err := a.SetMaintenance("quiescing", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMaintenance("maintenance", nil); err == nil {
		t.Fatal("maintenance admitted while backend ownership was uncertain")
	}
}

func TestCancelledStartupCannotRemoveReplacementRegistration(t *testing.T) {
	backend := &admissionBackend{playbackGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	first, _ := a.ReserveAdmission(AceID{id: "same-writer"}, nil)
	defer first.Release()
	second, _ := a.ReserveAdmission(AceID{id: "same-writer"}, nil)
	defer second.Release()
	stream, err := a.FetchReservedStream(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	writer := &discardClient{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstResult := make(chan error, 1)
	go func() { firstResult <- a.StartReservedStream(ctx, first, stream, writer) }()
	eventually(t, func() bool { return backend.playback.Load() == 1 })
	a.StopStream(stream, writer)
	secondResult := make(chan error, 1)
	go func() { secondResult <- a.StartReservedStream(context.Background(), second, stream, writer) }()
	eventually(t, func() bool { return a.ClientCount() == 1 })
	cancel()
	if err := <-firstResult; err == nil {
		t.Fatal("cancelled original startup succeeded")
	}
	if a.ClientCount() != 1 {
		close(backend.playbackGate)
		<-secondResult
		t.Fatal("cancelled old startup removed replacement registration")
	}
	close(backend.playbackGate)
	if err := <-secondResult; err != nil {
		t.Fatal("replacement startup failed:", err)
	}
}

func TestCompletedStartupCannotClaimReplacementRegistration(t *testing.T) {
	backend := &admissionBackend{playbackGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	first, _ := a.ReserveAdmission(AceID{id: "same-writer"}, nil)
	defer first.Release()
	second, _ := a.ReserveAdmission(AceID{id: "same-writer"}, nil)
	defer second.Release()
	stream, err := a.FetchReservedStream(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	writer := &discardClient{}
	firstResult := make(chan error, 1)
	go func() { firstResult <- a.StartReservedStream(context.Background(), first, stream, writer) }()
	eventually(t, func() bool { return backend.playback.Load() == 1 })
	a.StopStream(stream, writer)
	secondResult := make(chan error, 1)
	go func() { secondResult <- a.StartReservedStream(context.Background(), second, stream, writer) }()
	eventually(t, func() bool { return a.ClientCount() == 1 })
	close(backend.playbackGate)
	if err := <-firstResult; err == nil {
		t.Fatal("old successful startup claimed replacement registration")
	}
	if err := <-secondResult; err != nil || a.ClientCount() != 1 {
		t.Fatalf("old startup completion damaged replacement: %v, clients=%d", err, a.ClientCount())
	}
}

func TestMetadataPreDispatchFailureDoesNotInventCleanup(t *testing.T) {
	a := &Acexy{Scheme: "http", Host: "127.0.0.1", Port: 1, Endpoint: MPEG_TS_ENDPOINT}
	a.Init()
	defer a.Shutdown()
	a.middleware.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture dial failed before connection")
		},
	}
	reservation, err := a.ReserveAdmission(AceID{id: "not-dispatched"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.FetchReservedStream(context.Background(), reservation)
	reservation.Release()
	if err == nil {
		t.Fatal("fixture dial failure unexpectedly succeeded")
	}
	a.workers.Wait()
	status, _ := a.GetStatus(nil)
	if status.PendingCleanup != 0 {
		t.Fatal("pre-dispatch failure incorrectly retained backend cleanup")
	}
	if err := a.SetMaintenance("maintenance", nil); err != nil {
		t.Fatal("pre-dispatch failure prevented idle maintenance:", err)
	}
}

func TestCancelledMetadataRecoversLateCleanupIdentity(t *testing.T) {
	created, releaseMetadata, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ace/getstream":
			close(created)
			select {
			case <-releaseMetadata:
				fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
			case <-r.Context().Done():
			}
		case "/stop":
			close(stopped)
			fmt.Fprint(w, `{"response":"ok"}`)
		case "/play":
			t.Error("cancelled startup opened playback")
		}
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	a := &Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()), Endpoint: MPEG_TS_ENDPOINT, NoResponseTimeout: time.Second}
	a.Init()
	defer a.Shutdown()
	reservation, err := a.ReserveAdmission(AceID{id: "late-identity"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-created
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.FetchReservedStream(ctx, reservation); err == nil {
		t.Fatal("cancelled metadata waiter succeeded")
	}
	status, _ := a.GetStatus(nil)
	if status.PendingCleanup != 1 {
		t.Fatal("lost ownership before metadata identity was recovered")
	}
	close(releaseMetadata)
	a.workers.Wait()
	select {
	case <-stopped:
	default:
		t.Fatal("late metadata command identity was not stopped")
	}
	status, _ = a.GetStatus(nil)
	if status.PendingCleanup != 0 {
		t.Fatal("successful recovered stop did not resolve cleanup")
	}
}

func TestFailedMetadataStillUsesRecoveredCommandIdentity(t *testing.T) {
	for _, responseStatus := range []int{200, 500} {
		t.Run(fmt.Sprint(responseStatus), func(t *testing.T) {
			var stops atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ace/getstream":
					w.WriteHeader(responseStatus)
					// A valid cleanup identity remains usable even though the
					// missing playback URL or unsuccessful status rejects startup.
					fmt.Fprintf(w, `{"response":{"command_url":"http://%s/stop"}}`, r.Host)
				case "/stop":
					stops.Add(1)
					fmt.Fprint(w, `{"response":"ok"}`)
				}
			}))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			a := &Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()), Endpoint: MPEG_TS_ENDPOINT}
			a.Init()
			defer a.Shutdown()
			reservation, err := a.ReserveAdmission(AceID{id: "recoverable failure"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.FetchReservedStream(context.Background(), reservation); err == nil {
				t.Fatal("invalid metadata unexpectedly started")
			}
			reservation.Release()
			a.workers.Wait()
			status, _ := a.GetStatus(nil)
			if stops.Load() != 1 || status.PendingCleanup != 0 {
				t.Fatalf("recovered identity was not cleaned: stops=%d pending=%d", stops.Load(), status.PendingCleanup)
			}
		})
	}
}
