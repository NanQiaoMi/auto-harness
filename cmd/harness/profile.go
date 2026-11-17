package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/NanQiaoMi/auto-harness/agentprofile"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/rpc"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// profileSetup is the resolved agent environment for this run.
type profileSetup struct {
	requested string
	resolved  *agentprofile.Resolved
}

// resolveProfile loads the requested profile.
//
// Project profiles are available only when the project was already
// trusted (a saved decision or trust_project). A first-run interactive
// trust prompt happens after model and session assembly, so the caller
// must trust the project in one run before its profile can be selected
// in the next. An explicit profile path remains available on the first
// run because the user named it directly.
func resolveProfile(selector, workdir string, trusted bool) (*profileSetup, error) {
	setup := &profileSetup{requested: strings.TrimSpace(selector)}
	if setup.requested == "" {
		return setup, nil
	}
	resolved, err := agentprofile.Resolve(setup.requested, workdir, trusted)
	if err != nil {
		return nil, err
	}
	setup.resolved = resolved
	return setup, nil
}

// projectTrustedAtStartup reports whether the project may already
// contribute a profile before the full extension-backed trust decision
// runs. Reading the saved store is data-only; no project code runs.
func projectTrustedAtStartup(cfg *config.Config, workdir string) bool {
	if cfg.TrustProject {
		return true
	}
	store := loadTrustStore(trustStorePath())
	return store.decision(absolutePath(workdir)) == trustYes
}

func (p *profileSetup) profile() *agentprofile.Profile {
	if p == nil || p.resolved == nil {
		return nil
	}
	return &p.resolved.Profile
}

func (p *profileSetup) fromProject() bool {
	return p != nil && p.resolved != nil && p.resolved.Scope == agentprofile.ScopeProject
}

// listProfiles prints profile names without executing any of them.
func listProfiles(workdir string) error {
	profiles, err := agentprofile.List(workdir, true)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Println("no agent profiles")
		return nil
	}
	for _, profile := range profiles {
		fmt.Printf("%-20s %-8s %s\n", profile.Name, profile.Scope, profile.Path)
	}
	return nil
}

// report prints the environment the profile asked for and what the
// runtime assembled. Environment values are never printed: a profile
// is a reasonable place to reference a token, not to leak one into a
// terminal transcript.
func (p *profileSetup) report(cfg *config.Config, registry *tools.Registry, loader *extensionLoader, mcpSetup *mcpSetup, skillSetup *skillSetup, promptSetup *promptSetup, model, provider string) {
	if p == nil || p.resolved == nil {
		fmt.Println("agent profile: none")
		fmt.Println("use -profile <name|path> or HARNESS_PROFILE to select one")
		printActiveTools(registry)
		return
	}
	profile := p.resolved.Profile
	fmt.Printf("profile:            %s\n", profile.Name)
	if profile.Description != "" {
		fmt.Printf("description:        %s\n", profile.Description)
	}
	fmt.Printf("source:             %s (%s)\n", p.resolved.Path, p.resolved.Scope)
	fmt.Printf("model:              %s (%s)\n", model, provider)
	turns := cfg.MaxTurns
	if turns == 0 {
		turns = 25
	}
	fmt.Printf("max turns:          %d\n", turns)
	if cfg.MaxTokens > 0 {
		fmt.Printf("max output tokens:  %d\n", cfg.MaxTokens)
	}
	if cfg.Context != nil {
		compactAt := cfg.Context.CompactAt
		if compactAt == 0 {
			compactAt = 0.75
		}
		fmt.Printf("context compact:    %.0f%%\n", compactAt*100)
	}
	printSelection("tools", profile.Tools)
	printSelection("plugins", profile.Plugins)
	printSelection("MCP servers", profile.MCPServers)
	printSelection("skills", profile.Skills)
	printSelection("prompts", profile.Prompts)
	if len(profile.PluginDirs) > 0 {
		fmt.Printf("plugin dirs:        %s\n", strings.Join(profile.PluginDirs, ", "))
	}
	if len(profile.SkillDirs) > 0 {
		fmt.Printf("skill dirs:         %s\n", strings.Join(profile.SkillDirs, ", "))
	}
	if len(profile.PromptDirs) > 0 {
		fmt.Printf("prompt dirs:        %s\n", strings.Join(profile.PromptDirs, ", "))
	}
	if len(profile.Env) > 0 {
		keys := make([]string, 0, len(profile.Env))
		for key := range profile.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Printf("child env keys:     %s\n", strings.Join(keys, ", "))
	}
	printActiveTools(registry)
	if loader != nil {
		fmt.Printf("plugins discovered: %d (profile-selected %d)\n",
			len(loader.pluginRecords), countSelected(loader.pluginRecords))
		for _, err := range loader.diagnostics {
			fmt.Printf("plugin warning:     %v\n", err)
		}
	}
	if mcpSetup != nil {
		fmt.Printf("MCP attempted:      %d\n", len(mcpSetup.configs))
		for _, name := range mcpSetup.missing {
			fmt.Printf("MCP missing:        %s\n", name)
		}
	}
	if skillSetup != nil {
		fmt.Printf("skills loaded:      %d\n", skillSetup.registry.Len())
		for _, name := range skillSetup.missing {
			fmt.Printf("skill missing:      %s\n", name)
		}
	}
	if promptSetup != nil {
		fmt.Printf("prompts loaded:     %d\n", promptSetup.registry.Len())
		for _, name := range promptSetup.missing {
			fmt.Printf("prompt missing:     %s\n", name)
		}
	}
}

