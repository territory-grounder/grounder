package main

// Pins the contract between the causality composer and the REAL Proxmox actor-evidence reader (review of !1819,
// round 5): the composer tolerates a failed TARGET read only when the reader's error says "not found" — the pve
// reader's answer for a name that is not a guest (a PVE node). If modules/actorevidence/pve rewords that error,
// this test goes red before the guest lane silently loses every tg-caused reading. It also proves the reader's
// clean-miss coverage marker covers the SUBJECT end to end.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/actorevidence/pve"
	"github.com/territory-grounder/grounder/temporal/runner"
)

func TestCausalityToleratesOnlyThePveReadersNotFoundOnTheTarget(t *testing.T) {
	// A one-guest cluster: guest01 lives on pvenode01; the node itself is not a guest and resolves to "not found".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api2/json/cluster/resources"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": "pvenode01", "name": "guest01", "vmid": 101, "status": "running"}}})
		case strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pvenode01/tasks"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("TG662_PVE_CONTRACT_TOKEN", "test-token")
	reader := pve.New(srv.URL, config.SecretRef("env:TG662_PVE_CONTRACT_TOKEN"), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))

	// The reader's own answer for the node — the exact text the composer keys on.
	if _, err := reader.Read(context.Background(), "pvenode01", time.Now().Add(-time.Hour), time.Now()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Fatalf("the pve reader must answer a non-guest name with a 'not found' error, got %v", err)
	}

	cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}}
	read := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{reader}, cfg)
	ev, err := read(context.Background(), tg662Query())
	if err != nil {
		t.Fatal(err)
	}
	if !ev.ActorsKnown || !ev.ActorCovered {
		t.Fatalf("the subject (a guest) must read KNOWN and COVERED by the reader's clean-miss marker while the node's 'not found' is only noted, got %+v", ev)
	}
	noted := false
	for _, n := range ev.Notes {
		if strings.Contains(n, "noted, not blinding") && strings.Contains(strings.ToLower(n), "not found") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the target's 'not found' must be recorded as a non-blinding note, got %v", ev.Notes)
	}
	if got, _ := runner.NewActivities(runner.Deps{CausalityEvidence: func(context.Context, runner.CausalityQuery) (runner.CausalityEvidence, error) { return ev, nil }}).
		MarkTerminusFactsActivity(context.Background(), runner.MarkTerminusFactsInput{ExternalRef: "r", Mutated: true, ConfirmedClear: true, CollateralKnown: true,
			VerdictMatch: true, Host: "guest01", TargetHost: "pvenode01", AlertRule: "Devices-up/down", OpClass: "start-guest"}); got.Causality != runner.CausalityTGCaused {
		t.Fatalf("a covered, quiet guest with a 'not found' node read is tg-caused, got %+v", got)
	}
}
