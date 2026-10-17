package config

import (
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultListen             = "127.0.0.1:8765"
	defaultCallTimeout        = 120 * time.Second
	defaultConnectTimeout     = 60 * time.Second
	defaultSessionIdleTimeout = 30 * time.Minute
)

var serverNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type Gateway struct {
	Listen             string
	CallTimeout        time.Duration
	ConnectTimeout     time.Duration
	SessionIdleTimeout time.Duration
}

type Server struct {
	Name        string
	Command     string
	Args        []string
	Cwd         string
	Env         map[string]string
	KeychainEnv map[string]string
	Enabled     bool
}

type Config struct {
	Gateway Gateway
	Servers map[string]Server
}

type rawGateway struct {
	Listen             string `toml:"listen"`
	CallTimeout        string `toml:"call_timeout"`
	ConnectTimeout     string `toml:"connect_timeout"`
	SessionIdleTimeout string `toml:"session_idle_timeout"`
}

type rawServer struct {
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Cwd         string            `toml:"cwd"`
	Env         map[string]string `toml:"env"`
	KeychainEnv map[string]string `toml:"keychain_env"`
	Enabled     *bool             `toml:"enabled"`
}

type rawConfig struct {
	Gateway rawGateway           `toml:"gateway"`
	Servers map[string]rawServer `toml:"servers"`
}

func Load(path string) (*Config, error) {
	var raw rawConfig
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("load config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}

	c := &Config{Servers: make(map[string]Server, len(raw.Servers))}
	if c.Gateway, err = raw.Gateway.resolve(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	for name, rs := range raw.Servers {
		c.Servers[name] = Server{
			Name:        name,
			Command:     rs.Command,
			Args:        rs.Args,
			Cwd:         rs.Cwd,
			Env:         rs.Env,
			KeychainEnv: rs.KeychainEnv,
			Enabled:     rs.Enabled == nil || *rs.Enabled,
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return c, nil
}

func (g rawGateway) resolve() (Gateway, error) {
	out := Gateway{Listen: g.Listen}
	if out.Listen == "" {
		out.Listen = defaultListen
	}
	var err error
	if out.CallTimeout, err = parseDuration("call_timeout", g.CallTimeout, defaultCallTimeout); err != nil {
		return out, err
	}
	if out.ConnectTimeout, err = parseDuration("connect_timeout", g.ConnectTimeout, defaultConnectTimeout); err != nil {
		return out, err
	}
	out.SessionIdleTimeout, err = parseDuration("session_idle_timeout", g.SessionIdleTimeout, defaultSessionIdleTimeout)
	return out, err
}

func parseDuration(key, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("gateway.%s: %w", key, err)
	}
	return d, nil
}

func (c *Config) Validate() error {
	if err := validateListen(c.Gateway.Listen); err != nil {
		return err
	}
	for _, d := range []struct {
		key string
		v   time.Duration
	}{
		{"call_timeout", c.Gateway.CallTimeout},
		{"connect_timeout", c.Gateway.ConnectTimeout},
		{"session_idle_timeout", c.Gateway.SessionIdleTimeout},
	} {
		if d.v <= 0 {
			return fmt.Errorf("gateway.%s must be positive", d.key)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		if err := validateServer(name, c.Servers[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("gateway.listen %q: %w", listen, err)
	}
	if port == "" {
		return fmt.Errorf("gateway.listen %q: missing port", listen)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("gateway.listen %q: host must be a loopback address", listen)
	}
	return nil
}

func validateServer(name string, s Server) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("servers.%s: invalid name, must match %s", name, serverNameRE)
	}
	if s.Command == "" {
		return fmt.Errorf("servers.%s: command is required", name)
	}
	for _, k := range slices.Sorted(maps.Keys(s.KeychainEnv)) {
		service, account, ok := strings.Cut(s.KeychainEnv[k], "/")
		if !ok || service == "" || account == "" {
			return fmt.Errorf("servers.%s.keychain_env.%s: %q must be \"service/account\"", name, k, s.KeychainEnv[k])
		}
	}
	return nil
}

func DefaultPath() string {
	if p := os.Getenv("MCPCTL_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".config", "mcpctl", "config.toml")
}

func DefaultStateDir() string {
	if p := os.Getenv("MCPCTL_STATE_DIR"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".local", "state", "mcpctl")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func (s Server) SameTransport(o Server) bool {
	return s.Command == o.Command &&
		slices.Equal(s.Args, o.Args) &&
		s.Cwd == o.Cwd &&
		maps.Equal(s.Env, o.Env) &&
		maps.Equal(s.KeychainEnv, o.KeychainEnv)
}
