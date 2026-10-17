package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func ReadSecret(in io.Reader, prompt io.Writer, label string) (string, error) {
	var value string
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(prompt, "Value for %s: ", label)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		value = string(b)
	} else {
		b, err := io.ReadAll(io.LimitReader(in, 1<<20))
		if err != nil {
			return "", err
		}
		value = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if value == "" {
		return "", errors.New("empty secret value")
	}
	return value, nil
}
