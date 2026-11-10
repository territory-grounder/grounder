package risk

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-685: the registry's declaration outranks the proposal's own reversibility claim. disk-grow proposed as
// "reversible" by the model still polls with irreversible-op-class; a reversible class with the same input does not.
// KILLING MUTATION: drop Step 1b (the DeclaredIrreversible check) — RED here (disk-grow classifies AUTO).
func TestARegistryIrreversibleClassPollsWhateverTheProposalClaims(t *testing.T) {
	in := GatedInput{OpClass: "disk-grow", Reversible: Reversible, HasPrediction: true}
	d := Classify(in)
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "irreversible-op-class" {
		t.Fatalf("disk-grow must poll as irreversible-op-class whatever the proposal claims, got %s %v", d.Band, d.Signals)
	}
	in.OpClass = "restart-service"
	if d := Classify(in); d.Signals["poll_reason"] == "irreversible-op-class" {
		t.Fatalf("a registry-reversible class must not poll for irreversibility, got %v", d.Signals)
	}
}
