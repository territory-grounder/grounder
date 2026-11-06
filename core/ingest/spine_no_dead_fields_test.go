package ingest

// NO DECLARED-BUT-DEAD FIELD ON THE INGEST SPINE (TG-373, the TG-66 floor applied to a second spine).
//
// TG-66 found ToolCalls declared on ActionManifest with zero writers, zero readers and no column, deleted
// it, and built core/manifest/spine_no_dead_fields_test.go so no field on the GOVERNANCE spine could repeat
// it. IncidentEnvelope is a second spine, and it had the same defect the floor was built against.
//
// IncidentEnvelope.IP was written by FOUR ingest modules and read by nothing:
//
//	modules/ingest/crowdsec:216                 raw2.IP = ipVal
//	modules/ingest/prometheus-alertmanager:156  raw.IP  = target
//	modules/ingest/librenms/normalize.go:148    raw.IP  = p.Host
//	modules/ingest/authlog:244                  raw.IP  = ip
//	core/ingest/normalize.go:36                 ip, err := validateIP(raw.IP)
//
// core parsed and validated it into the envelope, and the alert-log projection dropped it. Measured
// 2026-08-06: 48 of 165 prometheus-alertmanager rows had no host and 40 of those carried their only
// identifier in that field — including the three alerts TG received about its own AWX outage.
//
// A field declared on this struct is a PROMISE that an accepted incident carries that fact. So every
// exported field must be one of two things, in writing:
//
//   - PERSISTED — it has a column on ingest_alert, the front door's own record of what it accepted, or
//   - IN-PROCESS — deliberately not durable, with the reason recorded here.
//
// A field in neither set is the IP shape again, and that is what fails.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// envelopeColumn maps a field name to its ingest_alert column where the two differ. The table predates the
// struct's current shape, so the names are not mechanically derivable.
var envelopeColumn = map[string]string{
	"SourceID":  "source_id",
	"AlertRule": "alert_rule",
	"Labels":    "labels_json",
	"IP":        "subject_ip", // TG-373: named for what it IS, beside delivery_peer which is how it ARRIVED
}

// inProcessOnly names IncidentEnvelope fields deliberately NOT persisted on ingest_alert, with the reason.
// Empty today, and that is the point: every field on this spine currently reaches the record. It exists as
// a seam so the next exemption is a written decision rather than a quiet edit to the loop below.
var inProcessOnly = map[string]string{}

// TestEveryEnvelopeFieldReachesTheRecordOrIsDeclaredInProcess is the floor.
//
// KILLING MUTATION: remove subject_ip from migration 0063 (or the IP entry above). RED — which is exactly
// the state the ingest spine shipped in until TG-373.
func TestEveryEnvelopeFieldReachesTheRecordOrIsDeclaredInProcess(t *testing.T) {
	cols := ingestAlertColumns(t)
	if len(cols) < 8 {
		t.Fatalf("found only %d ingest_alert column(s) %v — the migration parse is broken and this guard is "+
			"asserting over almost nothing", len(cols), cols)
	}

	typ := reflect.TypeOf(IncidentEnvelope{})
	exported := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported: internal bookkeeping, not part of the record's promise
		}
		exported++

		col, ok := envelopeColumn[f.Name]
		if !ok {
			col = snake(f.Name)
		}
		if cols[col] {
			continue
		}
		if reason, declared := inProcessOnly[f.Name]; declared {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("IncidentEnvelope.%s is declared in-process with an EMPTY reason — an unexplained "+
					"exemption reads the same as a forgotten one", f.Name)
			}
			continue
		}
		t.Errorf("IncidentEnvelope.%s has no column on ingest_alert (looked for %q) and is not declared "+
			"in-process.\nThis struct is what the front door promises an accepted incident carries. IP made "+
			"exactly that promise — four ingest modules populated it, core validated it, and nothing ever "+
			"read it. Either persist it, or add %q to inProcessOnly with the reason it cannot be.",
			f.Name, col, f.Name)
	}
	if exported < 8 {
		t.Fatalf("only %d exported field(s) on IncidentEnvelope — the reflection found almost nothing and "+
			"every assertion above is vacuous", exported)
	}
}

// A recorded exemption must name a field that EXISTS. A stale entry exempts nothing while making the list
// look considered, and would pre-exempt a future field that happens to take the name.
func TestNoStaleIngestInProcessExemptions(t *testing.T) {
	typ := reflect.TypeOf(IncidentEnvelope{})
	have := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		have[typ.Field(i).Name] = true
	}
	for name := range inProcessOnly {
		if !have[name] {
			t.Errorf("inProcessOnly names %q, which is not a field on IncidentEnvelope — a stale exemption "+
				"exempts nothing and pre-exempts any future field of that name", name)
		}
	}
	for name := range envelopeColumn {
		if !have[name] {
			t.Errorf("envelopeColumn names %q, which is not a field on IncidentEnvelope — the mapping has "+
				"drifted from the struct and a renamed field would fall through to snake() unnoticed", name)
		}
	}
}

// snake renders a Go field name as its conventional column name (ExternalRef -> external_ref).
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ingestAlertColumns parses every migration for the columns ingest_alert actually has. Mirrors
// core/manifest's manifestColumns — the same shape, because the two floors must not drift in how they read
// a schema.
func ingestAlertColumns(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — this guard cannot assert anything about migrations it cannot read", dir, err)
	}
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?ingest_alert\s*\((.*?)\);`)
	// The whole ALTER statement, then every ADD COLUMN inside it. A regex anchored on
	// "ALTER TABLE ingest_alert ADD COLUMN <name>" finds only the FIRST name, and migration 0062 adds two
	// in one statement — so delivery_host would have looked absent while being present.
	alterStmt := regexp.MustCompile(`(?is)ALTER TABLE ingest_alert\b(.*?);`)
	addCol := regexp.MustCompile(`(?i)ADD COLUMN (?:IF NOT EXISTS )?([a-z_]+)`)
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
				name := strings.Trim(strings.Fields(line)[0], "\",")
				if name != "" && !strings.EqualFold(name, "PRIMARY") && !strings.EqualFold(name, "CONSTRAINT") {
					cols[strings.ToLower(name)] = true
				}
			}
		}
		// Scoped to the ingest_alert ALTER statements only: a migration file may alter several tables, and
		// crediting another table's column here would exempt a genuinely dead field.
		for _, stmt := range alterStmt.FindAllStringSubmatch(src, -1) {
			for _, m := range addCol.FindAllStringSubmatch(stmt[1], -1) {
				cols[strings.ToLower(m[1])] = true
			}
		}
	}
	return cols
}
