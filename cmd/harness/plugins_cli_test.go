package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

func TestSplitPluginSpec(t *testing.T) {
	cases := []struct {
		input      string
		name       string
		constraint string
	}{
		{"app", "app", "*"},
		{"app@1.2.3", "app", "1.2.3"},
		{" app @ ^1.0.0 ", "app", "^1.0.0"},
	}
	for _, tc := range cases {
		name, constraint, err := splitPluginSpec(tc.input)
		if err != nil {
			t.Fatalf("splitPluginSpec(%q): %v", tc.input, err)
		}
		if name != tc.name || constraint != tc.constraint {
			t.Errorf("splitPluginSpec(%q) = %q, %q", tc.input, name, constraint)
		}
	}
	for _, value := range []string{"@1.0.0", "app@", "", "  "} {
		if _, _, err := splitPluginSpec(value); err == nil {
			t.Errorf("splitPluginSpec(%q) succeeded", value)
		}
	}
}

func TestPluginCommandRejectsMultipleActions(t *testing.T) {
	isolateHome(t)
	handled, code, err := runPluginCommand(pluginCommandOptions{
		List:  true,
		Trust: "acme=/tmp/key.pub",
	})
	if !handled || code == 0 || err == nil {
		t.Fatalf("handled/code/error = %v/%d/%v", handled, code, err)
	}
}

func TestPluginCommandListAndVerifyEmptyHome(t *testing.T) {
	isolateHome(t)
	handled, code, err := runPluginCommand(pluginCommandOptions{List: true})
	if !handled || code != 0 || err != nil {
		t.Fatalf("list handled/code/error = %v/%d/%v", handled, code, err)
	}
	handled, code, err = runPluginCommand(pluginCommandOptions{Verify: true})
	if !handled || code != 0 || err != nil {
		t.Fatalf("verify handled/code/error = %v/%d/%v", handled, code, err)
	}
	handled, code, err = runPluginCommand(pluginCommandOptions{TrustList: true})
	if !handled || code != 0 || err != nil {
		t.Fatalf("trust list handled/code/error = %v/%d/%v", handled, code, err)
	}
}

func TestPluginCommandIgnoresIndexWithoutAction(t *testing.T) {
	handled, code, err := runPluginCommand(pluginCommandOptions{Index: "/not/used"})
	if handled || code != 0 || err != nil {
		t.Fatalf("handled/code/error = %v/%d/%v", handled, code, err)
	}
}

func TestSplitPublisherKeySpec(t *testing.T) {
	publisher, path, err := splitPublisherKeySpec(" acme = /tmp/key.pub ")
	if err != nil || publisher != "acme" || path != "/tmp/key.pub" {
		t.Fatalf("splitPublisherKeySpec = %q, %q, %v", publisher, path, err)
	}
	for _, value := range []string{"", "acme", "=key", "acme="} {
		if _, _, err := splitPublisherKeySpec(value); err == nil {
			t.Errorf("splitPublisherKeySpec(%q) succeeded", value)
		}
	}
}

func TestPluginPublisherTrustCommands(t *testing.T) {
	home := isolateHome(t)
	dir := t.TempDir()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "publisher.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	handled, code, err := runPluginCommand(pluginCommandOptions{
		Trust: "acme=" + keyPath,
	})
	if !handled || code != 0 || err != nil {
		t.Fatalf("trust handled/code/error = %v/%d/%v", handled, code, err)
	}
	storePath := filepath.Join(home, ".harness", plugins.PublisherStoreFileName)
	store, err := plugins.LoadPublisherStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Publisher != "acme" ||
		len(entries[0].KeyIDs) != 1 ||
		entries[0].KeyIDs[0] != plugins.PublicKeyID(publicKey) {
		t.Fatalf("trust entries = %#v", entries)
	}

	handled, code, err = runPluginCommand(pluginCommandOptions{Untrust: "acme"})
	if !handled || code != 0 || err != nil {
		t.Fatalf("untrust handled/code/error = %v/%d/%v", handled, code, err)
	}
	store, err = plugins.LoadPublisherStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("entries after untrust = %#v, %v", entries, err)
	}
}

func TestPluginCommandRejectsUnsignedRemoteRegistry(t *testing.T) {
	isolateHome(t)
	index := `{
	  "schema_version": 1,
	  "plugins": {
	    "app": {
	      "versions": {
	        "1.0.0": {
	          "artifact": {
	            "url": "app.tar.gz",
	            "sha256": "` + strings.Repeat("a", 64) + `"
	          }
	        }
	      }
	    }
	  }
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	defer server.Close()

	handled, code, err := runPluginCommand(pluginCommandOptions{
		Index:   server.URL + "/index.json",
		Install: "app",
	})
	if !handled || code == 0 || err == nil ||
		!strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned remote install = %v/%d/%v", handled, code, err)
	}
}

func TestPluginCommandInit(t *testing.T) {
	isolateHome(t)
	outDir := filepath.Join(t.TempDir(), "hello-plugin")
	handled, code, err := runPluginCommand(pluginCommandOptions{
		Init: "hello-plugin",
		Lang: "typescript",
		Dir:  outDir,
	})
	if !handled || code != 0 || err != nil {
		t.Fatalf("init handled/code/error = %v/%d/%v", handled, code, err)
	}
	cand, err := plugins.Load(outDir)
	if err != nil {
		t.Fatalf("plugins.Load(%s): %v", outDir, err)
	}
	if cand.Manifest.Name != "hello-plugin" {
		t.Errorf("Name = %q, want hello-plugin", cand.Manifest.Name)
	}

	handled, code, err = runPluginCommand(pluginCommandOptions{
		Init: "other",
		List: true,
	})
	if !handled || code == 0 || err == nil {
		t.Fatalf("init+list handled/code/error = %v/%d/%v", handled, code, err)
	}
}
