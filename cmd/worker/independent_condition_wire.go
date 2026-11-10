package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	pveattr "github.com/territory-grounder/grounder/modules/actorevidence/pve"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

// independentConditionRequired reports whether this process refuses any effect the actuation plane has not checked
// against its own sources. The chain refuses when required and unwired, so a half-configured deployment fails closed.
func independentConditionRequired() bool {
	return conditionEnforced(planeEnv("TG_INDEPENDENT_CONDITION", ""))
}

// conditionEnforced treats anything but unset or "off" as enforcing, so a mistyped value cannot switch the check off.
func conditionEnforced(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	return v != "" && v != "off"
}

// activeCondition is the check every chain in this process consults. It is empty until the attribution rules load.
var activeCondition atomic.Pointer[actuate.IndependentCondition]

// conditionGate adapts activeCondition to the chain, and reads as unreadable until the check is installed.
type conditionGate struct{}

func (conditionGate) Check(ctx context.Context, a manifest.Action) (bool, string, bool) {
	c := activeCondition.Load()
	if c == nil {
		return false, "", false
	}
	return c.Check(ctx, a)
}

func (conditionGate) CheckUndo(ctx context.Context, inverse manifest.Action, f actuate.UndoFacts) (bool, string, bool) {
	c := activeCondition.Load()
	if c == nil {
		return false, "", false
	}
	return c.CheckUndo(ctx, inverse, f)
}

// wireIndependentCondition installs the actuation plane's own check when this process enforces it.
func wireIndependentCondition(cfg attribution.Config, pool *db.Pool) {
	if !independentConditionRequired() {
		log.Print("independent condition: OFF (TG_INDEPENDENT_CONDITION unset or off) — effects rest on the durable records alone")
		return
	}
	c := buildIndependentCondition(cfg, stateReadRecorderFor(pool))
	if pool != nil {
		store := db.NewActionExecutionStore(pool)
		c.LastExecuted = func(ctx context.Context, actionID string) (actuate.ExecutionRecord, bool, error) {
			e, found, err := store.LatestExecution(ctx, actionID)
			return actuate.ExecutionRecord{At: e.ExecutedAt, Verdict: e.Verdict, Unverifiable: e.Unverifiable}, found, err
		}
	}
	activeCondition.Store(c)
	probeIndependentCondition(c)
}

func buildIndependentCondition(cfg attribution.Config, rec stateReadRecorder) *actuate.IndependentCondition {
	c := &actuate.IndependentCondition{
		Classify: actuate.GuestStopClassifier(cfg),
	}
	url, ref := planeEnv("TG_INDEPENDENT_PVE_URL", ""), planeEnv("TG_INDEPENDENT_PVE_TOKEN_REF", "")
	if url != "" && ref != "" {
		var opts []pveattr.Option
		if truthyEnv("TG_PVE_INSECURE") {
			opts = append(opts, pveattr.WithHTTPClient(estateHTTPClient(true)))
		}
		reader := pveattr.New(url, config.SecretRef(ref), opts...)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := reader.CheckCanReadAllTasks(ctx); err != nil {
			log.Printf("independent condition: guest source DISABLED, every guest action will be refused: %v", err)
		} else {
			c.GuestEvidence, c.RanRecently = reader.Read, reader.RanRecently
		}
	}
	if deps := librenmsDeployments(planeEnv("TG_INDEPENDENT_LIBRENMS_DEPLOYMENTS", "")); len(deps) > 0 {
		src := librenms.NewAlertSource(deps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))))
		c.AlertingHosts = func(ctx context.Context) (map[string]bool, error) {
			envs, _, err := src.FetchActive(ctx)
			if err != nil {
				return nil, err
			}
			hosts := make([]string, 0, len(envs))
			for _, e := range envs {
				hosts = append(hosts, e.Host)
			}
			return actuate.AlertingHostSet(hosts), nil
		}
	}
	c.Bindings = parseResourceBindings(planeEnv("TG_INDEPENDENT_SSH_BINDINGS", ""))
	log.Printf("independent condition: %s", bindingSummary(c.Bindings))
	c.Probe = actuate.ProbeService
	c.ServiceActive = serviceActiveReader(planeEnv("TG_ACTUATION_SSH_KNOWN_HOSTS", ""), planeEnv("TG_ACTUATION_SSH_IDENTITY", ""), config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", "")), rec, "independent-condition")
	return c
}

