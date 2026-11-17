package main

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

func TestSignRegistry(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
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
	if err := os.WriteFile(indexPath, []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, "publisher.pem")
	publicKeyPath := filepath.Join(dir, "publisher.pub")
	if err := generatePublisherKey(keyPath, publicKeyPath); err != nil {
		t.Fatal(err)
	}
	publicKey, err := plugins.ReadPublicKeyFile(publicKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "signed.json")
	if err := signRegistry(indexPath, "acme", keyPath, outputPath); err != nil {
		t.Fatalf("signRegistry: %v", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := plugins.ParseIndexDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugins.VerifyIndexSignature(&signed, plugins.TrustPolicy{
		Publishers: map[string][]ed25519.PublicKey{
			"acme": {publicKey},
		},
	}); err != nil {
		t.Fatalf("VerifyIndexSignature: %v", err)
	}
}

func TestGeneratePublisherKeyRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "publisher.pem")
	publicPath := filepath.Join(dir, "publisher.pub")
	if err := generatePublisherKey(privatePath, publicPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key permissions = %o, want 600", perm)
	}
	if err := generatePublisherKey(privatePath, publicPath); err == nil {
		t.Fatal("key generation overwrote existing files")
	}
	if err := generatePublisherKey(filepath.Join(dir, "same.pem"),
		filepath.Join(dir, "same.pem")); err == nil {
		t.Fatal("key generation accepted the same private and public path")
	}
}
