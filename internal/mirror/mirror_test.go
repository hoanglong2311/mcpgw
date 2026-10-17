package mirror

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %v", res.Content)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestListAndCallTools(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		tools := toolNames(t, c.ClientSession)
		for _, name := range []string{"echo", "slow", "progress", "noschema", "badschema"} {
			if tools[name] == nil {
				t.Errorf("tool %q missing from %v", name, tools)
			}
		}
		res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resultText(t, res); got != "hi" {
			t.Fatalf("echo = %q", got)
		}
	})
}

func TestCallToolBeforeList(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "cold"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resultText(t, res); got != "cold" {
			t.Fatalf("echo = %q", got)
		}
		if _, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("unknown tool error = %v", err)
		}
	})
}

func TestToolSchemaNormalised(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		tools := toolNames(t, c.ClientSession)
		for _, name := range []string{"noschema", "badschema"} {
			schema, ok := tools[name].InputSchema.(map[string]any)
			if !ok || schema["type"] != "object" {
				t.Errorf("%s schema = %#v", name, tools[name].InputSchema)
			}
		}
	})
}

func TestConcurrentFirstUseSharesOneSync(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := c.ListTools(t.Context(), nil); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if n := h.up.listCalls.Load(); n != 1 {
			t.Fatalf("upstream tools/list calls = %d, want 1", n)
		}
		if n := h.src.connectCount(); n != 1 {
			t.Fatalf("connects = %d, want 1", n)
		}
	})
}

func TestResources(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		res, err := c.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fake://a"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Contents[0].Text != "resource a" {
			t.Fatalf("read a = %q", res.Contents[0].Text)
		}
		res, err = c.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fake://item/7"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Contents[0].Text != "item 7" {
			t.Fatalf("read item = %q", res.Contents[0].Text)
		}
		var uris []string
		for r, err := range c.Resources(t.Context(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			uris = append(uris, r.URI)
		}
		if len(uris) != 1 || uris[0] != "fake://a" {
			t.Fatalf("resources = %v", uris)
		}
		var templates []string
		for r, err := range c.ResourceTemplates(t.Context(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			templates = append(templates, r.URITemplate)
		}
		if len(templates) != 1 || templates[0] != "fake://item/{id}" {
			t.Fatalf("templates = %v", templates)
		}
	})
}

func TestSubscribeDeliversOnlyToSubscriber(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c1, c2 := h.client(t), h.client(t)
		if err := c1.Subscribe(t.Context(), &mcp.SubscribeParams{URI: "fake://a"}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "upstream subscribed", func() bool { return h.up.subscribed.Load() == 1 })
		if err := h.up.srv.ResourceUpdated(t.Context(), &mcp.ResourceUpdatedNotificationParams{URI: "fake://a"}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "update on subscriber", func() bool { return len(c1.updates()) == 1 })
		time.Sleep(100 * time.Millisecond)
		if got := c2.updates(); len(got) != 0 {
			t.Fatalf("non-subscriber got %v", got)
		}
		if got := c1.updates(); got[0] != "fake://a" {
			t.Fatalf("update uri = %v", got)
		}
	})
}

func TestSubscribeRefcount(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c1, c2, c3 := h.client(t), h.client(t), h.client(t)
		p := &mcp.SubscribeParams{URI: "fake://a"}
		u := &mcp.UnsubscribeParams{URI: "fake://a"}
		for _, c := range []*downstream{c1, c2} {
			if err := c.Subscribe(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, "upstream subscribed", func() bool { return h.up.subscribed.Load() >= 1 })
		time.Sleep(100 * time.Millisecond)
		if n := h.up.subscribed.Load(); n != 1 {
			t.Fatalf("upstream subscribes = %d, want 1", n)
		}
		if err := c1.Unsubscribe(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if n := h.up.unsubscribed.Load(); n != 0 {
			t.Fatalf("upstream unsubscribes = %d, want 0", n)
		}
		if err := c2.Unsubscribe(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		eventually(t, "last unsubscribe", func() bool { return h.up.unsubscribed.Load() == 1 })
		if err := c3.Subscribe(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		eventually(t, "resubscribe", func() bool { return h.up.subscribed.Load() == 2 })
		_ = c3.Close()
		eventually(t, "unsubscribe after session close", func() bool { return h.up.unsubscribed.Load() == 2 })
	})
}

func TestPromptsAndCompletion(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		var names []string
		for p, err := range c.Prompts(t.Context(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, p.Name)
		}
		if len(names) != 1 || names[0] != "greet" {
			t.Fatalf("prompts = %v", names)
		}
		got, err := c.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "ann"}})
		if err != nil {
			t.Fatal(err)
		}
		if txt := got.Messages[0].Content.(*mcp.TextContent).Text; txt != "hello ann" {
			t.Fatalf("prompt = %q", txt)
		}
		comp, err := c.Complete(t.Context(), &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greet"},
			Argument: mcp.CompleteParamsArgument{Name: "name", Value: "al"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(comp.Completion.Values) != "[alice alex]" {
			t.Fatalf("completion = %v", comp.Completion.Values)
		}
	})
}

