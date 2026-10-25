package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoanglong2311/mcpgw/internal/config"
	"github.com/hoanglong2311/mcpgw/internal/control"
)

type fakeControl struct {
	status *control.Status
	err    error
	calls  []string
}

func (f *fakeControl) Status(context.Context) (*control.Status, error) {
	f.calls = append(f.calls, "status")
	return f.status, f.err
}

func (f *fakeControl) server(call, name string) (*control.ServerStatus, error) {
	f.calls = append(f.calls, call+" "+name)
	if f.err != nil {
		return nil, f.err
	}
	return &control.ServerStatus{Name: name, State: "ready"}, nil
}

func (f *fakeControl) Enable(_ context.Context, n string) (*control.ServerStatus, error) {
	return f.server("enable", n)
}
func (f *fakeControl) Disable(_ context.Context, n string) (*control.ServerStatus, error) {
	return f.server("disable", n)
}
func (f *fakeControl) Restart(_ context.Context, n string) (*control.ServerStatus, error) {
	return f.server("restart", n)
}
func (f *fakeControl) Reload(context.Context) (*control.Status, error) {
	f.calls = append(f.calls, "reload")
	return f.status, f.err
}

type fakeSecrets struct{ ref, value string }

func (f *fakeSecrets) Set(_ context.Context, ref, value string) error {
	f.ref, f.value = ref, value
	return nil
}

type harness struct {
	t          *testing.T
	env        Env
	out, errb  *bytes.Buffer
	ctl        *fakeControl
	secrets    *fakeSecrets
	launchCmds [][]string
	launchOut  string
	launchErr  error
	home       string
	configPath string
	stateDir   string
	serveOpts  *ServeOptions
	connectURL string
	connectNm  string
}

