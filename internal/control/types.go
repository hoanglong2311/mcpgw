package control

import (
	"path/filepath"
	"time"
)

const (
	StateIdle       = "idle"
	StateConnecting = "connecting"
	StateReady      = "ready"
	StateBackoff    = "backoff"
	StateDisabled   = "disabled"
)

const (
	PathStatus  = "/v1/status"
	PathReload  = "/v1/reload"
	PathServers = "/v1/servers/"

	ActionEnable  = "enable"
	ActionDisable = "disable"
	ActionRestart = "restart"
)

type Status struct {
	Version   string         `json:"version"`
	PID       int            `json:"pid"`
	Listen    string         `json:"listen"`
	StartedAt time.Time      `json:"started_at"`
	Servers   []ServerStatus `json:"servers"`
}

type ServerStatus struct {
	Name      string     `json:"name"`
	URL       string     `json:"url"`
	Enabled   bool       `json:"enabled"`
	State     string     `json:"state"`
	PID       int        `json:"pid"`
	PGID      int        `json:"pgid"`
	Sessions  int        `json:"sessions"`
	Restarts  int        `json:"restarts"`
	Since     time.Time  `json:"since"`
	LastError string     `json:"last_error"`
	RetryAt   *time.Time `json:"retry_at"`
}

type ErrorBody struct {
	Error string `json:"error"`
}

func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, "control.sock")
}

func ServerPath(name, action string) string {
	return PathServers + name + "/" + action
}
