package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ★ EVERY INTAKE THAT RECORDS AN ACCEPTED ALERT MUST ALSO OBSERVE IT.
//
// The shadow measurement is the evidence an owner decides on when choosing whether suppression may start
// ACTING. Computing it over some intakes and not others does not make it conservative — it makes its error
// depend on which sources happen to be wired, and it moves whenever traffic shifts between them.
//
// MEASURED 2026-07-29 over seven days, before this was closed:
//
//	librenms                 1,677 alerts   single-envelope HTTP arm   OBSERVED
//	prometheus-alertmanager     27 alerts   grouped-transport arm      NOT observed
//	pve-liveness                16 alerts   worker poller              NOT observed
//
// So the number covered 97.5% of volume — materially better than the standing "measured over a fraction"
// claim, and still wrong in the direction that matters: the two unobserved intakes are the ones that GROW.
// pve-liveness is TG's own fastest detector (~85s vs ~610s mean detection), and the grouped arm carries
// CrowdSec, a security source.
//
// THE ORACLE IS STRUCTURAL, AND DELIBERATELY SO. A behaviour test can only exercise the call sites someone
// remembered to write; the defect here IS the site nobody wrote. So this pairs the two facts that must move
// together — a place that appends to the alert log is a place that accepted an alert, and therefore a place
// that must observe it.
func TestEveryAcceptedAlertIsAlsoObserved(t *testing.T) {
	appendRE := regexp.MustCompile(`Alerts\.Append\(|AlertLogStore\([^)]*\)\.Append\(`)
	// ★ THE RECEIVER DOT IS LOAD-BEARING. The first version matched a bare `ObserveAccepted(`, which also
	// matches the SuppressionObserver INTERFACE DECLARATION in this file. That inflated the count by one, so
	// a file missing exactly ONE call site still balanced — and exactly one missing call site is the defect.
	// Mutation BM (delete the grouped-transport arm's observation, the real live gap) was GREEN against it.
	// A completeness oracle that counts a type declaration as a call site cannot count.
	// `\s*` because a call may be wrapped across lines — the receiver dot ends one line and the method
	// name begins the next. Requiring them adjacent made this oracle miss a real call site, which is the
	// same class of error as counting a declaration: a completeness check that miscounts is not a check.
	observeRE := regexp.MustCompile(`\.\s*ObserveAccepted\(`)

	for _, f := range []string{"ingest.go", "../../cmd/worker/main.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		appends := len(appendRE.FindAllString(src, -1))
		observes := len(observeRE.FindAllString(src, -1))
		if appends == 0 {
			t.Errorf("%s: found ZERO alert-log appends — this sweep would pass vacuously, which is how the "+
				"missing observation survived in the first place", f)
			continue
		}
		if observes < appends {
			t.Errorf("%s records %d accepted alert(s) into the alert log but observes only %d for the "+
				"suppression shadow. An intake that accepts an alert without observing it is silently excluded "+
				"from the percentage the enable decision rests on.", f, appends, observes)
		}
	}
}

// The interface must stay SHADOW-shaped: no context to cancel it, no error to fail the ingest path on. If
// ObserveAccepted ever gained either, an observation could delay or reject a real alert — the front door
// would start depending on a measurement.
func TestTheObserverCannotDelayOrFailIngest(t *testing.T) {
	b, err := os.ReadFile("ingest.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "ObserveAccepted(host, alertRule string, at time.Time)")
	if i < 0 {
		t.Fatal("the SuppressionObserver method signature changed shape — re-read why it takes no context " +
			"and returns no error before adjusting this test")
	}
	// Nothing on the ingest path may branch on its result.
	for _, bad := range []string{"if err := d.Suppression.ObserveAccepted", "= d.Suppression.ObserveAccepted"} {
		if strings.Contains(src, bad) {
			t.Errorf("the ingest path consumes the observer's result (%q) — a shadow measurement must not be "+
				"able to change what the front door does with a real alert", bad)
		}
	}
}
