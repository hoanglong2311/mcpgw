package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"mcpctl/internal/clients"
	"mcpctl/internal/config"
	"mcpctl/internal/control"
	"mcpctl/internal/launchd"
)

func clientNames() string { return strings.Join(clients.Names(), ", ") }

func noArgs(pos []string, cmd string) error {
	if len(pos) > 0 {
		return usagef("%s: unexpected argument %q", cmd, pos[0])
	}
	return nil
}

func oneArg(pos []string, cmd, what string) (string, error) {
	switch len(pos) {
	case 0:
		return "", usagef("%s: missing %s", cmd, what)
	case 1:
		return pos[0], nil
	}
	return "", usagef("%s: unexpected argument %q", cmd, pos[1])
}

func (a *app) version(_ context.Context, args []string) error {
	fs := a.flags("version")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(pos, "version"); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "mcpctl %s\n", a.env.Version)
	return nil
}

func (a *app) serve(ctx context.Context, args []string) error {
	fs := a.flags("serve")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(pos, "serve"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.env.Serve(ctx, ServeOptions{
		ConfigPath: a.configPath,
		StateDir:   a.stateDir,
		Version:    a.env.Version,
		Logger:     slog.New(slog.NewTextHandler(a.env.Stderr, nil)),
	})
}

func (a *app) offlineStatus() (*control.Status, error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, err
	}
	toggles, err := config.LoadToggles(a.stateDir)
	if err != nil {
		return nil, err
	}
	eff := config.Effective(cfg, toggles)
	st := &control.Status{Listen: cfg.Gateway.Listen}
	for _, name := range slices.Sorted(maps.Keys(eff)) {
		st.Servers = append(st.Servers, control.ServerStatus{
			Name:    name,
			URL:     serverURL(cfg.Gateway.Listen, name),
			Enabled: eff[name].Enabled,
			State:   "stopped",
		})
	}
	return st, nil
}

func serverURL(listen, name string) string {
	return "http://" + listen + "/s/" + name
}

func (a *app) list(ctx context.Context, args []string) error {
	fs := a.flags("list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(pos, "list"); err != nil {
		return err
	}
	st, err := a.control().Status(ctx)
	if errors.Is(err, control.ErrDaemonNotRunning) {
		fmt.Fprintln(a.env.Stderr, "mcpctl daemon is not running; showing configured state")
		st, err = a.offlineStatus()
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(st)
	}
	printServers(a.env.Stdout, st.Servers)
	return nil
}

func (a *app) status(ctx context.Context, args []string) error {
	fs := a.flags("status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(pos, "status"); err != nil {
		return err
	}
	st, err := a.control().Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(st)
	}
	fmt.Fprintf(a.env.Stdout, "mcpctl %s (pid %d) listening on %s, up %s\n",
		st.Version, st.PID, st.Listen, time.Since(st.StartedAt).Round(time.Second))
	printServers(a.env.Stdout, st.Servers)
	return nil
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printServers(w io.Writer, servers []control.ServerStatus) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENABLED\tSTATE\tPID\tSESSIONS\tURL\tERROR")
	for _, s := range servers {
		pid := "-"
		if s.PID > 0 {
			pid = strconv.Itoa(s.PID)
		}
		lastErr := s.LastError
		if lastErr == "" {
			lastErr = "-"
		}
		fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%d\t%s\t%s\n", s.Name, s.Enabled, s.State, pid, s.Sessions, s.URL, lastErr)
	}
	_ = tw.Flush()
}

func (a *app) toggle(ctx context.Context, cmd string, args []string, on bool) error {
	fs := a.flags(cmd)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := oneArg(pos, cmd, "server name")
	if err != nil {
		return err
	}
	ctl := a.control()
	var st *control.ServerStatus
	if on {
		st, err = ctl.Enable(ctx, name)
	} else {
		st, err = ctl.Disable(ctx, name)
	}
	if err == nil {
		fmt.Fprintf(a.env.Stdout, "%sd %s (state %s)\n", cmd, name, st.State)
		return nil
	}
	if !errors.Is(err, control.ErrDaemonNotRunning) {
		return err
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	if _, ok := cfg.Servers[name]; !ok {
		return fmt.Errorf("unknown server %q", name)
	}
	toggles, err := config.LoadToggles(a.stateDir)
	if err != nil {
		return err
	}
	toggles[name] = on
	if err := config.SaveToggles(a.stateDir, toggles); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "mcpctl daemon is not running: %sd %s in toggles.json, applies on next start\n", cmd, name)
	return nil
}

func (a *app) restart(ctx context.Context, args []string) error {
	fs := a.flags("restart")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := oneArg(pos, "restart", "server name")
	if err != nil {
		return err
	}
	st, err := a.control().Restart(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "restarted %s (state %s)\n", name, st.State)
	return nil
}

