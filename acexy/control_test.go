package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"javinator9889/acexy/lib/acexy"
)

func TestMaintenanceControlPrivatePersistentAndExplicit(t *testing.T) {
	dir, err := os.MkdirTemp(".", "control-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	newInstance := func() *acexy.Acexy {
		a := &acexy.Acexy{}
		a.Init()
		t.Cleanup(a.Shutdown)
		return a
	}
	a := newInstance()
	server, err := startControl(a, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	call := func(method, path string, code int, phase string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://unix"+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != code {
			t.Fatalf("%s %s: got %d, want %d", method, path, resp.StatusCode, code)
		}
		if phase != "" {
			var status acexy.AcexyStatus
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			if status.MaintenancePhase != phase {
				t.Fatalf("phase=%q, want %q", status.MaintenancePhase, phase)
			}
		}
	}
	call("GET", "/status", 200, "open")
	call("GET", "/enter", 405, "")
	call("POST", "/quiesce", 200, "quiescing")
	call("POST", "/enter", 200, "maintenance")
	call("POST", "/quiesce", 409, "maintenance")
	for _, path := range []string{socket, socket + ".state"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private mode not enforced for %s: %v", path, err)
		}
	}
	public := httptest.NewServer(&Proxy{Acexy: a})
	defer public.Close()
	for _, path := range []string{"/quiesce", "/enter", "/exit"} {
		resp, err := http.Post(public.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("public control %s reachable: %d", path, resp.StatusCode)
		}
	}
	server.Close()
	client.CloseIdleConnections()
	restarted := newInstance()
	server, err = startControl(restarted, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	call("GET", "/status", 200, "maintenance")
	id, _ := acexy.NewAceID("blocked", "")
	if _, err := restarted.ReserveAdmission(id, nil); err == nil {
		t.Fatal("restart silently reopened maintenance")
	}
	call("POST", "/exit", 200, "open")
	body, err := os.ReadFile(socket + ".state")
	if err != nil {
		t.Fatal(err)
	}
	var state maintenanceState
	if json.Unmarshal(body, &state) != nil || state.Phase != "open" {
		t.Fatal("exit not durably recorded")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("unexpected abandoned state artifacts: %v %v", files, err)
	}
}

func TestMaintenancePersistenceFailureClosesAdmission(t *testing.T) {
	a := &acexy.Acexy{}
	a.Init()
	defer a.Shutdown()
	handler := controlHandler(a, func(string) error { return errors.New("disk failed") })
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/quiesce", nil))
	if w.Code != 500 {
		t.Fatalf("failed persistence acknowledged: %d", w.Code)
	}
	status, _ := a.GetStatus(nil)
	if !status.AdmissionPaused {
		t.Fatal("uncertain state persistence left admission open")
	}
}

func TestMaintenanceInvalidStateFailsStartup(t *testing.T) {
	dir, err := os.MkdirTemp(".", "control-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	if err := os.WriteFile(socket+".state", []byte(`{"maintenance_phase":"unknown"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &acexy.Acexy{}
	a.Init()
	if _, err := startControl(a, socket); err == nil {
		t.Fatal("invalid durable state ignored")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control listener opened before state validation")
	}
}

func TestMaintenanceControlDisabledByDefault(t *testing.T) {
	a := &acexy.Acexy{}
	a.Init()
	control, err := startControl(a, "")
	if err != nil || control != nil {
		t.Fatalf("default control not disabled: %v %v", control, err)
	}
}

func TestMaintenanceStatusIntegrationSchema(t *testing.T) {
	a := &acexy.Acexy{}
	a.Init()
	defer a.Shutdown()
	handler := controlHandler(a, nil)
	for _, phase := range []string{"open", "quiescing", "maintenance"} {
		if err := a.SetMaintenance(phase, nil); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
		var status map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]any{
			"maintenance": phase, "maintenance_phase": phase,
			"pending_admissions": float64(0), "pending_starts": float64(0),
			"pending_cleanup": float64(0), "streams": float64(0), "total_clients": float64(0),
		} {
			if status[key] != want {
				t.Fatalf("phase %s: %s=%v, want %v", phase, key, status[key], want)
			}
		}
	}
}
