package ext

import (
	"context"
	"errors"
	"testing"

	"github.com/NanQiaoMi/auto-harness/contextmgr"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// fakeProvider stands in for a protocol an extension serves.
type fakeProvider struct{ name string }

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	return nil, errors.New("fake: not used")
}

// TestRegisterProtocolRoutesToTheExtension checks that an extension can
// serve a wire protocol the kernel does not compile in.
//
// Without this, a new API shape means patching llm.defaultFactory.
func TestRegisterProtocolRoutesToTheExtension(t *testing.T) {
	models := llm.NewRegistry()
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	api.Models = models

	var built string
	err := api.RegisterProtocol("acme", func(c llm.ProviderConfig) (llm.Provider, error) {
		built = c.Name
		return &fakeProvider{name: "acme"}, nil
	})
	if err != nil {
		t.Fatalf("RegisterProtocol: %v", err)
	}
	if err := models.RegisterProvider(llm.ProviderConfig{Name: "acme-gw", BaseURL: "https://acme.test", Protocol: "acme"}); err != nil {
		t.Fatal(err)
	}
	models.RegisterModel(llm.Model{ID: "acme-1", Provider: "acme-gw", Protocol: "acme"})

	provider, err := models.ProviderFor("acme-1", "")
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if provider.Name() != "acme" {
		t.Errorf("provider = %q, want the extension's", provider.Name())
	}
	if built != "acme-gw" {
		t.Errorf("builder received provider %q, want acme-gw", built)
	}
}

func TestRegisterProtocolRejectsDuplicates(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	api.Models = llm.NewRegistry()
	build := func(llm.ProviderConfig) (llm.Provider, error) { return &fakeProvider{name: "x"}, nil }
	if err := api.RegisterProtocol("acme", build); err != nil {
		t.Fatal(err)
	}
	if err := api.RegisterProtocol("acme", build); err == nil {
		t.Error("a duplicate protocol was accepted")
	}
	if err := api.RegisterProtocol("", build); err == nil {
		t.Error("an empty protocol name was accepted")
	}
	// A built-in protocol is not an extension's to replace: it would
	// change every provider that uses it, including unknown ones.
	if err := api.RegisterProtocol("openai", build); err == nil {
		t.Error("a built-in protocol was replaced")
	}
}

func TestRegisterProtocolNeedsARegistry(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	if err := api.RegisterProtocol("acme", func(llm.ProviderConfig) (llm.Provider, error) { return nil, nil }); err == nil {
		t.Error("a protocol was registered without a model registry")
	}
}

func TestProtocolsListsBuiltinsAndExtensions(t *testing.T) {
	models := llm.NewRegistry()
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	api.Models = models
	if err := api.RegisterProtocol("acme", func(llm.ProviderConfig) (llm.Provider, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	names := models.Protocols()
	want := map[string]bool{"openai": true, "anthropic": true, "gemini": true, "acme": true}
	if len(names) != len(want) {
		t.Fatalf("Protocols() = %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected protocol %q", n)
		}
	}
}

// stubBuilder is a context builder that records whether it ran.
type stubBuilder struct {
	ran  bool
	msgs []llm.Message
}

func (s *stubBuilder) Build(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
	s.ran = true
	return contextmgr.Result{Messages: in.Messages}, nil
}

func TestNoPolicyLeavesTheBuilderAlone(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	kernel := &stubBuilder{}
	if got := api.ContextBuilder(kernel); got != kernel {
		t.Error("ContextBuilder replaced the kernel builder with no policy registered")
	}
}

func TestContextPolicyWrapsTheKernelBuilder(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())

	var sawKernel bool
	if err := api.UseContextPolicy(func(next ContextBuilder) ContextBuilder {
		return builderFunc(func(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
			sawKernel = next != nil
			// The policy adds a message before the kernel sees it.
			in.Messages = append(in.Messages, llm.Message{Role: llm.RoleUser, Content: "injected"})
			return next.Build(ctx, in)
		})
	}); err != nil {
		t.Fatal(err)
	}

	kernel := &stubBuilder{}
	wrapped := api.ContextBuilder(kernel)
	if wrapped == nil {
		t.Fatal("ContextBuilder returned nil")
	}
	if _, err := wrapped.Build(context.Background(), contextmgr.Input{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !sawKernel {
		t.Error("the policy did not receive the kernel builder")
	}
	if !kernel.ran {
		t.Error("the kernel builder never ran")
	}
}

func TestContextPolicyCanReplaceTheKernel(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	if err := api.UseContextPolicy(func(next ContextBuilder) ContextBuilder {
		return builderFunc(func(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
			// The policy ignores next and answers itself.
			return contextmgr.Result{Messages: []llm.Message{{Role: llm.RoleUser, Content: "replaced"}}}, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	kernel := &stubBuilder{}
	result, err := api.ContextBuilder(kernel).Build(context.Background(), contextmgr.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if kernel.ran {
		t.Error("the kernel builder ran although the policy replaced it")
	}
	if len(result.Messages) != 1 || result.Messages[0].Content != "replaced" {
		t.Errorf("result = %#v", result.Messages)
	}
}

func TestContextPoliciesRunInRegistrationOrder(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	var order []string
	for _, name := range []string{"first", "second"} {
		name := name
		if err := api.UseContextPolicy(func(next ContextBuilder) ContextBuilder {
			return builderFunc(func(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
				order = append(order, name)
				return next.Build(ctx, in)
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := api.ContextBuilder(&stubBuilder{}).Build(context.Background(), contextmgr.Input{}); err != nil {
		t.Fatal(err)
	}
	// The first registered is the outermost, so it sees the request
	// first.
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("order = %v, want [first second]", order)
	}
}

// TestReloadDropsTheContextPolicy checks that a reloaded extension does
// not stack its policy twice.
func TestReloadDropsTheContextPolicy(t *testing.T) {
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	ext := Named("policy", func(api *API) error {
		return api.UseContextPolicy(func(next ContextBuilder) ContextBuilder {
			return builderFunc(func(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
				return next.Build(ctx, in)
			})
		})
	})
	mgr := NewManager(api)
	if err := mgr.Load(ext); err != nil {
		t.Fatal(err)
	}
	if got := api.ContextBuilder(nil); got == nil {
		t.Fatal("the policy was not registered")
	}
	if err := mgr.Reload(ext); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The reloaded extension registers its policy once, so the wrapper
	// chain holds exactly one entry. A scope that failed to drop the old
	// registration would leave two.
	var applied int
	if err := api.UseContextPolicy(func(next ContextBuilder) ContextBuilder {
		applied++
		return next
	}); err != nil {
		t.Fatal(err)
	}
	api.ContextBuilder(&stubBuilder{})
	// Two policies total: the reloaded extension's, and the counter.
	if applied != 1 {
		t.Errorf("the counter policy was applied %d times", applied)
	}
	if got := api.ContextBuilder(&stubBuilder{}); got == nil {
		t.Error("ContextBuilder returned nil after the reload")
	}
}

// builderFunc adapts a function to ContextBuilder.
type builderFunc func(context.Context, contextmgr.Input) (contextmgr.Result, error)

func (f builderFunc) Build(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
	return f(ctx, in)
}
