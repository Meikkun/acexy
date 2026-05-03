package acexy

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// BroadcastChunk is a single chunk of stream data to be distributed to clients.
type BroadcastChunk struct {
	Data []byte
}

// ClientWriter represents a single client connected to a stream.
type ClientWriter struct {
	id        string
	out       io.Writer
	queue     chan BroadcastChunk
	done      chan struct{}
	closed    atomic.Bool
	evicted   atomic.Bool
	closeOnce sync.Once
}

// Broadcaster distributes stream chunks to multiple clients using per-client
// bounded queues. A slow client cannot block the backend or other clients.
type Broadcaster struct {
	mu        sync.RWMutex
	clients   map[io.Writer]*ClientWriter
	closed    bool
	queueSize int
	onEvict   func(io.Writer)
}

// NewBroadcaster creates a new Broadcaster with the given per-client queue size.
func NewBroadcaster(queueSize int) *Broadcaster {
	return &Broadcaster{
		clients:   make(map[io.Writer]*ClientWriter),
		queueSize: queueSize,
	}
}

// SetOnEvict sets a callback that is called when a client is evicted.
func (b *Broadcaster) SetOnEvict(fn func(io.Writer)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onEvict = fn
}

// Add registers a new client writer. If the broadcaster is closed, this is a no-op.
func (b *Broadcaster) Add(out io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	if _, ok := b.clients[out]; ok {
		return
	}

	cw := &ClientWriter{
		id:    fmt.Sprintf("%p", out),
		out:   out,
		queue: make(chan BroadcastChunk, b.queueSize),
		done:  make(chan struct{}),
	}
	b.clients[out] = cw

	go cw.run(b.onEvict)
}

// Remove unregisters a client writer and closes its queue.
func (b *Broadcaster) Remove(out io.Writer) {
	b.mu.Lock()
	cw, ok := b.clients[out]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.clients, out)
	b.mu.Unlock()

	cw.close()
}

// Close closes the broadcaster and all client queues.
func (b *Broadcaster) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	clients := make([]*ClientWriter, 0, len(b.clients))
	for _, cw := range b.clients {
		clients = append(clients, cw)
	}
	b.clients = make(map[io.Writer]*ClientWriter)
	b.mu.Unlock()

	for _, cw := range clients {
		cw.close()
	}
	return nil
}

// Write distributes a chunk of data to all connected clients.
// It copies the data once and enqueues it to each client's queue.
// If a client's queue is full, the client is evicted.
// Returns success if at least one client accepted the chunk.
func (b *Broadcaster) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return 0, io.ErrClosedPipe
	}
	clients := make([]*ClientWriter, 0, len(b.clients))
	for _, cw := range b.clients {
		clients = append(clients, cw)
	}
	b.mu.RUnlock()

	if len(clients) == 0 {
		return len(p), nil
	}

	chunkData := append([]byte(nil), p...)
	chunk := BroadcastChunk{Data: chunkData}

	for _, cw := range clients {
		if cw.closed.Load() {
			continue
		}
		select {
		case cw.queue <- chunk:
		default:
			if !cw.closed.Load() {
				slog.Warn(
					"client queue full, evicting",
					"client_id", cw.id,
					"queue_size", b.queueSize,
				)
				b.evict(cw)
			}
		}
	}

	// If no client accepted the chunk, the data is dropped but the stream
	// continues. io.Copy treats any error as fatal, so we must return nil
	// to keep the backend connection alive.
	return len(p), nil
}

func (b *Broadcaster) evict(cw *ClientWriter) {
	b.mu.Lock()
	if _, ok := b.clients[cw.out]; ok {
		delete(b.clients, cw.out)
	}
	fn := b.onEvict
	b.mu.Unlock()

	go func() {
		if cw.evicted.CompareAndSwap(false, true) {
			cw.close()
			if fn != nil {
				fn(cw.out)
			}
		}
	}()
}

func (cw *ClientWriter) run(onEvict func(io.Writer)) {
	for {
		select {
		case chunk := <-cw.queue:
			_, err := cw.out.Write(chunk.Data)
			if err != nil {
				if !cw.closed.Load() && !cw.evicted.Load() {
					slog.Debug("client write error, evicting", "client_id", cw.id, "error", err)
					if onEvict != nil {
						onEvict(cw.out)
					}
				}
				return
			}
		case <-cw.done:
			return
		}
	}
}

func (cw *ClientWriter) close() {
	cw.closeOnce.Do(func() {
		cw.closed.Store(true)
		close(cw.done)
		if c, ok := cw.out.(io.Closer); ok {
			c.Close()
		}
	})
}
