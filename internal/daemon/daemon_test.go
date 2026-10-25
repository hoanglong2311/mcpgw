package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoanglong2311/mcpgw/internal/control"
)

func TestSharedChildAcrossClients(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()

	for i := range 3 {
		cs := h.connect("fake", nil)
		tools, err := cs.ListTools(context.Background(), nil)
		if err != nil || len(tools.Tools) == 0 {
			t.Fatalf("client %d list tools: %v %v", i, tools, err)
		}
		got, err := h.echo(cs, "hi")
		if err != nil || got != "hi" {
			t.Fatalf("client %d echo = %q, %v", i, got, err)
		}
	}
	if pids := readPIDs(h.path("fake", "spawn")); len(pids) != 1 {
		t.Fatalf("spawn log = %v, want exactly 1 pid", pids)
	}
}

func TestDisableEnableWithoutRestart(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()
	ctx := context.Background()

	cs := h.connect("fake", nil)
	if _, err := h.echo(cs, "a"); err != nil {
		t.Fatal(err)
	}
	ss, err := h.ctl.Disable(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if ss.Enabled || ss.State != control.StateDisabled {
		t.Fatalf("after disable: %+v", ss)
	}
	if _, err := h.echo(cs, "b"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("call while disabled: %v", err)
	}
	if _, err := h.ctl.Enable(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	var got string
	eventually(t, 10*time.Second, "call after enable", func() bool {
		got, err = h.echo(cs, "c")
		return err == nil
	})
	if got != "c" {
		t.Fatalf("echo = %q", got)
	}
	select {
	case err := <-h.done:
		t.Fatalf("daemon exited: %v", err)
	default:
	}
	if pids := readPIDs(h.path("fake", "spawn")); len(pids) != 2 {
		t.Fatalf("spawn log = %v, want 2 generations", pids)
	}
}

func TestProcessGroupCleanup(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake", env: map[string]string{"FAKE_GRANDCHILD": "1"}})
	h.start()

	cs := h.connect("fake", nil)
	if _, err := h.echo(cs, "x"); err != nil {
		t.Fatal(err)
	}
	gc := readPIDs(h.path("fake", "gc"))
	child := readPIDs(h.path("fake", "spawn"))
	if len(gc) != 1 || len(child) != 1 {
		t.Fatalf("gc=%v child=%v", gc, child)
	}
	if _, err := h.ctl.Disable(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "grandchild killed on disable", func() bool { return !alive(gc[0]) && !alive(child[0]) })

	if _, err := h.ctl.Enable(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "call after enable", func() bool {
		_, err := h.echo(cs, "y")
		return err == nil
	})
	all := append(readPIDs(h.path("fake", "gc")), readPIDs(h.path("fake", "spawn"))...)
	h.stop()
	for _, pid := range all {
		if pid != gc[0] && pid != child[0] && alive(pid) {
			t.Fatalf("pid %d still alive after daemon shutdown", pid)
		}
	}
	if _, err := os.Stat(h.state + "/control.sock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket left behind: %v", err)
	}
}

func TestClientCancelReachesUpstream(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()
	cs := h.connect("fake", nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"millis": 30000}})
		errc <- err
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled call returned no error")
	}
	eventually(t, 10*time.Second, "upstream cancel log", func() bool {
		data, _ := os.ReadFile(h.path("fake", "cancel"))
		return strings.Contains(string(data), "slow cancelled")
	})
}

func TestProtocolProxying(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()

	var (
		updated  = make(chan string, 4)
		listed   = make(chan struct{}, 8)
		progress = make(chan float64, 8)
	)
	cs := h.connect("fake", &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { updated <- r.Params.URI },
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { listed <- struct{}{} },
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			progress <- r.Params.Progress
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if init := cs.InitializeResult(); init == nil || init.Capabilities.Resources == nil || !init.Capabilities.Resources.Subscribe {
		t.Fatalf("initialize result: %+v", init)
	}
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fake://a"})
	if err != nil || res.Contents[0].Text != "resource a" {
		t.Fatalf("read resource: %+v %v", res, err)
	}
	res, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fake://item/7"})
	if err != nil || res.Contents[0].Text != "item 7" {
		t.Fatalf("read template: %+v %v", res, err)
	}
	prompt, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "z"}})
	if err != nil || prompt.Messages[0].Content.(*mcp.TextContent).Text != "hello z" {
		t.Fatalf("get prompt: %+v %v", prompt, err)
	}
	comp, err := cs.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greet"},
		Argument: mcp.CompleteParamsArgument{Name: "name", Value: "al"},
	})
	if err != nil || len(comp.Completion.Values) != 2 {
		t.Fatalf("complete: %+v %v", comp, err)
	}

	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "fake://a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "touch", Arguments: map[string]any{"uri": "fake://a"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case uri := <-updated:
		if uri != "fake://a" {
			t.Fatalf("updated uri = %q", uri)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no resource updated notification")
	}

	params := &mcp.CallToolParams{Name: "progress"}
	params.SetProgressToken("tok-1")
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}
	for want := 1.0; want <= 3; want++ {
		select {
		case got := <-progress:
			if got != want {
				t.Fatalf("progress = %v, want %v", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("missing progress %v", want)
		}
	}

	drain(listed)
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "add_tool", Arguments: map[string]any{"name": "extra"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-listed:
	case <-time.After(10 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	eventually(t, 10*time.Second, "extra tool listed", func() bool {
		tools, err := cs.ListTools(ctx, nil)
		if err != nil {
			return false
		}
		for _, tl := range tools.Tools {
			if tl.Name == "extra" {
				return true
			}
		}
		return false
	})
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func TestListChangedAfterReEnable(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()
	var changes atomic.Int32
	cs := h.connect("fake", &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changes.Add(1) },
	})
	if _, err := h.echo(cs, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctl.Disable(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "tools cleared notification", func() bool { return changes.Load() >= 1 })
	before := changes.Load()
	if _, err := h.ctl.Enable(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "tools restored notification", func() bool { return changes.Load() > before })
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("tools after re-enable: %v %v", tools, err)
	}
}

func TestConfigPollingAddsServer(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "one"})
	h.start()
	if _, err := h.echo(h.connect("one", nil), "a"); err != nil {
		t.Fatal(err)
	}

	h.writeConfig(serverSpec{name: "one"}, serverSpec{name: "two"})
	eventually(t, 10*time.Second, "second server routed", func() bool {
		resp, err := http.Post(h.url("/s/two"), "application/json", strings.NewReader("{}"))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode != http.StatusNotFound
	})
	if got, err := h.echo(h.connect("two", nil), "b"); err != nil || got != "b" {
		t.Fatalf("echo two = %q, %v", got, err)
	}

	h.writeConfig(serverSpec{name: "two"})
	eventually(t, 10*time.Second, "first server removed", func() bool {
		resp, err := http.Post(h.url("/s/one"), "application/json", strings.NewReader("{}"))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound
	})
	st, err := h.ctl.Status(context.Background())
	if err != nil || len(st.Servers) != 1 || st.Servers[0].Name != "two" {
		t.Fatalf("status after removal: %+v %v", st, err)
	}
}

