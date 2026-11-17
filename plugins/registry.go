package plugins

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// RegistrySchemaVersion is the plugin registry document version.
	RegistrySchemaVersion = 1
	// RegistrySignatureAlgorithm is the only registry signature
	// algorithm accepted by this build.
	RegistrySignatureAlgorithm = "ed25519"

	maxIndexBytes      = 4 << 20
	maxIndexPlugins    = 256
	maxIndexVersions   = 128
	maxResolvedPlugins = 64
	resolveAttempts    = 20000
)

// Index is a plugin registry document. Artifact URLs are resolved
// relative to the registry document.
type Index struct {
	SchemaVersion int                      `json:"schema_version"`
	Publisher     string                   `json:"publisher,omitempty"`
	Plugins       map[string]IndexedPlugin `json:"plugins"`
	Signature     *IndexSignature          `json:"signature,omitempty"`
}

// IndexSignature authenticates the canonical registry payload. Artifact
// SHA-256 values are part of the signed payload, so the signature binds
// both the selected version metadata and the archive bytes it names.
type IndexSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

// IndexedPlugin describes all published versions of one plugin.
type IndexedPlugin struct {
	Description string                    `json:"description,omitempty"`
	Versions    map[string]IndexedVersion `json:"versions"`
}

// IndexedVersion is one immutable plugin release.
type IndexedVersion struct {
	Description  string            `json:"description,omitempty"`
	Artifact     Artifact          `json:"artifact"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

// Artifact points at one .tar.gz plugin archive and its SHA-256 digest.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Catalog is a loaded registry and the base needed to resolve its
// relative artifact paths.
type Catalog struct {
	Source      string
	Publisher   string
	SignerKeyID string
	Verified    bool
	index       Index
	baseURL     *url.URL
	baseDir     string
}

// TrustPolicy supplies the publisher keys used to authenticate signed
// registries. RequireSignature is set by callers that must not accept
// an unsigned index.
type TrustPolicy struct {
	Publishers       map[string][]ed25519.PublicKey
	RequireSignature bool
}

// ResolvedPackage is one version selected for installation.
type ResolvedPackage struct {
	Name         string
	Version      string
	Artifact     Artifact
	Dependencies map[string]string
}

type dependencyRequirement struct {
	constraint Constraint
	requiredBy string
}

// OpenCatalog loads a local path or an HTTP(S) registry document.
// Plain HTTP is accepted only for loopback development servers.
func OpenCatalog(source string) (*Catalog, error) {
	return OpenCatalogWithPolicy(source, TrustPolicy{})
}

// OpenCatalogWithPolicy is OpenCatalog with registry signature
// verification and an optional requirement that a signature exist.
func OpenCatalogWithPolicy(source string, policy TrustPolicy) (*Catalog, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("plugin registry: source is empty")
	}
	catalog := &Catalog{Source: source}
	var data []byte
	var err error
	if isHTTPURL(source) {
		endpoint, parseErr := url.Parse(source)
		if parseErr != nil {
			return nil, fmt.Errorf("plugin registry: %w", parseErr)
		}
		if err := validateRemoteURL(endpoint); err != nil {
			return nil, fmt.Errorf("plugin registry: %w", err)
		}
		catalog.baseURL = endpoint
		data, err = fetchLimited(endpoint.String(), maxIndexBytes)
	} else {
		abs, absErr := filepath.Abs(source)
		if absErr != nil {
			return nil, fmt.Errorf("plugin registry: %w", absErr)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return nil, fmt.Errorf("plugin registry: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("plugin registry: %s is not a regular file", abs)
		}
		if info.Size() > maxIndexBytes {
			return nil, fmt.Errorf("plugin registry: index exceeds %d bytes", maxIndexBytes)
		}
		catalog.Source = abs
		catalog.baseDir = filepath.Dir(abs)
		data, err = os.ReadFile(abs)
	}
	if err != nil {
		return nil, fmt.Errorf("plugin registry: %w", err)
	}
	index, err := ParseIndexDocument(data)
	if err != nil {
		return nil, fmt.Errorf("plugin registry: %w", err)
	}
	if index.Signature == nil {
		if policy.RequireSignature {
			return nil, fmt.Errorf("plugin registry: registry is unsigned")
		}
	} else {
		if err := VerifyIndexSignature(&index, policy); err != nil {
			return nil, fmt.Errorf("plugin registry: %w", err)
		}
		catalog.Publisher = index.Publisher
		catalog.SignerKeyID = index.Signature.KeyID
		catalog.Verified = true
	}
	catalog.index = index
	return catalog, nil
}

// ParseIndexDocument decodes and validates exactly one registry JSON
// value. Signed documents are structurally validated here; authenticity
// is checked when a trust policy is applied.
func ParseIndexDocument(data []byte) (Index, error) {
	var index Index
	if err := decodeStrictJSON(data, &index); err != nil {
		return Index{}, err
	}
	if err := validateIndex(&index); err != nil {
		return Index{}, err
	}
	return index, nil
}

// MarshalIndexDocument serializes a validated registry document.
func MarshalIndexDocument(index Index) ([]byte, error) {
	if err := validateIndex(&index); err != nil {
		return nil, err
	}
	return marshalIndent(index)
}

func validateIndex(index *Index) error {
	if err := validateIndexPayload(index); err != nil {
		return err
	}
	if index.Signature == nil {
		if index.Publisher != "" {
			return fmt.Errorf("publisher requires a signature")
		}
		return nil
	}
	if err := validatePublisherName(index.Publisher); err != nil {
		return err
	}
	if index.Signature.Algorithm != RegistrySignatureAlgorithm {
		return fmt.Errorf("unsupported signature algorithm %q", index.Signature.Algorithm)
	}
	if err := validateKeyID(index.Signature.KeyID); err != nil {
		return fmt.Errorf("signature.key_id: %w", err)
	}
	if _, err := decodeSignature(index.Signature.Value); err != nil {
		return err
	}
	return nil
}

func validateIndexPayload(index *Index) error {
	if index.SchemaVersion != RegistrySchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", index.SchemaVersion, RegistrySchemaVersion)
	}
	if len(index.Plugins) == 0 {
		return fmt.Errorf("registry has no plugins")
	}
	if len(index.Plugins) > maxIndexPlugins {
		return fmt.Errorf("registry has too many plugins (%d > %d)", len(index.Plugins), maxIndexPlugins)
	}
	for name, plugin := range index.Plugins {
		if !pluginNameRE.MatchString(name) {
			return fmt.Errorf("plugin name %q is invalid", name)
		}
		if len(name) > 128 {
			return fmt.Errorf("plugin name %q is too long", name)
		}
		if len(plugin.Versions) == 0 {
			return fmt.Errorf("plugin %q has no versions", name)
		}
		if len(plugin.Versions) > maxIndexVersions {
			return fmt.Errorf("plugin %q has too many versions", name)
		}
		for version, item := range plugin.Versions {
			if _, err := ParseVersion(version); err != nil {
				return fmt.Errorf("plugin %q: %w", name, err)
			}
			if strings.TrimSpace(item.Artifact.URL) == "" {
				return fmt.Errorf("plugin %q %s: artifact.url is required", name, version)
			}
			if len(item.Artifact.URL) > 4096 {
				return fmt.Errorf("plugin %q %s: artifact.url is too long", name, version)
			}
			digest, err := normalizeSHA256(item.Artifact.SHA256)
			if err != nil {
				return fmt.Errorf("plugin %q %s: %w", name, version, err)
			}
			plugin.Versions[version] = IndexedVersion{
				Description:  item.Description,
				Artifact:     Artifact{URL: item.Artifact.URL, SHA256: digest},
				Dependencies: item.Dependencies,
			}
			for dependency, constraint := range item.Dependencies {
				if !pluginNameRE.MatchString(dependency) {
					return fmt.Errorf("plugin %q %s: dependency name %q is invalid", name, version, dependency)
				}
				if dependency == name {
					return fmt.Errorf("plugin %q %s cannot depend on itself", name, version)
				}
				if strings.TrimSpace(constraint) == "" {
					return fmt.Errorf("plugin %q %s: dependency %s has an empty constraint", name, version, dependency)
				}
				if _, err := ParseConstraint(constraint); err != nil {
					return fmt.Errorf("plugin %q %s: dependency %s: %w", name, version, dependency, err)
				}
			}
		}
		index.Plugins[name] = plugin
	}
	return nil
}

// Resolve chooses the highest satisfying version of the requested
// plugin and all transitive dependencies. It backtracks when different
// releases impose incompatible dependency constraints.
func (c *Catalog) Resolve(name, constraintValue string) ([]ResolvedPackage, error) {
	rootConstraint, err := ParseConstraint(constraintValue)
	if err != nil {
		return nil, err
	}
	if _, ok := c.index.Plugins[name]; !ok {
		return nil, fmt.Errorf("plugin %q is not in the registry", name)
	}

	requirements := map[string][]dependencyRequirement{
		name: {{constraint: rootConstraint, requiredBy: "request"}},
	}
	selected := map[string]string{}
	attempts := 0
	lastProblem := ""

	var search func() (map[string]string, bool)
	search = func() (map[string]string, bool) {
		attempts++
		if attempts > resolveAttempts {
			lastProblem = "dependency resolution exceeded its search budget"
			return nil, false
		}

		for packageName, selectedVersion := range selected {
			version, _ := ParseVersion(selectedVersion)
			for _, req := range requirements[packageName] {
				if !req.constraint.Match(version) {
					lastProblem = fmt.Sprintf("%s %s does not satisfy %s required by %s",
						packageName, selectedVersion, req.constraint, req.requiredBy)
					return nil, false
				}
			}
		}

		var unresolved []string
		for packageName := range requirements {
			if _, ok := selected[packageName]; !ok {
				unresolved = append(unresolved, packageName)
			}
		}
		if len(unresolved) == 0 {
			out := make(map[string]string, len(selected))
			for packageName, version := range selected {
				out[packageName] = version
			}
			return out, true
		}
		sort.Strings(unresolved)
		current := unresolved[0]
		indexed, ok := c.index.Plugins[current]
		if !ok {
			lastProblem = fmt.Sprintf("dependency %q is not in the registry", current)
			return nil, false
		}
		versions := make([]string, 0, len(indexed.Versions))
		for version := range indexed.Versions {
			versions = append(versions, version)
		}
		versions, err := sortVersionsDescending(versions)
		if err != nil {
			lastProblem = err.Error()
			return nil, false
		}
		matched := false
		for _, versionText := range versions {
			version, _ := ParseVersion(versionText)
			matches := true
			for _, req := range requirements[current] {
				if !req.constraint.Match(version) {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			matched = true
			item := indexed.Versions[versionText]
			var addedDependencies []string
			missingDependency := ""
			for dependency, dependencyConstraint := range item.Dependencies {
				if _, exists := c.index.Plugins[dependency]; !exists {
					missingDependency = dependency
					break
				}
				constraint, _ := ParseConstraint(dependencyConstraint)
				requirements[dependency] = append(requirements[dependency],
					dependencyRequirement{constraint: constraint, requiredBy: current + " " + versionText})
				addedDependencies = append(addedDependencies, dependency)
			}
			if missingDependency != "" {
				lastProblem = fmt.Sprintf("%s %s requires missing plugin %q",
					current, versionText, missingDependency)
				for _, dependency := range addedDependencies {
					removeRequirement(requirements, dependency, current+" "+versionText)
				}
				continue
			}
			selected[current] = versionText
			if len(selected) > maxResolvedPlugins {
				lastProblem = fmt.Sprintf("dependency graph exceeds %d plugins", maxResolvedPlugins)
				delete(selected, current)
				for _, dependency := range addedDependencies {
					removeRequirement(requirements, dependency, current+" "+versionText)
				}
				continue
			}
			if result, found := search(); found {
				return result, true
			}
			delete(selected, current)
			for _, dependency := range addedDependencies {
				removeRequirement(requirements, dependency, current+" "+versionText)
			}
		}
		if !matched {
			lastProblem = fmt.Sprintf("no version of %s satisfies all constraints", current)
		}
		return nil, false
	}

	resolved, ok := search()
	if !ok {
		if lastProblem == "" {
			lastProblem = "no compatible dependency set"
		}
		return nil, fmt.Errorf("plugin registry: resolve %s@%s: %s", name, rootConstraint, lastProblem)
	}
	names := make([]string, 0, len(resolved))
	for packageName := range resolved {
		names = append(names, packageName)
	}
	sort.Strings(names)
	out := make([]ResolvedPackage, 0, len(names))
	for _, packageName := range names {
		version := resolved[packageName]
		item := c.index.Plugins[packageName].Versions[version]
		out = append(out, ResolvedPackage{
			Name:         packageName,
			Version:      version,
			Artifact:     item.Artifact,
			Dependencies: cloneStringMap(item.Dependencies),
		})
	}
	return out, nil
}

func removeRequirement(requirements map[string][]dependencyRequirement, dependency, requiredBy string) {
	items := requirements[dependency]
	filtered := items[:0]
	for _, item := range items {
		if item.requiredBy != requiredBy {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		delete(requirements, dependency)
	} else {
		requirements[dependency] = filtered
	}
}

// ArtifactURL resolves an index artifact URL against the registry
// location and rejects insecure remote transports.
func (c *Catalog) ArtifactURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("artifact URL is empty")
	}
	if c.baseURL != nil {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if parsed.IsAbs() {
			if parsed.Scheme == "file" || parsed.Host == "" {
				return "", fmt.Errorf("remote registries cannot reference local artifact %q", raw)
			}
			if err := validateRemoteURL(parsed); err != nil {
				return "", err
			}
			return parsed.String(), nil
		}
		resolved := c.baseURL.ResolveReference(parsed)
		if err := validateRemoteURL(resolved); err != nil {
			return "", err
		}
		return resolved.String(), nil
	}
	if isHTTPURL(raw) {
		resolved, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if err := validateRemoteURL(resolved); err != nil {
			return "", err
		}
		return resolved.String(), nil
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(c.baseDir, raw)
	}
	return filepath.Clean(raw), nil
}

// FetchArtifact downloads or reads one artifact, enforcing its digest.
func (c *Catalog) FetchArtifact(artifact Artifact) ([]byte, error) {
	location, err := c.ArtifactURL(artifact.URL)
	if err != nil {
		return nil, err
	}
	var data []byte
	if isHTTPURL(location) {
		data, err = fetchLimited(location, maxArtifactBytes)
	} else {
		info, statErr := os.Stat(location)
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", location)
		}
		if info.Size() > maxArtifactBytes {
			return nil, fmt.Errorf("artifact exceeds %d bytes", maxArtifactBytes)
		}
		data, err = os.ReadFile(location)
	}
	if err != nil {
		return nil, err
	}
	want, err := normalizeSHA256(artifact.SHA256)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("artifact SHA-256 mismatch: got %s, want %s",
			hex.EncodeToString(got[:]), want)
	}
	return data, nil
}

func fetchLimited(location string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if err := validateRemoteURL(parsed); err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return validateRemoteURL(req.URL)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", parsed, response.Status)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	reader := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func validateRemoteURL(parsed *url.URL) error {
	if parsed.Host == "" {
		return fmt.Errorf("plugin registry URL has no host")
	}
	if parsed.User != nil {
		return fmt.Errorf("plugin registry URLs must not embed credentials")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("plain HTTP is allowed only for loopback plugin registries")
	default:
		return fmt.Errorf("unsupported plugin registry URL scheme %q", parsed.Scheme)
	}
}

func isHTTPURL(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// IsRemoteRegistry reports whether a registry source uses HTTP(S).
func IsRemoteRegistry(source string) bool {
	return isHTTPURL(strings.TrimSpace(source))
}

func normalizeSHA256(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("sha256 must contain %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("sha256: %w", err)
	}
	return value, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("the file contains more than one JSON value")
		}
		return err
	}
	return nil
}

func marshalIndent(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
