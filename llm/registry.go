package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// ProviderConfig describes how to reach one provider endpoint.
type ProviderConfig struct {
	// Name identifies the provider ("openai", "anthropic").
	Name string `json:"name"`
	// BaseURL is the API root. For OpenAI-compatible providers it is the
	// ".../v1" prefix; for Anthropic it is the API root.
	BaseURL string `json:"base_url"`
	// APIKey overrides the environment-provided credential when set.
	APIKey string `json:"api_key,omitempty"`
	// Protocol selects the wire format: "openai" or "anthropic".
	Protocol string `json:"protocol"`
}

// Registry maps model IDs to providers and builds providers on demand.
// It is the multi-provider routing layer: one harness can talk to several
// endpoints and switch models across them without losing configuration.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*ProviderConfig
	models    map[string]Model
	// headers are applied to every provider this registry builds.
	headers map[string]string
	// factory builds a live provider for a config. Tests replace it.
	factory func(ProviderConfig) (Provider, error)
	// protocols are wire protocols registered by extensions.
	protocols map[string]func(ProviderConfig) (Provider, error)
}

// NewRegistry returns a registry seeded with the built-in catalog.
func NewRegistry() *Registry {
	r := &Registry{
		providers: map[string]*ProviderConfig{},
		models:    map[string]Model{},
		headers:   map[string]string{},
		factory:   defaultFactory,
		protocols: map[string]func(ProviderConfig) (Provider, error){},
	}
	for _, m := range Catalog {
		r.models[m.ID] = m
	}
	return r
}

// defaultFactory builds a provider from a config.
func defaultFactory(c ProviderConfig) (Provider, error) {
	switch c.Protocol {
	case "", "openai":
		return NewOpenAIProvider(c.APIKey, c.BaseURL), nil
	case "anthropic":
		return NewAnthropicProvider(c.APIKey, c.BaseURL), nil
	case "gemini":
		return NewGeminiProvider(c.APIKey, c.BaseURL), nil
	default:
		return nil, fmt.Errorf("llm: unknown protocol %q (want openai, anthropic or gemini)", c.Protocol)
	}
}

// RegisterProvider adds or replaces a provider configuration.
func (r *Registry) RegisterProvider(c ProviderConfig) error {
	if c.Name == "" {
		return fmt.Errorf("llm: provider needs a name")
	}
	if c.Protocol == "" {
		c.Protocol = "openai"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := c
	r.providers[c.Name] = &cp
	return nil
}

// RegisterProtocol adds a wire protocol an extension can serve.
//
// Without this the registry only knows the protocols compiled into it,
// so a new API shape means patching the kernel. With it an extension
// registers a builder for a protocol name and models that declare that
// protocol route to it.
//
// The builder is called once per provider the registry builds; it
// receives the resolved config (base URL, key, name) and returns a
// Provider. The returned function removes the registration, which is
// what hot reload needs.
func (r *Registry) RegisterProtocol(name string, build func(ProviderConfig) (Provider, error)) (unregister func(), err error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("llm: a protocol needs a name")
	}
	if build == nil {
		return nil, fmt.Errorf("llm: protocol %q has no builder", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.protocols == nil {
		r.protocols = map[string]func(ProviderConfig) (Provider, error){}
	}
	if _, exists := r.protocols[name]; exists {
		return nil, fmt.Errorf("llm: protocol %q is already registered", name)
	}
	r.protocols[name] = build
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.protocols, name)
	}, nil
}

