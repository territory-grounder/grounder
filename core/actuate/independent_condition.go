package actuate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// IndependentConditionLookback is how far back the guest lane reads the task log. It is longer than the triage
// attribution window on purpose: a deliberate stop stays deliberate after the triage window has passed.
const IndependentConditionLookback = 7 * 24 * time.Hour

// IndependentRecentRun is how recently a guest must have been running for a start to be considered a heal. A guest
// that has been off longer than this is not a fresh outage, whatever the durable records claim.
const IndependentRecentRun = 2 * time.Hour

// ConditionChecker is what the chain consults. It is an interface so a process can install it after the chain exists.
type ConditionChecker interface {
	Check(ctx context.Context, a manifest.Action) (allow bool, reason string, ok bool)
	CheckUndo(ctx context.Context, inverse manifest.Action, facts UndoFacts) (allow bool, reason string, ok bool)
}

// ResourceBinding names one service or container the actuation plane may act on, on one host, and the live check of
// that exact object. The check is what ties an action to its target: an alert on the same host about something else
// authorizes nothing.
type ResourceBinding struct {
	Kind  string
	Name  string
	Probe string
	Ops   []string
}

// ProbeResult is one live check of a bound object.
type ProbeResult int

const (
	ProbeUnknown ProbeResult = iota
	ProbeDown
	ProbeUp
)

// DuplicateWindow is how soon the same action on the same target is treated as a repeat. The actuation plane's own
// execution record decides this, so a replay under a new incident reference is still a repeat.
const DuplicateWindow = 10 * time.Minute

// ExecutionRecord is the actuation plane's own record of the last run of an action.
type ExecutionRecord struct {
	At           time.Time
	Verdict      string
	Unverifiable bool
}

// serviceProbeGap separates the two samples a service action needs, so one transient failure cannot authorize it.
const serviceProbeGap = 3 * time.Second

// IndependentCondition answers, from sources the triage plane cannot write, whether this specific action on this
// specific target is warranted. The triage plane authors every durable record an execution rests on, so those records
// cannot be the authority against a compromised triage plane. It can only withhold an action.
type IndependentCondition struct {
	GuestEvidence func(ctx context.Context, guest string, since, until time.Time) ([]attribution.Evidence, error)
	Classify      func(guest string, ev []attribution.Evidence) attribution.Taxonomy
	RanRecently   func(ctx context.Context, guest string, within time.Duration) (bool, error)
	AlertingHosts func(ctx context.Context) (map[string]bool, error)
	ServiceActive func(ctx context.Context, host, unit string) (active bool, ok bool)
	Bindings      map[string][]ResourceBinding
	Probe         func(ctx context.Context, target string) ProbeResult
	LastExecuted  func(ctx context.Context, actionID string) (rec ExecutionRecord, found bool, err error)
	ProbeGap      time.Duration
	Now           func() time.Time
}

const guestStartOpClass = "start-guest"

var deliberateStopVerbs = map[string]bool{
	"vzstop": true, "vzshutdown": true, "vzsuspend": true,
	"qmstop": true, "qmshutdown": true, "qmsuspend": true,
}

// Check returns allow=false with a reason when the action must not run, and ok=false when a source could not answer.
func (c *IndependentCondition) Check(ctx context.Context, a manifest.Action) (allow bool, reason string, ok bool) {
	host := ResourceHost(ResourcePath(a))
	if host == "" || host == ResourceKeyNoTarget {
		return false, "the action names no host the condition can be checked for", true
	}
	spec, known := opschema.Lookup(a.OpClass)
	if !known {
		return false, fmt.Sprintf("op class %q has no independent source", a.OpClass), true
	}
	note, reason, ok := c.checkRepeat(ctx, a)
	if reason != "" || !ok {
		return false, reason, ok
	}
	allow, reason, ok = c.checkFamily(ctx, a, spec.Family, host)
	if allow {
		reason = note + reason
	}
	return allow, reason, ok
}

