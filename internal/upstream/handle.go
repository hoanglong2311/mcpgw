package upstream

import (
	"context"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoanglong2311/mcpgw/internal/config"
	"github.com/hoanglong2311/mcpgw/internal/source"
)

type handle struct {
	m    *Manager
	name string

	mu         sync.Mutex
	configured bool
	cfg        config.Server
	gen        *generation
	restarts   int
	subs       map[int]func(source.Event)
	nextSub    int
}

var _ source.Source = (*handle)(nil)

func (h *handle) Session(ctx context.Context) (*mcp.ClientSession, error) {
	for {
		h.mu.Lock()
		configured, enabled, g := h.configured, h.cfg.Enabled, h.gen
		h.mu.Unlock()
		switch {
		case !configured:
			return nil, source.ErrUnknown
		case !enabled:
			return nil, source.ErrDisabled
		}
		s, err := g.session(ctx)
		if errors.Is(err, errGenStopped) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return s, err
	}
}

func (h *handle) Subscribe(fn func(source.Event)) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = make(map[int]func(source.Event))
	}
	id := h.nextSub
	h.nextSub++
	h.subs[id] = fn
	return func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

func (h *handle) emit(ev source.Event) {
	h.mu.Lock()
	fns := make([]func(source.Event), 0, len(h.subs))
	for _, fn := range h.subs {
		fns = append(fns, fn)
	}
	h.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

func (h *handle) ReportCallResult(err error) {
	h.mu.Lock()
	g := h.gen
	enabled := h.configured && h.cfg.Enabled
	h.mu.Unlock()
	if g == nil || !enabled {
		return
	}
	if g.reportCall(err) {
		go h.restartGen(g)
	}
}

func (h *handle) newGen() *generation {
	var prev <-chan struct{}
	if h.gen != nil {
		prev = h.gen.done
	}
	return newGeneration(h, h.cfg, prev)
}

func (h *handle) restartGen(g *generation) {
	h.mu.Lock()
	if h.gen != g || !h.configured || !h.cfg.Enabled {
		h.mu.Unlock()
		return
	}
	h.gen = h.newGen()
	h.restarts++
	h.mu.Unlock()
	g.stop()
	h.emit(source.Event{Kind: source.Stopped})
}
