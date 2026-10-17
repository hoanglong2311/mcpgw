package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"mcpctl/internal/source"
)

var ErrAlreadyRunning = errors.New("another mcpctl daemon is already listening on the control socket")

type Backend interface {
	Status(ctx context.Context) *Status
	Reload(ctx context.Context) (*Status, error)
	Enable(ctx context.Context, name string) (*ServerStatus, error)
	Disable(ctx context.Context, name string) (*ServerStatus, error)
	Restart(ctx context.Context, name string) (*ServerStatus, error)
}

func NewHandler(b Backend) http.Handler {
	s := &server{b: b}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, http.StatusOK, b.Status(r.Context()))
	})
	mux.HandleFunc("POST "+PathReload, func(w http.ResponseWriter, r *http.Request) {
		st, err := b.Reload(r.Context())
		s.respond(w, st, err)
	})
	mux.HandleFunc("POST "+PathServers+"{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var (
			st  *ServerStatus
			err error
		)
		switch r.PathValue("action") {
		case ActionEnable:
			st, err = b.Enable(r.Context(), name)
		case ActionDisable:
			st, err = b.Disable(r.Context(), name)
		case ActionRestart:
			st, err = b.Restart(r.Context(), name)
		default:
			respondError(w, http.StatusNotFound, "unknown action")
			return
		}
		s.respond(w, st, err)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		respondError(w, http.StatusNotFound, "not found")
	})
	return mux
}

type server struct{ b Backend }

func (s *server) respond(w http.ResponseWriter, v any, err error) {
	switch {
	case err == nil:
		respondJSON(w, http.StatusOK, v)
	case errors.Is(err, source.ErrUnknown):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, source.ErrDisabled):
		respondError(w, http.StatusConflict, err.Error())
	default:
		respondError(w, http.StatusInternalServerError, err.Error())
	}
}

func respondError(w http.ResponseWriter, code int, msg string) {
	respondJSON(w, code, ErrorBody{Error: msg})
}

func respondJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func Listen(socketPath string) (net.Listener, error) {
	if c, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
		c.Close()
		return nil, ErrAlreadyRunning
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
