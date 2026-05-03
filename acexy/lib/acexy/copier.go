package acexy

import (
	"io"
	"log/slog"
	"sync"
	"time"
)

const (
	MPEGTS_PACKET_SIZE       = 188
	DEFAULT_COPY_PACKETS     = 256
	DEFAULT_COPY_BUFFER_SIZE = MPEGTS_PACKET_SIZE * DEFAULT_COPY_PACKETS
)

// Copier is an implementation that copies the data from the source to the destination.
// It has an empty timeout that is used to determine when the source is empty - this is,
// it has no more data to read after the timeout.
type Copier struct {
	// The destination to copy the data to.
	Destination io.Writer
	// The source to copy the data from.
	Source io.Reader
	// The timeout to use when the source is empty.
	EmptyTimeout time.Duration
	// BufferSize is deprecated. Streaming now uses a fixed TS-aligned copy buffer
	// to avoid large downstream write bursts.
	BufferSize int

	/**! Private Data */
	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

// Starts copying the data from the source to the destination.
func (c *Copier) Copy() error {
	slog.Debug("copy started", "copy_buffer_size", DEFAULT_COPY_BUFFER_SIZE)

	c.mu.Lock()
	c.timer = time.NewTimer(c.EmptyTimeout)
	c.closed = false
	c.mu.Unlock()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-done:
				slog.Debug("Done copying")
				return
			case <-c.timer.C:
				// On timeout, close both the source and the destination
				c.mu.Lock()
				c.closed = true
				c.mu.Unlock()
				if closer, ok := c.Source.(io.Closer); ok {
					slog.Debug("Closing source")
					closer.Close()
				}
				if closer, ok := c.Destination.(io.Closer); ok {
					slog.Debug("Closing destination")
					closer.Close()
				}
				return
			}
		}
	}()

	_, err := io.Copy(c, c.Source)

	return err
}

// Write writes the data to the destination. It also resets the timer if there is data to write.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	c.mu.Lock()
	if c.closed || c.timer == nil {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	// Reset the timer safely under the mutex
	if !c.timer.Stop() {
		select {
		case <-c.timer.C:
		default:
		}
	}
	c.timer.Reset(c.EmptyTimeout)
	dst := c.Destination
	c.mu.Unlock()

	// Copy the data before passing it to the destination. Some destinations
	// (e.g. io.Pipe) may hold a reference to the slice after Write returns,
	// which would race with io.Copy reusing its buffer.
	p2 := make([]byte, len(p))
	copy(p2, p)
	return dst.Write(p2)
}
