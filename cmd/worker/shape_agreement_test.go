package main

// THE TWO SHAPE CHECKS MUST AGREE (TG-262).
//
// A value's declared shape is judged twice, by two implementations that cannot share code:
//
//   - cpconfig.ValidateWrite  — at the console, so the operator is refused in the dialog;
//   - bindingValueFault       — at the worker's resolver, so a row written any other way is refused
//                               before it reaches a fail-closed consumer.
//
// core/ may not import modules/, so the duplication is structural rather than lazy. What must NOT be
// structural is DRIFT: if the console accepts what the worker refuses, the operator is back to a saved
// setting that is not in effect — the exact defect TG-262 exists to close, wearing a new hat. If the
// console refuses what the worker accepts, a legal configuration becomes unreachable.
//
// This drives BOTH over one table and asserts they reach the same verdict.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/modules/desc"
)

// KILLING MUTATION: change either implementation's rule for any row below — e.g. make the console accept
// a bare host as a URL, or the worker accept "soon" as a duration. RED on that row.
func TestTheConsoleAndTheWorkerAgreeOnValueShape(t *testing.T) {
	cases := []struct {
		name     string
		typ      desc.FieldType
		pattern  string
		maxLen   int
		maxItems int
		value    string
		wantOK   bool
	}{
		{"duration ok", desc.TypeDuration, "", 0, 0, "30s", true},
		{"duration bad", desc.TypeDuration, "", 0, 0, "soon", false},
		{"bool ok", desc.TypeBool, "", 0, 0, "true", true},
		{"bool bad", desc.TypeBool, "", 0, 0, "maybe", false},
		{"url ok", desc.TypeURL, "", 0, 0, "https://h.example.test:8006", true},
		{"url no scheme", desc.TypeURL, "", 0, 0, "h.example.test:8006", false},
		{"url no host", desc.TypeURL, "", 0, 0, "https://", false},
		{"maxlen ok", desc.TypeText, "", 16, 0, "short", true},
		{"maxlen over", desc.TypeText, "", 8, 0, "far too long to fit", false},
		{"pattern ok", desc.TypeText, `^[0-9]+$`, 0, 0, "12345", true},
		{"pattern bad", desc.TypeText, `^[0-9]+$`, 0, 0, "12a45", false},
		{"idlist within maxitems", desc.TypeIDList, "", 0, 3, "a,b,c", true},
		{"idlist over maxitems", desc.TypeIDList, "", 0, 2, "a,b,c", false},
		{"idlist entry pattern ok", desc.TypeIDList, `^[0-9]+$`, 0, 0, "1,2,3", true},
		{"idlist entry pattern bad", desc.TypeIDList, `^[0-9]+$`, 0, 0, "1,2,three", false},
		{"kvmap ok", desc.TypeKVMap, "", 0, 0, "k=v,k2=v2", true},
		{"unconstrained text", desc.TypeText, "", 0, 0, "anything at all", true},
	}

	const name = "module.agree.test.field"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The console's judgement, through the real exported entry point.
			cpconfig.SetModuleKeys([]cpconfig.Key{{
				Name: name, ConsoleWritable: true,
				Type: string(tc.typ), Pattern: tc.pattern, MaxLen: tc.maxLen, MaxItems: tc.maxItems,
			}})
			t.Cleanup(func() { cpconfig.SetModuleKeys(nil) })
			_, consoleErr := cpconfig.ValidateWrite(name, tc.value)
			consoleOK := consoleErr == nil

			// The worker's judgement, through the real resolver check.
			workerOK := bindingValueFault(catalog.EnvBinding{
				Type: tc.typ, Pattern: tc.pattern, MaxLen: tc.maxLen, MaxItems: tc.maxItems,
			}, tc.value) == ""

			if consoleOK != workerOK {
				t.Fatalf("DRIFT: console accepts=%v, worker accepts=%v for %s value %q — one of them is "+
					"lying to the operator about what will take effect (console err: %v)",
					consoleOK, workerOK, tc.typ, tc.value, consoleErr)
			}
			if consoleOK != tc.wantOK {
				t.Fatalf("both agree on %v but the table expects %v — the shared expectation moved",
					consoleOK, tc.wantOK)
			}
		})
	}
}
