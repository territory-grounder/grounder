// Package episodes rebuilds the fault → alert → session → execution → credit attribution of an injection
// campaign from IMMUTABLE identifiers, so a heal is credited to exactly the fault it answered (TG-672, spec/025
// REQ-2534).
//
// WHY THIS EXISTS. The 2026-11-07 evidence pack attached one execution to two faults: fault 1670 (a container
// hang) and fault 1675 (a device-down on the same guest, injected 41 minutes later) both claimed the 18:29:05Z
// start-guest, because the attachment query joined alerts to faults by HOST and a window reaching ten minutes
// past each fault's restore deadline — and on a pool guest that is faulted every ~40 minutes those windows
// overlap for every consecutive pair (25 pairs in the cohort). The corrected rule is NON-OVERLAPPING
// OWNERSHIP: a fault owns its host from its injection until the EARLIER of the next injection on that host and
// its restore deadline plus a grace, so an alert, and everything keyed to that alert's external_ref, belongs to
// at most one fault. 1670's own alert produced no execution; 1675's did.
//
// The rules, stated once:
//
//   - OWNERSHIP: owner(alert) is the fault on the same host with injected_at <= received_at < own_until, where
//     own_until = min(next injection on the host, restore_due_at + OwnershipGrace). Intervals are disjoint per
//     host by construction, so there is at most one owner. Faults injected BEFORE the window are loaded too
//     (a lookback) so an early in-window alert finds its real owner instead of the first in-window fault.
//   - CLASS MATCH: the alert's rule must be compatible with the fault's class (device-down ↔ device/ICMP/SNMP
//     rules; the service, container and hang classes ↔ service/http rules; disk-fill and log-fill ↔ space/disk;
//     mem-pressure ↔ memory — the same vocabulary core/db's detectRuleMatch uses for A1). An owned alert of the
//     wrong class is counted as dropped, never attributed, and a class neither side knows NEVER matches: a
//     missing mapping fails closed, exactly as it does for A1.
//   - CAMPAIGN vs OWNERSHIP: every fault participates in ownership, whatever campaign stamped it — a drill or
//     another soak on the same host still ends the previous fault's interval. The note prefix selects which
//     faults are REPORTED as the cohort; a non-cohort fault that owns in-window evidence is listed beside it.
//   - ONE-TO-MANY is allowed: a fault may own several alerts (two sources, a re-fire), and every execution or
//     credit keyed to any of them is the fault's. MANY-TO-ONE is not: an alert has one owner.
//   - OVERLAP AND AMBIGUITY (TG-679; pre-registration v1 §5): two faults A and B on one host OVERLAP when B was
//     injected inside A's RAW window (A's injection to its restore deadline plus the grace). Ownership truncation
//     then assigns every piece of evidence to one of them, and the truncation is ACCEPTED only where evidence
//     separates the two: an execution BEFORE B was injected is A's by time; an execution at or after it is
//     separated when its op class is the one its owner's fault class is healed by and not the other's
//     (ExpectedOp; a fault type with no healing op never separates, on either side), or — for B's executions —
//     when A was HEALED first (an execution of A verified clean before B was injected); a credit of A verified
//     after B was injected stands only when its op class separates; an alert attributed to B whose rule class A could also raise is separated only when A was
//     restored before B was injected and the alert was OBSERVED (source time, not delivery time) at or after
//     B's injection (a fault of no known class could raise any alert). Consecutive same-host faults only: with a
//     fault between them, the earlier one's ownership already ended. Anything else leaves the pair `ambiguous`: both faults stay in every denominator and are
//     never credited; a credit they carry is disclosed as ambiguous_credited. Every overlapping pair is listed
//     (Report.Overlaps) with the rule that separated it or the evidence that could not be separated. The injector
//     keys busy and settle by HOST (one active fault per guest), so an overlap means B landed in A's +grace tail.
//   - UNMATCHED faults stay in every denominator: a fault with no owned alert is `no-alert`; one with alerts but
//     no execution is `alerted-no-exec`. An execution whose alert belongs to a fault outside the window is
//     reported as `owner-outside-window`; one whose alert no fault owns is `unattributed`. A credit keyed to an
//     unowned alert is listed too. A credit counts as CREDITED only with outcome `verified_clean` — the G3
//     ladder's own currency; any other outcome is carried, visible, uncounted. Nothing is dropped.
//
// Pure: the package has no database. core/db loads the rows; tools/episodes prints them.
package episodes

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// OwnershipGrace extends a fault's ownership past its restore deadline: the injector's reconcile restores a
// little after `restore_due_at`, and an alert landing in that tail still describes this fault.
const OwnershipGrace = 10 * time.Minute

// Class is the two-way vocabulary both the injector's fault types and the monitoring rules fold into.
type Class string

const (
	ClassGuest   Class = "guest"
	ClassService Class = "service"
	ClassDisk    Class = "disk"
	ClassMemory  Class = "memory"
	// ClassOther is "no mapping": a fault or rule neither vocabulary knows. It never matches anything, so an
	// unknown fault class cannot claim an unknown rule — the fail-closed rule core/db.detectRuleMatch states.
	ClassOther Class = "other"
)

// FaultClass folds the injector's closed fault_type enumeration onto Class (core/db.detectRuleMatch's rows).
func FaultClass(faultType string) Class {
	switch faultType {
	case "device-down":
		return ClassGuest
	case "service-down", "container-down", "container-hang", "service-hang":
		return ClassService
	case "disk-fill", "log-fill":
		return ClassDisk
	case "mem-pressure":
		return ClassMemory
	}
	return ClassOther
}

