package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoanglong2311/mcpgw/internal/config"
	"github.com/hoanglong2311/mcpgw/internal/source"
	"github.com/hoanglong2311/mcpgw/internal/testutil"
)

type fixture struct {
	t          *testing.T
	m          *Manager
	dir        string
	spawnLog   string
	grandchild string
	bin        string
}

func newFixture(t *testing.T, tweak func(*Options)) *fixture {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		StateDir:       dir,
		ConnectTimeout: 10 * time.Second,
		stopGrace:      time.Second,
	}
	if tweak != nil {
		tweak(&opts)
	}
	f := &fixture{
		t:          t,
		m:          NewManager(opts),
		dir:        dir,
		spawnLog:   filepath.Join(dir, "spawn.log"),
		grandchild: filepath.Join(dir, "grandchild.log"),
		bin:        testutil.BuildFakeServer(t),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		f.m.Shutdown(ctx)
		for _, pid := range f.pids(f.grandchild) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return f
}

func (f *fixture) server(name string, extraEnv map[string]string) config.Server {
	env := map[string]string{"FAKE_SPAWN_LOG": f.spawnLog}
	for k, v := range extraEnv {
		env[k] = v
	}
	return config.Server{Name: name, Command: f.bin, Env: env, Enabled: true}
}

func (f *fixture) apply(servers ...config.Server) Changes {
	f.t.Helper()
	d := make(map[string]config.Server)
	for _, s := range servers {
		d[s.Name] = s
	}
	return f.m.Apply(d)
}

func (f *fixture) pids(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (f *fixture) spawns() int { return len(f.pids(f.spawnLog)) }

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, "pid "+strconv.Itoa(pid)+" to die", func() bool { return !pidAlive(pid) })
}

func callEcho(t *testing.T, src source.Source, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := src.Session(ctx)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != text {
		t.Fatalf("echo = %q, want %q", got, text)
	}
}

func statusOf(t *testing.T, m *Manager, name string) Status {
	t.Helper()
	for _, s := range m.Status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s", name)
	return Status{}
}

type eventLog struct {
	mu     sync.Mutex
	events []source.Event
}

func (l *eventLog) add(ev source.Event) {
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

func (l *eventLog) count(k source.EventKind) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (l *eventLog) first(k source.EventKind) (source.Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == k {
			return e, true
		}
	}
	return source.Event{}, false
}

func isBackoff(err error) *source.BackoffError {
	var be *source.BackoffError
	if errors.As(err, &be) {
		return be
	}
	return nil
}
