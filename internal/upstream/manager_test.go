package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/config"
	"mcpctl/internal/source"
)

func TestConcurrentSessionsShareOneChild(t *testing.T) {
	f := newFixture(t, nil)
	f.apply(f.server("a", nil))
	src := f.m.Source("a")

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callEcho(t, src, fmt.Sprintf("hi-%d", i))
		}()
	}
	wg.Wait()
	if n := f.spawns(); n != 1 {
		t.Fatalf("spawns = %d, want 1", n)
	}
}

func TestEnableAfterDisableServesCallsFromNewProcess(t *testing.T) {
	f := newFixture(t, nil)
	srv := f.server("a", nil)
	f.apply(srv)
	src := f.m.Source("a")
	callEcho(t, src, "one")
	pid1 := statusOf(t, f.m, "a").PID
	events := &eventLog{}
	src.Subscribe(events.add)

	srv.Enabled = false
	ch := f.apply(srv)
	if len(ch.Stopped) != 1 || ch.Stopped[0] != "a" {
		t.Fatalf("changes = %+v", ch)
	}
	if _, err := src.Session(context.Background()); !errors.Is(err, source.ErrDisabled) {
		t.Fatalf("Session while disabled: %v", err)
	}
	if st := statusOf(t, f.m, "a"); st.State != "disabled" || st.Enabled {
		t.Fatalf("status = %+v", st)
	}
	waitDead(t, pid1)
	if events.count(source.Stopped) != 1 {
		t.Fatalf("Stopped events = %d", events.count(source.Stopped))
	}

	srv.Enabled = true
	ch = f.apply(srv)
	if len(ch.Started) != 1 || ch.Started[0] != "a" {
		t.Fatalf("changes = %+v", ch)
	}
	callEcho(t, src, "two")
	pid2 := statusOf(t, f.m, "a").PID
	if pid2 == 0 || pid2 == pid1 {
		t.Fatalf("pid after enable = %d, before = %d", pid2, pid1)
	}
	if n := f.spawns(); n != 2 {
		t.Fatalf("spawns = %d, want 2", n)
	}
}

func TestSourceHandleBeforeServerExists(t *testing.T) {
	f := newFixture(t, nil)
	src := f.m.Source("late")
	if _, err := src.Session(context.Background()); !errors.Is(err, source.ErrUnknown) {
		t.Fatalf("unknown Session: %v", err)
	}
	f.apply(f.server("late", nil))
	callEcho(t, src, "now works")

	if ch := f.m.Apply(nil); len(ch.Removed) != 1 {
		t.Fatalf("changes = %+v", ch)
	}
	if _, err := src.Session(context.Background()); !errors.Is(err, source.ErrUnknown) {
		t.Fatalf("removed Session: %v", err)
	}
}

func groupEnv(f *fixture) map[string]string {
	return map[string]string{"FAKE_GRANDCHILD": "1", "FAKE_GRANDCHILD_LOG": f.grandchild}
}

func startWithGrandchild(t *testing.T, f *fixture) (leader, grand int) {
	t.Helper()
	f.apply(f.server("g", groupEnv(f)))
	callEcho(t, f.m.Source("g"), "x")
	waitFor(t, "grandchild pid", func() bool { return len(f.pids(f.grandchild)) > 0 })
	pids := f.pids(f.grandchild)
	grand = pids[len(pids)-1]
	if !pidAlive(grand) {
		t.Fatal("grandchild not alive")
	}
	pgid, err := syscall.Getpgid(grand)
	if err != nil {
		t.Fatal(err)
	}
	leader = statusOf(t, f.m, "g").PID
	if pgid != leader {
		t.Fatalf("grandchild pgid = %d, leader pid = %d", pgid, leader)
	}
	if st := statusOf(t, f.m, "g"); st.PGID != leader {
		t.Fatalf("status pgid = %d", st.PGID)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "run", "g.pgid")); err != nil {
		t.Fatalf("pgid file: %v", err)
	}
	return leader, grand
}

func TestGrandchildKilledOnDisable(t *testing.T) {
	f := newFixture(t, nil)
	leader, grand := startWithGrandchild(t, f)
	srv := f.server("g", groupEnv(f))
	srv.Enabled = false
	f.apply(srv)
	waitDead(t, grand)
	waitDead(t, leader)
	if _, err := os.Stat(filepath.Join(f.dir, "run", "g.pgid")); !os.IsNotExist(err) {
		t.Fatalf("pgid file still present: %v", err)
	}
}

func TestGrandchildKilledOnRestart(t *testing.T) {
	f := newFixture(t, nil)
	leader, grand := startWithGrandchild(t, f)
	if err := f.m.Restart("g"); err != nil {
		t.Fatal(err)
	}
	waitDead(t, grand)
	waitDead(t, leader)
	callEcho(t, f.m.Source("g"), "again")
	if st := statusOf(t, f.m, "g"); st.PID == leader || st.Restarts != 1 {
		t.Fatalf("status after restart = %+v", st)
	}
}

