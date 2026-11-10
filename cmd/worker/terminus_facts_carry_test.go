package main

// TG-695: the terminus facts cross the runner ↔ db seam whole. The causality the terminus computes must reach the
// triage row, and the window must read it back — a dropped field withheld every commit-confirmed credit.

import (
	"reflect"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// KILLING MUTATION: map field by field again and leave out Causality (or CausalityEvidence) in either direction —
// RED here, naming the field.
func TestTerminusFactsCrossTheSeamWhole(t *testing.T) {
	at := time.Date(2026, 11, 8, 8, 38, 38, 0, time.UTC)
	in := runner.TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &at, CollateralKnown: true,
		CollateralObserved: true, FactsAt: &at, Causality: runner.CausalityTGCaused, CausalityEvidence: "injector: 0 restore(s)"}
	// Control: every field of the fixture is set, so a dropped field cannot hide behind a zero value.
	v := reflect.ValueOf(in)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("control: fixture field %s is zero — set it, or the test cannot see it dropped", v.Type().Field(i).Name)
		}
	}
	stored := toDBTerminusFacts(in)
	back := fromDBTerminusFacts(stored)
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("the facts must round-trip whole, got %+v want %+v", back, in)
	}
	sv := reflect.ValueOf(stored)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		f := sv.FieldByName(name)
		if !f.IsValid() || !reflect.DeepEqual(f.Interface(), v.Field(i).Interface()) {
			t.Errorf("field %s did not reach the db facts (got %v)", name, f)
		}
	}
	if stored.Causality != runner.CausalityTGCaused || back.Causality != runner.CausalityTGCaused {
		t.Fatalf("the causality must reach the triage row and come back to the window, got %q / %q", stored.Causality, back.Causality)
	}
}

// The two structs stay field-for-field identical (the conversion depends on it; this names the drift if a field is
// renamed rather than added, which a conversion would also refuse — said here so the failure reads as a sentence).
func TestTheRunnerAndDBTerminusFactsHaveTheSameFields(t *testing.T) {
	r, d := reflect.TypeOf(runner.TerminusFacts{}), reflect.TypeOf(db.TerminusFacts{})
	if r.NumField() != d.NumField() {
		t.Fatalf("runner.TerminusFacts has %d fields, db.TerminusFacts %d", r.NumField(), d.NumField())
	}
	for i := 0; i < r.NumField(); i++ {
		if r.Field(i).Name != d.Field(i).Name || r.Field(i).Type != d.Field(i).Type {
			t.Errorf("field %d: runner %s %s vs db %s %s", i, r.Field(i).Name, r.Field(i).Type, d.Field(i).Name, d.Field(i).Type)
		}
	}
}
