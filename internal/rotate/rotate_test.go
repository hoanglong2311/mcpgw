package rotate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeepsOneBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	r, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, s := range []string{"aaaaaa", "bbbbbb", "cccccc"} {
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if string(cur) != "cccccc" || string(old) != "bbbbbb" {
		t.Fatalf("cur=%q old=%q", cur, old)
	}
}
