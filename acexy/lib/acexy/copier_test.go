package acexy

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

type recordingWriter struct {
	mu     sync.Mutex
	writes []int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, len(p))
	return len(p), nil
}

func maxWriteSize(writes []int) int {
	max := 0
	for _, n := range writes {
		if n > max {
			max = n
		}
	}
	return max
}

func TestCopier_FlushesOnEOF(t *testing.T) {
	var buf bytes.Buffer
	data := []byte("hello world")
	reader := bytes.NewReader(data)

	copier := &Copier{
		Destination:  &buf,
		Source:       reader,
		EmptyTimeout: 5 * time.Second,
		BufferSize:   4096,
	}

	err := copier.Copy()
	if err != nil {
		t.Fatalf("Copy failed: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("expected %q, got %q", data, buf.Bytes())
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

type blockingReader struct {
	closed chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.closed
	return 0, io.EOF
}

func (r *blockingReader) Close() error {
	close(r.closed)
	return nil
}

func TestCopier_TimeoutClosesSourceAndDestination(t *testing.T) {
	src := &blockingReader{closed: make(chan struct{})}
	var buf bytes.Buffer

	copier := &Copier{
		Destination:  &buf,
		Source:       src,
		EmptyTimeout: 100 * time.Millisecond,
		BufferSize:   4096,
	}

	done := make(chan error, 1)
	go func() {
		done <- copier.Copy()
	}()

	// Wait for the timeout to fire and close the source
	select {
	case <-done:
		// Copy returned (likely nil because EOF is not an error)
	case <-time.After(1 * time.Second):
		t.Fatal("copier did not return after timeout")
	}
}

// noWriteToReader wraps an io.Reader to hide the io.WriterTo optimization.
// io.CopyBuffer bypasses its buffer when the source implements io.WriterTo,
// which would make the test falsely report a giant burst.
type noWriteToReader struct {
	io.Reader
}

func TestCopier_DoesNotEmitBufferSizedBursts(t *testing.T) {
	sourceSize := 2 * 1024 * 1024
	bufferSize := 1024 * 1024

	src := &noWriteToReader{bytes.NewReader(make([]byte, sourceSize))}
	dst := &recordingWriter{}

	copier := &Copier{
		Destination:  dst,
		Source:       src,
		EmptyTimeout: 5 * time.Second,
		BufferSize:   bufferSize,
	}

	if err := copier.Copy(); err != nil {
		t.Fatalf("Copy failed: %v", err)
	}

	maxWrite := maxWriteSize(dst.writes)
	if maxWrite > DEFAULT_COPY_BUFFER_SIZE {
		t.Fatalf("write burst too large: got %d, want <= %d", maxWrite, DEFAULT_COPY_BUFFER_SIZE)
	}
}

func TestCopier_DirectCopyCopiesAllBytes(t *testing.T) {
	input := []byte("hello world direct copy test")
	src := bytes.NewReader(input)
	var dst bytes.Buffer

	copier := &Copier{
		Destination:  &dst,
		Source:       src,
		EmptyTimeout: 5 * time.Second,
		BufferSize:   4096,
	}

	if err := copier.Copy(); err != nil {
		t.Fatalf("Copy failed: %v", err)
	}

	if !bytes.Equal(input, dst.Bytes()) {
		t.Errorf("expected %q, got %q", input, dst.Bytes())
	}
}

func TestCopier_LargeConfiguredBufferDoesNotCreateLargeWrites(t *testing.T) {
	sourceSize := 10 * 1024 * 1024

	src := &noWriteToReader{bytes.NewReader(make([]byte, sourceSize))}
	dst := &recordingWriter{}

	copier := &Copier{
		Destination:  dst,
		Source:       src,
		EmptyTimeout: 5 * time.Second,
		BufferSize:   9 * 1024 * 1024,
	}

	if err := copier.Copy(); err != nil {
		t.Fatalf("Copy failed: %v", err)
	}

	// Total bytes copied must match input
	total := 0
	for _, n := range dst.writes {
		total += n
	}
	if total != sourceSize {
		t.Fatalf("expected total %d bytes, got %d", sourceSize, total)
	}

	maxWrite := maxWriteSize(dst.writes)
	if maxWrite > DEFAULT_COPY_BUFFER_SIZE {
		t.Fatalf("write burst too large: got %d, want <= %d", maxWrite, DEFAULT_COPY_BUFFER_SIZE)
	}
}
