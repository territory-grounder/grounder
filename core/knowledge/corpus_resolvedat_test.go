package knowledge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TG-341. `json:"resolved_at,omitempty"` on a time.Time is a silent no-op — encoding/json omits empty
// strings, zero numbers, false, nil pointers/interfaces and empty maps/slices/arrays, and a zero time.Time
// is a non-empty struct. So every corpus re-serialized through WriteCorpus stamped
// "0001-01-01T00:00:00Z" onto every undated row (measured: 140/140 of deploy/knowledge/corpus.seed.json).
//
// Nothing downstream is misled by it — that is exactly why it survived. The cost is that a committed,
// human-reviewed artifact reads as if it holds a corrupt date where it actually holds no date, and the
// published JSON contract advertised the field as omittable when it never was.

const fakeZeroDate = "0001-01-01T00:00:00Z"

// KILLING MUTATION: delete Incident.MarshalJSON. The zero date returns to every undated row. RED.
func TestUndatedIncidentOmitsResolvedAt(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCorpus(&buf, []Incident{{ExternalRef: "inc-1", Host: "web01", Summary: "no resolution date"}}); err != nil {
		t.Fatalf("WriteCorpus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, fakeZeroDate) {
		t.Errorf("an undated row serialized with the fake date %s — a reader cannot tell that from a real "+
			"one, which is the whole defect:\n%s", fakeZeroDate, out)
	}
	if strings.Contains(out, "resolved_at") {
		t.Errorf("an undated row still carries a resolved_at key at all:\n%s", out)
	}
}

// The other half: a row that DOES know its date must still publish it, in RFC3339, unchanged. A fix that
// dropped every resolved_at would pass the test above and silently delete the recency channel
// (MECH-105/107) that TG-341's sibling work added to break alphabetical top-k ties.
//
// KILLING MUTATION: make MarshalJSON always take the omit branch. RED.
func TestDatedIncidentKeepsResolvedAt(t *testing.T) {
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var buf bytes.Buffer
	if err := WriteCorpus(&buf, []Incident{{ExternalRef: "inc-2", ResolvedAt: when}}); err != nil {
		t.Fatalf("WriteCorpus: %v", err)
	}
	if !strings.Contains(buf.String(), "2026-03-04T05:06:07Z") {
		t.Errorf("a dated row lost its resolved_at — the recency channel would go dark:\n%s", buf.String())
	}
}

// Every other field must survive the alias/embed trick. A marshaller that quietly dropped, renamed or
// reordered a field would still pass both tests above; this pins the whole shape by round-tripping through
// the REAL reader, which runs DisallowUnknownFields and so also rejects a renamed key.
func TestRoundTripPreservesEveryField(t *testing.T) {
	in := []Incident{
		{
			ExternalRef: "inc-3", Host: "db01", AlertRule: "HostDown", Site: "nl",
			Summary: "s", Resolution: "r", Tags: []string{"a", "b"},
			ResolvedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		// The UNDATED row carries every other field too, deliberately: the omit branch is a different code
		// path from the dated one, and a first version of this fixture left it bare — so a mutation that
		// dropped Tags inside the omit branch stayed green. A round-trip fixture has to exercise both
		// branches with the same field coverage or it only guards one of them.
		{
			ExternalRef: "inc-4", Host: "db02", AlertRule: "HighLatency", Site: "gr",
			Summary: "undated", Resolution: "restarted", Tags: []string{"c", "d"},
		},
	}
	var buf bytes.Buffer
	if err := WriteCorpus(&buf, in); err != nil {
		t.Fatalf("WriteCorpus: %v", err)
	}
	got, err := ParseCorpus(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParseCorpus rejected what WriteCorpus produced: %v\n%s", err, buf.String())
	}
	if len(got) != len(in) {
		t.Fatalf("round trip changed the row count: %d -> %d", len(in), len(got))
	}
	for i := range in {
		if !got[i].ResolvedAt.Equal(in[i].ResolvedAt) {
			t.Errorf("row %d resolved_at %v -> %v", i, in[i].ResolvedAt, got[i].ResolvedAt)
		}
		a, b := in[i], got[i]
		a.ResolvedAt, b.ResolvedAt = time.Time{}, time.Time{} // compared above; time.Time is not ==-safe
		if a.ExternalRef != b.ExternalRef || a.Host != b.Host || a.AlertRule != b.AlertRule ||
			a.Site != b.Site || a.Summary != b.Summary || a.Resolution != b.Resolution ||
			strings.Join(a.Tags, ",") != strings.Join(b.Tags, ",") {
			t.Errorf("row %d changed across the round trip:\n in: %+v\nout: %+v", i, a, b)
		}
	}
}

// The vacuity floor for this whole file: prove the plain struct tag really IS a no-op, so the tests above
// are guarding a marshaller that does work rather than a language feature that already did it. If Go ever
// makes omitempty understand zero structs, this fails and the custom marshaller can go.
func TestOmitemptyOnATimeIsStillANoOp(t *testing.T) {
	type plain struct {
		ResolvedAt time.Time `json:"resolved_at,omitempty"`
	}
	b, err := json.Marshal(plain{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fakeZeroDate) {
		t.Skipf("encoding/json now omits a zero time.Time under omitempty (%s) — Incident.MarshalJSON is "+
			"redundant and can be deleted", b)
	}
}
