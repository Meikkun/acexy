package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"javinator9889/acexy/lib/acexy"
)

func TestFiniteStreamDrainsPayloadAndFinalHTTPFraming(t *testing.T) {
	previousTimeout := streamTimeout
	streamTimeout = 2 * time.Second
	defer func() { streamTimeout = previousTimeout }()
	for _, endpoint := range []acexy.AcexyEndpoint{acexy.M3U8_ENDPOINT, acexy.MPEG_TS_ENDPOINT} {
		for _, payload := range [][]byte{
			nil,
			[]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:10,\nsegment.ts\n#EXT-X-ENDLIST\n"),
			bytes.Repeat([]byte("finite payload"), 8192),
		} {
			t.Run(fmt.Sprintf("%s/%d", endpoint, len(payload)), func(t *testing.T) {
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case string(endpoint):
						fmt.Fprintf(w, `{"response":{"playback_url":"http://%s/play","command_url":"http://%s/stop"}}`, r.Host, r.Host)
					case "/play":
						w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
						w.Write(payload)
					case "/stop":
						fmt.Fprint(w, `{"response":"ok"}`)
					}
				}))
				defer backend.Close()
				u, _ := url.Parse(backend.URL)
				a := &acexy.Acexy{Scheme: "http", Host: u.Hostname(), Port: mustParseInt(u.Port()),
					Endpoint: endpoint, NoResponseTimeout: time.Second, EmptyTimeout: time.Second, WriteTimeout: time.Second}
				a.Init()
				defer a.Shutdown()
				proxy := httptest.NewServer(&Proxy{Acexy: a})
				defer proxy.Close()
				for i := 0; i < 10; i++ {
					resp, err := http.Get(proxy.URL + "/ace/getstream?id=" + fmt.Sprint(i))
					if err != nil {
						t.Fatalf("finite successful backend closed before response headers: %v", err)
					}
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, payload) {
						t.Fatalf("finite response: status=%d bytes=%q error=%v; want complete %q", resp.StatusCode, got, err, payload)
					}
				}
			})
		}
	}
}
