package ext

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestCapabilityRejectsUndeclaredTools(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ext.py")
	writeFile(t, script, pythonExtensionScript)
	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.Capabilities = []string{CapabilityHooks}
	err := pe.Setup(api)
	if err == nil || !strings.Contains(err.Error(), "tools") {
		if err == nil {
			pe.Stop()
		}
		t.Fatalf("Setup error = %v, want a tools-capability error", err)
	}
	if pe.connection() != nil {
		t.Fatal("a failed Setup left the extension process running")
	}
}

func TestCapabilityFiltersPushedEvents(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ext.py")
	writeFile(t, script, pythonExtensionScript)
	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.Capabilities = []string{CapabilityTools}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer pe.Stop()

	got := make(chan struct{}, 1)
	bus.On(EventInput, func(*Event) error {
		got <- struct{}{}
		return nil
	})
	notifyExtension(t, pe, MethodEvent, EventParams{Event: string(EventInput), Text: "ping"})
	select {
	case <-got:
		t.Fatal("an event without the events capability reached the bus")
	case <-time.After(250 * time.Millisecond):
	}
}
