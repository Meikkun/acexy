// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
package acexy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// As of how the middleware is defined, we tell Go the structure that should match the HTTP
// response for AceStream: https://docs.acestream.net/developers/start-playback/#using-middleware.
// We are interested in the "playback_url" and the "command_url" fields: The first one
// references the stream, and the second one tells the stream to finish.
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
	Clients               *uint  `json:"clients,omitempty"`
	Streams               *uint  `json:"streams,omitempty"`
	ID                    *AceID `json:"stream_id,omitempty"`
	StatURL               string `json:"stat_url,omitempty"`
	MaxConnections        int    `json:"max_connections,omitempty"`
	MaxConcurrentChannels int    `json:"max_concurrent_channels,omitempty"`
	TotalClients          *uint  `json:"total_clients,omitempty"`
}

// ErrLimitReached indicates a connection or channel limit has been reached.
type ErrLimitReached struct {
	Message string
}

func (e *ErrLimitReached) Error() string {
	return e.Message
}

// The stream information is stored in a structure referencing the `AceStreamResponse`
// plus some extra information to determine whether we should keep the stream alive or not.
type AceStream struct {
	PlaybackURL string
	StatURL     string
	CommandURL  string
	ID          AceID
}

type ongoingStream struct {
	clients     uint
	done        chan struct{}
	closeDone   sync.Once // guards close(done) against concurrent callers
	player      *http.Response
	stream      *AceStream
	copier      *Copier
	broadcaster *Broadcaster
	evicted     map[io.Writer]struct{} // writers evicted by broadcaster
}

// Structure referencing the AceStream Proxy - this is, ourselves
type Acexy struct {
	Scheme                string        // The scheme to be used when connecting to the AceStream middleware
	Host                  string        // The host to be used when connecting to the AceStream middleware
	Port                  int           // The port to be used when connecting to the AceStream middleware
	Endpoint              AcexyEndpoint // The endpoint to be used when connecting to the AceStream middleware
	EmptyTimeout          time.Duration // Timeout after which, if no data is written, the stream is closed
	BufferSize            int           // Deprecated: no longer controls streaming output buffering
	NoResponseTimeout     time.Duration // Timeout to wait for a response from the AceStream middleware
	WriteTimeout          time.Duration // Timeout for writing to a client before eviction
	ClientQueueSize       int           // Per-client queue size for the broadcaster
	MaxConnections        int           // Maximum concurrent streaming clients (0 = unlimited)
	MaxConcurrentChannels int           // Maximum distinct broadcasts (0 = unlimited)

	// Information about ongoing streams
	streams    map[AceID]*ongoingStream
	mutex      *sync.Mutex
	middleware *http.Client
}

type AcexyEndpoint string

// The AceStream API available endpoints
const (
	M3U8_ENDPOINT    AcexyEndpoint = "/ace/manifest.m3u8"
	MPEG_TS_ENDPOINT AcexyEndpoint = "/ace/getstream"
)

