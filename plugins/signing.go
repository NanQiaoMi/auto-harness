package plugins

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const registrySignatureDomain = "auto-harness/plugin-registry/v1\x00"

// SignIndex signs the canonical registry payload with an Ed25519
// publisher key. The publisher and key fingerprint are part of the
// signed document.
func SignIndex(index *Index, publisher string, privateKey ed25519.PrivateKey) error {
	if index == nil {
		return fmt.Errorf("plugin registry sign: index is nil")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("plugin registry sign: Ed25519 private key is invalid")
	}
	if index.Signature != nil {
		return fmt.Errorf("plugin registry sign: index is already signed")
	}
	if err := validateIndexPayload(index); err != nil {
		return fmt.Errorf("plugin registry sign: %w", err)
	}
	if err := validatePublisherName(publisher); err != nil {
		return fmt.Errorf("plugin registry sign: %w", err)
	}
	index.Publisher = publisher
	payload, err := signedIndexPayload(index)
	if err != nil {
		return fmt.Errorf("plugin registry sign: %w", err)
	}
	signature := ed25519.Sign(privateKey, payload)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	index.Signature = &IndexSignature{
		Algorithm: RegistrySignatureAlgorithm,
		KeyID:     PublicKeyID(publicKey),
		Value:     base64.StdEncoding.EncodeToString(signature),
	}
	return validateIndex(index)
}

// VerifyIndexSignature authenticates a signed registry with the keys
// allowed by the policy. It does not modify the index.
func VerifyIndexSignature(index *Index, policy TrustPolicy) error {
	if index == nil {
		return fmt.Errorf("plugin registry signature: index is nil")
	}
	if err := validateIndex(index); err != nil {
		return err
	}
	if index.Signature == nil {
		return fmt.Errorf("plugin registry signature: registry is unsigned")
	}
	keys := policy.Publishers[index.Publisher]
	if len(keys) == 0 {
		return fmt.Errorf("plugin registry signature: publisher %q is not trusted", index.Publisher)
	}
	signature, err := decodeSignature(index.Signature.Value)
	if err != nil {
		return err
	}
	payload, err := signedIndexPayload(index)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			continue
		}
		if PublicKeyID(key) != index.Signature.KeyID {
			continue
		}
		if !ed25519.Verify(key, payload, signature) {
			return fmt.Errorf("plugin registry signature: signature verification failed")
		}
		return nil
	}
	return fmt.Errorf(
		"plugin registry signature: key %s for publisher %q is not trusted",
		index.Signature.KeyID, index.Publisher)
}

// PublicKeyID returns the stable fingerprint used in signatures and
// the local publisher trust store.
func PublicKeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return RegistrySignatureAlgorithm + ":" + hex.EncodeToString(sum[:])
}

func canonicalIndexPayload(index *Index) ([]byte, error) {
	copy := *index
	copy.Signature = nil
	return json.Marshal(copy)
}

func signedIndexPayload(index *Index) ([]byte, error) {
	payload, err := canonicalIndexPayload(index)
	if err != nil {
		return nil, err
	}
	signed := make([]byte, 0, len(registrySignatureDomain)+len(payload))
	signed = append(signed, registrySignatureDomain...)
	signed = append(signed, payload...)
	return signed, nil
}

func validatePublisherName(publisher string) error {
	if !pluginNameRE.MatchString(publisher) {
		return fmt.Errorf("publisher %q must be a stable name", publisher)
	}
	if len(publisher) > 128 {
		return fmt.Errorf("publisher name is too long")
	}
	return nil
}

func validateKeyID(keyID string) error {
	prefix := RegistrySignatureAlgorithm + ":"
	if !strings.HasPrefix(keyID, prefix) || len(keyID) != len(prefix)+sha256.Size*2 {
		return fmt.Errorf("key_id must be %s<sha256-hex>", prefix)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(keyID, prefix)); err != nil {
		return fmt.Errorf("key_id: %w", err)
	}
	return nil
}

func decodeSignature(value string) ([]byte, error) {
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("signature.value: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf(
			"signature.value must decode to %d bytes", ed25519.SignatureSize)
	}
	return signature, nil
}
