package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoanglong2311/mcpgw/internal/source"
)

func TestForwardMetaDropsReservedKeys(t *testing.T) {
	in := mcp.Meta{
		"progressToken":                        "t",
		mcp.MetaKeyProtocolVersion:             "2026-07-28",
		mcp.MetaKeyClientInfo:                  map[string]any{"name": "c"},
		mcp.MetaKeyClientCapabilities:          map[string]any{},
		mcp.MetaKeyLogLevel:                    "info",
		"io.modelcontextprotocol/anythingElse": 1,
		"custom":                               "keep",
	}
	if got, want := forwardMeta(in), (mcp.Meta{"custom": "keep"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := forwardMeta(mcp.Meta{mcp.MetaKeyProtocolVersion: "x"}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

type httpSource struct {
	url string
	cs  *mcp.ClientSession
}

func (s *httpSource) Session(ctx context.Context) (*mcp.ClientSession, error) {
	if s.cs != nil {
		return s.cs, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "gw", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.url}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		return nil, err
	}
	s.cs = cs
	return cs, nil
}

func (s *httpSource) Subscribe(func(source.Event)) func() { return func() {} }
func (s *httpSource) ReportCallResult(error)              {}

func TestCurrentProtocolClientThroughStatefulLegacyUpstream(t *testing.T) {
	up := newUpstream()
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up.srv }, nil))
	src := &httpSource{url: ts.URL}
	m := New("fake", src, Options{})
	t.Cleanup(func() {
		m.Close()
		if src.cs != nil {
			_ = src.cs.Close()
		}
		ts.CloseClientConnections()
		ts.Close()
	})
	h := &harness{up: up, m: m}
	c := h.client(t)
	if v := c.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Fatalf("downstream protocol = %s", v)
	}
	res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "via http"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(t, res); got != "via http" {
		t.Fatalf("echo = %q", got)
	}
	rr, err := c.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fake://a"})
	if err != nil || rr.Contents[0].Text != "resource a" {
		t.Fatalf("read = %v, %v", rr, err)
	}
	if _, err := c.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "x"}}); err != nil {
		t.Fatal(err)
	}
}
