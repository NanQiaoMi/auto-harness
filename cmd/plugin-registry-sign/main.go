// Command plugin-registry-sign generates Ed25519 publisher keypairs
// and applies publisher signatures to unsigned plugin registries.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

func main() {
	indexPath := flag.String("index", "", "unsigned registry JSON input")
	publisher := flag.String("publisher", "", "stable publisher name")
	keyPath := flag.String("key", "", "Ed25519 PKCS#8 PEM private key")
	outputPath := flag.String("out", "-", "signed registry output path, or - for stdout")
	generateKey := flag.Bool("generate-key", false, "generate an Ed25519 publisher keypair")
	privateKeyOut := flag.String("private-key-out", "publisher.pem", "generated PKCS#8 private key path")
	publicKeyOut := flag.String("public-key-out", "publisher.pub", "generated PKIX public key path")
	flag.Parse()

	var err error
	if *generateKey {
		err = generatePublisherKey(*privateKeyOut, *publicKeyOut)
	} else {
		err = signRegistry(*indexPath, *publisher, *keyPath, *outputPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin-registry-sign:", err)
		os.Exit(1)
	}
}

func generatePublisherKey(privateKeyPath, publicKeyPath string) error {
	if strings.TrimSpace(privateKeyPath) == "" || strings.TrimSpace(publicKeyPath) == "" {
		return fmt.Errorf("private and public key output paths are required")
	}
	privateAbs, err := filepath.Abs(privateKeyPath)
	if err != nil {
		return err
	}
	publicAbs, err := filepath.Abs(publicKeyPath)
	if err != nil {
		return err
	}
	if privateAbs == publicAbs {
		return fmt.Errorf("private and public key output paths must differ")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := writeNewFile(privateKeyPath, privatePEM, 0o600); err != nil {
		return err
	}
	if err := writeNewFile(publicKeyPath, publicPEM, 0o644); err != nil {
		return fmt.Errorf("private key was written to %s, but public key failed: %w",
			privateKeyPath, err)
	}
	fmt.Printf("wrote private key %s\n", privateKeyPath)
	fmt.Printf("wrote public key %s (%s)\n", publicKeyPath, plugins.PublicKeyID(publicKey))
	return nil
}

func signRegistry(indexPath, publisher, keyPath, outputPath string) error {
	if indexPath == "" {
		return fmt.Errorf("-index is required")
	}
	if publisher == "" {
		return fmt.Errorf("-publisher is required")
	}
	if keyPath == "" {
		return fmt.Errorf("-key is required")
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	index, err := plugins.ParseIndexDocument(data)
	if err != nil {
		return err
	}
	if index.Signature != nil {
		return fmt.Errorf("index is already signed")
	}
	privateKey, err := plugins.ReadPrivateKeyFile(keyPath)
	if err != nil {
		return err
	}
	if err := plugins.SignIndex(&index, publisher, privateKey); err != nil {
		return err
	}
	signed, err := plugins.MarshalIndexDocument(index)
	if err != nil {
		return err
	}
	if outputPath == "-" {
		_, err = os.Stdout.Write(signed)
		return err
	}
	return writeFileAtomic(outputPath, signed)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".plugin-registry-*")
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

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
