package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hoanglong2311/mcpgw/internal/control"
)

type Action int

const (
	ActionNone Action = iota
	ActionToggle
	ActionRestart
	ActionCopyURL
	ActionReload
	ActionOpenConfig
	ActionOpenLogs
	ActionStartGateway
	ActionToggleLogin
	ActionQuit
)

type Kind int

const (
	KindNormal Kind = iota
	KindCheckbox
	KindSeparator
)

type Item struct {
	Kind     Kind
	Label    string
	Checked  bool
	Disabled bool
	Action   Action
	Server   string
	Arg      string
	Children []Item
}

type Snapshot struct {
	Status       *control.Status
	Err          error
	ActionErr    string
	LoginEnabled bool
	LoginKnown   bool
	Now          time.Time
}

const maxErrLen = 80

func BuildMenu(s Snapshot) []Item {
	var items []Item
	switch {
	case s.Err != nil && !errors.Is(s.Err, control.ErrDaemonNotRunning):
		items = append(items,
			Item{Label: "Gateway: unreachable", Disabled: true},
			Item{Label: "  " + clip(s.Err.Error()), Disabled: true},
			Item{Label: "Start gateway", Action: ActionStartGateway},
		)
	case s.Status == nil:
		items = append(items,
			Item{Label: "Gateway: stopped", Disabled: true},
			Item{Label: "Start gateway", Action: ActionStartGateway},
		)
	default:
		items = append(items, Item{Label: "Gateway: running on " + s.Status.Listen, Disabled: true})
		items = append(items, serverItems(s.Status.Servers, s.Now)...)
		if len(s.Status.Servers) > 0 {
			items = append(items, Item{Kind: KindSeparator})
			items = append(items,
				serverSubmenu("Restart", ActionRestart, s.Status.Servers, false),
				serverSubmenu("Copy URL", ActionCopyURL, s.Status.Servers, true),
			)
		}
		items = append(items, Item{Label: "Reload config", Action: ActionReload})
	}
	if s.ActionErr != "" {
		items = append(items, Item{Label: "Last action failed: " + clip(s.ActionErr), Disabled: true})
	}
	items = append(items,
		Item{Kind: KindSeparator},
		Item{Label: "Open config", Action: ActionOpenConfig},
		Item{Label: "Open logs folder", Action: ActionOpenLogs},
	)
	if s.LoginKnown {
		items = append(items, Item{Kind: KindCheckbox, Label: "Start at login", Checked: s.LoginEnabled, Action: ActionToggleLogin})
	}
	items = append(items, Item{Kind: KindSeparator}, Item{Label: "Quit tray", Action: ActionQuit})
	return items
}

func sortedServers(servers []control.ServerStatus) []control.ServerStatus {
	out := append([]control.ServerStatus(nil), servers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func serverItems(servers []control.ServerStatus, now time.Time) []Item {
	var items []Item
	for _, sv := range sortedServers(servers) {
		items = append(items, Item{
			Kind:    KindCheckbox,
			Label:   serverLabel(sv),
			Checked: sv.Enabled,
			Action:  ActionToggle,
			Server:  sv.Name,
		})
		if sv.LastError != "" {
			items = append(items, Item{Label: "    " + clip(sv.LastError), Disabled: true})
		}
		if sv.RetryAt != nil {
			if d := sv.RetryAt.Sub(now).Round(time.Second); d > 0 {
				items = append(items, Item{Label: fmt.Sprintf("    retry in %s", d), Disabled: true})
			}
		}
	}
	return items
}

func serverLabel(sv control.ServerStatus) string {
	state := sv.State
	if !sv.Enabled {
		state = control.StateDisabled
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s", sv.Name, state)
	if sv.PID > 0 {
		fmt.Fprintf(&b, " pid %d", sv.PID)
	}
	if sv.Sessions > 0 {
		fmt.Fprintf(&b, " · %d sessions", sv.Sessions)
	}
	return b.String()
}

func serverSubmenu(label string, action Action, servers []control.ServerStatus, all bool) Item {
	sub := Item{Label: label}
	for _, sv := range sortedServers(servers) {
		if !all && !sv.Enabled {
			continue
		}
		it := Item{Label: sv.Name, Action: action, Server: sv.Name}
		if action == ActionCopyURL {
			it.Arg = sv.URL
		}
		sub.Children = append(sub.Children, it)
	}
	if len(sub.Children) == 0 {
		sub.Disabled = true
	}
	return sub
}

func clip(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	r := []rune(s)
	if len(r) > maxErrLen {
		return string(r[:maxErrLen-1]) + "…"
	}
	return s
}
