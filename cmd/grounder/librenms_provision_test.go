package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// TestPlanLibrenmsPushAuth pins the boot provisioning decision (config-not-code): provision the LibreNMS
// push-ingest bearer ONLY when a deployment is declared AND the token ref resolves non-empty; otherwise
// skip with a reason (a credential-less source would just fail closed on every push). The plan carries
// only the REF, never the resolved literal (INV-13).
func TestPlanLibrenmsPushAuth(t *testing.T) {
	const ref = "env:TG_LIBRENMS_INGEST_TOKEN"
	tests := []struct {
		name          string
		declared      bool
		tokenRef      string
		resolved      string
		wantProvision bool
		wantRef       string
	}{
		{"declared + resolves → provision", true, ref, "s3cr3t-value", true, ref},
		{"not declared → skip even if resolves", false, ref, "s3cr3t-value", false, ""},
		{"declared but ref unset → skip", true, "", "", false, ""},
		{"declared but ref blank → skip", true, "   ", "", false, ""},
		{"declared, ref set, resolves empty → skip (fail closed)", true, ref, "", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := planLibrenmsPushAuth(tc.declared, tc.tokenRef, tc.resolved)
			if plan.Provision != tc.wantProvision {
				t.Fatalf("Provision = %v, want %v (reason: %s)", plan.Provision, tc.wantProvision, plan.Reason)
			}
			if plan.TokenRef != tc.wantRef {
				t.Errorf("TokenRef = %q, want %q", plan.TokenRef, tc.wantRef)
			}
			// The literal secret value must NEVER appear in the stored ref or the log reason (INV-13).
			if tc.resolved != "" && (plan.TokenRef == tc.resolved || strings.Contains(plan.Reason, tc.resolved)) {
				t.Errorf("plan leaked the resolved literal secret: ref=%q reason=%q", plan.TokenRef, plan.Reason)
			}
			if plan.Reason == "" {
				t.Error("plan must always carry a human-readable reason for the log line")
			}
		})
	}
}

// fakeUpserter records the last UpsertSource call (and can force an error) — a pure-Go oracle for the
// provisioning wrapper without a Postgres (pgx paths are compose-only).
type fakeUpserter struct {
	calls    int
	sourceID string
	tokenRef string
	err      error
}

func (f *fakeUpserter) UpsertSource(_ context.Context, sourceID, tokenRef string) error {
	f.calls++
	f.sourceID, f.tokenRef = sourceID, tokenRef
	return f.err
}

// TestProvisionLibrenmsPushAuth exercises the wrapper end to end against a fake upserter: it upserts the
// `librenms` source (keyed by REF, never literal) exactly when deployments are declared AND the ref
// resolves non-empty, and skips otherwise — proving the boot step is idempotent config-not-code.
func TestProvisionLibrenmsPushAuth(t *testing.T) {
	t.Run("declared + env resolves → upsert librenms by ref", func(t *testing.T) {
		t.Setenv("TG_LIBRENMS_INGEST_TOKEN", "push-bearer-value")
		up := &fakeUpserter{}
		cfg := envConfig{
			LibrenmsDeployments:    "NL|https://dc1nms01/api/v0|env:LIBRENMS_NL_TOKEN",
			LibrenmsIngestTokenRef: config.SecretRef("env:TG_LIBRENMS_INGEST_TOKEN"),
		}
		provisionLibrenmsPushAuth(context.Background(), up, cfg)
		if up.calls != 1 {
			t.Fatalf("UpsertSource calls = %d, want 1", up.calls)
		}
		if up.sourceID != "librenms" {
			t.Errorf("sourceID = %q, want \"librenms\"", up.sourceID)
		}
		if up.tokenRef != "env:TG_LIBRENMS_INGEST_TOKEN" {
			t.Errorf("stored tokenRef = %q, want the REF (never the literal)", up.tokenRef)
		}
		if up.tokenRef == "push-bearer-value" {
			t.Error("stored the literal secret instead of the ref (INV-13 violation)")
		}
	})

	t.Run("no deployments → no upsert", func(t *testing.T) {
		t.Setenv("TG_LIBRENMS_INGEST_TOKEN", "push-bearer-value")
		up := &fakeUpserter{}
		provisionLibrenmsPushAuth(context.Background(), up, envConfig{
			LibrenmsDeployments:    "",
			LibrenmsIngestTokenRef: config.SecretRef("env:TG_LIBRENMS_INGEST_TOKEN"),
		})
		if up.calls != 0 {
			t.Fatalf("UpsertSource calls = %d, want 0 (no deployments declared)", up.calls)
		}
	})

	t.Run("declared but ref resolves empty → no upsert", func(t *testing.T) {
		t.Setenv("TG_LIBRENMS_INGEST_TOKEN", "") // present-but-empty ⇒ resolves empty
		up := &fakeUpserter{}
		provisionLibrenmsPushAuth(context.Background(), up, envConfig{
			LibrenmsDeployments:    "NL|https://dc1nms01/api/v0|env:LIBRENMS_NL_TOKEN",
			LibrenmsIngestTokenRef: config.SecretRef("env:TG_LIBRENMS_INGEST_TOKEN"),
		})
		if up.calls != 0 {
			t.Fatalf("UpsertSource calls = %d, want 0 (ref resolves empty ⇒ fail closed)", up.calls)
		}
	})

	t.Run("db error is swallowed (fail open on optional DB)", func(t *testing.T) {
		t.Setenv("TG_LIBRENMS_INGEST_TOKEN", "push-bearer-value")
		up := &fakeUpserter{err: errors.New("db down")}
		// Must not panic / crash — it logs and returns.
		provisionLibrenmsPushAuth(context.Background(), up, envConfig{
			LibrenmsDeployments:    "NL|https://dc1nms01/api/v0|env:LIBRENMS_NL_TOKEN",
			LibrenmsIngestTokenRef: config.SecretRef("env:TG_LIBRENMS_INGEST_TOKEN"),
		})
		if up.calls != 1 {
			t.Fatalf("UpsertSource calls = %d, want 1 (attempted once, error swallowed)", up.calls)
		}
	})
}
