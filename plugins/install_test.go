package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testPluginVersion struct {
	version      string
	dependencies map[string]string
	prefix       string
}

func TestInstallVerifyUpgradeAndRemove(t *testing.T) {
	catalogDir := t.TempDir()
	catalog := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"base": {{version: "1.0.0"}, {version: "1.1.0"}},
		"app": {
			{version: "1.0.0", dependencies: map[string]string{"base": "^1.0.0"}},
			{version: "1.1.0", dependencies: map[string]string{"base": ">=1.1.0 <2.0.0"}},
		},
	})
	root := t.TempDir()

	records, err := Install(root, catalog, "app", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	lock, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Plugins["app"].Version != "1.0.0" || lock.Plugins["base"].Version != "1.1.0" {
		t.Fatalf("lock = %+v", lock.Plugins)
	}
	if issues, err := Verify(root); err != nil || len(issues) != 0 {
		t.Fatalf("Verify = %v, %v", issues, err)
	}
	installed, err := ListInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range installed {
		if !item.Healthy {
			t.Fatalf("%s is unhealthy: %s", item.Name, item.Problem)
		}
	}

	records, err = Install(root, catalog, "app", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("upgrade records = %+v", records)
	}
	lock, err = ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Plugins["app"].Version != "1.1.0" || lock.Plugins["base"].Version != "1.1.0" {
		t.Fatalf("upgrade lock = %+v", lock.Plugins)
	}

	if err := Remove(root, "base"); err == nil || !strings.Contains(err.Error(), "required by app") {
		t.Fatalf("Remove dependency error = %v", err)
	}
	if err := Remove(root, "app"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "base"); err != nil {
		t.Fatal(err)
	}
	if issues, err := Verify(root); err != nil || len(issues) != 0 {
		t.Fatalf("Verify after remove = %v, %v", issues, err)
	}
}

func TestInstallRejectsDigestMismatchWithoutChangingCurrentVersion(t *testing.T) {
	catalogDir := t.TempDir()
	catalog := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	root := t.TempDir()
	if _, err := Install(root, catalog, "app", "1.0.0"); err != nil {
		t.Fatal(err)
	}

	broken := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"app": {{version: "1.1.0"}},
	})
	path := filepath.Join(catalogDir, "app-1.1.0.tar.gz")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	writeTestFile(t, path, data)
	if _, err := Install(root, broken, "app", "1.1.0"); err == nil ||
		!strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("Install error = %v", err)
	}
	lock, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Plugins["app"].Version != "1.0.0" {
		t.Fatalf("failed upgrade changed lock: %+v", lock.Plugins["app"])
	}
	if issues, err := Verify(root); err != nil || len(issues) != 0 {
		t.Fatalf("Verify = %v, %v", issues, err)
	}
}

func TestVerifyDetectsTamperingAndUnmanagedPlugins(t *testing.T) {
	catalog := testCatalog(t, t.TempDir(), map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	root := t.TempDir()
	if _, err := Install(root, catalog, "app", "*"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "app", "README.md"), []byte("tampered\n"))
	issues, err := Verify(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0].String(), "tree digest") {
		t.Fatalf("tamper issues = %v", issues)
	}

	if _, err := Install(root, catalog, "app", "*"); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root, "manual", `{
	  "schema_version": 1, "name": "manual", "version": "1.0.0", "command": ["python3", "plugin.py"]
	}`)
	issues, err = Verify(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0].Problem, "not recorded") {
		t.Fatalf("unmanaged issues = %v", issues)
	}
}

func TestInstallAcceptsWrappedArchiveRoot(t *testing.T) {
	catalog := testCatalog(t, t.TempDir(), map[string][]testPluginVersion{
		"wrapped": {{version: "1.0.0", prefix: "wrapped-1.0.0/"}},
	})
	root := t.TempDir()
	if _, err := Install(root, catalog, "wrapped", "*"); err != nil {
		t.Fatal(err)
	}
	candidate, err := readCandidate(filepath.Join(root, "wrapped"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Manifest.Name != "wrapped" || candidate.Manifest.Version != "1.0.0" {
		t.Fatalf("manifest = %+v", candidate.Manifest)
	}
}

func testCatalog(t *testing.T, dir string, releases map[string][]testPluginVersion) *Catalog {
	t.Helper()
	index := map[string]any{
		"schema_version": 1,
		"plugins":        map[string]any{},
	}
	plugins := index["plugins"].(map[string]any)
	for name, versions := range releases {
		versionMap := map[string]any{}
		for _, release := range versions {
			data := pluginArchive(t, name, release.version, release.prefix)
			filename := name + "-" + release.version + ".tar.gz"
			writeTestFile(t, filepath.Join(dir, filename), data)
			item := map[string]any{
				"artifact": map[string]any{
					"url":    filename,
					"sha256": digestBytes(data),
				},
			}
			if len(release.dependencies) > 0 {
				item["dependencies"] = release.dependencies
			}
			versionMap[release.version] = item
		}
		plugins[name] = map[string]any{"versions": versionMap}
	}
	catalog, err := OpenCatalog(writeIndex(t, dir, index))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
