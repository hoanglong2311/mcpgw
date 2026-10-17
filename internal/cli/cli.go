package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"mcpctl/internal/config"
	"mcpctl/internal/control"
	"mcpctl/internal/launchd"
)

const usageText = `usage: mcpctl [--config PATH] [--state-dir DIR] <command> [args]

commands:
  serve [--log-stderr]        run the gateway daemon in the foreground (logs to <state>/logs/daemon.log unless --log-stderr)
  list [--json]               list configured servers and their state
  status [--json]             show daemon status (exit 1 if the daemon is not running)
  enable <name>               enable a server
  disable <name>              disable a server
  restart <name>              restart a server (daemon required)
  reload                      re-read the config file (daemon required)
  secret set <service/account>  store a secret in the Keychain (value read from the terminal or stdin)
  connect <name> [--url URL]  stdio bridge to the gateway, for Claude Desktop
  daemon install|uninstall|status
                              manage the launchd agent local.mcpctl
  clients [<client>]          print config snippets for MCP clients (` + "%s" + `)
  version                     print the version

global flags:
  --config PATH               config file (default ` + "%s" + `)
  --state-dir DIR             state directory (default ` + "%s" + `)

exit codes: 0 ok, 1 runtime error, 2 usage error
`

type Control interface {
	Status(ctx context.Context) (*control.Status, error)
	Enable(ctx context.Context, name string) (*control.ServerStatus, error)
	Disable(ctx context.Context, name string) (*control.ServerStatus, error)
	Restart(ctx context.Context, name string) (*control.ServerStatus, error)
	Reload(ctx context.Context) (*control.Status, error)
}

type SecretStore interface {
	Set(ctx context.Context, ref, value string) error
}

type ServeOptions struct {
	ConfigPath, StateDir, Version string
	Logger                        *slog.Logger
}

type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Version        string
	Home           string
	UID            int
	NewControl     func(socketPath string) Control
	Launchctl      launchd.Runner
	Secrets        SecretStore
	ReadSecret     func(in io.Reader, prompt io.Writer, label string) (string, error)
	Serve          func(ctx context.Context, opts ServeOptions) error
	Connect        func(ctx context.Context, name, url string, log *slog.Logger) error
	Executable     func() (string, error)
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

type app struct {
	env           Env
	configPath    string
	stateDir      string
	defaultConfig string
	defaultState  string
}

func Run(ctx context.Context, env Env, args []string) int {
	a := &app{
		env:           env,
		defaultConfig: config.DefaultPath(),
		defaultState:  config.DefaultStateDir(),
	}
	a.configPath, a.stateDir = a.defaultConfig, a.defaultState

	err := a.dispatch(ctx, args)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case isUsage(err):
		fmt.Fprintf(env.Stderr, "mcpctl: %v\nrun 'mcpctl help' for usage\n", err)
		return 2
	default:
		fmt.Fprintf(env.Stderr, "mcpctl: %v\n", err)
		return 1
	}
}

func isUsage(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}

func (a *app) printUsage(w io.Writer) {
	fmt.Fprintf(w, usageText, clientNames(), a.defaultConfig, a.defaultState)
}

func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.configPath, "config", a.configPath, "config file")
	fs.StringVar(&a.stateDir, "state-dir", a.stateDir, "state directory")
	return fs
}

func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (a *app) dispatch(ctx context.Context, args []string) error {
	global := a.flags("mcpctl")
	rest, err := globalParse(global, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.printUsage(a.env.Stdout)
		}
		return err
	}
	if len(rest) == 0 {
		a.printUsage(a.env.Stderr)
		return usagef("missing command")
	}
	cmd, cargs := rest[0], rest[1:]
	run := map[string]func(context.Context, []string) error{
		"serve":   a.serve,
		"list":    a.list,
		"status":  a.status,
		"enable":  func(ctx context.Context, args []string) error { return a.toggle(ctx, "enable", args, true) },
		"disable": func(ctx context.Context, args []string) error { return a.toggle(ctx, "disable", args, false) },
		"restart": a.restart,
		"reload":  a.reload,
		"secret":  a.secret,
		"connect": a.connect,
		"daemon":  a.daemon,
		"clients": a.clients,
		"version": a.version,
	}
	if cmd == "help" {
		a.printUsage(a.env.Stdout)
		return nil
	}
	fn, ok := run[cmd]
	if !ok {
		return usagef("unknown command %q", cmd)
	}
	if err := fn(ctx, cargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.printUsage(a.env.Stdout)
		}
		return err
	}
	return nil
}

func globalParse(fs *flag.FlagSet, args []string) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usagef("%v", err)
	}
	return fs.Args(), nil
}

func (a *app) control() Control {
	return a.env.NewControl(control.SocketPath(a.stateDir))
}
