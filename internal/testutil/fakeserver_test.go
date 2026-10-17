package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T, env ...string) (*mcp.ClientSession, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command(BuildFakeServer(t))
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, cmd
}

func TestFakeServer(t *testing.T) {
	dir := t.TempDir()
	spawnLog := filepath.Join(dir, "spawn.log")
	cs, cmd := connect(t, "FAKE_SPAWN_LOG="+spawnLog)
	ctx := context.Background()

	if got := cs.InitializeResult().Instructions; got == "" {
		t.Error("instructions missing")
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, n := range []string{"echo", "slow", "progress", "add_tool"} {
		if !names[n] {
			t.Errorf("tool %s missing: %v", n, names)
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "hi" {
		t.Errorf("echo = %q", got)
	}

	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fake://a"})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Contents[0].Text != "resource a" {
		t.Errorf("fake://a = %q", rr.Contents[0].Text)
	}
	rr, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fake://item/42"})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Contents[0].Text != "item 42" {
		t.Errorf("template read = %q", rr.Contents[0].Text)
	}
	tpls, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(tpls.ResourceTemplates) != 1 {
		t.Errorf("templates: %v %v", tpls, err)
	}

	pr, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := pr.Messages[0].Content.(*mcp.TextContent).Text; got != "hello bob" {
		t.Errorf("greet = %q", got)
	}

	cr, err := cs.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greet"},
		Argument: mcp.CompleteParamsArgument{Name: "name", Value: "al"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Completion.Values) != 2 {
		t.Errorf("completion = %v", cr.Completion.Values)
	}

	data, err := os.ReadFile(spawnLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(cmd.Process.Pid) {
		t.Errorf("spawn log %q, pid %d", data, cmd.Process.Pid)
	}
}

func TestFakeServerProgress(t *testing.T) {
	got := make(chan float64, 8)
	cmd := exec.Command(BuildFakeServer(t))
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			got <- r.Params.Progress
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	params := &mcp.CallToolParams{Name: "progress"}
	params.SetProgressToken("tok")
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}
	for want := 1.0; want <= 3; want++ {
		select {
		case p := <-got:
			if p != want {
				t.Errorf("progress %v want %v", p, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing progress %v", want)
		}
	}
}

func TestFakeServerAddToolNotifiesListChanged(t *testing.T) {
	changed := make(chan struct{}, 4)
	cmd := exec.Command(BuildFakeServer(t))
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "add_tool", Arguments: map[string]any{"name": "extra"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "extra"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "dynamic extra" {
		t.Errorf("extra = %q", got)
	}
}

func TestFakeServerSlowCancelLog(t *testing.T) {
	dir := t.TempDir()
	cancelLog := filepath.Join(dir, "cancel.log")
	cs, _ := connect(t, "FAKE_CANCEL_LOG="+cancelLog)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"millis": 10000}}); err == nil {
		t.Fatal("expected cancel error")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(cancelLog); len(data) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("upstream never logged cancellation")
}

func TestFakeServerFail(t *testing.T) {
	cmd := exec.Command(BuildFakeServer(t))
	cmd.Env = append(os.Environ(), "FAKE_FAIL=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err == nil {
		cs.Close()
		t.Fatal("expected connect failure")
	}
}

func TestFakeServerGrandchildInSameGroup(t *testing.T) {
	dir := t.TempDir()
	gcLog := filepath.Join(dir, "gc.log")
	_, cmd := connect(t, "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_LOG="+gcLog)
	data, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatal(err)
	}
	gpid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(gpid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("grandchild pgid %d, want %d", pgid, cmd.Process.Pid)
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}