// RuleClass folds a monitoring rule name onto Class — the same patterns core/db.detectRuleMatch uses
// (core/db pins the two by test), kept case-insensitive. Order matters only where the patterns could overlap:
// a "Service" rule is a service rule even when its text also mentions a disk.
func RuleClass(rule string) Class {
	r := strings.ToLower(rule)
	switch {
	case strings.Contains(r, "device") && strings.Contains(r, "down"), strings.Contains(r, "icmp"), strings.Contains(r, "snmp"):
		return ClassGuest
	case strings.Contains(r, "service"), strings.Contains(r, "http"), strings.Contains(r, "nginx"):
		return ClassService
	case strings.Contains(r, "space"), strings.Contains(r, "disk"):
		return ClassDisk
	case strings.Contains(r, "memory"), strings.Contains(r, "mem"):
		return ClassMemory
	}
	return ClassOther
}

// classesMatch is the attribution test: equal, and known. ClassOther on either side is a missing mapping and
// fails closed.
func classesMatch(fault, rule Class) bool { return fault != ClassOther && fault == rule }

// CreditedOutcome is the one graduation_credit outcome that counts as a credit — the ladder's verified clean run.
const CreditedOutcome = "verified_clean"

// Fault is one injected_fault row. RestoreDueAt / RestoredAt are zero when the row carries none.
type Fault struct {
	ID           int64
	Host         string
	Node         string
	FaultType    string
	InjectedAt   time.Time
	RestoreDueAt time.Time
	RestoredAt   time.Time
	RestoreState string
	Virt         string
	Note         string
}

// Alert is one ingest_alert row (external_ref is unique there).
type Alert struct {
	ExternalRef string
	Host        string
	Rule        string
	ReceivedAt  time.Time
	ObservedAt  time.Time
	Source      string
	Site        string
}

// Execution is one action_execution row, with the op_class of the session it answered.
type Execution struct {
	ID          int64
	ExternalRef string
	ActionID    string
	OpClass     string
	ExecutedAt  time.Time
	Verdict     string
	Site        string
	TargetHost  string
}

// Credit is one graduation_credit row.
type Credit struct {
	ExternalRef string
	OpClass     string
	Outcome     string
	CreditedAt  time.Time
}

// Session is the session_triage row behind an external_ref (what the agent decided).
type Session struct {
	ExternalRef string
	OpClass     string
	Band        string
	Outcome     string
	HarnessFP   string
	Synthetic   bool
}

// Input is everything one attribution needs, already loaded. Faults should cover [Since − lookback,
// Until + lookback) — EVERY fault, whatever campaign stamped it — so an alert near either edge finds its real
// owner (a successor injected after the window still ends the last in-window fault's interval); alerts,
// executions and credits should cover the same span. NotePrefix selects the faults REPORTED as the cohort
// ("" = all); it never narrows ownership.
type Input struct {
	Since, Until time.Time
	NotePrefix   string
	Faults       []Fault
	Alerts       []Alert
	Executions   []Execution
	Credits      []Credit
	Sessions     map[string]Session
	// SiteOf attributes a fault to a declared site from its own record; nil uses DefaultSiteOf.
	SiteOf func(host, node string) string
}

// DefaultSiteOf mirrors core/db's faultSite: the Proxmox node first, else the host name, through the one
// site-vocabulary authority; node and host naming DIFFERENT declared sites refuses to guess ("").
func DefaultSiteOf(host, node string) string {
	n, h := ingest.CanonicalizeSite(node), ingest.CanonicalizeSite(host)
	nOK, hOK := ingest.IsDeclaredSite(n), ingest.IsDeclaredSite(h)
	switch {
	case nOK && hOK && n != h:
		return ""
	case nOK:
		return n
	case hOK:
		return h
	}
	return ""
}

// AttributedAlert is an alert with its owner's bookkeeping.
type AttributedAlert struct {
	Alert
	// RawOverlaps counts the OTHER same-host faults whose raw window (injection → restore deadline + grace) also
	// contains this alert — where a host+window join would have double-attributed it.
	RawOverlaps int
}

// Disposition values, per fault.
const (
	DispositionCredited        = "credited"
	DispositionExecutedNoCred  = "executed-no-credit"
	DispositionAlertedNoExec   = "alerted-no-exec"
	DispositionNoAlert         = "no-alert"
	DispositionOwnerOutside    = "owner-outside-window"
	DispositionOwnerOtherCamp  = "owner-outside-campaign"
	DispositionUnattributed    = "unattributed"
	DispositionWrongClassOwner = "owner-wrong-class"
	// DispositionAmbiguous (TG-679): the fault overlaps a same-host neighbour (Overlap) and some evidence of the
	// pair cannot be separated between the two — the ownership cut decided the attribution rather than observing
	// it. The fault stays in every denominator and is NEVER credited; its credits are disclosed as
	// ambiguous_credited.
	DispositionAmbiguous = "ambiguous"
)

// Episode is one fault with everything attributed to it.
type Episode struct {
	Fault
	Site              string
	Class             Class
	InCohort          bool
	OwnUntil          time.Time
	Alerts            []AttributedAlert
	DroppedWrongClass int
	// Ambiguous and AmbiguousWith (TG-679): see DispositionAmbiguous. AmbiguousWith lists the other faults, by
	// id, of the unseparated overlaps — symmetric: each names the other.
	Ambiguous     bool
	AmbiguousWith []int64
	Executions    []Execution
	// Credits are the owned graduation_credit rows with outcome verified_clean; OtherCredits the owned rows with
	// any other outcome — carried, visible, never counted as a credit.
	Credits      []Credit
	OtherCredits []Credit
	// Session is the owner's first attributed alert's session_triage row (what the agent decided), if loaded.
	Session   Session
	FirstExec time.Time
	// ExecBeforeDue / ExecBeforeReconcile are meaningful only when FirstExec is set: did TG act before the
	// injector's deadline, and before the injector actually restored the guest.
	ExecBeforeDue       bool
	ExecBeforeReconcile bool
	Disposition         string
}

