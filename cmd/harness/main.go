// Command harness is the auto-harness CLI: a pi-style coding agent
// harness with a streaming REPL, slash commands and one-shot mode.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/contextmgr"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/perm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/shellext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

const version = "0.1.5"

var errExitREPL = errors.New("exit requested")

func main() {
	var (
		modelFlag        = flag.String("model", "", "model ID (default: config or claude-sonnet-5-5)")
		providerFlag     = flag.String("provider", "", "provider: openai | anthropic (default: inferred from model)")
		baseURLFlag      = flag.String("base-url", "", "provider base URL override")
		systemFlag       = flag.String("system", "", "override the system prompt")
		maxTurns         = flag.Int("max-turns", 0, "cap on agent loop iterations (default 25)")
		maxTokens        = flag.Int("max-tokens", 0, "cap on tokens per completion (provider default)")
		sessionFlag      = flag.String("session", "", "resume session ID")
		listSessions     = flag.Bool("list-sessions", false, "list saved sessions and exit")
		printMode        = flag.Bool("print", false, "one-shot mode: run the given prompt and exit")
		showVersion      = flag.Bool("version", false, "print version and exit")
		quiet            = flag.Bool("quiet", false, "suppress tool/hook notices (model text only)")
		showThinking     = flag.Bool("show-thinking", false, "stream model thinking output to stderr")
		tuiMode          = flag.Bool("tui", false, "use the differential-rendering TUI for the REPL")
		rpcMode          = flag.Bool("rpc", false, "run in RPC mode: JSON commands on stdin, records on stdout")
		modeFlag         = flag.String("mode", "", "output mode: json (one-shot event stream on stdout)")
		noMCP            = flag.Bool("no-mcp", false, "disable MCP servers for this run")
		listMCP          = flag.Bool("mcp-list", false, "list configured MCP servers and exit")
		profileFlag      = flag.String("profile", "", "agent environment profile: name or JSON path")
		listProfilesFlag = flag.Bool("list-profiles", false, "list available agent profiles and exit")
		pluginIndex      = flag.String("plugin-index", os.Getenv("HARNESS_PLUGIN_INDEX"), "plugin registry path or HTTPS URL")
		pluginInstall    = flag.String("plugin-install", "", "install a managed plugin: name[@version]")
		pluginRemove     = flag.String("plugin-remove", "", "remove a managed plugin")
		pluginList       = flag.Bool("plugin-list", false, "list managed plugins and exit")
		pluginVerify     = flag.Bool("plugin-verify", false, "verify the managed plugin lock and trees")
		pluginTrust      = flag.String("plugin-trust", "", "trust a publisher key: publisher=/path/to/public-key")
		pluginUntrust    = flag.String("plugin-untrust", "", "remove trust for one plugin publisher")
		pluginTrustList  = flag.Bool("plugin-trust-list", false, "list trusted plugin publishers and keys")
		pluginUnsigned   = flag.Bool("plugin-allow-unsigned", false, "allow an unsigned remote plugin registry")
		noGitCheckpoints = flag.Bool("no-git-checkpoints", false, "disable automatic workspace checkpoints")
		checkpointLimit  = flag.Int("checkpoint-limit", gitkit.DefaultCheckpointLimit, "maximum retained workspace checkpoints")
	)
	flag.Parse()

	if *checkpointLimit < 1 {
		fatal(fmt.Errorf("-checkpoint-limit must be at least 1"))
	}

	if *showVersion {
		fmt.Println("harness", version)
		return
	}

	// The output mode is one flag: "json" is the machine-readable
	// event stream. The RPC mode has its own flag because it is
	// driven rather than one-shot.
	jsonOutput := false
	switch *modeFlag {
	case "":
	case "json":
		jsonOutput = true
	default:
		fatal(fmt.Errorf("invalid mode %q: valid values are json", *modeFlag))
	}

	if handled, code, err := runPluginCommand(pluginCommandOptions{
		Index:         *pluginIndex,
		Install:       *pluginInstall,
		Remove:        *pluginRemove,
		List:          *pluginList,
		Verify:        *pluginVerify,
		Trust:         *pluginTrust,
		Untrust:       *pluginUntrust,
		TrustList:     *pluginTrustList,
		AllowUnsigned: *pluginUnsigned,
	}); handled {
		if err != nil {
			fatal(err)
		}
		if code != 0 {
			os.Exit(code)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if *listProfilesFlag {
		if err := listProfiles(workdir()); err != nil {
			fatal(err)
		}
		return
	}
	profileSetup, err := resolveProfile(profileSelector(*profileFlag), workdir(),
		projectTrustedAtStartup(cfg, workdir()))
	if err != nil {
		fatal(err)
	}
	if profileSetup.profile() != nil {
		profileSetup.resolved.ApplyToConfig(cfg)
	}
	if *modelFlag != "" {
		cfg.Model = *modelFlag
	}
	if *providerFlag != "" {
		cfg.Provider = *providerFlag
	} else if *modelFlag != "" {
		cfg.Provider = llm.GuessProvider(*modelFlag)
	}
	if *baseURLFlag != "" {
		cfg.BaseURL = *baseURLFlag
	}
	if *systemFlag != "" {
		cfg.SystemPrompt = *systemFlag
	}
	if *maxTurns > 0 {
		cfg.MaxTurns = *maxTurns
	}
	if *maxTokens > 0 {
		cfg.MaxTokens = *maxTokens
	}
	if *noMCP {
		cfg.NoMCP = true
	}
	cfg.ResolveProviderEnvironment()

	if *listSessions {
		list, err := session.List(cfg.SessionDir)
		if err != nil {
			fatal(err)
		}
		if len(list) == 0 {
			fmt.Println("no sessions")
			return
		}
		for _, info := range list {
			fmt.Printf("%s  %3d msgs  %-22s %s\n",
				info.ID, info.Messages, info.CreatedAt.Local().Format("2006-01-02 15:04:05"), info.Model)
		}
		return
	}

	// --- model registry: one harness, many providers ------------------
	llmReg := llm.NewRegistry()
	// The configured provider is registered under both its own name and
	// its protocol, so protocol-based routing (a catalog that says a
	// model speaks the Anthropic API) finds the right endpoint.
	primary := llm.ProviderConfig{
		Name:     cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
		Protocol: cfg.Provider,
	}
	if err := llmReg.RegisterProvider(primary); err != nil {
		fatal(err)
	}
	if cfg.Provider == "openai" || cfg.Provider == "anthropic" {
		if err := llmReg.RegisterProvider(llm.ProviderConfig{
			Name:     cfg.Provider + "-proto",
			BaseURL:  cfg.BaseURL,
			APIKey:   cfg.APIKey,
			Protocol: cfg.Provider,
		}); err != nil {
			fatal(err)
		}
	}
	// The default model is served by the configured provider unless the
	// catalog says otherwise.
	if m, ok := llmReg.Model(cfg.Model); ok {
		m.Provider = cfg.Provider
		llmReg.RegisterModel(m)
	} else {
		llmReg.RegisterModel(llm.Model{
			ID: cfg.Model, Name: cfg.Model,
			Provider: cfg.Provider, Protocol: cfg.Provider,
		})
	}
	// Extra provider endpoints from the config file (multi-provider
	// setups): each entry is a full provider config.
	for _, pc := range cfg.Providers {
		if err := llmReg.RegisterProvider(pc); err != nil {
			fatal(err)
		}
	}
	// Remote catalog sync is opt-in via the config file.
	if cfg.CatalogURL != "" {
		if err := llmReg.SyncCatalog(cfg.Provider, cfg.CatalogURL, cfg.APIKey); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] catalog sync: %v\n", err)
		}
	}

	// --- session ---------------------------------------------------
	// The session is created first: gateways route on a stable session
	// header, which is derived from the session id.
	var sess *session.Session
	if *sessionFlag != "" {
		sess, err = session.Load(cfg.SessionDir, *sessionFlag)
		if err != nil {
			fatal(err)
		}
	} else {
		sess, err = session.New(cfg.SessionDir, cfg.Model)
		if err != nil {
			fatal(err)
		}
	}
	defer sess.Close()

	// Gateways like OpenCode Go use a stable session header for routing
	// and prompt caching; identify the conversation up front. The registry
	// applies it to every provider it builds, so switching models across
	// providers keeps the header.
	llmReg.SetHeaders(map[string]string{"x-opencode-session": sess.Meta.ID})

	provider, err := llmReg.ProviderFor(cfg.Model, cfg.Provider)
	if err != nil {
		fatal(err)
	}

	// --- wiring: bus, tools, commands, extensions -------------------
	bus := ext.NewBus()
	registry := tools.NewRegistry()
	commands := ext.NewCommandRegistry()
	api := ext.NewAPI(bus, registry, commands)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The permission layer is an extension, like pi's: the kernel has
	// no permission system of its own. A one-shot run has nobody to
	// approve a call, so it fails safe there.
	//
	// The RPC mode is never interactive: its stdin carries the
	// protocol, not the user's typing, so a permission prompt would
	// eat the client's records. Its commands are the client's own,
	// so they are allowed and the gate only vetoes them.
	//
	// The JSON mode is one-shot like the print mode, so it is
	// non-interactive for the same reason.
	interactive := !*printMode && !*rpcMode && !jsonOutput && perm.IsTerminal(os.Stdin)

	// The REPL, the permission prompt and the trust prompt share one
	// reader over stdin: several readers over the same stream would
	// steal each other's input.
	var lines perm.LineReader
	if interactive {
		lines = perm.NewLineReader(os.Stdin)
	}

	// --- extensions: the trusted scope first -----------------------
	// The user's extensions load before the trust decision: they are
	// trusted by definition, and they are the ones that can decide
	// whether to trust the project.
	extMgr := ext.NewManager(api)
	shutdownTrusted := func() {
		extMgr.Shutdown()
		_ = sess.Close()
	}
	loader := &extensionLoader{projectRoot: workdir(), cfg: cfg}
	loader.configure(profileSetup.profile(), profileSetup.fromProject())
	if err := loadExtensionSet(extMgr, loader.build()); err != nil {
		// A broken extension must not stop the harness from starting.
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}

	// --- project trust ---------------------------------------------
	// Nothing the project ships runs or loads before this decision: its
	// extensions, MCP servers, permission rules, skills and prompt
	// templates all wait for it.
	trustSetup := resolveProjectTrust(cfg, workdir(), bus, interactive, trustPrompter(lines))
	if profileSetup.fromProject() && !trustSetup.decision.Trusted {
		failStartup(fmt.Errorf("agent profile %q belongs to a project that was not trusted for this run",
			profileSetup.resolved.Profile.Name),
			shutdownTrusted)
	}
	loader.trusted = trustSetup.decision.Trusted
	if err := loadExtensionSet(extMgr, loader.buildProject()); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}
	if err := loader.missingPluginError(); err != nil {
		failStartup(err, shutdownTrusted)
	}
	if err := loader.selectedPluginError(extMgr); err != nil {
		failStartup(err, shutdownTrusted)
	}

	// --- permissions and sandbox ----------------------------------
	permSetup := loadPermissions(ctx, cfg, workdir(), registry, interactive, trustSetup.decision.Trusted)

	checkpointMgr := gitkit.NewCheckpointManager(workdir())
	checkpointMgr.Limit = *checkpointLimit

	// The bash tool runs inside the sandbox when one is configured.
	bashTool := tools.NewBashTool()
	if runner := permSetup.sandboxRunner(); runner != nil {
		bashTool = tools.NewSandboxedBashTool(runner)
	}
	for _, t := range []*tools.Tool{
		bashTool,
		tools.NewReadTool(),
		tools.NewWriteTool(),
		tools.NewEditTool(),
		gitkit.NewGitTool(workdir()),
		gitkit.NewCheckpointTool(checkpointMgr),
	} {
		if err := registry.Register(t); err != nil {
			failStartup(err, shutdownTrusted)
		}
	}

	// The permission guard joins the extension set last, so it sees
	// what every other handler decided about a call, and survives a
	// reload like everything else.
	if !cfg.NoPermissions {
		if err := extMgr.Load(permSetup.guard); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
		}
	}

	// --- telemetry -------------------------------------------------
	// Opt-in local diagnostics. The collector is an extension like any
	// other; the kernel does not know telemetry exists.
	telemetrySetup := loadTelemetry(cfg, cfg.Provider, cfg.Model)
	defer telemetrySetup.Close()
	telemetrySetup.load(extMgr)

	defer extMgr.Shutdown()

	// --- MCP servers ---------------------------------------------
	// The servers' tools are registered like any other tool, so the
	// model can call them without knowing about MCP.
	activeProfile := profileSetup.profile()
	var mcpSelection []string
	var childEnv map[string]string
	if activeProfile != nil {
		mcpSelection = activeProfile.MCPServers
		childEnv = activeProfile.Env
	}
	mcpSetup := loadMCPServers(ctx, cfg, workdir(), registry, trustSetup.decision.Trusted,
		mcpSelection, childEnv)
	defer mcpSetup.manager.Close()
	shutdownRuntime := func() {
		mcpSetup.manager.Close()
		extMgr.Shutdown()
		telemetrySetup.Close()
		_ = sess.Close()
	}
	for name, err := range mcpSetup.errors {
		fmt.Fprintf(os.Stderr, "[warn] mcp %s: %v\n", name, err)
	}

	// -mcp-list connects to every server, prints the state and exits.
	if *listMCP {
		mcpSetup.listCommand()
		if len(mcpSetup.errors) > 0 || len(mcpSetup.missing) > 0 {
			exitStatus(1, shutdownRuntime)
		}
		return
	}
	if len(mcpSetup.missing) > 0 {
		failStartup(fmt.Errorf("agent profile selected MCP servers that are not configured: %s",
			strings.Join(mcpSetup.missing, ", ")),
			shutdownRuntime)
	}
	if failed := mcpSetup.selectedFailures(); len(failed) > 0 {
		failStartup(fmt.Errorf("agent profile selected MCP servers that failed to connect: %s",
			strings.Join(failed, ", ")),
			shutdownRuntime)
	}

	// --- prompt templates, project context and skills ---------------
	// The templates are markdown files that become slash commands; the
	// context files are the project's instructions; the skills are the
	// instructions the model loads when a task matches. All three are
	// optional and all three are data, not code.
	promptDirs, promptNames := profilePrompts(activeProfile)
	promptSetup := loadPrompts(cfg, workdir(), trustSetup.decision.Trusted, promptDirs, promptNames)
	skillDirs, skillNames := profileSkills(activeProfile)
	skillSetup := loadSkills(cfg, workdir(), trustSetup.decision.Trusted, skillDirs, skillNames)
	var missingContent []string
	for _, name := range skillSetup.missing {
		missingContent = append(missingContent, "skill:"+name)
	}
	for _, name := range promptSetup.missing {
		missingContent = append(missingContent, "prompt:"+name)
	}
	if len(missingContent) > 0 {
		failStartup(fmt.Errorf("agent profile selections were not found: %s", strings.Join(missingContent, ", ")),
			shutdownRuntime)
	}
	if activeProfile != nil && activeProfile.Tools != nil {
		if err := registry.SetActiveToolsExact(activeProfile.Tools); err != nil {
			failStartup(fmt.Errorf("agent profile tools: %w", err), shutdownRuntime)
		}
	}

	contextOpts := contextmgr.Options{}
	configuredWindow := 0
	if cfg.Context != nil {
		configuredWindow = cfg.Context.Window
		contextOpts.ContextWindow = configuredWindow
		contextOpts.ReserveTokens = cfg.Context.ReserveTokens
		contextOpts.CompactAt = cfg.Context.CompactAt
		contextOpts.KeepRecentTokens = cfg.Context.KeepRecentTokens
		contextOpts.KeepRecentMessages = cfg.Context.KeepRecentMessages
		contextOpts.SummaryMaxTokens = cfg.Context.SummaryMaxTokens
		contextOpts.SummaryChunkTokens = cfg.Context.SummaryChunkTokens
		contextOpts.ArtifactThreshold = cfg.Context.ArtifactThreshold
		contextOpts.ArtifactPreview = cfg.Context.ArtifactPreview
	}
	if cfg.MaxTokens > 0 {
		contextOpts.MaxOutputTokens = cfg.MaxTokens
	}

	ag := agent.New(agent.Config{
		Provider: provider,
		Model:    cfg.Model,
		// The project context files fold into the system prompt, and
		// the skills are advertised in it.
		SystemPrompt:  skillSetup.systemPrompt(promptSetup.systemPrompt(cfg.SystemPrompt)),
		MaxTokens:     cfg.MaxTokens,
		ContextWindow: configuredWindow,
		MaxTurns:      cfg.MaxTurns,
		ContextBudget: func(modelID string) (int, int) {
			if info, ok := llmReg.Model(modelID); ok {
				return info.ContextWindow, info.MaxOutput
			}
			return 0, 0
		},
		// Resolve the provider per model so /model can cross providers.
		Resolver: func(modelID string) (llm.Provider, error) {
			return llmReg.ProviderFor(modelID, "")
		},
	}, bus, registry)
	ag.Session = sess
	if !*noGitCheckpoints {
		ag.Checkpointer = checkpointMgr
		if _, created, err := checkpointMgr.Create(ctx, "session start",
			gitkit.CheckpointManual, "session"); err != nil {
			if !errors.Is(err, gitkit.ErrNotRepository) {
				fmt.Fprintf(os.Stderr, "[warn] workspace checkpoint: %v\n", err)
			}
		} else if created && !*quiet {
			fmt.Fprintln(os.Stderr, "[checkpoint] created initial workspace snapshot")
		}
	}
	ag.Compactor = func(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
		model := ag.Config().Model
		current, err := llmReg.ProviderFor(model, "")
		if err != nil {
			return nil, err
		}
		return agent.NewSummarizer(current, model)(ctx, messages)
	}
	ag.Logf = func(format string, args ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[hook] "+format+"\n", args...)
		}
	}
	ag.SetHistory(sess.Messages)

	// Context budgeting and automatic compression are part of the
	// runtime, not a one-off command. The manager estimates each
	// provider request, summarizes old units, and moves oversized tool
	// output into the session's artifact store when needed.
	artifactDir := ""
	if cfg.Context != nil {
		artifactDir = cfg.Context.ArtifactDir
	}
	if artifactDir == "" {
		sessionDir := cfg.SessionDir
		if sessionDir == "" {
			sessionDir = filepath.Dir(sess.Path)
		}
		artifactDir = filepath.Join(sessionDir, "artifacts", sess.Meta.ID)
	}
	contextMgr := contextmgr.NewManager(
		contextOpts,
		nil,
		contextmgr.SummarizerFunc(func(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
			model := ag.Config().Model
			p, err := llmReg.ProviderFor(model, "")
			if err != nil {
				return "", err
			}
			return contextmgr.SummarizeOnce(ctx, p, model, messages, maxTokens)
		}),
		contextmgr.NewFileArtifactStore(artifactDir),
	)
	ag.SetContextManager(contextMgr)

	// The "!" shortcut runs the user's own shell commands, through the
	// same bash tool, so the sandbox applies to them too.
	var userShellRunner shellext.Runner = &shellext.ToolRunner{Tool: bashTool}
	if !*noGitCheckpoints {
		userShellRunner = &workspaceCheckpointRunner{
			checkpointer: checkpointMgr,
			runner:       userShellRunner,
		}
	}
	shellShortcut := &shellext.Shortcut{
		Runner:       userShellRunner,
		Conversation: ag,
		Bus:          bus,
		Out:          os.Stdout,
	}

	// The UI is just another extension consumer: stream model text as it
	// arrives and show one-line tool notices. The TUI installs its own
	// handlers, so they are only registered here.
	//
	// The RPC mode writes the protocol to stdout, and the JSON mode
	// writes the event stream there, so nothing else may go there: the
	// model's text goes to stderr, and the consumer reads the events
	// off the stream instead.
	if !*tuiMode && !*rpcMode && !jsonOutput {
		bus.On(ext.EventMessageUpdate, func(e *ext.Event) error {
			if e.Data != nil {
				if t, _ := e.Data["thinking"].(bool); t {
					if *showThinking {
						fmt.Fprint(os.Stderr, e.Text)
					}
					return nil
				}
			}
			fmt.Print(e.Text)
			return nil
		})
		notice := &toolNotice{registry: registry, cwd: workdir()}
		notice.install(bus, *quiet, func() bool { return false }, func(line string) {
			fmt.Fprintf(os.Stderr, "%s\n", line)
		})
	}

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart, Reason: sessionReason(*sessionFlag)})

	// The JSON mode is one-shot like the print mode, but it tells the
	// whole story: the session header first, then every event, then
	// the record that says the run is over.
	if jsonOutput {
		prompt := strings.Join(flag.Args(), " ")
		if strings.TrimSpace(prompt) == "" {
			failStartup(fmt.Errorf("--mode json requires a prompt argument"), shutdownRuntime)
		}
		// A skill command is expanded here, the way the print mode
		// does it: the REPL dispatches it through the registry.
		if expanded, ok := skillSetup.expand(prompt); ok {
			prompt = expanded
		}
		if code := runJSONMode(ctx, ag, sess, bus, prompt, workdir()); code != 0 {
			exitStatus(code, shutdownRuntime)
		}
		return
	}

	if *printMode {
		prompt := strings.Join(flag.Args(), " ")
		if strings.TrimSpace(prompt) == "" {
			failStartup(fmt.Errorf("-print requires a prompt argument"), shutdownRuntime)
		}
		// A skill command in one-shot mode is expanded here: the REPL
		// dispatches it through the command registry instead.
		if expanded, ok := skillSetup.expand(prompt); ok {
			prompt = expanded
		}
		res, err := ag.Run(ctx, prompt)
		if err != nil {
			failStartup(err, shutdownRuntime)
		}
		if res.Final != "" && !strings.HasSuffix(res.Final, "\n") {
			fmt.Println()
		}
		return
	}

	// The RPC mode is headless: stdout carries the protocol and
	// nothing else, so the model's text and the tool notices must not
	// go there. The RPC server writes the records itself.
	if *rpcMode {
		if err := runRPCMode(ctx, ag, sess, bus, commands, cfg, llmReg, extMgr,
			loader, mcpSetup, permSetup, telemetrySetup, shellShortcut, promptSetup,
			skillSetup, trustSetup, profileSetup, checkpointMgr, *quiet); err != nil {
			fmt.Fprintf(os.Stderr, "harness: %v\n", err)
			exitStatus(1, shutdownRuntime)
		}
		return
	}

	runREPL(ctx, ag, sess, bus, commands, cfg, llmReg, extMgr, loader, mcpSetup, permSetup,
		telemetrySetup, shellShortcut, promptSetup, skillSetup, trustSetup, profileSetup,
		checkpointMgr, lines, *quiet, *tuiMode)
}

