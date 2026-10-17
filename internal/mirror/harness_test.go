package mirror

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/source"
)

type upstream struct {
	srv          *mcp.Server
	started      chan struct{}
	cancelled    chan string
	arrived      chan struct{}
	gate         chan struct{}
	subscribed   atomic.Int32
	unsubscribed atomic.Int32
	listCalls    atomic.Int32
}

var legacyProtocol bool

var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func eachProtocol(t *testing.T, fn func(t *testing.T)) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			legacyProtocol = legacy
			t.Cleanup(func() { legacyProtocol = false })
			fn(t)
		})
	}
}

func newUpstream() *upstream {
	up := &upstream{
		started:   make(chan struct{}, 8),
		cancelled: make(chan string, 8),
		arrived:   make(chan struct{}, 8),
		gate:      make(chan struct{}),
	}
	var versions []string
	if legacyProtocol {
		versions = legacyVersions
	}
	up.srv = mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, &mcp.ServerOptions{
		SupportedProtocolVersions: versions,
		SubscribeHandler: func(context.Context, *mcp.SubscribeRequest) error {
			up.subscribed.Add(1)
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error {
			up.unsubscribed.Add(1)
			return nil
		},
		CompletionHandler: func(_ context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			var values []string
			for _, n := range []string{"alice", "alex", "bob"} {
				if strings.HasPrefix(n, req.Params.Argument.Value) {
					values = append(values, n)
				}
			}
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: values, Total: len(values)}}, nil
		},
	})
	up.srv.AddReceivingMiddleware(up.rewriteToolList)

	type echoArgs struct {
		Text string `json:"text"`
	}
	mcp.AddTool(up.srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
		return text(a.Text), nil, nil
	})

	type slowArgs struct {
		Millis int `json:"millis"`
	}
	mcp.AddTool(up.srv, &mcp.Tool{Name: "slow"}, func(ctx context.Context, _ *mcp.CallToolRequest, a slowArgs) (*mcp.CallToolResult, any, error) {
		up.started <- struct{}{}
		select {
		case <-time.After(time.Duration(a.Millis) * time.Millisecond):
			return text("done"), nil, nil
		case <-ctx.Done():
			up.cancelled <- "slow"
			return nil, nil, ctx.Err()
		}
	})

	type progressArgs struct {
		Tag string `json:"tag"`
	}
	mcp.AddTool(up.srv, &mcp.Tool{Name: "progress"}, func(ctx context.Context, req *mcp.CallToolRequest, a progressArgs) (*mcp.CallToolResult, any, error) {
		up.arrived <- struct{}{}
		<-up.gate
		for i := 1; i <= 2; i++ {
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: req.Params.GetProgressToken(),
				Progress:      float64(i),
				Message:       a.Tag,
			})
			if err != nil {
				return nil, nil, err
			}
		}
		return text("progressed " + a.Tag), nil, nil
	})

	objectSchema := map[string]any{"type": "object"}
	noop := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text("ok"), nil }
	up.srv.AddTool(&mcp.Tool{Name: "noschema", InputSchema: objectSchema}, noop)
	up.srv.AddTool(&mcp.Tool{Name: "badschema", InputSchema: objectSchema}, noop)

	up.srv.AddResource(&mcp.Resource{URI: "fake://a", Name: "a", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "resource a"}}}, nil
		})
	up.srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "fake://item/{id}", Name: "item", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "item " + strings.TrimPrefix(req.Params.URI, "fake://item/")}}}, nil
		})
	up.srv.AddPrompt(&mcp.Prompt{Name: "greet", Arguments: []*mcp.PromptArgument{{Name: "name", Required: true}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["name"]}}}}, nil
		})
	return up
}