func (c *IndependentCondition) checkFamily(ctx context.Context, a manifest.Action, family, host string) (bool, string, bool) {
	switch family {
	case opschema.FamilyGuestLifecycle:
		if a.OpClass != guestStartOpClass {
			return false, fmt.Sprintf("op class %q is not an allowed forward guest action", a.OpClass), true
		}
		if safety.IsStatefulWorkload(a.SafetyParts()...) {
			return false, fmt.Sprintf("%q names a stateful workload, which is never started automatically", host), true
		}
		return c.checkGuest(ctx, host)
	case opschema.FamilyServiceLifecycle:
		return c.checkResource(ctx, a, host, "unit", a.Params[opschema.ParamUnit])
	case opschema.FamilyContainerLifecycle:
		return c.checkResource(ctx, a, host, "container", a.Params[opschema.ParamContainer])
	}
	return false, fmt.Sprintf("op class %q has no independent source", a.OpClass), true
}

// UndoFacts are the actuation plane's own facts about an undo: its execution record, and whether a manual order carries
// a valid grounder signature.
type UndoFacts struct {
	Found        bool
	TargetHost   string
	ExecutedAt   time.Time
	Verdict      string
	Unverifiable bool
	AutoFired    bool
	Signed       bool
	// AlreadyUndone is the actuation plane's own record that an inverse of this forward action already ran.
	AlreadyUndone bool
	// Forward is the sealed action behind the executed action id. Its content is bound to that id by hash, so an undo
	// is compared with what really ran. Nil when no sealed record matches.
	Forward *manifest.Action
}

// AutoUndoMaxAge bounds an automatic undo. A commit-confirm window is minutes long and the hold watch can fire later, so
// this is an hour rather than the window itself.
const AutoUndoMaxAge = time.Hour

// CheckUndo allows an undo only for an execution the actuation plane recorded on the same target. A manual undo must
// carry the grounder's signature. An automatic undo must be recent and answer a non-success the actuation plane itself
// recorded: a deviation verdict, or an unverified execution while monitoring shows an active alert on the target.
func (c *IndependentCondition) CheckUndo(ctx context.Context, inverse manifest.Action, f UndoFacts) (bool, string, bool) {
	if !f.Found {
		return false, "no execution of the forward action is on record", true
	}
	if f.AlreadyUndone {
		return false, "this action has already been undone", true
	}
	target := ResourceHost(ResourcePath(inverse))
	if target == "" || target != ResourceHost(ResourcePath(manifest.Action{Target: f.TargetHost})) {
		return false, fmt.Sprintf("the recorded forward action ran on %q, not on %q", f.TargetHost, target), true
	}
	if reason := undoMismatch(inverse, f.Forward); reason != "" {
		return false, reason, true
	}
	now := c.clock()
	if now.Sub(f.ExecutedAt) > IndependentConditionLookback {
		return false, "the recorded forward action is too old to undo", true
	}
	if !f.AutoFired {
		if !f.Signed {
			return false, "a manual undo must carry the grounder's signature", true
		}
		return true, "signed manual undo of a recorded execution", true
	}
	if now.Sub(f.ExecutedAt) > AutoUndoMaxAge {
		return false, "an automatic undo is only allowed within an hour of the action it reverses", true
	}
	switch {
	case f.Verdict == "partial" || f.Verdict == "deviation":
		return true, "the recorded verdict is a deviation", true
	case f.Verdict == "match" && !f.Unverifiable:
		return false, "the recorded verdict is a match, so there is nothing to undo", true
	}
	return c.alertOnHost(ctx, target, "an unverified execution may only be undone while monitoring shows an active alert on it")
}

// undoMismatch says why an undo is not the undo of this forward action: it must act on the same object, in the forward
// class or the class declared as its inverse.
func undoMismatch(inverse manifest.Action, forward *manifest.Action) string {
	if forward == nil {
		return "no sealed record of the forward action matches the executed action id"
	}
	if got, want := ResourcePath(inverse), ResourcePath(*forward); got != want {
		return fmt.Sprintf("the undo names %q but the forward action acted on %q", got, want)
	}
	if !undoClassAllowed(inverse.OpClass, forward.OpClass) {
		return fmt.Sprintf("op class %q is not an undo of %q", inverse.OpClass, forward.OpClass)
	}
	return ""
}

