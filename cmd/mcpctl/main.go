package main

import (
	"context"
	"os"
	"path/filepath"

	"mcpctl/internal/bridge"
	"mcpctl/internal/cli"
	"mcpctl/internal/control"
	"mcpctl/internal/daemon"
	"mcpctl/internal/launchd"
	"mcpctl/internal/secrets"
)

var Version = "dev"

func main() {
	home, _ := os.UserHomeDir()
	env := cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: Version,
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

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
