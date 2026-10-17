package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTogglesRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	got, err := LoadToggles(dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v %v", got, err)
	}
	want := Toggles{"a": true, "b": false}
	if err := SaveToggles(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadToggles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["a"] || got["b"] {
		t.Errorf("got %v", got)
	}
}

func TestSaveTogglesPermsAndNoLeftovers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	for range 3 {
		if err := SaveToggles(dir, Toggles{"a": true}); err != nil {
			t.Fatal(err)
		}
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %v", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, "toggles.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file perm %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestLoadTogglesCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "toggles.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToggles(dir); err == nil {
		t.Fatal("expected error")
	}
}

func TestEffective(t *testing.T) {
	c := &Config{Servers: map[string]Server{
		"on":      {Name: "on", Enabled: true},
		"off":     {Name: "off", Enabled: false},
		"flipoff": {Name: "flipoff", Enabled: true},
		"flipon":  {Name: "flipon", Enabled: false},
	}}
	got := Effective(c, Toggles{"flipoff": false, "flipon": true, "ghost": true})
	want := map[string]bool{"on": true, "off": false, "flipoff": false, "flipon": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for n, e := range want {
		if got[n].Enabled != e {
			t.Errorf("%s: enabled=%v want %v", n, got[n].Enabled, e)
		}
	}
	if !c.Servers["flipoff"].Enabled {
		t.Error("Effective must not mutate config")
	}
}