func TestInvalidConfigKeepsRunning(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()
	cs := h.connect("fake", nil)

	if err := os.WriteFile(h.cfgPath, []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctl.Reload(context.Background()); err == nil {
		t.Fatal("reload of invalid config succeeded")
	}
	time.Sleep(2500 * time.Millisecond)
	if got, err := h.echo(cs, "still"); err != nil || got != "still" {
		t.Fatalf("echo after bad config = %q, %v", got, err)
	}
}

func TestHTTPSurface(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "secret-name"})
	h.start()

	do := func(method, path string, hdr map[string]string, host string) (int, string) {
		req, _ := http.NewRequest(method, h.url(path), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		b.Write(buf[:n])
		return resp.StatusCode, b.String()
	}

	if code, body := do("GET", "/healthz", nil, ""); code != 200 || body != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	for _, p := range []string{"/", "/s/", "/s/nope", "/s/secret-name/x", "/other"} {
		code, body := do("GET", p, nil, "")
		if code != 404 || strings.Contains(body, "secret-name") {
			t.Fatalf("%s = %d %q", p, code, body)
		}
	}
	if code, _ := do("POST", "/s/secret-name", nil, "evil.example"); code != http.StatusForbidden {
		t.Fatalf("rebinding Host accepted: %d", code)
	}
	if code, _ := do("POST", "/s/secret-name", map[string]string{"Origin": "https://evil.example"}, ""); code != http.StatusForbidden {
		t.Fatalf("cross-origin POST accepted: %d", code)
	}
}

func TestControlStatusAndErrors(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()
	ctx := context.Background()

	cs := h.connect("fake", nil)
	if _, err := h.echo(cs, "x"); err != nil {
		t.Fatal(err)
	}
	st, err := h.ctl.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.PID != os.Getpid() || st.Listen != "127.0.0.1:"+itoa(h.port) || st.StartedAt.IsZero() || len(st.Servers) != 1 {
		t.Fatalf("status = %+v", st)
	}
	s := st.Servers[0]
	if s.Name != "fake" || s.URL != h.url("/s/fake") || !s.Enabled || s.State != control.StateReady || s.PID == 0 || s.PGID == 0 || s.Sessions != 1 {
		t.Fatalf("server status = %+v", s)
	}

	rs, err := h.ctl.Restart(ctx, "fake")
	if err != nil || rs.Restarts != 1 {
		t.Fatalf("restart = %+v, %v", rs, err)
	}
	if _, err := h.echo(cs, "after restart"); err != nil {
		t.Fatalf("call after restart: %v", err)
	}

	var apiErr *control.APIError
	for _, call := range []func() error{
		func() error { _, err := h.ctl.Enable(ctx, "nope"); return err },
		func() error { _, err := h.ctl.Disable(ctx, "nope"); return err },
		func() error { _, err := h.ctl.Restart(ctx, "nope"); return err },
	} {
		if err := call(); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown server err = %v", err)
		}
	}
	if _, err := h.ctl.Disable(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctl.Restart(ctx, "fake"); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("restart disabled err = %v", err)
	}
	info, err := os.Stat(h.state + "/control.sock")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", info, err)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(serverSpec{name: "fake"})
	h.start()

	err := Run(context.Background(), Options{ConfigPath: h.cfgPath, StateDir: h.state})
	if !errors.Is(err, control.ErrAlreadyRunning) {
		t.Fatalf("second Run = %v", err)
	}
	if _, err := h.ctl.Status(context.Background()); err != nil {
		t.Fatalf("first daemon broken: %v", err)
	}
}

func TestPortBusyFails(t *testing.T) {
	h := newHarness(t)
	ln, err := netListen(h.port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h.writeConfig(serverSpec{name: "fake"})

	err = Run(context.Background(), Options{ConfigPath: h.cfgPath, StateDir: h.state})
	if err == nil || !strings.Contains(err.Error(), "gateway listen") {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Stat(h.state + "/control.sock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket left behind: %v", err)
	}
}
