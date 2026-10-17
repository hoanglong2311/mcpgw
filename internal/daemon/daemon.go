package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"mcpctl/internal/config"
	"mcpctl/internal/control"
	"mcpctl/internal/gateway"
	"mcpctl/internal/mirror"
	"mcpctl/internal/source"
	"mcpctl/internal/upstream"
)

type entry struct {
	mirror    *mirror.Mirror
	cancelSub func()
}

type daemon struct {
	ctx       context.Context
	opts      Options
	log       *slog.Logger
	mgr       *upstream.Manager
	gw        *gateway.Gateway
	startedAt time.Time
	listen    string

	mu      sync.Mutex
	cfg     *config.Config
	entries map[string]*entry
}

func newDaemon(ctx context.Context, opts Options, mgr *upstream.Manager, cfg *config.Config, gw *gateway.Gateway) *daemon {
	return &daemon{
		ctx:       ctx,
		opts:      opts,
		log:       opts.Logger,
		mgr:       mgr,
		gw:        gw,
		startedAt: time.Now(),
		listen:    cfg.Gateway.Listen,
		cfg:       cfg,
		entries:   make(map[string]*entry),
	}
}

func (d *daemon) apply(cfg *config.Config, toggles config.Toggles) {
	for name := range cfg.Servers {
		if d.entries[name] == nil {
			d.addMirror(name, cfg.Gateway)
		}
	}
	changes := d.mgr.Apply(config.Effective(cfg, toggles))
	for _, name := range changes.Removed {
		d.removeMirror(name)
	}
	d.cfg = cfg

	eff := config.Effective(cfg, toggles)
	warm := slices.Concat(changes.Started, changes.Replaced)
	for _, name := range warm {
		if eff[name].Enabled {
			d.warm(name)
		}
	}
}

func (d *daemon) addMirror(name string, gc config.Gateway) {
	src := d.mgr.Source(name)
	m := mirror.New(name, src, mirror.Options{
		CallTimeout:  gc.CallTimeout,
		Instructions: d.readInstructions(name),
		Logger:       d.log,
	})
	cancel := src.Subscribe(func(ev source.Event) {
		if ev.Kind == source.Connected {
			d.writeInstructions(name, ev.Instructions)
		}
	})
	d.entries[name] = &entry{mirror: m, cancelSub: cancel}
	d.gw.Add(name, m.Server())
}

func (d *daemon) removeMirror(name string) {
	e := d.entries[name]
	if e == nil {
		return
	}
	delete(d.entries, name)
	d.gw.Remove(name)
	e.cancelSub()
	e.mirror.Close()
}

func (d *daemon) closeMirrors() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(d.entries)) {
		d.removeMirror(name)
	}
}

func (d *daemon) warm(name string) {
	e := d.entries[name]
	if e == nil || e.mirror.Sessions() == 0 {
		return
	}
	src := d.mgr.Source(name)
	timeout := d.cfg.Gateway.ConnectTimeout
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, timeout)
		defer cancel()
		if _, err := src.Session(ctx); err != nil {
			d.log.Warn("reconnect after enable", "server", name, "err", err)
		}
	}()
}

func (d *daemon) cachePath(name string) string {
	return filepath.Join(d.opts.StateDir, "cache", name+".json")
}

type cacheFile struct {
	Instructions string `json:"instructions"`
}

func (d *daemon) readInstructions(name string) string {
	data, err := os.ReadFile(d.cachePath(name))
	if err != nil {
		return ""
	}
	var c cacheFile
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.Instructions
}

func (d *daemon) writeInstructions(name, instructions string) {
	data, err := json.Marshal(cacheFile{Instructions: instructions})
	if err != nil {
		return
	}
	path := d.cachePath(name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		d.log.Warn("write instructions cache", "server", name, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		d.log.Warn("write instructions cache", "server", name, "err", err)
	}
}

func (d *daemon) reload() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reloadLocked()
}

func (d *daemon) reloadLocked() error {
	cfg, err := config.Load(d.opts.ConfigPath)
	if err != nil {
		return err
	}
	toggles, err := config.LoadToggles(d.opts.StateDir)
	if err != nil {
		return err
	}
	if cfg.Gateway.Listen != d.listen {
		d.log.Warn("gateway.listen changed; restart the daemon to apply", "current", d.listen, "new", cfg.Gateway.Listen)
	}
	d.apply(cfg, toggles)
	d.log.Info("config reloaded", "servers", len(cfg.Servers))
	return nil
}

type fileSig struct {
	mod  time.Time
	size int64
}

func statSig(path string) (fileSig, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return fileSig{}, false
	}
	return fileSig{st.ModTime(), st.Size()}, true
}

func (d *daemon) poll(ctx context.Context) {
	last, _ := statSig(d.opts.ConfigPath)
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sig, ok := statSig(d.opts.ConfigPath)
		if !ok || sig == last {
			continue
		}
		last = sig
		if err := d.reload(); err != nil {
			d.log.Error("reload config failed; keeping previous config", "err", err)
		}
	}
}

func (d *daemon) Status(context.Context) *control.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusLocked()
}

func (d *daemon) statusLocked() *control.Status {
	st := &control.Status{
		Version:   d.opts.Version,
		PID:       os.Getpid(),
		Listen:    d.listen,
		StartedAt: d.startedAt,
		Servers:   []control.ServerStatus{},
	}
	for _, us := range d.mgr.Status() {
		st.Servers = append(st.Servers, d.serverStatusLocked(us))
	}
	return st
}

func (d *daemon) serverStatusLocked(us upstream.Status) control.ServerStatus {
	ss := control.ServerStatus{
		Name:      us.Name,
		URL:       fmt.Sprintf("http://%s/s/%s", d.listen, us.Name),
		Enabled:   us.Enabled,
		State:     us.State,
		PID:       us.PID,
		PGID:      us.PGID,
		Restarts:  us.Restarts,
		Since:     us.Since,
		LastError: us.LastError,
		RetryAt:   us.RetryAt,
	}
	if e := d.entries[us.Name]; e != nil {
		ss.Sessions = e.mirror.Sessions()
	}
	return ss
}

func (d *daemon) serverStatusLockedByName(name string) (*control.ServerStatus, error) {
	for _, us := range d.mgr.Status() {
		if us.Name == name {
			ss := d.serverStatusLocked(us)
			return &ss, nil
		}
	}
	return nil, source.ErrUnknown
}

func (d *daemon) Reload(context.Context) (*control.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.reloadLocked(); err != nil {
		return nil, err
	}
	return d.statusLocked(), nil
}

func (d *daemon) Enable(_ context.Context, name string) (*control.ServerStatus, error) {
	return d.setEnabled(name, true)
}

func (d *daemon) Disable(_ context.Context, name string) (*control.ServerStatus, error) {
	return d.setEnabled(name, false)
}

func (d *daemon) setEnabled(name string, on bool) (*control.ServerStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.cfg.Servers[name]; !ok {
		return nil, source.ErrUnknown
	}
	toggles, err := config.LoadToggles(d.opts.StateDir)
	if err != nil {
		return nil, err
	}
	toggles[name] = on
	if err := config.SaveToggles(d.opts.StateDir, toggles); err != nil {
		return nil, err
	}
	if err := d.reloadLocked(); err != nil {
		return nil, err
	}
	return d.serverStatusLockedByName(name)
}

func (d *daemon) Restart(_ context.Context, name string) (*control.ServerStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.mgr.Restart(name); err != nil {
		return nil, err
	}
	d.warm(name)
	return d.serverStatusLockedByName(name)
}
