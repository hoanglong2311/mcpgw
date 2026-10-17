package launchd

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func testSpec() Spec {
	return Spec{
		Executable: "/opt/mcpctl/bin/mcpctl",
		Args:       []string{"--config", "/tmp/a&b/config.toml"},
		StateDir:   "/Users/x/.local/state/mcpctl",
		Home:       "/Users/x",
	}
}

func TestPlistGolden(t *testing.T) {
	got := testSpec().Plist()
	golden := filepath.Join("testdata", "local.mcpctl.plist")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("plist mismatch:\n%s", got)
	}
}

func TestPlistLint(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	path := filepath.Join(t.TempDir(), "p.plist")
	if err := os.WriteFile(path, testSpec().Plist(), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v: %s", err, out)
	}
}

type fakeRunner struct {
	calls [][]string
	fn    func(args []string) (string, error)
}

func (f *fakeRunner) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.fn != nil {
		return f.fn(args)
	}
	return "", nil
}

func TestInstall(t *testing.T) {
	home := t.TempDir()
	fr := &fakeRunner{fn: func(args []string) (string, error) {
		if args[0] == "bootout" {
			return "Boot-out failed: 3: No such process", errors.New("exit status 3")
		}
		return "", nil
	}}
	m := &Manager{Run: fr.run, UID: 501, Home: home}
	s := testSpec()
	s.Home = home
	s.StateDir = filepath.Join(home, "state")
	path, err := m.Install(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "Library", "LaunchAgents", "local.mcpctl.plist") {
		t.Fatalf("path = %s", path)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("plist stat = %v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(s.StateDir, "logs")); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"bootout", "gui/501/local.mcpctl"},
		{"bootstrap", "gui/501", path},
	}
	if !reflect.DeepEqual(fr.calls, want) {
		t.Fatalf("calls = %v", fr.calls)
	}
}

func TestInstallBootstrapFails(t *testing.T) {
	fr := &fakeRunner{fn: func(args []string) (string, error) {
		if args[0] == "bootstrap" {
			return "Bootstrap failed: 5", errors.New("exit status 5")
		}
		return "", nil
	}}
	m := &Manager{Run: fr.run, UID: 501, Home: t.TempDir()}
	s := testSpec()
	s.StateDir = t.TempDir()
	if _, err := m.Install(t.Context(), s); err == nil || !strings.Contains(err.Error(), "Bootstrap failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	home := t.TempDir()
	fr := &fakeRunner{}
	m := &Manager{Run: fr.run, UID: 501, Home: home}
	s := testSpec()
	s.StateDir = t.TempDir()
	path, err := m.Install(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	fr.calls = nil
	if err := m.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still present: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, [][]string{{"bootout", "gui/501/local.mcpctl"}}) {
		t.Fatalf("calls = %v", fr.calls)
	}
	if err := m.Uninstall(t.Context()); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

const printOut = `gui/501/local.mcpctl = {
	active count = 1
	path = /Users/x/Library/LaunchAgents/local.mcpctl.plist
	state = running
	program = /opt/mcpctl/bin/mcpctl
	pid = 4242
	last exit code = (never exited)
}
`

func TestStatus(t *testing.T) {
	m := &Manager{Run: (&fakeRunner{fn: func([]string) (string, error) { return printOut, nil }}).run, UID: 501, Home: t.TempDir()}
	info, err := m.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Loaded || info.State != "running" || info.PID != 4242 || info.LastExit != "(never exited)" || info.PlistInstalled {
		t.Fatalf("info = %+v", info)
	}
}

func TestStatusNotLoaded(t *testing.T) {
	fr := &fakeRunner{fn: func([]string) (string, error) {
		return "Could not find service", errors.New("exit status 113")
	}}
	m := &Manager{Run: fr.run, UID: 501, Home: t.TempDir()}
	info, err := m.Status(t.Context())
	if err != nil || info.Loaded {
		t.Fatalf("info = %+v, err = %v", info, err)
	}
}