// UnownedExecution is an execution the cohort cannot claim, with the reason.
type UnownedExecution struct {
	Execution
	Reason  string
	OwnerID int64
}

// UnownedCredit is a credit the cohort cannot claim, with the reason.
type UnownedCredit struct {
	Credit
	Reason  string
	OwnerID int64
}

// Counts is one cell of the summary.
type Counts struct {
	Faults           int `json:"faults"`
	Alerted          int `json:"alerted"`
	NoAlert          int `json:"no_alert"`
	Executed         int `json:"executed"`
	Credited         int `json:"credited"`
	ExecutedNoCredit int `json:"executed_no_credit"`
	Ambiguous        int `json:"ambiguous"`
	// AmbiguousCredited counts ambiguous faults that carried a verified_clean credit: disclosed, never in Credited.
	AmbiguousCredited int `json:"ambiguous_credited"`
	ExecBeforeDue     int `json:"exec_before_due"`
	ExecBeforeRecon   int `json:"exec_before_reconcile"`
	Executions        int `json:"executions"`
}

// Summary is the cohort's disposition table.
type Summary struct {
	Since, Until              time.Time         `json:"-"`
	Total                     Counts            `json:"total"`
	BySite                    map[string]Counts `json:"by_site"`
	ByFaultType               map[string]Counts `json:"by_fault_type"`
	ByVirt                    map[string]Counts `json:"by_virt"`
	BySiteFaultTypeVirt       map[string]Counts `json:"by_site_fault_type_virt"`
	ExecutionsAttributed      int               `json:"executions_attributed"`
	ExecutionsOwnerOutside    int               `json:"executions_owner_outside_window_or_campaign"`
	ExecutionsOwnerWrongClass int               `json:"executions_owner_wrong_class"`
	ExecutionsUnattributed    int               `json:"executions_unattributed"`
	CreditsAttributed         int               `json:"credits_attributed"`
	CreditsAmbiguous          int               `json:"credits_ambiguous"`
	OverlapPairs              int               `json:"overlap_pairs"`
	OverlapsSeparated         int               `json:"overlaps_separated"`
	OverlapsAmbiguous         int               `json:"overlaps_ambiguous"`
	CreditsOtherOutcome       int               `json:"credits_other_outcome"`
	CreditsUnowned            int               `json:"credits_unowned"`
	AlertsOwnedWrongClass     int               `json:"alerts_owned_wrong_class"`
	AlertsUnowned             int               `json:"alerts_unowned"`
	MaxGuestShareOfExecutions map[string]string `json:"max_guest_share_of_executions"`
}

// Report is Attribute's output.
type Report struct {
	Since, Until      time.Time
	Episodes          []Episode
	OwnersOutside     []Episode
	UnownedExecutions []UnownedExecution
	UnownedCredits    []UnownedCredit
	// Overlaps lists every same-host pair where B was injected inside A's raw window and at least one of the two
	// is in the cohort, with the evidence and the rule that separated it (or did not).
	Overlaps []Overlap
	Summary  Summary
}

// ExpectedOp is the op class a fault type is HEALED by — the one-to-one map the G3 ladder credits per op class
// (device-down → start-guest, container-down → start-container, container-hang → restart-container, service-down →
// start-service, service-hang → restart-service). "" for a fault type with no op class of its own: it never
// separates anything.
func ExpectedOp(faultType string) string {
	switch faultType {
	case "device-down":
		return "start-guest"
	case "container-down":
		return "start-container"
	case "container-hang":
		return "restart-container"
	case "service-down":
		return "start-service"
	case "service-hang":
		return "restart-service"
	}
	return ""
}

// opSeparates reports whether an execution of op class op, attributed to a fault of type owner, is told apart from
// the other fault of the pair by its op class: it is the owner's healing op and not the other's.
func opSeparates(op, owner, other string) bool {
	want, theirs := ExpectedOp(owner), ExpectedOp(other)
	return want != "" && theirs != "" && op == want && op != theirs
}

// Overlap is one same-host pair whose raw windows overlap (B injected inside A's raw window), with the evidence
// that decides whether ownership truncation may stand.
type Overlap struct {
	A, B           int64
	Host           string
	Site           string
	AType, BType   string
	AInCohort      bool
	BInCohort      bool
	GapMinutes     float64
	AInjectedAt    time.Time
	ARestoreDueAt  time.Time
	ARestoredAt    time.Time
	BInjectedAt    time.Time
	AExecsBeforeB  []Execution
	AExecsAfterB   []Execution
	BExecs         []Execution
	AFirstCreditAt time.Time
	BFirstCreditAt time.Time
	// HostExecsUnattributed are executions on the same guest inside B's raw window that neither fault owns (keyed
	// to an organic or unowned alert). They credit neither fault and do not decide the pair; they are disclosed
	// because they may have changed the state either fault's verification read.
	HostExecsUnattributed []Execution
	// Separated is true when every piece of the pair's evidence is told apart; Rule names how (or, when false,
	// the first piece that could not be).
	Separated    bool
	Rule         string
	ADisposition string
	BDisposition string
}