func TestProgressRoutedToOriginatingClient(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		a, b := h.client(t), h.client(t)
		call := func(c *downstream, token any, tag string) {
			params := &mcp.CallToolParams{Name: "progress", Arguments: map[string]any{"tag": tag}}
			params.SetProgressToken(token)
			res, err := c.CallTool(t.Context(), params)
			if err != nil {
				t.Error(err)
				return
			}
			if got := resultText(t, res); got != "progressed "+tag {
				t.Errorf("result = %q", got)
			}
		}
		var wg sync.WaitGroup
		wg.Go(func() { call(a, "tokA", "A") })
		wg.Go(func() { call(b, 42, "B") })
		for range 2 {
			select {
			case <-h.up.arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("calls did not reach upstream")
			}
		}
		close(h.up.gate)
		wg.Wait()
		eventually(t, "progress delivered", func() bool { return len(a.progressed()) == 2 && len(b.progressed()) == 2 })
		for _, p := range a.progressed() {
			if p.ProgressToken != "tokA" || p.Message != "A" {
				t.Errorf("client A got %+v", p)
			}
		}
		for _, p := range b.progressed() {
			if fmt.Sprint(p.ProgressToken) != "42" || p.Message != "B" {
				t.Errorf("client B got %+v", p)
			}
		}
		eventually(t, "progress routes released", func() bool {
			h.m.pmu.Lock()
			defer h.m.pmu.Unlock()
			return len(h.m.progress) == 0
		})
	})
}

func TestClientCancelReachesUpstream(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"millis": 60000}})
			errc <- err
		}()
		select {
		case <-h.up.started:
		case <-time.After(5 * time.Second):
			t.Fatal("slow call never reached upstream")
		}
		cancel()
		select {
		case <-h.up.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not observe cancellation")
		}
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("client err = %v", err)
		}
	})
}

func TestCallTimeout(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{CallTimeout: 150 * time.Millisecond})
		c := h.client(t)
		_, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"millis": 60000}})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v", err)
		}
		select {
		case <-h.up.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not observe cancellation")
		}
		reports := h.src.reported()
		if len(reports) != 1 || !errors.Is(reports[0], context.DeadlineExceeded) {
			t.Fatalf("reports = %v", reports)
		}
		if _, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "x"}}); err != nil {
			t.Fatal(err)
		}
		if reports = h.src.reported(); len(reports) != 2 || reports[1] != nil {
			t.Fatalf("reports = %v", reports)
		}
	})
}

func TestUpstreamToolChangesPropagate(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		toolNames(t, c.ClientSession)
		eventually(t, "initial notification", func() bool { return c.toolChanges.Load() >= 1 })

		before := c.toolChanges.Load()
		h.up.srv.AddTool(&mcp.Tool{Name: "late", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text("late"), nil })
		eventually(t, "list_changed after add", func() bool { return c.toolChanges.Load() > before })
		if toolNames(t, c.ClientSession)["late"] == nil {
			t.Fatal("added tool not visible downstream")
		}
		res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "late"})
		if err != nil || resultText(t, res) != "late" {
			t.Fatalf("late call = %v, %v", res, err)
		}

		before = c.toolChanges.Load()
		h.up.srv.RemoveTools("late")
		eventually(t, "list_changed after remove", func() bool { return c.toolChanges.Load() > before })
		if toolNames(t, c.ClientSession)["late"] != nil {
			t.Fatal("removed tool still visible downstream")
		}
	})
}

func TestStoppedClearsAndReconnectRestores(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		c := h.client(t)
		if len(toolNames(t, c.ClientSession)) == 0 {
			t.Fatal("no tools before stop")
		}
		before := c.toolChanges.Load()
		h.src.stop()
		eventually(t, "registry cleared", func() bool {
			h.m.mu.Lock()
			defer h.m.mu.Unlock()
			return len(h.m.tools)+len(h.m.resources)+len(h.m.templates)+len(h.m.prompts) == 0
		})
		eventually(t, "list_changed after stop", func() bool { return c.toolChanges.Load() > before })
		_, err := c.ListTools(t.Context(), nil)
		if err == nil || !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "mcpctl enable fake") {
			t.Fatalf("disabled err = %v", err)
		}

		h.src.setMode("ok")
		if len(toolNames(t, c.ClientSession)) == 0 {
			t.Fatal("catalog not restored after reconnect")
		}
		if n := h.src.connectCount(); n != 2 {
			t.Fatalf("connects = %d, want 2", n)
		}
		res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "again"}})
		if err != nil || resultText(t, res) != "again" {
			t.Fatalf("call after reconnect = %v, %v", res, err)
		}
	})
}

func TestBackoffError(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		h.src.setMode("backoff")
		c := h.client(t)
		_, err := c.ListTools(t.Context(), nil)
		if err == nil || !strings.Contains(err.Error(), "backing off until") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("backoff err = %v", err)
		}
		h.src.setMode("ok")
		if len(toolNames(t, c.ClientSession)) == 0 {
			t.Fatal("no tools after backoff cleared")
		}
	})
}

func TestSessionsTracked(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{})
		if n := h.m.Sessions(); n != 0 {
			t.Fatalf("sessions = %d", n)
		}
		c1, c2 := h.client(t), h.client(t)
		eventually(t, "two sessions", func() bool { return h.m.Sessions() == 2 })
		_ = c1.Close()
		eventually(t, "one session", func() bool { return h.m.Sessions() == 1 })
		_ = c2.Close()
		eventually(t, "no sessions", func() bool { return h.m.Sessions() == 0 })
	})
}

func TestCapabilitiesAndInstructions(t *testing.T) {
	eachProtocol(t, func(t *testing.T) {
		h := newHarness(t, Options{Instructions: "use me"})
		c := h.client(t)
		init := c.InitializeResult()
		caps := init.Capabilities
		if caps.Tools == nil || !caps.Tools.ListChanged || caps.Resources == nil || !caps.Resources.Subscribe ||
			!caps.Resources.ListChanged || caps.Prompts == nil || caps.Completions == nil {
			t.Fatalf("capabilities = %+v", caps)
		}
		if init.Instructions != "use me" {
			t.Fatalf("instructions = %q", init.Instructions)
		}
	})
}
