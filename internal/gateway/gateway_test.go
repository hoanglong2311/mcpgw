package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRouting(t *testing.T) {
	g := New(time.Minute)
	g.Add("a", mcp.NewServer(&mcp.Implementation{Name: "a", Version: "1"}, nil))

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", 200},
		{"GET", "/", 404},
		{"GET", "/s/", 404},
		{"GET", "/s/missing", 404},
		{"GET", "/s/a/extra", 404},
	} {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}

	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/s/a", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("registered route returned 404")
	}
	g.Remove("a")
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/s/a", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("removed route = %d", rec.Code)
	}
}
