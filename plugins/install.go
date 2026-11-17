package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// LockSchemaVersion is the installed-plugin lockfile version.
	LockSchemaVersion = 2
	// LockFileName is the lockfile inside the managed plugin directory.
	LockFileName = "plugins.lock.json"
	maxLockBytes = 4 << 20
)

// LockFile records every managed plugin and its immutable tree digest.
type LockFile struct {
	SchemaVersion int                     `json:"schema_version"`
	Plugins       map[string]LockedPlugin `json:"plugins"`
}

// LockedPlugin is one installed release.
type LockedPlugin struct {
	Version        string            `json:"version"`
	Digest         string            `json:"digest"`
	ArtifactSHA256 string            `json:"artifact_sha256"`
	Source         string            `json:"source"`
	Publisher      string            `json:"publisher,omitempty"`
	SignerKeyID    string            `json:"signer_key_id,omitempty"`
	Dependencies   map[string]string `json:"dependencies,omitempty"`
}

// InstalledRecord is returned after a successful install.
type InstalledRecord struct {
	Name         string
	Version      string
	Digest       string
	Dependencies map[string]string
}

// VerificationIssue describes one lock/tree mismatch.
type VerificationIssue struct {
	Plugin  string
	Problem string
}

func (i VerificationIssue) String() string {
	if i.Plugin == "" {
		return i.Problem
	}
	return fmt.Sprintf("%s: %s", i.Plugin, i.Problem)
}

// InstalledInfo is one lock entry with its current on-disk health.
type InstalledInfo struct {
	Name         string
	Version      string
	Digest       string
	Publisher    string
	SignerKeyID  string
	Dependencies map[string]string
	Healthy      bool
	Problem      string
}

// InstallDir returns the user's managed plugin directory.
func InstallDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".harness", "plugins")
	}
	return filepath.Join(home, ".harness", "plugins")
}

// Install resolves a plugin and its dependencies, extracts and
// verifies every artifact, then atomically replaces the managed
// directories and writes the lockfile.
func Install(root string, catalog *Catalog, name, constraint string) ([]InstalledRecord, error) {
	if catalog == nil {
		return nil, fmt.Errorf("plugin install: catalog is nil")
	}
	root, err := prepareRoot(root)
	if err != nil {
		return nil, err
	}
	lock, err := ReadLock(root)
	if err != nil {
		return nil, err
	}
	resolved, err := catalog.Resolve(name, constraint)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]string, len(resolved))
	for _, item := range resolved {
		selected[item.Name] = item.Version
	}

	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	records := make([]InstalledRecord, 0, len(resolved))
	for _, item := range resolved {
		data, err := catalog.FetchArtifact(item.Artifact)
		if err != nil {
			return nil, fmt.Errorf("plugin %s %s: %w", item.Name, item.Version, err)
		}
		raw := filepath.Join(stage, ".raw-"+item.Name)
		if err := extractArchive(data, raw); err != nil {
			return nil, fmt.Errorf("plugin %s %s: %w", item.Name, item.Version, err)
		}
		sourceRoot, err := pluginRoot(raw)
		if err != nil {
			return nil, fmt.Errorf("plugin %s %s: %w", item.Name, item.Version, err)
		}
		staged := filepath.Join(stage, item.Name)
		if err := os.Rename(sourceRoot, staged); err != nil {
			return nil, err
		}
		candidate, err := readCandidate(staged)
		if err != nil {
			return nil, err
		}
		if candidate.Legacy || candidate.Manifest.Name != item.Name {
			return nil, fmt.Errorf("plugin %s: archive manifest name is %q", item.Name, candidate.Manifest.Name)
		}
		if candidate.Manifest.Version != item.Version {
			return nil, fmt.Errorf("plugin %s: archive manifest version is %q, want %s",
				item.Name, candidate.Manifest.Version, item.Version)
		}
		if _, err := ParseVersion(candidate.Manifest.Version); err != nil {
			return nil, fmt.Errorf("plugin %s: %w", item.Name, err)
		}
		for dependency, constraintValue := range item.Dependencies {
			installedVersion, ok := selected[dependency]
			if !ok {
				return nil, fmt.Errorf("plugin %s: dependency %s was not resolved", item.Name, dependency)
			}
			constraint, _ := ParseConstraint(constraintValue)
			version, _ := ParseVersion(installedVersion)
			if !constraint.Match(version) {
				return nil, fmt.Errorf("plugin %s depends on %s %s but resolution selected %s",
					item.Name, dependency, constraintValue, installedVersion)
			}
		}
		digest, err := treeDigest(staged)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", item.Name, err)
		}
		records = append(records, InstalledRecord{
			Name:         item.Name,
			Version:      item.Version,
			Digest:       digest,
			Dependencies: cloneStringMap(item.Dependencies),
		})
	}

	changes, err := commitStaged(root, stage, records)
	if err != nil {
		return nil, err
	}
	next := cloneLock(lock)
	for _, record := range records {
		item := findResolved(resolved, record.Name)
		next.Plugins[record.Name] = LockedPlugin{
			Version:        record.Version,
			Digest:         record.Digest,
			ArtifactSHA256: item.Artifact.SHA256,
			Source:         catalog.Source,
			Publisher:      catalog.Publisher,
			SignerKeyID:    catalog.SignerKeyID,
			Dependencies:   cloneStringMap(record.Dependencies),
		}
	}
	if err := WriteLock(root, next); err != nil {
		rollbackChanges(root, changes)
		return nil, err
	}
	return records, nil
}

