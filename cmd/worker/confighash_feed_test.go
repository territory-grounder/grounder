package main

// TG-466 slice 2: confighashBaselineAdapter is a pure field translation between confighash's and core/db's
// independently-declared, field-identical types. This proves the translation both directions and that a
// store error passes through VERBATIM (never swallowed here — that is Collector.Sweep's job, one layer up).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/cmdb/pve/confighash"
)

// fakeConfighashBaselineWriter is an in-memory confighashBaselineWriter capturing what it was called with.
type fakeConfighashBaselineWriter struct {
	gotObs db.GuestConfigObservation
	out    db.GuestConfigOutcome
	err    error
}

func (f *fakeConfighashBaselineWriter) Record(_ context.Context, obs db.GuestConfigObservation) (db.GuestConfigOutcome, error) {
	f.gotObs = obs
	return f.out, f.err
}

func TestConfighashBaselineAdapterTranslatesFields(t *testing.T) {
	w := &fakeConfighashBaselineWriter{out: db.GuestConfigOutcome{Changed: true, PreviousHash: "ch1:old"}}
	adapter := confighashBaselineAdapter{store: w}

	in := confighash.Observed{VMID: 466201, Guest: "web01", Node: "pve-a", Kind: "lxc", Hash: "ch1:new"}
	out, err := adapter.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record must not error: %v", err)
	}

	// Every field must have crossed the boundary untouched, in BOTH directions.
	if w.gotObs.VMID != in.VMID || w.gotObs.Guest != in.Guest || w.gotObs.Node != in.Node ||
		w.gotObs.Kind != in.Kind || w.gotObs.Hash != in.Hash {
		t.Fatalf("Observed did not translate field-for-field into db.GuestConfigObservation: got %+v from %+v", w.gotObs, in)
	}
	if !out.Changed || out.PreviousHash != "ch1:old" {
		t.Fatalf("db.GuestConfigOutcome did not translate field-for-field into confighash.Outcome: got %+v", out)
	}
	if out.FirstSighting {
		t.Fatalf("FirstSighting must translate false-as-false, got %+v", out)
	}
}

func TestConfighashBaselineAdapterPassesErrorThrough(t *testing.T) {
	boom := errors.New("guest_config_baseline: connection reset")
	w := &fakeConfighashBaselineWriter{err: boom}
	adapter := confighashBaselineAdapter{store: w}

	out, err := adapter.Record(context.Background(), confighash.Observed{VMID: 1, Guest: "web01", Hash: "ch1:x"})
	if !errors.Is(err, boom) {
		t.Fatalf("a store error must pass through VERBATIM (this seam never swallows or masks it), got %v", err)
	}
	if out != (confighash.Outcome{}) {
		t.Fatalf("an error must return the zero Outcome, never a partial/fabricated one, got %+v", out)
	}
}

// THE HALF-ARMED CASE (review finding): TG_PVE_CONFIGHASH_ENABLED set, but the confighash reader never
// armed (TG_PVE_URL unset, or TG_PVE_RO_TOKEN_REF unset/unresolvable). Before this fix, the read seam
// (Deps.GuestConfigChangedWithin) was gated on dbPool+flag alone, so it wired anyway — always answering
// false against a baseline nothing ever swept: fail-safe (no false positive), but a silent, permanent
// false-negative masquerading as "armed". Both halves must now refuse to arm, and the misconfiguration must
// be LOUD (a distinct WARNING), never the same quiet as the deliberate flag-off default.
func TestConfighashReadArmedRefusesTheHalfArmedCase(t *testing.T) {
	armed, logLine := confighashReadArmed(true /* dbConnected */, false /* readerArmed */, true /* flagOn */, true /* classifyPlane */)
	if armed {
		t.Fatal("flag ON but the reader never armed must NOT arm the read seam — it would answer false forever against an unswept baseline")
	}
	if !strings.Contains(logLine, "WARNING") {
		t.Fatalf("a half-armed misconfiguration must log a WARNING, not read as the deliberate dark default, got %q", logLine)
	}
	if strings.Contains(logLine, "ship-dark default") {
		t.Fatalf("the half-armed line must NOT read like the deliberate flag-unset case, got %q", logLine)
	}

	w := confighashSweepWarning(true /* flagOn */, false /* readerArmed */)
	if w == "" {
		t.Fatal("confighashSweepWarning must fire for the SAME half-armed case (flag ON, reader not armed)")
	}
	if !strings.Contains(w, "WARNING") {
		t.Fatalf("confighashSweepWarning must be a WARNING line, got %q", w)
	}
}

