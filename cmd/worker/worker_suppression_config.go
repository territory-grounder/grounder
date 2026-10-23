package main

// Freeze / suppression / cronicle-maintenance CONFIG loaders, carved out of main.go's composition
// root (TG-501 LOC-debt paydown). Pure config-not-code parsers + the maintenance-window projection:
// each reads an operator-declared JSON/env source and fails toward investigating (an absent or broken
// declaration grants no suppression, never a dead worker). Behaviour is unchanged by the move; the
// characterization tests (suppress_test.go, cronicle_freeze_test.go, worker_suppression_config_test.go)
// pin it, and the wiring-inventory guard proves no composition line was lost.

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/schedule"
	"github.com/territory-grounder/grounder/core/suppression"
	"github.com/territory-grounder/grounder/modules/schedule/cronicle"
)

// freezeWindows reads operator-declared maintenance/chaos freeze windows from a JSON file (config-not-code):
// [{"scope":"host-or-rule-or-empty","start":"RFC3339","end":"RFC3339","reason":"..."}]. An empty path, an
// unreadable/parse-broken file, or a malformed/inverted row yields no window — fail toward investigating (a
// freeze is a deliberate declaration; absent one, the alert is triaged, never silently dropped).
func freezeWindows(path string) []suppression.FreezeWindow {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read freeze file %q: %v (no freeze windows)", path, err)
		return nil
	}
	var rows []struct{ Scope, Start, End, Reason string }
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad freeze file %q: %v (no freeze windows)", path, err)
		return nil
	}
	var out []suppression.FreezeWindow
	for _, r := range rows {
		start, e1 := time.Parse(time.RFC3339, r.Start)
		end, e2 := time.Parse(time.RFC3339, r.End)
		if e1 != nil || e2 != nil || !end.After(start) {
			continue // skip malformed / inverted windows
		}
		out = append(out, suppression.FreezeWindow{Scope: r.Scope, Start: start, End: end, Reason: r.Reason})
	}
	return out
}

