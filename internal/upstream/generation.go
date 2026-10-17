package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpctl/internal/config"
	"mcpctl/internal/rotate"
	"mcpctl/internal/source"
)

var errGenStopped = errors.New("upstream generation stopped")

const (
	stateIdle       = "idle"
	stateConnecting = "connecting"
	stateReady      = "ready"
	stateBackoff    = "backoff"
	stateDisabled   = "disabled"
)

type connectCall struct{ done chan struct{} }

type generation struct {
	h     *handle
	cfg   config.Server
	after <-chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	stopped  bool
	sess     *mcp.ClientSession
	proc     *proc
	call     *connectCall
	failures int
	retryAt  time.Time
	lastErr  error
	since    time.Time
	readyAt  time.Time
	timeouts int
}

func newGeneration(h *handle, cfg config.Server, after <-chan struct{}) *generation {
	ctx, cancel := context.WithCancel(context.Background())
	return &generation{h: h, cfg: cfg, after: after, ctx: ctx, cancel: cancel, done: make(chan struct{}), since: time.Now()}
}

func (g *generation) opts() *Options { return &g.h.m.opts }

func (g *generation) session(ctx context.Context) (*mcp.ClientSession, error) {
	for {
		g.mu.Lock()
		switch {
		case g.stopped:
			g.mu.Unlock()
			return nil, errGenStopped
		case g.sess != nil:
			s := g.sess
			if g.failures > 0 && time.Since(g.readyAt) >= g.opts().healthyAfter {
				g.failures = 0
			}
			g.mu.Unlock()
			return s, nil
		case g.call != nil:
			c := g.call
			g.mu.Unlock()
			select {
			case <-c.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case g.failures > 0 && time.Now().Before(g.retryAt):
			err := &source.BackoffError{Last: g.lastErr, RetryAt: g.retryAt}
			g.mu.Unlock()
			return nil, err
		default:
			c := &connectCall{done: make(chan struct{})}
			g.call = c
			g.mu.Unlock()
			go g.connect(c)
		}
	}
}

func (g *generation) connect(c *connectCall) {
	sess, err := g.dial()

	g.mu.Lock()
	g.call = nil
	switch {
	case err != nil:
		if !g.stopped {
			g.failLocked(err)
			p := g.proc
			g.proc = nil
			g.mu.Unlock()
			g.killProc(p)
			close(c.done)
			return
		}
	default:
		g.sess = sess
		if !g.stopped {
			g.readyAt = time.Now()
			g.since = g.readyAt
			g.lastErr = nil
			go g.watch(sess)
			var instr string
			if r := sess.InitializeResult(); r != nil {
				instr = r.Instructions
			}
			g.mu.Unlock()
			close(c.done)
			g.emit(source.Event{Kind: source.Connected, Instructions: instr})
			return
		}
	}
	g.mu.Unlock()
	close(c.done)
}

func (g *generation) failLocked(err error) {
	g.failures++
	g.lastErr = err
	g.since = time.Now()
	g.retryAt = g.since.Add(g.opts().backoff(g.failures))
	g.opts().Logger.Warn("upstream failed", "server", g.h.name, "failures", g.failures, "retry_at", g.retryAt, "err", err)
}

func (o *Options) backoff(failures int) time.Duration {
	d := o.backoffBase
	for i := 1; i < failures && d < o.backoffMax; i++ {
		d *= 2
	}
	return min(d, o.backoffMax)
}

type startHook struct {
	mcp.Transport
	after func()
}

func (t startHook) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err == nil {
		t.after()
	}
	return c, err
}

