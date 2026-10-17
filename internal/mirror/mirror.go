package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/source"
)

const codeUnavailable = -32001

const defaultCallTimeout = 120 * time.Second

type Options struct {
	CallTimeout  time.Duration
	Instructions string
	Logger       *slog.Logger

	supportedVersions []string
}

type Mirror struct {
	name   string
	src    source.Source
	opts   Options
	log    *slog.Logger
	server *mcp.Server

	cancelSub func()
	closeOnce sync.Once

	mu        sync.Mutex
	gen       uint64
	fresh     bool
	inflight  *syncCall
	tools     map[string]string
	resources map[string]string
	templates map[string]string
	prompts   map[string]string

	progressSeq atomic.Uint64
	pmu         sync.Mutex
	progress    map[string]progressRoute

	smu      sync.Mutex
	subs     map[string]map[*mcp.ServerSession]struct{}
	watching map[*mcp.ServerSession]struct{}
}

type progressRoute struct {
	session *mcp.ServerSession
	token   any
}

func New(name string, src source.Source, opts Options) *Mirror {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Mirror{
		name:      name,
		src:       src,
		opts:      opts,
		log:       log.With("mirror", name),
		tools:     map[string]string{},
		resources: map[string]string{},
		templates: map[string]string{},
		prompts:   map[string]string{},
		progress:  map[string]progressRoute{},
		subs:      map[string]map[*mcp.ServerSession]struct{}{},
		watching:  map[*mcp.ServerSession]struct{}{},
	}
	m.server = mcp.NewServer(&mcp.Implementation{Name: "mcpctl-" + name, Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions:              opts.Instructions,
		SupportedProtocolVersions: opts.supportedVersions,
		Capabilities: &mcp.ServerCapabilities{
			Tools:       &mcp.ToolCapabilities{ListChanged: true},
			Resources:   &mcp.ResourceCapabilities{Subscribe: true, ListChanged: true},
			Prompts:     &mcp.PromptCapabilities{ListChanged: true},
			Completions: &mcp.CompletionCapabilities{},
		},
		CompletionHandler:  m.complete,
		SubscribeHandler:   m.subscribe,
		UnsubscribeHandler: m.unsubscribe,
	})
	m.server.AddReceivingMiddleware(m.ensureSyncedMiddleware)
	m.cancelSub = src.Subscribe(m.onEvent)
	return m
}

func (m *Mirror) Server() *mcp.Server { return m.server }

func (m *Mirror) Sessions() int {
	n := 0
	for range m.server.Sessions() {
		n++
	}
	return n
}

func (m *Mirror) Close() {
	m.closeOnce.Do(func() {
		m.cancelSub()
		for ss := range m.server.Sessions() {
			_ = ss.Close()
		}
	})
}

func (m *Mirror) ensureSyncedMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if needsSync(method) {
			if err := m.ensureSynced(ctx); err != nil {
				return nil, m.rpcError(err)
			}
		}
		return next(ctx, method, req)
	}
}

func needsSync(method string) bool {
	return strings.HasPrefix(method, "tools/") ||
		strings.HasPrefix(method, "resources/") ||
		strings.HasPrefix(method, "prompts/") ||
		method == "completion/complete"
}

func (m *Mirror) rpcError(err error) error {
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return je
	}
	var be *source.BackoffError
	switch {
	case errors.As(err, &be):
		return &jsonrpc.Error{Code: codeUnavailable, Message: fmt.Sprintf(
			"upstream %q is unavailable: backing off until %s (retry in %s): %v",
			m.name, be.RetryAt.Format(time.RFC3339), time.Until(be.RetryAt).Round(time.Second), be.Last)}
	case errors.Is(err, source.ErrDisabled):
		return &jsonrpc.Error{Code: codeUnavailable, Message: fmt.Sprintf(
			"upstream %q is disabled; run `mcpctl enable %s`", m.name, m.name)}
	case errors.Is(err, context.DeadlineExceeded):
		return &jsonrpc.Error{Code: codeUnavailable, Message: fmt.Sprintf("upstream %q: timed out: %v", m.name, err)}
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: fmt.Sprintf("upstream %q: %v", m.name, err)}
}

func (m *Mirror) onEvent(ev source.Event) {
	switch ev.Kind {
	case source.ToolsChanged, source.PromptsChanged, source.ResourcesChanged, source.Connected:
		m.invalidate()
		if m.Sessions() > 0 {
			go m.resync(ev.Kind == source.Connected)
		}
	case source.Stopped:
		m.invalidate()
		m.clearRegistry()
	case source.ResourceUpdated:
		if err := m.server.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: ev.URI}); err != nil {
			m.log.Warn("forward resource update", "uri", ev.URI, "err", err)
		}
	case source.Progress:
		m.routeProgress(ev.Progress)
	}
}

func (m *Mirror) invalidate() {
	m.mu.Lock()
	m.gen++
	m.fresh = false
	m.mu.Unlock()
}

func (m *Mirror) resync(resubscribe bool) {
	if err := m.ensureSynced(context.Background()); err != nil {
		m.log.Warn("background resync failed", "err", err)
		return
	}
	if resubscribe {
		m.resubscribe()
	}
}

func (m *Mirror) routeProgress(p *mcp.ProgressNotificationParams) {
	if p == nil {
		return
	}
	key, ok := p.ProgressToken.(string)
	if !ok {
		return
	}
	m.pmu.Lock()
	route, ok := m.progress[key]
	m.pmu.Unlock()
	if !ok {
		return
	}
	out := *p
	out.ProgressToken = route.token
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := route.session.NotifyProgress(ctx, &out); err != nil {
		m.log.Debug("forward progress", "err", err)
	}
}
