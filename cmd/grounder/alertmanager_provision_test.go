package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
)

// ★ THE READ THAT MAKES TG_AM_INGEST_TOKEN_REF A REAL KNOB (TG-278, wired by TG-284).
//
// The Alertmanager push bearer was a 64-char literal in TG_AM_INGEST_TOKEN, and its sources row was created
// by hand-run SQL. `grep -c TG_AM_INGEST_TOKEN_REF cmd/ core/ modules/` returned 0, so there was no
// configuration that could move the credential to a vault. Adding the variable WITHOUT this provisioner
// would have been the worse defect: an operator who set it to bao: would get a green secret-policy gate and
// a sources row still pointing at the plaintext.
//
// KILLING MUTATION (executed 2026-08-04): change the upsert to `up.UpsertSource(ctx, "librenms", ...)` (the
// id the neighbouring provisioner uses). This test then fails with:
//
//	provisioned the sources row as "librenms", but the front door authenticates Alertmanager pushes as
//	"prometheus-alertmanager" — every AM webhook would 401 while the boot log claims success.
func TestProvisionAlertmanagerPushAuth(t *testing.T) {
	t.Run("ref resolves → upsert the alertmanager source by REF", func(t *testing.T) {
		t.Setenv("TG_AM_INGEST_TOKEN", "64-chars-of-alertmanager-bearer")
		up := &fakeUpserter{}
		provisionAlertmanagerPushAuth(context.Background(), up, envConfig{
			AMIngestTokenRef: config.SecretRef("env:TG_AM_INGEST_TOKEN"),
		})
		if up.calls != 1 {
			t.Fatalf("UpsertSource calls = %d, want 1 — TG_AM_INGEST_TOKEN_REF is set and resolves, so the row "+
				"must be provisioned; a knob nothing reads is the defect this repo exists to stop shipping", up.calls)
		}
		if up.sourceID != alertmanager.SourceType {
			t.Errorf("provisioned the sources row as %q, but the front door authenticates Alertmanager pushes as "+
				"%q — every AM webhook would 401 while the boot log claims success", up.sourceID, alertmanager.SourceType)
		}
		if up.tokenRef != "env:TG_AM_INGEST_TOKEN" {
			t.Errorf("stored tokenRef = %q, want the REF", up.tokenRef)
		}
		if up.tokenRef == "64-chars-of-alertmanager-bearer" {
			t.Error("stored the literal bearer instead of the reference (INV-13 violation)")
		}
	})

	t.Run("a bao: ref repoints the row — the migration path the credential never had", func(t *testing.T) {
		up := &fakeUpserter{}
		// A bao: ref resolves through the delivery client; with no OpenBao wired it resolves empty, so the
		// provisioner correctly declines rather than writing a row that would fail closed on every push.
		provisionAlertmanagerPushAuth(context.Background(), up, envConfig{
			AMIngestTokenRef: config.SecretRef("bao:secret/data/tg/alertmanager#ingest_token"),
		})
		if up.calls != 0 {
			t.Fatalf("a ref that resolves to nothing must NOT provision a credential-less source; calls = %d", up.calls)
		}
	})

	t.Run("ref unset → no upsert", func(t *testing.T) {
		up := &fakeUpserter{}
		provisionAlertmanagerPushAuth(context.Background(), up, envConfig{AMIngestTokenRef: ""})
		if up.calls != 0 {
			t.Fatalf("UpsertSource calls = %d, want 0 (nothing configured)", up.calls)
		}
	})

	t.Run("ref resolves empty → no upsert (fail closed)", func(t *testing.T) {
		t.Setenv("TG_AM_INGEST_TOKEN", "")
		up := &fakeUpserter{}
		provisionAlertmanagerPushAuth(context.Background(), up, envConfig{
			AMIngestTokenRef: config.SecretRef("env:TG_AM_INGEST_TOKEN"),
		})
		if up.calls != 0 {
			t.Fatalf("UpsertSource calls = %d, want 0 — a credential-less source would 401 every push", up.calls)
		}
	})

	t.Run("db error is swallowed (fail open on optional DB)", func(t *testing.T) {
		t.Setenv("TG_AM_INGEST_TOKEN", "64-chars-of-alertmanager-bearer")
		up := &fakeUpserter{err: errors.New("db down")}
		provisionAlertmanagerPushAuth(context.Background(), up, envConfig{
			AMIngestTokenRef: config.SecretRef("env:TG_AM_INGEST_TOKEN"),
		})
		if up.calls != 1 {
			t.Fatalf("UpsertSource calls = %d, want 1 (attempted once, error logged, boot continues)", up.calls)
		}
	})
}