const testConfig = `
[gateway]
listen = "127.0.0.1:9000"

[servers.alpha]
command = "/bin/true"

[servers.beta]
command = "/bin/true"
enabled = false
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:          t,
		out:        &bytes.Buffer{},
		errb:       &bytes.Buffer{},
		ctl:        &fakeControl{err: control.ErrDaemonNotRunning},
		secrets:    &fakeSecrets{},
		home:       filepath.Join(dir, "home"),
		configPath: filepath.Join(dir, "config.toml"),
		stateDir:   filepath.Join(dir, "state"),
	}
	if err := os.WriteFile(h.configPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env = Env{
		Stdin:      strings.NewReader("s3cret\n"),
		Stdout:     h.out,
		Stderr:     h.errb,
		Version:    "1.2.3",
		Home:       h.home,
		UID:        501,
		NewControl: func(string) Control { return h.ctl },
		Launchctl: func(_ context.Context, args ...string) (string, error) {
			h.launchCmds = append(h.launchCmds, args)
			return h.launchOut, h.launchErr
		},
		Secrets:    h.secrets,
		ReadSecret: ReadSecret,
		Serve: func(_ context.Context, o ServeOptions) error {
			h.serveOpts = &o
			return nil
		},
		Connect: func(_ context.Context, name, url string, _ *slog.Logger) error {
			h.connectNm, h.connectURL = name, url
			return nil
		},
		Executable: func() (string, error) { return "/opt/mcpctl/bin/mcpctl", nil },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.errb.Reset()
	global := []string{"--config", h.configPath, "--state-dir", h.stateDir}
	return Run(h.t.Context(), h.env, append(global, args...))
}

func (h *harness) wantExit(code int, args ...string) {
	h.t.Helper()
	if got := h.run(args...); got != code {
		h.t.Fatalf("mcpctl %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, got, code, h.out, h.errb)
	}
}

func runningStatus() *control.Status {
	return &control.Status{
		Version: "1.2.3",
		PID:     42,
		Listen:  "127.0.0.1:9000",
		Servers: []control.ServerStatus{
			{Name: "alpha", URL: "http://127.0.0.1:9000/s/alpha", Enabled: true, State: "ready", PID: 77, Sessions: 2},
			{Name: "beta", URL: "http://127.0.0.1:9000/s/beta", State: "disabled", LastError: "boom"},
		},
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "version")
	if h.out.String() != "mcpctl 1.2.3\n" {
		t.Fatalf("out = %q", h.out)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"enable"},
		{"enable", "a", "b"},
		{"disable"},
		{"restart"},
		{"reload", "extra"},
		{"list", "extra"},
		{"list", "--nope"},
		{"secret"},
		{"secret", "get", "x/y"},
		{"secret", "set"},
		{"connect"},
		{"daemon"},
		{"daemon", "bogus"},
		{"clients", "vim"},
		{"clients", "codex", "extra"},
		{"serve", "extra"},
	}
	for _, args := range cases {
		h := newHarness(t)
		h.wantExit(2, args...)
		if h.errb.Len() == 0 {
			t.Errorf("%v: no usage message", args)
		}
	}
}

func TestHelp(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "help")
	if !strings.Contains(h.out.String(), "secret set <service/account>") {
		t.Fatalf("out = %q", h.out)
	}
	h.wantExit(0, "list", "-h")
}

func TestServe(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "serve")
	if h.serveOpts == nil || h.serveOpts.ConfigPath != h.configPath || h.serveOpts.StateDir != h.stateDir || h.serveOpts.Version != "1.2.3" {
		t.Fatalf("serve opts = %+v", h.serveOpts)
	}
	if h.serveOpts.Logger != nil {
		t.Fatal("default serve must pass a nil logger so the daemon uses daemon.log")
	}
	h.wantExit(0, "serve", "--log-stderr")
	if h.serveOpts.Logger == nil {
		t.Fatal("--log-stderr must pass a logger")
	}
	h.env.Serve = func(context.Context, ServeOptions) error { return errors.New("bind failed") }
	h.wantExit(1, "serve")
	if !strings.Contains(h.errb.String(), "bind failed") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

func TestListRunning(t *testing.T) {
	h := newHarness(t)
	h.ctl.err, h.ctl.status = nil, runningStatus()
	h.wantExit(0, "list")
	for _, want := range []string{"NAME", "alpha", "ready", "77", "boom", "http://127.0.0.1:9000/s/beta"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out)
		}
	}
	h.wantExit(0, "list", "--json")
	var st control.Status
	if err := json.Unmarshal(h.out.Bytes(), &st); err != nil || len(st.Servers) != 2 || st.PID != 42 {
		t.Fatalf("json = %q err %v", h.out, err)
	}
}

func TestListDaemonNotRunning(t *testing.T) {
	h := newHarness(t)
	if err := config.SaveToggles(h.stateDir, config.Toggles{"alpha": false, "beta": true}); err != nil {
		t.Fatal(err)
	}
	h.wantExit(0, "list")
	if !strings.Contains(h.errb.String(), "daemon is not running") {
		t.Fatalf("stderr = %q", h.errb)
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "alpha") || !strings.Contains(lines[1], "false") ||
		!strings.Contains(lines[1], "stopped") || !strings.Contains(lines[2], "beta") || !strings.Contains(lines[2], "true") {
		t.Fatalf("out:\n%s", h.out)
	}

	h.wantExit(0, "list", "--json")
	var st control.Status
	if err := json.Unmarshal(h.out.Bytes(), &st); err != nil {
		t.Fatalf("json = %q: %v", h.out, err)
	}
	if st.Listen != "127.0.0.1:9000" || st.Servers[1].URL != "http://127.0.0.1:9000/s/beta" || st.Servers[0].State != "stopped" {
		t.Fatalf("status = %+v", st)
	}
}

func TestListDaemonNotRunningBadConfig(t *testing.T) {
	h := newHarness(t)
	h.configPath = filepath.Join(t.TempDir(), "missing.toml")
	h.wantExit(1, "list")
}

func TestStatus(t *testing.T) {
	h := newHarness(t)
	h.ctl.err, h.ctl.status = nil, runningStatus()
	h.wantExit(0, "status")
	if !strings.Contains(h.out.String(), "pid 42") || !strings.Contains(h.out.String(), "127.0.0.1:9000") {
		t.Fatalf("out = %q", h.out)
	}
	h.wantExit(0, "status", "--json")

	h.ctl.err = control.ErrDaemonNotRunning
	h.wantExit(1, "status")
	if !strings.Contains(h.errb.String(), "daemon is not running") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

func TestEnableDisableViaDaemon(t *testing.T) {
	h := newHarness(t)
	h.ctl.err = nil
	h.wantExit(0, "enable", "beta")
	h.wantExit(0, "disable", "alpha")
	if strings.Join(h.ctl.calls, ",") != "enable beta,disable alpha" {
		t.Fatalf("calls = %v", h.ctl.calls)
	}
	if _, err := os.Stat(h.stateDir); err == nil {
		t.Fatal("toggles written although daemon handled it")
	}
	h.ctl.err = &control.APIError{StatusCode: 404, Message: "unknown server"}
	h.wantExit(1, "enable", "nope")
	if !strings.Contains(h.errb.String(), "unknown server") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

func TestEnableDisableDaemonNotRunning(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "enable", "beta")
	if !strings.Contains(h.out.String(), "applies on next start") {
		t.Fatalf("out = %q", h.out)
	}
	toggles, err := config.LoadToggles(h.stateDir)
	if err != nil || !toggles["beta"] {
		t.Fatalf("toggles = %v, %v", toggles, err)
	}
	h.wantExit(0, "disable", "alpha", "--config", h.configPath)
	toggles, _ = config.LoadToggles(h.stateDir)
	if toggles["alpha"] || !toggles["beta"] {
		t.Fatalf("toggles = %v", toggles)
	}
	if v, ok := toggles["alpha"]; !ok || v {
		t.Fatalf("alpha toggle missing: %v", toggles)
	}
	h.wantExit(1, "enable", "ghost")
	if !strings.Contains(h.errb.String(), "unknown server") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

func TestRestartReload(t *testing.T) {
	h := newHarness(t)
	h.wantExit(1, "restart", "alpha")
	h.wantExit(1, "reload")
	if !strings.Contains(h.errb.String(), "daemon is not running") {
		t.Fatalf("stderr = %q", h.errb)
	}

	h.ctl.err, h.ctl.status = nil, runningStatus()
	h.wantExit(0, "restart", "alpha")
	if !strings.Contains(h.out.String(), "restarted alpha") {
		t.Fatalf("out = %q", h.out)
	}
	h.wantExit(0, "reload")
	if !strings.Contains(h.out.String(), "reloaded: 2 servers") {
		t.Fatalf("out = %q", h.out)
	}
}

func TestSecretSet(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "secret", "set", "mcpctl/jira-pat")
	if h.secrets.ref != "mcpctl/jira-pat" || h.secrets.value != "s3cret" {
		t.Fatalf("stored %+v", h.secrets)
	}
	if h.out.String() != "stored mcpctl/jira-pat\n" {
		t.Fatalf("out = %q", h.out)
	}
	if strings.Contains(h.out.String()+h.errb.String(), "s3cret") {
		t.Fatal("secret leaked to output")
	}

	h.env.Stdin = strings.NewReader("")
	h.wantExit(1, "secret", "set", "mcpctl/jira-pat")
}

func TestConnect(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "connect", "alpha")
	if h.connectNm != "alpha" || h.connectURL != "http://127.0.0.1:9000/s/alpha" {
		t.Fatalf("connect %q %q", h.connectNm, h.connectURL)
	}
	h.wantExit(0, "connect", "alpha", "--url", "http://127.0.0.1:1/s/x")
	if h.connectURL != "http://127.0.0.1:1/s/x" {
		t.Fatalf("url = %q", h.connectURL)
	}
	h.wantExit(1, "connect", "ghost")
}

func TestDaemonInstallUninstallStatus(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "daemon", "install")
	plist := filepath.Join(h.home, "Library", "LaunchAgents", "local.mcpctl.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/opt/mcpctl/bin/mcpctl", "<string>serve</string>", "--config", h.configPath, "--state-dir", h.stateDir} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if len(h.launchCmds) != 2 || h.launchCmds[0][0] != "bootout" || h.launchCmds[1][0] != "bootstrap" || h.launchCmds[1][1] != "gui/501" {
		t.Fatalf("launchctl calls = %v", h.launchCmds)
	}

	h.launchCmds = nil
	h.launchOut = "state = running\npid = 4242\nlast exit code = 0\n"
	h.wantExit(0, "daemon", "status")
	for _, want := range []string{"loaded: true", "state: running", "pid: 4242", "last exit: 0"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, h.out)
		}
	}

	h.launchOut, h.launchErr = "Could not find service", errors.New("exit status 113")
	h.wantExit(1, "daemon", "status")
	if !strings.Contains(h.out.String(), "loaded: false") {
		t.Fatalf("out = %q", h.out)
	}

	h.launchErr = nil
	h.wantExit(0, "daemon", "uninstall")
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remains: %v", err)
	}
}

func TestDaemonInstallDefaultPathsOmitFlags(t *testing.T) {
	h := newHarness(t)
	app := &app{env: h.env, defaultConfig: h.configPath, defaultState: h.stateDir, configPath: h.configPath, stateDir: h.stateDir}
	if err := app.daemon(t.Context(), []string{"install"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(h.home, "Library", "LaunchAgents", "local.mcpctl.plist"))
	if strings.Contains(string(data), "--config") || strings.Contains(string(data), "--state-dir") {
		t.Fatalf("default paths leaked into plist:\n%s", data)
	}
}

func TestDaemonInstallBootstrapFailure(t *testing.T) {
	h := newHarness(t)
	h.launchErr = errors.New("exit status 5")
	h.wantExit(1, "daemon", "install")
}

func TestClients(t *testing.T) {
	h := newHarness(t)
	h.wantExit(0, "clients", "codex")
	want := "[mcp_servers.alpha]\nurl = \"http://127.0.0.1:9000/s/alpha\"\n"
	if h.out.String() != want {
		t.Fatalf("out = %q", h.out)
	}

	h.wantExit(0, "clients")
	for _, want := range []string{"# claude-code:", "claude mcp add --transport http --scope user alpha", "# grok:", "[compat.claude]", "# claude-desktop:", `"connect"`} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out)
		}
	}
	if strings.Contains(h.out.String(), "beta") {
		t.Fatal("disabled server listed")
	}

	if err := config.SaveToggles(h.stateDir, config.Toggles{"alpha": false}); err != nil {
		t.Fatal(err)
	}
	h.wantExit(1, "clients", "codex")
	if !strings.Contains(h.errb.String(), "no enabled servers") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

func TestReadSecretStdin(t *testing.T) {
	got, err := ReadSecret(strings.NewReader("abc\r\n"), &bytes.Buffer{}, "x/y")
	if err != nil || got != "abc" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ReadSecret(strings.NewReader("\n"), &bytes.Buffer{}, "x/y"); err == nil {
		t.Fatal("expected error for empty value")
	}
}
