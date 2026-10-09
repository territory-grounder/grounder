package httpapi

// BOTH ARMS OF THE FRONT DOOR, NOT ONE (TG-372).
//
// The ingest handler Appends from TWO places: the single-envelope arm and the grouped-transport arm that
// serves batched webhooks (Alertmanager sends grouped). An intake that records its origin on one shape and
// not the other answers "how did this alert reach TG" only by luck — and the shape it silently loses is the
// BATCHED one, which is what the estate Alertmanager actually sends.
//
// This guard exists because the mutation found it. Removing `.WithDelivery(deliveryContext(r))` from the
// grouped arm SURVIVED the round-trip tests in core/db: those exercise the store, and the store was
// perfectly happy to write whatever the handler handed it. Same shape this project keeps finding — the
// helper tested, the wiring not.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ingestHandlerCode returns core/httpapi/ingest.go with comment lines removed. The rationale above and in
// ingest.go names WithDelivery in prose, and a comment is not a call.
func ingestHandlerCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ingest.go")
	if err != nil {
		t.Fatalf("read ingest.go: %v", err)
	}
	var kept []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// KILLING MUTATION: drop `.WithDelivery(deliveryContext(r))` from either arm. RED, and it names which one.
func TestBothIngestArmsStampDelivery(t *testing.T) {
	code := ingestHandlerCode(t)

	// Every RecordFromEnvelope in the HTTP handler must be followed by the delivery stamp. Matching the
	// PAIR rather than counting each separately is what makes an arm that constructs-but-does-not-stamp a
	// failure instead of an arithmetic coincidence.
	construct := regexp.MustCompile(`RecordFromEnvelope\([^)]*\)`)
	stamped := regexp.MustCompile(`RecordFromEnvelope\([^)]*\)\.\s*\n?\s*WithDelivery\(deliveryContext\(r\)\)`)

	nConstruct := len(construct.FindAllString(code, -1))
	nStamped := len(stamped.FindAllString(code, -1))

	// VACUITY FLOOR. If the handler is restructured so the regex stops matching, this test would pass while
	// checking nothing — the exact defect class it guards.
	if nConstruct < 2 {
		t.Fatalf("found %d RecordFromEnvelope call(s) in ingest.go, expected at least 2 (the single arm and "+
			"the grouped-transport arm). The scan has stopped matching, so an unstamped arm would not be "+
			"detected either", nConstruct)
	}
	if nStamped != nConstruct {
		t.Errorf("%d of %d RecordFromEnvelope call(s) in the ingest handler stamp the delivery context. An "+
			"arm that Appends without .WithDelivery(deliveryContext(r)) records an alert whose origin TG "+
			"cannot state — and the arm most easily missed is the GROUPED-transport one, which is the shape "+
			"the estate Alertmanager sends.", nStamped, nConstruct)
	}
}

// The stamp must read the request, not invent a value. A constant would satisfy the pairing test above while
// recording a fiction on every row — worse than recording nothing, because a fiction is believed.
//
// KILLING MUTATION: replace deliveryContext's body with a constant. RED.
func TestDeliveryContextReadsTheRequestRatherThanInventingOne(t *testing.T) {
	code := ingestHandlerCode(t)
	body := regexp.MustCompile(`func deliveryContext\(r \*http\.Request\) \(peer, host string\) \{[^}]*\}`).
		FindString(code)
	if body == "" {
		t.Fatal("deliveryContext is gone or reshaped — the guard above then pairs against nothing")
	}
	for _, want := range []string{"r.RemoteAddr", "r.Host"} {
		if !strings.Contains(body, want) {
			t.Errorf("deliveryContext does not read %s, so what it records is not what arrived.\nGot: %s", want, body)
		}
	}
	// X-Forwarded-For is caller-controlled; trusting it would let a source choose what TG records about it.
	if strings.Contains(body, "X-Forwarded-For") || strings.Contains(body, "Forwarded") {
		t.Error("deliveryContext resolves a forwarding header. That header is caller-controlled, so the peer " +
			"TG records would be the peer the caller nominated — evidence chosen by the thing it is evidence about")
	}
}
