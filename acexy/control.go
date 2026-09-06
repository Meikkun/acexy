package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"javinator9889/acexy/lib/acexy"
)

type maintenanceState struct {
	Phase string `json:"maintenance_phase"`
}

// startControl is a bootstrap operation: restore the durable gate before opening
// the public listener. There are deliberately no control routes in Proxy.
func startControl(a *acexy.Acexy, socket string) (*http.Server, error) {
	if socket == "" {
		return nil, nil
	}
	statePath := socket + ".state"
	if info, err := os.Lstat(statePath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("maintenance state must be a regular file")
		}
		if err := os.Chmod(statePath, 0600); err != nil {
			return nil, err
		}
		body, err := os.ReadFile(statePath)
		if err != nil {
			return nil, err
		}
		var state maintenanceState
		if err := json.Unmarshal(body, &state); err != nil {
			return nil, fmt.Errorf("invalid maintenance state: %w", err)
		}
		if err := a.SetMaintenance(state.Phase, nil); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("control path exists and is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("control socket already in use")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot verify stale control socket: %w", dialErr)
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Bootstrap only: create with private permissions, not a chmod-after-bind
	// window during which another user could queue a privileged request.
	mask := syscall.Umask(0077)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	syscall.Umask(mask)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:           controlHandler(a, func(phase string) error { return persistMaintenance(statePath, phase) }),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	return server, nil
}

func controlHandler(a *acexy.Acexy, persist func(string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase := ""
		switch r.URL.Path {
		case "/status":
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
		case "/quiesce":
			phase = "quiescing"
		case "/enter":
			phase = "maintenance"
		case "/exit":
			phase = "open"
		default:
			http.NotFound(w, r)
			return
		}
		if phase != "" {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := a.SetMaintenance(phase, persist); err != nil {
				var busy *acexy.ErrLimitReached
				code := http.StatusInternalServerError
				if errors.As(err, &busy) {
					code = http.StatusConflict
				}
				status, _ := a.GetStatus(nil)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(struct {
					acexy.AcexyStatus
					Error string `json:"error"`
				}{status, "maintenance_transition_failed"})
				return
			}
		}
		status, _ := a.GetStatus(nil)
		writeStatusJSON(w, status)
	})
}

func persistMaintenance(path, phase string) error {
	data, err := json.Marshal(maintenanceState{Phase: phase})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