// Initializes the Acexy structure
func (a *Acexy) Init() {
	a.streams = make(map[AceID]*ongoingStream)
	a.mutex = &sync.Mutex{}
	// The transport to be used when connecting to the AceStream middleware. We have to tweak it
	// a little bit to avoid compression and to limit the number of connections per host. Otherwise,
	// the AceStream Middleware won't work.
	a.middleware = &http.Client{
		Transport: &http.Transport{
			DisableCompression:    true,
			MaxIdleConns:          10,
			MaxConnsPerHost:       10,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: a.NoResponseTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Starts a new stream. The stream is enqueued in the AceStream backend, returning a playback
// URL to reproduce it and a command URL to finish it. If the stream is already enqueued,
// the playback URL is returned. A number of clients can be reproducing the same stream at
// the same time through the middleware. When the last client finishes, the stream is removed.
// The stream is identified by the “id“ identifier. Optionally, takes extra parameters to
// customize the stream.
func (a *Acexy) FetchStream(aceId AceID, extraParams url.Values) (*AceStream, error) {
	a.mutex.Lock()

	// Check if the stream is already enqueued (reusing = no new channel)
	if stream, ok := a.streams[aceId]; ok {
		slog.Info("Reusing existing", "stream", aceId, "clients", stream.clients)
		a.mutex.Unlock()
		return stream.stream, nil
	}

	// Check channel limit before creating a new stream
	if a.MaxConcurrentChannels > 0 && uint(len(a.streams)) >= uint(a.MaxConcurrentChannels) {
		a.mutex.Unlock()
		return nil, &ErrLimitReached{Message: fmt.Sprintf("maximum concurrent channels reached (%d active)", len(a.streams))}
	}

	// Release the mutex BEFORE the HTTP call to the AceStream backend.
	// GetStream can take up to NoResponseTimeout (10s) and would block
	// all other operations if the mutex were held.
	a.mutex.Unlock()

	middleware, err := GetStream(a, aceId, extraParams)
	if err != nil {
		slog.Error("Error getting stream middleware", "error", err)
		return nil, err
	}

	// Re-acquire the mutex to update state
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Another goroutine may have created this stream while the mutex was released
	if existing, ok := a.streams[aceId]; ok {
		slog.Info("Reusing existing (created during fetch)", "stream", aceId, "clients", existing.clients)
		// Close the duplicate backend stream to prevent leak
		go func(commandURL string) {
			dup := &AceStream{CommandURL: commandURL}
			if err := CloseStream(dup); err != nil {
				slog.Debug("Error closing duplicate backend stream", "error", err)
			}
		}(middleware.Response.CommandURL)
		return existing.stream, nil
	}

	// We got the stream information, build the structure around it and register the stream
	slog.Debug("Middleware Information", "id", aceId, "middleware", middleware)
	stream := &AceStream{
		PlaybackURL: middleware.Response.PlaybackURL,
		StatURL:     middleware.Response.StatURL,
		CommandURL:  middleware.Response.CommandURL,
		ID:          aceId,
	}

	queueSize := a.ClientQueueSize
	if queueSize <= 0 {
		queueSize = 64
	}
	broadcaster := NewBroadcaster(queueSize)
	os := &ongoingStream{
		clients:     0,
		done:        make(chan struct{}),
		player:      nil,
		stream:      stream,
		broadcaster: broadcaster,
		evicted:     make(map[io.Writer]struct{}),
	}
	a.streams[aceId] = os

	// When the broadcaster evicts a slow consumer, record it and decrement the
	// client count. StopStream checks this set to avoid double-decrementing.
	broadcaster.SetOnEvict(func(w io.Writer) {
		a.mutex.Lock()
		os.evicted[w] = struct{}{}
		if os.clients > 0 {
			os.clients--
			slog.Info("Writer evicted by timeout", "stream", aceId, "clients", os.clients)
		}
		shouldRelease := os.clients == 0
		a.mutex.Unlock()

		if shouldRelease {
			if err := a.releaseStream(stream); err != nil {
				slog.Warn("Error releasing stream after eviction", "error", err)
			}
		}
	})

	slog.Info("Started new stream", "id", aceId, "clients", os.clients)
	return stream, nil
}

func (a *Acexy) StartStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		a.mutex.Unlock()
		slog.Debug("Stream not found", "stream", stream.ID)
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}

	// Add the writer to the broadcaster
	ongoingStream.broadcaster.Add(out)

	// Register the new client
	ongoingStream.clients++

	// Check if the stream is already being played
	if ongoingStream.player != nil {
		a.mutex.Unlock()
		return nil
	}

	// Release the mutex BEFORE the HTTP call to the AceStream backend.
	// This call can take up to NoResponseTimeout (10s) and would block
	// all other operations (FetchStream, StopStream, GetStatus) if the
	// mutex were held.
	playbackURL := stream.PlaybackURL
	a.mutex.Unlock()

	resp, err := a.middleware.Get(playbackURL)

	// Re-acquire the mutex to update state
	a.mutex.Lock()

	// Re-check the stream still exists (could have been released while
	// the mutex was not held). If the stream was released, our writer and
	// client count were already cleaned up by releaseStream/StopStream.
	ongoingStream, ok = a.streams[stream.ID]
	if !ok {
		a.mutex.Unlock()
		if resp != nil {
			resp.Body.Close()
		}
		slog.Debug("Stream released during playback fetch", "stream", stream.ID)
		return fmt.Errorf(`stream "%s" was released`, stream.ID)
	}

	if err != nil {
		slog.Error("Failed to forward stream", "error", err)
		// Remove the writer we added before the HTTP call — if we don't,
		// the copier (started by another client) will try to write to
		// this client's now-invalid ResponseWriter, causing a nil pointer
		// dereference panic.
		ongoingStream.broadcaster.Remove(out)
		ongoingStream.clients--
		shouldRelease := ongoingStream.clients == 0
		a.mutex.Unlock()
		if shouldRelease {
			if releaseErr := a.releaseStream(stream); releaseErr != nil {
				slog.Warn("Error releasing stream", "error", releaseErr)
			}
		}
		return err
	}

	// Another goroutine may have started playback while the mutex was released
	if ongoingStream.player != nil {
		a.mutex.Unlock()
		resp.Body.Close()
		return nil
	}

	// Forward the response to the broadcaster
	ongoingStream.copier = &Copier{
		Destination:  ongoingStream.broadcaster,
		Source:       resp.Body,
		EmptyTimeout: a.EmptyTimeout,
		BufferSize:   a.BufferSize,
	}

	go func() {
		// Start copying the stream
		if err := ongoingStream.copier.Copy(); err != nil {
			if errors.Is(err, net.ErrClosed) {
				slog.Debug("Connection closed", "stream", stream.ID)
			} else {
				slog.Debug("Failed to copy response body", "stream", stream.ID, "error", err)
			}
		}
		slog.Debug("Copy done", "stream", stream.ID)
		ongoingStream.closeDone.Do(func() {
			close(ongoingStream.done)
			slog.Debug("Stream closed", "stream", stream.ID)
		})
	}()

	ongoingStream.player = resp
	a.mutex.Unlock()
	return nil
}

// Releases a stream that is no longer being used. The stream is removed from the AceStream backend.
// If the stream is not enqueued, an error is returned. If the stream has clients reproducing it,
// the stream is not removed. The stream is identified by the "id" identifier.
//
// This function acquires the mutex internally. Callers must NOT hold the mutex.
func (a *Acexy) releaseStream(stream *AceStream) error {
	a.mutex.Lock()
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		a.mutex.Unlock()
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}
	if ongoingStream.clients > 0 {
		a.mutex.Unlock()
		return fmt.Errorf(`stream "%s" has clients`, stream.ID)
	}

	// Remove the stream from the list while holding the lock
	delete(a.streams, stream.ID)
	a.mutex.Unlock()

	slog.Debug("Stopping stream", "stream", stream.ID)

	// Close broadcaster and player synchronously so the copier exits immediately
	ongoingStream.broadcaster.Close()
	if ongoingStream.player != nil {
		slog.Debug("Closing player", "stream", stream.ID)
		ongoingStream.player.Body.Close()
	}

	// Backend cleanup can take time; we already released the mutex
	if err := CloseStream(stream); err != nil {
		slog.Debug("Error closing stream", "error", err)
	}

	ongoingStream.closeDone.Do(func() {
		close(ongoingStream.done)
		slog.Debug("Stream done", "stream", stream.ID)
	})

	return nil
}

