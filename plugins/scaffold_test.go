package plugins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestScaffoldValidation(t *testing.T) {
	dir := t.TempDir()

	for _, badName := range []string{"", "  ", "../bad", "bad name", ".hidden", strings.Repeat("a", 129)} {
		if _, err := Scaffold(ScaffoldOptions{
			Name: badName,
			Dir:  filepath.Join(dir, "out"),
		}); err == nil {
			t.Errorf("Scaffold(name=%q) succeeded, want error", badName)
		}
	}

	if _, err := Scaffold(ScaffoldOptions{
		Name: "valid-plugin",
		Lang: "rust",
		Dir:  filepath.Join(dir, "rust-plugin"),
	}); err == nil {
		t.Fatal("Scaffold(lang=rust) succeeded, want error")
	}

	nonEmpty := filepath.Join(dir, "non-empty")
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(ScaffoldOptions{
		Name: "valid-plugin",
		Dir:  nonEmpty,
	}); err == nil {
		t.Fatal("Scaffold over non-empty directory succeeded, want error")
	}

	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(ScaffoldOptions{
		Name: "valid-plugin",
		Dir:  filePath,
	}); err == nil {
		t.Fatal("Scaffold over regular file succeeded, want error")
	}
}

func TestScaffoldDefaultsAndLoad(t *testing.T) {
	cases := []struct {
		name     string
		langIn   string
		wantLang string
		runtime  string
	}{
		{name: "py-demo.v1", langIn: "", wantLang: LangPython, runtime: "python3"},
		{name: "py-alias", langIn: "PY", wantLang: LangPython, runtime: "python3"},
		{name: "ts-demo", langIn: "ts", wantLang: LangTypeScript, runtime: "node"},
		{name: "go-demo", langIn: "go", wantLang: LangGo, runtime: "go"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), tc.name)
			res, err := Scaffold(ScaffoldOptions{
				Name: tc.name,
				Lang: tc.langIn,
				Dir:  target,
			})
			if err != nil {
				t.Fatalf("Scaffold: %v", err)
			}
			if res.Lang != tc.wantLang {
				t.Fatalf("Lang = %q, want %q", res.Lang, tc.wantLang)
			}
			if len(res.Files) == 0 {
				t.Fatal("Files is empty")
			}

			cand, err := Load(target)
			if err != nil {
				t.Fatalf("Load(%s): %v", target, err)
			}
			if cand.Manifest.Name != tc.name {
				t.Errorf("Manifest.Name = %q, want %q", cand.Manifest.Name, tc.name)
			}
			if cand.Manifest.Sandbox == nil {
				t.Fatal("Manifest.Sandbox is nil, want default read-only sandbox")
			}
			if cand.Manifest.Sandbox.WritePaths == nil || len(cand.Manifest.Sandbox.WritePaths) != 0 {
				t.Errorf("WritePaths = %#v, want non-nil empty slice []", cand.Manifest.Sandbox.WritePaths)
			}
			if cand.Manifest.Sandbox.AllowNetwork == nil || *cand.Manifest.Sandbox.AllowNetwork {
				t.Errorf("AllowNetwork = %#v, want false", cand.Manifest.Sandbox.AllowNetwork)
			}
			prof, err := cand.SandboxProfile(true)
			if err != nil {
				t.Fatalf("SandboxProfile: %v", err)
			}
			if prof.AllowNetwork || len(prof.WritePaths) != 0 {
				t.Errorf("SandboxProfile = %#v, want read-only no-network", prof)
			}

			if _, err := exec.LookPath(tc.runtime); err != nil {
				t.Skipf("%s not in PATH, skipping live handshake test", tc.runtime)
			}

			pe, err := cand.Extension(nil)
			if err != nil {
				t.Fatalf("Extension: %v", err)
			}
			reg := tools.NewRegistry()
			api := ext.NewAPI(ext.NewBus(), reg, ext.NewCommandRegistry())
			if err := pe.Setup(api); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			defer pe.Stop()

			toolName := scaffoldToolName(tc.name)
			tool, ok := reg.Get(toolName)
			if !ok {
				t.Fatalf("expected tool %q to be registered", toolName)
			}
			out, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"hello"}`), nil)
			if err != nil {
				t.Fatalf("tool.Execute: %v", err)
			}
			if out.IsError {
				t.Fatalf("tool returned error: %s", out.Content)
			}
			want := tc.name + ": hello"
			if out.Content != want {
				t.Errorf("tool output = %q, want %q", out.Content, want)
			}
		})
	}
}
