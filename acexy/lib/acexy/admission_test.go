package acexy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type admissionBackend struct {
	metadataGate chan struct{}
	playbackGate chan struct{}
	stopGate     chan struct{}
	metadata     atomic.Int32
	playback     atomic.Int32
	stops        atomic.Int32
	status       int
	body         string
	stopStatus   int
}

func newAdmissionBackend(t *testing.T, backend *admissionBackend) *Acexy {
	t.Helper()
	wait := func(r *http.Request, gate chan struct{}) bool {
		if gate == nil {
			return true
		}
		select {
		case <-gate:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ace/getstream":
			backend.metadata.Add(1)
			if !wait(r, backend.metadataGate) {
				return
			}
			if backend.body != "" {
				fmt.Fprint(w, backend.body)
				return
			}
			fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
		case "/play":
			backend.playback.Add(1)
			if !wait(r, backend.playbackGate) {
				return
			}
			if backend.status != 0 {
				w.WriteHeader(backend.status)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := w.Write([]byte("healthy bytes")); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}
		case "/stop":
			backend.stops.Add(1)
			if !wait(r, backend.stopGate) {
				return
			}
			if backend.stopStatus != 0 {
				w.WriteHeader(backend.stopStatus)
				return
			}
			fmt.Fprint(w, `{"response":"ok"}`)
		}
	}))
	u, _ := url.Parse(server.URL)
	a := &Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()),
		Endpoint: MPEG_TS_ENDPOINT, NoResponseTimeout: time.Second, EmptyTimeout: time.Second}
	a.Init()
	t.Cleanup(func() { a.Shutdown(); server.Close() })
	return a
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lifecycle transition")
		}
		time.Sleep(time.Millisecond)
	}
}

type discardClient struct{ bytes atomic.Int64 }

func (w *discardClient) Write(p []byte) (int, error) {
	w.bytes.Add(int64(len(p)))
	return len(p), nil
}