// Attribute runs the rules in the package doc over in.
func Attribute(in Input) Report {
	siteOf := in.SiteOf
	if siteOf == nil {
		siteOf = DefaultSiteOf
	}
	faults := append([]Fault(nil), in.Faults...)
	sort.Slice(faults, func(i, j int) bool {
		if faults[i].Host != faults[j].Host {
			return faults[i].Host < faults[j].Host
		}
		if !faults[i].InjectedAt.Equal(faults[j].InjectedAt) {
			return faults[i].InjectedAt.Before(faults[j].InjectedAt)
		}
		return faults[i].ID < faults[j].ID
	})
	eps := make([]*Episode, len(faults))
	for i, f := range faults {
		inWindow := !f.InjectedAt.Before(in.Since) && f.InjectedAt.Before(in.Until)
		inCampaign := in.NotePrefix == "" || strings.HasPrefix(f.Note, in.NotePrefix)
		e := &Episode{Fault: f, Site: siteOf(f.Host, f.Node), Class: FaultClass(f.FaultType), InCohort: inWindow && inCampaign}
		e.OwnUntil = ownUntil(faults, i)
		eps[i] = e
	}
	byHost := map[string][]*Episode{}
	for _, e := range eps {
		byHost[e.Host] = append(byHost[e.Host], e)
	}

	// Alerts → owner, by ref.
	ownerOf := map[string]*Episode{}
	wrongClass, unowned := 0, 0
	for _, a := range in.Alerts {
		var owner *Episode
		for _, e := range byHost[a.Host] {
			if !a.ReceivedAt.Before(e.InjectedAt) && a.ReceivedAt.Before(e.OwnUntil) {
				owner = e
				break
			}
		}
		if owner == nil {
			unowned++
			continue
		}
		if !classesMatch(owner.Class, RuleClass(a.Rule)) {
			owner.DroppedWrongClass++
			wrongClass++
			continue
		}
		att := AttributedAlert{Alert: a}
		for _, other := range byHost[a.Host] {
			if other != owner && rawCovers(other, a.ReceivedAt) {
				att.RawOverlaps++
			}
		}
		owner.Alerts = append(owner.Alerts, att)
		ownerOf[a.ExternalRef] = owner
		if ses, ok := in.Sessions[a.ExternalRef]; ok && owner.Session.ExternalRef == "" {
			owner.Session = ses
		}
	}

	// Executions and credits ride the alert's ref.
	alertByRef := map[string]Alert{}
	for _, a := range in.Alerts {
		alertByRef[a.ExternalRef] = a
	}
	// outsideReason names why an owner is not in the cohort: injected outside the window, or another campaign.
	outsideReason := func(e *Episode) string {
		if e.InjectedAt.Before(in.Since) || !e.InjectedAt.Before(in.Until) {
			return DispositionOwnerOutside
		}
		return DispositionOwnerOtherCamp
	}
	// unownedReason names why no cohort fault owns a ref: its alert was owned but of the wrong class, or nobody
	// owned it at all (organic, or no fault on that host).
	unownedReason := func(ref string) string {
		if a, seen := alertByRef[ref]; seen {
			for _, e := range byHost[a.Host] {
				if !a.ReceivedAt.Before(e.InjectedAt) && a.ReceivedAt.Before(e.OwnUntil) && !classesMatch(e.Class, RuleClass(a.Rule)) {
					return DispositionWrongClassOwner
				}
			}
		}
		return DispositionUnattributed
	}
	// Rows the loader read OUTSIDE the window (its lookback) exist to give in-window evidence its real owner; they
	// are attached to that owner but never LISTED as the window's unowned evidence — the listing answers "what
	// happened inside the window that the cohort cannot claim", and a heal from before the window is not that.
	inWindow := func(t time.Time) bool { return !t.Before(in.Since) && t.Before(in.Until) }
	var unownedExecs []UnownedExecution
	for _, x := range in.Executions {
		owner, ok := ownerOf[x.ExternalRef]
		switch {
		case ok && owner.InCohort:
			owner.Executions = append(owner.Executions, x)
		case ok:
			owner.Executions = append(owner.Executions, x)
			if inWindow(x.ExecutedAt) {
				unownedExecs = append(unownedExecs, UnownedExecution{Execution: x, Reason: outsideReason(owner), OwnerID: owner.ID})
			}
		case inWindow(x.ExecutedAt):
			unownedExecs = append(unownedExecs, UnownedExecution{Execution: x, Reason: unownedReason(x.ExternalRef)})
		}
	}
	var unownedCredits []UnownedCredit
	for _, c := range in.Credits {
		owner, ok := ownerOf[c.ExternalRef]
		switch {
		case ok && c.Outcome != CreditedOutcome:
			owner.OtherCredits = append(owner.OtherCredits, c)
		case ok && owner.InCohort:
			owner.Credits = append(owner.Credits, c)
		case ok:
			owner.Credits = append(owner.Credits, c)
			if inWindow(c.CreditedAt) {
				unownedCredits = append(unownedCredits, UnownedCredit{Credit: c, Reason: outsideReason(owner), OwnerID: owner.ID})
			}
		case inWindow(c.CreditedAt):
			unownedCredits = append(unownedCredits, UnownedCredit{Credit: c, Reason: unownedReason(c.ExternalRef)})
		}
	}

	// Overlapping same-host pairs: truncation stands only where the evidence separates the two faults (TG-679).
	var overlaps []*Overlap
	owned := map[int64]bool{}
	for _, e := range eps {
		for _, x := range e.Executions {
			owned[x.ID] = true
		}
	}
	for host, fs := range byHost {
		for i := 0; i+1 < len(fs); i++ {
			// Consecutive pairs only: with a fault between them, A's ownership already ended at the one between,
			// and C's evidence is judged against that neighbour (its own pair).
			a, b := fs[i], fs[i+1]
			if !rawCovers(a, b.InjectedAt) || (!a.InCohort && !b.InCohort) {
				continue
			}
			o := separate(a, b)
			for _, x := range in.Executions {
				if x.TargetHost == host && !owned[x.ID] && !x.ExecutedAt.Before(b.InjectedAt) && rawCovers(b, x.ExecutedAt) {
					o.HostExecsUnattributed = append(o.HostExecsUnattributed, x)
				}
			}
			overlaps = append(overlaps, o)
		}
	}

	r := Report{Since: in.Since, Until: in.Until}
	r.Summary = Summary{Since: in.Since, Until: in.Until, BySite: map[string]Counts{}, ByFaultType: map[string]Counts{},
		ByVirt: map[string]Counts{}, BySiteFaultTypeVirt: map[string]Counts{}, MaxGuestShareOfExecutions: map[string]string{},
		AlertsOwnedWrongClass: wrongClass, AlertsUnowned: unowned}
	execsByGuestSite := map[string]map[string]int{}
	for _, e := range eps {
		finish(e)
		if !e.InCohort {
			inWindow := !e.InjectedAt.Before(in.Since) && e.InjectedAt.Before(in.Until)
			if len(e.Executions) > 0 || len(e.Alerts) > 0 || inWindow {
				r.OwnersOutside = append(r.OwnersOutside, *e)
			}
			continue
		}
		r.Episodes = append(r.Episodes, *e)
		add := func(m map[string]Counts, k string) {
			c := m[k]
			tally(&c, e)
			m[k] = c
		}
		tally(&r.Summary.Total, e)
		add(r.Summary.BySite, orUnknown(e.Site))
		add(r.Summary.ByFaultType, e.FaultType)
		add(r.Summary.ByVirt, orUnknown(e.Virt))
		add(r.Summary.BySiteFaultTypeVirt, orUnknown(e.Site)+"/"+e.FaultType+"/"+orUnknown(e.Virt))
		r.Summary.ExecutionsAttributed += len(e.Executions)
		if e.Ambiguous {
			r.Summary.CreditsAmbiguous += len(e.Credits)
		} else {
			r.Summary.CreditsAttributed += len(e.Credits)
		}
		r.Summary.CreditsOtherOutcome += len(e.OtherCredits)
		if len(e.Executions) > 0 {
			if execsByGuestSite[orUnknown(e.Site)] == nil {
				execsByGuestSite[orUnknown(e.Site)] = map[string]int{}
			}
			execsByGuestSite[orUnknown(e.Site)][e.Host] += len(e.Executions)
		}
	}
	for site, guests := range execsByGuestSite {
		total, best, bestGuest := 0, 0, ""
		for g, n := range guests {
			total += n
			if n > best || (n == best && g < bestGuest) {
				best, bestGuest = n, g
			}
		}
		r.Summary.MaxGuestShareOfExecutions[site] = fmt.Sprintf("%d/%d (%s)", best, total, bestGuest)
	}
	byID := map[int64]*Episode{}
	for _, e := range eps {
		byID[e.ID] = e
	}
	sort.Slice(overlaps, func(i, j int) bool {
		if !overlaps[i].AInjectedAt.Equal(overlaps[j].AInjectedAt) {
			return overlaps[i].AInjectedAt.Before(overlaps[j].AInjectedAt)
		}
		return overlaps[i].A < overlaps[j].A
	})
	for _, o := range overlaps {
		o.ADisposition, o.BDisposition = byID[o.A].Disposition, byID[o.B].Disposition
		r.Overlaps = append(r.Overlaps, *o)
		r.Summary.OverlapPairs++
		if o.Separated {
			r.Summary.OverlapsSeparated++
		} else {
			r.Summary.OverlapsAmbiguous++
		}
	}
	sort.Slice(r.Episodes, func(i, j int) bool { return r.Episodes[i].InjectedAt.Before(r.Episodes[j].InjectedAt) })
	sort.Slice(r.OwnersOutside, func(i, j int) bool { return r.OwnersOutside[i].InjectedAt.Before(r.OwnersOutside[j].InjectedAt) })
	for _, u := range unownedExecs {
		switch u.Reason {
		case DispositionOwnerOutside, DispositionOwnerOtherCamp:
			r.Summary.ExecutionsOwnerOutside++
		case DispositionWrongClassOwner:
			r.Summary.ExecutionsOwnerWrongClass++
		default:
			r.Summary.ExecutionsUnattributed++
		}
	}
	r.Summary.CreditsUnowned = len(unownedCredits)
	r.UnownedExecutions = unownedExecs
	r.UnownedCredits = unownedCredits
	return r
}

