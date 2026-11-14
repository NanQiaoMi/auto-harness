// Command harness is the auto-harness CLI: a pi-style coding agent
// harness with a streaming REPL, slash commands and one-shot mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/perm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/shellext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

const version = "0.1.1"

func main() {
	var (
		modelFlag    = flag.String("model", "", "model ID (default: config or claude-sonnet-5-5)")
		providerFlag = flag.String("provider", "", "provider: openai | anthropic (default: inferred from model)")
		baseURLFlag  = flag.String("base-url", "", "provider base URL override")
		systemFlag   = flag.String("system", "", "override the system prompt")
		maxTurns     = flag.Int("max-turns", 0, "cap on agent loop iterations (default 25)")
		maxTokens    = flag.Int("max-tokens", 0, "cap on tokens per completion (provider default)")
		sessionFlag  = flag.String("session", "", "resume session ID")
		listSessions = flag.Bool("list-sessions", false, "list saved sessions and exit")
		printMode    = flag.Bool("print", false, "one-shot mode: run the given prompt and exit")
		showVersion  = flag.Bool("version", false, "print version and exit")
		quiet        = flag.Bool("quiet", false, "suppress tool/hook notices (model text only)")
		showThinking = flag.Bool("show-thinking", false, "stream model thinking output to stderr")
		tuiMode      = flag.Bool("tui", false, "use the differential-rendering TUI for the REPL")
		rpcMode      = flag.Bool("rpc", false, "run in RPC mode: JSON commands on stdin, records on stdout")
		modeFlag     = flag.String("mode", "", "output mode: json (one-shot event stream on stdout)")
		noMCP        = flag.Bool("no-mcp", false, "disable MCP servers for this run")
		listMCP      = flag.Bool("mcp-list", false, "list configured MCP servers and exit")
	)
	flag.Parse()

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

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
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

	// --- permissions and sandbox ----------------------------------
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
	permSetup := loadPermissions(ctx, cfg, workdir(), registry, interactive)

	// The bash tool runs inside the sandbox when one is configured.
	bashTool := tools.NewBashTool()
	if runner := permSetup.sandboxRunner(); runner != nil {
		bashTool = tools.NewSandboxedBashTool(runner)
	}
	for _, t := range []*tools.Tool{bashTool, tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool()} {
		if err := registry.Register(t); err != nil {
			fatal(err)
		}
	}

	extMgr := ext.NewManager(api)
	loader := &extensionLoader{projectRoot: workdir(), cfg: cfg}
	if err := extMgr.LoadAll(loader.build()...); err != nil {
		// A broken extension must not stop the harness from starting.
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}
	// The permission guard joins the extension set, so it survives a
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
	mcpSetup := loadMCPServers(ctx, cfg, workdir(), registry)
	defer mcpSetup.manager.Close()
	for name, err := range mcpSetup.errors {
		fmt.Fprintf(os.Stderr, "[warn] mcp %s: %v\n", name, err)
	}

	// -mcp-list connects to every server, prints the state and exits.
	if *listMCP {
		mcpSetup.listCommand()
		if len(mcpSetup.errors) > 0 {
			os.Exit(1)
		}
		return
	}

	// --- prompt templates, project context and skills ---------------
	// The templates are markdown files that become slash commands; the
	// context files are the project's instructions; the skills are the
	// instructions the model loads when a task matches. All three are
	// optional and all three are data, not code.
	promptSetup := loadPrompts(cfg, workdir())
	skillSetup := loadSkills(cfg, workdir())

	ag := agent.New(agent.Config{
		Provider: provider,
		Model:    cfg.Model,
		// The project context files fold into the system prompt, and
		// the skills are advertised in it.
		SystemPrompt: skillSetup.systemPrompt(promptSetup.systemPrompt(cfg.SystemPrompt)),
		MaxTokens:    cfg.MaxTokens,
		MaxTurns:     cfg.MaxTurns,
		// Resolve the provider per model so /model can cross providers.
		Resolver: func(modelID string) (llm.Provider, error) {
			return llmReg.ProviderFor(modelID, "")
		},
	}, bus, registry)
	ag.Session = sess
	ag.Compactor = agent.NewSummarizer(provider, cfg.Model)
	ag.Logf = func(format string, args ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[hook] "+format+"\n", args...)
		}
	}
	ag.SetHistory(sess.Messages)

	// The "!" shortcut runs the user's own shell commands, through the
	// same bash tool, so the sandbox applies to them too.
	shellShortcut := &shellext.Shortcut{
		Runner:       &shellext.ToolRunner{Tool: bashTool},
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
			fatal(fmt.Errorf("--mode json requires a prompt argument"))
		}
		// A skill command is expanded here, the way the print mode
		// does it: the REPL dispatches it through the registry.
		if expanded, ok := skillSetup.expand(prompt); ok {
			prompt = expanded
		}
		runJSONMode(ctx, ag, sess, bus, prompt, workdir())
		return
	}

	if *printMode {
		prompt := strings.Join(flag.Args(), " ")
		if strings.TrimSpace(prompt) == "" {
			fatal(fmt.Errorf("-print requires a prompt argument"))
		}
		// A skill command in one-shot mode is expanded here: the REPL
		// dispatches it through the command registry instead.
		if expanded, ok := skillSetup.expand(prompt); ok {
			prompt = expanded
		}
		res, err := ag.Run(ctx, prompt)
		if err != nil {
			fatal(err)
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
		runRPCMode(ctx, ag, sess, bus, commands, cfg, llmReg, extMgr, loader, mcpSetup, permSetup, telemetrySetup, shellShortcut, promptSetup, skillSetup, *quiet)
		return
	}

	runREPL(ctx, ag, sess, bus, commands, cfg, llmReg, extMgr, loader, mcpSetup, permSetup, telemetrySetup, shellShortcut, promptSetup, skillSetup, *quiet, *tuiMode)
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
func runREPL(ctx context.Context, ag *agent.Agent, sess *session.Session, bus *ext.Bus, commands *ext.CommandRegistry, cfg *config.Config, llmReg *llm.Registry, extMgr *ext.Manager, loader *extensionLoader, mcpSetup *mcpSetup, permSetup *permSetup, telemetrySetup *telemetrySetup, shellShortcut *shellext.Shortcut, promptSetup *promptSetup, skillSetup *skillSetup, quiet bool, tuiMode bool) {
	registerBuiltinCommands(commands, ag, sess, bus, cfg, llmReg, extMgr, loader, mcpSetup, permSetup, telemetrySetup, &quiet)

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
		runTUIREPL(ctx, ag, bus, commands, cfg, permSetup, shellShortcut, promptSetup, skillSetup, quiet)
		return
	}

	fmt.Printf("auto-harness %s — model %s (%s). /help for commands.\n", version, cfg.Model, cfg.Provider)
	// The REPL and the permission prompt share one reader over stdin, so
	// neither steals the other's input.
	lines := perm.NewLineReader(os.Stdin)
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
			handleCommand(ctx, commands, line)
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
func handleCommand(ctx context.Context, commands *ext.CommandRegistry, line string) {
	name, args := line, ""
	if i := strings.IndexAny(line, " \t"); i > 0 {
		name, args = line[:i], strings.TrimSpace(line[i+1:])
	}
	name = strings.TrimPrefix(name, "/")
	// Builtins take precedence over suffixed extension collisions.
	c, ok := commands.Get(name)
	if !ok {
		fmt.Printf("unknown command /%s — try /help\n", name)
		return
	}
	if err := c.Handler(ctx, args); err != nil {
		fmt.Fprintf(os.Stderr, "/%s: %v\n", name, err)
	}
}

// registerBuiltinCommands wires the REPL's own slash commands through
// the same registry extensions use.
func registerBuiltinCommands(commands *ext.CommandRegistry, ag *agent.Agent, sess *session.Session, bus *ext.Bus, cfg *config.Config, llmReg *llm.Registry, extMgr *ext.Manager, loader *extensionLoader, mcpSetup *mcpSetup, permSetup *permSetup, telemetrySetup *telemetrySetup, quiet *bool) {
	registerTreeCommands(commands, &treeSession{sess: sess, ag: ag, dir: cfg.SessionDir})
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
			if err := extMgr.Reload(loader.build()...); err != nil {
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
			os.Exit(0)
			return nil
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
