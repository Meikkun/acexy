package acexy

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := make([]byte, s.buf.Len())
	copy(b, s.buf.Bytes())
	return b
}

func (s *safeBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func TestBroadcaster_WritesToAllClients(t *testing.T) {
	b := NewBroadcaster(64)
	defer b.Close()

	buf1 := &safeBuffer{}
	buf2 := &safeBuffer{}
	b.Add(buf1)
	b.Add(buf2)

	data := []byte("hello broadcaster")
	n, err := b.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected n=%d, got %d", len(data), n)
	}

	// Give the client goroutines time to drain
	time.Sleep(100 * time.Millisecond)

	if !bytes.Equal(buf1.Bytes(), data) {
		t.Errorf("client 1 expected %q, got %q", data, buf1.Bytes())
	}
	if !bytes.Equal(buf2.Bytes(), data) {
		t.Errorf("client 2 expected %q, got %q", data, buf2.Bytes())
	}
}

func TestBroadcaster_SlowClientEvicted(t *testing.T) {
	b := NewBroadcaster(128)
	defer b.Close()

	fast := &safeBuffer{}
	slow := &slowBlockingWriter{}

	b.Add(fast)
	b.Add(slow)

	// Write enough chunks to fill the slow client's queue and trigger eviction.
	// Yield occasionally so the fast client's run goroutine gets a chance to drain.
	data := []byte("x")
	for i := 0; i < 200; i++ {
		b.Write(data)
		if i%20 == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Give time for eviction and draining
	time.Sleep(300 * time.Millisecond)

	b.mu.RLock()
	remaining := len(b.clients)
	_, fastStillConnected := b.clients[fast]
	_, slowStillConnected := b.clients[slow]
	b.mu.RUnlock()

	// At least one client should survive (the fast one).
	if remaining == 0 {
		t.Fatalf("expected at least 1 client after eviction, got 0")
	}
	if !fastStillConnected {
		t.Fatal("expected fast client to remain connected")
	}
	if slowStillConnected {
		t.Fatal("expected slow client to be evicted")
	}

	// The surviving client should have received data.
	if fast.Len() == 0 {
		t.Errorf("fast client received no data")
	}
}

type slowBlockingWriter struct {
	mu sync.Mutex
}

func (s *slowBlockingWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	return len(p), nil
}

func TestBroadcaster_NoClientsDoesNotError(t *testing.T) {
	b := NewBroadcaster(64)
	defer b.Close()

	data := []byte("no clients")
	n, err := b.Write(data)
	if err != nil {
		t.Fatalf("Write with no clients should not error, got: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected n=%d, got %d", len(data), n)
	}
}

func TestBroadcaster_CloseIsIdempotent(t *testing.T) {
	b := NewBroadcaster(64)

	buf := &safeBuffer{}
	b.Add(buf)

	if err := b.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}

	// Write after close should error
	_, err := b.Write([]byte("after close"))
	if err != io.ErrClosedPipe {
		t.Errorf("expected ErrClosedPipe after close, got %v", err)
	}
}

func TestBroadcaster_RemoveClient(t *testing.T) {
	b := NewBroadcaster(64)
	defer b.Close()

	buf1 := &safeBuffer{}
	buf2 := &safeBuffer{}
	b.Add(buf1)
	b.Add(buf2)

	b.Remove(buf1)

	data := []byte("only buf2 should get this")
	b.Write(data)
	time.Sleep(100 * time.Millisecond)

	if buf1.Len() != 0 {
		t.Errorf("removed client should have 0 bytes, got %d", buf1.Len())
	}
	if !bytes.Equal(buf2.Bytes(), data) {
		t.Errorf("remaining client expected %q, got %q", data, buf2.Bytes())
	}
}

func TestBroadcaster_Race(t *testing.T) {
	b := NewBroadcaster(64)
	defer b.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := &safeBuffer{}
			b.Add(buf)
			time.Sleep(10 * time.Millisecond)
			b.Remove(buf)
		}()
	}

	for i := 0; i < 100; i++ {
		b.Write([]byte("race test data"))
	}

	wg.Wait()
}
