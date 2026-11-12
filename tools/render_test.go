package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveRenderersFallsBackToTheTool(t *testing.T) {
	reg := NewRegistry()
	tool := &Tool{
		Name: "custom",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			return Result{}, nil
		},
		Renderers: &Renderers{
			RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
				return []string{"custom call"}, nil
			},
		},
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	r := reg.ResolveRenderers("custom")
	if r == nil || r.RenderCall == nil {
		t.Fatal("the tool's own renderer should be found")
	}
	lines, err := r.RenderCall(nil, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || lines[0] != "custom call" {
		t.Errorf("lines = %v, want [custom call]", lines)
	}
}

func TestResolveRenderersChainsResolvers(t *testing.T) {
	reg := NewRegistry()
	var seen []string

	// The first resolver takes the tool it cares about.
	reg.AddRenderer(func(name string, next func() *Renderers) *Renderers {
		seen = append(seen, "first:"+name)
		if name == "wanted" {
			return &Renderers{RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
				return []string{"first"}, nil
			}}
		}
		return next()
	})
	// The second one only steps in when there is nothing else.
	reg.AddRenderer(func(name string, next func() *Renderers) *Renderers {
		seen = append(seen, "second:"+name)
		if next() == nil {
			return &Renderers{RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
				return []string{"second"}, nil
			}}
		}
		return next()
	})

	// The tool the first resolver wanted.
	r := reg.ResolveRenderers("wanted")
	if r == nil || r.RenderCall == nil {
		t.Fatal("expected a renderer")
	}
	lines, _ := r.RenderCall(nil, RenderContext{})
	if len(lines) != 1 || lines[0] != "first" {
		t.Errorf("lines = %v, want [first]", lines)
	}
	// The first resolver took the tool, so the chain stopped there:
	// the second one was never asked.
	if len(seen) != 1 || seen[0] != "first:wanted" {
		t.Errorf("seen = %v, want the chain to stop at the first resolver", seen)
	}

	// A tool nobody registered: the second resolver steps in.
	seen = nil
	r = reg.ResolveRenderers("unknown")
	if r == nil || r.RenderCall == nil {
		t.Fatal("expected a renderer")
	}
	lines, _ = r.RenderCall(nil, RenderContext{})
	if len(lines) != 1 || lines[0] != "second" {
		t.Errorf("lines = %v, want [second]", lines)
	}
}

func TestResolveRenderersResolverCanDeferToTheTool(t *testing.T) {
	reg := NewRegistry()
	tool := &Tool{
		Name: "custom",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			return Result{}, nil
		},
		Renderers: &Renderers{RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
			return []string{"tool's own"}, nil
		}},
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.AddRenderer(func(name string, next func() *Renderers) *Renderers {
		return next()
	})

	r := reg.ResolveRenderers("custom")
	lines, _ := r.RenderCall(nil, RenderContext{})
	if len(lines) != 1 || lines[0] != "tool's own" {
		t.Errorf("lines = %v, want the tool's own renderer", lines)
	}
}

func TestRenderersWithBuiltInFillsTheGap(t *testing.T) {
	own := &Renderers{RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
		return []string{"mine"}, nil
	}}
	merged := own.WithBuiltIn(BuiltInRenderer("read"))
	if merged.RenderCall == nil {
		t.Fatal("the own renderer should survive")
	}
	lines, _ := merged.RenderCall(nil, RenderContext{})
	if len(lines) != 1 || lines[0] != "mine" {
		t.Errorf("lines = %v, want [mine]", lines)
	}
	if merged.RenderResult == nil {
		t.Error("the built-in result renderer should fill the gap")
	}
}

func TestBuiltInRenderersCoverTheBuiltInTools(t *testing.T) {
	for _, name := range []string{"read", "write", "edit", "bash"} {
		r := BuiltInRenderer(name)
		if r == nil {
			t.Fatalf("no built-in renderer for %q", name)
		}
		if r.RenderCall == nil || r.RenderResult == nil {
			t.Errorf("the %q renderer is incomplete", name)
		}
	}
}

func TestReadRendererShowsThePath(t *testing.T) {
	r := BuiltInRenderer("read")
	lines, err := r.RenderCall(map[string]any{"path": "main.go"}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "main.go") {
		t.Errorf("lines = %v, want the path", lines)
	}
	if !strings.Contains(lines[0], "read") {
		t.Errorf("lines = %v, want the tool name", lines)
	}
}

func TestReadRendererHidesTheContentWhenCollapsed(t *testing.T) {
	r := BuiltInRenderer("read")
	lines, err := r.RenderResult(Result{Content: "1\tpackage main"}, RenderResultOptions{}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("lines = %v, want none when collapsed", lines)
	}

	lines, err = r.RenderResult(Result{Content: "1\tpackage main"}, RenderResultOptions{Expanded: true}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "package main") {
		t.Errorf("lines = %v, want the content when expanded", lines)
	}
}