// ownUntil is the end of faults[i]'s ownership: the next injection on the same host, or its restore deadline
// plus the grace, whichever is earlier; a fault with no deadline and no successor owns the host open-endedly.
func ownUntil(sorted []Fault, i int) time.Time {
	f := sorted[i]
	var until time.Time
	if i+1 < len(sorted) && sorted[i+1].Host == f.Host {
		until = sorted[i+1].InjectedAt
	}
	if !f.RestoreDueAt.IsZero() {
		due := f.RestoreDueAt.Add(OwnershipGrace)
		if until.IsZero() || due.Before(until) {
			until = due
		}
	}
	if until.IsZero() {
		until = openEnded
	}
	return until
}

// openEnded marks an ownership with no end: no deadline and no successor (rendered as an empty own_until).
var openEnded = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// rawCovers reports whether t lies in e's RAW window — injection → restore deadline + grace — the interval a
// host+window join would have used, before the ownership cut. A fault with NO deadline is the least understood
// fault on the host: its raw window ends at its actual restore + grace when one was recorded, and is OPEN-ENDED
// when neither is known — it fails closed, marking every later same-host evidence rather than ignoring it.
func rawCovers(e *Episode, t time.Time) bool {
	end := openEnded
	switch {
	case !e.RestoreDueAt.IsZero():
		end = e.RestoreDueAt.Add(OwnershipGrace)
	case !e.RestoredAt.IsZero():
		end = e.RestoredAt.Add(OwnershipGrace)
	}
	return !t.Before(e.InjectedAt) && t.Before(end)
}

