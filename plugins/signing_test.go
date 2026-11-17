package plugins

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedRegistryInstallRecordsPublisher(t *testing.T) {
	catalogDir := t.TempDir()
	unsigned := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	publicKey, privateKey := testEd25519Key(t)
	index := unsigned.index
	if err := SignIndex(&index, "acme", privateKey); err != nil {
		t.Fatalf("SignIndex: %v", err)
	}
	signedPath := writeSignedIndex(t, catalogDir, index)

	policy := TrustPolicy{
		Publishers: map[string][]ed25519.PublicKey{
			"acme": {publicKey},
		},
		RequireSignature: true,
	}
	catalog, err := OpenCatalogWithPolicy(signedPath, policy)
	if err != nil {
		t.Fatalf("OpenCatalogWithPolicy: %v", err)
	}
	if !catalog.Verified || catalog.Publisher != "acme" ||
		catalog.SignerKeyID != PublicKeyID(publicKey) {
		t.Fatalf("catalog provenance = %#v", catalog)
	}

	root := t.TempDir()
	if _, err := Install(root, catalog, "app", "*"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	lock, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	locked := lock.Plugins["app"]
	if locked.Publisher != "acme" || locked.SignerKeyID != PublicKeyID(publicKey) {
		t.Fatalf("lock provenance = %#v", locked)
	}
	installed, err := ListInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].Publisher != "acme" ||
		installed[0].SignerKeyID != PublicKeyID(publicKey) {
		t.Fatalf("installed = %#v", installed)
	}
}

func TestSignedRegistryRejectsTamperingAndUntrustedKeys(t *testing.T) {
	catalogDir := t.TempDir()
	unsigned := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	publicKey, privateKey := testEd25519Key(t)
	index := unsigned.index
	if err := SignIndex(&index, "acme", privateKey); err != nil {
		t.Fatalf("SignIndex: %v", err)
	}
	signed := mustMarshalIndex(t, index)
	policy := TrustPolicy{
		Publishers: map[string][]ed25519.PublicKey{
			"acme": {publicKey},
		},
	}

	var document map[string]any
	if err := json.Unmarshal(signed, &document); err != nil {
		t.Fatal(err)
	}
	plugins := document["plugins"].(map[string]any)
	app := plugins["app"].(map[string]any)
	versions := app["versions"].(map[string]any)
	version := versions["1.0.0"].(map[string]any)
	version["description"] = "tampered after signing"
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(catalogDir, "tampered-index.json")
	writeTestFile(t, tamperedPath, tampered)
	if _, err := OpenCatalogWithPolicy(tamperedPath, policy); err == nil ||
		!strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("tampered registry error = %v", err)
	}

	otherPublic, _ := testEd25519Key(t)
	untrustedPolicy := TrustPolicy{
		Publishers: map[string][]ed25519.PublicKey{
			"acme": {otherPublic},
		},
	}
	if _, err := OpenCatalogWithPolicy(writeSignedIndex(t, catalogDir, index), untrustedPolicy); err == nil ||
		!strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("untrusted key error = %v", err)
	}
}

func TestSignedRegistryUsesProtocolDomain(t *testing.T) {
	catalogDir := t.TempDir()
	unsigned := testCatalog(t, catalogDir, map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	publicKey, privateKey := testEd25519Key(t)
	index := unsigned.index
	index.Publisher = "acme"
	payload, err := canonicalIndexPayload(&index)
	if err != nil {
		t.Fatal(err)
	}
	index.Signature = &IndexSignature{
		Algorithm: RegistrySignatureAlgorithm,
		KeyID:     PublicKeyID(publicKey),
		Value:     base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	policy := TrustPolicy{
		Publishers: map[string][]ed25519.PublicKey{
			"acme": {publicKey},
		},
	}
	if _, err := OpenCatalogWithPolicy(
		writeSignedIndex(t, catalogDir, index), policy); err == nil ||
		!strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("undomained signature error = %v", err)
	}
}

func TestSignedRegistryPolicyRejectsUnsignedIndex(t *testing.T) {
	dir := t.TempDir()
	testCatalog(t, dir, map[string][]testPluginVersion{
		"app": {{version: "1.0.0"}},
	})
	path := filepath.Join(dir, "index.json")
	if _, err := OpenCatalogWithPolicy(path, TrustPolicy{RequireSignature: true}); err == nil ||
		!strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned registry error = %v", err)
	}
}

