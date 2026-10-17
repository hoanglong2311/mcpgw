package clients

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Server struct {
	Name string
	URL  string
}

type Client struct {
	Name   string
	Target string
	Render func(servers []Server, exe string) string
}

var All = []Client{
	{"claude-code", "run in a shell", renderClaudeCode},
	{"codex", "~/.codex/config.toml", renderTOML},
	{"orca", "Orca MCP settings (TOML)", renderTOML},
	{"grok", "~/.grok/config.toml", renderGrok},
	{"hermes", "~/.hermes/config.yaml", renderHermes},
	{"claude-desktop", "~/Library/Application Support/Claude/claude_desktop_config.json", renderClaudeDesktop},
}

func Find(name string) (Client, bool) {
	for _, c := range All {
		if c.Name == name {
			return c, true
		}
	}
	return Client{}, false
}

func Names() []string {
	names := make([]string, len(All))
	for i, c := range All {
		names[i] = c.Name
	}
	return names
}

func renderClaudeCode(servers []Server, _ string) string {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "claude mcp add --transport http --scope user %s %s\n", s.Name, s.URL)
	}
	return b.String()
}

func renderTOML(servers []Server, _ string) string {
	var b strings.Builder
	for i, s := range servers {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\nurl = %q\n", s.Name, s.URL)
	}
	return b.String()
}

func renderGrok(servers []Server, exe string) string {
	return renderTOML(servers, exe) + "\n[compat.claude]\nmcps = false\n"
}

func renderHermes(servers []Server, _ string) string {
	var b strings.Builder
	b.WriteString("mcp_servers:\n")
	for _, s := range servers {
		fmt.Fprintf(&b, "  %s:\n    url: %q\n", s.Name, s.URL)
	}
	return b.String()
}

func renderClaudeDesktop(servers []Server, exe string) string {
	type entry struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	out := struct {
		MCPServers map[string]entry `json:"mcpServers"`
	}{map[string]entry{}}
	for _, s := range servers {
		out.MCPServers[s.Name] = entry{Command: exe, Args: []string{"connect", s.Name}}
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data) + "\n"
}
