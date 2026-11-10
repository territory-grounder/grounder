// Command guardallow renders the host guard's allowlist FROM THE OP-CLASS REGISTRY.
//
// THE DEFECT IT REMOVES. /etc/tg-actuator-guard.allow holds the exact SSH_ORIGINAL_COMMAND strings TG may
// send; the guard compares byte-for-byte with `grep -qxF` and exits 42 otherwise. That list was authored by
// hand in deploy/actuation-guard/install.sh, which emitted three shapes — `systemctl restart|reload <unit>`
// and `docker restart <container>`. The registry can emit five: it also has `systemctl start` (start-service)
// and `docker start` (start-container), BOTH graduated to AUTO. So TG could autonomously choose a start,
// clear every one of the six gates, record the approval in the ledger, and die at the host with exit 42.
// That is not hypothetical — core/actuate/interceptor.go records exactly such a denial on librespeed01.
//
// The rollback half is worse. spec.RollbackArgv renders `systemctl stop` and `docker stop`, and no code path
// ever wrote either verb into the allowlist. A heal that needs undoing could not be undone, and the failure
// would arrive at the worst possible moment.
//
// WHY GENERATE RATHER THAN COMPARE. A comparison is a third thing to maintain, and it is satisfied by editing
// either side. Deleting one of the two lists is the only fix that cannot drift: install.sh stops authoring
// commands and consumes this instead, so a new op-class is authorised by the same act that registers it.
//
// The operator still supplies the SUBJECTS — which units and which containers this host permits. That is the
// part that must stay operator-owned and per-host; the guard's value is that it is narrower than the
// registry, not that it is a different shape from it.
//
// Usage:
//
//	guardallow -units "nginx.service apache2.service" -containers "actualbudget-actual_server-1"
//	guardallow -manifest adopted.json          # subjects from the reviewed world model
//	guardallow -units "nginx.service" -manifest adopted.json   # UNION of both operator acts
//
// SUBJECTS MAY NOW COME FROM THE REVIEWED WORLD MODEL (spec/027 REQ-2704). -manifest reads the JSON
// GET /v1/manifest serves and takes the APPROVED (and stale — still granted) entries as subjects, so the
// same adopt click that grants TG a target also authorises it at the host. It is a UNION with the typed
// flags for the reason the allowlist provider unions: both lists are operator acts, and a grant must not
// evaporate because the other source was consulted.
//
// THE HOST GUARD REMAINS FINAL AUTHORITY. This tool only ever RENDERS a candidate list; a human installs
// it. An entry that is merely drafted contributes nothing — discovery cannot authorise itself at the host
// any more than it can in the control plane.
//
// THE SAME HOLDS FOR A VERB RATIFIED AT RUNTIME (TG-669). An op-class the admin ratifies through
// /v1/opclass/candidates/{key}/ratify enters the COMPOSED registry (the overlay), which this tool renders
// from — but the pattern file already installed on every host (`-mode=pattern`, rolled out by
// deploy/actuation-guard/rollout.sh) was rendered BEFORE the grant and does not contain the new shape. Until
// an operator re-renders the pattern and re-runs the rollout, every host refuses the new verb with exit 42,
// exactly as it refuses an arbitrary command. That is the intended order: ratification grants the CONTROL
// PLANE; the hosts are granted by the separate, operator-run re-render — two acts, never one click.
//
// Output: one SSH_ORIGINAL_COMMAND string per line, rendered through ssh.RemoteCommand — the same function
// that renders what TG actually sends, so the quoting cannot diverge.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
)

func main() {
	units := flag.String("units", "", "whitespace-separated systemd units this host permits")
	containers := flag.String("containers", "", "whitespace-separated docker containers this host permits")
	manifest := flag.String("manifest", "", "path to the JSON GET /v1/manifest serves; APPROVED entries become subjects (spec/027 REQ-2704)")
	mode := flag.String("mode", "list", "list (exact lines for the given subjects) or pattern (TG-626 follow-estate shapes + deny set)")
	flag.Parse()

	if *mode == "pattern" {
		fmt.Println("# tg-actuator-guard pattern file (TG-626) — rendered by tools/guardallow -mode=pattern; do not hand-edit")
		for _, l := range RenderPattern() {
			fmt.Println(l)
		}
		return
	}

	u, c := strings.Fields(*units), strings.Fields(*containers)
	if *manifest != "" {
		raw, err := os.ReadFile(*manifest)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guardallow: read manifest:", err)
			os.Exit(1)
		}
		mu, mc, err := SubjectsFromManifest(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guardallow: parse manifest:", err)
			os.Exit(1)
		}
		u, c = append(u, mu...), append(c, mc...)
	}

	lines, err := Render(u, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guardallow:", err)
		os.Exit(1)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}

