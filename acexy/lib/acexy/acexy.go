// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
package acexy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type AceStreamResponse struct {
	PlaybackURL       string `json:"playback_url"`
	StatURL           string `json:"stat_url"`
	CommandURL        string `json:"command_url"`
	Infohash          string `json:"infohash"`
	PlaybackSessionID string `json:"playback_session_id"`
	IsLive            int    `json:"is_live"`
	IsEncrypted       int    `json:"is_encrypted"`
	ClientSessionID   int    `json:"client_session_id"`
}

type AceStreamMiddleware struct {
	Response AceStreamResponse `json:"response"`
	Error    string            `json:"error"`
}

type AceStreamCommand struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

type AcexyStatus struct {
	Clients               *uint              `json:"clients,omitempty"`
	Streams               *uint              `json:"streams,omitempty"`
	ID                    *AceID             `json:"stream_id,omitempty"`
	StatURL               string             `json:"stat_url,omitempty"`
	MaxConnections        int                `json:"max_connections,omitempty"`
	MaxConcurrentChannels int                `json:"max_concurrent_channels,omitempty"`
	TotalClients          *uint              `json:"total_clients,omitempty"`
	ActiveStreams         []ActiveStreamInfo `json:"active_streams,omitempty"`
	PendingAdmissions     uint               `json:"pending_admissions"`
	PendingStarts         uint               `json:"pending_starts"`
	PendingCleanup        uint               `json:"pending_cleanup"`
	Maintenance           string             `json:"maintenance"`
	MaintenancePhase      string             `json:"maintenance_phase"`
	AdmissionPaused       bool               `json:"admission_paused"`
}

type ActiveStreamInfo struct {
	ID       string `json:"id"`
	Infohash string `json:"infohash,omitempty"`
	Clients  uint   `json:"clients"`
}

type ErrLimitReached struct{ Message string }

func (e *ErrLimitReached) Error() string { return e.Message }

type AceStream struct {
	PlaybackURL string
	StatURL     string
	CommandURL  string
	ID          AceID
}

type clientRegistration struct {
	writer io.Writer
	ready  chan struct{}
}

// Once a connection is handed to the transport, cancellation cannot establish
// that the engine did not receive the request. Dial/TLS failures before this
// point are definite pre-dispatch failures and need no backend cleanup.
type metadataAttempt struct {
	mayHaveDispatched atomic.Bool
	rejected          bool
}

func (r *clientRegistration) Write(p []byte) (int, error) { return r.writer.Write(p) }

