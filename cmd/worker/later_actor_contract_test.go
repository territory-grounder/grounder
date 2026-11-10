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
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/actorevidence/pve"
	"github.com/territory-grounder/grounder/temporal/runner"
)

func TestTheRealTaskLogReadsWhoMovedTheGuestAfterTheWindowArmed(t *testing.T) {
	armed := time.Now().Add(-35 * time.Minute).Truncate(time.Second)
	task := func(kind, user string, at time.Time) map[string]any {
		upid := fmt.Sprintf("UPID:pvenode01:0000AAAA:0000BBBB:%08X:%s:101:%s:", at.Unix(), kind, user)
		return map[string]any{"upid": upid, "node": "pvenode01", "type": kind, "id": "101", "user": user, "starttime": at.Unix(), "endtime": at.Unix() + 5, "status": "OK"}
	}
	cases := []struct {
		name  string
		tasks []map[string]any
		want  string
	}{
		{"TG's start then a person's shutdown", []map[string]any{task("qmstart", "tg-actuate@pve!lane", armed.Add(2*time.Second)), task("qmshutdown", "root@pam", armed.Add(26*time.Minute))}, attribution.AttributedAuthorized.String()},
		{"TG's start alone", []map[string]any{task("qmstart", "tg-actuate@pve!lane", armed.Add(2*time.Second))}, attribution.AttributedSelf.String()},
		{"a stranger's shutdown beside a person's", []map[string]any{task("qmshutdown", "root@pam", armed.Add(20*time.Minute)), task("qmshutdown", "mallory@pve", armed.Add(26*time.Minute))}, attribution.AttributedSuspicious.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasPrefix(r.URL.Path, "/api2/json/cluster/resources"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": "pvenode01", "name": "guest01", "vmid": 101, "status": "stopped"}}})
				case strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pvenode01/tasks"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.tasks})
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			t.Setenv("TG702_PVE_CONTRACT_TOKEN", "test-token")
			reader := pve.New(srv.URL, config.SecretRef("env:TG702_PVE_CONTRACT_TOKEN"), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
			cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
			ev, err := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{reader}, cfg)(context.Background(),
				runner.CausalityQuery{Host: "guest01", TargetHost: "guest01", OpClass: "start-guest", IncidentAt: armed, ExecutedAt: armed, ClearedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if !ev.ActorsKnown || !ev.ActorCovered || ev.ActorUnattributed != 0 {
				t.Fatalf("the guest's task log must read known and covered, got %+v", ev)
			}
			if ev.ActorTaxonomy != tc.want {
				t.Fatalf("taxonomy %q, want %q (%+v)", ev.ActorTaxonomy, tc.want, ev)
			}
		})
	}
}
