package main

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// clientWriter is used only by the handler's copy loop. Abort may run
// concurrently, but only changes the socket deadline, never writes a response.
type clientWriter struct {
	controller *http.ResponseController
	response   http.ResponseWriter
	timeout    time.Duration
	mu         sync.Mutex
	aborted    bool
}

func newClientWriter(w http.ResponseWriter, timeout time.Duration) (*clientWriter, error) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &clientWriter{controller: controller, response: w, timeout: timeout}, nil
}

func (w *clientWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.aborted {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := w.response.Write(p)
	if err == nil {
		err = w.controller.Flush()
	}
	return n, err
}

func (w *clientWriter) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.aborted = true
	// A past deadline also interrupts a Write already blocked in the kernel.
	_ = w.controller.SetWriteDeadline(time.Now().Add(-time.Second))
}

func (w *clientWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.aborted {
		return io.ErrClosedPipe
	}
	// net/http writes final chunk framing after the handler returns.
	return w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
}

type clientPipe struct {
	*io.PipeWriter
	done chan struct{}
	once sync.Once
}

func (p *clientPipe) Close() error {
	p.once.Do(func() {
		close(p.done)
		_ = p.PipeWriter.CloseWithError(errors.New("client closed"))
	})
	return nil
}

func (p *clientPipe) CloseGracefully() error {
	p.once.Do(func() { _ = p.PipeWriter.Close() })
	return nil
}
