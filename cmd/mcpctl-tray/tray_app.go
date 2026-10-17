package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"mcpctl/internal/config"
	"mcpctl/internal/control"
)

const (
	pollInterval = 3 * time.Second
	callTimeout  = 10 * time.Second
	launchdLabel = "local.mcpctl"
)

type trayApp struct {
	client   *control.Client
	stateDir string
	tray     *mygo.Tray
	refresh  chan struct{}
	stop     chan struct{}

	mu        sync.Mutex
	actionErr string
	last      []Item
	lastTip   string
}

func newTrayApp() *trayApp {
	dir := config.DefaultStateDir()
	return &trayApp{
		client:   control.NewClient(control.SocketPath(dir)),
		stateDir: dir,
		refresh:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

func (a *trayApp) start() {
	tray, err := mygo.NewTray(mygo.TrayOptions{
		Icon:           trayIcon,
		IconIsTemplate: true,
		ToolTip:        "mcpctl",
	})
	if err != nil {
		log.Fatal("tray: ", err)
	}
	a.tray = tray
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		close(a.stop)
		tray.Destroy()
	})
	go a.poll()
}

func (a *trayApp) poll() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		a.update()
		select {
		case <-a.stop:
			return
		case <-t.C:
		case <-a.refresh:
		}
	}
}

func (a *trayApp) kick() {
	a.mu.Lock()
	a.last = nil
	a.mu.Unlock()
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

func (a *trayApp) update() {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	st, err := a.client.Status(ctx)
	if err != nil {
		st = nil
	}
	a.mu.Lock()
	snap := Snapshot{
		Status:     st,
		Err:        err,
		ActionErr:  a.actionErr,
		LoginKnown: true,
		Now:        time.Now(),
	}
	a.mu.Unlock()
	snap.LoginEnabled = mygo.App.OpenAtLogin()
	items := BuildMenu(snap)
	tip := tooltip(snap)

	a.mu.Lock()
	changed := !reflect.DeepEqual(items, a.last)
	tipChanged := tip != a.lastTip
	a.last, a.lastTip = items, tip
	a.mu.Unlock()

	if tipChanged {
		a.tray.SetToolTip(tip)
	}
	if changed {
		a.tray.SetMenu(mygo.NewMenu(a.toTemplate(items)))
	}
}

func tooltip(s Snapshot) string {
	switch {
	case s.Status != nil:
		return "mcpctl: running on " + s.Status.Listen
	case s.Err != nil:
		return "mcpctl: stopped"
	default:
		return "mcpctl"
	}
}

func (a *trayApp) toTemplate(items []Item) []*mygo.MenuItem {
	out := make([]*mygo.MenuItem, 0, len(items))
	for _, it := range items {
		if it.Kind == KindSeparator {
			out = append(out, mygo.Separator())
			continue
		}
		mi := &mygo.MenuItem{Label: it.Label, Disabled: it.Disabled, Checked: it.Checked}
		if it.Kind == KindCheckbox {
			mi.Type = mygo.MenuItemCheckbox
		}
		if len(it.Children) > 0 {
			mi.Submenu = a.toTemplate(it.Children)
		}
		if it.Action != ActionNone {
			it := it
			mi.Click = func(*mygo.MenuItem, *mygo.Window) { a.handle(it) }
		}
		out = append(out, mi)
	}
	return out
}

func (a *trayApp) handle(it Item) {
	switch it.Action {
	case ActionQuit:
		mygo.App.Quit()
	case ActionOpenConfig:
		a.report(mygo.Shell.OpenPath(config.DefaultPath()))
	case ActionOpenLogs:
		dir := filepath.Join(a.stateDir, "logs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			a.report(err)
			return
		}
		a.report(mygo.Shell.OpenPath(dir))
	case ActionCopyURL:
		mygo.Clipboard.WriteText(it.Arg)
	case ActionToggleLogin:
		a.report(mygo.App.SetOpenAtLogin(!it.Checked))
		a.kick()
	default:
		go func() {
			a.report(a.run(it))
			a.kick()
		}()
	}
}

func (a *trayApp) run(it Item) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	switch it.Action {
	case ActionToggle:
		if it.Checked {
			_, err := a.client.Disable(ctx, it.Server)
			return err
		}
		_, err := a.client.Enable(ctx, it.Server)
		return err
	case ActionRestart:
		_, err := a.client.Restart(ctx, it.Server)
		return err
	case ActionReload:
		_, err := a.client.Reload(ctx)
		return err
	case ActionStartGateway:
		return startGateway()
	}
	return nil
}

func startGateway() error {
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
	out, err := exec.Command("/bin/launchctl", "kickstart", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart %s failed (not installed? run `mcpctl daemon install`): %s", target, trimOutput(out, err))
	}
	return nil
}

func trimOutput(out []byte, err error) string {
	if s := clip(string(out)); s != "" {
		return s
	}
	return err.Error()
}

func (a *trayApp) report(err error) {
	a.mu.Lock()
	if err == nil {
		a.actionErr = ""
		a.mu.Unlock()
		return
	}
	a.actionErr = err.Error()
	a.mu.Unlock()
	log.Print("tray: ", err)
	go func() {
		_ = mygo.NewNotification(mygo.NotificationOptions{
			ID:    "mcpctl-error",
			Title: "mcpctl",
			Body:  clip(err.Error()),
		}).Show()
	}()
	a.kick()
}
