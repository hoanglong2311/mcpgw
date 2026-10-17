package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Label       = "local.mcpctl"
	launchctl   = "/bin/launchctl"
	defaultPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

type Runner func(ctx context.Context, args ...string) (string, error)

func ExecRunner(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, launchctl, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

type Spec struct {
	Executable string
	Args       []string
	StateDir   string
	Home       string
}

func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func (s Spec) Plist() []byte {
	var b bytes.Buffer
	str := func(indent, v string) {
		b.WriteString(indent + "<string>")
		_ = xml.EscapeText(&b, []byte(v))
		b.WriteString("</string>\n")
	}
	key := func(indent, k string) { b.WriteString(indent + "<key>" + k + "</key>\n") }

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key("\t", "Label")
	str("\t", Label)
	key("\t", "ProgramArguments")
	b.WriteString("\t<array>\n")
	str("\t\t", s.Executable)
	str("\t\t", "serve")
	for _, a := range s.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	key("\t", "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("\t", "KeepAlive")
	b.WriteString("\t<dict>\n")
	key("\t\t", "SuccessfulExit")
	b.WriteString("\t\t<false/>\n\t</dict>\n")
	key("\t", "ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")
	key("\t", "EnvironmentVariables")
	b.WriteString("\t<dict>\n")
	key("\t\t", "HOME")
	str("\t\t", s.Home)
	key("\t\t", "PATH")
	str("\t\t", defaultPath)
	b.WriteString("\t</dict>\n")
	key("\t", "StandardOutPath")
	str("\t", filepath.Join(s.StateDir, "logs", "launchd.out"))
	key("\t", "StandardErrorPath")
	str("\t", filepath.Join(s.StateDir, "logs", "launchd.err"))
	key("\t", "ProcessType")
	str("\t", "Standard")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

type Manager struct {
	Run  Runner
	UID  int
	Home string
}

func (m *Manager) domainTarget() string  { return "gui/" + strconv.Itoa(m.UID) }
func (m *Manager) serviceTarget() string { return m.domainTarget() + "/" + Label }

func (m *Manager) Install(ctx context.Context, s Spec) (string, error) {
	path := PlistPath(m.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(s.StateDir, "logs"), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, s.Plist(), 0o644); err != nil {
		return "", err
	}
	_, _ = m.Run(ctx, "bootout", m.serviceTarget())
	if out, err := m.Run(ctx, "bootstrap", m.domainTarget(), path); err != nil {
		return path, fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(out))
	}
	return path, nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	_, _ = m.Run(ctx, "bootout", m.serviceTarget())
	if err := os.Remove(PlistPath(m.Home)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type Info struct {
	PlistInstalled bool
	Loaded         bool
	State          string
	PID            int
	LastExit       string
}

func (m *Manager) Status(ctx context.Context) (Info, error) {
	var info Info
	if _, err := os.Stat(PlistPath(m.Home)); err == nil {
		info.PlistInstalled = true
	}
	out, err := m.Run(ctx, "print", m.serviceTarget())
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) || strings.Contains(out, "Could not find service") {
			return info, nil
		}
		return info, fmt.Errorf("launchctl print: %w", err)
	}
	info.Loaded = true
	parsePrint(out, &info)
	return info, nil
}

func parsePrint(out string, info *Info) {
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			info.State = v
		case "pid":
			info.PID, _ = strconv.Atoi(v)
		case "last exit code":
			info.LastExit = v
		}
	}
}