// separate decides one overlapping pair (A injected first; B injected inside A's raw window) by the rules in the
// package doc, marking both faults ambiguous when any piece of evidence cannot be told apart.
func separate(a, b *Episode) *Overlap {
	o := &Overlap{A: a.ID, B: b.ID, Host: a.Host, Site: a.Site, AType: a.FaultType, BType: b.FaultType,
		AInCohort: a.InCohort, BInCohort: b.InCohort, GapMinutes: b.InjectedAt.Sub(a.InjectedAt).Minutes(),
		AInjectedAt: a.InjectedAt, ARestoreDueAt: a.RestoreDueAt, ARestoredAt: a.RestoredAt, BInjectedAt: b.InjectedAt,
		BExecs: append([]Execution(nil), b.Executions...)}
	for _, x := range a.Executions {
		if x.ExecutedAt.Before(b.InjectedAt) {
			o.AExecsBeforeB = append(o.AExecsBeforeB, x)
		} else {
			o.AExecsAfterB = append(o.AExecsAfterB, x)
		}
	}
	o.AFirstCreditAt, o.BFirstCreditAt = firstCredit(a.Credits), firstCredit(b.Credits)
	// A was HEALED first: an attributed execution of A before B was injected whose verified clean credit was also
	// recorded before B was injected. An execution alone is a fact, not a resolution (an execution reads `match`
	// even for a no-op heal); only the verified read says A's state was clean before B existed.
	var healedFirst *Execution
	for i, x := range o.AExecsBeforeB {
		for _, c := range a.Credits {
			if c.ExternalRef == x.ExternalRef && c.CreditedAt.Before(b.InjectedAt) {
				healedFirst = &o.AExecsBeforeB[i]
				break
			}
		}
		if healedFirst != nil {
			break
		}
	}
	var why, fail []string
	for _, x := range o.AExecsBeforeB {
		why = append(why, fmt.Sprintf("A's execution %d (%s) ran before B was injected: A's by time", x.ID, x.OpClass))
	}
	for _, x := range o.AExecsAfterB {
		if opSeparates(x.OpClass, a.FaultType, b.FaultType) {
			why = append(why, fmt.Sprintf("A's execution %d is %s, A's healing op, not B's %q", x.ID, x.OpClass, ExpectedOp(b.FaultType)))
		} else {
			fail = append(fail, fmt.Sprintf("A's execution %d (%s) ran after B was injected and its op class does not separate %s from %s", x.ID, x.OpClass, a.FaultType, b.FaultType))
		}
	}
	for _, x := range o.BExecs {
		switch {
		case opSeparates(x.OpClass, b.FaultType, a.FaultType):
			why = append(why, fmt.Sprintf("op class separates: B's execution %d is %s, B's healing op, not A's %q", x.ID, x.OpClass, ExpectedOp(a.FaultType)))
		case healedFirst != nil:
			why = append(why, fmt.Sprintf("B's execution %d (%s) follows A's execution %d (%s), verified clean before B was injected", x.ID, x.OpClass, healedFirst.ID, healedFirst.OpClass))
		default:
			fail = append(fail, fmt.Sprintf("B's execution %d (%s) is not B's healing op %q apart from A's %q, and A was not verified healed before B was injected", x.ID, x.OpClass, ExpectedOp(b.FaultType), ExpectedOp(a.FaultType)))
		}
	}
	// A credit of A recorded at or after B's injection was READ while B was live: it stands only when its op class
	// separates (A's healing op, not B's).
	for _, c := range a.Credits {
		if c.CreditedAt.Before(b.InjectedAt) {
			continue
		}
		if opSeparates(c.OpClass, a.FaultType, b.FaultType) {
			why = append(why, fmt.Sprintf("A's credit (%s) was verified after B was injected and is A's healing op, not B's", c.OpClass))
		} else {
			fail = append(fail, fmt.Sprintf("A's credit (%s) was verified at %s, after B was injected, and its op class does not separate %s from %s", c.OpClass, c.CreditedAt.UTC().Format(time.RFC3339), a.FaultType, b.FaultType))
		}
	}
	restoredFirst := !a.RestoredAt.IsZero() && !a.RestoredAt.After(b.InjectedAt)
	for _, al := range b.Alerts {
		if a.Class != ClassOther && !classesMatch(a.Class, RuleClass(al.Rule)) {
			why = append(why, fmt.Sprintf("B's alert %s (%s) is a rule class %s cannot raise", al.ExternalRef, al.Rule, a.FaultType))
			continue
		}
		switch {
		case restoredFirst && !al.ObservedAt.IsZero() && !al.ObservedAt.Before(b.InjectedAt):
			why = append(why, fmt.Sprintf("B's alert %s observed %s, after B was injected and after A was restored", al.ExternalRef, al.ObservedAt.UTC().Format(time.RFC3339)))
		case !restoredFirst:
			fail = append(fail, fmt.Sprintf("B's alert %s is of a rule class A raises and A was not restored before B was injected", al.ExternalRef))
		default:
			fail = append(fail, fmt.Sprintf("B's alert %s is of a rule class A raises and was observed before B was injected (or its observation time is unknown)", al.ExternalRef))
		}
	}
	o.Separated = len(fail) == 0
	switch {
	case !o.Separated:
		o.Rule = "AMBIGUOUS: " + strings.Join(fail, "; ")
		markAmbiguous(a, b)
	case len(why) == 0:
		o.Rule = "nothing to separate: no execution on either side after B was injected and no alert of B that A could raise"
	default:
		o.Rule = strings.Join(why, "; ")
	}
	return o
}

func firstCredit(cs []Credit) time.Time {
	var t time.Time
	for _, c := range cs {
		if t.IsZero() || c.CreditedAt.Before(t) {
			t = c.CreditedAt
		}
	}
	return t
}

// markAmbiguous records an unseparated overlap on BOTH faults (TG-679).
func markAmbiguous(a, b *Episode) {
	a.Ambiguous, b.Ambiguous = true, true
	a.AmbiguousWith = addID(a.AmbiguousWith, b.ID)
	b.AmbiguousWith = addID(b.AmbiguousWith, a.ID)
}

func addID(ids []int64, id int64) []int64 {
	for _, x := range ids {
		if x == id {
			return ids
		}
	}
	ids = append(ids, id)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// finish derives the per-fault timing facts and the disposition.
func finish(e *Episode) {
	for _, x := range e.Executions {
		if e.FirstExec.IsZero() || x.ExecutedAt.Before(e.FirstExec) {
			e.FirstExec = x.ExecutedAt
		}
	}
	if !e.FirstExec.IsZero() {
		e.ExecBeforeDue = !e.RestoreDueAt.IsZero() && e.FirstExec.Before(e.RestoreDueAt)
		e.ExecBeforeReconcile = !e.RestoredAt.IsZero() && e.FirstExec.Before(e.RestoredAt)
	}
	switch {
	case e.Ambiguous:
		e.Disposition = DispositionAmbiguous
	case len(e.Credits) > 0:
		e.Disposition = DispositionCredited
	case len(e.Executions) > 0:
		e.Disposition = DispositionExecutedNoCred
	case len(e.Alerts) > 0:
		e.Disposition = DispositionAlertedNoExec
	default:
		e.Disposition = DispositionNoAlert
	}
}

func tally(c *Counts, e *Episode) {
	c.Faults++
	if len(e.Alerts) > 0 {
		c.Alerted++
	} else {
		c.NoAlert++
	}
	if len(e.Executions) > 0 {
		c.Executed++
		c.Executions += len(e.Executions)
		if e.ExecBeforeDue {
			c.ExecBeforeDue++
		}
		if e.ExecBeforeReconcile {
			c.ExecBeforeRecon++
		}
	}
	switch {
	case e.Ambiguous:
		c.Ambiguous++
		if len(e.Credits) > 0 {
			c.AmbiguousCredited++
		}
	case len(e.Credits) > 0:
		c.Credited++
	case len(e.Executions) > 0:
		c.ExecutedNoCredit++
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// CSVHeader is the per-fault CSV's column list, in order.
var CSVHeader = []string{"fault_id", "host", "site", "node", "virt", "fault_type", "class", "in_cohort", "injected_at",
	"restore_due_at", "restored_at", "restore_state", "own_until", "matched_alerts", "alert_refs", "alert_rules",
	"ambiguous", "ambiguous_with", "max_raw_overlaps", "dropped_wrong_class", "session_outcome", "session_band", "session_synthetic",
	"n_exec", "exec_ids", "exec_op_classes", "first_exec", "exec_before_due", "exec_before_reconcile",
	"n_credit", "credit_op_classes", "credited_at", "n_credit_other_outcome", "disposition"}

// WriteCSV writes the cohort episodes (one row per fault) and then the owners outside the window, flagged.
func WriteCSV(w io.Writer, r Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	rows := append(append([]Episode(nil), r.Episodes...), r.OwnersOutside...)
	for _, e := range rows {
		if err := cw.Write(csvRow(e)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func csvRow(e Episode) []string {
	ts := func(t time.Time) string {
		if t.IsZero() || t.Equal(openEnded) {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	refs, rules, execIDs, execOps, credOps, credAts := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	maxOverlap := 0
	for _, a := range e.Alerts {
		refs = append(refs, a.ExternalRef)
		rules = append(rules, a.Rule)
		if a.RawOverlaps > maxOverlap {
			maxOverlap = a.RawOverlaps
		}
	}
	for _, x := range e.Executions {
		execIDs = append(execIDs, strconv.FormatInt(x.ID, 10))
		execOps = append(execOps, x.OpClass+"@"+ts(x.ExecutedAt)+"/"+x.Verdict)
	}
	for _, c := range e.Credits {
		credOps = append(credOps, c.OpClass)
		credAts = append(credAts, ts(c.CreditedAt))
	}
	ambWith := make([]string, 0, len(e.AmbiguousWith))
	for _, id := range e.AmbiguousWith {
		ambWith = append(ambWith, strconv.FormatInt(id, 10))
	}
	return []string{strconv.FormatInt(e.ID, 10), e.Host, e.Site, e.Node, e.Virt, e.FaultType, string(e.Class),
		strconv.FormatBool(e.InCohort), ts(e.InjectedAt), ts(e.RestoreDueAt), ts(e.RestoredAt), e.RestoreState,
		ts(e.OwnUntil), strconv.Itoa(len(e.Alerts)), strings.Join(refs, ";"), strings.Join(rules, ";"),
		strconv.FormatBool(e.Ambiguous), strings.Join(ambWith, ";"), strconv.Itoa(maxOverlap), strconv.Itoa(e.DroppedWrongClass),
		e.Session.Outcome, e.Session.Band, strconv.FormatBool(e.Session.Synthetic),
		strconv.Itoa(len(e.Executions)), strings.Join(execIDs, ";"), strings.Join(execOps, ";"), ts(e.FirstExec),
		strconv.FormatBool(e.ExecBeforeDue), strconv.FormatBool(e.ExecBeforeReconcile), strconv.Itoa(len(e.Credits)),
		strings.Join(credOps, ";"), strings.Join(credAts, ";"), strconv.Itoa(len(e.OtherCredits)), e.Disposition}
}

// Text renders the summary for a terminal.
func (r Report) Text() string {
	b := &strings.Builder{}
	s := r.Summary
	fmt.Fprintf(b, "episode disposition  window [%s, %s)  — ownership: injection → min(next injection on host, restore_due + %s); class-matched alerts; a same-guest overlap stands only where evidence separates the two faults, else both are ambiguous (in the denominator, never credited)\n",
		r.Since.UTC().Format(time.RFC3339), r.Until.UTC().Format(time.RFC3339), OwnershipGrace)
	line := func(k string, c Counts) {
		fmt.Fprintf(b, "  %-34s faults=%-3d alerted=%-3d no-alert=%-3d executed=%-3d credited=%-3d exec-no-credit=%-3d ambiguous=%-3d (of which carried a credit %d) before-due=%-3d before-reconcile=%-3d executions=%d\n",
			k, c.Faults, c.Alerted, c.NoAlert, c.Executed, c.Credited, c.ExecutedNoCredit, c.Ambiguous, c.AmbiguousCredited, c.ExecBeforeDue, c.ExecBeforeRecon, c.Executions)
	}
	line("TOTAL", s.Total)
	for _, k := range sortedKeys(s.BySite) {
		line("site "+k, s.BySite[k])
	}
	for _, k := range sortedKeys(s.ByFaultType) {
		line("class "+k, s.ByFaultType[k])
	}
	for _, k := range sortedKeys(s.ByVirt) {
		line("virt "+k, s.ByVirt[k])
	}
	for _, k := range sortedKeys(s.BySiteFaultTypeVirt) {
		line(k, s.BySiteFaultTypeVirt[k])
	}
	fmt.Fprintf(b, "  executions: attributed=%d owner-outside-window-or-campaign=%d owner-wrong-class=%d unattributed=%d\n",
		s.ExecutionsAttributed, s.ExecutionsOwnerOutside, s.ExecutionsOwnerWrongClass, s.ExecutionsUnattributed)
	fmt.Fprintf(b, "  credits: attributed(verified_clean)=%d ambiguous(disclosed, not counted)=%d other-outcome=%d unowned=%d; alerts owned but wrong class=%d, unowned=%d\n",
		s.CreditsAttributed, s.CreditsAmbiguous, s.CreditsOtherOutcome, s.CreditsUnowned, s.AlertsOwnedWrongClass, s.AlertsUnowned)
	fmt.Fprintf(b, "  same-guest raw-window overlaps: %d pair(s); separated by evidence %d; ambiguous %d\n", s.OverlapPairs, s.OverlapsSeparated, s.OverlapsAmbiguous)
	for _, o := range r.Overlaps {
		verdict := "separated"
		if !o.Separated {
			verdict = "AMBIGUOUS"
		}
		fmt.Fprintf(b, "    %d (%s) -> %d (%s) gap %.1fm: %s [%s / %s] — %s\n", o.A, o.AType, o.B, o.BType, o.GapMinutes, verdict, o.ADisposition, o.BDisposition, o.Rule)
	}
	for _, k := range sortedKeys(s.MaxGuestShareOfExecutions) {
		fmt.Fprintf(b, "  max guest share of executions %s: %s\n", k, s.MaxGuestShareOfExecutions[k])
	}
	for _, u := range r.UnownedExecutions {
		fmt.Fprintf(b, "  execution %d (%s, %s) %s", u.ID, u.OpClass, u.ExecutedAt.UTC().Format(time.RFC3339), u.Reason)
		if u.OwnerID != 0 {
			fmt.Fprintf(b, " (owner fault %d)", u.OwnerID)
		}
		fmt.Fprintf(b, "\n")
	}
	for _, u := range r.UnownedCredits {
		fmt.Fprintf(b, "  credit %s/%s (%s) %s", u.OpClass, u.ExternalRef, u.CreditedAt.UTC().Format(time.RFC3339), u.Reason)
		if u.OwnerID != 0 {
			fmt.Fprintf(b, " (owner fault %d)", u.OwnerID)
		}
		fmt.Fprintf(b, "\n")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// OverlapsHeader is the overlap table's column list (WriteOverlapsMarkdown, WriteOverlapsCSV).
var OverlapsHeader = []string{"A", "A type", "A cohort", "B", "B type", "B cohort", "site", "gap (min)", "A restored (injector)",
	"B injected", "A restored before B (s)", "A executions before B", "A executions after B", "B executions",
	"A verified clean", "B verified clean", "other executions on the guest", "separated", "A disposition", "B disposition", "rule"}

func overlapRow(o Overlap) []string {
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05")
	}
	xs := func(es []Execution) string {
		parts := make([]string, 0, len(es))
		for _, x := range es {
			parts = append(parts, fmt.Sprintf("%d %s %s", x.ID, x.OpClass, x.ExecutedAt.UTC().Format("15:04:05")))
		}
		if len(parts) == 0 {
			return "-"
		}
		return strings.Join(parts, "; ")
	}
	lead := ""
	if !o.ARestoredAt.IsZero() {
		lead = strconv.FormatFloat(o.BInjectedAt.Sub(o.ARestoredAt).Seconds(), 'f', 0, 64)
	}
	return []string{strconv.FormatInt(o.A, 10), o.AType, strconv.FormatBool(o.AInCohort), strconv.FormatInt(o.B, 10), o.BType,
		strconv.FormatBool(o.BInCohort), o.Site, strconv.FormatFloat(o.GapMinutes, 'f', 1, 64), ts(o.ARestoredAt), ts(o.BInjectedAt), lead,
		xs(o.AExecsBeforeB), xs(o.AExecsAfterB), xs(o.BExecs), ts(o.AFirstCreditAt), ts(o.BFirstCreditAt), xs(o.HostExecsUnattributed), strconv.FormatBool(o.Separated),
		o.ADisposition, o.BDisposition, o.Rule}
}

// WriteOverlapsCSV writes one row per overlapping pair.
func WriteOverlapsCSV(w io.Writer, r Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(OverlapsHeader); err != nil {
		return err
	}
	for _, o := range r.Overlaps {
		if err := cw.Write(overlapRow(o)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteOverlapsMarkdown writes the same table as Markdown (pipes in cells escaped).
func WriteOverlapsMarkdown(w io.Writer, r Report) error {
	esc := func(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
	row := func(cells []string) string {
		for i := range cells {
			cells[i] = esc(cells[i])
		}
		return "| " + strings.Join(cells, " | ") + " |\n"
	}
	sep := make([]string, len(OverlapsHeader))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, row(append([]string(nil), OverlapsHeader...))+row(sep)); err != nil {
		return err
	}
	for _, o := range r.Overlaps {
		if _, err := io.WriteString(w, row(overlapRow(o))); err != nil {
			return err
		}
	}
	return nil
}