func TestGrandchildKilledOnShutdown(t *testing.T) {
	f := newFixture(t, nil)
	leader, grand := startWithGrandchild(t, f)
	if err := f.m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDead(t, grand)
	waitDead(t, leader)
}

func TestReapStaleKillsGroupFromPreviousDaemon(t *testing.T) {
	f := newFixture(t, nil)
	leader, grand := startWithGrandchild(t, f)

	prev := NewManager(Options{StateDir: f.dir})
	b, err := os.ReadFile(filepath.Join(f.dir, "run", "g.pgid"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), fmt.Sprint(leader)) {
		t.Fatalf("pgid file = %q", b)
	}
	if err := prev.ReapStale(); err != nil {
		t.Fatal(err)
	}
	waitDead(t, grand)
	waitDead(t, leader)
	if _, err := os.Stat(filepath.Join(f.dir, "run", "g.pgid")); !os.IsNotExist(err) {
		t.Fatalf("pgid file still present: %v", err)
	}
}

func TestReapStaleIgnoresUnrelatedProcess(t *testing.T) {
	f := newFixture(t, nil)
	f.apply(f.server("g", nil))
	callEcho(t, f.m.Source("g"), "x")
	leader := statusOf(t, f.m, "g").PID

	runDir := filepath.Join(f.dir, "run")
	if err := os.Remove(filepath.Join(runDir, "g.pgid")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "other.pgid"), []byte(fmt.Sprintf("%d definitely-not-this\n", leader)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ReapStale(); err != nil {
		t.Fatal(err)
	}
	if !pidAlive(leader) {
		t.Fatal("unrelated leader was killed")
	}
	if _, err := os.Stat(filepath.Join(runDir, "other.pgid")); !os.IsNotExist(err) {
		t.Fatalf("stale file kept: %v", err)
	}
}

func TestSpawnFailureBacksOffWithoutSpawnStorm(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.backoffBase = 400 * time.Millisecond })
	f.apply(f.server("bad", map[string]string{"FAKE_FAIL": "1"}))
	src := f.m.Source("bad")

	_, err := src.Session(context.Background())
	be := isBackoff(err)
	if be == nil {
		t.Fatalf("first failure = %v, want BackoffError", err)
	}
	start := time.Now()
	for range 50 {
		t0 := time.Now()
		_, err := src.Session(context.Background())
		if isBackoff(err) == nil {
			t.Fatalf("during cooldown: %v", err)
		}
		if time.Since(t0) > 100*time.Millisecond {
			t.Fatalf("fail-fast took %v", time.Since(t0))
		}
	}
	if time.Since(start) < 350*time.Millisecond {
		if n := failLines(f, "bad"); n != 1 {
			t.Fatalf("spawn count during cooldown = %d, want 1", n)
		}
	}
	st := statusOf(t, f.m, "bad")
	if st.State == "backoff" && (st.RetryAt == nil || st.LastError == "") {
		t.Fatalf("status = %+v", st)
	}

	time.Sleep(time.Until(be.RetryAt) + 50*time.Millisecond)
	_, err = src.Session(context.Background())
	be2 := isBackoff(err)
	if be2 == nil || !be2.RetryAt.After(be.RetryAt.Add(500*time.Millisecond)) {
		t.Fatalf("second failure = %v (first retry %v)", err, be.RetryAt)
	}
	waitFor(t, "two failed spawns logged", func() bool { return failLines(f, "bad") == 2 })
}

func failLines(f *fixture, name string) int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "logs", name+".log"))
	return strings.Count(string(b), "FAKE_FAIL set")
}

