package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const togglesFile = "toggles.json"

type Toggles map[string]bool

func LoadToggles(stateDir string) (Toggles, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, togglesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Toggles{}, nil
	}
	if err != nil {
		return nil, err
	}
	t := Toggles{}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", togglesFile, err)
	}
	return t, nil
}

func SaveToggles(stateDir string, t Toggles) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, togglesFile+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(stateDir, togglesFile))
}

func Effective(c *Config, t Toggles) map[string]Server {
	out := make(map[string]Server, len(c.Servers))
	for name, s := range c.Servers {
		if v, ok := t[name]; ok {
			s.Enabled = v
		}
		out[name] = s
	}
	return out
}