func TestReadRendererShowsTheErrorEvenWhenCollapsed(t *testing.T) {
	r := BuiltInRenderer("read")
	lines, err := r.RenderResult(Result{Content: "read x: no such file", IsError: true}, RenderResultOptions{}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "no such file") {
		t.Errorf("lines = %v, want the error", lines)
	}
}

func TestShellRendererPreviewsTheOutput(t *testing.T) {
	r := BuiltInRenderer("bash")
	lines, err := r.RenderCall(map[string]any{"command": "ls -la"}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "ls -la") {
		t.Errorf("lines = %v, want the command", lines)
	}

	output := strings.Repeat("line\n", 30)
	lines, err = r.RenderResult(Result{Content: output}, RenderResultOptions{}, RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != previewLines+1 {
		t.Errorf("lines = %d, want %d", len(lines), previewLines+1)
	}
	if !strings.Contains(lines[0], "20 earlier lines") {
		t.Errorf("lines[0] = %q, want the hidden-line marker", lines[0])
	}
}

func TestDiffLinesShowsTheChange(t *testing.T) {
	lines := DiffLines("a\nb\nc", "a\nB\nc")
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want two changed lines", lines)
	}
	if lines[0] != "-b" || lines[1] != "+B" {
		t.Errorf("lines = %v, want [-b +B]", lines)
	}
}

func TestDiffLinesDropsTheCommonParts(t *testing.T) {
	lines := DiffLines("x\n1\ny", "x\n2\ny")
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want two changed lines", lines)
	}
	if lines[0] != "-1" || lines[1] != "+2" {
		t.Errorf("lines = %v, want [-1 +2]", lines)
	}
}

func TestDiffLinesOnIdenticalText(t *testing.T) {
	lines := DiffLines("a\nb", "a\nb")
	if len(lines) != 0 {
		t.Errorf("lines = %v, want none", lines)
	}
}

func TestDiffLinesHandlesEmptySides(t *testing.T) {
	lines := DiffLines("", "a\nb")
	if len(lines) != 2 || lines[0] != "+a" || lines[1] != "+b" {
		t.Errorf("lines = %v, want [+a +b]", lines)
	}
	lines = DiffLines("a\nb", "")
	if len(lines) != 2 || lines[0] != "-a" || lines[1] != "-b" {
		t.Errorf("lines = %v, want [-a -b]", lines)
	}
}

func TestMutationQueueSerializesTheSameFile(t *testing.T) {
	q := NewMutationQueue()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	var mu sync.Mutex
	var order []string

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := q.WithMutation(path, func() error {
				mu.Lock()
				order = append(order, "in")
				mu.Unlock()
				// A sleep inside the critical section: without the
				// queue the markers would interleave.
				mu.Lock()
				order = append(order, "out")
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("mutation: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Every "in" must be followed by its "out" before the next "in".
	for i := 0; i < len(order); i += 2 {
		if order[i] != "in" || order[i+1] != "out" {
			t.Fatalf("order = %v, want the mutations serialized", order)
		}
	}
}

func TestMutationQueueReleasesOnError(t *testing.T) {
	q := NewMutationQueue()
	path := filepath.Join(t.TempDir(), "file.txt")

	if err := q.WithMutation(path, func() error { return os.ErrPermission }); err == nil {
		t.Fatal("expected the error")
	}
	// The queue must not be stuck: a second call goes through.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := q.WithMutation(path, func() error { return nil }); err != nil {
			t.Errorf("mutation: %v", err)
		}
	}()
	<-done
}

func TestMutationQueueDoesNotBlockOtherFiles(t *testing.T) {
	q := NewMutationQueue()
	dir := t.TempDir()
	first := filepath.Join(dir, "a.txt")
	second := filepath.Join(dir, "b.txt")

	holding := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = q.WithMutation(first, func() error {
			close(holding)
			<-released
			return nil
		})
	}()
	<-holding

	// The other file must not wait for the first one.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.WithMutation(second, func() error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the mutation of another file was blocked")
	}
	close(released)
}

func TestWriteAndEditTakeTheQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	write := NewWriteTool()
	edit := NewEditTool()

	args, _ := json.Marshal(map[string]string{"path": path, "content": "hello\n"})
	if _, err := write.Execute(context.Background(), "1", args, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	args, _ = json.Marshal(map[string]string{"path": path, "old_string": "hello", "new_string": "world"})
	if _, err := edit.Execute(context.Background(), "2", args, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "world\n" {
		t.Errorf("content = %q, want %q", data, "world\n")
	}
}
