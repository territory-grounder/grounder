package policy

import (
	"context"
	"errors"
	"testing"
)

// TG-506: the durable seam. Load reads the store's override into the toggle so the worker's Effective()
// observes an override an admin set on the grounder plane; and a Load ERROR never clobbers the current
// override — a transient store failure must not silently flip the engine's effective state.
func TestEngineToggleDurableLoadAndNoClobber(t *testing.T) {
	ctx := context.Background()
	disabled := false
	store := &MemToggleStore{}
	if err := store.Save(ctx, &disabled, "admin"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	tg := NewEngineToggle(nil, nil).WithStore(store)

	// Before Load: no override → follows the per-mode default. (Semi-auto defaults engine-ON, so the
	// disabled override below is a genuine, observable change, not a no-op.)
	if tg.Effective(ModeSemiAuto) != EngineDefaultForMode(ModeSemiAuto) {
		t.Fatal("before Load, Effective must follow the per-mode default")
	}
	// After Load: the durable override (disabled) wins.
	if err := tg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tg.Effective(ModeSemiAuto) != false {
		t.Fatal("after Load, Effective must reflect the durable override (disabled)")
	}
	// A subsequent Load error is surfaced AND leaves the loaded override intact (no silent flip).
	store.WithLoadError(errors.New("db down"))
	if err := tg.Load(ctx); err == nil {
		t.Fatal("a store Load error must be surfaced to the caller")
	}
	if tg.Effective(ModeSemiAuto) != false {
		t.Fatal("a Load error must NOT clobber the last-known override")
	}
}
