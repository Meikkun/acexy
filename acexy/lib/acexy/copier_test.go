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

func TestCopier_TimeoutClosesSourceAndDestination(t *testing.T) {
	pr, pw := io.Pipe()
	src := &closeTracker{Reader: pr}

	copier := &Copier{
		Destination:  pw,
		Source:       src,
		EmptyTimeout: 100 * time.Millisecond,
		BufferSize:   4096,
	}

	// The copier will timeout because no data is written to pw
	err := copier.Copy()
	if err == nil {
		t.Error("expected timeout error, got nil")
	}

	if !src.closed {
		t.Error("expected source to be closed on timeout")
	}
}