// Protocols lists the wire protocols the registry can build.
func (r *Registry) Protocols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.protocols)+3)
	names = append(names, "openai", "anthropic", "gemini")
	for name := range r.protocols {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// protocolBuilder returns the builder for a protocol, if one is
// registered by an extension.
func (r *Registry) protocolBuilder(name string) (func(ProviderConfig) (Provider, error), bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	build, ok := r.protocols[name]
	return build, ok
}

// Provider returns the config for a provider name.
func (r *Registry) Provider(name string) (*ProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.providers[name]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// RegisterModel adds or replaces model metadata.
func (r *Registry) RegisterModel(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[m.ID] = m
}

// Model returns metadata for a model ID.
func (r *Registry) Model(id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[id]
	return m, ok
}

// Models lists registered models sorted by ID.
func (r *Registry) Models() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Resolve decides which provider serves a model ID. An explicit provider
// wins; otherwise the registry's model metadata decides; otherwise the
// model name is matched heuristically.
func (r *Registry) Resolve(modelID, preferredProvider string) (ProviderConfig, error) {
	r.mu.RLock()
	providers := make(map[string]ProviderConfig, len(r.providers))
	for k, v := range r.providers {
		providers[k] = *v
	}
	model, hasModel := r.models[modelID]
	r.mu.RUnlock()

	if preferredProvider != "" {
		if c, ok := providers[preferredProvider]; ok {
			return c, nil
		}
		return ProviderConfig{}, fmt.Errorf("llm: unknown provider %q", preferredProvider)
	}
	if hasModel {
		// The model's own provider wins when it is registered.
		if c, ok := providers[model.Provider]; ok {
			if model.Protocol == "" || c.Protocol == model.Protocol {
				return c, nil
			}
		}
		// Otherwise route by wire protocol: a model served over the
		// Anthropic API should use the Anthropic endpoint even when the
		// registered provider is named differently.
		if model.Protocol != "" {
			for _, c := range providers {
				if c.Protocol == model.Protocol {
					return c, nil
				}
			}
		}
	}
	// Fall back to a heuristic and then to any registered provider that
	// speaks the right protocol.
	guess := GuessProvider(modelID)
	if c, ok := providers[guess]; ok {
		return c, nil
	}
	for _, c := range providers {
		if c.Protocol == guess {
			return c, nil
		}
	}
	// A model an extension added may name a protocol it also registered,
	// with no provider entry of its own.
	if hasModel {
		for _, c := range providers {
			if model.Protocol != "" && c.Protocol == model.Protocol {
				return c, nil
			}
		}
	}
	return ProviderConfig{}, fmt.Errorf("llm: no provider configured for model %q", modelID)
}

// ProviderFor builds a live provider for a model ID.
func (r *Registry) ProviderFor(modelID, preferredProvider string) (Provider, error) {
	cfg, err := r.Resolve(modelID, preferredProvider)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		cfg.APIKey = EnvKeyFor(cfg.Protocol)
	}
	r.mu.RLock()
	factory := r.factory
	r.mu.RUnlock()
	// An extension-registered protocol wins over the built-in factory:
	// it is the only way to serve a wire format the kernel does not
	// know, and a registered name is not a built-in one.
	var p Provider
	if build, ok := r.protocolBuilder(cfg.Protocol); ok {
		p, err = build(cfg)
	} else {
		p, err = factory(cfg)
	}
	if err != nil {
		return nil, err
	}
	// Gateway-wide headers (routing, session affinity) apply to every
	// provider this registry builds, not just the first one.
	r.mu.RLock()
	extra := make(map[string]string, len(r.headers))
	for k, v := range r.headers {
		extra[k] = v
	}
	r.mu.RUnlock()
	if len(extra) > 0 {
		switch prov := p.(type) {
		case *OpenAIProvider:
			prov.ExtraHeaders = mergeHeaders(prov.ExtraHeaders, extra)
		case *AnthropicProvider:
			prov.ExtraHeaders = mergeHeaders(prov.ExtraHeaders, extra)
		case *GeminiProvider:
			prov.ExtraHeaders = mergeHeaders(prov.ExtraHeaders, extra)
		}
	}
	return p, nil
}

// mergeHeaders merges extra into base without overwriting base's values.
func mergeHeaders(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}

// SetHeaders sets headers applied to every provider the registry builds
// (e.g. a stable session id for gateways that route on it).
func (r *Registry) SetHeaders(headers map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = headers
}

// SetFactory overrides how providers are built (tests use this).
func (r *Registry) SetFactory(f func(ProviderConfig) (Provider, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factory = f
}

// EnvKeyFor returns the conventional environment variable for a protocol.
func EnvKeyFor(protocol string) string {
	switch protocol {
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY")
	case "gemini":
		if k := os.Getenv("GEMINI_API_KEY"); k != "" {
			return k
		}
		return os.Getenv("GOOGLE_API_KEY")
	default:
		return os.Getenv("OPENAI_API_KEY")
	}
}

// EnvBaseURLFor returns the conventional base URL for a protocol.
func EnvBaseURLFor(protocol string) string {
	switch protocol {
	case "anthropic":
		return os.Getenv("ANTHROPIC_BASE_URL")
	case "gemini":
		return os.Getenv("GEMINI_BASE_URL")
	default:
		return os.Getenv("OPENAI_BASE_URL")
	}
}

// SyncCatalog fetches a remote model catalog and registers the models it
// lists. The endpoint is expected to return an OpenAI-style
// {"data":[{"id":"...","owned_by":"..."}]} document. Models are attached to
// the provider named in the request, unless the catalog says otherwise.
func (r *Registry) SyncCatalog(providerName, endpoint, apiKey string) error {
	body, err := fetchJSON(endpoint, apiKey)
	if err != nil {
		return fmt.Errorf("llm: sync catalog: %w", err)
	}
	models, err := parseCatalog(body)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range models {
		// A catalog entry carries its own protocol; keep the provider
		// name only as a label for models served by this endpoint.
		m.Provider = providerName
		r.models[m.ID] = m
	}
	return nil
}

// ParseCatalog extracts model metadata from an OpenAI-style catalog
// document. It is exported so callers can reuse the parser for a
// custom catalog format.
func ParseCatalog(body []byte) ([]Model, error) { return parseCatalog(body) }

func parseCatalog(body []byte) ([]Model, error) {
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			OwnedBy string `json:"owned_by"`
			Family  string `json:"family"`
			Limit   struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Capabilities struct {
				Tools struct {
					Supported bool `json:"supported"`
				} `json:"tools"`
				Reasoning struct {
					Supported bool `json:"supported"`
				} `json:"reasoning"`
			} `json:"capabilities"`
			// API lists the wire protocols a model is served by, e.g.
			// {"chat-completions": {...}, "messages": {...}}. The key
			// names mirror the protocol families.
			API map[string]json.RawMessage `json:"api"`
		} `json:"data"`
	}
	if err := jsonUnmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("llm: parse catalog: %w", err)
	}
	out := make([]Model, 0, len(doc.Data))
	for _, d := range doc.Data {
		if d.ID == "" {
			continue
		}
		name := d.Name
		if name == "" {
			name = strings.ToUpper(d.ID[:1]) + d.ID[1:]
		}
		out = append(out, Model{
			ID:            d.ID,
			Name:          name,
			Protocol:      protocolFromAPI(d.API),
			ContextWindow: d.Limit.Context,
			MaxOutput:     d.Limit.Output,
			Reasoning:     d.Capabilities.Reasoning.Supported,
		})
	}
	return out, nil
}

// protocolFromAPI maps a catalog entry's API families to a wire protocol.
// OpenAI-compatible catalogs list "chat-completions"; Anthropic-style
// ones list "messages"; Gemini lists "generate-content".
func protocolFromAPI(api map[string]json.RawMessage) string {
	if len(api) == 0 {
		return ""
	}
	for _, family := range []string{"chat-completions", "responses"} {
		if _, ok := api[family]; ok {
			return "openai"
		}
	}
	if _, ok := api["messages"]; ok {
		return "anthropic"
	}
	if _, ok := api["generate-content"]; ok {
		return "gemini"
	}
	return ""
}
