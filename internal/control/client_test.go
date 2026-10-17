package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := SocketPath(shortDir(t))
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func TestClientMethods(t *testing.T) {
	started := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	retry := started.Add(time.Minute)
	st := Status{Version: "v1", PID: 42, Listen: "127.0.0.1:8765", StartedAt: started,
		Servers: []ServerStatus{{Name: "a", URL: "http://x/s/a", Enabled: true, State: StateBackoff, PID: 1, PGID: 2, Sessions: 3, Restarts: 4, Since: started, LastError: "e", RetryAt: &retry}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, st) })
	mux.HandleFunc("POST "+PathReload, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, st) })
	for _, act := range []string{ActionEnable, ActionDisable, ActionRestart} {
		mux.HandleFunc("POST "+PathServers+"{name}/"+act, func(w http.ResponseWriter, r *http.Request) {
			s := ServerStatus{Name: r.PathValue("name"), State: act}
			writeJSON(w, 200, s)
		})
	}
	c := NewClient(serve(t, mux))
	ctx := context.Background()

	got, err := c.Status(ctx)
	if err != nil || got.PID != 42 || got.Servers[0].RetryAt == nil || !got.Servers[0].RetryAt.Equal(retry) || got.Servers[0].PGID != 2 {
		t.Fatalf("status %+v %v", got, err)
	}
	if got, err := c.Reload(ctx); err != nil || got.Version != "v1" {
		t.Fatalf("reload %+v %v", got, err)
	}
	for act, fn := range map[string]func(context.Context, string) (*ServerStatus, error){
		ActionEnable: c.Enable, ActionDisable: c.Disable, ActionRestart: c.Restart,
	} {
		s, err := fn(ctx, "srv-1")
		if err != nil || s.Name != "srv-1" || s.State != act {
			t.Fatalf("%s: %+v %v", act, s, err)
		}
	}
}

func TestStatusJSONShape(t *testing.T) {
	b, _ := json.Marshal(ServerStatus{})
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"name", "url", "enabled", "state", "pid", "pgid", "sessions", "restarts", "since", "last_error", "retry_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	b, _ = json.Marshal(Status{})
	json.Unmarshal(b, &m)
	for _, k := range []string{"version", "pid", "listen", "started_at", "servers"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
}

func TestClientErrorMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/servers/missing/enable", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, ErrorBody{Error: "unknown server \"missing\""})
	})
	mux.HandleFunc(PathReload, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "plain failure", 500)
	})
	c := NewClient(serve(t, mux))
	_, err := c.Enable(context.Background(), "missing")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 404 || ae.Message != `unknown server "missing"` {
		t.Fatalf("err %v", err)
	}
	_, err = c.Reload(context.Background())
	if !errors.As(err, &ae) || ae.StatusCode != 500 || ae.Message == "" {
		t.Fatalf("err %v", err)
	}
}

func TestClientDaemonNotRunning(t *testing.T) {
	dir := shortDir(t)
	sock := SocketPath(dir)
	c := NewClient(sock)
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("missing socket: %v", err)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(filepath.Join(dir, "control.sock")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("stale socket: %v", err)
	}
}
