package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, "[servers.a]\ncommand = \"/bin/a\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Gateway
	if g.Listen != "127.0.0.1:8765" || g.CallTimeout != 120*time.Second ||
		g.ConnectTimeout != 60*time.Second || g.SessionIdleTimeout != 30*time.Minute {
		t.Errorf("gateway defaults: %+v", g)
	}
	s := c.Servers["a"]
	if s.Name != "a" || s.Command != "/bin/a" || !s.Enabled {
		t.Errorf("server: %+v", s)
	}
}

func TestLoadFull(t *testing.T) {
	c, err := Load(writeConfig(t, `
[gateway]
listen = "[::1]:9000"
call_timeout = "5s"
connect_timeout = "1m30s"
session_idle_timeout = "10m"

[servers.atlassian]
command = "/opt/homebrew/bin/uvx"
args = ["mcp-atlassian"]
cwd = "/tmp"
env = { JIRA_URL = "https://x" }
keychain_env = { JIRA_PERSONAL_TOKEN = "mcpctl/jira-pat" }
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Gateway
	if g.Listen != "[::1]:9000" || g.CallTimeout != 5*time.Second ||
		g.ConnectTimeout != 90*time.Second || g.SessionIdleTimeout != 10*time.Minute {
		t.Errorf("gateway: %+v", g)
	}
	s := c.Servers["atlassian"]
	if s.Enabled || s.Cwd != "/tmp" || s.Args[0] != "mcp-atlassian" ||
		s.Env["JIRA_URL"] != "https://x" || s.KeychainEnv["JIRA_PERSONAL_TOKEN"] != "mcpctl/jira-pat" {
		t.Errorf("server: %+v", s)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"non loopback", "[gateway]\nlisten = \"0.0.0.0:8765\"\n", "loopback"},
		{"public ip", "[gateway]\nlisten = \"192.168.1.5:8765\"\n", "loopback"},
		{"hostname", "[gateway]\nlisten = \"example.com:8765\"\n", "loopback"},
		{"no port", "[gateway]\nlisten = \"127.0.0.1\"\n", "listen"},
		{"bad duration", "[gateway]\ncall_timeout = \"soon\"\n", "call_timeout"},
		{"zero duration", "[gateway]\nconnect_timeout = \"0s\"\n", "connect_timeout"},
		{"bad name upper", "[servers.Foo]\ncommand = \"x\"\n", "invalid name"},
		{"bad name leading dash", "[servers.\"-a\"]\ncommand = \"x\"\n", "invalid name"},
		{"name too long", "[servers." + strings.Repeat("a", 33) + "]\ncommand = \"x\"\n", "invalid name"},
		{"missing command", "[servers.a]\nargs = []\n", "command is required"},
		{"keychain no slash", "[servers.a]\ncommand = \"x\"\nkeychain_env = { K = \"svc\" }\n", "service/account"},
		{"keychain empty service", "[servers.a]\ncommand = \"x\"\nkeychain_env = { K = \"/acc\" }\n", "service/account"},
		{"keychain empty account", "[servers.a]\ncommand = \"x\"\nkeychain_env = { K = \"svc/\" }\n", "service/account"},
		{"unknown top key", "foo = 1\n", "unknown keys"},
		{"unknown server key", "[servers.a]\ncommand = \"x\"\ncommnad = \"y\"\n", "servers.a.commnad"},
		{"unknown gateway key", "[gateway]\nport = 1\n", "gateway.port"},
		{"invalid toml", "[servers\n", "load config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadListenAccepted(t *testing.T) {
	for _, l := range []string{"127.0.0.1:1", "127.5.5.5:80", "[::1]:80", "localhost:80"} {
		if _, err := Load(writeConfig(t, "[gateway]\nlisten = \""+l+"\"\n")); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestSameTransport(t *testing.T) {
	base := Server{Command: "c", Args: []string{"a"}, Cwd: "/x", Env: map[string]string{"A": "1"}, KeychainEnv: map[string]string{"K": "s/a"}}
	same := Server{Name: "other", Enabled: true, Command: "c", Args: []string{"a"}, Cwd: "/x", Env: map[string]string{"A": "1"}, KeychainEnv: map[string]string{"K": "s/a"}}
	if !base.SameTransport(same) {
		t.Error("name and enabled must not matter")
	}
	mut := []func(*Server){
		func(s *Server) { s.Command = "d" },
		func(s *Server) { s.Args = []string{"b"} },
		func(s *Server) { s.Args = nil },
		func(s *Server) { s.Cwd = "/y" },
		func(s *Server) { s.Env = map[string]string{"A": "2"} },
		func(s *Server) { s.Env = nil },
		func(s *Server) { s.KeychainEnv = map[string]string{"K": "s/b"} },
	}
	for i, m := range mut {
		o := same
		m(&o)
		if base.SameTransport(o) {
			t.Errorf("mutation %d should differ", i)
		}
	}
}

func TestDefaultPaths(t *testing.T) {
	t.Setenv("MCPCTL_CONFIG", "/c/config.toml")
	t.Setenv("MCPCTL_STATE_DIR", "/s")
	if DefaultPath() != "/c/config.toml" || DefaultStateDir() != "/s" {
		t.Error("env override ignored")
	}
	t.Setenv("MCPCTL_CONFIG", "")
	t.Setenv("MCPCTL_STATE_DIR", "")
	t.Setenv("HOME", "/h")
	if got := DefaultPath(); got != "/h/.config/mcpctl/config.toml" {
		t.Errorf("DefaultPath = %s", got)
	}
	if got := DefaultStateDir(); got != "/h/.local/state/mcpctl" {
		t.Errorf("DefaultStateDir = %s", got)
	}
}