type installedChange struct {
	Name   string
	Backup string
	HadOld bool
}

func commitStaged(root, stage string, records []InstalledRecord) ([]installedChange, error) {
	backupRoot := filepath.Join(stage, ".backup")
	changes := make([]installedChange, 0, len(records))
	rollback := func() {
		rollbackChanges(root, changes)
	}
	sorted := append([]InstalledRecord{}, records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, record := range sorted {
		target := filepath.Join(root, record.Name)
		if err := ensureInside(root, target); err != nil {
			rollback()
			return nil, err
		}
		change := installedChange{Name: record.Name}
		if info, err := os.Lstat(target); err == nil {
			if !info.IsDir() {
				rollback()
				return nil, fmt.Errorf("plugin install: %s exists and is not a directory", target)
			}
			if err := os.MkdirAll(backupRoot, 0o755); err != nil {
				rollback()
				return nil, err
			}
			change.Backup = filepath.Join(backupRoot, record.Name)
			if err := os.Rename(target, change.Backup); err != nil {
				rollback()
				return nil, err
			}
			change.HadOld = true
		} else if !os.IsNotExist(err) {
			rollback()
			return nil, err
		}
		if err := os.Rename(filepath.Join(stage, record.Name), target); err != nil {
			if change.HadOld {
				_ = os.Rename(change.Backup, target)
			}
			rollback()
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func rollbackChanges(root string, changes []installedChange) {
	for i := len(changes) - 1; i >= 0; i-- {
		change := changes[i]
		target := filepath.Join(root, change.Name)
		_ = os.RemoveAll(target)
		if change.HadOld {
			_ = os.Rename(change.Backup, target)
		}
	}
}

func findResolved(items []ResolvedPackage, name string) ResolvedPackage {
	for _, item := range items {
		if item.Name == name {
			return item
		}
	}
	return ResolvedPackage{}
}

func prepareRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("plugin install: root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return real, nil
}

// ReadLock loads the managed-plugin lockfile. A missing file is an
// empty lock, not an error.
func ReadLock(root string) (*LockFile, error) {
	path := filepath.Join(root, LockFileName)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &LockFile{SchemaVersion: LockSchemaVersion, Plugins: map[string]LockedPlugin{}}, nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("plugin lock: %s is not a regular file", path)
	}
	if info.Size() > maxLockBytes {
		return nil, fmt.Errorf("plugin lock exceeds %d bytes", maxLockBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock LockFile
	if err := decodeStrictJSON(data, &lock); err != nil {
		return nil, fmt.Errorf("plugin lock: %s: %w", path, err)
	}
	if lock.SchemaVersion != 1 && lock.SchemaVersion != LockSchemaVersion {
		return nil, fmt.Errorf("plugin lock: unsupported schema_version %d", lock.SchemaVersion)
	}
	if lock.Plugins == nil {
		lock.Plugins = map[string]LockedPlugin{}
	}
	for name, item := range lock.Plugins {
		if !pluginNameRE.MatchString(name) {
			return nil, fmt.Errorf("plugin lock: invalid name %q", name)
		}
		if _, err := ParseVersion(item.Version); err != nil {
			return nil, fmt.Errorf("plugin lock: %s: %w", name, err)
		}
		if _, err := normalizeSHA256(item.Digest); err != nil {
			return nil, fmt.Errorf("plugin lock: %s digest: %w", name, err)
		}
		if _, err := normalizeSHA256(item.ArtifactSHA256); err != nil {
			return nil, fmt.Errorf("plugin lock: %s artifact digest: %w", name, err)
		}
		if (item.Publisher == "") != (item.SignerKeyID == "") {
			return nil, fmt.Errorf(
				"plugin lock: %s publisher and signer_key_id must appear together", name)
		}
		if item.Publisher != "" {
			if err := validatePublisherName(item.Publisher); err != nil {
				return nil, fmt.Errorf("plugin lock: %s: %w", name, err)
			}
			if err := validateKeyID(item.SignerKeyID); err != nil {
				return nil, fmt.Errorf("plugin lock: %s signer_key_id: %w", name, err)
			}
		}
		for dependency, constraintValue := range item.Dependencies {
			if !pluginNameRE.MatchString(dependency) {
				return nil, fmt.Errorf("plugin lock: %s dependency %q is invalid", name, dependency)
			}
			if strings.TrimSpace(constraintValue) == "" {
				return nil, fmt.Errorf("plugin lock: %s dependency %s has an empty constraint", name, dependency)
			}
			if _, err := ParseConstraint(constraintValue); err != nil {
				return nil, fmt.Errorf("plugin lock: %s dependency %s: %w", name, dependency, err)
			}
		}
	}
	return &lock, nil
}

// WriteLock atomically replaces the managed-plugin lockfile.
func WriteLock(root string, lock *LockFile) error {
	if lock == nil {
		return fmt.Errorf("plugin lock: nil")
	}
	if lock.Plugins == nil {
		lock.Plugins = map[string]LockedPlugin{}
	}
	lock.SchemaVersion = LockSchemaVersion
	data, err := marshalIndent(lock)
	if err != nil {
		return err
	}
	path := filepath.Join(root, LockFileName)
	temp, err := os.CreateTemp(root, ".plugins.lock-*")
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

func cloneLock(lock *LockFile) *LockFile {
	next := &LockFile{SchemaVersion: LockSchemaVersion, Plugins: map[string]LockedPlugin{}}
	if lock == nil {
		return next
	}
	for name, item := range lock.Plugins {
		item.Dependencies = cloneStringMap(item.Dependencies)
		next.Plugins[name] = item
	}
	return next
}

// ListInstalled reports lock entries and whether each tree currently
// matches its digest.
func ListInstalled(root string) ([]InstalledInfo, error) {
	lock, err := ReadLock(root)
	if err != nil {
		return nil, err
	}
	names := sortedLockNames(lock)
	out := make([]InstalledInfo, 0, len(names))
	for _, name := range names {
		item := lock.Plugins[name]
		info := InstalledInfo{
			Name:         name,
			Version:      item.Version,
			Digest:       item.Digest,
			Publisher:    item.Publisher,
			SignerKeyID:  item.SignerKeyID,
			Dependencies: cloneStringMap(item.Dependencies),
		}
		problem := verifyPackage(root, name, item, lock)
		if problem == "" {
			info.Healthy = true
		} else {
			info.Problem = problem
		}
		out = append(out, info)
	}
	return out, nil
}

// Verify checks every managed plugin and reports lockfile or tree
// mismatches, missing dependencies and unmanaged manifest plugins.
func Verify(root string) ([]VerificationIssue, error) {
	lock, err := ReadLock(root)
	if err != nil {
		return nil, err
	}
	var issues []VerificationIssue
	for _, name := range sortedLockNames(lock) {
		if problem := verifyPackage(root, name, lock.Plugins[name], lock); problem != "" {
			issues = append(issues, VerificationIssue{Plugin: name, Problem: problem})
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return issues, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, managed := lock.Plugins[entry.Name()]; managed {
			continue
		}
		if manifestExists(filepath.Join(root, entry.Name())) {
			issues = append(issues, VerificationIssue{
				Plugin:  entry.Name(),
				Problem: "plugin directory exists but is not recorded in the lockfile",
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Plugin == issues[j].Plugin {
			return issues[i].Problem < issues[j].Problem
		}
		return issues[i].Plugin < issues[j].Plugin
	})
	return issues, nil
}

func verifyPackage(root, name string, item LockedPlugin, lock *LockFile) string {
	dir := filepath.Join(root, name)
	candidate, err := readCandidate(dir)
	if err != nil {
		return err.Error()
	}
	if candidate.Legacy {
		return "installed entry is not a manifest plugin"
	}
	if candidate.Manifest.Name != name {
		return fmt.Sprintf("manifest name is %q", candidate.Manifest.Name)
	}
	if candidate.Manifest.Version != item.Version {
		return fmt.Sprintf("manifest version is %q, lockfile has %s", candidate.Manifest.Version, item.Version)
	}
	digest, err := treeDigest(dir)
	if err != nil {
		return err.Error()
	}
	if digest != item.Digest {
		return fmt.Sprintf("tree digest is %s, lockfile has %s", digest, item.Digest)
	}
	for dependency, constraintValue := range item.Dependencies {
		locked, ok := lock.Plugins[dependency]
		if !ok {
			return fmt.Sprintf("dependency %s is not installed", dependency)
		}
		constraint, _ := ParseConstraint(constraintValue)
		version, _ := ParseVersion(locked.Version)
		if !constraint.Match(version) {
			return fmt.Sprintf("dependency %s %s does not satisfy %s", dependency, locked.Version, constraintValue)
		}
	}
	return ""
}

// Remove uninstalls one managed plugin. It refuses when another
// installed plugin declares a dependency on it.
func Remove(root, name string) error {
	if !pluginNameRE.MatchString(name) {
		return fmt.Errorf("plugin remove: invalid name %q", name)
	}
	root, err := prepareRoot(root)
	if err != nil {
		return err
	}
	lock, err := ReadLock(root)
	if err != nil {
		return err
	}
	if _, ok := lock.Plugins[name]; !ok {
		return fmt.Errorf("plugin %q is not installed by the lockfile", name)
	}
	var dependents []string
	for other, item := range lock.Plugins {
		if other == name {
			continue
		}
		if _, ok := item.Dependencies[name]; ok {
			dependents = append(dependents, other)
		}
	}
	if len(dependents) > 0 {
		sort.Strings(dependents)
		return fmt.Errorf("plugin %q is required by %s", name, strings.Join(dependents, ", "))
	}
	target := filepath.Join(root, name)
	trash, err := os.MkdirTemp(root, ".remove-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(trash)
	hadTarget := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, filepath.Join(trash, name)); err != nil {
			return err
		}
		hadTarget = true
	} else if !os.IsNotExist(err) {
		return err
	}
	delete(lock.Plugins, name)
	if err := WriteLock(root, lock); err != nil {
		if hadTarget {
			_ = os.Rename(filepath.Join(trash, name), target)
		}
		return err
	}
	return nil
}

func sortedLockNames(lock *LockFile) []string {
	names := make([]string, 0, len(lock.Plugins))
	for name := range lock.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