func (up *upstream) rewriteToolList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method != "tools/list" || err != nil {
			return res, err
		}
		up.listCalls.Add(1)
		lr := res.(*mcp.ListToolsResult)
		out := *lr
		out.Tools = nil
		for _, t := range lr.Tools {
			c := *t
			switch c.Name {
			case "noschema":
				c.InputSchema = nil
			case "badschema":
				c.InputSchema = map[string]any{"type": "string"}
			}
			out.Tools = append(out.Tools, &c)
		}
		return &out, nil
	}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type testSource struct {
	up *upstream

	mu       sync.Mutex
	cs       *mcp.ClientSession
	mode     string
	connects int
	fns      map[int]func(source.Event)
	next     int
	reports  []error
}

func newTestSource(up *upstream) *testSource {
	return &testSource{up: up, mode: "ok", fns: map[int]func(source.Event){}}
}

func (s *testSource) Session(ctx context.Context) (*mcp.ClientSession, error) {
	s.mu.Lock()
	switch s.mode {
	case "disabled":
		s.mu.Unlock()
		return nil, source.ErrDisabled
	case "backoff":
		s.mu.Unlock()
		return nil, &source.BackoffError{Last: errors.New("boom"), RetryAt: time.Now().Add(30 * time.Second)}
	}
	if s.cs != nil {
		cs := s.cs
		s.mu.Unlock()
		return cs, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "gw", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			s.emit(source.Event{Kind: source.ToolsChanged})
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			s.emit(source.Event{Kind: source.PromptsChanged})
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			s.emit(source.Event{Kind: source.ResourcesChanged})
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			s.emit(source.Event{Kind: source.ResourceUpdated, URI: req.Params.URI})
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			s.emit(source.Event{Kind: source.Progress, Progress: req.Params})
		},
	})
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := s.up.srv.Connect(context.Background(), t1, nil); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.cs = cs
	s.connects++
	s.mu.Unlock()
	s.emit(source.Event{Kind: source.Connected})
	return cs, nil
}

func (s *testSource) Subscribe(fn func(source.Event)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	s.fns[id] = fn
	return func() {
		s.mu.Lock()
		delete(s.fns, id)
		s.mu.Unlock()
	}
}

func (s *testSource) ReportCallResult(err error) {
	s.mu.Lock()
	s.reports = append(s.reports, err)
	s.mu.Unlock()
}

func (s *testSource) emit(ev source.Event) {
	s.mu.Lock()
	fns := make([]func(source.Event), 0, len(s.fns))
	for _, fn := range s.fns {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

func (s *testSource) stop() {
	s.mu.Lock()
	s.mode = "disabled"
	cs := s.cs
	s.cs = nil
	s.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
	s.emit(source.Event{Kind: source.Stopped})
}

func (s *testSource) setMode(mode string) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *testSource) connectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects
}

func (s *testSource) reported() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.reports...)
}

type harness struct {
	up  *upstream
	src *testSource
	m   *Mirror
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	up := newUpstream()
	src := newTestSource(up)
	if legacyProtocol {
		opts.supportedVersions = legacyVersions
	}
	m := New("fake", src, opts)
	t.Cleanup(m.Close)
	return &harness{up: up, src: src, m: m}
}

type downstream struct {
	*mcp.ClientSession
	toolChanges atomic.Int32
	mu          sync.Mutex
	updated     []string
	progress    []*mcp.ProgressNotificationParams
}

func (d *downstream) updates() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.updated...)
}

func (d *downstream) progressed() []*mcp.ProgressNotificationParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*mcp.ProgressNotificationParams(nil), d.progress...)
}

func (h *harness) client(t *testing.T) *downstream {
	t.Helper()
	d := &downstream{}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { d.toolChanges.Add(1) },
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			d.mu.Lock()
			d.updated = append(d.updated, req.Params.URI)
			d.mu.Unlock()
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			d.mu.Lock()
			d.progress = append(d.progress, req.Params)
			d.mu.Unlock()
		},
	})
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := h.m.Server().Connect(t.Context(), t1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(t.Context(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.ClientSession = cs
	t.Cleanup(func() { _ = cs.Close() })
	return d
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	out := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = tool
	}
	return out
}
