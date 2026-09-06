package rosetta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryManualOverridesRemote(t *testing.T) {
	r := newRegistry()
	r.SetRemote([]ModelInfo{
		{ID: "m1", DisplayName: "remote", ContextWindow: 8192, SupportsThinking: true},
	})
	r.SetManual([]ModelInfo{
		{ID: "m1", DisplayName: "manual"}, // sparse: only the display name
	})
	mi, ok := r.Lookup("m1")
	if !ok {
		t.Fatal("m1 not found")
	}
	if mi.DisplayName != "manual" {
		t.Errorf("DisplayName = %q, want manual (manual layer wins)", mi.DisplayName)
	}
	if mi.ContextWindow != 8192 {
		t.Errorf("ContextWindow = %d, want 8192 (inherited from remote)", mi.ContextWindow)
	}
	if !mi.SupportsThinking {
		t.Errorf("SupportsThinking = false, want true (higher layer cannot revoke)")
	}
	if !mi.Known {
		t.Errorf("Known = false, want true")
	}
}

func TestRegistryRemoteOnly(t *testing.T) {
	r := newRegistry()
	r.SetRemote([]ModelInfo{{ID: "m1"}})
	mi, ok := r.Lookup("m1")
	if !ok {
		t.Fatal("m1 not found")
	}
	if mi.Known {
		t.Error("remote-only entry must not be Known")
	}
}

func TestRegistryAliases(t *testing.T) {
	r := newRegistry()
	r.SetManual([]ModelInfo{{ID: "claude-sonnet-4-5", Aliases: []string{"sonnet", ""}}})
	if _, ok := r.Lookup("sonnet"); !ok {
		t.Fatal("alias sonnet not resolved")
	}
	if _, ok := r.Lookup("sonnet-4"); ok {
		t.Fatal("unknown alias must not resolve")
	}
}

func TestRegistryListSorted(t *testing.T) {
	r := newRegistry()
	r.SetManual([]ModelInfo{{ID: "b"}, {ID: "a"}})
	list := r.List()
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("list not sorted: %+v", list)
	}
}

func TestRegistryEmptyIDSkipped(t *testing.T) {
	r := newRegistry()
	r.SetManual([]ModelInfo{{ID: ""}, {ID: "ok"}})
	if len(r.List()) != 1 {
		t.Fatalf("empty id must be skipped")
	}
}

func TestRegistryLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	doc := `{"models":[{"id":"m1","context_window":100000,"aliases":["m"]}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRegistry()
	if err := r.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	mi, ok := r.Lookup("m")
	if !ok {
		t.Fatal("alias from file not resolved")
	}
	if !mi.Known || mi.ContextWindow != 100000 {
		t.Fatalf("loaded entry wrong: %+v", mi)
	}
}

func TestRegistryLoadFileErrors(t *testing.T) {
	r := newRegistry()
	if err := r.LoadFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file must error")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)
	if err := r.LoadFile(path); err == nil {
		t.Fatal("bad json must error")
	}
}
