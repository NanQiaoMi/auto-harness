package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryResolveByModelAndProvider(t *testing.T) {
	r := NewRegistry()
	mustRegister := func(c ProviderConfig) {
		if err := r.RegisterProvider(c); err != nil {
			t.Fatal(err)
		}
	}
	mustRegister(ProviderConfig{Name: "openai", BaseURL: "https://o/v1", Protocol: "openai"})
	mustRegister(ProviderConfig{Name: "anthropic", BaseURL: "https://a", Protocol: "anthropic"})
	r.RegisterModel(Model{ID: "claude-sonnet-5-5", Provider: "anthropic"})
	r.RegisterModel(Model{ID: "gpt-5.2", Provider: "openai"})

	// Model metadata decides the provider.
	c, err := r.Resolve("claude-sonnet-5-5", "")
	if err != nil || c.Name != "anthropic" {
		t.Errorf("resolve claude = %+v (%v)", c, err)
	}
	c, err = r.Resolve("gpt-5.2", "")
	if err != nil || c.Name != "openai" {
		t.Errorf("resolve gpt = %+v (%v)", c, err)
	}
	// Explicit provider wins.
	c, err = r.Resolve("claude-sonnet-5-5", "openai")
	if err != nil || c.Name != "openai" {
		t.Errorf("explicit provider = %+v (%v)", c, err)
	}
	// Unknown provider is an error.
	if _, err := r.Resolve("gpt-5.2", "nope"); err == nil {
		t.Error("want error for unknown provider")
	}
	// Unknown model falls back to a registered provider of the right protocol.
	c, err = r.Resolve("some-new-model", "")
	if err != nil || c.Name != "openai" {
		t.Errorf("fallback = %+v (%v)", c, err)
	}
}

func TestRegistryProviderForUsesFactory(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterProvider(ProviderConfig{Name: "openai", Protocol: "openai", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	var got ProviderConfig
	r.SetFactory(func(c ProviderConfig) (Provider, error) {
		got = c
		return &OpenAIProvider{APIKey: c.APIKey}, nil
	})
	p, err := r.ProviderFor("m", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Errorf("provider type = %T", p)
	}
	if got.APIKey != "k" {
		t.Errorf("factory got key %q", got.APIKey)
	}
}

func TestRegistryCatalogSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "deepseek-v4.1-flash", "name": "DeepSeek V4.1 Flash"},
				{"id": "other-model"},
			},
		})
	}))
	defer srv.Close()

	r := NewRegistry()
	if err := r.RegisterProvider(ProviderConfig{Name: "openai", Protocol: "openai"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncCatalog("openai", srv.URL, "test"); err != nil {
		t.Fatal(err)
	}
	m, ok := r.Model("deepseek-v4.1-flash")
	if !ok {
		t.Fatal("synced model missing")
	}
	if m.Provider != "openai" || m.Name != "DeepSeek V4.1 Flash" {
		t.Errorf("model = %+v", m)
	}
	if _, ok := r.Model("other-model"); !ok {
		t.Error("second model missing")
	}
	// The built-in catalog entries remain; the two synced models are added.
	if len(r.Models()) != len(Catalog)+2 {
		t.Errorf("models = %d, want %d", len(r.Models()), len(Catalog)+2)
	}
}

func TestParseCatalog(t *testing.T) {
	body := []byte(`{"data":[{"id":"m1","name":"M One","limit":{"context":200000,"output":32000},"capabilities":{"tools":{"supported":true},"reasoning":{"supported":true}}}]}`)
	models, err := ParseCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d", len(models))
	}
	m := models[0]
	if m.ID != "m1" || m.ContextWindow != 200000 || m.MaxOutput != 32000 || !m.Reasoning {
		t.Errorf("model = %+v", m)
	}
}

func TestRegistryAppliesHeadersToEveryProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterProvider(ProviderConfig{Name: "a", Protocol: "openai", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterProvider(ProviderConfig{Name: "b", Protocol: "anthropic", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	r.SetHeaders(map[string]string{"x-opencode-session": "sess-1"})

	p1, err := r.ProviderFor("m1", "a")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := r.ProviderFor("m2", "b")
	if err != nil {
		t.Fatal(err)
	}
	if got := p1.(*OpenAIProvider).ExtraHeaders["x-opencode-session"]; got != "sess-1" {
		t.Errorf("openai provider header = %q", got)
	}
	if got := p2.(*AnthropicProvider).ExtraHeaders["x-opencode-session"]; got != "sess-1" {
		t.Errorf("anthropic provider header = %q", got)
	}
}

func TestRegistryRoutesByProtocol(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterProvider(ProviderConfig{Name: "gw", Protocol: "openai", BaseURL: "https://gw/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterProvider(ProviderConfig{Name: "anth", Protocol: "anthropic", BaseURL: "https://gw/anth"}); err != nil {
		t.Fatal(err)
	}
	// A catalog entry that speaks the Anthropic API must route to the
	// Anthropic endpoint even though its provider label is "gw".
	r.RegisterModel(Model{ID: "claude-x", Provider: "gw", Protocol: "anthropic"})

	c, err := r.Resolve("claude-x", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Protocol != "anthropic" {
		t.Errorf("resolved protocol = %q, want anthropic", c.Protocol)
	}
}

func TestProviderHooksIntercept(t *testing.T) {
	var gotHeader, gotModel string
	var gotStatus int
	var sawResponse bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io_WriteString(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	p := NewOpenAIProvider("k", srv.URL)
	p.Hooks = &Hooks{
		BeforeHeaders: func(provider string, headers map[string]string) {
			headers["X-Trace"] = "trace-1"
		},
		BeforeRequest: func(provider string, req *ChatRequest) {
			gotModel = req.Model
			req.Model = "rewritten"
		},
		AfterResponse: func(provider string, status int, body []byte) {
			gotStatus = status
			sawResponse = true
		},
	}

	stream, err := p.Chat(context.Background(), ChatRequest{Model: "original"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, _, err := Collect(stream, nil); err != nil {
		t.Fatal(err)
	}

	if gotHeader != "trace-1" {
		t.Errorf("header hook = %q", gotHeader)
	}
	if gotModel != "original" {
		t.Errorf("request hook saw model %q", gotModel)
	}
	if !sawResponse || gotStatus != http.StatusOK {
		t.Errorf("response hook: status=%d saw=%v", gotStatus, sawResponse)
	}
}

// io_WriteString is a small helper so the test stays readable.
func io_WriteString(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Errorf("write: %v", err)
	}
}
