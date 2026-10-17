package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// BuildFakeServer compiles the fake upstream MCP server once per test binary
// and returns the executable path. The output lives in a stable per-user temp
// directory so repeated runs reuse go's build cache and leave nothing behind.
func BuildFakeServer(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		dir := filepath.Join(os.TempDir(), "mcpctl-fakeserver-"+strconv.Itoa(os.Getuid()))
		if buildErr = os.MkdirAll(dir, 0o700); buildErr != nil {
			return
		}
		buildPath = filepath.Join(dir, "fakeserver")
		cmd := exec.Command("go", "build", "-o", buildPath, "./fakeserver")
		cmd.Dir = filepath.Dir(file)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildError{err: err, out: string(out)}
		}
	})
	if buildErr != nil {
		t.Fatalf("build fake server: %v", buildErr)
	}
	return buildPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }
