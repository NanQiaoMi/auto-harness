package plugins

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// PublisherStoreSchemaVersion is the trusted-publisher file version.
	PublisherStoreSchemaVersion = 1
	// PublisherStoreFileName is the user's trusted publisher file.
	PublisherStoreFileName = "plugin_publishers.json"

	maxTrustedPublishers = 256
	maxKeysPerPublisher  = 16
	maxKeyFileBytes      = 64 << 10
)

// PublisherStore is the local trust root for signed plugin registries.
// It contains public keys only.
type PublisherStore struct {
	SchemaVersion int                 `json:"schema_version"`
	Publishers    map[string][]string `json:"publishers"`
}

// TrustedPublisher is one publisher and the fingerprints of its keys.
type TrustedPublisher struct {
	Publisher string
	KeyIDs    []string
}

// PublisherStorePath returns the default trusted-publisher file.
func PublisherStorePath() string {
	if path := strings.TrimSpace(os.Getenv("HARNESS_PLUGIN_TRUST_FILE")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".harness", PublisherStoreFileName)
	}
	return filepath.Join(home, ".harness", PublisherStoreFileName)
}

// EmptyPublisherStore returns an empty trust root.
func EmptyPublisherStore() *PublisherStore {
	return &PublisherStore{
		SchemaVersion: PublisherStoreSchemaVersion,
		Publishers:    map[string][]string{},
	}
}

