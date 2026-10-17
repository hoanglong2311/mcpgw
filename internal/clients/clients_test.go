package clients

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var servers = []Server{
	{"atlassian", "http://127.0.0.1:8765/s/atlassian"},
	{"gitlab", "http://127.0.0.1:8765/s/gitlab"},
}

func TestGolden(t *testing.T) {
	for _, c := range All {
		t.Run(c.Name, func(t *testing.T) {
			got := c.Render(servers, "/opt/mcpctl/bin/mcpctl")
			golden := filepath.Join("testdata", c.Name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("mismatch:\n%s", got)
			}
		})
	}
}

func TestFind(t *testing.T) {
	if _, ok := Find("codex"); !ok {
		t.Fatal("codex not found")
	}
	if _, ok := Find("vim"); ok {
		t.Fatal("unexpected client")
	}
	if len(Names()) != 6 {
		t.Fatalf("names = %v", Names())
	}
}