// cronicleProviders parses TG_CRONICLE_DEPLOYMENTS (config-not-code, grammar `id|baseurl|keyref[|dur][|ca]`,
// semicolon-separated) into a live read-only provider per declared Cronicle instance. It is the composition
// root the spec/019 maintenance-window sensor was missing (TG-411): unset/blank env yields NO providers, so
// the sensor stays dark and the default deployment is byte-for-byte unchanged (inert by default). A row that
// cannot build a client (bad url/keyref/CA) is skipped with a log line rather than failing the worker — a
// broken scheduler declaration grants no window, never a dead worker.
func cronicleProviders(spec string) []*cronicle.Provider {
	var out []*cronicle.Provider
	for _, d := range cronicle.ParseDeployments(spec) {
		p, err := cronicle.NewProviderFromDeployment(d)
		if err != nil {
			log.Printf("cronicle: deployment %q not wired: %v (its maintenance windows are dark)", d.ID, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// maintenanceFreezeWindows projects the ACTIVE maintenance windows of an already-read schedule onto absolute
// suppression freeze windows: a sanctioned maintenance window (schedule.KindMaintenance) is exactly the span
// in which the change's expected alerts should be suppressed before spending a session (spec/019 → spec/005
// tier-1). Only maintenance-kind windows are projected — a change-FREEZE window (no change is sanctioned) is
// not an expected-alert span and is left to normal triage. A wildcard target ("" or "*") becomes an
// estate-wide freeze (Scope ""); any other target passes through as the FreezeGate's exact host/rule scope.
// This is the pure projection (no I/O) so the oracle can drive it from a constructed Calendar.
func maintenanceFreezeWindows(cal schedule.Calendar, now time.Time) []suppression.FreezeWindow {
	var out []suppression.FreezeWindow
	for _, w := range cal.MaintenanceWindows() {
		start, end, ok := w.ActiveSpan(now)
		if !ok {
			continue // not currently inside an occurrence — nothing to freeze right now
		}
		scope := scheduleScope(w.Target)
		// The FreezeGate matches scope EXACTLY (not path.Match), so a GLOB target ("dc1*") would be a
		// window that is genuinely active in the scheduler yet never equals a concrete alert host — a silent
		// no-op. Drop it VISIBLY rather than project a window that can never match (no silent caps): the
		// operator sees the skip and can pin the event to a concrete host, or use '*' for estate-wide.
		if scope != "" && strings.ContainsAny(scope, "*?[") {
			log.Printf("cronicle: maintenance window %q targets a glob scope %q the freeze plane can't match "+
				"exactly — skipped (pin the Cronicle event to a concrete host, or '*' for the whole estate)", w.Title, w.Target)
			continue
		}
		out = append(out, suppression.FreezeWindow{
			Scope:  scope,
			Start:  start,
			End:    end,
			Reason: "cronicle maintenance window: " + w.Title,
		})
	}
	return out
}

// scheduleScope maps a schedule window's target onto a FreezeGate scope: a whole-estate target ("" or "*")
// becomes the FreezeGate's estate-wide scope (""); any concrete host passes through. (The FreezeGate matches
// scope EXACTLY, so a glob target narrows to nothing there — acceptable for v1: maintenance windows are
// host- or estate-scoped in practice, and the alternative is silently over-freezing.)
func scheduleScope(target string) string {
	if target == "*" {
		return ""
	}
	return target
}

// cronicleFreezeWindows reads every live Cronicle provider and returns the union of their active maintenance
// windows as absolute freeze windows. It FAILS CLOSED per provider (REQ-1903): an unreadable schedule
// contributes NO window — the estate stays open to full triage rather than freezing on stale data — and a
// read failure of one provider never suppresses the others.
func cronicleFreezeWindows(ctx context.Context, providers []*cronicle.Provider, now time.Time) []suppression.FreezeWindow {
	var out []suppression.FreezeWindow
	for _, p := range providers {
		cal, _, err := p.Snapshot(ctx)
		if err != nil {
			log.Printf("cronicle: schedule read failed (%v) — no maintenance freeze from it this cycle (fail-closed)", err)
			continue
		}
		out = append(out, maintenanceFreezeWindows(cal, now)...)
	}
	return out
}

// suppressRules reads operator-declared active-memory suppress rules from a JSON file (config-not-code):
// [{"host":"glob","rule":"glob","reason":"..."}] (path.Match globs; either side "*" for any). An empty
// path, a broken file, or a CATCH-ALL rule (both patterns "*") is refused — a catch-all would silence every
// non-critical alert, so it is dropped with a warning rather than suppressing the whole estate. A malformed
// glob matches nothing (the stage fails open), and critical/unknown severity is never suppressed by a rule.
func suppressRules(path string) []suppression.SuppressRule {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read rules file %q: %v (no operator rules)", path, err)
		return nil
	}
	var rows []struct{ Host, Rule, Reason string }
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad rules file %q: %v (no operator rules)", path, err)
		return nil
	}
	var out []suppression.SuppressRule
	for _, r := range rows {
		if r.Host == "*" && r.Rule == "*" {
			log.Printf("suppression: refusing a catch-all operator rule (host=* rule=*) — it would suppress the whole estate")
			continue
		}
		out = append(out, suppression.SuppressRule{HostPattern: r.Host, RulePattern: r.Rule, Reason: r.Reason})
	}
	return out
}

// suppressPatterns reads operator-declared known-transient patterns from a JSON file (config-not-code):
// [{"alert_rule":"...","estate":"...","confidence":0.8}]. A DECLARED pattern (no LastSeen) has no recency
// gate, but the stage still requires confidence >= 0.7 AND a transient-nature keyword in the rule
// (flap/blip/recover/…) to suppress — so a standing fault like "DiskFull" is never auto-suppressed. An empty
// path, a broken file, or a rule-less row yields no pattern.
func suppressPatterns(path string) []suppression.TransientPattern {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read patterns file %q: %v (no patterns)", path, err)
		return nil
	}
	var rows []struct {
		AlertRule  string  `json:"alert_rule"`
		Estate     string  `json:"estate"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad patterns file %q: %v (no patterns)", path, err)
		return nil
	}
	var out []suppression.TransientPattern
	for _, r := range rows {
		if r.AlertRule == "" {
			continue
		}
		out = append(out, suppression.TransientPattern{AlertRule: r.AlertRule, Estate: r.Estate, Confidence: r.Confidence})
	}
	return out
}

// suppressSchedules reads operator-declared recurring reboot schedules from a JSON file (config-not-code):
// [{"host":"...","cron":"0 3 * * *","timezone":"Europe/Athens","valid_from":"RFC3339","valid_until":"RFC3339"}].
// A declared schedule is registered LIVE (an operator declaration IS the authorization — no observe-before-
// live), so a reboot-class alert on that host inside the DST-correct cron window (± tolerance) is suppressed.
// A row without a host or cron is skipped. Empty ⇒ no DECLARED scheduled-reboot suppression.
//
// Source is DECLARED explicitly: it is what keeps this lane on its current behavior when the LEARNED lane is
// armed beside it — a declared row is never subject to observe-before-live, and never to the learned lane's
// governance-demotion consult (an operator declaration is not TG's to revoke).
func suppressSchedules(path string) []suppression.Schedule {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read schedules file %q: %v (no schedules)", path, err)
		return nil
	}
	var rows []struct {
		Host       string `json:"host"`
		Cron       string `json:"cron"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"valid_from"`
		ValidUntil string `json:"valid_until"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad schedules file %q: %v (no schedules)", path, err)
		return nil
	}
	var out []suppression.Schedule
	for _, r := range rows {
		if r.Host == "" || r.Cron == "" {
			continue
		}
		sc := suppression.Schedule{Host: r.Host, Cron: r.Cron, Timezone: r.Timezone, Kind: "declared", Source: suppression.SourceDeclared, Status: suppression.SchLive}
		if t, e := time.Parse(time.RFC3339, r.ValidFrom); e == nil {
			sc.ValidFrom = t
		}
		if t, e := time.Parse(time.RFC3339, r.ValidUntil); e == nil {
			sc.ValidUntil = t
		}
		out = append(out, sc)
	}
	return out
}

// foldPolicies reads operator-declared blast-radius fold policies from a JSON file (config-not-code):
// [{"host":"child-host-or-*","rule":"child-rule-or-*","site":"...","valid_from":"RFC3339","valid_until":"RFC3339"}].
// A matching CHILD alert is folded — posted as a notice, no session — while the policy is valid. An
// operator-declared policy is treated as verified-at-load (LastVerifiedAt = now) with an effectively-infinite
// freshness, because it has no learned staleness failure mode; only its valid window gates it. A catch-all
// (host=* rule=*) is refused so a config slip cannot fold the whole estate into silent notices.
func foldPolicies(path string) []suppression.SuppressionPolicy {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read folds file %q: %v (no fold policies)", path, err)
		return nil
	}
	var rows []struct {
		Host       string `json:"host"`
		Rule       string `json:"rule"`
		Site       string `json:"site"`
		ValidFrom  string `json:"valid_from"`
		ValidUntil string `json:"valid_until"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad folds file %q: %v (no fold policies)", path, err)
		return nil
	}
	now := time.Now()
	var out []suppression.SuppressionPolicy
	for _, r := range rows {
		if r.Host == "" && r.Rule == "" {
			continue
		}
		if r.Host == "*" && r.Rule == "*" {
			log.Printf("suppression: refusing a catch-all fold policy (host=* rule=*) — it would fold the whole estate to notices")
			continue
		}
		p := suppression.SuppressionPolicy{HostScope: r.Host, RuleScope: r.Rule, Site: r.Site, LastVerifiedAt: now}
		if t, e := time.Parse(time.RFC3339, r.ValidFrom); e == nil {
			p.ValidFrom = t
		}
		if t, e := time.Parse(time.RFC3339, r.ValidUntil); e == nil {
			p.ValidUntil = t
		}
		out = append(out, p)
	}
	return out
}
