package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/contextmgr"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/orchestrator"
	"github.com/NanQiaoMi/auto-harness/sandbox"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// subagentRuntime builds a fresh agent for each delegated task. The
// parent agent and each child share the extension bus (so permissions
// and telemetry still apply) but not sessions, context managers or
// workspace-bound tools.
type subagentRuntime struct {
	parent             *agent.Agent
	cfg                *config.Config
	bus                *ext.Bus
	registry           *tools.Registry
	llmReg             *llm.Registry
	contextOpts        contextmgr.Options
	checkpointLimit    int
	checkpointsEnabled bool
	maxDepth           int
	parentWorkdir      string
	storeDir           string
	sessionDir         string
	quiet              bool
	parentSessionPath  pathRef
}

type pathRef struct {
	mu    sync.RWMutex
	value string
}

func (p *pathRef) set(value string) {
	p.mu.Lock()
	p.value = value
	p.mu.Unlock()
}

func (p *pathRef) get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.value
}

// factory is the orchestrator worker factory for the real harness.
func (r *subagentRuntime) factory(ctx context.Context, task orchestrator.Task) (orchestrator.Prepared, error) {
	workspace, isolation, err := r.workspaceFor(ctx, task)
	if err != nil {
		return orchestrator.Prepared{}, err
	}
	cleanupWorkspace := isolation == orchestrator.IsolationWorktree
	defer func() {
		if cleanupWorkspace {
			_ = gitkit.RemoveWorktree(context.Background(), r.parentWorkdir, workspace, true)
		}
	}()
	model := strings.TrimSpace(task.Model)
	if model == "" {
		model = r.parent.Config().Model
	}
	sess, err := session.NewChild(r.sessionDir, model, r.parentSessionPath.get(),
		"subagent: "+task.Name)
	if err != nil {
		return orchestrator.Prepared{}, err
	}
	cleanupSession := true
	defer func() {
		if cleanupSession {
			_ = sess.Close()
		}
	}()

	checkpointMgr := gitkit.NewCheckpointManager(workspace)
	checkpointMgr.Limit = r.checkpointLimit
	checkpointMgr.BindSession = func(kind gitkit.CheckpointKind, tool string) gitkit.SessionBinding {
		return checkpointSessionBinding(sess, kind, tool)
	}

	gitDirs, _ := gitkit.GitDirs(ctx, workspace)
	allowNetwork := true
	if r.cfg.Sandbox {
		allowNetwork = r.cfg.SandboxAllowNetwork
	}
	childRegistry := r.registry.Clone()
	if err := replaceAgentTools(childRegistry, workspace, checkpointMgr, gitDirs, allowNetwork); err != nil {
		return orchestrator.Prepared{}, err
	}
	if task.Depth >= r.maxDepth {
		names := make([]string, 0)
		for _, tool := range childRegistry.GetActiveTools() {
			if tool.Name != "agent" {
				names = append(names, tool.Name)
			}
		}
		if err := childRegistry.SetActiveToolsExact(names); err != nil {
			return orchestrator.Prepared{}, err
		}
	}

	parentCfg := r.parent.Config()
	systemPrompt := parentCfg.SystemPrompt + subagentPrompt(task, workspace, isolation)
	ag := agent.New(agent.Config{
		Model:         model,
		SystemPrompt:  systemPrompt,
		AgentID:       task.ID,
		ParentAgentID: task.ParentID,
		Workspace:     workspace,
		MaxTokens:     parentCfg.MaxTokens,
		ContextWindow: r.contextOpts.ContextWindow,
		MaxTurns:      parentCfg.MaxTurns,
		Temperature:   parentCfg.Temperature,
		Thinking:      parentCfg.Thinking,
		ContextBudget: func(modelID string) (int, int) {
			if info, ok := r.llmReg.Model(modelID); ok {
				return info.ContextWindow, info.MaxOutput
			}
			return 0, 0
		},
		Resolver: func(modelID string) (llm.Provider, error) {
			return r.llmReg.ProviderFor(modelID, "")
		},
	}, r.bus, childRegistry)
	ag.Session = sess
	if r.checkpointsEnabled {
		ag.Checkpointer = checkpointMgr
	}
	ag.Compactor = func(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
		current, err := r.llmReg.ProviderFor(model, "")
		if err != nil {
			return nil, err
		}
		return agent.NewSummarizer(current, model)(ctx, messages)
	}
	ag.Logf = func(format string, args ...any) {
		if !r.quiet {
			fmt.Fprintf(os.Stderr, "[subagent %s] "+format+"\n",
				append([]any{task.ID}, args...)...)
		}
	}
	ag.SetHistory(sess.Messages)

	contextMgr := contextmgr.NewManager(
		r.contextOpts,
		nil,
		contextmgr.SummarizerFunc(func(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
			p, err := r.llmReg.ProviderFor(model, "")
			if err != nil {
				return "", err
			}
			return contextmgr.SummarizeOnce(ctx, p, model, messages, maxTokens)
		}),
		contextmgr.NewFileArtifactStore(r.artifactDir(sess)),
	)
	ag.SetContextManager(contextMgr)

	if r.checkpointsEnabled {
		if _, created, err := checkpointMgr.Create(ctx, "subagent start",
			gitkit.CheckpointManual, "subagent"); err != nil {
			if !errors.Is(err, gitkit.ErrNotRepository) {
				return orchestrator.Prepared{}, fmt.Errorf("subagent checkpoint: %w", err)
			}
		} else if created && !r.quiet {
			fmt.Fprintf(os.Stderr, "[subagent %s] workspace checkpoint created in %s\n",
				task.ID, workspace)
		}
	}

	cleanupSession = false
	cleanupWorkspace = false
	return orchestrator.Prepared{
		Worker: &delegatedWorker{
			agent: ag,
			sess:  sess,
		},
		SessionID:   sess.Meta.ID,
		SessionPath: sess.Path,
		Workspace:   workspace,
		Isolation:   isolation,
	}, nil
}

