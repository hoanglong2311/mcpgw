package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const syncTimeout = 60 * time.Second

type syncCall struct {
	done chan struct{}
	err  error
}

type catalog struct {
	tools     []*mcp.Tool
	resources []*mcp.Resource
	templates []*mcp.ResourceTemplate
	prompts   []*mcp.Prompt
}

func (m *Mirror) ensureSynced(ctx context.Context) error {
	for {
		m.mu.Lock()
		if m.fresh {
			m.mu.Unlock()
			return nil
		}
		call := m.inflight
		if call == nil {
			call = &syncCall{done: make(chan struct{})}
			m.inflight = call
			go m.runSync(call)
		}
		m.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if call.err != nil {
			return call.err
		}
	}
}

func (m *Mirror) runSync(call *syncCall) {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	cs, err := m.src.Session(ctx)
	var cat *catalog
	var gen uint64
	if err == nil {
		m.mu.Lock()
		gen = m.gen
		m.mu.Unlock()
		cat, err = fetchCatalog(ctx, cs)
	}
	m.mu.Lock()
	if err == nil && gen == m.gen {
		m.apply(cat)
		m.fresh = true
	}
	m.inflight = nil
	m.mu.Unlock()
	call.err = err
	close(call.done)
}

func fetchCatalog(ctx context.Context, cs *mcp.ClientSession) (*catalog, error) {
	var (
		cat catalog
		err error
	)
	if cat.tools, err = collect(cs.Tools(ctx, nil)); err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	if cat.resources, err = collect(cs.Resources(ctx, nil)); err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	if cat.templates, err = collect(cs.ResourceTemplates(ctx, nil)); err != nil {
		return nil, fmt.Errorf("list resource templates: %w", err)
	}
	if cat.prompts, err = collect(cs.Prompts(ctx, nil)); err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	for i, t := range cat.tools {
		cat.tools[i] = normalizeTool(t)
	}
	return &cat, nil
}

func collect[T any](seq iter.Seq2[*T, error]) ([]*T, error) {
	var out []*T
	for v, err := range seq {
		if err != nil {
			var je *jsonrpc.Error
			if errors.As(err, &je) && je.Code == jsonrpc.CodeMethodNotFound {
				return nil, nil
			}
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func normalizeTool(t *mcp.Tool) *mcp.Tool {
	c := *t
	var schema map[string]any
	if b, err := json.Marshal(c.InputSchema); err != nil || json.Unmarshal(b, &schema) != nil || schema["type"] != "object" {
		c.InputSchema = map[string]any{"type": "object"}
	}
	return &c
}

func (m *Mirror) apply(cat *catalog) {
	m.tools = reconcile(m, "tool", m.tools, cat.tools, func(t *mcp.Tool) string { return t.Name },
		func(t *mcp.Tool) { m.server.AddTool(t, m.callTool) }, m.server.RemoveTools)
	m.resources = reconcile(m, "resource", m.resources, cat.resources, func(r *mcp.Resource) string { return r.URI },
		func(r *mcp.Resource) { m.server.AddResource(r, m.readResource) }, m.server.RemoveResources)
	m.templates = reconcile(m, "resource template", m.templates, cat.templates, func(t *mcp.ResourceTemplate) string { return t.URITemplate },
		func(t *mcp.ResourceTemplate) { m.server.AddResourceTemplate(t, m.readResource) }, m.server.RemoveResourceTemplates)
	m.prompts = reconcile(m, "prompt", m.prompts, cat.prompts, func(p *mcp.Prompt) string { return p.Name },
		func(p *mcp.Prompt) { m.server.AddPrompt(p, m.getPrompt) }, m.server.RemovePrompts)
}

func (m *Mirror) clearRegistry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apply(&catalog{})
}

func reconcile[T any](m *Mirror, kind string, cur map[string]string, items []*T, key func(*T) string, add func(*T), remove func(...string)) map[string]string {
	next := make(map[string]string, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		k := key(it)
		b, err := json.Marshal(it)
		if err != nil {
			m.log.Warn("skip "+kind, "key", k, "err", err)
			continue
		}
		next[k] = string(b)
		if cur[k] != next[k] {
			m.safeAdd(kind, k, func() { add(it) })
		}
	}
	var gone []string
	for k := range cur {
		if _, ok := next[k]; !ok {
			gone = append(gone, k)
		}
	}
	if len(gone) > 0 {
		remove(gone...)
	}
	return next
}

func (m *Mirror) safeAdd(kind, key string, add func()) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("skip "+kind, "key", key, "panic", fmt.Sprint(r))
		}
	}()
	add()
}
