package pmw

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockWriter struct {
	mu      sync.Mutex
	blocked bool
}

func (b *blockWriter) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	b.blocked = true
	b.mu.Unlock()

	// Block forever
	select {}
}

type fastWriter struct {
	mu    sync.Mutex
	wrote int
}

func (f *fastWriter) Write(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrote += len(p)
	return len(p), nil
}

func TestSlowWriterEviction(t *testing.T) {
	fast := &fastWriter{}
	slow := &blockWriter{}

	timeout := 100 * time.Millisecond
	pmw := New(context.Background(), timeout, fast, slow)

	data := []byte("test data")
	n, err := pmw.Write(data)

	// Write must succeed because the fast writer succeeded
	if err != nil {
		t.Errorf("Expected no error from Write (fast writer succeeded), got: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected n=%d, got %d", len(data), n)
	}

	// The fast writer should have received data
	fast.mu.Lock()
	if fast.wrote != len(data) {
		t.Errorf("Fast writer should have received %d bytes, got %d", len(data), fast.wrote)
	}
	fast.mu.Unlock()

	// Give some time for the async eviction to happen
	time.Sleep(200 * time.Millisecond)

	pmw.RLock()
	defer pmw.RUnlock()
	if len(pmw.writers) != 1 {
		t.Errorf("Expected 1 writer after eviction, got %d", len(pmw.writers))
	}

	if pmw.writers[0] != fast {
		t.Error("Remaining writer should be the fast one")
	}
}

func TestOnEvictCallback(t *testing.T) {
	fast := &fastWriter{}
	slow := &blockWriter{}

	timeout := 100 * time.Millisecond
	pmw := New(context.Background(), timeout, fast, slow)

	var evicted atomic.Value
	pmw.SetOnEvict(func(w io.Writer) {
		evicted.Store(w)
	})

	data := []byte("test data")
	_, _ = pmw.Write(data)

	// Give time for eviction callback to fire
	time.Sleep(200 * time.Millisecond)

	ev := evicted.Load()
	if ev == nil {
		t.Fatal("OnEvict callback was not called")
	}
	if ev != slow {
		t.Error("OnEvict callback should have received the slow writer")
	}
}

func TestAllWritersFail(t *testing.T) {
	slow1 := &blockWriter{}
	slow2 := &blockWriter{}

	timeout := 100 * time.Millisecond
	pmw := New(context.Background(), timeout, slow1, slow2)

	data := []byte("test data")
	n, err := pmw.Write(data)

	if err == nil {
		t.Error("Expected error when all writers fail, got nil")
	}
	if n != 0 {
		t.Errorf("Expected n=0 when all writers fail, got %d", n)
	}

	_, ok := err.(PMultiWriterError)
	if !ok {
		t.Errorf("Expected PMultiWriterError, got %T", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	w := &fastWriter{}
	pmw := New(context.Background(), 1*time.Second, w)

	if err := pmw.Close(); err != nil {
		t.Fatalf("First Close() returned error: %v", err)
	}

	// Second Close() must not panic
	if err := pmw.Close(); err != nil {
		t.Fatalf("Second Close() returned error: %v", err)
	}
}

func TestNormalWrite(t *testing.T) {
	w1 := &fastWriter{}
	w2 := &fastWriter{}

	pmw := New(context.Background(), 1*time.Second, w1, w2)

	data := []byte("hello")
	n, err := pmw.Write(data)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected n=%d, got %d", len(data), n)
	}

	if w1.wrote != len(data) || w2.wrote != len(data) {
		t.Error("Both writers should have received data")
	}
}

type shortWriter struct {
	mu    sync.Mutex
	wrote int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p) / 2
	s.wrote += n
	return n, nil
}

func TestPMultiWriter_PartialWriteReturnsError(t *testing.T) {
	short := &shortWriter{}
	fast := &fastWriter{}

	pmw := New(context.Background(), 1*time.Second, short, fast)

	data := []byte("test data for partial write")
	n, err := pmw.Write(data)

	// At least one writer succeeded, so Write should return success
	if err != nil {
		t.Fatalf("Expected no error because fast writer succeeded, got: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected n=%d, got %d", len(data), n)
	}

	// The short writer is NOT evicted on write errors (only on timeouts).
	// Both writers should still be present.
	pmw.RLock()
	remaining := len(pmw.writers)
	pmw.RUnlock()
	if remaining != 2 {
		t.Errorf("Expected 2 writers (short writes do not evict), got %d", remaining)
	}
}

func TestPMultiWriter_AllPartialWritesReturnError(t *testing.T) {
	short1 := &shortWriter{}
	short2 := &shortWriter{}

	pmw := New(context.Background(), 1*time.Second, short1, short2)

	data := []byte("test data for all partial writes")
	n, err := pmw.Write(data)

	// All writers failed, so Write should return an error
	if err == nil {
		t.Fatal("Expected error when all writers short-write, got nil")
	}
	if n != 0 {
		t.Errorf("Expected n=0 when all writers fail, got %d", n)
	}

	_, ok := err.(PMultiWriterError)
	if !ok {
		t.Errorf("Expected PMultiWriterError, got %T", err)
	}
}
