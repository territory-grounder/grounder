package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/actorevidence/pve"
	"github.com/territory-grounder/grounder/temporal/runner"
)

type sealContractStore struct{ row db.CommitConfirmRow }

func (s *sealContractStore) ArmCommitConfirm(context.Context, db.CommitConfirmRow) error { return nil }
func (s *sealContractStore) Resolve(context.Context, string, string, string, string, string) error {
	return nil
}
func (s *sealContractStore) Get(context.Context, string, string) (db.CommitConfirmRow, error) {
	return s.row, nil
}

type sealContractManifests struct{ m *manifest.ActionManifest }

func (f sealContractManifests) Get(context.Context, string) (*manifest.ActionManifest, bool, error) {
	return f.m, true, nil
}

type sealContractExecs struct{}

func (sealContractExecs) ExecutionFor(context.Context, string, string) (db.ForwardExecution, bool, error) {
	return db.ForwardExecution{}, false, nil
}
func (sealContractExecs) ExecutionsSince(context.Context, time.Time, string, int) ([]db.ExecutionSince, error) {
	return nil, nil
}

func sealContractTask(kind, user, token string, at time.Time) map[string]any {
	upid := fmt.Sprintf("UPID:pvenode01:0000AAAA:0000BBBB:%08X:%s:101:%s:", at.Unix(), kind, user)
	t := map[string]any{"upid": upid, "node": "pvenode01", "type": kind, "id": "101", "user": user,
		"starttime": at.Unix(), "endtime": at.Unix() + 3, "status": "OK"}
	if token != "" {
		t["tokenid"] = token
	}
	return t
}

func TestTheSealDecisionReadsTheRealTaskLogUnderTheLiveCarveOutShape(t *testing.T) {
	armed := time.Now().Add(-12 * time.Minute).Truncate(time.Second)
	cfg := attribution.Config{
		SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"},
		Sanctioned: map[string][]string{"pve": {"root@pam", "root@pam!mcp"}},
		CarveOuts: []attribution.CarveOut{{ID: "test-pool", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"guest01"},
			ValidFrom: time.Now().Add(-24 * time.Hour), ValidUntil: time.Now().Add(30 * 24 * time.Hour)}},
	}
	tgStart := sealContractTask("vzstart", "tg-actuate@pve", "lane", armed.Add(2*time.Second))
	tgRecord := []time.Time{armed.Add(2 * time.Second)}
	cases := []struct {
		name     string
		tasks    []map[string]any
		tgRecord []time.Time
		running  bool
		want     string
		detail   string
	}{
		{"a sanctioned token stops the healed guest: the corrected L4 input", []map[string]any{tgStart, sealContractTask("vzstop", "root@pam", "mcp", armed.Add(40*time.Second))}, tgRecord, false, "superseded", "attributed-authorized"},
		{"the carve-out actor stops the healed guest: the L4 run-1 input", []map[string]any{tgStart, sealContractTask("vzstop", "root@pam", "", armed.Add(40*time.Second))}, tgRecord, false, "superseded", "authorized-test"},
		{"allowed: only TG acted and the guest still runs, so the inverse seals", []map[string]any{tgStart}, tgRecord, true, "sealed", ""},
		{"kill path: only TG acted and the guest is already stopped", []map[string]any{tgStart}, tgRecord, false, "failed", "inverse precondition violated"},
		{"a stranger stopped it", []map[string]any{tgStart, sealContractTask("vzstop", "mallory@pve", "", armed.Add(40*time.Second))}, tgRecord, false, "failed", "inverse precondition violated"},
		{"a stranger acted and the guest still runs", []map[string]any{tgStart, sealContractTask("vzreboot", "mallory@pve", "", armed.Add(40*time.Second))}, tgRecord, true, "held", "unsanctioned actor"},
		{"TG's own token with no TG execution behind it never fires the inverse", []map[string]any{tgStart}, nil, true, "held", "unsanctioned actor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				status := "stopped"
				if tc.running {
					status = "running"
				}
				switch {
				case strings.HasPrefix(r.URL.Path, "/api2/json/cluster/resources"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": "pvenode01", "name": "guest01", "vmid": 101, "type": "lxc", "status": status}}})
				case strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pvenode01/tasks"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.tasks})
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			t.Setenv("TG702_SEAL_CONTRACT_TOKEN", "test-token")
			reader := pve.New(srv.URL, config.SecretRef("env:TG702_SEAL_CONTRACT_TOKEN"), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second),
				pve.WithSelfCorrelation(func() string { return "tg-actuate@pve!lane" }, func(_ context.Context, guest string, from, to time.Time) ([]time.Time, error) {
					if guest != "guest01" {
						return nil, fmt.Errorf("unexpected guest %q", guest)
					}
					return tc.tgRecord, nil
				}))
			fwd, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-guest", Target: "guest01",
				Params: map[string]string{"guest": "guest01"}, Reversible: true}, safety.BandAuto, "ph-seal-contract", "")
			if err != nil {
				t.Fatal(err)
			}
			store := &sealContractStore{row: db.CommitConfirmRow{ActionID: fwd.ActionID, ExternalRef: "seal-contract", OpClass: "start-guest",
				TargetHost: "guest01", WindowSeconds: 600, ForwardBand: safety.BandAuto.String(), ArmedAt: armed}}
			acts := runner.NewActivities(runner.Deps{
				CommitConfirm:     store,
				Manifests:         sealContractManifests{m: fwd},
				Executions:        sealContractExecs{},
				Ledger:            audit.NewLedger(),
				CausalityEvidence: causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{reader}, cfg),
				Gate: &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) {
					return tc.running, "contract: guest_liveness", true
				}},
			})
			res, err := acts.SealCommitConfirmInverseActivity(context.Background(), runner.SealCommitConfirmInverseInput{ActionID: fwd.ActionID, ExternalRef: "seal-contract"})
			if err != nil {
				t.Fatal(err)
			}
			got, said := "failed", res.Reason
			switch {
			case res.Sealed:
				got = "sealed"
			case res.Superseded:
				got = "superseded"
			case res.HoldReason != "":
				got, said = "held", res.HoldReason
			}
			if got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, said, tc.want)
			}
			if tc.detail != "" && !strings.Contains(said, tc.detail) {
				t.Fatalf("the decision must say %q, got %q", tc.detail, said)
			}
		})
	}
}
