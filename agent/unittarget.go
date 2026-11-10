package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/manifest"
)

// UnitTargetGate holds a proposal ONCE when its systemd unit is not grounded in the host's own unit list (TG-647).
//
// Seen live on 2026-11-06 17:24 NL: on a memory-pressure alert the agent read the host's top-memory process table
// (process names only), took `uvicorn` from it, and proposed restart-service with unit `uvicorn`. `uvicorn` is a
// process, not a unit. `systemctl restart uvicorn` exited 5 at the host and nothing changed: a safe failure, but a
// wasted heal that left the real cause in place. The session never read check-host-services, which lists the
// host's units and the owning unit of every process.
//
// The gate does NOT run the read itself and does NOT veto. When the unit is not established it nudges once, with
// the exact call or with the reason, and the agent answers through the normal tool and proposal paths. A repeat
// proposal is accepted, as with every other one-shot nudge in this loop: the host's own guard and the effect
// leaf still stand behind it. The tool comes from the RUNNER, never from model text (INV-08).
type UnitTargetGate struct {
	Tool string // the host unit-list read, e.g. check-host-services
}

// unitParam is the op-schema parameter that names a systemd unit (restart-service, start-service).
const unitParam = "unit"

// hold returns the one-time nudge for a proposal whose unit is not grounded, or "" when the proposal passes: the
// gate is unset or its tool is absent, the action names no unit or no host, or the unit appears AS A UNIT in a
// successful read of that host.
func (g *UnitTargetGate) hold(tools *ToolSet, a manifest.Action, gathered []ToolResult) string {
	unit := strings.TrimSpace(a.Params[unitParam])
	host := strings.TrimSpace(a.Target)
	if g == nil || strings.TrimSpace(g.Tool) == "" || tools == nil || unit == "" || host == "" {
		return ""
	}
	if _, ok := tools.Get(g.Tool); !ok {
		return ""
	}
	read := false
	for _, r := range gathered {
		if r.Tool != g.Tool || !r.Success || !sameHost(r.Target, host) {
			continue
		}
		read = true
		if listsUnit(r.Output, unit) {
			return ""
		}
	}
	call, _ := json.Marshal(map[string]any{"action": "tool", "tool": g.Tool, "args": map[string]string{"host": host}})
	if !read {
		return fmt.Sprintf("PROPOSAL NOT YET ACCEPTED: %s names systemd unit %q, but you have not read the host's unit list. "+
			"A process name from a process or memory table is not a unit name. Read it first: %s — then re-emit the "+
			"proposal with the unit exactly as listed there, or stop citing what you observed.", a.OpClass, unit, string(call))
	}
	return fmt.Sprintf("PROPOSAL NOT YET ACCEPTED: %s names systemd unit %q, and the host's unit lists you read (failed, "+
		"inactive, enabled) do not contain it. A process name is not a unit name. Re-emit the proposal with a unit from "+
		"those lists, or stop citing what you observed.", a.OpClass, unit)
}

// sameHost compares two host names on their short form, so `app01` and `app01.example.net` are one host.
func sameHost(a, b string) bool {
	short := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
		return h
	}
	return short(a) != "" && short(a) == short(b)
}

// listsUnit reports whether output names unit AS A UNIT: a whitespace-delimited token equal to the unit's full
// name (`nginx` is matched as `nginx.service`). A bare process name in a `comm` column therefore does not count,
// which is the whole point: that column is where `uvicorn` came from. A template instance (`foo@bar.service`)
// also matches its template file (`foo@.service`), which is how list-unit-files shows it.
//
// Known gap, accepted: a unit that runs but is neither enabled, failed nor inactive (a static unit, one started by
// hand) is listed nowhere, so it is held once. The repeat proposal is accepted, so the cost is one cycle.
func listsUnit(output, unit string) bool {
	want := strings.ToLower(strings.TrimSpace(unit))
	if !strings.Contains(want, ".") {
		want += ".service"
	}
	template := ""
	if at := strings.IndexByte(want, '@'); at > 0 {
		if dot := strings.LastIndexByte(want, '.'); dot > at {
			template = want[:at+1] + want[dot:]
		}
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(output), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '●' || r == '*' || r == ',' || r == ':'
	}) {
		if tok == want || (template != "" && tok == template) {
			return true
		}
	}
	return false
}
