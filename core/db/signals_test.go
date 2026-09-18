package db

import "testing"

// decodeSignals must tolerate a non-string signal value: a single number/bool must degrade THAT key only, never
// drop the whole screen (the old map[string]string unmarshal failed the entire object on one typed value).
func TestDecodeSignals(t *testing.T) {
	// mixed: a string, a number, a bool — all must survive, stringified.
	m := decodeSignals([]byte(`{"poll_reason":"ood-novel-incident","blast_radius":3,"stateful":true}`))
	if m["poll_reason"] != "ood-novel-incident" {
		t.Errorf("string signal lost: %q", m["poll_reason"])
	}
	if m["blast_radius"] != "3" {
		t.Errorf("numeric signal not stringified: %q (whole screen would have been dropped by the old code)", m["blast_radius"])
	}
	if m["stateful"] != "true" {
		t.Errorf("bool signal not stringified: %q", m["stateful"])
	}
	// empty / null cases
	if decodeSignals(nil) != nil || decodeSignals([]byte(`{}`)) != nil {
		t.Errorf("empty signals must decode to nil")
	}
	if decodeSignals([]byte(`not json`)) != nil {
		t.Errorf("unparseable blob must decode to nil (screen omitted, never fabricated)")
	}
	if got := decodeSignals([]byte(`{"k":null}`)); got["k"] != "" {
		t.Errorf("null value must be empty string, got %q", got["k"])
	}
}