func (g *generation) dial() (*mcp.ClientSession, error) {
	o := g.opts()
	if g.after != nil {
		select {
		case <-g.after:
		case <-g.ctx.Done():
			return nil, g.ctx.Err()
		}
	}
	env, err := buildEnv(g.cfg, o.Resolve)
	if err != nil {
		return nil, err
	}
	cmd, err := buildCmd(g.cfg, env)
	if err != nil {
		return nil, err
	}
	logDir := filepath.Join(o.StateDir, "logs")
	runDir := filepath.Join(o.StateDir, "run")
	for _, d := range []string{logDir, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	lf, err := rotate.Open(filepath.Join(logDir, g.h.name+".log"), rotate.DefaultMaxSize)
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		lf.Close()
		return nil, err
	}
	go rotate.Pump(pr, lf)
	closePW := sync.OnceFunc(func() { pw.Close() })
	defer closePW()
	cmd.Stderr = pw

	pgidPath := filepath.Join(runDir, g.h.name+".pgid")
	tr := startHook{
		Transport: &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second},
		after: func() {
			closePW()
			if err := writePgidFile(pgidPath, cmd.Process.Pid, g.cfg.Command); err != nil {
				o.Logger.Warn("write pgid file", "server", g.h.name, "err", err)
			}
			g.mu.Lock()
			g.proc = &proc{pid: cmd.Process.Pid, pgidPath: pgidPath}
			g.mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(g.ctx, o.ConnectTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpctl", Version: "0"}, g.clientOptions())
	return client.Connect(ctx, tr, nil)
}

func (g *generation) clientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			g.emit(source.Event{Kind: source.ToolsChanged})
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			g.emit(source.Event{Kind: source.PromptsChanged})
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			g.emit(source.Event{Kind: source.ResourcesChanged})
		},
		ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) {
			g.emit(source.Event{Kind: source.ResourceUpdated, URI: r.Params.URI})
		},
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			g.emit(source.Event{Kind: source.Progress, Progress: r.Params})
		},
	}
}

func (g *generation) emit(ev source.Event) {
	g.mu.Lock()
	stopped := g.stopped
	g.mu.Unlock()
	if !stopped {
		g.h.emit(ev)
	}
}

func (g *generation) watch(sess *mcp.ClientSession) {
	sess.Wait()
	g.mu.Lock()
	if g.stopped || g.sess != sess {
		g.mu.Unlock()
		return
	}
	p := g.proc
	g.sess, g.proc = nil, nil
	c := &connectCall{done: make(chan struct{})}
	g.call = c
	g.mu.Unlock()

	sess.Close()
	g.killProc(p)

	g.mu.Lock()
	g.call = nil
	if time.Since(g.readyAt) >= g.opts().healthyAfter {
		g.failures = 0
		g.since = time.Now()
	} else {
		g.failLocked(errors.New("upstream process exited"))
	}
	g.mu.Unlock()
	close(c.done)
}

func (g *generation) killProc(p *proc) {
	if p == nil {
		return
	}
	killGroup(p.pid, g.opts().stopGrace)
	os.Remove(p.pgidPath)
}

func (g *generation) stop() {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		<-g.done
		return
	}
	g.stopped = true
	g.cancel()
	c := g.call
	g.mu.Unlock()
	if c != nil {
		<-c.done
	}
	g.mu.Lock()
	sess, p := g.sess, g.proc
	g.sess, g.proc = nil, nil
	g.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	g.killProc(p)
	close(g.done)
}

func (g *generation) reportCall(err error) (restart bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		g.timeouts = 0
		return false
	}
	g.timeouts++
	if g.timeouts < 2 {
		return false
	}
	g.timeouts = 0
	return true
}

func (g *generation) status() (st Status) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st.Since = g.since
	if g.lastErr != nil {
		st.LastError = g.lastErr.Error()
	}
	switch {
	case g.stopped:
		st.State = stateDisabled
	case g.sess != nil:
		st.State = stateReady
	case g.call != nil:
		st.State = stateConnecting
	case g.failures > 0 && time.Now().Before(g.retryAt):
		st.State = stateBackoff
		t := g.retryAt
		st.RetryAt = &t
	default:
		st.State = stateIdle
	}
	if g.proc != nil && g.sess != nil {
		st.PID, st.PGID = g.proc.pid, g.proc.pid
	}
	return st
}

func (g *generation) String() string { return fmt.Sprintf("generation(%s)", g.h.name) }