// Finishes a stream. The stream is removed from the AceStream backend. If the stream is not
// enqueued, an error is returned. If the stream has clients reproducing it, the stream is not
// removed. The stream is identified by the "id" identifier.
func (a *Acexy) StopStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		a.mutex.Unlock()
		slog.Debug("Stream not found", "stream", stream.ID)
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}

	// Remove the writer from the broadcaster
	ongoingStream.broadcaster.Remove(out)

	// If this writer was already evicted by the broadcaster, the client
	// count was already decremented in the OnEvict callback. Skip the
	// decrement to avoid double-counting.
	if _, wasEvicted := ongoingStream.evicted[out]; wasEvicted {
		delete(ongoingStream.evicted, out)
		a.mutex.Unlock()
		slog.Debug("Writer was already evicted, skipping decrement", "stream", stream.ID)
		return nil
	}

	// Unregister the client
	if ongoingStream.clients > 0 {
		ongoingStream.clients--
		slog.Info("Client stopped", "stream", stream.ID, "clients", ongoingStream.clients)
	} else {
		slog.Warn("Stream has no clients", "stream", stream.ID)
	}

	// Check if we have to stop the stream
	shouldRelease := ongoingStream.clients == 0
	a.mutex.Unlock()

	if shouldRelease {
		if err := a.releaseStream(stream); err != nil {
			slog.Warn("Error releasing stream", "error", err)
			return err
		}
		slog.Info("Stream done", "stream", stream.ID)
	}
	return nil
}

// Waits for the stream to finish. The stream is identified by the “id“ identifier. If the stream
// is not enqueued, nil is returned. The function returns a channel that will be closed when the
// stream finishes.
func (a *Acexy) WaitStream(stream *AceStream) <-chan struct{} {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		return nil
	}

	return ongoingStream.done
}

