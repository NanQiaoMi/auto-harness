package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File is the on-disk format of an MCP configuration file. It matches
// the layout other MCP clients use.
type File struct {
	// MCPServers maps a server name to its configuration.
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// LoadFile reads an mcp.json file.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", path, err)
	}
	return &f, nil
}

// SaveFile writes an mcp.json file.
func SaveFile(path string, f *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Servers returns the configured servers, sorted by name.
func (f *File) Servers() []ServerConfig {
	if f == nil {
		return nil
	}
	names := make([]string, 0, len(f.MCPServers))
	for n := range f.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ServerConfig, 0, len(names))
	for _, n := range names {
		cfg := f.MCPServers[n]
		if cfg.Name == "" {
			cfg.Name = n
		}
		out = append(out, cfg)
	}
	return out
}

// Config is the merged result of the user-level and project-level MCP
// configuration files.
type Config struct {
	// Servers are the merged server configurations.
	Servers []ServerConfig
	// Sources records where each server came from.
	Sources map[string]string
}

// LoadConfig merges the user-level and project-level MCP files.
//
// A project entry with the same name as a user-level one replaces it.
// The project file is only read when the project is trusted; the caller
// decides that by passing trustProject.
func LoadConfig(userPath, projectPath string, trustProject bool) (*Config, error) {
	cfg := &Config{Sources: map[string]string{}}
	byName := map[string]ServerConfig{}

	if userPath != "" {
		f, err := LoadFile(userPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else {
			for _, s := range f.Servers() {
				byName[s.Name] = s
				cfg.Sources[s.Name] = userPath
			}
		}
	}

	if trustProject && projectPath != "" {
		f, err := LoadFile(projectPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else {
			for _, s := range f.Servers() {
				byName[s.Name] = s
				cfg.Sources[s.Name] = projectPath
			}
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cfg.Servers = append(cfg.Servers, byName[n])
	}
	return cfg, nil
}

// DefaultUserPath returns the user-level MCP configuration path.
func DefaultUserPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".harness/mcp.json"
	}
	return filepath.Join(home, ".harness", "mcp.json")
}

// DefaultProjectPath returns the project-level MCP configuration path.
func DefaultProjectPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".harness", "mcp.json")
}

// Validate checks a server configuration and reports what is wrong.
func Validate(cfg ServerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("mcp: a server needs a name")
	}
	if strings.ContainsAny(cfg.Name, " _-") {
		return fmt.Errorf("mcp: %s: a server name may only contain letters, digits, '_' and '-'", cfg.Name)
	}
	if cfg.Type == "sse" {
		return fmt.Errorf("mcp: %s: the SSE transport is not supported", cfg.Name)
	}
	if cfg.Type != "" && cfg.Type != "stdio" && cfg.Type != "http" && cfg.Type != "streamable-http" {
		return fmt.Errorf("mcp: %s: unknown type %q", cfg.Name, cfg.Type)
	}
	if cfg.Command == "" && cfg.URL == "" {
		return fmt.Errorf("mcp: %s: needs a command or a url", cfg.Name)
	}
	if cfg.Command != "" && cfg.URL != "" {
		return fmt.Errorf("mcp: %s: has both a command and a url", cfg.Name)
	}
	return nil
}
