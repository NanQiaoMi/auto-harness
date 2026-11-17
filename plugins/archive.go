package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxArtifactBytes  = 64 << 20
	maxExtractedBytes = 256 << 20
	maxArchiveFiles   = 8192
)

// extractArchive unpacks a plugin .tar.gz without allowing links,
// devices, duplicate paths, absolute paths or directory traversal.
func extractArchive(data []byte, dest string) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("plugin archive: gzip: %w", err)
	}
	defer gzipReader.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	seen := map[string]bool{}
	reader := tar.NewReader(gzipReader)
	fileCount := 0
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("plugin archive: %w", err)
		}
		fileCount++
		if fileCount > maxArchiveFiles {
			return fmt.Errorf("plugin archive contains more than %d entries", maxArchiveFiles)
		}
		relative, err := safeArchivePath(header.Name)
		if err != nil {
			return err
		}
		if relative == "." {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("plugin archive contains an invalid root entry")
		}
		if seen[relative] {
			return fmt.Errorf("plugin archive contains %q more than once", relative)
		}
		seen[relative] = true
		target := filepath.Join(dest, relative)
		if err := ensureInside(dest, target); err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 {
				return fmt.Errorf("plugin archive: %s has a negative size", header.Name)
			}
			total += header.Size
			if total > maxExtractedBytes {
				return fmt.Errorf("plugin archive expands beyond %d bytes", maxExtractedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("plugin archive: %s: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("plugin archive: links are not allowed (%s)", header.Name)
		default:
			return fmt.Errorf("plugin archive: unsupported entry type %d (%s)", header.Typeflag, header.Name)
		}
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("plugin archive contains a NUL byte in a path")
	}
	path := filepath.FromSlash(name)
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("plugin archive contains an absolute path %q", name)
	}
	path = filepath.Clean(path)
	if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin archive path %q escapes the install directory", name)
	}
	return path, nil
}

func ensureInside(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("plugin path %s escapes %s", target, root)
	}
	return nil
}

// pluginRoot accepts either a manifest at the archive root or one
// single top-level directory that contains the manifest.
func pluginRoot(extracted string) (string, error) {
	if manifestExists(extracted) {
		return extracted, nil
	}
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		candidate := filepath.Join(extracted, entries[0].Name())
		if manifestExists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("plugin archive must contain plugin.json at its root or inside one top-level directory")
}

// treeDigest hashes every directory, file path, executable bit and file
// body in a plugin tree. It is the lockfile integrity value.
func treeDigest(root string) (string, error) {
	digest := sha256.New()
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			return fmt.Errorf("plugin contains a symlink: %s", relative)
		case entry.IsDir():
			fmt.Fprintf(digest, "d\x00%s\x00", relative)
		case info.Mode().IsRegular():
			access := "f0"
			if info.Mode()&0o111 != 0 {
				access = "f1"
			}
			fmt.Fprintf(digest, "%s\x00%s\x00%d\x00", access, relative, info.Size())
			total += info.Size()
			if total > maxExtractedBytes {
				return fmt.Errorf("plugin tree exceeds %d bytes", maxExtractedBytes)
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(digest, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("plugin contains an unsupported file type: %s", relative)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
