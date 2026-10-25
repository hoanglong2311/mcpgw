package upstream

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hoanglong2311/mcpgw/internal/config"
)

var envAllowlist = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "SHELL"}

func buildEnv(cfg config.Server, resolve func(string) (string, error)) ([]string, error) {
	m := make(map[string]string)
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			m[k] = v
		}
	}
	for k, v := range cfg.Env {
		m[k] = v
	}
	for k, ref := range cfg.KeychainEnv {
		if resolve == nil {
			return nil, fmt.Errorf("resolve keychain_env %s: no resolver configured", k)
		}
		v, err := resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve keychain_env %s: %w", k, err)
		}
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

func buildCmd(cfg config.Server, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = env
	cmd.Dir = cfg.Cwd
	if cmd.Dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		cmd.Dir = home
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

type proc struct {
	pid      int
	pgidPath string
}

func writePgidFile(path string, pid int, command string) error {
	line := strconv.Itoa(pid) + " " + filepath.Base(command) + "\n"
	return os.WriteFile(path, []byte(line), 0o600)
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killGroup(pgid int, grace time.Duration) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if waitGroupGone(pgid, grace) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	waitGroupGone(pgid, time.Second)
}

func waitGroupGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !groupAlive(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroups() (map[int][]string, error) {
	out, err := exec.Command("ps", "-axo", "pgid=,command=").Output()
	if err != nil {
		return nil, err
	}
	groups := make(map[int][]string)
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		pgid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		groups[pgid] = append(groups[pgid], strings.Join(f[1:], " "))
	}
	return groups, sc.Err()
}

func (m *Manager) ReapStale() error {
	runDir := filepath.Join(m.opts.StateDir, "run")
	files, err := filepath.Glob(filepath.Join(runDir, "*.pgid"))
	if err != nil || len(files) == 0 {
		return err
	}
	groups, err := processGroups()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	var errs []error
	for _, path := range files {
		pgid, base := parsePgidFile(path)
		if base == "" {
			base = m.commandBase(strings.TrimSuffix(filepath.Base(path), ".pgid"))
		}
		if pgid > 1 && base != "" && groupMatches(groups[pgid], base) {
			m.opts.Logger.Warn("reaping stale process group", "pgid", pgid, "file", filepath.Base(path))
			killGroup(pgid, m.opts.stopGrace)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func parsePgidFile(path string) (pgid int, base string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, ""
	}
	pgid, _ = strconv.Atoi(f[0])
	if len(f) > 1 {
		base = f[1]
	}
	return pgid, base
}

func groupMatches(cmds []string, base string) bool {
	for _, c := range cmds {
		if strings.Contains(c, base) {
			return true
		}
	}
	return false
}
