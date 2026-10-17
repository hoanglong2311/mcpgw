package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/source"
)

type echoArgs struct {
	Text string `json:"text"`
}

func newGateway(t *testing.T) (*httptest.Server, *mcp.Server) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "gw", Version: "1"}, &mcp.ServerOptions{Instructions: "gateway says hi"})
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: a.Text}}}, nil, nil
	})
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(h.Close)
	return h, srv
}

func connectDownstream(t *testing.T, src source.Source) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ct, st := mcp.NewInMemoryTransports()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(ctx, "gw", src, st, nil)
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "desktop", Version: "1"}, nil).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: legacyProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

func callEcho(t *testing.T, cs *mcp.ClientSession, text string) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestBridgeMirrorsGatewayTool(t *testing.T) {
	gw, _ := newGateway(t)
	src := NewSource(gw.URL, nil)
	defer src.Close()
	cs := connectDownstream(t, src)

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
	if got := callEcho(t, cs, "hello"); got != "hello" {
		t.Fatalf("echo = %q", got)
	}
}

func TestBridgeLazyConnectAndEvents(t *testing.T) {
	gw, srv := newGateway(t)
	src := NewSource(gw.URL, nil)
	defer src.Close()

	events := make(chan source.Event, 16)
	src.Subscribe(func(ev source.Event) { events <- ev })

	cs, err := src.Session(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, source.Connected)
	if ev.Instructions != "gateway says hi" {
		t.Fatalf("instructions = %q", ev.Instructions)
	}
	if again, _ := src.Session(t.Context()); again != cs {
		t.Fatal("session not reused")
	}

	mcp.AddTool(srv, &mcp.Tool{Name: "extra", Description: "extra"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	waitEvent(t, events, source.ToolsChanged)
}

func TestBridgeReconnects(t *testing.T) {
	var down atomic.Bool
	srv := mcp.NewServer(&mcp.Implementation{Name: "gw", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: a.Text}}}, nil, nil
	})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer gw.Close()

	src := NewSource(gw.URL, nil)
	defer src.Close()
	cs := connectDownstream(t, src)
	if got := callEcho(t, cs, "one"); got != "one" {
		t.Fatalf("echo = %q", got)
	}

	down.Store(true)
	first, _ := src.Session(t.Context())
	if first != nil {
		_ = first.Close()
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := src.Session(t.Context()); err == nil {
		t.Fatal("expected connect failure while gateway is down")
	}

	down.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := src.Session(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reconnected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := callEcho(t, cs, "two"); got != "two" {
		t.Fatalf("echo after reconnect = %q", got)
	}
}

func TestBridgeConnectFailureIsLazy(t *testing.T) {
	src := NewSource("http://127.0.0.1:1/s/none", nil)
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := src.Session(ctx); err == nil {
		t.Fatal("expected error")
	}
}

func waitEvent(t *testing.T, ch <-chan source.Event, kind source.EventKind) source.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %v", kind)
		}
	}
}
