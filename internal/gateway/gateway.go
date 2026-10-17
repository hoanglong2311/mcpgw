package gateway

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const routePrefix = "/s/"

type Gateway struct {
	sessionTimeout time.Duration
	cop            *http.CrossOriginProtection

	mu     sync.RWMutex
	routes map[string]http.Handler
}

func New(sessionIdleTimeout time.Duration) *Gateway {
	return &Gateway{
		sessionTimeout: sessionIdleTimeout,
		cop:            http.NewCrossOriginProtection(),
		routes:         make(map[string]http.Handler),
	}
}

func (g *Gateway) Add(name string, srv *mcp.Server) {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		SessionTimeout:        g.sessionTimeout,
		CrossOriginProtection: g.cop,
	})
	g.mu.Lock()
	g.routes[name] = h
	g.mu.Unlock()
}

func (g *Gateway) Remove(name string) {
	g.mu.Lock()
	delete(g.routes, name)
	g.mu.Unlock()
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, routePrefix); ok && name != "" && !strings.Contains(name, "/") {
		g.mu.RLock()
		h := g.routes[name]
		g.mu.RUnlock()
		if h != nil {
			h.ServeHTTP(w, r)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}
