package eval

import (
	"reflect"
	"testing"
)

// TG-575: a scorecard discloses the distinct harness identities behind its sessions, sorted, so two arms
// can be compared on configuration and an arm that mixed two configurations is visible. Killing mutation:
// drop the fps collection in Aggregate → the field is empty for sessions that carry a fingerprint.
func TestAggregateDisclosesHarnessFingerprints(t *testing.T) {
	sc := Aggregate([]Session{{Ref: "a", HarnessFP: "b2"}, {Ref: "b", HarnessFP: "a1"}, {Ref: "c", HarnessFP: "a1"}, {Ref: "d"}}, nil)
	if got, want := sc.HarnessFingerprints, []string{"a1", "b2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("harness_fingerprints: got %v, want %v (distinct, sorted, empty values skipped)", got, want)
	}
	if sc := Aggregate([]Session{{Ref: "x"}}, nil); len(sc.HarnessFingerprints) != 0 {
		t.Fatalf("a pre-feature capture must disclose no fingerprint, got %v", sc.HarnessFingerprints)
	}
}