func TestSignedRegistryValidatesMetadata(t *testing.T) {
	base := Index{
		SchemaVersion: RegistrySchemaVersion,
		Publisher:     "acme",
		Plugins: map[string]IndexedPlugin{
			"app": {Versions: map[string]IndexedVersion{
				"1.0.0": {
					Artifact: Artifact{
						URL:    "app.tar.gz",
						SHA256: strings.Repeat("a", 64),
					},
				},
			}},
		},
	}
	cases := []struct {
		name      string
		signature *IndexSignature
		want      string
	}{
		{"algorithm", &IndexSignature{
			Algorithm: "rsa", KeyID: "ed25519:" + strings.Repeat("0", 64),
			Value: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		}, "algorithm"},
		{"key id", &IndexSignature{
			Algorithm: RegistrySignatureAlgorithm, KeyID: "bad",
			Value: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		}, "key_id"},
		{"signature bytes", &IndexSignature{
			Algorithm: RegistrySignatureAlgorithm, KeyID: "ed25519:" + strings.Repeat("0", 64),
			Value: base64.StdEncoding.EncodeToString([]byte("short")),
		}, "bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := base
			index.Signature = tc.signature
			if err := validateIndex(&index); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateIndex error = %v, want %q", err, tc.want)
			}
		})
	}

	unsigned := base
	unsigned.Publisher = ""
	unsigned.Signature = nil
	if err := validateIndex(&unsigned); err != nil {
		t.Fatalf("unsigned index: %v", err)
	}
	unsigned.Publisher = "acme"
	if err := validateIndex(&unsigned); err == nil ||
		!strings.Contains(err.Error(), "publisher requires") {
		t.Fatalf("publisher mismatch error = %v", err)
	}
}

func TestLockFileReadsV1AndWritesCurrentSchema(t *testing.T) {
	root := t.TempDir()
	oldLock := `{
	  "schema_version": 1,
	  "plugins": {
	    "app": {
	      "version": "1.0.0",
	      "digest": "` + strings.Repeat("a", 64) + `",
	      "artifact_sha256": "` + strings.Repeat("b", 64) + `",
	      "source": "./registry.json",
	      "dependencies": {
	        "base": "^1.0.0"
	      }
	    }
	  }
	}`
	writeTestFile(t, filepath.Join(root, LockFileName), []byte(oldLock))
	lock, err := ReadLock(root)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if lock.SchemaVersion != 1 || lock.Plugins["app"].Version != "1.0.0" {
		t.Fatalf("v1 lock = %#v", lock)
	}
	item := lock.Plugins["app"]
	item.Publisher = "acme"
	item.SignerKeyID = "ed25519:" + strings.Repeat("c", 64)
	lock.Plugins["app"] = item
	if err := WriteLock(root, lock); err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	upgraded, err := ReadLock(root)
	if err != nil {
		t.Fatalf("ReadLock upgraded: %v", err)
	}
	if upgraded.SchemaVersion != LockSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", upgraded.SchemaVersion, LockSchemaVersion)
	}
	upgradedItem := upgraded.Plugins["app"]
	if upgradedItem.Publisher != "acme" || upgradedItem.SignerKeyID != item.SignerKeyID {
		t.Fatalf("upgraded provenance = %#v", upgradedItem)
	}
}

