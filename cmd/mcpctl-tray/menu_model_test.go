package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoanglong2311/mcpgw/internal/control"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func labels(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Kind == KindSeparator {
			out = append(out, "---")
			continue
		}
		out = append(out, it.Label)
	}
	return out
}

func find(items []Item, label string) (Item, bool) {
	for _, it := range items {
		if it.Label == label {
			return it, true
		}
	}
	return Item{}, false
}

func TestDaemonDown(t *testing.T) {
	for _, err := range []error{nil, fmt.Errorf("dial: %w", control.ErrDaemonNotRunning)} {
		items := BuildMenu(Snapshot{Err: err, Now: now})
		if items[0].Label != "Gateway: stopped" || !items[0].Disabled {
			t.Fatalf("first item = %+v", items[0])
		}
		start, ok := find(items, "Start gateway")
		if !ok || start.Action != ActionStartGateway {
			t.Fatalf("missing start gateway: %v", labels(items))
		}
		if _, ok := find(items, "Reload config"); ok {
			t.Fatal("reload offered while daemon down")
		}
		if _, ok := find(items, "Quit tray"); !ok {
			t.Fatal("missing quit")
		}
	}
}

func TestUnreachableError(t *testing.T) {
	items := BuildMenu(Snapshot{Err: errors.New("control api: boom\nsecond line"), Now: now})
	if items[0].Label != "Gateway: unreachable" {
		t.Fatalf("got %v", labels(items))
	}
	if items[1].Label != "  control api: boom" || !items[1].Disabled {
		t.Fatalf("error line = %+v", items[1])
	}
	if _, ok := find(items, "Start gateway"); !ok {
		t.Fatal("start gateway missing")
	}
}

func TestRunningServersSortedAndLabelled(t *testing.T) {
	retry := now.Add(30 * time.Second)
	st := &control.Status{
		Listen: "127.0.0.1:8765",
		Servers: []control.ServerStatus{
			{Name: "zeta", URL: "http://127.0.0.1:8765/s/zeta", Enabled: false, State: control.StateIdle},
			{Name: "atlassian", URL: "http://127.0.0.1:8765/s/atlassian", Enabled: true, State: control.StateReady, PID: 4242, Sessions: 3},
			{Name: "mid", Enabled: true, State: control.StateBackoff, LastError: "spawn failed: exit 1\ndetails", RetryAt: &retry},
		},
	}
	items := BuildMenu(Snapshot{Status: st, Now: now})
	want := []string{
		"Gateway: running on 127.0.0.1:8765",
		"atlassian — ready pid 4242 · 3 sessions",
		"mid — backoff",
		"    spawn failed: exit 1",
		"    retry in 30s",
		"zeta — disabled",
	}
	got := labels(items)
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], w, got)
		}
	}
	if !items[1].Checked || items[1].Action != ActionToggle || items[1].Server != "atlassian" {
		t.Fatalf("atlassian item = %+v", items[1])
	}
	if items[5].Checked {
		t.Fatal("disabled server checked")
	}
	if !items[3].Disabled || !items[4].Disabled {
		t.Fatal("error lines must be disabled")
	}
}

func TestSubmenus(t *testing.T) {
	st := &control.Status{Listen: "x", Servers: []control.ServerStatus{
		{Name: "b", URL: "u-b", Enabled: false},
		{Name: "a", URL: "u-a", Enabled: true},
	}}
	items := BuildMenu(Snapshot{Status: st, Now: now})
	restart, ok := find(items, "Restart")
	if !ok || len(restart.Children) != 1 || restart.Children[0].Server != "a" || restart.Children[0].Action != ActionRestart {
		t.Fatalf("restart = %+v", restart)
	}
	copyURL, _ := find(items, "Copy URL")
	if len(copyURL.Children) != 2 || copyURL.Children[0].Arg != "u-a" || copyURL.Children[1].Arg != "u-b" {
		t.Fatalf("copy url = %+v", copyURL)
	}
}

func TestRestartDisabledWhenNoneEnabled(t *testing.T) {
	st := &control.Status{Listen: "x", Servers: []control.ServerStatus{{Name: "a"}}}
	items := BuildMenu(Snapshot{Status: st, Now: now})
	restart, _ := find(items, "Restart")
	if !restart.Disabled {
		t.Fatalf("restart should be disabled: %+v", restart)
	}
}

func TestNoServers(t *testing.T) {
	items := BuildMenu(Snapshot{Status: &control.Status{Listen: "x"}, Now: now})
	if _, ok := find(items, "Restart"); ok {
		t.Fatal("restart submenu without servers")
	}
	if _, ok := find(items, "Reload config"); !ok {
		t.Fatal("reload missing")
	}
}

func TestPastRetryHidden(t *testing.T) {
	past := now.Add(-time.Second)
	st := &control.Status{Listen: "x", Servers: []control.ServerStatus{{Name: "a", Enabled: true, RetryAt: &past}}}
	for _, l := range labels(BuildMenu(Snapshot{Status: st, Now: now})) {
		if strings.Contains(l, "retry") {
			t.Fatalf("stale retry line %q", l)
		}
	}
}

func TestLoginItem(t *testing.T) {
	items := BuildMenu(Snapshot{LoginKnown: true, LoginEnabled: true, Now: now})
	it, ok := find(items, "Start at login")
	if !ok || it.Kind != KindCheckbox || !it.Checked || it.Action != ActionToggleLogin {
		t.Fatalf("login item = %+v", it)
	}
	if _, ok := find(BuildMenu(Snapshot{Now: now}), "Start at login"); ok {
		t.Fatal("login item without LoginKnown")
	}
}

func TestActionErrorLineAndClip(t *testing.T) {
	long := strings.Repeat("x", 200)
	items := BuildMenu(Snapshot{Status: &control.Status{Listen: "x"}, ActionErr: long, Now: now})
	var line string
	for _, it := range items {
		if strings.HasPrefix(it.Label, "Last action failed") {
			line = it.Label
			if !it.Disabled {
				t.Fatal("must be disabled")
			}
		}
	}
	if line == "" || len([]rune(line)) > len("Last action failed: ")+maxErrLen {
		t.Fatalf("line = %q", line)
	}
}

func TestStaticItemsAlwaysPresent(t *testing.T) {
	for _, s := range []Snapshot{{Now: now}, {Status: &control.Status{Listen: "x"}, Now: now}, {Err: errors.New("e"), Now: now}} {
		items := BuildMenu(s)
		for _, l := range []string{"Open config", "Open logs folder", "Quit tray"} {
			if _, ok := find(items, l); !ok {
				t.Fatalf("%q missing in %v", l, labels(items))
			}
		}
	}
}