// The deliberate ship-dark default (flag simply unset) must NOT warn — a warning on every unconfigured
// worker would train operators to ignore it, defeating the one case (the half-armed misconfiguration above)
// where it matters.
func TestConfighashReadArmedStaysQuietWhenTheFlagIsSimplyOff(t *testing.T) {
	armed, logLine := confighashReadArmed(true, false, false, true)
	if armed {
		t.Fatal("flag off must never arm the read seam")
	}
	if strings.Contains(logLine, "WARNING") {
		t.Fatalf("the deliberate dark default must not read as a misconfiguration WARNING, got %q", logLine)
	}

	if w := confighashSweepWarning(false, false); w != "" {
		t.Fatalf("no warning when the flag is simply unset, got %q", w)
	}
}

// Both arms must actually ARM when everything resolved — the fix must not have overcorrected into permanent
// refusal.
func TestConfighashReadArmedArmsWhenFullyResolved(t *testing.T) {
	armed, logLine := confighashReadArmed(true, true, true, true)
	if !armed {
		t.Fatalf("dbPool connected + reader armed must ARM the read seam, got armed=%v log=%q", armed, logLine)
	}
	if !strings.Contains(logLine, "ARMED") {
		t.Fatalf("expected an ARMED boot line, got %q", logLine)
	}
	if w := confighashSweepWarning(true, true); w != "" {
		t.Fatalf("no warning once the reader actually armed, got %q", w)
	}
}

// A separate half-armed shape: the reader armed cleanly (credential fine) but no durable pool is connected.
// This must ALSO refuse to arm the read seam (there is nowhere to read guest_config_baseline FROM), with a
// message that names the pool — not the credential — as the reason, so an operator is not sent chasing the
// wrong fix.
func TestConfighashReadArmedRefusesWithNoPoolEvenIfReaderArmed(t *testing.T) {
	armed, logLine := confighashReadArmed(false /* dbConnected */, true /* readerArmed */, true /* flagOn */, true /* classifyPlane */)
	if armed {
		t.Fatal("no durable pool must never arm the read seam, regardless of the reader's state")
	}
	if !strings.Contains(logLine, "pool") {
		t.Fatalf("expected the no-pool reason to be named, got %q", logLine)
	}
}

// On the ACTUATION plane the confighash mutation signal is inert (its consumer is the triage classify path),
// so the boot line must say N/A-on-this-plane, never OFF/UNREACHABLE — the latter reads as a gap where there
// is none (it nearly tripped a false drift alarm on 2026-08-26). Flag value is irrelevant on this plane.
func TestConfighashReadArmedActuationPlaneIsNotADrift(t *testing.T) {
	for _, flagOn := range []bool{true, false} {
		armed, logLine := confighashReadArmed(true, true, flagOn, false /* classifyPlane */)
		if armed {
			t.Fatalf("the actuation plane never arms the confighash read seam (classify runs on triage); flagOn=%v", flagOn)
		}
		if !strings.Contains(logLine, "N/A on the actuation plane") || strings.Contains(logLine, "UNREACHABLE") {
			t.Fatalf("actuation-plane line must read N/A-not-UNREACHABLE (flagOn=%v): %q", flagOn, logLine)
		}
	}
}