// parseResourceBindings reads host|unit-or-container|name|probe|op,op rows. With no list, no service or
// container action is authorized. Any malformed row denies everything, because a typo must not lift the limit.
func parseResourceBindings(raw string) map[string][]actuate.ResourceBinding {
	out := map[string][]actuate.ResourceBinding{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	for _, row := range strings.Split(raw, ";") {
		if strings.TrimSpace(row) == "" {
			continue
		}
		f := strings.Split(strings.TrimSpace(row), "|")
		host := actuate.AlertingHostSet(f[:1])
		if len(f) != 5 || (f[1] != "unit" && f[1] != "container") || strings.TrimSpace(f[2]) == "" || !validProbe(f[3]) || strings.TrimSpace(f[4]) == "" || len(host) != 1 || !probeFitsBinding(f[0], f[3], f[4]) {
			log.Printf("independent condition: malformed binding row %q — every service and container action will be refused", row)
			return map[string][]actuate.ResourceBinding{}
		}
		if protectedBinding(strings.TrimSpace(f[0]), strings.TrimSpace(f[2])) {
			log.Printf("independent condition: binding %q names a never-touch or stateful object — every service and container action will be refused", row)
			return map[string][]actuate.ResourceBinding{}
		}
		for h := range host {
			out[h] = append(out[h], actuate.ResourceBinding{Kind: f[1], Name: actuate.NormalizeBoundObject(f[1], f[2]), Probe: strings.TrimSpace(f[3]), Ops: splitOps(f[4])})
		}
	}
	return out
}

// bindingSummary names every loaded binding for the boot log, so an empty list cannot read as a healthy one.
func bindingSummary(b map[string][]actuate.ResourceBinding) string {
	var rows []string
	for host, list := range b {
		for _, r := range list {
			rows = append(rows, host+"/"+r.Kind+"/"+r.Name+"["+strings.Join(r.Ops, ",")+"]")
		}
	}
	if len(rows) == 0 {
		return "0 service or container bindings — every service and container action will be refused"
	}
	sort.Strings(rows)
	return fmt.Sprintf("%d service or container binding(s): %s", len(rows), strings.Join(rows, " "))
}

func protectedBinding(host, name string) bool {
	return ssh.NeverTouchUnit(name) || safety.IsStatefulWorkload(host, name)
}

func splitOps(raw string) []string {
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func validProbe(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "tcp://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// probeFitsBinding refuses a probe that points at a different host than the binding, and a tcp probe on a restart
// action: a hung process still completes the TCP handshake, so tcp cannot show the fault a restart answers.
func probeFitsBinding(host, probe, ops string) bool {
	u, err := url.Parse(strings.TrimSpace(probe))
	if err != nil {
		return false
	}
	name := u.Hostname()
	if ip := net.ParseIP(name); ip != nil {
		if ip.IsLoopback() {
			return false
		}
	} else if !strings.EqualFold(strings.SplitN(name, ".", 2)[0], strings.SplitN(strings.TrimSpace(host), ".", 2)[0]) {
		return false
	}
	if u.Scheme == "tcp" && strings.Contains(ops, "restart-") {
		return false
	}
	return true
}

// probeIndependentCondition reads each source once at boot so a wrong credential shows up in Shadow, not on the
// first heal. It logs only; the gate itself is what refuses.
func probeIndependentCondition(c *actuate.IndependentCondition) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	guest := planeEnv("TG_INDEPENDENT_PVE_PROBE_GUEST", "")
	switch {
	case c.GuestEvidence == nil:
		log.Print("independent condition: guest source NOT configured — guest actions will be refused")
	case guest == "":
		log.Print("independent condition: guest source configured, not probed (TG_INDEPENDENT_PVE_PROBE_GUEST unset)")
	default:
		now := time.Now()
		if _, err := c.GuestEvidence(ctx, guest, now.Add(-time.Hour), now); err != nil {
			log.Printf("independent condition: guest source probe FAILED: %v", err)
		} else {
			log.Print("independent condition: guest source probe ok")
		}
	}
	if c.AlertingHosts == nil {
		log.Print("independent condition: alert source NOT configured — service and container actions will be refused")
		return
	}
	if hosts, err := c.AlertingHosts(ctx); err != nil {
		log.Printf("independent condition: alert source probe FAILED: %v", err)
	} else {
		log.Printf("independent condition: alert source probe ok (%d alerting host(s))", len(hosts))
	}
	log.Print("independent condition: ENFORCING — every effect is checked against the actuation plane's own sources")
}