func TestConcurrentAdmissionIncludesPendingStarts(t *testing.T) {
	backend := &admissionBackend{metadataGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	a.MaxConnections = 5
	var wg sync.WaitGroup
	results := make(chan *Admission, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := a.ReserveAdmission(AceID{id: "same"}, nil)
			if err == nil {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	status, _ := a.GetStatus(nil)
	if status.PendingAdmissions != 5 || a.ClientCount() != 0 {
		t.Fatalf("limit excludes pending admissions: %+v", status)
	}
	close(backend.metadataGate)
	for r := range results {
		if _, err := a.FetchReservedStream(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		r.Release()
		r.Release()
	}
	eventually(t, func() bool {
		status, _ := a.GetStatus(nil)
		return status.PendingAdmissions == 0 && status.PendingCleanup == 0
	})
}

func TestConcurrentChannelAdmission(t *testing.T) {
	backend := &admissionBackend{metadataGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	a.MaxConcurrentChannels = 3
	var wg sync.WaitGroup
	results := make(chan *Admission, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := a.ReserveAdmission(AceID{id: fmt.Sprint(i)}, nil)
			if err == nil {
				results <- r
			}
		}(i)
	}
	wg.Wait()
	close(results)
	if a.StreamCount() != 3 || len(results) != 3 {
		t.Fatalf("channel reservations exceeded limit: %d streams, %d reservations", a.StreamCount(), len(results))
	}
	for r := range results {
		r.Release()
	}
	close(backend.metadataGate)
}

func TestConcurrentJoinStartsOneBackend(t *testing.T) {
	backend := &admissionBackend{metadataGate: make(chan struct{}), playbackGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	const clients = 20
	reservations := make([]*Admission, clients)
	for i := range reservations {
		var err error
		reservations[i], err = a.ReserveAdmission(AceID{id: "same"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer reservations[i].Release()
	}
	close(backend.metadataGate)
	var wg sync.WaitGroup
	for _, r := range reservations {
		wg.Add(1)
		go func(r *Admission) {
			defer wg.Done()
			stream, err := a.FetchReservedStream(context.Background(), r)
			if err == nil {
				err = a.StartReservedStream(context.Background(), r, stream, &discardClient{})
			}
			if err != nil {
				t.Errorf("join: %v", err)
			}
		}(r)
	}
	eventually(t, func() bool { return a.ClientCount() == clients })
	close(backend.playbackGate)
	wg.Wait()
	if backend.metadata.Load() != 1 || backend.playback.Load() != 1 {
		t.Fatalf("duplicated backend startup: metadata=%d playback=%d", backend.metadata.Load(), backend.playback.Load())
	}
}

func TestCancelledFirstWaiterDoesNotCancelOtherViewers(t *testing.T) {
	for _, stage := range []string{"metadata", "playback"} {
		t.Run(stage, func(t *testing.T) {
			backend := &admissionBackend{}
			gate := make(chan struct{})
			if stage == "metadata" {
				backend.metadataGate = gate
			} else {
				backend.playbackGate = gate
			}
			a := newAdmissionBackend(t, backend)
			first, _ := a.ReserveAdmission(AceID{id: "shared"}, nil)
			second, _ := a.ReserveAdmission(AceID{id: "shared"}, nil)
			defer first.Release()
			defer second.Release()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				stream, err := a.FetchReservedStream(ctx, first)
				if err == nil {
					err = a.StartReservedStream(ctx, first, stream, &discardClient{})
				}
				result <- err
			}()
			if stage == "metadata" {
				eventually(t, func() bool { return backend.metadata.Load() == 1 })
			} else {
				eventually(t, func() bool { return backend.playback.Load() == 1 })
			}
			cancel()
			if err := <-result; err == nil {
				t.Fatal("cancelled first waiter succeeded")
			}
			close(gate)
			stream, err := a.FetchReservedStream(context.Background(), second)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.StartReservedStream(context.Background(), second, stream, &discardClient{}); err != nil {
				t.Fatal(err)
			}
			if backend.metadata.Load() != 1 || backend.playback.Load() != 1 {
				t.Fatal("remaining viewer needed duplicate startup")
			}
		})
	}
}

func TestAdmissionReleasedOnEveryStartupError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		status  int
		cancel  bool
		timeout bool
	}{
		{name: "malformed", body: "{"},
		{name: "middleware error", body: `{"error":"bad"}`},
		{name: "missing URLs", body: `{"response":{}}`},
		{name: "playback failure", status: 500},
		{name: "cancel", cancel: true},
		{name: "timeout", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &admissionBackend{body: tc.body, status: tc.status}
			if tc.timeout {
				backend.metadataGate = make(chan struct{})
			}
			a := newAdmissionBackend(t, backend)
			if tc.timeout {
				a.NoResponseTimeout = 10 * time.Millisecond
			}
			a.MaxConnections = 1
			r, err := a.ReserveAdmission(AceID{id: "failure"}, nil)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			stream, err := a.FetchReservedStream(ctx, r)
			if err == nil {
				err = a.StartReservedStream(ctx, r, stream, &discardClient{})
			}
			r.Release()
			if err == nil {
				t.Fatal("startup unexpectedly succeeded")
			}
			eventually(t, func() bool {
				status, _ := a.GetStatus(nil)
				// A dispatched metadata failure without a command identity must
				// retain cleanup uncertainty, but never consumes client capacity.
				return a.ClientCount() == 0 && status.PendingAdmissions == 0 && a.StreamCount() == 0
			})
		})
	}
}

func TestFailedBackendCleanupRemainsVisibleWithoutStoppingHealthyStream(t *testing.T) {
	backend := &admissionBackend{stopStatus: 500}
	a := newAdmissionBackend(t, backend)
	stream, err := a.FetchStream(AceID{id: "failed cleanup"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.releaseStream(stream); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return backend.stops.Load() == 2 })
	healthy, err := a.FetchStream(AceID{id: "healthy"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &discardClient{}
	if err := a.StartStream(healthy, writer); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return writer.bytes.Load() > 0 })
	status, _ := a.GetStatus(nil)
	if status.PendingCleanup != 1 || *status.TotalClients != 1 {
		t.Fatalf("failed cleanup confused with healthy viewer: %+v", status)
	}
	before := writer.bytes.Load()
	eventually(t, func() bool { return writer.bytes.Load() > before })
	if backend.stops.Load() != 2 {
		t.Fatal("stop retries were not bounded")
	}
}

func TestOldSessionCleanupCannotReleaseReplacement(t *testing.T) {
	backend := &admissionBackend{stopGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	id := AceID{id: "replace"}
	old, err := a.FetchStream(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldWriter := &discardClient{}
	if err := a.StartStream(old, oldWriter); err != nil {
		t.Fatal(err)
	}
	a.StopStream(old, oldWriter)
	eventually(t, func() bool { return backend.stops.Load() == 1 })
	replacement, err := a.FetchStream(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &discardClient{}
	if err := a.StartStream(replacement, w); err != nil {
		t.Fatal(err)
	}
	a.StopStream(old, w)
	if err := a.StartStream(old, &discardClient{}); err == nil {
		t.Fatal("old pointer started replacement")
	}
	if a.WaitStream(old) != nil {
		t.Fatal("old wait returned replacement lifetime")
	}
	close(backend.stopGate)
	before := w.bytes.Load()
	eventually(t, func() bool { return w.bytes.Load() > before })
	if a.ClientCount() != 1 {
		t.Fatal("late cleanup changed replacement ownership")
	}
}

func TestMaintenanceAdmissionAndCleanupGate(t *testing.T) {
	backend := &admissionBackend{stopGate: make(chan struct{})}
	a := newAdmissionBackend(t, backend)
	r, _ := a.ReserveAdmission(AceID{id: "live"}, nil)
	stream, err := a.FetchReservedStream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetMaintenance("quiescing", nil); err != nil {
		t.Fatal(err)
	}
	join, err := a.ReserveAdmission(stream.ID, nil)
	if err != nil {
		t.Fatal("quiesce rejected existing-channel join", err)
	}
	if _, err := a.ReserveAdmission(AceID{id: "new"}, nil); err == nil {
		t.Fatal("quiesce admitted new channel")
	}
	if err := a.SetMaintenance("maintenance", nil); err == nil {
		t.Fatal("maintenance ignored pending admissions")
	}
	r.Release()
	join.Release()
	eventually(t, func() bool { return backend.stops.Load() == 1 })
	if err := a.SetMaintenance("maintenance", nil); err == nil {
		t.Fatal("maintenance ignored pending cleanup")
	}
	close(backend.stopGate)
	eventually(t, func() bool { status, _ := a.GetStatus(nil); return status.PendingCleanup == 0 })
	if err := a.SetMaintenance("maintenance", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReserveAdmission(stream.ID, nil); err == nil {
		t.Fatal("maintenance admitted viewer")
	}
	if err := a.SetMaintenance("quiescing", nil); err == nil {
		t.Fatal("quiesce reopened maintenance")
	}
	if err := a.SetMaintenance("open", nil); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceEnterPublishesBeforeConcurrentAdmission(t *testing.T) {
	backend := &admissionBackend{}
	a := newAdmissionBackend(t, backend)
	persistStarted, finishPersistence := make(chan struct{}), make(chan struct{})
	transition := make(chan error, 1)
	go func() {
		transition <- a.SetMaintenance("maintenance", func(string) error {
			close(persistStarted)
			<-finishPersistence
			return nil
		})
	}()
	<-persistStarted
	admission := make(chan error, 1)
	go func() {
		r, err := a.ReserveAdmission(AceID{id: "racing"}, nil)
		if r != nil {
			r.Release()
		}
		admission <- err
	}()
	close(finishPersistence)
	if err := <-transition; err != nil {
		t.Fatal(err)
	}
	if err := <-admission; err == nil {
		t.Fatal("admission crossed the durable maintenance transition")
	}
	if backend.metadata.Load() != 0 {
		t.Fatal("maintenance allowed backend traffic")
	}
}

var _ io.Writer = (*discardClient)(nil)