func (a *app) reload(ctx context.Context, args []string) error {
	fs := a.flags("reload")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(pos, "reload"); err != nil {
		return err
	}
	st, err := a.control().Reload(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "reloaded: %d servers\n", len(st.Servers))
	return nil
}

func (a *app) secret(ctx context.Context, args []string) error {
	fs := a.flags("secret")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] != "set" {
		return usagef("secret: want 'secret set <service/account>'")
	}
	ref, err := oneArg(pos[1:], "secret set", "service/account")
	if err != nil {
		return err
	}
	value, err := a.env.ReadSecret(a.env.Stdin, a.env.Stderr, ref)
	if err != nil {
		return err
	}
	if err := a.env.Secrets.Set(ctx, ref, value); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "stored %s\n", ref)
	return nil
}

func (a *app) connect(ctx context.Context, args []string) error {
	fs := a.flags("connect")
	url := fs.String("url", "", "gateway URL (default http://<gateway.listen>/s/<name>)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := oneArg(pos, "connect", "server name")
	if err != nil {
		return err
	}
	target := *url
	if target == "" {
		cfg, err := config.Load(a.configPath)
		if err != nil {
			return err
		}
		if _, ok := cfg.Servers[name]; !ok {
			return fmt.Errorf("unknown server %q", name)
		}
		target = serverURL(cfg.Gateway.Listen, name)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(a.env.Stderr, nil))
	return a.env.Connect(ctx, name, target, log)
}

func (a *app) launchd() *launchd.Manager {
	return &launchd.Manager{Run: a.env.Launchctl, UID: a.env.UID, Home: a.env.Home}
}

func (a *app) daemon(ctx context.Context, args []string) error {
	fs := a.flags("daemon")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("daemon: want install, uninstall or status")
	}
	mgr := a.launchd()
	switch pos[0] {
	case "install":
		exe, err := a.env.Executable()
		if err != nil {
			return err
		}
		spec := launchd.Spec{Executable: exe, StateDir: a.stateDir, Home: a.env.Home}
		if a.configPath != a.defaultConfig {
			abs, err := filepath.Abs(a.configPath)
			if err != nil {
				return err
			}
			spec.Args = append(spec.Args, "--config", abs)
		}
		if a.stateDir != a.defaultState {
			abs, err := filepath.Abs(a.stateDir)
			if err != nil {
				return err
			}
			spec.StateDir = abs
			spec.Args = append(spec.Args, "--state-dir", abs)
		}
		path, err := mgr.Install(ctx, spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.env.Stdout, "installed %s (label %s)\n", path, launchd.Label)
		return nil
	case "uninstall":
		if err := mgr.Uninstall(ctx); err != nil {
			return err
		}
		fmt.Fprintf(a.env.Stdout, "uninstalled %s\n", launchd.Label)
		return nil
	case "status":
		info, err := mgr.Status(ctx)
		if err != nil {
			return err
		}
		w := a.env.Stdout
		fmt.Fprintf(w, "label: %s\nplist: %s\nloaded: %t\n", launchd.Label, launchd.PlistPath(a.env.Home), info.Loaded)
		if !info.Loaded {
			return errors.New("launchd agent is not loaded")
		}
		fmt.Fprintf(w, "state: %s\n", info.State)
		if info.PID > 0 {
			fmt.Fprintf(w, "pid: %d\n", info.PID)
		}
		if info.LastExit != "" {
			fmt.Fprintf(w, "last exit: %s\n", info.LastExit)
		}
		return nil
	}
	return usagef("daemon: unknown action %q", pos[0])
}

func (a *app) clients(_ context.Context, args []string) error {
	fs := a.flags("clients")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef("clients: unexpected argument %q", pos[1])
	}
	var selected []clients.Client
	if len(pos) == 1 {
		c, ok := clients.Find(pos[0])
		if !ok {
			return usagef("clients: unknown client %q (want one of %s)", pos[0], clientNames())
		}
		selected = []clients.Client{c}
	} else {
		selected = clients.All
	}

	st, err := a.offlineStatus()
	if err != nil {
		return err
	}
	var servers []clients.Server
	for _, s := range st.Servers {
		if s.Enabled {
			servers = append(servers, clients.Server{Name: s.Name, URL: s.URL})
		}
	}
	if len(servers) == 0 {
		return errors.New("no enabled servers in config")
	}
	exe, err := a.env.Executable()
	if err != nil {
		return err
	}
	for i, c := range selected {
		if len(pos) == 0 {
			if i > 0 {
				fmt.Fprintln(a.env.Stdout)
			}
			fmt.Fprintf(a.env.Stdout, "# %s: %s\n", c.Name, c.Target)
		}
		fmt.Fprint(a.env.Stdout, c.Render(servers, exe))
	}
	return nil
}