func (r *subagentRuntime) workspaceFor(ctx context.Context, task orchestrator.Task) (string, orchestrator.Isolation, error) {
	if task.Isolation == orchestrator.IsolationShared {
		return r.parentWorkdir, orchestrator.IsolationShared, nil
	}
	root, err := gitkit.Repository(ctx, r.parentWorkdir)
	if err != nil {
		if errors.Is(err, gitkit.ErrNotRepository) {
			if task.Isolation == orchestrator.IsolationWorktree {
				return "", "", fmt.Errorf("worktree isolation requires a Git repository")
			}
			return r.parentWorkdir, orchestrator.IsolationShared, nil
		}
		return "", "", err
	}
	path := filepath.Join(r.storeDir, "workspaces", task.ID)
	worktree, err := gitkit.AddDetachedWorktree(ctx, root, path)
	if err != nil {
		return "", "", fmt.Errorf("create isolated worktree: %w", err)
	}
	return worktree, orchestrator.IsolationWorktree, nil
}

func replaceAgentTools(registry *tools.Registry, workspace string, checkpointMgr *gitkit.Manager,
	gitDirs []string, allowNetwork bool) error {
	var bash *tools.Tool
	if runner := scopedSandboxRunner(workspace, gitDirs, allowNetwork); runner != nil {
		bash = tools.NewSandboxedBashToolAt(workspace, runner)
	} else {
		bash = tools.NewBashToolAt(workspace)
	}
	for _, tool := range []*tools.Tool{
		bash,
		tools.NewReadToolAt(workspace),
		tools.NewWriteToolAt(workspace),
		tools.NewEditToolAt(workspace),
		gitkit.NewGitTool(workspace),
		gitkit.NewCheckpointTool(checkpointMgr),
	} {
		if _, ok := registry.Get(tool.Name); !ok {
			continue
		}
		if err := registry.Replace(tool); err != nil {
			return err
		}
	}
	return nil
}

func scopedSandboxRunner(workspace string, gitDirs []string, allowNetwork bool) tools.SandboxRunner {
	if !sandbox.Available() {
		return nil
	}
	profile := sandbox.DefaultProfile(workspace)
	profile.WritePaths = append(profile.WritePaths, gitDirs...)
	profile.AllowNetwork = allowNetwork
	return func(ctx context.Context, command, workdir string) (string, bool, error) {
		return sandbox.Exec(ctx, command, profile, workdir)
	}
}

func (r *subagentRuntime) artifactDir(sess *session.Session) string {
	base := ""
	if r.cfg.Context != nil {
		base = strings.TrimSpace(r.cfg.Context.ArtifactDir)
	}
	if base == "" {
		base = filepath.Join(filepath.Dir(sess.Path), "artifacts")
	}
	return filepath.Join(base, sess.Meta.ID)
}

func subagentPrompt(task orchestrator.Task, workspace string, isolation orchestrator.Isolation) string {
	var b strings.Builder
	b.WriteString("\n\n## Delegated subagent\n")
	fmt.Fprintf(&b, "You are delegated task %s", task.ID)
	if task.ParentID != "" {
		fmt.Fprintf(&b, " under parent task %s", task.ParentID)
	}
	b.WriteString(". Work independently and return a concise final report: what changed, what you verified, and any remaining risk.\n")
	fmt.Fprintf(&b, "Workspace: %s\n", workspace)
	if isolation == orchestrator.IsolationShared {
		b.WriteString("Isolation: shared parent workspace. Be especially careful not to overwrite unrelated user changes.\n")
	} else {
		b.WriteString("Isolation: linked Git worktree. Edit only inside this workspace; the parent workspace is read-only to you.\n")
	}
	b.WriteString("Use workspace-relative paths. Do not ask the parent for information that can be discovered from the workspace.\n")
	return b.String()
}

type delegatedWorker struct {
	agent *agent.Agent
	sess  *session.Session
}

func (w *delegatedWorker) Run(ctx context.Context, prompt string) (orchestrator.Outcome, error) {
	result, err := w.agent.Run(ctx, prompt)
	if result == nil {
		return orchestrator.Outcome{}, err
	}
	outcome := orchestrator.Outcome{
		Turns: result.Turns,
		Usage: result.Usage,
	}
	outcome.Summary = strings.TrimSpace(result.Final)
	if outcome.Summary == "" {
		outcome.Summary = fmt.Sprintf(
			"Delegated agent finished after %d turn(s) without a final text report.", result.Turns)
	}
	return outcome, err
}

func (w *delegatedWorker) Close() error {
	if w == nil || w.sess == nil {
		return nil
	}
	return w.sess.Close()
}