// Render returns every SSH_ORIGINAL_COMMAND string the registry can produce for the given subjects, forward
// argv AND rollback argv, deduplicated and sorted.
//
// ROLLBACKS ARE INCLUDED DELIBERATELY. An op-class whose forward verb is authorised but whose compensating
// verb is not is worse than one that is refused outright: it can act and then cannot undo. Whether a rollback
// is ever issued is the interceptor's decision; whether the host would accept it must not be an accident.
//
// A spec that cannot render (no argv template, or params this subject does not satisfy) contributes nothing
// rather than a partial line — a malformed allowlist entry is either dead weight or, worse, an authorisation
// nobody intended.
func Render(units, containers []string) ([]string, error) {
	seen := map[string]bool{}
	for _, spec := range opschema.Specs() {
		// The param name a class needs tells us which subject list it draws from. Derived from the spec's
		// declared params, not from a hand-kept mapping of op-class → subject kind, because that mapping
		// would be the next thing to drift.
		for _, p := range spec.Params {
			var subjects []string
			switch p.Name {
			case "unit":
				subjects = units
			case "container":
				subjects = containers
			default:
				continue
			}
			for _, s := range subjects {
				params := map[string]string{p.Name: s}
				rendered := false
				if argv, err := spec.Argv(params); err == nil && len(argv) > 0 {
					seen[ssh.RemoteCommand(argv)] = true
					rendered = true
				}
				if rb, err := spec.RollbackArgv(params); err == nil && len(rb) > 0 {
					seen[ssh.RemoteCommand(rb)] = true
					rendered = true
				}
				// The rollback-necessity READ (TG-464). Before firing a stop-* inverse the actuate plane
				// confirms the unit is still RUNNING with `systemctl is-active <unit>` over the SAME key this
				// guard pins. A guard that authorises the stop but refuses the read leaves every service
				// rollback refusing at gate 4i (the probe fails closed on the denial), so the read ships with
				// the verbs — same act, same rendering, sourced from the one exported shape
				// (ssh.ServiceActiveProbeArgv) the probe itself sends. Emitted ONLY for a unit the registry
				// actually rendered a verb for: a subject the registry refuses gets no read line either.
				if rendered && p.Name == opschema.ParamUnit {
					seen[ssh.RemoteCommand(ssh.ServiceActiveProbeArgv(s))] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}

// manifestDoc is the slice of GET /v1/manifest this tool reads. Only the fields that decide subject
// membership are parsed; everything else on that surface is deliberately ignored here.
type manifestDoc struct {
	Entries []struct {
		Name          string `json:"name"`
		Status        string `json:"status"`
		AllowlistKind string `json:"allowlist_kind"`
	} `json:"entries"`
}

// SubjectsFromManifest extracts the host-guard subjects from a manifest document: the units and containers
// an operator has ADOPTED (approved, or stale-but-still-granted).
//
// A DRAFT CONTRIBUTES NOTHING. That is the whole security property of this function: discovery finding a
// unit must never authorise it at the host, because then discovery would be authorising itself and the
// operator's review would be decorative. Only a status that materialises in the control plane materialises
// here, and the kind comes from the SAME server-computed field the leaf allowlists use, so the host guard
// and the leaf gate can never disagree about which list a subject belongs in.
func SubjectsFromManifest(raw []byte) (units, containers []string, err error) {
	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	for _, e := range doc.Entries {
		// The granted set, mirroring the control plane exactly: approved AND stale (a stale entry is
		// still granted — discovery losing sight of a unit must not silently narrow the host guard either).
		if e.Status != "approved" && e.Status != "retired_candidate_stale" {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		switch e.AllowlistKind {
		case "unit":
			units = append(units, name)
		case "container":
			containers = append(containers, name)
		}
		// Any other kind (guest, or an entry that materialises into nothing) is not a host-guard
		// subject: the guard authorises systemctl and docker commands, not hypervisor calls.
	}
	return units, containers, nil
}