// The plan is pure and must never carry a resolved literal into the log line or the stored ref (INV-13).
func TestPlanAlertmanagerPushAuthNeverCarriesTheLiteral(t *testing.T) {
	const literal = "64-chars-of-alertmanager-bearer"
	plan := planAlertmanagerPushAuth("env:TG_AM_INGEST_TOKEN", literal)
	if !plan.Provision {
		t.Fatalf("a set, resolving ref must provision; reason: %s", plan.Reason)
	}
	if plan.TokenRef == literal || strings.Contains(plan.Reason, literal) {
		t.Errorf("the plan leaked the resolved bearer: ref=%q reason=%q — this reason is written to the boot log",
			plan.TokenRef, plan.Reason)
	}
	if plan.Reason == "" {
		t.Error("every branch must carry a human-readable reason, or a skipped provisioning is silent")
	}
	// The skip branches must say WHICH variable to set, not just that something is missing.
	if r := planAlertmanagerPushAuth("", "").Reason; !strings.Contains(r, "TG_AM_INGEST_TOKEN_REF") {
		t.Errorf("the unset-ref reason must name the variable an operator has to set; got %q", r)
	}
}

// ★ THE DEFAULT MUST BE BEHAVIOUR-PRESERVING. The live `prometheus-alertmanager` sources row already holds
// ingest_token_ref = env:TG_AM_INGEST_TOKEN. If loadEnv defaulted to anything else, the first boot after this
// MR would rewrite a working row to a reference that resolves nowhere and silently 401 every Alertmanager
// webhook — a fix that breaks the thing it was tidying up.
func TestAMIngestTokenRefDefaultMatchesTheLiveRow(t *testing.T) {
	// Setenv first so the harness restores whatever the environment really had, then UNSET: the case that
	// matters is a deployment that has never heard of this variable, not one that sets it empty (loadEnv
	// uses LookupEnv, so set-but-empty deliberately means "disabled" for every key here).
	t.Setenv("TG_AM_INGEST_TOKEN_REF", "placeholder")
	if err := os.Unsetenv("TG_AM_INGEST_TOKEN_REF"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if got := string(loadEnv().AMIngestTokenRef); got != "env:TG_AM_INGEST_TOKEN" {
		t.Fatalf("AMIngestTokenRef default = %q, want env:TG_AM_INGEST_TOKEN — the value the live sources row "+
			"already holds, so an existing deployment is unchanged by this MR", got)
	}
	// Explicitly empty means "do not provision", the same as every other optional ref in loadEnv.
	t.Setenv("TG_AM_INGEST_TOKEN_REF", "")
	if got := string(loadEnv().AMIngestTokenRef); got != "" {
		t.Errorf("an explicitly-emptied ref must stay empty (operator opt-out), not silently re-default; got %q", got)
	}
	t.Setenv("TG_AM_INGEST_TOKEN_REF", "bao:secret/data/tg/alertmanager#ingest_token")
	if got := string(loadEnv().AMIngestTokenRef); got != "bao:secret/data/tg/alertmanager#ingest_token" {
		t.Fatalf("an operator-set ref must win over the default, or the migration path does not exist; got %q", got)
	}
}

// ★ THE GATE MUST JUDGE THE VALUE ACTUALLY IN FORCE, NOT THE ONE IN THE RAW ENVIRONMENT.
//
// TG_LIBRENMS_DEPLOYMENTS is a console-WRITABLE descriptor field, and boot_config.go refuses only the AUTH
// and BOOTSTRAP key sets — so an operator can change it from the console, and cfg.LibrenmsDeployments (not
// os.Getenv) is what the front door then runs on. Enumerating from the raw environment would police a value
// nothing is using while the one in force went unexamined: TG-284's own defect, one level down.
//
// KILLING MUTATION (executed 2026-08-04): change secretEntries' accessor back to plain os.Getenv. This test
// then fails with "the console-overridden LibreNMS token is not policed".
func TestSecretEntriesPoliceTheOverriddenValueNotTheRawEnv(t *testing.T) {
	// The raw environment says one thing...
	t.Setenv("TG_LIBRENMS_DEPLOYMENTS", "envsite|https://env.example/api/v0|bao:secret/data/tg/librenms#env")
	// ...and the value actually in force (as loadEnv resolved it, console override included) says another.
	cfg := envConfig{
		LibrenmsDeployments: "gr|https://nms.gr/api/v0|env:LIBRENMS_GR_TOKEN",
		AMIngestTokenRef:    config.SecretRef("env:TG_AM_INGEST_TOKEN"),
	}
	var names []string
	for _, e := range cfg.secretEntries() {
		names = append(names, e.Name+"="+string(e.Ref))
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "TG_LIBRENMS_DEPLOYMENTS[gr]=env:LIBRENMS_GR_TOKEN") {
		t.Errorf("the console-overridden LibreNMS token is not policed — the gate read the raw environment "+
			"instead of the value the front door runs on. entries: %v", names)
	}
	if strings.Contains(joined, "envsite") {
		t.Errorf("the gate policed the RAW env value, which is not the one in force — a clean verdict over it "+
			"says nothing about the running system. entries: %v", names)
	}
	if !strings.Contains(joined, "TG_AM_INGEST_TOKEN_REF=env:TG_AM_INGEST_TOKEN") {
		t.Errorf("the Alertmanager bearer must be policed with the ref the provisioner will actually use; got %v", names)
	}
}