func TestLockFileRequiresCompleteProvenance(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{"publisher only", `"publisher": "acme",`},
		{"signer only", `"signer_key_id": "ed25519:` + strings.Repeat("c", 64) + `",`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			lock := `{
			  "schema_version": 2,
			  "plugins": {
			    "app": {
			      ` + tc.extra + `
			      "version": "1.0.0",
			      "digest": "` + strings.Repeat("a", 64) + `",
			      "artifact_sha256": "` + strings.Repeat("b", 64) + `",
			      "source": "./registry.json"
			    }
			  }
			}`
			writeTestFile(t, filepath.Join(root, LockFileName), []byte(lock))
			if _, err := ReadLock(root); err == nil ||
				!strings.Contains(err.Error(), "must appear together") {
				t.Fatalf("ReadLock error = %v", err)
			}
		})
	}
}

func TestPublisherStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PublisherStoreFileName)
	publicKey, _ := testEd25519Key(t)

	publicPath := filepath.Join(dir, "publisher.pub")
	writeTestFile(t, publicPath, pemPublicKey(t, publicKey))
	loadedPublic, err := ReadPublicKeyFile(publicPath)
	if err != nil {
		t.Fatalf("ReadPublicKeyFile: %v", err)
	}
	if PublicKeyID(loadedPublic) != PublicKeyID(publicKey) {
		t.Fatal("PEM public key changed during parsing")
	}

	store, err := LoadPublisherStore(path)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := store.Add("acme", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != PublicKeyID(publicKey) {
		t.Fatalf("key id = %s", keyID)
	}
	if err := store.Write(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadPublisherStore(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reloaded.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Publisher != "acme" ||
		len(entries[0].KeyIDs) != 1 || entries[0].KeyIDs[0] != keyID {
		t.Fatalf("entries = %#v", entries)
	}
	policy, err := reloaded.Policy(true)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.RequireSignature || len(policy.Publishers["acme"]) != 1 {
		t.Fatalf("policy = %#v", policy)
	}
	if err := reloaded.RemovePublisher("acme"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.RemovePublisher("acme"); err == nil {
		t.Fatal("removing an unknown publisher succeeded")
	}
}

func TestPublisherStorePathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-publishers.json")
	t.Setenv("HARNESS_PLUGIN_TRUST_FILE", path)
	if got := PublisherStorePath(); got != path {
		t.Fatalf("PublisherStorePath = %q, want %q", got, path)
	}
}

func TestPublisherStoreRejectsInvalidKey(t *testing.T) {
	store := EmptyPublisherStore()
	if _, err := store.Add("acme", ed25519.PublicKey("short")); err == nil {
		t.Fatal("invalid public key was accepted")
	}
	if _, err := store.Add("../bad", make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
		t.Fatal("invalid publisher name was accepted")
	}
	path := filepath.Join(t.TempDir(), PublisherStoreFileName)
	writeTestFile(t, path, []byte(`{
	  "schema_version": 1,
	  "publishers": {"acme": ["not-base64"]}
	}`))
	if _, err := LoadPublisherStore(path); err == nil {
		t.Fatal("invalid stored public key was accepted")
	}
}

func TestReadPrivateKeyFile(t *testing.T) {
	_, privateKey := testEd25519Key(t)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "publisher.pem")
	writeTestFile(t, path, pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	}))
	got, err := ReadPrivateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Public().(ed25519.PublicKey), privateKey.Public().(ed25519.PublicKey)) {
		t.Fatal("private key changed during parsing")
	}
}

func testEd25519Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func pemPublicKey(t *testing.T, key ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func writeSignedIndex(t *testing.T, dir string, index Index) string {
	t.Helper()
	data := mustMarshalIndex(t, index)
	path := filepath.Join(dir, "signed-index.json")
	writeTestFile(t, path, data)
	return path
}

func mustMarshalIndex(t *testing.T, index Index) []byte {
	t.Helper()
	data, err := MarshalIndexDocument(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