func (r *clientRegistration) Close() error {
	if closer, ok := r.writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (r *clientRegistration) CloseGracefully() error {
	if closer, ok := r.writer.(interface{ CloseGracefully() error }); ok {
		return closer.CloseGracefully()
	}
	return r.Close()
}

type ongoingStream struct {
	stream         *AceStream
	registrations  map[io.Writer]*clientRegistration
	reservations   map[*Admission]struct{}
	done           chan struct{}
	metadataDone   chan struct{}
	metadataErr    error
	start          chan struct{}
	startDone      chan struct{}
	startErr       error
	startRequested bool
	started        bool
	legacy         bool
	retired        bool
	ending         bool
	ctx            context.Context
	cancel         context.CancelFunc
	broadcaster    *Broadcaster
}

// Admission is an exact-once connection reservation. Its session also owns a
// channel slot, including while middleware metadata is still in flight.
type Admission struct {
	owner   *Acexy
	session *ongoingStream
	active  bool
}

func (r *Admission) Release() {
	if r == nil || r.owner == nil {
		return
	}
	a := r.owner
	a.mutex.Lock()
	if r.active {
		r.active = false
		delete(r.session.reservations, r)
		a.retireIfUnusedLocked(r.session)
	}
	a.mutex.Unlock()
}

type Acexy struct {
	Scheme                string
	Host                  string
	Port                  int
	Endpoint              AcexyEndpoint
	EmptyTimeout          time.Duration
	BufferSize            int // Deprecated.
	NoResponseTimeout     time.Duration
	WriteTimeout          time.Duration
	ClientQueueSize       int
	MaxConnections        int
	MaxConcurrentChannels int

	streams       map[AceID]*ongoingStream
	mutex         *sync.Mutex
	middleware    *http.Client
	cleanup       map[*ongoingStream]error
	workers       sync.WaitGroup
	shuttingDown  bool
	phase         string
	cleanupCtx    context.Context
	cleanupCancel context.CancelFunc
}

type AcexyEndpoint string

const (
	M3U8_ENDPOINT    AcexyEndpoint = "/ace/manifest.m3u8"
	MPEG_TS_ENDPOINT AcexyEndpoint = "/ace/getstream"
)

func (a *Acexy) Init() {
	a.streams = make(map[AceID]*ongoingStream)
	a.cleanup = make(map[*ongoingStream]error)
	a.mutex = &sync.Mutex{}
	a.phase = "open"
	a.cleanupCtx, a.cleanupCancel = context.WithCancel(context.Background())
	a.middleware = &http.Client{Transport: &http.Transport{
		// The engine can stall playback redirects on reused HTTP connections.
		DisableKeepAlives: true, DisableCompression: true, MaxConnsPerHost: 10,
		ResponseHeaderTimeout: a.startupTimeout(), ExpectContinueTimeout: time.Second,
	}}
}

func (a *Acexy) startupTimeout() time.Duration {
	if a.NoResponseTimeout > 0 {
		return a.NoResponseTimeout
	}
	return 10 * time.Second
}

func cloneValues(values url.Values) url.Values {
	copy := make(url.Values, len(values))
	for key, values := range values {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}

func (a *Acexy) countsLocked() (clients, pending uint) {
	for _, s := range a.streams {
		clients += uint(len(s.registrations))
		pending += uint(len(s.reservations))
	}
	return
}

func (a *Acexy) sessionLocked(id AceID, params url.Values) (*ongoingStream, error) {
	if a.shuttingDown || a.phase == "maintenance" {
		return nil, &ErrLimitReached{"admission paused"}
	}
	if s := a.streams[id]; s != nil {
		if s.ending {
			return nil, &ErrLimitReached{"stream is finishing"}
		}
		return s, nil
	}
	if a.phase == "quiescing" {
		return nil, &ErrLimitReached{"new channels paused"}
	}
	if a.MaxConcurrentChannels > 0 && len(a.streams) >= a.MaxConcurrentChannels {
		return nil, &ErrLimitReached{"maximum concurrent channels reached"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	queueSize := a.ClientQueueSize
	if queueSize <= 0 {
		queueSize = 64
	}
	s := &ongoingStream{
		stream:        &AceStream{ID: id},
		registrations: make(map[io.Writer]*clientRegistration),
		reservations:  make(map[*Admission]struct{}),
		done:          make(chan struct{}), metadataDone: make(chan struct{}),
		start: make(chan struct{}), startDone: make(chan struct{}),
		ctx: ctx, cancel: cancel, broadcaster: NewBroadcaster(queueSize),
	}
	s.broadcaster.SetOnEvict(func(w io.Writer) { a.removeClient(s, w, "eviction") })
	a.streams[id] = s
	a.workers.Add(1)
	go a.runSession(s, cloneValues(params))
	return s, nil
}

// ReserveAdmission atomically reserves connection and distinct-channel capacity,
// then starts one shared metadata request. Release must be called on every path.
func (a *Acexy) ReserveAdmission(id AceID, params url.Values) (*Admission, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	clients, pending := a.countsLocked()
	if a.MaxConnections > 0 && clients+pending >= uint(a.MaxConnections) {
		return nil, &ErrLimitReached{"maximum connections reached"}
	}
	s, err := a.sessionLocked(id, params)
	if err != nil {
		return nil, err
	}
	r := &Admission{owner: a, session: s, active: true}
	s.reservations[r] = struct{}{}
	return r, nil
}

// FetchReservedStream waits independently of other viewers' startup requests.
func (a *Acexy) FetchReservedStream(ctx context.Context, r *Admission) (*AceStream, error) {
	if r == nil || r.owner != a {
		return nil, errors.New("invalid admission")
	}
	select {
	case <-ctx.Done():
		r.Release()
		return nil, ctx.Err()
	case <-r.session.metadataDone:
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	s := r.session
	if s.metadataErr != nil {
		return nil, s.metadataErr
	}
	if !r.active || a.streams[s.stream.ID] != s {
		return nil, errors.New("admission released")
	}
	return s.stream, nil
}

// FetchStream preserves the metadata-only library API on the same session map.
// Such a session remains available until StartStream, releaseStream or Shutdown.
func (a *Acexy) FetchStream(id AceID, params url.Values) (*AceStream, error) {
	a.mutex.Lock()
	s, err := a.sessionLocked(id, params)
	if err == nil {
		s.legacy = true
	}
	a.mutex.Unlock()
	if err != nil {
		return nil, err
	}
	<-s.metadataDone
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return s.stream, s.metadataErr
}

func (a *Acexy) StartStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()
	s := a.streams[stream.ID]
	if s == nil || s.stream != stream {
		a.mutex.Unlock()
		return errors.New("stream not found")
	}
	clients, pending := a.countsLocked()
	if a.shuttingDown || a.phase == "maintenance" || (a.MaxConnections > 0 && clients+pending >= uint(a.MaxConnections)) {
		a.mutex.Unlock()
		return &ErrLimitReached{"admission unavailable"}
	}
	r := &Admission{owner: a, session: s, active: true}
	s.reservations[r] = struct{}{}
	a.mutex.Unlock()
	defer r.Release()
	return a.StartReservedStream(context.Background(), r, stream, out)
}

func (a *Acexy) StartReservedStream(ctx context.Context, r *Admission, stream *AceStream, out io.Writer) error {
	if r == nil || r.owner != a || stream == nil {
		return errors.New("invalid admission")
	}
	a.mutex.Lock()
	s := r.session
	if !r.active || s.stream != stream || a.streams[stream.ID] != s || a.shuttingDown || s.ending {
		a.mutex.Unlock()
		return errors.New("stream released")
	}
	if _, exists := s.registrations[out]; exists {
		a.mutex.Unlock()
		return errors.New("writer already registered")
	}
	r.active = false
	delete(s.reservations, r)
	s.legacy = false
	registration := &clientRegistration{writer: out, ready: make(chan struct{})}
	defer close(registration.ready)
	s.registrations[out] = registration
	// Add only creates a queue; it does not call user code or perform I/O.
	s.broadcaster.Add(registration)
	if !s.startRequested {
		s.startRequested = true
		close(s.start)
	}
	a.mutex.Unlock()
	select {
	case <-ctx.Done():
		a.removeClient(s, registration, "startup cancelled")
		return ctx.Err()
	case <-s.startDone:
	}
	a.mutex.Lock()
	err := s.startErr
	registered := s.registrations[out] == registration
	a.mutex.Unlock()
	if err != nil || !registered {
		a.removeClient(s, registration, "startup failed")
		if err == nil {
			err = errors.New("stream released during startup")
		}
		return err
	}
	return nil
}

// retireLocked publishes cleanup before releasing the channel slot. The worker
// owns all response bodies and engine cleanup, including late metadata results.
func (a *Acexy) retireLocked(s *ongoingStream) {
	if a.streams[s.stream.ID] != s || s.retired {
		return
	}
	s.retired = true
	delete(a.streams, s.stream.ID)
	a.cleanup[s] = nil
	for r := range s.reservations {
		r.active = false
		delete(s.reservations, r)
	}
	close(s.done)
	if s.cancel != nil {
		s.cancel()
	}
}

func (a *Acexy) retireIfUnusedLocked(s *ongoingStream) {
	if len(s.registrations) == 0 && len(s.reservations) == 0 && !s.legacy {
		a.retireLocked(s)
	}
}

func (a *Acexy) removeClient(expected *ongoingStream, writer io.Writer, reason string) {
	a.mutex.Lock()
	if a.streams[expected.stream.ID] != expected {
		a.mutex.Unlock()
		return
	}
	// Broadcaster callbacks carry the exact registration token, not just the
	// user writer, which may already have been attached again.
	var callback *clientRegistration
	if registration, ok := writer.(*clientRegistration); ok {
		callback = registration
		writer = registration.writer
	}
	registration, exists := expected.registrations[writer]
	if !exists || (callback != nil && callback != registration) {
		a.mutex.Unlock()
		return
	}
	delete(expected.registrations, writer)
	a.retireIfUnusedLocked(expected)
	a.mutex.Unlock()
	expected.broadcaster.Remove(registration)
}

func (a *Acexy) StopStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()
	s := a.streams[stream.ID]
	a.mutex.Unlock()
	if s != nil && s.stream == stream {
		a.removeClient(s, out, "disconnect")
	}
	return nil
}

func (a *Acexy) releaseStream(stream *AceStream) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	s := a.streams[stream.ID]
	if s == nil || s.stream != stream {
		return nil
	}
	if len(s.registrations) != 0 || len(s.reservations) != 0 {
		return errors.New("stream has clients or admissions")
	}
	a.retireLocked(s)
	return nil
}

func (a *Acexy) WaitStream(stream *AceStream) <-chan struct{} {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if s := a.streams[stream.ID]; s != nil && s.stream == stream {
		return s.done
	}
	return nil
}

func (a *Acexy) runSession(s *ongoingStream, params url.Values) {
	defer a.workers.Done()
	var player *http.Response
	metadataPublished, startPublished := false, false
	var runErr error
	var uncertainOwnership bool
	defer func() {
		a.mutex.Lock()
		s.ending = true
		if !metadataPublished {
			s.metadataErr = runErr
			close(s.metadataDone)
		}
		if !startPublished {
			s.startErr = runErr
			close(s.startDone)
		}
		a.mutex.Unlock()
		// Every client exits through the same exact-once operation.
		a.mutex.Lock()
		writers := make([]io.Writer, 0, len(s.registrations))
		for w := range s.registrations {
			writers = append(writers, w)
		}
		a.mutex.Unlock()
		for _, w := range writers {
			a.removeClient(s, w, "session finished")
		}
		a.mutex.Lock()
		a.retireLocked(s)
		a.mutex.Unlock()
		s.broadcaster.Close()
		if player != nil {
			player.Body.Close()
		}
		// Independent, bounded cleanup survives viewer cancellation.
		var cleanupErr error
		if uncertainOwnership {
			cleanupErr = errors.New("metadata dispatch has no confirmed backend cleanup identity")
		}
		if s.stream.CommandURL != "" {
			for attempt := 0; attempt < 2; attempt++ {
				ctx, cancel := context.WithTimeout(a.cleanupCtx, 3*time.Second)
				cleanupErr = closeStream(ctx, s.stream)
				cancel()
				if cleanupErr == nil || a.cleanupCtx.Err() != nil {
					break
				}
			}
		}
		a.mutex.Lock()
		if cleanupErr == nil {
			delete(a.cleanup, s)
		} else {
			a.cleanup[s] = cleanupErr
		}
		a.mutex.Unlock()
	}()

	attempt := &metadataAttempt{}
	ctx, cancel := context.WithTimeout(a.cleanupCtx, a.startupTimeout())
	stopEarlyCancellation := context.AfterFunc(s.ctx, func() {
		if !attempt.mayHaveDispatched.Load() {
			cancel()
		}
	})
	// After possible dispatch, allow the bounded metadata response to recover
	// the command URL even if all viewers leave. Shutdown still cancels this
	// request through cleanupCtx within its single overall budget.
	metadata, err := getStream(ctx, a, s.stream.ID, params, attempt)
	stopEarlyCancellation()
	cancel()
	uncertainOwnership = attempt.mayHaveDispatched.Load()
	a.mutex.Lock()
	if metadata != nil {
		s.stream.PlaybackURL = metadata.Response.PlaybackURL
		if validBackendURL(metadata.Response.CommandURL) {
			s.stream.CommandURL = metadata.Response.CommandURL
		}
		s.stream.StatURL = metadata.Response.StatURL
		if attempt.rejected && s.stream.CommandURL == "" {
			uncertainOwnership = false
		}
	}
	s.metadataErr = err
	metadataPublished = true
	close(s.metadataDone)
	a.mutex.Unlock()
	if err != nil {
		runErr = err
		return
	}
	select {
	case <-s.ctx.Done():
		runErr = s.ctx.Err()
		return
	case <-s.start:
	}
	// A header deadline bounds startup without limiting the playback lifetime.
	timer := time.AfterFunc(a.startupTimeout(), s.cancel)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.stream.PlaybackURL, nil)
	if err == nil {
		player, err = a.middleware.Do(req)
	}
	if !timer.Stop() {
		err = context.DeadlineExceeded
	}
	if err == nil && (player.StatusCode < 200 || player.StatusCode >= 300) {
		err = fmt.Errorf("playback HTTP status %d", player.StatusCode)
	}
	if err != nil {
		runErr = err
		return
	}
	a.mutex.Lock()
	if s.retired {
		a.mutex.Unlock()
		runErr = context.Canceled
		return
	}
	s.started = true
	startPublished = true
	close(s.startDone)
	a.mutex.Unlock()
	copier := &Copier{Destination: s.broadcaster, Source: player.Body, EmptyTimeout: a.EmptyTimeout, BufferSize: a.BufferSize}
	runErr = copier.Copy()
	if runErr == nil {
		a.mutex.Lock()
		s.ending = true
		registrations := make([]*clientRegistration, 0, len(s.registrations))
		for _, registration := range s.registrations {
			registrations = append(registrations, registration)
		}
		a.mutex.Unlock()
		timeout := a.WriteTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		drainCtx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		// Even an empty finite response must acknowledge startup before the
		// successful registration is retired.
		for _, registration := range registrations {
			select {
			case <-registration.ready:
			case <-drainCtx.Done():
				runErr = drainCtx.Err()
				return
			}
		}
		runErr = s.broadcaster.Drain(drainCtx)
	}
}

func GetStream(a *Acexy, id AceID, params url.Values) (*AceStreamMiddleware, error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.startupTimeout())
	defer cancel()
	return getStream(ctx, a, id, params, nil)
}

func validBackendURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func getStream(ctx context.Context, a *Acexy, id AceID, params url.Values, attempt *metadataAttempt) (*AceStreamMiddleware, error) {
	if attempt != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { attempt.mayHaveDispatched.Store(true) },
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Scheme+"://"+a.Host+":"+strconv.Itoa(a.Port)+string(a.Endpoint), nil)
	if err != nil {
		return nil, err
	}
	q := cloneValues(params)
	kind, value := id.ID()
	q.Set(string(kind), value)
	q.Set("format", "json")
	q.Set("pid", uuid.NewString())
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Content-Type", "application/json")
	res, err := a.middleware.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var response AceStreamMiddleware
	if err := decodeBackendJSON(res.Body, &response); err != nil {
		return &response, err
	}
	if attempt != nil && response.Error != "" {
		attempt.rejected = true
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &response, fmt.Errorf("middleware HTTP status %d", res.StatusCode)
	}
	if response.Error != "" {
		return &response, errors.New("middleware rejected stream")
	}
	if !validBackendURL(response.Response.PlaybackURL) || !validBackendURL(response.Response.CommandURL) {
		return &response, errors.New("invalid middleware playback or command URL")
	}
	return &response, nil
}

func CloseStream(stream *AceStream) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return closeStream(ctx, stream)
}

