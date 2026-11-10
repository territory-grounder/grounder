package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/ingest/crowdsec"
)

// ★ THE PROVISIONER THE ONLY SECURITY-TELEMETRY INGEST NEVER HAD (TG-291).
//
// `ingest/crowdsec` is declared in the module registry and printed at every boot among the estate's
// capabilities. Measured on the live box 2026-08-06: `sources` holds exactly two rows (librenms,
// prometheus-alertmanager), so AuthIngestPush hits "unknown source or no token provisioned ⇒ fail closed"
// and `POST /v1/ingest/crowdsec` returns 401 — confirmed against the running API, not inferred. CrowdSec
// has delivered 0 of 2,999 ingest_alert rows in the table's whole history.
//
// Both other push sources got a boot provisioner. This one did not, so it could not be configured at all
// without hand-run SQL — and hand-run SQL is exactly what UpsertSource's own doc comment exists to replace.
func TestProvisionCrowdsecPushSource(t *testing.T) {
	t.Run("ref resolves → upsert the crowdsec source by REF", func(t *testing.T) {
		t.Setenv("TG_CROWDSEC_INGEST_TOKEN", "a-crowdsec-bearer-token")
		up := &fakeUpserter{}
		provisionPushSource(context.Background(), up, crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", config.SecretRef("env:TG_CROWDSEC_INGEST_TOKEN"))

		if up.calls != 1 {
			t.Fatalf("UpsertSource called %d times, want 1", up.calls)
		}
		// THE ID MUST MATCH THE FRONT DOOR. AuthIngestPush looks the row up by the {source_type} URL
		// param, so a provisioner that spells the id differently writes a row nothing authenticates
		// against — a green boot log and a 401 on every push, which is the state this ticket describes.
		if up.sourceID != crowdsec.SourceType {
			t.Errorf("provisioned the sources row as %q, but the front door authenticates CrowdSec pushes "+
				"as %q — every push would 401 while the boot log claims success", up.sourceID, crowdsec.SourceType)
		}
		// INV-13: the REF is stored, never the literal.
		if up.tokenRef != "env:TG_CROWDSEC_INGEST_TOKEN" {
			t.Errorf("stored token ref = %q, want the ref itself", up.tokenRef)
		}
		if strings.Contains(up.tokenRef, "a-crowdsec-bearer-token") {
			t.Errorf("the RESOLVED LITERAL reached the sources row (%q). The row must hold the reference; "+
				"the literal is resolved only to test presence.", up.tokenRef)
		}
	})

	t.Run("unset ref → provision NOTHING", func(t *testing.T) {
		up := &fakeUpserter{}
		provisionPushSource(context.Background(), up, crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", "")
		if up.calls != 0 {
			t.Errorf("UpsertSource called %d times for an unset ref, want 0. A row with no usable credential "+
				"authenticates nothing and makes an operator read the 401s as a source fault rather than a "+
				"missing credential.", up.calls)
		}
	})

	t.Run("ref that resolves EMPTY → provision nothing", func(t *testing.T) {
		up := &fakeUpserter{}
		provisionPushSource(context.Background(), up, crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", config.SecretRef("env:TG_CROWDSEC_TOKEN_THAT_IS_NOT_SET"))
		if up.calls != 0 {
			t.Errorf("UpsertSource called %d times for a ref resolving empty, want 0", up.calls)
		}
	})

	t.Run("DB error does not crash the read-only foundation", func(t *testing.T) {
		t.Setenv("TG_CROWDSEC_INGEST_TOKEN", "a-crowdsec-bearer-token")
		up := &fakeUpserter{err: errors.New("pool closed")}
		provisionPushSource(context.Background(), up, crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", config.SecretRef("env:TG_CROWDSEC_INGEST_TOKEN"))
		// Reaching here without a panic is the assertion: optional provisioning must degrade, never abort.
	})
}

// The plan is a pure function so the provision/skip decision is oracle-testable without a database.
func TestPlanPushSource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ref, resolved string
		wantProvision bool
	}{
		{"unset ref", "", "", false},
		{"whitespace-only ref", "   ", "", false},
		{"ref resolves empty", "bao:secret/data/tg/crowdsec#ingest_token", "", false},
		// The resolved value must not be a SUBSTRING of the ref, or the leak assertion below matches the ref
		// itself and fails on correct code — "tok" is inside "#ingest_token".
		{"ref resolves", "bao:secret/data/tg/crowdsec#ingest_token", "s3cr3t-bearer-value", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planPushSource(crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", tc.ref, tc.resolved)
			if got.Provision != tc.wantProvision {
				t.Fatalf("Provision = %v, want %v (reason: %s)", got.Provision, tc.wantProvision, got.Reason)
			}
			if got.Reason == "" {
				t.Error("no Reason — the boot log would say nothing about why the source is or is not provisioned")
			}
			// THE LITERAL MUST NEVER REACH THE PLAN. It is resolved only to test presence (INV-13).
			if tc.resolved != "" && strings.Contains(got.Reason, tc.resolved) {
				t.Errorf("the resolved token literal %q appears in the boot-log reason %q", tc.resolved, got.Reason)
			}
			if got.Provision && got.TokenRef != tc.ref {
				t.Errorf("TokenRef = %q, want the ref %q", got.TokenRef, tc.ref)
			}
		})
	}
}

// THE COMPOSITION ROOT. Every test above exercises the provisioner directly; none notices if main() never
// calls it — which is precisely the defect being fixed here, since the function's absence was never a
// compile error, only a source that silently could not be configured.
//
// Comment lines are stripped before matching so this cannot be satisfied by the prose above it;
// TestTheCrowdsecWiringGuardIgnoresProse holds that stripping honest.
func TestMainProvisionsTheCrowdsecSource(t *testing.T) {
	src := stripLineComments(readMainGo(t))
	if !strings.Contains(src, "provisionPushSource(ctx, db.NewSourceResolver(pool), crowdsec.SourceType,") {
		t.Fatal("main.go never provisions the crowdsec source. The provisioner exists, is fully tested, and " +
			"nothing runs it — so `sources` still has no crowdsec row and every push still 401s. That is the " +
			"same shape as the defect this ticket reports: a declared capability with nothing behind it.")
	}
}

func TestTheCrowdsecWiringGuardIgnoresProse(t *testing.T) {
	prose := "// provisionPushSource(ctx, db.NewSourceResolver(pool), crowdsec.SourceType, up, cfg)\nfunc main() {}\n"
	if got := stripLineComments(prose); strings.Contains(got, "provisionPushSource(ctx, db.NewSourceResolver(pool), crowdsec.SourceType,") {
		t.Fatalf("stripLineComments left a commented-out call in place, so the wiring guard would pass on "+
			"prose alone; got %q", got)
	}
}

// readMainGo fails rather than returning "" when main.go is unreadable: an empty string would satisfy
// every not-contains assertion and report health for a file nobody managed to open.
func readMainGo(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("main.go is empty — the guards below would be vacuous")
	}
	return string(b)
}

func stripLineComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// THE SIBLING HOLE, found while fixing crowdsec (TG-315 / TG-291).
//
// catalog.go records ingest/authlog as a "push-only receiver" — the syslog-ng collector POSTs folded events
// to /v1/ingest/authlog. It has no sources row either, and it has never delivered a row. Guarded separately
// from crowdsec on purpose: one shared provisioner means one call site can be deleted while the other keeps
// this file green, and a scoped guard that only watches the source I was looking at is how the sibling got
// missed in the first place.
func TestMainProvisionsTheAuthlogSource(t *testing.T) {
	src := stripLineComments(readMainGo(t))
	if !strings.Contains(src, "provisionPushSource(ctx, db.NewSourceResolver(pool), authlog.SourceType,") {
		t.Fatal("main.go never provisions the authlog source. TG-315 shipped a push-only receiver that cannot " +
			"be authenticated against, so the collector's POSTs 401 and the connector reports as a declared " +
			"capability that has never delivered.")
	}
}

// Every push source the registry declares must be provisionable, or it is a capability with nothing behind
// it. This enumerates rather than spot-checking: naming only the sources I happened to look at is exactly
// the scoped measurement that let authlog hide behind crowdsec.
func TestEveryDeclaredPushSourceHasAProvisioner(t *testing.T) {
	src := stripLineComments(readMainGo(t))
	for _, want := range []string{
		"librenms", // provisionLibrenmsPushAuth
		"alertmanager.SourceType",
		"crowdsec.SourceType",
		"authlog.SourceType",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no provisioning path references %s — a push source with no sources row fails closed on "+
				"every push while the boot log advertises it", want)
		}
	}
}
