package mirror

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	progressTokenKey   = "progressToken"
	reservedMetaPrefix = "io.modelcontextprotocol/"
)

const progressGrace = 500 * time.Millisecond

func (m *Mirror) callTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cs, err := m.src.Session(ctx)
	if err != nil {
		return nil, m.rpcError(err)
	}
	p := req.Params
	params := &mcp.CallToolParams{
		Meta:           forwardMeta(p.Meta),
		Name:           p.Name,
		InputResponses: p.InputResponses,
		RequestState:   p.RequestState,
	}
	if len(p.Arguments) > 0 {
		params.Arguments = p.Arguments
	}
	if orig := p.GetProgressToken(); orig != nil {
		key := fmt.Sprintf("mcpctl:%s:%d", m.name, m.progressSeq.Add(1))
		if params.Meta == nil {
			params.Meta = mcp.Meta{}
		}
		params.Meta[progressTokenKey] = key
		m.pmu.Lock()
		m.progress[key] = progressRoute{session: req.Session, token: orig}
		m.pmu.Unlock()
		defer time.AfterFunc(progressGrace, func() {
			m.pmu.Lock()
			delete(m.progress, key)
			m.pmu.Unlock()
		})
	}
	callCtx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	res, err := cs.CallTool(callCtx, params)
	return res, m.finishCall(ctx, callCtx, err)
}

func (m *Mirror) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	cs, err := m.src.Session(ctx)
	if err != nil {
		return nil, m.rpcError(err)
	}
	p := *req.Params
	p.Meta = forwardMeta(p.Meta)
	callCtx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	res, err := cs.ReadResource(callCtx, &p)
	return res, m.finishCall(ctx, callCtx, err)
}

func (m *Mirror) getPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	cs, err := m.src.Session(ctx)
	if err != nil {
		return nil, m.rpcError(err)
	}
	p := *req.Params
	p.Meta = forwardMeta(p.Meta)
	callCtx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	res, err := cs.GetPrompt(callCtx, &p)
	return res, m.finishCall(ctx, callCtx, err)
}

func (m *Mirror) complete(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	cs, err := m.src.Session(ctx)
	if err != nil {
		return nil, m.rpcError(err)
	}
	p := *req.Params
	p.Meta = forwardMeta(p.Meta)
	callCtx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	res, err := cs.Complete(callCtx, &p)
	return res, m.finishCall(ctx, callCtx, err)
}

func (m *Mirror) finishCall(parent, call context.Context, err error) error {
	if err == nil {
		m.src.ReportCallResult(nil)
		return nil
	}
	if call.Err() == context.DeadlineExceeded && parent.Err() == nil {
		err = fmt.Errorf("%w after %s", context.DeadlineExceeded, m.opts.CallTimeout)
	}
	m.src.ReportCallResult(err)
	return m.rpcError(err)
}

func forwardMeta(in mcp.Meta) mcp.Meta {
	if len(in) == 0 {
		return nil
	}
	out := maps.Clone(in)
	for k := range out {
		if k == progressTokenKey || strings.HasPrefix(k, reservedMetaPrefix) {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *Mirror) subscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	uri, ss := req.Params.URI, req.Session
	m.smu.Lock()
	holders := m.subs[uri]
	first := len(holders) == 0
	m.smu.Unlock()
	if first {
		cs, err := m.src.Session(ctx)
		if err != nil {
			return m.rpcError(err)
		}
		if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			return m.rpcError(err)
		}
	}
	m.smu.Lock()
	if m.subs[uri] == nil {
		m.subs[uri] = map[*mcp.ServerSession]struct{}{}
	}
	m.subs[uri][ss] = struct{}{}
	_, watched := m.watching[ss]
	m.watching[ss] = struct{}{}
	m.smu.Unlock()
	if !watched {
		go func() {
			_ = ss.Wait()
			m.dropSession(ss)
		}()
	}
	return nil
}

func (m *Mirror) unsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	if m.release(req.Params.URI, req.Session) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return m.upstreamUnsubscribe(ctx, req.Params.URI)
	}
	return nil
}

func (m *Mirror) release(uri string, ss *mcp.ServerSession) (last bool) {
	m.smu.Lock()
	defer m.smu.Unlock()
	holders, ok := m.subs[uri]
	if !ok {
		return false
	}
	if _, ok := holders[ss]; !ok {
		return false
	}
	delete(holders, ss)
	if len(holders) == 0 {
		delete(m.subs, uri)
		return true
	}
	return false
}

func (m *Mirror) dropSession(ss *mcp.ServerSession) {
	m.smu.Lock()
	delete(m.watching, ss)
	var uris []string
	for uri, holders := range m.subs {
		if _, ok := holders[ss]; ok {
			uris = append(uris, uri)
		}
	}
	m.smu.Unlock()
	for _, uri := range uris {
		if m.release(uri, ss) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := m.upstreamUnsubscribe(ctx, uri); err != nil {
				m.log.Debug("release subscription", "uri", uri, "err", err)
			}
			cancel()
		}
	}
}

func (m *Mirror) upstreamUnsubscribe(ctx context.Context, uri string) error {
	cs, err := m.src.Session(ctx)
	if err != nil {
		return m.rpcError(err)
	}
	if err := cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri}); err != nil {
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			return je
		}
		return m.rpcError(err)
	}
	return nil
}

func (m *Mirror) resubscribe() {
	m.smu.Lock()
	uris := make([]string, 0, len(m.subs))
	for uri := range m.subs {
		uris = append(uris, uri)
	}
	m.smu.Unlock()
	if len(uris) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := m.src.Session(ctx)
	if err != nil {
		return
	}
	for _, uri := range uris {
		if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			m.log.Warn("resubscribe", "uri", uri, "err", err)
		}
	}
}