func decodeBackendJSON(body io.Reader, value any) error {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("backend response too large")
	}
	return json.Unmarshal(data, value)
}

func closeStream(ctx context.Context, stream *AceStream) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stream.CommandURL, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("method", "stop")
	req.URL.RawQuery = q.Encode()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("stop HTTP status %d", res.StatusCode)
	}
	var response AceStreamCommand
	if err := decodeBackendJSON(res.Body, &response); err != nil {
		return err
	}
	if response.Error != "" || response.Response == "" {
		return errors.New("middleware rejected stop")
	}
	return nil
}

func (a *Acexy) GetStatus(id *AceID) (AcexyStatus, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	total, pending := a.countsLocked()
	streams := uint(len(a.streams))
	status := AcexyStatus{
		PendingAdmissions: pending, PendingCleanup: uint(len(a.cleanup)),
		Maintenance: a.phase, MaintenancePhase: a.phase,
		AdmissionPaused: a.shuttingDown || a.phase != "open",
	}
	for _, s := range a.streams {
		if !s.started {
			status.PendingStarts++
		}
	}
	if id != nil {
		s := a.streams[*id]
		if s == nil {
			return AcexyStatus{}, errors.New("stream not found")
		}
		clients, copiedID := uint(len(s.registrations)), *id
		status.Clients, status.ID, status.StatURL = &clients, &copiedID, s.stream.StatURL
		return status, nil
	}
	status.Streams, status.TotalClients = &streams, &total
	status.MaxConnections, status.MaxConcurrentChannels = a.MaxConnections, a.MaxConcurrentChannels
	for id, s := range a.streams {
		info := ActiveStreamInfo{Clients: uint(len(s.registrations))}
		kind, value := id.ID()
		if kind == "id" {
			info.ID = value
		} else {
			info.Infohash = value
		}
		status.ActiveStreams = append(status.ActiveStreams, info)
	}
	return status, nil
}

