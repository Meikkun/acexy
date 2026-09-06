package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"javinator9889/acexy/lib/acexy"
)

func newWriteTestProxy(t *testing.T, timeout time.Duration, playbackStatus int, stop func(http.ResponseWriter, *http.Request)) (*httptest.Server, *acexy.Acexy) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ace/getstream":
			fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
		case "/play":
			if playbackStatus != 200 {
				w.WriteHeader(playbackStatus)
				return
			}
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			chunk := make([]byte, 32*1024)
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := w.Write(chunk); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}
		case "/stop":
			if stop != nil {
				stop(w, r)
				return
			}
			fmt.Fprint(w, `{"response":"ok"}`)
		}
	}))
	u, _ := url.Parse(backend.URL)
	a := &acexy.Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()),
		Endpoint: acexy.MPEG_TS_ENDPOINT, NoResponseTimeout: time.Second,
		EmptyTimeout: time.Second, WriteTimeout: timeout}
	a.Init()
	proxy := httptest.NewServer(&Proxy{Acexy: a})
	t.Cleanup(func() {
		a.Shutdown()
		proxy.Close()
		backend.Close()
	})
	return proxy, a
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for client state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHealthyStreamOutlivesWriteTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	proxy, a := newWriteTestProxy(t, timeout, 200, nil)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(proxy.URL + "/ace/getstream?id=healthy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("unexpected live response: %d %v", resp.StatusCode, resp.Header)
	}
	start := time.Now()
	buffer := make([]byte, 32768)
	for time.Since(start) < 5*timeout {
		if _, err := io.ReadFull(resp.Body, buffer); err != nil {
			t.Fatalf("healthy stream hit total-duration deadline: %v", err)
		}
	}
	if a.ClientCount() != 1 {
		t.Fatal("healthy viewer unexpectedly unregistered")
	}
}

func TestWriteDeadlineDisconnectsSlowClientOnly(t *testing.T) {
	const timeout = 150 * time.Millisecond
	proxy, a := newWriteTestProxy(t, timeout, 200, nil)
	healthy, err := http.Get(proxy.URL + "/ace/getstream?id=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Body.Close()
	var received atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 32768)
		for {
			n, err := healthy.Body.Read(buffer)
			received.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	defer func() { healthy.Body.Close(); <-done }()
	address := proxy.Listener.Addr().String()
	slow, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if err := slow.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(slow, "GET /ace/getstream?id=shared HTTP/1.1\r\nHost: %s\r\n\r\n", address); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return a.ClientCount() == 2 })
	waitFor(t, func() bool { return a.ClientCount() == 1 })
	before := received.Load()
	waitFor(t, func() bool { return received.Load() > before+65536 })
}

func TestWriteDeadlineActuallyInterruptsBlockedSocket(t *testing.T) {
	result := make(chan error, 1)
	const timeout = 80 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer, err := newClientWriter(w, timeout)
		if err != nil {
			result <- err
			return
		}
		buffer := make([]byte, 1024*1024)
		for {
			start := time.Now()
			if _, err := writer.Write(buffer); err != nil {
				if elapsed := time.Since(start); elapsed > timeout+400*time.Millisecond {
					result <- fmt.Errorf("write took %v", elapsed)
				} else {
					result <- nil
				}
				return
			}
		}
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("socket write remained blocked")
	}
}

func TestClientWriterRejectsUnsupportedDeadlines(t *testing.T) {
	if _, err := newClientWriter(httptest.NewRecorder(), time.Second); err == nil {
		t.Fatal("unsupported deadline silently disabled")
	}
}

func TestClientWriterAbortInterruptsBlockedWrite(t *testing.T) {
	writerReady := make(chan *clientWriter, 1)
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer, err := newClientWriter(w, time.Minute)
		if err != nil {
			result <- err
			return
		}
		writerReady <- writer
		_, err = writer.Write(make([]byte, 16*1024*1024))
		result <- err
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	writer := <-writerReady
	// Receiving response bytes proves the write entered the socket; leave the
	// much larger remainder blocked instead of relying on a sleep to start it.
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("write was not blocked before cancellation: %v", err)
	default:
	}
	writer.Abort()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("aborted write succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation waited for the one-minute write deadline")
	}
}

func TestPlaybackErrorIsNotSuccessfulVideo(t *testing.T) {
	for _, status := range []int{404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			proxy, a := newWriteTestProxy(t, time.Second, status, nil)
			resp, err := http.Get(proxy.URL + "/ace/getstream?id=bad")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 502 || resp.Header.Get("Content-Type") == "video/MP2T" {
				t.Fatalf("playback %d returned %d %q", status, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			waitFor(t, func() bool {
				s, _ := a.GetStatus(nil)
				return a.ClientCount() == 0 && s.PendingAdmissions == 0 && s.PendingCleanup == 0
			})
		})
	}
}

func TestStartupFailuresReturnGatewayStatusAndReleaseAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"metadata status", 502},
		{"malformed metadata", 502},
		{"metadata timeout", 504},
		{"playback timeout", 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ace/getstream":
					switch tc.name {
					case "metadata status":
						w.WriteHeader(500)
					case "malformed metadata":
						fmt.Fprint(w, `{"response":`)
					case "metadata timeout":
						<-r.Context().Done()
					default:
						fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
					}
				case "/play":
					<-r.Context().Done()
				case "/stop":
					fmt.Fprint(w, `{"response":"ok"}`)
				}
			}))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			a := &acexy.Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()),
				Endpoint: acexy.MPEG_TS_ENDPOINT, NoResponseTimeout: 30 * time.Millisecond, MaxConnections: 1}
			a.Init()
			defer a.Shutdown()
			proxy := httptest.NewServer(&Proxy{Acexy: a})
			defer proxy.Close()
			resp, err := http.Get(proxy.URL + "/ace/getstream?id=start")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.want)
			}
			waitFor(t, func() bool {
				status, _ := a.GetStatus(nil)
				return status.PendingAdmissions == 0 && *status.TotalClients == 0
			})
		})
	}
}

func TestShutdownUsesOneDeadline(t *testing.T) {
	var stops atomic.Int32
	proxy, a := newWriteTestProxy(t, time.Second, 200, func(w http.ResponseWriter, r *http.Request) {
		stops.Add(1)
		<-r.Context().Done()
	})
	for i := 0; i < 3; i++ {
		id, _ := acexy.NewAceID(fmt.Sprint(i), "")
		if _, err := a.FetchStream(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	shutdownProxy(ctx, proxy.Config, a)
	if elapsed := time.Since(start); elapsed > 650*time.Millisecond {
		t.Fatalf("shutdown exceeded single budget: %v", elapsed)
	}
	if stops.Load() != 3 {
		t.Fatalf("backend cleanup not attempted concurrently: %d", stops.Load())
	}
	if a.ClientCount() != 0 || a.StreamCount() != 0 {
		t.Fatal("shutdown left active sessions")
	}
}
