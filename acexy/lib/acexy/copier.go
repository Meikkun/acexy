package acexy

import (
	"bufio"
	"io"
	"log/slog"
	"sync"
	"time"
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
	// The buffer size to use when copying the data.
	BufferSize int

	/**! Private Data */
	mu             sync.Mutex
	timer          *time.Timer
	bufferedWriter *bufio.Writer
	closed         bool
}

// Starts copying the data from the source to the destination.
func (c *Copier) Copy() error {
	c.mu.Lock()
	c.bufferedWriter = bufio.NewWriterSize(c.Destination, c.BufferSize)
	c.timer = time.NewTimer(c.EmptyTimeout)
	c.closed = false
	c.mu.Unlock()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			c.mu.Lock()
			if c.closed || c.timer == nil {
				c.mu.Unlock()
				return
			}
			if !c.timer.Stop() {
				select {
				case <-c.timer.C:
				default:
				}
			}
			c.timer.Reset(c.EmptyTimeout)
			c.mu.Unlock()

			select {
			case <-done:
				slog.Debug("Done copying")
				return
			case <-c.timer.C:
				// On timeout, flush the buffer, and close both the source and the destination
				c.mu.Lock()
				if c.bufferedWriter != nil {
					c.bufferedWriter.Flush()
				}
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

	// Flush on normal completion
	c.mu.Lock()
	if !c.closed && c.bufferedWriter != nil {
		c.bufferedWriter.Flush()
	}
	c.mu.Unlock()

	return err
}

// Write writes the data to the destination. It also resets the timer if there is data to write.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.timer == nil || c.bufferedWriter == nil {
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
	// Write the data to the destination
	return c.bufferedWriter.Write(p)
}
