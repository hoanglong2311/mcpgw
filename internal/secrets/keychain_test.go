package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type call struct {
	stdin string
	args  []string
}

func fake(out string, err error, calls *[]call) Runner {
	return func(_ context.Context, stdin string, args ...string) (string, error) {
		*calls = append(*calls, call{stdin, args})
		return out, err
	}
}

func TestResolve(t *testing.T) {
	var calls []call
	k := &Keychain{Run: fake("s3cret\n", nil, &calls)}
	got, err := k.Resolve("svc/acct")
	if err != nil || got != "s3cret" {
		t.Fatalf("got %q, %v", got, err)
	}
	want := []string{"find-generic-password", "-s", "svc", "-a", "acct", "-w"}
	if strings.Join(calls[0].args, " ") != strings.Join(want, " ") {
		t.Fatalf("args %v", calls[0].args)
	}
}

func TestResolveNotFound(t *testing.T) {
	var calls []call
	k := &Keychain{Run: fake("", errors.New("exit status 44: security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), &calls)}
	_, err := k.Resolve("svc/acct")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "svc/acct") {
		t.Fatalf("err %v", err)
	}
}

func TestResolveOtherError(t *testing.T) {
	var calls []call
	k := &Keychain{Run: fake("", errors.New("boom"), &calls)}
	_, err := k.Resolve("svc/acct")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestResolveTimeout(t *testing.T) {
	k := &Keychain{Timeout: 20 * time.Millisecond, Run: func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	_, err := k.Resolve("svc/acct")
	if err == nil || !strings.Contains(err.Error(), `keychain "svc/acct": timed out waiting for /usr/bin/security`) {
		t.Fatalf("err %v", err)
	}
}

func TestResolveBadRef(t *testing.T) {
	k := &Keychain{Run: func(context.Context, string, ...string) (string, error) { t.Fatal("ran"); return "", nil }}
	for _, ref := range []string{"", "a", "/b", "a/", "a\"b/c", "a/b\nc"} {
		if _, err := k.Resolve(ref); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
}

func TestSetStdinOnly(t *testing.T) {
	var calls []call
	k := &Keychain{Run: fake("", nil, &calls)}
	secret := `p"a\ss word`
	if err := k.Set(context.Background(), "svc/acct", secret); err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if len(c.args) != 1 || c.args[0] != "-i" {
		t.Fatalf("args %v", c.args)
	}
	for _, a := range c.args {
		if strings.Contains(a, "ss word") {
			t.Fatal("secret on argv")
		}
	}
	want := `add-generic-password -U -s "svc" -a "acct" -T "/usr/bin/security" -w "p\"a\\ss word"` + "\n"
	if c.stdin != want {
		t.Fatalf("stdin %q want %q", c.stdin, want)
	}
}

func TestSetRejects(t *testing.T) {
	k := &Keychain{Run: func(context.Context, string, ...string) (string, error) { t.Fatal("ran"); return "", nil }}
	for _, v := range []string{"", "a\nb", "a\x00b", "a\rb"} {
		if err := k.Set(context.Background(), "svc/acct", v); err == nil {
			t.Errorf("value %q accepted", v)
		}
	}
	if err := k.Set(context.Background(), "s\"/a", "v"); err == nil {
		t.Error("bad ref accepted")
	}
}

func TestSetErrorRedacts(t *testing.T) {
	var calls []call
	k := &Keychain{Run: fake("", errors.New("failed near hunter2"), &calls)}
	err := k.Set(context.Background(), "svc/acct", "hunter2")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err %v", err)
	}
}

func TestKeychainIntegration(t *testing.T) {
	if os.Getenv("MCPCTL_KEYCHAIN_IT") != "1" {
		t.Skip("set MCPCTL_KEYCHAIN_IT=1")
	}
	b := make([]byte, 6)
	rand.Read(b)
	ref := "mcpctl-it/" + hex.EncodeToString(b)
	value := `dummy "q" \b ` + hex.EncodeToString(b)
	k := New()
	ctx := context.Background()
	t.Cleanup(func() {
		k.Run(ctx, "", "delete-generic-password", "-s", "mcpctl-it", "-a", strings.TrimPrefix(ref, "mcpctl-it/"))
	})
	if err := k.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}
	got, err := k.Resolve(ref)
	if err != nil || got != value {
		t.Fatalf("resolve: %v match=%v", err, got == value)
	}
	if err := k.Set(ctx, ref, value+"2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Resolve(ref); got != value+"2" {
		t.Fatal("update not applied")
	}
	if _, err := k.Run(ctx, "", "delete-generic-password", "-s", "mcpctl-it", "-a", strings.TrimPrefix(ref, "mcpctl-it/")); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Resolve(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
