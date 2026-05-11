package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"javinator9889/acexy/lib/acexy"
)

func TestHandleStatus_GlobalWhenNoParams(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestHandleStatus_WithID(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status?id=test", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing stream, got %d", rr.Code)
	}
}

func TestHandleStatus_WithInfohash(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status?infohash=test", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing stream, got %d", rr.Code)
	}
}

func TestHandleStatus_BothIDAndInfohashReturnsBadRequest(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status?id=test&infohash=test", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestHandleStatus_EmptyValueReturnsBadRequest(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status?id=", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestHandleStatus_GlobalWithActiveStreams(t *testing.T) {
	a := &acexy.Acexy{
		Scheme:   "http",
		Host:     "localhost",
		Port:     6878,
		Endpoint: acexy.MPEG_TS_ENDPOINT,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	req := httptest.NewRequest(http.MethodGet, "/ace/status", nil)
	rr := httptest.NewRecorder()

	proxy.HandleStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"total_clients"`) {
		t.Errorf("expected total_clients field, got: %s", body)
	}
	if !strings.Contains(body, `"streams"`) {
		t.Errorf("expected streams field, got: %s", body)
	}
}
