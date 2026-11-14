package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/mcp"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// mcpSetup holds the MCP configuration and the connection manager.
type mcpSetup struct {
	// manager holds the connected servers.
	manager *mcp.Manager
	// configs are the server configurations that were attempted.
	configs []mcp.ServerConfig
	// sources records where each server's configuration came from.
	sources map[string]string
	// errors holds the connection failures, by server name.
	errors map[string]error
	// userPath is the user-level mcp.json.
	userPath string
	// projectPath is the project-level mcp.json.
	projectPath string
}

// loadMCPServers reads the MCP configuration and connects the servers.
//
// The configuration comes from three places, later ones winning:
//   - ~/.harness/mcp.json (user level)
//   - ./.harness/mcp.json (project level, only when trusted)
//   - the "mcp_servers" key in the harness config file
//
// A server that fails to connect is reported but does not stop the
// others.
func loadMCPServers(ctx context.Context, cfg *config.Config, projectRoot string, registry *tools.Registry) *mcpSetup {
	setup := &mcpSetup{
		manager:     mcp.NewManager(registry),
		sources:     map[string]string{},
		errors:      map[string]error{},
		userPath:    mcp.DefaultUserPath(),
		projectPath: mcp.DefaultProjectPath(projectRoot),
	}
	if cfg.NoMCP {
		return setup
	}

	// The configuration files.
	merged, err := mcp.LoadConfig(setup.userPath, setup.projectPath, cfg.TrustProject)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] mcp config: %v\n", err)
	}
	byName := map[string]mcp.ServerConfig{}
	for _, s := range merged.Servers {
		byName[s.Name] = s
		setup.sources[s.Name] = merged.Sources[s.Name]
	}

	// The servers declared in the harness config file win.
	for name, s := range cfg.MCPServers {
		if s.Name == "" {
			s.Name = name
		}
		byName[s.Name] = s
		setup.sources[s.Name] = "config.json"
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		s := byName[name]
		if err := mcp.Validate(s); err != nil {
			setup.errors[name] = err
			continue
		}
		if !s.IsEnabled() {
			continue
		}
		setup.configs = append(setup.configs, s)
		if err := setup.manager.Connect(ctx, s); err != nil {
			setup.errors[name] = err
		}
	}

	// The resource tools cover every server with resources. They are
	// registered last, so a server's own tool cannot collide with
	// them.
	if err := setup.manager.RegisterResourceTools(); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] mcp resources: %v\n", err)
	}
	return setup
}

// report prints the state of every configured MCP server.
func (s *mcpSetup) report() {
	if len(s.configs) == 0 && len(s.errors) == 0 {
		fmt.Println("no MCP servers configured")
		return
	}
	connected := map[string]bool{}
	for _, st := range s.manager.Statuses() {
		connected[st.Name] = true
		fmt.Printf("%-20s connected  %d tools  %s\n", st.Name, st.Tools, st.Description)
	}
	// The servers that offer resources get their own line: the model
	// can reach their content through the resource tools.
	for _, rs := range s.manager.ResourceServers() {
		fmt.Printf("%-20s resources available\n", rs.Name)
	}
	// The servers that failed come first: they need attention.
	var failed []string
	for name := range s.errors {
		failed = append(failed, name)
	}
	sort.Strings(failed)
	for _, name := range failed {
		fmt.Printf("%-20s failed     %v\n", name, s.errors[name])
	}
}

// toolNames returns the names of the tools a server contributed.
func (s *mcpSetup) toolNames(server string) []string {
	client, ok := s.manager.Client(server)
	if !ok {
		return nil
	}
	var out []string
	for _, t := range client.Tools() {
		out = append(out, mcp.ToolName(server, t.Name))
	}
	return out
}

// addCommand adds a server to the user-level mcp.json.
func (s *mcpSetup) addCommand(name string, server mcp.ServerConfig) error {
	f := &mcp.File{MCPServers: map[string]mcp.ServerConfig{}}
	if existing, err := mcp.LoadFile(s.userPath); err == nil {
		f = existing
		if f.MCPServers == nil {
			f.MCPServers = map[string]mcp.ServerConfig{}
		}
	}
	if err := mcp.Validate(server); err != nil {
		return err
	}
	f.MCPServers[name] = server
	return mcp.SaveFile(s.userPath, f)
}

// removeCommand removes a server from the user-level mcp.json.
func (s *mcpSetup) removeCommand(name string) error {
	f, err := mcp.LoadFile(s.userPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no MCP configuration at %s", s.userPath)
		}
		return err
	}
	if _, ok := f.MCPServers[name]; !ok {
		return fmt.Errorf("no MCP server named %q", name)
	}
	delete(f.MCPServers, name)
	return mcp.SaveFile(s.userPath, f)
}

// listCommand prints the configured servers with their state.
func (s *mcpSetup) listCommand() {
	fmt.Printf("user config:    %s\n", s.userPath)
	fmt.Printf("project config: %s\n", s.projectPath)
	fmt.Println()
	s.report()
}

// parseServerFlags turns the flags of the "mcp add" command into a
// server configuration.
func parseServerFlags(args []string) (name string, server mcp.ServerConfig, err error) {
	if len(args) == 0 {
		return "", server, fmt.Errorf("usage: /mcp add <name> [--url <url>] [-- <command> [args...]]")
	}
	name = args[0]
	rest := args[1:]

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--url":
			if i+1 >= len(rest) {
				return "", server, fmt.Errorf("--url needs a value")
			}
			server.URL = rest[i+1]
			i++
		case "--env":
			if i+1 >= len(rest) {
				return "", server, fmt.Errorf("--env needs a value")
			}
			key, value, found := strings.Cut(rest[i+1], "=")
			if !found {
				return "", server, fmt.Errorf("--env needs KEY=VALUE")
			}
			if server.Env == nil {
				server.Env = map[string]string{}
			}
			server.Env[key] = value
			i++
		case "--description":
			if i+1 >= len(rest) {
				return "", server, fmt.Errorf("--description needs a value")
			}
			server.Description = rest[i+1]
			i++
		case "--":
			// The rest is the command.
			cmd := rest[i+1:]
			if len(cmd) == 0 {
				return "", server, fmt.Errorf("no command given")
			}
			server.Command = cmd[0]
			server.Args = cmd[1:]
			return name, server, nil
		default:
			return "", server, fmt.Errorf("unknown flag %q", rest[i])
		}
	}
	return name, server, nil
}

// expandHome replaces a leading ~/ with the home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