func (a *Acexy) ClientCount() uint {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	count, _ := a.countsLocked()
	return count
}

func (a *Acexy) StreamCount() uint {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return uint(len(a.streams))
}

// ShutdownContext closes admission immediately and waits within the caller's
// single shutdown budget. Backend cleanup never extends that budget.
func (a *Acexy) ShutdownContext(ctx context.Context) error {
	defer a.middleware.CloseIdleConnections()
	stopCancellation := context.AfterFunc(ctx, a.cleanupCancel)
	defer stopCancellation()
	a.mutex.Lock()
	a.shuttingDown = true
	sessions := make([]*ongoingStream, 0, len(a.streams))
	for _, s := range a.streams {
		sessions = append(sessions, s)
	}
	a.mutex.Unlock()
	for _, s := range sessions {
		a.mutex.Lock()
		writers := make([]io.Writer, 0, len(s.registrations))
		for w := range s.registrations {
			writers = append(writers, w)
		}
		a.mutex.Unlock()
		for _, w := range writers {
			a.removeClient(s, w, "shutdown")
		}
		a.mutex.Lock()
		a.retireLocked(s)
		a.mutex.Unlock()
	}
	done := make(chan struct{})
	go func() { a.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		a.cleanupCancel()
		return ctx.Err()
	case <-done:
		a.mutex.Lock()
		unresolved := len(a.cleanup)
		a.mutex.Unlock()
		if unresolved != 0 {
			return fmt.Errorf("%d backend cleanups unresolved", unresolved)
		}
		return nil
	}
}

func (a *Acexy) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.ShutdownContext(ctx)
}

// SetMaintenance persists while holding the admission lock so no admission can
// slip between an idle check and a durable gate transition. persist performs
// only local state-file I/O, never backend requests or process control.
func (a *Acexy) SetMaintenance(phase string, persist func(string) error) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	switch phase {
	case "open", "quiescing":
		if phase == "quiescing" && a.phase == "maintenance" {
			return &ErrLimitReached{"maintenance requires explicit exit"}
		}
	case "maintenance":
		clients, pending := a.countsLocked()
		if clients != 0 || pending != 0 || len(a.streams) != 0 || len(a.cleanup) != 0 {
			return &ErrLimitReached{"maintenance requires idle sessions and cleanup"}
		}
	default:
		return errors.New("invalid maintenance phase")
	}
	if persist != nil {
		if err := persist(phase); err != nil {
			// A failed directory sync can mean the rename already happened.
			// Fail closed rather than admitting against uncertain durable state.
			a.phase = "maintenance"
			return err
		}
	}
	a.phase = phase
	return nil
}