// LoadPublisherStore reads the trusted-publisher file. A missing file
// is an empty trust root.
func LoadPublisherStore(path string) (*PublisherStore, error) {
	if strings.TrimSpace(path) == "" {
		path = PublisherStorePath()
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EmptyPublisherStore(), nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("plugin publishers: %s is not a regular file", path)
	}
	if info.Size() > maxIndexBytes {
		return nil, fmt.Errorf("plugin publishers: file is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var store PublisherStore
	if err := decodeStrictJSON(data, &store); err != nil {
		return nil, fmt.Errorf("plugin publishers: %s: %w", path, err)
	}
	if store.SchemaVersion != PublisherStoreSchemaVersion {
		return nil, fmt.Errorf(
			"plugin publishers: unsupported schema_version %d", store.SchemaVersion)
	}
	if store.Publishers == nil {
		store.Publishers = map[string][]string{}
	}
	if len(store.Publishers) > maxTrustedPublishers {
		return nil, fmt.Errorf("plugin publishers: too many publishers")
	}
	for publisher, keys := range store.Publishers {
		if err := validatePublisherName(publisher); err != nil {
			return nil, fmt.Errorf("plugin publishers: %w", err)
		}
		if len(keys) == 0 || len(keys) > maxKeysPerPublisher {
			return nil, fmt.Errorf(
				"plugin publishers: %s must have between 1 and %d keys",
				publisher, maxKeysPerPublisher)
		}
		seen := map[string]bool{}
		for i, encoded := range keys {
			key, err := DecodePublicKey(encoded)
			if err != nil {
				return nil, fmt.Errorf(
					"plugin publishers: %s key %d: %w", publisher, i, err)
			}
			canonical := base64.StdEncoding.EncodeToString(key)
			if seen[canonical] {
				return nil, fmt.Errorf(
					"plugin publishers: %s contains a duplicate key", publisher)
			}
			seen[canonical] = true
			keys[i] = canonical
		}
		sort.Strings(keys)
		store.Publishers[publisher] = keys
	}
	return &store, nil
}

// Add trusts a publisher public key.
func (s *PublisherStore) Add(publisher string, key ed25519.PublicKey) (string, error) {
	if s == nil {
		return "", fmt.Errorf("plugin publishers: store is nil")
	}
	if err := validatePublisherName(publisher); err != nil {
		return "", err
	}
	if len(key) != ed25519.PublicKeySize {
		return "", fmt.Errorf("plugin publishers: Ed25519 public key is invalid")
	}
	if s.Publishers == nil {
		s.Publishers = map[string][]string{}
	}
	if _, exists := s.Publishers[publisher]; !exists && len(s.Publishers) >= maxTrustedPublishers {
		return "", fmt.Errorf("plugin publishers: too many publishers")
	}
	keys := s.Publishers[publisher]
	if len(keys) >= maxKeysPerPublisher {
		return "", fmt.Errorf("plugin publishers: %s already has %d keys", publisher, len(keys))
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	for _, existing := range keys {
		if existing == encoded {
			return PublicKeyID(key), nil
		}
	}
	keys = append(keys, encoded)
	sort.Strings(keys)
	s.Publishers[publisher] = keys
	return PublicKeyID(key), nil
}

// RemovePublisher removes every trusted key for one publisher.
func (s *PublisherStore) RemovePublisher(publisher string) error {
	if s == nil || s.Publishers == nil {
		return fmt.Errorf("publisher %q is not trusted", publisher)
	}
	if _, ok := s.Publishers[publisher]; !ok {
		return fmt.Errorf("publisher %q is not trusted", publisher)
	}
	delete(s.Publishers, publisher)
	return nil
}

// Entries lists trusted publishers in stable order.
func (s *PublisherStore) Entries() ([]TrustedPublisher, error) {
	if s == nil {
		return nil, nil
	}
	names := make([]string, 0, len(s.Publishers))
	for name := range s.Publishers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]TrustedPublisher, 0, len(names))
	for _, name := range names {
		entry := TrustedPublisher{Publisher: name}
		for _, encoded := range s.Publishers[name] {
			key, err := DecodePublicKey(encoded)
			if err != nil {
				return nil, fmt.Errorf("publisher %s: %w", name, err)
			}
			entry.KeyIDs = append(entry.KeyIDs, PublicKeyID(key))
		}
		sort.Strings(entry.KeyIDs)
		out = append(out, entry)
	}
	return out, nil
}

// Policy converts the trust root into the registry verification policy.
func (s *PublisherStore) Policy(requireSignature bool) (TrustPolicy, error) {
	policy := TrustPolicy{
		Publishers:       map[string][]ed25519.PublicKey{},
		RequireSignature: requireSignature,
	}
	if s == nil {
		return policy, nil
	}
	for publisher, encodedKeys := range s.Publishers {
		for _, encoded := range encodedKeys {
			key, err := DecodePublicKey(encoded)
			if err != nil {
				return TrustPolicy{}, fmt.Errorf("publisher %s: %w", publisher, err)
			}
			policy.Publishers[publisher] = append(policy.Publishers[publisher], key)
		}
	}
	return policy, nil
}

// Write atomically replaces the trusted-publisher file.
func (s *PublisherStore) Write(path string) error {
	if s == nil {
		return fmt.Errorf("plugin publishers: store is nil")
	}
	if strings.TrimSpace(path) == "" {
		path = PublisherStorePath()
	}
	s.SchemaVersion = PublisherStoreSchemaVersion
	if s.Publishers == nil {
		s.Publishers = map[string][]string{}
	}
	data, err := marshalIndent(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".plugin-publishers-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// ReadPublicKeyFile reads a raw base64 or PKIX PEM Ed25519 public key.
func ReadPublicKeyFile(path string) (ed25519.PublicKey, error) {
	data, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("public key PEM block is %q, want PUBLIC KEY", block.Type)
		}
		value, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("public key PEM: %w", err)
		}
		key, ok := value.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is not Ed25519")
		}
		return append(ed25519.PublicKey(nil), key...), nil
	}
	return DecodePublicKey(string(data))
}

// ReadPrivateKeyFile reads a PKCS#8 PEM Ed25519 private key.
func ReadPrivateKeyFile(path string) (ed25519.PrivateKey, error) {
	data, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key must be PKCS#8 PEM")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key PEM block is %q, want PRIVATE KEY", block.Type)
	}
	value, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key PEM: %w", err)
	}
	key, ok := value.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not Ed25519")
	}
	return append(ed25519.PrivateKey(nil), key...), nil
}

// DecodePublicKey parses a raw Ed25519 public key encoded as base64.
func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("public key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must decode to %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), nil
}

func readKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxKeyFileBytes {
		return nil, fmt.Errorf("%s is too large", path)
	}
	return os.ReadFile(path)
}
