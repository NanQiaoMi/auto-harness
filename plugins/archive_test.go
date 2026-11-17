package plugins

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "extract")
	archive := tarGZ(t, []tarEntry{
		{Name: "plugin.json", Type: tar.TypeReg, Body: []byte("{}")},
		{Name: "../escaped", Type: tar.TypeReg, Body: []byte("bad")},
	})
	err := extractArchive(archive, target)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("extractArchive error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("traversal wrote outside the destination: %v", err)
	}
}

func TestExtractArchiveRejectsLinks(t *testing.T) {
	archive := tarGZ(t, []tarEntry{
		{Name: "plugin.json", Type: tar.TypeReg, Body: []byte("{}")},
		{Name: "link", Type: tar.TypeSymlink, Linkname: "/etc/passwd"},
	})
	err := extractArchive(archive, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "links are not allowed") {
		t.Fatalf("extractArchive error = %v", err)
	}
}

func TestPluginRootAcceptsOneTopLevelDirectory(t *testing.T) {
	root := t.TempDir()
	archive := pluginArchive(t, "wrapped", "1.0.0", "wrapped-1.0.0/")
	if err := extractArchive(archive, root); err != nil {
		t.Fatal(err)
	}
	found, err := pluginRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if found != filepath.Join(root, "wrapped-1.0.0") {
		t.Fatalf("pluginRoot = %q", found)
	}
}

func TestTreeDigestDetectsTamperingAndModeChanges(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "plugin.json"), []byte("{}"))
	writeTestFile(t, filepath.Join(root, "plugin.py"), []byte("print('ok')\n"))
	if err := os.Chmod(filepath.Join(root, "plugin.py"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("tree digest is not stable")
	}
	if err := os.Chmod(filepath.Join(root, "plugin.py"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedMode, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if changedMode == first {
		t.Fatal("tree digest did not change with the executable bit")
	}
	writeTestFile(t, filepath.Join(root, "plugin.json"), []byte(`{"changed":true}`))
	changedBody, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if changedBody == changedMode {
		t.Fatal("tree digest did not change with file content")
	}
}
