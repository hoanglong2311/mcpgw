package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/control"
	"mcpctl/internal/testutil"
)

type serverSpec struct {
	name string
	env  map[string]string
}

type harness struct {
	t       *testing.T
	dir     string
	cfgPath string
	state   string
	port    int
	bin     string
	ctx     context.CancelFunc
	done    chan error
	ctl     *control.Client
	names   []string
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "mc")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:       t,
		dir:     dir,
		cfgPath: filepath.Join(dir, "config.toml"),
		state:   filepath.Join(dir, "st"),
		port:    freePort(t),
		bin:     testutil.BuildFakeServer(t),
	}
	h.ctl = control.NewClient(control.SocketPath(h.state))
	t.Cleanup(func() {
		h.stop()
		h.killLeftovers()
		os.RemoveAll(dir)
	})
	return h
}

func (h *harness) path(name, kind string) string {
	return filepath.Join(h.dir, name+"."+kind)
}

func (h *harness) writeConfig(servers ...serverSpec) {
	h.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[gateway]\nlisten = \"127.0.0.1:%d\"\ncall_timeout = \"10s\"\nconnect_timeout = \"20s\"\n", h.port)
	h.names = nil
	for _, s := range servers {
		h.names = append(h.names, s.name)
		env := map[string]string{
			"FAKE_SPAWN_LOG":      h.path(s.name, "spawn"),
			"FAKE_GRANDCHILD_LOG": h.path(s.name, "gc"),
			"FAKE_CANCEL_LOG":     h.path(s.name, "cancel"),
		}
		for k, v := range s.env {
			env[k] = v
		}
		var pairs []string
		for k, v := range env {
			pairs = append(pairs, fmt.Sprintf("%s = %q", k, v))
		}
		slices.Sort(pairs)
		fmt.Fprintf(&b, "\n[servers.%s]\ncommand = %q\nenv = { %s }\n", s.name, h.bin, strings.Join(pairs, ", "))
	}
	if err := os.WriteFile(h.cfgPath, []byte(b.String()), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = cancel
	h.done = make(chan error, 1)
	go func() {
		h.done <- Run(ctx, Options{
			ConfigPath: h.cfgPath,
			StateDir:   h.state,
			Version:    "test",
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	eventually(h.t, 10*time.Second, "daemon healthy", func() bool {
		resp, err := http.Get(h.url("/healthz"))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

func (h *harness) stop() {
	if h.ctx == nil {
		return
	}
	h.ctx()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		h.t.Error("daemon did not stop")
	}
	h.ctx = nil
}

func (h *harness) killLeftovers() {
	for _, name := range h.names {
		for _, kind := range []string{"spawn", "gc"} {
			for _, pid := range readPIDs(h.path(name, kind)) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

func (h *harness) url(p string) string {
	return "http://127.0.0.1:" + strconv.Itoa(h.port) + p
}

func (h *harness) connect(name string, opts *mcp.ClientOptions) *mcp.ClientSession {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.url("/s/" + name)}, nil)
	if err != nil {
		h.t.Fatalf("connect %s: %v", name, err)
	}
	h.t.Cleanup(func() { cs.Close() })
	return cs
}

func (h *harness) echo(cs *mcp.ClientSession, text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
	if err != nil {
		return "", err
	}
	if len(res.Content) == 0 {
		return "", fmt.Errorf("empty result")
	}
	return res.Content[0].(*mcp.TextContent).Text, nil
}

func readPIDs(path string) []int {
	data, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func eventually(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func itoa(n int) string { return strconv.Itoa(n) }

func netListen(port int) (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
}
