package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// A PAGE SIZE IS NOT A COUNT.
//
// The console's alerts badge read "50" for every estate volume, because the only number the API gave it was
// len(page) and the page is fetched with limit=50. Measured live: the store held 1,553 accepted alerts, 549
// of them in the last 24h, and the badge said 50 — as it would have said 50 for 50 alerts or 50,000. The
// "Logs · Evidence" badge was the same defect compounded: ledger page (40) + alerts page (50) = exactly 90,
// a constant dressed as telemetry.
//
// The fix is a Counts field carrying the POPULATION. These oracles pin the property that distinguishes it
// from what it replaced: the reported total must MOVE when the population moves while the page size does
// not. A test that seeds fewer alerts than the limit cannot tell a count from a page length — so every
// fixture here deliberately seeds MORE rows than it asks for.

// bigLog seeds n alerts, the newest `recent` of them inside the 24h window.
func bigLog(n, recent int) *MemAlertLog {
	l := NewMemAlertLog(n + 10)
	now := time.Now()
	for i := 0; i < n; i++ {
		at := now.Add(-48 * time.Hour) // outside the window by default
		if i >= n-recent {
			at = now.Add(-time.Duration(i%20) * time.Minute)
		}
		l.Append(context.Background(), AlertRecord{
			ExternalRef: fmt.Sprintf("ref-%d", i), SourceType: "librenms",
			AlertRule: "Device-Down", Severity: "critical", ReceivedAt: at,
		})
	}
	return l
}

func getAlertsPage(t *testing.T, log AlertLog, query string) AlertsPage {
	t.Helper()
	d := Deps{Alerts: log}
	rec := httptest.NewRecorder()
	d.alertsHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/alerts"+query, nil), auth.Principal{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var page AlertsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return page
}

// TestCountsReportThePopulationNotThePageSize is the defect as an oracle.
func TestCountsReportThePopulationNotThePageSize(t *testing.T) {
	page := getAlertsPage(t, bigLog(300, 120), "?limit=50")

	if got := len(page.Alerts); got != 50 {
		t.Fatalf("page carried %d rows, want the requested 50 — fixture does not exercise the defect", got)
	}
	if page.Counts.Total == len(page.Alerts) {
		t.Errorf("counts.total = %d, which is exactly the page size — this is the defect the field exists "+
			"to remove: a badge fed from it reports the fetch limit, not how many alerts there are",
			page.Counts.Total)
	}
	if page.Counts.Total != 300 {
		t.Errorf("counts.total = %d, want 300 (the seeded population)", page.Counts.Total)
	}
	if page.Counts.Last24h != 120 {
		t.Errorf("counts.last_24h = %d, want 120 — the window count must be measured, not derived from "+
			"the page", page.Counts.Last24h)
	}
}

// TestTheTotalMovesWithThePopulationAndNotWithTheLimit — the discriminating property. Change the limit and
// the total must not budge; change the population and it must.
func TestTheTotalMovesWithThePopulationAndNotWithTheLimit(t *testing.T) {
	log := bigLog(300, 10)

	a := getAlertsPage(t, log, "?limit=10")
	b := getAlertsPage(t, log, "?limit=100")
	if len(a.Alerts) == len(b.Alerts) {
		t.Fatalf("both pages returned %d rows — the limit is not being honoured, so this test cannot "+
			"distinguish a count from a page length", len(a.Alerts))
	}
	if a.Counts.Total != b.Counts.Total {
		t.Errorf("counts.total changed with the fetch limit (%d vs %d) — it is tracking the page, not the "+
			"population", a.Counts.Total, b.Counts.Total)
	}

	bigger := getAlertsPage(t, bigLog(301, 10), "?limit=10")
	if bigger.Counts.Total != a.Counts.Total+1 {
		t.Errorf("counts.total = %d for a population of 301, want %d — the count does not follow the "+
			"population it claims to measure", bigger.Counts.Total, a.Counts.Total+1)
	}
}

// errCounts serves a page fine but cannot count — the partial-failure case.
type errCounts struct{ *MemAlertLog }

func (errCounts) Counts(context.Context, auth.Principal) (AlertCounts, error) {
	return AlertCounts{}, fmt.Errorf("counts unavailable")
}

// TestAnUncountableStoreFailsClosedRatherThanReportingZero — a zeroed count is a CONFIDENT LIE: the badge
// would read "0 alerts" for a store that merely could not be counted, which on an alerting surface is the
// most dangerous possible wrong answer.
func TestAnUncountableStoreFailsClosedRatherThanReportingZero(t *testing.T) {
	d := Deps{Alerts: errCounts{bigLog(300, 10)}}
	rec := httptest.NewRecorder()
	d.alertsHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/alerts?limit=10", nil), auth.Principal{})

	if rec.Code == http.StatusOK {
		var page AlertsPage
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		t.Errorf("a store whose Counts failed returned 200 with counts.total=%d — serving a zero here "+
			"tells the operator there are no alerts when the truth is that nobody could count them; "+
			"fail closed instead", page.Counts.Total)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
