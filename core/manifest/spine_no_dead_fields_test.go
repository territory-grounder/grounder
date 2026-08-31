package manifest

// NO DECLARED-BUT-DEAD FIELD ON THE GOVERNANCE SPINE (TG-66).
//
// ActionManifest's own doc comment calls it "the single immutable record the whole system replays,
// evaluates and audits against". A field declared there is a PROMISE that the record carries that fact.
//
// ToolCalls []string was such a promise and it was empty: zero production writers, zero readers — the only
// non-test occurrence in the entire tree was its own declaration — and no column in any migration. A
// reader looking for the executed tool calls would have found a field that named exactly what they wanted
// and always held nil, and concluded the manifest does not record what ran. It does; the executed argv is
// bound by the interceptor. The field was the misleading part.
//
// So every exported field must be one of two things, in writing:
//
//   - PERSISTED — it has a column in the manifest table, or
//   - IN-PROCESS — it is deliberately not durable, with the reason recorded here.
//
// A field in neither set is the ToolCalls shape again, and that is what fails.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// inProcessOnly names the ActionManifest fields that are deliberately NOT persisted, with the reason. The
// store's own doc comment records this design: the staged lifecycle chain is an in-process structure.
var inProcessOnly = map[string]string{
	"Provenance": "the proposal's origin is carried on the ledger event that produced it; duplicating it " +
		"onto the manifest row would create a second copy that can disagree with the first",
	"Stages": "the append-only lifecycle chain is an in-process structure — the manifest is sealed ONCE at " +
		"the predicted stage (first-wins ON CONFLICT DO NOTHING) and the chain continues in memory; " +
		"persisting it would require the post-seal UPDATE the append-only design forbids",
}

// TestEveryManifestFieldIsPersistedOrDeclaredInProcess is the floor.
func TestEveryManifestFieldIsPersistedOrDeclaredInProcess(t *testing.T) {
	cols := manifestColumns(t)
	if len(cols) < 5 {
		t.Fatalf("found only %d manifest column(s) %v — the migration parse is broken and this guard is "+
			"asserting over almost nothing", len(cols), cols)
	}

	typ := reflect.TypeOf(ActionManifest{})
	var exported int
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported: internal bookkeeping, not part of the record's promise
		}
		exported++

		// The JSON tag is the field's own name for itself; the column is snake_case of it.
		col := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if col == "" {
			col = strings.ToLower(f.Name)
		}
		if cols[col] {
			continue
		}
		if reason, ok := inProcessOnly[f.Name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("ActionManifest.%s is declared in-process with an EMPTY reason — an unexplained "+
					"exemption reads the same as a forgotten one", f.Name)
			}
			continue
		}
		t.Errorf("ActionManifest.%s (json %q) has no column in the manifest table and is not declared "+
			"in-process.\nThis struct is the record the whole system audits against, so a field here "+
			"PROMISES the record carries that fact. ToolCalls made exactly this promise and always held "+
			"nil. Either persist it, or add %q to inProcessOnly with the reason it cannot be.",
			f.Name, col, f.Name)
	}
	if exported < 5 {
		t.Fatalf("only %d exported field(s) on ActionManifest — the reflection found almost nothing and "+
			"every assertion above is vacuous", exported)
	}
}

// A recorded exemption must name a field that EXISTS. A stale entry silently exempts nothing while making
// the list look considered, and would pre-exempt a future field of that name.
func TestNoStaleInProcessExemptions(t *testing.T) {
	typ := reflect.TypeOf(ActionManifest{})
	have := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		have[typ.Field(i).Name] = true
	}
	for name := range inProcessOnly {
		if !have[name] {
			t.Errorf("inProcessOnly names %q, which is not a field on ActionManifest. The exemption list "+
				"overstates what has been considered, and a future field of that name would be exempted "+
				"without anyone deciding.", name)
		}
	}
}

// manifestColumns reads the columns the manifest table actually declares, straight from the migrations —
// no database needed, so this runs everywhere rather than only where a DSN is set.
func manifestColumns(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — this guard cannot assert anything about migrations it cannot read", dir, err)
	}
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?action_manifest\s*\((.*?)\);`)
	addCol := regexp.MustCompile(`(?is)ALTER TABLE action_manifest\s+ADD COLUMN (?:IF NOT EXISTS )?([a-z_]+)`)
	cols := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		src := string(b)
		if m := create.FindStringSubmatch(src); m != nil {
			for _, line := range strings.Split(m[1], "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "--") {
					continue // prose is not a column
				}
				name := strings.Fields(line)[0]
				name = strings.Trim(name, "\",")
				if name != "" && !strings.EqualFold(name, "PRIMARY") && !strings.EqualFold(name, "CONSTRAINT") {
					cols[strings.ToLower(name)] = true
				}
			}
		}
		for _, m := range addCol.FindAllStringSubmatch(src, -1) {
			cols[strings.ToLower(m[1])] = true
		}
	}
	return cols
}
