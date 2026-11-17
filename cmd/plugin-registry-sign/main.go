// Command plugin-registry-sign applies an Ed25519 publisher signature
// to an unsigned plugin registry document.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

func main() {
	indexPath := flag.String("index", "", "unsigned registry JSON input")
	publisher := flag.String("publisher", "", "stable publisher name")
	keyPath := flag.String("key", "", "Ed25519 PKCS#8 PEM private key")
	outputPath := flag.String("out", "-", "signed registry output path, or - for stdout")
	flag.Parse()

	if err := signRegistry(*indexPath, *publisher, *keyPath, *outputPath); err != nil {
		fmt.Fprintln(os.Stderr, "plugin-registry-sign:", err)
		os.Exit(1)
	}
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
