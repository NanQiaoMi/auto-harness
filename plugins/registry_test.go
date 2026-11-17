package plugins

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogResolvesHighestCompatibleSet(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "base-1.5.0.tar.gz"), []byte("base15"))
	writeTestFile(t, filepath.Join(dir, "base-2.0.0.tar.gz"), []byte("base20"))
	writeTestFile(t, filepath.Join(dir, "app-1.0.0.tar.gz"), []byte("app10"))
	writeTestFile(t, filepath.Join(dir, "app-2.0.0.tar.gz"), []byte("app20"))
	index := map[string]any{
		"schema_version": 1,
		"plugins": map[string]any{
			"base": map[string]any{
				"versions": map[string]any{
					"1.5.0": map[string]any{
						"artifact": map[string]any{"url": "base-1.5.0.tar.gz", "sha256": digestBytes([]byte("base15"))},
					},
					"2.0.0": map[string]any{
						"artifact": map[string]any{"url": "base-2.0.0.tar.gz", "sha256": digestBytes([]byte("base20"))},
					},
				},
			},
			"app": map[string]any{
				"versions": map[string]any{
					"1.0.0": map[string]any{
						"artifact":     map[string]any{"url": "app-1.0.0.tar.gz", "sha256": digestBytes([]byte("app10"))},
						"dependencies": map[string]string{"base": "^1.0.0"},
					},
					"2.0.0": map[string]any{
						"artifact":     map[string]any{"url": "app-2.0.0.tar.gz", "sha256": digestBytes([]byte("app20"))},
						"dependencies": map[string]string{"base": "^2.0.0"},
					},
				},
			},
		},
	}
	indexPath := writeIndex(t, dir, index)
	catalog, err := OpenCatalog(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := catalog.Resolve("app", "^1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, item := range resolved {
		got[item.Name] = item.Version
	}
	if got["app"] != "1.0.0" || got["base"] != "1.5.0" {
		t.Fatalf("resolved = %v, want app 1.0.0 + base 1.5.0", got)
	}

	resolved, err = catalog.Resolve("app", "*")
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]string{}
	for _, item := range resolved {
		got[item.Name] = item.Version
	}
	if got["app"] != "2.0.0" || got["base"] != "2.0.0" {
		t.Fatalf("resolved = %v, want app 2.0.0 + base 2.0.0", got)
	}
}

func TestCatalogResolvesTransitiveDependencies(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"app-1.0.0.tar.gz":  "app",
		"mid-1.2.0.tar.gz":  "mid",
		"base-1.8.0.tar.gz": "base",
	} {
		writeTestFile(t, filepath.Join(dir, name), []byte(body))
	}
	index := map[string]any{
		"schema_version": 1,
		"plugins": map[string]any{
			"app": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact":     map[string]any{"url": "app-1.0.0.tar.gz", "sha256": digestBytes([]byte("app"))},
					"dependencies": map[string]string{"mid": ">=1.0.0"},
				},
			}},
			"mid": map[string]any{"versions": map[string]any{
				"1.2.0": map[string]any{
					"artifact":     map[string]any{"url": "mid-1.2.0.tar.gz", "sha256": digestBytes([]byte("mid"))},
					"dependencies": map[string]string{"base": "~1.8.0"},
				},
			}},
			"base": map[string]any{"versions": map[string]any{
				"1.8.0": map[string]any{
					"artifact": map[string]any{"url": "base-1.8.0.tar.gz", "sha256": digestBytes([]byte("base"))},
				},
			}},
		},
	}
	catalog, err := OpenCatalog(writeIndex(t, dir, index))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve("app", "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 3 {
		t.Fatalf("resolved %d packages, want 3: %+v", len(resolved), resolved)
	}
}

func TestCatalogBacktracksConflictingDependencyChoices(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"root", "a", "z", "core"} {
		writeTestFile(t, filepath.Join(dir, name+".tar.gz"), []byte(name))
	}
	artifact := func(name string) map[string]any {
		return map[string]any{
			"url":    name + ".tar.gz",
			"sha256": digestBytes([]byte(name)),
		}
	}
	index := map[string]any{
		"schema_version": 1,
		"plugins": map[string]any{
			"root": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact":     artifact("root"),
					"dependencies": map[string]string{"a": "^1.0.0", "z": "^1.0.0"},
				},
			}},
			"a": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact":     artifact("a"),
					"dependencies": map[string]string{"core": "^1.0.0"},
				},
			}},
			"z": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact":     artifact("z"),
					"dependencies": map[string]string{"core": "^1.0.0"},
				},
				"1.1.0": map[string]any{
					"artifact":     artifact("z"),
					"dependencies": map[string]string{"core": "^2.0.0"},
				},
			}},
			"core": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{"artifact": artifact("core")},
				"2.0.0": map[string]any{"artifact": artifact("core")},
			}},
		},
	}
	catalog, err := OpenCatalog(writeIndex(t, dir, index))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve("root", "*")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, item := range resolved {
		got[item.Name] = item.Version
	}
	if got["core"] != "1.0.0" || got["z"] != "1.0.0" {
		t.Fatalf("backtracking picked %v, want core 1.0.0 and z 1.0.0", got)
	}
}

func TestCatalogRejectsMissingDependency(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "app.tar.gz"), []byte("app"))
	index := map[string]any{
		"schema_version": 1,
		"plugins": map[string]any{
			"app": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact":     map[string]any{"url": "app.tar.gz", "sha256": digestBytes([]byte("app"))},
					"dependencies": map[string]string{"missing": "*"},
				},
			}},
		},
	}
	catalog, err := OpenCatalog(writeIndex(t, dir, index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve("app", "*"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Resolve error = %v, want a missing dependency error", err)
	}
}

func TestCatalogRejectsUnknownFieldsAndInsecureSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	writeTestFile(t, path, []byte(`{"schema_version":1,"plugins":{},"extra":true}`))
	if _, err := OpenCatalog(path); err == nil {
		t.Fatal("unknown registry field was accepted")
	}
	if _, err := OpenCatalog("http://example.com/plugins.json"); err == nil {
		t.Fatal("non-loopback plain HTTP was accepted")
	}
}

func TestCatalogFetchesFromLoopbackHTTP(t *testing.T) {
	artifact := []byte("plugin archive")
	indexData, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"plugins": map[string]any{
			"remote": map[string]any{"versions": map[string]any{
				"1.0.0": map[string]any{
					"artifact": map[string]any{
						"url":    "remote.tar.gz",
						"sha256": digestBytes(artifact),
					},
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write(indexData)
		case "/remote.tar.gz":
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := OpenCatalog(server.URL + "/index.json")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve("remote", "*")
	if err != nil {
		t.Fatal(err)
	}
	got, err := catalog.FetchArtifact(resolved[0].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(artifact) {
		t.Fatalf("artifact = %q", got)
	}
}

func writeIndex(t *testing.T, dir string, index map[string]any) string {
	t.Helper()
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index.json")
	writeTestFile(t, path, data)
	return path
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