func undoClassAllowed(inverse, forward string) bool {
	inv := strings.ToLower(strings.TrimSpace(inverse))
	if inv == "" {
		return false
	}
	if inv == strings.ToLower(strings.TrimSpace(forward)) {
		return true
	}
	spec, known := opschema.Lookup(forward)
	return known && inv == strings.ToLower(strings.TrimSpace(spec.RollbackOpClass))
}

// checkRepeat decides what a recent run of the same action means. A prior run recorded as a clean success is followed by
// a new fault, so it is a recurrence and goes on to the live checks, which refute it if the object is healthy. A prior
// run with any other result is refused for now with a reason that says when it can be reconsidered: the next delivery of
// the fault after that time is judged afresh. It returns a note for a recurrence, a reason for a refusal, or ok=false.
func (c *IndependentCondition) checkRepeat(ctx context.Context, a manifest.Action) (note, reason string, ok bool) {
	if c.LastExecuted == nil {
		return "", "", false
	}
	id, err := a.ID()
	if err != nil {
		return "", "", false
	}
	rec, found, err := c.LastExecuted(ctx, id)
	if err != nil {
		return "", "", false
	}
	age := c.clock().Sub(rec.At)
	if !found || age >= DuplicateWindow {
		return "", "", true
	}
	if rec.Verdict == "match" && !rec.Unverifiable {
		return fmt.Sprintf("recurrence %s after a recorded successful run: ", age.Round(time.Second)), "", true
	}
	return "", fmt.Sprintf("the same action ran %s ago with result %q, so a repeat waits: it will be reconsidered if the fault is delivered again in %s or later",
		age.Round(time.Second), resultLabel(rec), (DuplicateWindow - age).Round(time.Second)), true
}

func resultLabel(rec ExecutionRecord) string {
	if rec.Unverifiable {
		return "unverified"
	}
	if rec.Verdict == "" {
		return "unknown"
	}
	return rec.Verdict
}

func (c *IndependentCondition) checkGuest(ctx context.Context, guest string) (bool, string, bool) {
	if c.GuestEvidence == nil || c.Classify == nil || c.RanRecently == nil {
		return false, "", false
	}
	now := c.clock()
	ev, err := c.GuestEvidence(ctx, guest, now.Add(-IndependentConditionLookback), now)
	if err != nil {
		return false, "", false
	}
	if latest, found := latestLifecycleEvent(ev); found && deliberateStopVerbs[strings.ToLower(latest.ActionKind)] {
		switch c.Classify(guest, []attribution.Evidence{latest}) {
		case attribution.AttributedAuthorized:
			return false, fmt.Sprintf("the latest stop of %q was made deliberately by %s", guest, latest.Actor), true
		case attribution.AttributedSuspicious:
			return false, fmt.Sprintf("the latest stop of %q was made by an unsanctioned actor %s", guest, latest.Actor), true
		}
	}
	ran, err := c.RanRecently(ctx, guest, IndependentRecentRun)
	if err != nil {
		return false, "", false
	}
	if !ran {
		return false, fmt.Sprintf("the hypervisor shows %q has not been running recently, so this is not a fresh outage", guest), true
	}
	return true, "the guest ran recently and its latest stop is not attributed to a person", true
}

func (c *IndependentCondition) checkResource(ctx context.Context, a manifest.Action, host, kind, name string) (bool, string, bool) {
	binding, found := c.binding(host, kind, NormalizeBoundObject(kind, name))
	if !found {
		return false, fmt.Sprintf("%s %q is not configured for autonomous action on %q", kind, name, host), true
	}
	if !containsOp(binding.Ops, a.OpClass) {
		return false, fmt.Sprintf("action %q is not configured for %s %q on %q", a.OpClass, kind, name, host), true
	}
	if a.OpClass == "start-service" {
		if c.ServiceActive == nil {
			return false, "", false
		}
		active, ok := c.ServiceActive(ctx, host, name)
		if !ok {
			return false, "", false
		}
		if active {
			return false, fmt.Sprintf("unit %q is already active on %q, so there is nothing to start", name, host), true
		}
	}
	return c.checkProbe(ctx, binding, host)
}