func TestToolsChangedAndProgressEvents(t *testing.T) {
	f := newFixture(t, nil)
	f.apply(f.server("a", nil))
	src := f.m.Source("a")
	events := &eventLog{}
	src.Subscribe(events.add)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := src.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Connected event", func() bool { return events.count(source.Connected) == 1 })
	if ev, _ := events.first(source.Connected); ev.Instructions != "fake upstream instructions" {
		t.Fatalf("instructions = %q", ev.Instructions)
	}

	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "add_tool", Arguments: map[string]any{"name": "extra"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ToolsChanged", func() bool { return events.count(source.ToolsChanged) > 0 })

	p := &mcp.CallToolParams{Name: "progress"}
	p.SetProgressToken("tok-1")
	if _, err := sess.CallTool(ctx, p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "3 progress events", func() bool { return events.count(source.Progress) == 3 })
	ev, _ := events.first(source.Progress)
	if ev.Progress == nil || ev.Progress.ProgressToken != "tok-1" {
		t.Fatalf("progress event = %+v", ev.Progress)
	}
}

func TestConsecutiveDeadlineExceededRestartsUpstream(t *testing.T) {
	f := newFixture(t, nil)
	f.apply(f.server("a", nil))
	src := f.m.Source("a")
	callEcho(t, src, "x")
	pid1 := statusOf(t, f.m, "a").PID

	src.ReportCallResult(context.DeadlineExceeded)
	src.ReportCallResult(nil)
	src.ReportCallResult(context.DeadlineExceeded)
	time.Sleep(100 * time.Millisecond)
	if pid := statusOf(t, f.m, "a").PID; pid != pid1 {
		t.Fatalf("restarted after non-consecutive timeouts: %d -> %d", pid1, pid)
	}

	src.ReportCallResult(context.DeadlineExceeded)
	waitDead(t, pid1)
	callEcho(t, src, "after")
	if pid := statusOf(t, f.m, "a").PID; pid == pid1 {
		t.Fatal("pid unchanged after restart")
	}
	if n := f.spawns(); n != 2 {
		t.Fatalf("spawns = %d, want 2", n)
	}
}

func TestTransportChangeReplacesGeneration(t *testing.T) {
	f := newFixture(t, nil)
	srv := f.server("a", nil)
	f.apply(srv)
	src := f.m.Source("a")
	callEcho(t, src, "x")
	pid1 := statusOf(t, f.m, "a").PID

	if ch := f.apply(srv); len(ch.Added)+len(ch.Replaced)+len(ch.Started)+len(ch.Stopped)+len(ch.Removed) != 0 {
		t.Fatalf("no-op apply changes = %+v", ch)
	}
	if pid := statusOf(t, f.m, "a").PID; pid != pid1 {
		t.Fatal("no-op apply restarted the child")
	}

	srv.Args = []string{"-ignored"}
	srv2 := srv
	srv2.Env = map[string]string{"FAKE_SPAWN_LOG": f.spawnLog, "X": "1"}
	ch := f.apply(srv2)
	if len(ch.Replaced) != 1 || ch.Replaced[0] != "a" {
		t.Fatalf("changes = %+v", ch)
	}
	waitDead(t, pid1)
	callEcho(t, src, "y")
	if pid := statusOf(t, f.m, "a").PID; pid == pid1 {
		t.Fatal("same pid after transport change")
	}
}

func TestChildExitWhileReadyReconnectsOnNextCall(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.healthyAfter = time.Millisecond })
	f.apply(f.server("a", nil))
	src := f.m.Source("a")
	events := &eventLog{}
	src.Subscribe(events.add)
	callEcho(t, src, "x")
	pid1 := statusOf(t, f.m, "a").PID

	syscall.Kill(pid1, syscall.SIGKILL)
	waitFor(t, "idle after child death", func() bool { return statusOf(t, f.m, "a").State == "idle" })
	callEcho(t, src, "y")
	if pid := statusOf(t, f.m, "a").PID; pid == pid1 {
		t.Fatal("did not respawn")
	}
	waitFor(t, "second Connected", func() bool { return events.count(source.Connected) == 2 })
}

func TestEarlyChildExitEntersBackoff(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.backoffBase = 300 * time.Millisecond })
	f.apply(f.server("a", nil))
	src := f.m.Source("a")
	callEcho(t, src, "x")
	syscall.Kill(statusOf(t, f.m, "a").PID, syscall.SIGKILL)
	waitFor(t, "backoff", func() bool { return statusOf(t, f.m, "a").State == "backoff" })
	if _, err := src.Session(context.Background()); isBackoff(err) == nil {
		t.Fatalf("Session = %v, want BackoffError", err)
	}
}

func TestSessionHonorsCallerContextWhileConnecting(t *testing.T) {
	f := newFixture(t, nil)
	f.apply(f.server("a", nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.m.Source("a").Session(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Session = %v", err)
	}
	callEcho(t, f.m.Source("a"), "still fine")
}

func TestKeychainResolveErrorNamesKeyNotValue(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Resolve = func(ref string) (string, error) { return "", errors.New("item not found") }
		o.backoffBase = time.Hour
	})
	srv := f.server("a", nil)
	srv.KeychainEnv = map[string]string{"API_TOKEN": "svc/acct"}
	f.apply(srv)
	_, err := f.m.Source("a").Session(context.Background())
	be := isBackoff(err)
	if be == nil || !strings.Contains(be.Last.Error(), "API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if f.spawns() != 0 {
		t.Fatal("spawned despite resolve failure")
	}
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("MCPCTL_CONFIG", "/secret/config")
	t.Setenv("MCPCTL_STATE_DIR", "/secret/state")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LANG", "en_US.UTF-8")

	cfg := config.Server{
		Env:         map[string]string{"JIRA_URL": "https://x", "LANG": "C"},
		KeychainEnv: map[string]string{"TOKEN": "svc/acct"},
	}
	env, err := buildEnv(cfg, func(ref string) (string, error) {
		if ref != "svc/acct" {
			t.Fatalf("ref = %q", ref)
		}
		return "s3cret", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k := range got {
		if strings.HasPrefix(k, "MCPCTL_") || k == "AWS_SECRET_ACCESS_KEY" {
			t.Fatalf("env leaked %s", k)
		}
	}
	want := map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C", "JIRA_URL": "https://x", "TOKEN": "s3cret"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("env[%s] = %q, want %q", k, got[k], v)
		}
	}

	_, err = buildEnv(cfg, func(string) (string, error) { return "", errors.New("boom") })
	if err == nil || !strings.Contains(err.Error(), "TOKEN") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}