// Performs a request to the AceStream backend to start a new stream. It uses the Acexy
// structure to get the host and port of the AceStream backend. The stream is identified
// by the “id“ identifier. Optionally, takes extra parameters to customize the stream.
// Returns the response from the AceStream backend. If the request fails, an error is returned.
// If the `AceStreamMiddleware:error` field is not empty, an error is returned.
func GetStream(a *Acexy, aceId AceID, extraParams url.Values) (*AceStreamMiddleware, error) {
	slog.Debug("Getting stream", "id", aceId, "extraParams", extraParams)
	slog.Debug("Acexy Information", "scheme", a.Scheme, "host", a.Host, "port", a.Port)
	req, err := http.NewRequest("GET", a.Scheme+"://"+a.Host+":"+strconv.Itoa(a.Port)+string(a.Endpoint), nil)
	if err != nil {
		return nil, err
	}

	// Add the query parameters. We use a unique PID to identify the client accessing the stream.
	// This prevents errors when multiple streams are accessed at the same time. Because of
	// using the UUID package, we can be sure that the PID is unique.
	pid := uuid.NewString()
	slog.Debug("Temporary PID", "pid", pid, "stream", aceId)
	if extraParams == nil {
		extraParams = req.URL.Query()
	}
	idType, id := aceId.ID()
	extraParams.Set(string(idType), id)
	extraParams.Set("format", "json")
	extraParams.Set("pid", pid)
	// and set the headers
	req.Header.Set("Content-Type", "application/json")
	req.URL.RawQuery = extraParams.Encode()

	slog.Debug("Request URL", "url", req.URL.String())
	client := &http.Client{
		Timeout: a.NoResponseTimeout,
	}
	res, err := client.Do(req)
	if err != nil {
		slog.Debug("Error getting stream", "error", err)
		return nil, err
	}
	slog.Debug("Stream response", "statusCode", res.StatusCode, "headers", res.Header, "res", res)
	defer res.Body.Close()

	// Read the response into the body
	body, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Debug("Error reading stream response", "error", err)
		return nil, err
	}

	slog.Debug("Stream response", "response", string(body))
	var response AceStreamMiddleware
	if err := json.Unmarshal(body, &response); err != nil {
		slog.Debug("Error unmarshalling stream response", "error", err)
		return nil, err
	}

	if response.Error != "" {
		slog.Debug("Error in stream response", "error", response.Error)
		return nil, errors.New(response.Error)
	}
	return &response, nil
}

// Closes the stream by performing a request to the AceStream backend. The `stream` parameter
// contains the command URL to send data to the middleware. As of the documentation, it is needed
// to add a "method=stop" query parameter to finish the stream.
func CloseStream(stream *AceStream) error {
	req, err := http.NewRequest("GET", stream.CommandURL, nil)
	if err != nil {
		return err
	}

	q := req.URL.Query()
	q.Add("method", "stop")
	req.URL.RawQuery = q.Encode()

	client := &http.Client{
		/* the backend should respond in way less time than this one, but it may hang (AceStream)
		 * is not very well coded), so we set a timeout to avoid hanging forever
		 */
		Timeout: 3 * time.Second,
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// Read the response into the body
	body, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Debug("Error reading stream response", "error", err)
		return err
	}

	var response AceStreamCommand
	if err := json.Unmarshal(body, &response); err != nil {
		slog.Debug("Error unmarshalling stream response", "error", err)
		return err
	}

	if response.Error != "" {
		slog.Debug("Error in stream response", "error", response.Error)
		return errors.New(response.Error)
	}
	return nil
}

// Gets the status of a stream. If the `id` parameter is nil, the global status is returned.
// If the stream is not enqueued, an error is returned. The stream is identified by the “id“
// identifier.
func (a *Acexy) GetStatus(id *AceID) (AcexyStatus, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Return the global status if no ID is given
	if id == nil {
		totalClients := uint(0)
		for _, s := range a.streams {
			totalClients += s.clients
		}
		streams := uint(len(a.streams))
		return AcexyStatus{
			Streams:               &streams,
			TotalClients:          &totalClients,
			MaxConnections:        a.MaxConnections,
			MaxConcurrentChannels: a.MaxConcurrentChannels,
		}, nil
	}

	// Check if the stream is already enqueued
	if stream, ok := a.streams[*id]; ok {
		return AcexyStatus{
			Clients: &stream.clients,
			ID:      id,
			StatURL: stream.stream.StatURL,
		}, nil
	}

	return AcexyStatus{}, fmt.Errorf(`stream "%s" not found`, id)
}

// ClientCount returns the total number of active streaming clients across all streams.
func (a *Acexy) ClientCount() uint {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	var count uint
	for _, s := range a.streams {
		count += s.clients
	}
	return count
}

// StreamCount returns the number of distinct active streams.
func (a *Acexy) StreamCount() uint {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	return uint(len(a.streams))
}

// Shutdown releases all active streams and their backend sessions.
// It must NOT be called while holding a.mutex.
func (a *Acexy) Shutdown() {
	a.mutex.Lock()
	streams := make([]*ongoingStream, 0, len(a.streams))
	for _, os := range a.streams {
		streams = append(streams, os)
	}
	a.streams = make(map[AceID]*ongoingStream)
	a.mutex.Unlock()

	for _, os := range streams {
		slog.Info("Shutting down stream", "stream", os.stream.ID)
		os.broadcaster.Close()
		if os.player != nil {
			os.player.Body.Close()
		}
		if err := CloseStream(os.stream); err != nil {
			slog.Debug("Error closing stream during shutdown", "error", err)
		}
		os.closeDone.Do(func() {
			close(os.done)
		})
	}
}
