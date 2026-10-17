package source

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type EventKind int

const (
	ToolsChanged EventKind = iota
	PromptsChanged
	ResourcesChanged
	ResourceUpdated
	Progress
	Connected
	Stopped
)

var kindNames = [...]string{"ToolsChanged", "PromptsChanged", "ResourcesChanged", "ResourceUpdated", "Progress", "Connected", "Stopped"}

func (k EventKind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("EventKind(%d)", int(k))
	}
	return kindNames[k]
}

type Event struct {
	Kind         EventKind
	URI          string
	Progress     *mcp.ProgressNotificationParams
	Instructions string
}

type Source interface {
	Session(ctx context.Context) (*mcp.ClientSession, error)
	Subscribe(fn func(Event)) (cancel func())
	ReportCallResult(err error)
}

var (
	ErrDisabled = errors.New("server is disabled")
	ErrUnknown  = errors.New("unknown server")
)

type BackoffError struct {
	Last    error
	RetryAt time.Time
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("upstream in backoff until %s: %v", e.RetryAt.Format(time.RFC3339), e.Last)
}

func (e *BackoffError) Unwrap() error { return e.Last }
