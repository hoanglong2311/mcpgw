package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

var ErrDaemonNotRunning = errors.New("mcpctl daemon is not running")

type Client struct {
	http *http.Client
}

func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control api: %s (HTTP %d)", e.Message, e.StatusCode)
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.do(ctx, http.MethodGet, PathStatus, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) Enable(ctx context.Context, name string) (*ServerStatus, error) {
	return c.serverAction(ctx, name, ActionEnable)
}

func (c *Client) Disable(ctx context.Context, name string) (*ServerStatus, error) {
	return c.serverAction(ctx, name, ActionDisable)
}

func (c *Client) Restart(ctx context.Context, name string) (*ServerStatus, error) {
	return c.serverAction(ctx, name, ActionRestart)
}

func (c *Client) Reload(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.do(ctx, http.MethodPost, PathReload, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) serverAction(ctx context.Context, name, action string) (*ServerStatus, error) {
	var s ServerStatus
	if err := c.do(ctx, http.MethodPost, ServerPath(url.PathEscape(name), action), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://mcpctl"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if isNotRunning(err) {
			return ErrDaemonNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var eb ErrorBody
		msg := http.StatusText(resp.StatusCode)
		if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
			msg = eb.Error
		}
		return &APIError{StatusCode: resp.StatusCode, Message: msg}
	}
	return json.Unmarshal(body, out)
}

func isNotRunning(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
