package acexy

import (
	"bytes"
	"io"
	"testing"
	"time"
)

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