// workdir returns the current working directory, used to locate the
// project-local extension directory.
func workdir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

func sessionReason(resumed string) string {
	if resumed != "" {
		return "resume"
	}
	return "new"
}

// runREPL is the interactive loop.
func runREPL(ctx context.Context, ag *agent.Agent, sess *session.Session, bus *ext.Bus, commands *ext.CommandRegistry, cfg *config.Config, llmReg *llm.Registry, extMgr *ext.Manager, loader *extensionLoader, mcpSetup *mcpSetup, permSetup *permSetup, telemetrySetup *telemetrySetup, shellShortcut *shellext.Shortcut, promptSetup *promptSetup, skillSetup *skillSetup, trustSetup *trustSetup, profileSetup *profileSetup, checkpointMgr *gitkit.Manager, lines perm.LineReader, quiet bool, tuiMode bool) {
	gitOut := newCommandOutput(os.Stdout)
	registerBuiltinCommands(commands, ag, sess, bus, cfg, llmReg, extMgr, loader, mcpSetup, permSetup, telemetrySetup, trustSetup, profileSetup, skillSetup, promptSetup, checkpointMgr, gitOut, &quiet)

	// The prompt templates are slash commands like any other, so they
	// live on the same registry. They are installed once here; the TUI
	// redirects their output into its transcript afterwards.
	if promptSetup != nil {
		promptSetup.install(commands, ag)
		commands.Register("prompts", &ext.Command{
			Description: "show the prompt templates and the project context files",
			Handler: func(ctx context.Context, args string) error {
				promptSetup.report(promptSetup.out)
				return nil
			},
		})
	}

	// The skills are slash commands too: /skill:name sends the skill's
	// instructions to the model.
	if skillSetup != nil {
		skillSetup.install(commands, ag)
		commands.Register("skills", &ext.Command{
			Description: "show the available skills and where they came from",
			Handler: func(ctx context.Context, args string) error {
				skillSetup.report(skillSetup.out)
				return nil
			},
		})
	}

	if tuiMode {
		runTUIREPL(ctx, ag, bus, commands, cfg, permSetup, shellShortcut, promptSetup, skillSetup, gitOut, quiet)
		return
	}

	fmt.Printf("auto-harness %s — model %s (%s). /help for commands.\n", version, cfg.Model, cfg.Provider)
	// The REPL, the permission prompt and the trust prompt share one
	// reader over stdin, so none of them steals another's input. The
	// reader is created before the project loads, because the trust
	// prompt may already have consumed a line.
	if lines == nil {
		lines = perm.NewLineReader(os.Stdin)
	}
	permSetup.setLineReader(lines)
	scanner := perm.NewScanner(lines)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if err := handleCommand(ctx, commands, line, os.Stdout, os.Stderr); errors.Is(err, errExitREPL) {
				return
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		// The "!" shortcut runs a shell command without asking the
		// model.
		if handled, err := shellShortcut.Handle(ctx, line); handled {
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			continue
		}
		res, err := ag.Run(ctx, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		if res.Final != "" && !strings.HasSuffix(res.Final, "\n") {
			fmt.Println()
		}
	}
}

// handleCommand dispatches a slash line to the command registry.
func handleCommand(ctx context.Context, commands *ext.CommandRegistry, line string, out, errOut io.Writer) error {
	name, args := line, ""
	if i := strings.IndexAny(line, " \t"); i > 0 {
		name, args = line[:i], strings.TrimSpace(line[i+1:])
	}
	name = strings.TrimPrefix(name, "/")
	// Builtins take precedence over suffixed extension collisions.
	c, ok := commands.Get(name)
	if !ok {
		fmt.Fprintf(out, "unknown command /%s — try /help\n", name)
		return nil
	}
	if err := c.Handler(ctx, args); err != nil {
		if errors.Is(err, errExitREPL) {
			return errExitREPL
		}
		fmt.Fprintf(errOut, "/%s: %v\n", name, err)
	}
	return nil
}

// registerBuiltinCommands wires the REPL's own slash commands through
// the same registry extensions use.
func registerBuiltinCommands(commands *ext.CommandRegistry, ag *agent.Agent, sess *session.Session, bus *ext.Bus, cfg *config.Config, llmReg *llm.Registry, extMgr *ext.Manager, loader *extensionLoader, mcpSetup *mcpSetup, permSetup *permSetup, telemetrySetup *telemetrySetup, trustSetup *trustSetup, profileSetup *profileSetup, skillSetup *skillSetup, promptSetup *promptSetup, checkpointMgr *gitkit.Manager, gitOut io.Writer, quiet *bool) {
	registerTreeCommands(commands, &treeSession{sess: sess, ag: ag, dir: cfg.SessionDir, bus: bus})
	registerGitCommands(commands, checkpointMgr, gitOut)
	commands.Register("trust", &ext.Command{
		Description: "show or set the project trust decision",
		Handler: func(ctx context.Context, args string) error {
			if strings.TrimSpace(args) == "" {
				trustSetup.report(func(line string) { fmt.Println(line) })
				return nil
			}
			msg, err := trustSetup.apply(args)
			if err != nil {
				return err
			}
			fmt.Println(msg)
			return nil
		},
	})
	commands.Register("help", &ext.Command{
		Description: "list available commands",
		Handler: func(ctx context.Context, args string) error {
			names := commands.List()
			sort.Strings(names)
			for _, n := range names {
				if c, ok := commands.Get(n); ok {
					fmt.Printf("/%-14s %s\n", n, c.Description)
				}
			}
			return nil
		},
	})
	commands.Register("tools", &ext.Command{
		Description: "list registered tools",
		Handler: func(ctx context.Context, args string) error {
			for _, t := range ag.Tools.GetAllTools() {
				fmt.Printf("%-10s %s\n", t.Name, t.Description)
			}
			return nil
		},
	})
	commands.Register("profile", &ext.Command{
		Description: "show the active agent environment profile and its assembly",
		Handler: func(ctx context.Context, args string) error {
			provider := ""
			if ag.Provider() != nil {
				provider = ag.Provider().Name()
			}
			profileSetup.report(cfg, ag.Tools, loader, mcpSetup, skillSetup, promptSetup,
				ag.Config().Model, provider)
			return nil
		},
	})
	commands.Register("plugins", &ext.Command{
		Description: "list discovered plugins, versions and load state",
		Handler: func(ctx context.Context, args string) error {
			loader.report(extMgr)
			return nil
		},
	})
	commands.Register("model", &ext.Command{
		Description: "show or switch the model: /model <id> (cross-provider aware)",
		Handler: func(ctx context.Context, args string) error {
			if args == "" {
				fmt.Println(ag.Config().Model)
				return nil
			}
			// Validate the model resolves to a provider before switching.
			if _, err := llmReg.Resolve(args, ""); err != nil {
				return fmt.Errorf("%w (use /models to list known models)", err)
			}
			ag.SetModel(args)
			sess.SetModel(args)
			fmt.Println("model set to", args)
			return nil
		},
	})
	commands.Register("models", &ext.Command{
		Description: "list known models and their providers",
		Handler: func(ctx context.Context, args string) error {
			models := llmReg.Models()
			if len(models) == 0 {
				fmt.Println("no models registered")
				return nil
			}
			cur := ag.Config().Model
			for _, m := range models {
				mark := " "
				if m.ID == cur {
					mark = "*"
				}
				proto := m.Protocol
				if proto == "" {
					proto = "-"
				}
				fmt.Printf("%s %-30s %-12s %-10s %s\n", mark, m.ID, proto, m.Provider, m.Name)
			}
			return nil
		},
	})
	commands.Register("sessions", &ext.Command{
		Description: "list saved sessions",
		Handler: func(ctx context.Context, args string) error {
			list, err := session.List(cfg.SessionDir)
			if err != nil {
				return err
			}
			for _, info := range list {
				fmt.Printf("%s  %3d msgs  %s\n", info.ID, info.Messages, info.CreatedAt.Local().Format("2006-01-02 15:04:05"))
			}
			return nil
		},
	})
	commands.Register("compact", &ext.Command{
		Description: "compress the conversation history into a summary",
		Handler: func(ctx context.Context, args string) error {
			before := len(ag.History())
			if err := ag.Compact(ctx); err != nil {
				return err
			}
			fmt.Printf("compacted: %d -> %d messages\n", before, len(ag.History()))
			return nil
		},
	})
	commands.Register("context", &ext.Command{
		Description: "show the context budget, estimate and last automatic compaction",
		Handler: func(ctx context.Context, args string) error {
			status, ok := ag.ContextStatus()
			if !ok {
				fmt.Println("context manager: not configured")
				return nil
			}
			percent := 0.0
			if status.PromptBudget > 0 {
				percent = float64(status.EstimatedTokens) / float64(status.PromptBudget) * 100
			}
			fmt.Printf("model:               %s\n", status.Model)
			fmt.Printf("context window:      %d tokens\n", status.ContextWindow)
			fmt.Printf("output reservation:  %d tokens\n", status.MaxOutputTokens)
			fmt.Printf("prompt budget:       %d tokens (%d safety reserve)\n",
				status.PromptBudget, status.ReserveTokens)
			fmt.Printf("current estimate:    %d tokens (%.1f%% of budget)\n",
				status.EstimatedTokens, percent)
			fmt.Printf("automatic compact:   %.0f%% of budget (%d tokens)\n",
				status.CompactAt*100, status.CompactThresholdTokens)
			fmt.Printf("recent history:      %d tokens / at least %d units\n",
				status.KeepRecentTokens, status.KeepRecentMessages)
			fmt.Printf("summary completion:  %d tokens\n", status.SummaryMaxTokens)
			if status.ArtifactDir != "" {
				fmt.Printf("artifact directory:  %s\n", status.ArtifactDir)
			}
			if last := status.LastCompaction; last != nil {
				fmt.Printf("last auto compact:   %d -> %d tokens, %d artifact(s), summary %d bytes, %s\n",
					last.TokensBefore, last.TokensAfter, last.Artifacts,
					last.SummaryBytes, last.At.Local().Format("2006-01-02 15:04:05"))
			} else {
				fmt.Println("last auto compact:   none in this process")
			}
			return nil
		},
	})
	commands.Register("quiet", &ext.Command{
		Description: "toggle tool/hook notices",
		Handler: func(ctx context.Context, args string) error {
			*quiet = !*quiet
			fmt.Println("quiet:", *quiet)
			return nil
		},
	})
	commands.Register("reload", &ext.Command{
		Description: "reload extensions: /reload [extension-dir]",
		Handler: func(ctx context.Context, args string) error {
			// An explicit directory overrides the configured ones for
			// this reload only.
			if strings.TrimSpace(args) != "" {
				loader.cfg.ExtensionDirs = append([]string{args}, loader.cfg.ExtensionDirs...)
			}
			before := len(extMgr.LoadedNames())
			extensions := loader.build()
			if err := loader.missingPluginError(); err != nil {
				return err
			}
			if err := extMgr.Reload(extensions...); err != nil {
				return err
			}
			if err := loader.selectedPluginError(extMgr); err != nil {
				return err
			}
			after := extMgr.LoadedNames()
			fmt.Printf("reloaded %d extensions: %s\n", len(after), strings.Join(after, ", "))
			if len(after) != before {
				fmt.Printf("(was %d)\n", before)
			}
			return nil
		},
	})
	commands.Register("extensions", &ext.Command{
		Description: "list loaded extensions",
		Handler: func(ctx context.Context, args string) error {
			names := extMgr.LoadedNames()
			if len(names) == 0 {
				fmt.Println("no extensions loaded")
				return nil
			}
			for _, n := range names {
				fmt.Println("•", n)
			}
			return nil
		},
	})
	commands.Register("mcp", &ext.Command{
		Description: "inspect MCP servers: /mcp | /mcp add <name> ... | /mcp remove <name>",
		Handler: func(ctx context.Context, args string) error {
			fields := strings.Fields(args)
			if len(fields) == 0 {
				mcpSetup.report()
				return nil
			}
			switch fields[0] {
			case "add":
				name, server, err := parseServerFlags(fields[1:])
				if err != nil {
					return err
				}
				if err := mcpSetup.addCommand(name, server); err != nil {
					return err
				}
				fmt.Printf("added MCP server %q to %s\n", name, mcpSetup.userPath)
				fmt.Println("run /reload to connect it in this session")
				return nil
			case "remove":
				if len(fields) < 2 {
					return fmt.Errorf("usage: /mcp remove <name>")
				}
				if err := mcpSetup.removeCommand(fields[1]); err != nil {
					return err
				}
				fmt.Printf("removed MCP server %q\n", fields[1])
				fmt.Println("run /reload to apply the change")
				return nil
			case "list":
				mcpSetup.listCommand()
				return nil
			default:
				return fmt.Errorf("usage: /mcp | /mcp add <name> [--url <url>] [--env K=V] -- <command> | /mcp remove <name>")
			}
		},
	})
	commands.Register("permissions", &ext.Command{
		Description: "show the permission rules: /permissions [allow <path>]",
		Handler: func(ctx context.Context, args string) error {
			fields := strings.Fields(args)
			if len(fields) == 0 {
				permSetup.report()
				return nil
			}
			switch fields[0] {
			case "allow":
				if len(fields) < 2 {
					return fmt.Errorf("usage: /permissions allow <path>")
				}
				return permSetup.allowPath(expandHomePath(fields[1]))
			default:
				return fmt.Errorf("usage: /permissions | /permissions allow <path>")
			}
		},
	})
	commands.Register("telemetry", &ext.Command{
		Description: "show the telemetry state: /telemetry [on|off|path|endpoint|prometheus]",
		Handler: func(ctx context.Context, args string) error {
			fields := strings.Fields(args)
			if len(fields) == 0 {
				telemetrySetup.report()
				return nil
			}
			switch fields[0] {
			case "on":
				cfg.Telemetry = true
				fmt.Println("telemetry on (takes effect on the next run)")
				return nil
			case "off":
				cfg.Telemetry = false
				fmt.Println("telemetry off (takes effect on the next run)")
				return nil
			case "path":
				fmt.Println(telemetrySetup.path)
				return nil
			case "endpoint":
				fmt.Println(telemetrySetup.endpoint)
				return nil
			case "prometheus":
				fmt.Println(telemetrySetup.prometheusEndpoint)
				return nil
			default:
				return fmt.Errorf("usage: /telemetry | /telemetry on | /telemetry off | /telemetry path | /telemetry endpoint | /telemetry prometheus")
			}
		},
	})
	commands.Register("exit", &ext.Command{
		Description: "quit the REPL",
		Handler: func(ctx context.Context, args string) error {
			return errExitREPL
		},
	})
}

func compactArgs(args []byte) string {
	s := string(args)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}

// failStartup releases resources that would normally be closed by
// deferred calls. os.Exit skips those defers, so every startup failure
// after a resource has started must name its cleanup explicitly.
func failStartup(err error, cleanups ...func()) {
	for _, cleanup := range cleanups {
		if cleanup != nil {
			cleanup()
		}
	}
	fatal(err)
}

// exitStatus is the non-error counterpart used by inspection commands
// that need a non-zero exit code after printing a report.
func exitStatus(status int, cleanups ...func()) {
	for _, cleanup := range cleanups {
		if cleanup != nil {
			cleanup()
		}
	}
	os.Exit(status)
}
