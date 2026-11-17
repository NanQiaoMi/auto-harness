package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"testing"
)

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type tarEntry struct {
	Name     string
	Type     byte
	Mode     int64
	Linkname string
	Body     []byte
}

func tarGZ(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.Name,
			Typeflag: entry.Type,
			Mode:     entry.Mode,
			Size:     int64(len(entry.Body)),
			Linkname: entry.Linkname,
		}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.Body) > 0 {
			if _, err := tarWriter.Write(entry.Body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func pluginArchive(t *testing.T, name, version, prefix string) []byte {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "schema_version": 1,
  "name": %q,
  "version": %q,
  "command": ["python3", "plugin.py"]
}
`, name, version)
	files := map[string]struct {
		body []byte
		mode int64
	}{
		"plugin.json": {body: []byte(manifest), mode: 0o644},
		"plugin.py":   {body: []byte("#!/usr/bin/env python3\n"), mode: 0o755},
		"README.md":   {body: []byte("# " + name + "\n"), mode: 0o644},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]tarEntry, 0, len(names))
	for _, name := range names {
		file := files[name]
		entries = append(entries, tarEntry{
			Name: prefix + name,
			Type: tar.TypeReg,
			Mode: file.mode,
			Body: file.body,
		})
	}
	return tarGZ(t, entries)
}
