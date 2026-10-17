package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mcpctl/internal/config"
	"mcpctl/internal/control"
	"mcpctl/internal/gateway"
	"mcpctl/internal/rotate"
	"mcpctl/internal/secrets"
	"mcpctl/internal/upstream"
)

const (
	pollInterval    = 2 * time.Second
	shutdownTimeout = 10 * time.Second
)

type Options struct {
	ConfigPath, StateDir, Version string
	Logger                        *slog.Logger
}

func Run(ctx context.Context, opts Options) error {
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultPath()
	}
	if opts.StateDir == "" {
		opts.StateDir = config.DefaultStateDir()
	}
	for _, sub := range []string{"", "logs", "cache", "run"} {
		if err := os.MkdirAll(filepath.Join(opts.StateDir, sub), 0o700); err != nil {
			return err
		}
	}
	if opts.Logger == nil {
		lf, err := rotate.Open(filepath.Join(opts.StateDir, "logs", "daemon.log"), rotate.DefaultMaxSize)
		if err != nil {
			return err
		}
		defer lf.Close()
		opts.Logger = slog.New(slog.NewTextHandler(lf, nil))
	}
	log := opts.Logger

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	toggles, err := config.LoadToggles(opts.StateDir)
	if err != nil {
		return err
	}

	sock := control.SocketPath(opts.StateDir)
	ctlLn, err := control.Listen(sock)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer os.Remove(sock)
	defer ctlLn.Close()

	httpLn, err := net.Listen("tcp", cfg.Gateway.Listen)
	if err != nil {
		return fmt.Errorf("gateway listen %s: %w", cfg.Gateway.Listen, err)
	}
	defer httpLn.Close()

	mgr := upstream.NewManager(upstream.Options{
		StateDir:       opts.StateDir,
		Resolve:        secrets.New().Resolve,
		ConnectTimeout: cfg.Gateway.ConnectTimeout,
		Logger:         log,
	})
	if err := mgr.ReapStale(); err != nil {
		log.Warn("reap stale upstream processes", "err", err)
	}

	d := newDaemon(ctx, opts, mgr, cfg, gateway.New(cfg.Gateway.SessionIdleTimeout))
	d.apply(cfg, toggles)
	log.Info("daemon started", "version", opts.Version, "listen", cfg.Gateway.Listen, "servers", len(cfg.Servers))

	httpSrv := &http.Server{Handler: d.gw, ReadHeaderTimeout: 10 * time.Second}
	ctlSrv := &http.Server{Handler: control.NewHandler(d), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- httpSrv.Serve(httpLn) }()
	go func() { errc <- ctlSrv.Serve(ctlLn) }()
	go d.poll(ctx)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve: %w", err)
		}
	}

	log.Info("daemon stopping")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- httpSrv.Shutdown(sctx) }()
	d.closeMirrors()
	if err := <-httpDone; err != nil {
		httpSrv.Close()
	}
	_ = ctlSrv.Close()
	if err := mgr.Shutdown(sctx); err != nil {
		log.Warn("upstream shutdown", "err", err)
	}
	log.Info("daemon stopped")
	return runErr
}
