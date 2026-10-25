package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/hoanglong2311/mcpgw/internal/bridge"
	"github.com/hoanglong2311/mcpgw/internal/cli"
	"github.com/hoanglong2311/mcpgw/internal/control"
	"github.com/hoanglong2311/mcpgw/internal/daemon"
	"github.com/hoanglong2311/mcpgw/internal/launchd"
	"github.com/hoanglong2311/mcpgw/internal/secrets"
)

var Version = "dev"

func main() {
	home, _ := os.UserHomeDir()
	env := cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version(),
		Home:    home,
		UID:     os.Getuid(),
		NewControl: func(socketPath string) cli.Control {
			return control.NewClient(socketPath)
		},
		Launchctl:  launchd.ExecRunner,
		Secrets:    secrets.New(),
		ReadSecret: cli.ReadSecret,
		Serve: func(ctx context.Context, o cli.ServeOptions) error {
			return daemon.Run(ctx, daemon.Options{ConfigPath: o.ConfigPath, StateDir: o.StateDir, Version: o.Version, Logger: o.Logger})
		},
		Connect:    bridge.Run,
		Executable: executable,
	}
	os.Exit(cli.Run(context.Background(), env, os.Args[1:]))
}

func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
