package attribution

// spec/023 REQ-2313 — THE MODEL NEVER SEES RAW LOG TEXT, guarded structurally rather than by review.
//
// Every domain reader parses its source (a PVE task log, a journal, a k8s audit event, a NetBox
// changelog) into THIS type, and the agent-facing tool renders only what this type carries. The
// minimization is therefore a property of the STRUCT, not of each reader's discipline: as long as
// Evidence carries exactly these six typed fields, a reader physically cannot hand the model a raw log
// line — it has nowhere to put one.
//
// That is also how the guarantee breaks. Adding one `Raw string` / `Line string` / `Message string`
// field to carry "just a bit of context" would silently reopen the whole path, in a struct nobody
// re-reads, and every reader would be free to fill it. This test is the tripwire on that edit: it pins
// the closed field set by name and type, so widening the type is a deliberate, reviewed act with a spec
// amendment behind it rather than a one-line convenience.
//
// It does NOT assert what the readers write into those fields — that is each reader's own oracle (the
// k8saudit/journal/pve parse tests) — only that there is no field a raw line could live in.

import (
	"reflect"
	"testing"
)

func TestEvidenceCarriesOnlyTheMinimizedFields(t *testing.T) {
	want := map[string]string{
		"Domain":     "string",
		"Actor":      "string",
		"ActionKind": "string",
		"Target":     "string",
		"ObservedAt": "time.Time",
		"Ref":        "string",
		"Covered":    "bool",
	}
	got := map[string]string{}
	ty := reflect.TypeOf(Evidence{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		got[f.Name] = f.Type.String()
	}
	for name, typ := range want {
		have, ok := got[name]
		if !ok {
			t.Errorf("Evidence lost its %q field — every reader and the agent-facing tool depend on it", name)
			continue
		}
		if have != typ {
			t.Errorf("Evidence.%s is %s, want %s", name, have, typ)
		}
		delete(got, name)
	}
	for name, typ := range got {
		t.Errorf("Evidence gained an undeclared field %s %s. REQ-2313 holds BECAUSE this type has nowhere to "+
			"put a raw log line: a new free-text field reopens the raw-text path for every domain reader at "+
			"once, in a struct nobody re-reads. If the field is genuinely needed, amend spec/023 and widen "+
			"this list deliberately.", name, typ)
	}
}