func containsOp(ops []string, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func (c *IndependentCondition) checkProbe(ctx context.Context, b ResourceBinding, host string) (bool, string, bool) {
	if c.Probe == nil || b.Probe == "" {
		return false, "", false
	}
	first := c.Probe(ctx, b.Probe)
	gap := c.ProbeGap
	if gap == 0 {
		gap = serviceProbeGap
	}
	select {
	case <-ctx.Done():
		return false, "", false
	case <-time.After(gap):
	}
	second := c.Probe(ctx, b.Probe)
	switch {
	case first == ProbeUnknown || second == ProbeUnknown:
		return false, "", false
	case first == ProbeUp && second == ProbeUp:
		return false, fmt.Sprintf("%s %q on %q is answering its own check, so there is no fault for this action to fix", b.Kind, b.Name, host), true
	case first != second:
		return false, fmt.Sprintf("the check of %s %q on %q gave conflicting answers, so it cannot justify this action", b.Kind, b.Name, host), true
	}
	return true, fmt.Sprintf("%s %q on %q failed its own check twice", b.Kind, b.Name, host), true
}

// NormalizeBoundObject treats "nginx" and "nginx.service" as one unit, because systemd does. It changes nothing else:
// case, other unit types such as nginx.socket, template instances and container names all stay exact.
func NormalizeBoundObject(kind, name string) string {
	name = strings.TrimSpace(name)
	if kind == "unit" {
		return strings.TrimSuffix(name, ".service")
	}
	return name
}

func (c *IndependentCondition) binding(host, kind, name string) (ResourceBinding, bool) {
	var match ResourceBinding
	n := 0
	for _, b := range c.Bindings[host] {
		if b.Kind == kind && b.Name == name {
			match = b
			n++
		}
	}
	return match, n == 1
}

func (c *IndependentCondition) alertOnHost(ctx context.Context, host, refusal string) (bool, string, bool) {
	if c.AlertingHosts == nil {
		return false, "", false
	}
	hosts, err := c.AlertingHosts(ctx)
	if err != nil {
		return false, "", false
	}
	if !hosts[host] {
		if refusal == "" {
			refusal = fmt.Sprintf("the monitoring system shows no active alert on %q", host)
		}
		return false, refusal, true
	}
	return true, "the monitoring system shows an active alert on the host", true
}

func (c *IndependentCondition) clock() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func latestLifecycleEvent(ev []attribution.Evidence) (attribution.Evidence, bool) {
	var latest attribution.Evidence
	found := false
	for _, e := range ev {
		if strings.TrimSpace(e.ActionKind) == "" {
			continue
		}
		if !found || e.ObservedAt.After(latest.ObservedAt) {
			latest, found = e, true
		}
	}
	return latest, found
}

// GuestStopClassifier classifies a guest's latest stop with the deployment's attribution rules. The attribution
// window is widened to the lookback, because the shorter triage window would drop an older stop and report nobody.
func GuestStopClassifier(cfg attribution.Config) func(string, []attribution.Evidence) attribution.Taxonomy {
	cfg.Window = IndependentConditionLookback + time.Minute
	return func(guest string, ev []attribution.Evidence) attribution.Taxonomy {
		return attribution.Attribute(guest, "start-guest", ev, nil, cfg).Taxonomy
	}
}

// AlertingHostSet normalizes monitoring host names to the form the scope and this check compare.
func AlertingHostSet(hosts []string) map[string]bool {
	out := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if n := ResourceHost(ResourcePath(manifest.Action{Target: h})); n != "" && n != ResourceKeyNoTarget {
			out[n] = true
		}
	}
	return out
}
