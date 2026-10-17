package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const securityBin = "/usr/bin/security"

var ErrNotFound = errors.New("keychain item not found")

type Runner func(ctx context.Context, stdin string, args ...string) (stdout string, err error)

const defaultTimeout = 10 * time.Second

type Keychain struct {
	Run     Runner
	Timeout time.Duration
}

func New() *Keychain {
	return &Keychain{Run: execRunner}
}

func execRunner(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, securityBin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return out.String(), nil
}

func splitRef(ref string) (service, account string, err error) {
	service, account, ok := strings.Cut(ref, "/")
	if !ok || service == "" || account == "" {
		return "", "", fmt.Errorf("invalid keychain ref %q: want service/account", ref)
	}
	for _, p := range []string{service, account} {
		if strings.ContainsAny(p, "\"\n\r\x00") {
			return "", "", fmt.Errorf("invalid keychain ref %q: forbidden character", ref)
		}
	}
	return service, account, nil
}

func (k *Keychain) Resolve(ref string) (string, error) {
	service, account, err := splitRef(ref)
	if err != nil {
		return "", err
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := k.Run(ctx, "", "find-generic-password", "-s", service, "-a", account, "-w")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("keychain %q: timed out waiting for %s (keychain locked?)", ref, securityBin)
		}
		if strings.Contains(err.Error(), "could not be found") || strings.Contains(err.Error(), "exit status 44") {
			return "", fmt.Errorf("keychain %q: %w", ref, ErrNotFound)
		}
		return "", fmt.Errorf("keychain %q: %w", ref, err)
	}
	return strings.TrimSuffix(out, "\n"), nil
}

func (k *Keychain) Set(ctx context.Context, ref, value string) error {
	service, account, err := splitRef(ref)
	if err != nil {
		return err
	}
	if value == "" || strings.ContainsAny(value, "\n\r\x00") {
		return fmt.Errorf("keychain %q: value must be non-empty and contain no newline or NUL", ref)
	}
	cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -T %s -w %s\n",
		quote(service), quote(account), quote(securityBin), quote(value))
	if _, err := k.Run(ctx, cmd, "-i"); err != nil {
		return fmt.Errorf("keychain %q: set failed: %w", ref, redact(err, value))
	}
	return nil
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func redact(err error, value string) error {
	if !strings.Contains(err.Error(), value) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), value, "***"))
}
