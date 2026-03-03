package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type CLITool struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ConfigPath  string `json:"configPath"`
	Installed   bool   `json:"installed"`
	HasConfig   bool   `json:"hasConfig"`
}

type ToolGuide struct {
	ToolName    string   `json:"toolName"`
	DisplayName string   `json:"displayName"`
	ConfigPath  string   `json:"configPath"`
	Steps       []string `json:"steps"`
	Snippet     string   `json:"snippet"`
}

type toolDef struct {
	name        string
	displayName string
	binary      string
	configRel   string // relative to $HOME
	format      string // "json-mcpServers", "json-opencode", "toml-codex"
}

var knownTools = []toolDef{
	{"claude", "Claude Code", "claude", ".claude.json", "json-mcpServers"},
	{"cursor", "Cursor", "cursor", ".cursor/mcp.json", "json-mcpServers"},
	{"gemini", "Gemini CLI", "gemini", ".gemini/settings.json", "json-mcpServers"},
	{"codex", "Codex", "codex", ".codex/config.toml", "toml-codex"},
	{"opencode", "OpenCode", "opencode", ".config/opencode/opencode.json", "json-opencode"},
	{"kilo", "Kilo Code", "kilo", ".kilocode/mcp.json", "json-mcpServers"},
	{"antygravity", "Antygravity", "antygravity", ".gemini/antygravity/mcp_config.json", "json-mcpServers"},
}

func (m *Manager) DetectTools() []CLITool {
	home, _ := os.UserHomeDir()
	var result []CLITool

	for _, td := range knownTools {
		configPath := filepath.Join(home, td.configRel)
		_, binErr := exec.LookPath(td.binary)
		_, statErr := os.Stat(configPath)

		installed := binErr == nil
		hasConfig := statErr == nil

		if !installed && !hasConfig {
			continue
		}

		result = append(result, CLITool{
			Name:        td.name,
			DisplayName: td.displayName,
			ConfigPath:  configPath,
			Installed:   installed,
			HasConfig:   hasConfig,
		})
	}

	return result
}

func findToolDef(name string) *toolDef {
	for i := range knownTools {
		if knownTools[i].name == name {
			return &knownTools[i]
		}
	}
	return nil
}

func (m *Manager) ToolGuide(toolName string) (*ToolGuide, error) {
	td := findToolDef(toolName)
	if td == nil {
		return nil, fmt.Errorf("unknown tool %q", toolName)
	}

	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, td.configRel)
	var snippet string
	steps := []string{
		fmt.Sprintf("Open config file: %s", configPath),
		"Add the MCP proxy entry shown below (merge with existing config; do not remove your other servers).",
		"Save file and restart/reload the CLI tool.",
	}

	switch td.format {
	case "json-mcpServers":
		snippet = `{
  "mcpServers": {
    "mcp-catalog-proxy": {
      "type": "streamableHttp",
      "url": "http://127.0.0.1:9847/mcp"
    }
  }
}`
		steps = append(steps,
			"If URL transport is not supported by your client, use stdio variant instead:",
			`"mcp-catalog-proxy": { "command": "/usr/local/bin/mcp-manager", "args": ["--mcp-stdio"] }`,
		)
	case "json-opencode":
		snippet = `{
  "mcp": {
    "mcp-catalog-proxy": {
      "type": "local",
      "command": ["/usr/local/bin/mcp-manager", "--mcp-stdio"],
      "enabled": true
    }
  }
}`
	case "toml-codex":
		snippet = `[mcp_servers.mcp-catalog-proxy]
command = "/usr/local/bin/mcp-manager"
args = ["--mcp-stdio"]
`
	default:
		return nil, fmt.Errorf("unsupported format %q", td.format)
	}

	return &ToolGuide{
		ToolName:    td.name,
		DisplayName: td.displayName,
		ConfigPath:  configPath,
		Steps:       steps,
		Snippet:     snippet,
	}, nil
}
