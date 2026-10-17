package upstream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mcpctl/internal/config"
	"mcpctl/internal/source"
)

type Options struct {
	StateDir       string
	Resolve        func(ref string) (string, error)
	ConnectTimeout time.Duration
	Logger         *slog.Logger

	backoffBase  time.Duration
	backoffMax   time.Duration
	healthyAfter time.Duration
	stopGrace    time.Duration
}

type Changes struct {
	Added, Removed, Started, Stopped, Replaced []string
}

type Status struct {
	Name      string
	Enabled   bool
	State     string
	PID, PGID int
	Restarts  int
	Since     time.Time
	LastError string
	RetryAt   *time.Time
}

type Manager struct {
	opts    Options
	applyMu sync.Mutex

	mu      sync.Mutex
	handles map[string]*handle
}

func NewManager(opts Options) *Manager {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.backoffBase <= 0 {
		opts.backoffBase = time.Second
	}
	if opts.backoffMax <= 0 {
		opts.backoffMax = 60 * time.Second
	}
	if opts.healthyAfter <= 0 {
		opts.healthyAfter = 60 * time.Second
	}
	if opts.stopGrace <= 0 {
		opts.stopGrace = 2 * time.Second
	}
	return &Manager{opts: opts, handles: make(map[string]*handle)}
}

func (m *Manager) handle(name string) *handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.handles[name]
	if h == nil {
		h = &handle{m: m, name: name}
		m.handles[name] = h
	}
	return h
}

func (m *Manager) Source(name string) source.Source { return m.handle(name) }

func (m *Manager) commandBase(name string) string {
	m.mu.Lock()
	h := m.handles[name]
	m.mu.Unlock()
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return filepath.Base(h.cfg.Command)
}

type pendingStop struct {
	g         *generation
	h         *handle
	emitEvent bool
}

func (m *Manager) Apply(desired map[string]config.Server) Changes {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	var ch Changes
	var stops []pendingStop

	for name, cfg := range desired {
		cfg.Name = name
		h := m.handle(name)
		h.mu.Lock()
		switch {
		case !h.configured:
			ch.Added = append(ch.Added, name)
			h.configured, h.cfg = true, cfg
			if cfg.Enabled {
				h.gen = h.newGen()
			}
		default:
			old := h.cfg
			transport := !old.SameTransport(cfg)
			if transport {
				ch.Replaced = append(ch.Replaced, name)
			}
			h.cfg = cfg
			switch {
			case old.Enabled && !cfg.Enabled:
				ch.Stopped = append(ch.Stopped, name)
				stops = append(stops, pendingStop{h.gen, h, true})
			case !old.Enabled && cfg.Enabled:
				ch.Started = append(ch.Started, name)
				h.gen = h.newGen()
			case old.Enabled && cfg.Enabled && transport:
				stops = append(stops, pendingStop{h.gen, h, true})
				h.gen = h.newGen()
			}
		}
		h.mu.Unlock()
	}

	m.mu.Lock()
	all := make([]*handle, 0, len(m.handles))
	for _, h := range m.handles {
		all = append(all, h)
	}
	m.mu.Unlock()
	for _, h := range all {
		if _, ok := desired[h.name]; ok {
			continue
		}
		h.mu.Lock()
		if h.configured {
			ch.Removed = append(ch.Removed, h.name)
			h.configured = false
			if h.cfg.Enabled {
				stops = append(stops, pendingStop{h.gen, h, true})
			}
			h.cfg.Enabled = false
		}
		h.mu.Unlock()
	}

	runStops(stops)
	for _, s := range [][]string{ch.Added, ch.Removed, ch.Started, ch.Stopped, ch.Replaced} {
		sort.Strings(s)
	}
	return ch
}

func runStops(stops []pendingStop) {
	var wg sync.WaitGroup
	for _, s := range stops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.g.stop()
			if s.emitEvent {
				s.h.emit(source.Event{Kind: source.Stopped})
			}
		}()
	}
	wg.Wait()
}

func (m *Manager) Status() []Status {
	m.mu.Lock()
	hs := make([]*handle, 0, len(m.handles))
	for _, h := range m.handles {
		hs = append(hs, h)
	}
	m.mu.Unlock()

	var out []Status
	for _, h := range hs {
		h.mu.Lock()
		configured, enabled, g, restarts := h.configured, h.cfg.Enabled, h.gen, h.restarts
		h.mu.Unlock()
		if !configured {
			continue
		}
		st := Status{State: stateDisabled}
		if enabled && g != nil {
			st = g.status()
		}
		st.Name, st.Enabled, st.Restarts = h.name, enabled, restarts
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (m *Manager) Restart(name string) error {
	h := m.handle(name)
	h.mu.Lock()
	switch {
	case !h.configured:
		h.mu.Unlock()
		return source.ErrUnknown
	case !h.cfg.Enabled:
		h.mu.Unlock()
		return source.ErrDisabled
	}
	old := h.gen
	h.gen = h.newGen()
	h.restarts++
	h.mu.Unlock()
	old.stop()
	h.emit(source.Event{Kind: source.Stopped})
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	hs := make([]*handle, 0, len(m.handles))
	for _, h := range m.handles {
		hs = append(hs, h)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, h := range hs {
		h.mu.Lock()
		g := h.gen
		h.configured = false
		h.mu.Unlock()
		if g == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.stop()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown: %w", ctx.Err())
	}
}
