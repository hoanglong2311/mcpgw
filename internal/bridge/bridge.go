package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoanglong2311/mcpgw/internal/mirror"
	"github.com/hoanglong2311/mcpgw/internal/source"
)

const legacyProtocolVersion = "2025-11-25"

type Source struct {
	url string
	log *slog.Logger

	connMu sync.Mutex

	mu      sync.Mutex
	sess    *mcp.ClientSession
	subs    map[int]func(source.Event)
	nextSub int
}

var _ source.Source = (*Source)(nil)

func NewSource(url string, log *slog.Logger) *Source {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Source{url: url, log: log, subs: map[int]func(source.Event){}}
}

func (s *Source) Session(ctx context.Context) (*mcp.ClientSession, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	cur := s.sess
	s.mu.Unlock()
	if cur != nil {
		return cur, nil
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "mcpctl-bridge", Version: "0"}, s.clientOptions())
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.url}, &mcp.ClientSessionOptions{ProtocolVersion: legacyProtocolVersion})
	if err != nil {
		s.log.Warn("gateway connect failed", "url", s.url, "err", err)
		return nil, err
	}
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()
	go s.watch(sess)

	var instr string
	if r := sess.InitializeResult(); r != nil {
		instr = r.Instructions
	}
	s.log.Info("connected to gateway", "url", s.url)
	s.emit(source.Event{Kind: source.Connected, Instructions: instr})
	return sess, nil
}

func (s *Source) watch(sess *mcp.ClientSession) {
	_ = sess.Wait()
	if s.drop(sess) {
		s.log.Warn("gateway session closed", "url", s.url)
		s.emit(source.Event{Kind: source.Stopped})
	}
}

func (s *Source) drop(sess *mcp.ClientSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != sess {
		return false
	}
	s.sess = nil
	return true
}

func (s *Source) ReportCallResult(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return
	}
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
}

func (s *Source) Subscribe(fn func(source.Event)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Source) emit(ev source.Event) {
	s.mu.Lock()
	fns := make([]func(source.Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

func (s *Source) Close() {
	s.mu.Lock()
	sess := s.sess
	s.sess = nil
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
}

func (s *Source) clientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			s.emit(source.Event{Kind: source.ToolsChanged})
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			s.emit(source.Event{Kind: source.PromptsChanged})
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			s.emit(source.Event{Kind: source.ResourcesChanged})
		},
		ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) {
			s.emit(source.Event{Kind: source.ResourceUpdated, URI: r.Params.URI})
		},
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			s.emit(source.Event{Kind: source.Progress, Progress: r.Params})
		},
	}
}

func Serve(ctx context.Context, name string, src source.Source, t mcp.Transport, log *slog.Logger) error {
	m := mirror.New(name, src, mirror.Options{Logger: log})
	defer m.Close()
	return m.Server().Run(ctx, t)
}

func Run(ctx context.Context, name, url string, log *slog.Logger) error {
	src := NewSource(url, log)
	defer src.Close()
	return Serve(ctx, name, src, &mcp.StdioTransport{}, log)
}