func printSelection(label string, values []string) {
	switch {
	case values == nil:
		fmt.Printf("%-19s all\n", label+":")
	case len(values) == 0:
		fmt.Printf("%-19s none (explicit)\n", label+":")
	default:
		fmt.Printf("%-19s %s\n", label+":", strings.Join(values, ", "))
	}
}

func printActiveTools(registry *tools.Registry) {
	if registry == nil {
		return
	}
	active := registry.GetActiveTools()
	all := registry.GetAllTools()
	names := make([]string, 0, len(active))
	for _, tool := range active {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	fmt.Printf("active tools:       %d/%d %s\n", len(active), len(all), strings.Join(names, ", "))
}

func countSelected(records []pluginRecord) int {
	count := 0
	for _, record := range records {
		if record.Selected {
			count++
		}
	}
	return count
}

// profileSelector applies the HARNESS_PROFILE default when the flag is
// absent.
func profileSelector(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return os.Getenv("HARNESS_PROFILE")
}

func profileSkills(profile *agentprofile.Profile) (dirs, names []string) {
	if profile == nil {
		return nil, nil
	}
	return profile.SkillDirs, profile.Skills
}

func profilePrompts(profile *agentprofile.Profile) (dirs, names []string) {
	if profile == nil {
		return nil, nil
	}
	return profile.PromptDirs, profile.Prompts
}

// rpcPayload builds the stable machine-readable environment report.
func (p *profileSetup) rpcPayload(cfg *config.Config, registry *tools.Registry, loader *extensionLoader, model, provider string) rpc.ProfilePayload {
	payload := rpc.ProfilePayload{
		Model:     model,
		Provider:  provider,
		MaxTurns:  cfg.MaxTurns,
		MaxTokens: cfg.MaxTokens,
	}
	if registry != nil {
		active := registry.GetActiveTools()
		payload.ActiveTools = make([]string, 0, len(active))
		for _, tool := range active {
			payload.ActiveTools = append(payload.ActiveTools, tool.Name)
		}
		sort.Strings(payload.ActiveTools)
		payload.AllTools = len(registry.GetAllTools())
	}
	if p == nil || p.resolved == nil {
		payload.ActiveTools = nonNilStrings(payload.ActiveTools)
		payload.Tools = []string{}
		payload.Plugins = []string{}
		payload.MCPServers = []string{}
		payload.Skills = []string{}
		payload.Prompts = []string{}
		return payload
	}
	profile := p.resolved.Profile
	payload.Enabled = true
	payload.Name = profile.Name
	payload.Description = profile.Description
	payload.Source = p.resolved.Path
	payload.Scope = string(p.resolved.Scope)
	payload.Tools, payload.ToolsAll = selectionPayload(profile.Tools)
	payload.Plugins, payload.PluginsAll = selectionPayload(profile.Plugins)
	payload.MCPServers, payload.MCPServersAll = selectionPayload(profile.MCPServers)
	payload.Skills, payload.SkillsAll = selectionPayload(profile.Skills)
	payload.Prompts, payload.PromptsAll = selectionPayload(profile.Prompts)
	payload.PluginDirs = append([]string{}, profile.PluginDirs...)
	payload.SkillDirs = append([]string{}, profile.SkillDirs...)
	payload.PromptDirs = append([]string{}, profile.PromptDirs...)
	for key := range profile.Env {
		payload.EnvKeys = append(payload.EnvKeys, key)
	}
	sort.Strings(payload.EnvKeys)
	if loader != nil {
		for _, err := range loader.diagnostics {
			payload.PluginWarnings = append(payload.PluginWarnings, err.Error())
		}
	}
	return payload
}

func selectionPayload(values []string) ([]string, bool) {
	if values == nil {
		return []string{}, true
	}
	return append([]string{}, values...), false
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// pluginsPayload snapshots the plugin catalog for RPC clients.
func pluginsPayload(loader *extensionLoader, manager *ext.Manager) rpc.PluginsPayload {
	payload := rpc.PluginsPayload{Plugins: []rpc.PluginInfo{}}
	if loader == nil {
		return payload
	}
	for _, record := range loader.pluginRecords {
		info := rpc.PluginInfo{
			Name:        record.Candidate.Name(),
			Version:     record.Candidate.Version(),
			Path:        record.Candidate.Path,
			Selected:    record.Selected,
			Description: record.Candidate.Manifest.Description,
		}
		switch {
		case !record.Selected:
			info.State = "skipped"
		case record.Err != nil:
			info.State = "error"
			info.Error = record.Err.Error()
		case manager.ProcessExtensionLoaded(record.Extension):
			status := record.Extension.SupervisionStatus()
			info.RestartMode = string(status.Mode)
			info.RestartCount = status.Restarts
			info.MaxRestarts = status.MaxRestarts
			info.Healthy = status.Healthy
			info.Restarting = status.Restarting
			info.LastExit = status.LastExit
			info.LastError = status.LastError
			switch {
			case status.Restarting:
				info.State = "restarting"
			case status.Healthy:
				info.State = "loaded"
			default:
				info.State = "unhealthy"
			}
		default:
			info.State = "not_loaded"
		}
		payload.Plugins = append(payload.Plugins, info)
	}
	sort.Slice(payload.Plugins, func(i, j int) bool {
		return payload.Plugins[i].Name < payload.Plugins[j].Name
	})
	return payload
}
