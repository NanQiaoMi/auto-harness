package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "publisher.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	}), 0o600); err != nil {
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
