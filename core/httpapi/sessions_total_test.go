package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

// A PAGE SIZE IS NOT A POPULATION — the third surface to carry this defect.
//
// The alerts badge reported a constant 50 because the only number it had was len(page) from a limit=50
// fetch (!665). The same shape reaches the Knowledge model, which composes its pages from
// GET /v1/sessions?limit=50 while the spine holds 1,306 sessions: any count derived from that read reports
// the fetch limit, not the estate.
//
// The discriminating fixture rule, learned the hard way: seed MORE rows than the page requests. A fixture
// with fewer rows than the limit cannot tell a count from a page length, and the control passes for free.

type spineFake struct {
	rows    []SessionSummary
	countFn func() (int, error)
}

func (f spineFake) RecentSessions(_ context.Context, _ auth.Principal, limit int) ([]SessionSummary, error) {
	if limit > 0 && limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}

func (f spineFake) SessionCount(context.Context, auth.Principal) (int, error) {
	if f.countFn != nil {
		return f.countFn()
	}
	return len(f.rows), nil
}

func spineOf(n int) spineFake {
	rows := make([]SessionSummary, n)
	for i := range rows {
		rows[i] = SessionSummary{ExternalRef: "ref", Band: "AUTO"}
	}
	return spineFake{rows: rows}
}

func getSessions(t *testing.T, r SessionsReader, query string) (SessionsPage, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	Deps{SessionsRead: r}.sessionsHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions"+query, nil), auth.Principal{})
	var page SessionsPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return page, rec.Code
}

// TestTotalIsThePopulationNotThePage — 300 sessions, a page of 50.
func TestTotalIsThePopulationNotThePage(t *testing.T) {
	page, code := getSessions(t, spineOf(300), "?limit=50")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(page.Sessions) != 50 {
		t.Fatalf("page returned %d rows, want the requested 50 — the fixture does not exercise the defect",
			len(page.Sessions))
	}
	if page.Total == len(page.Sessions) {
		t.Errorf("total = %d, exactly the page size — this is the defect: any surface deriving a count from "+
			"this read reports the fetch limit rather than the spine", page.Total)
	}
	if page.Total != 300 {
		t.Errorf("total = %d, want the seeded population 300", page.Total)
	}
}

// TestTotalMovesWithThePopulationNotTheLimit — the discriminating property.
func TestTotalMovesWithThePopulationNotTheLimit(t *testing.T) {
	spine := spineOf(300)
	small, _ := getSessions(t, spine, "?limit=10")
	big, _ := getSessions(t, spine, "?limit=100")

	if len(small.Sessions) == len(big.Sessions) {
		t.Fatalf("both pages returned %d rows — the limit is not honoured, so this cannot distinguish a "+
			"count from a page length", len(small.Sessions))
	}
	if small.Total != big.Total {
		t.Errorf("total changed with the fetch limit (%d vs %d) — it tracks the page, not the population",
			small.Total, big.Total)
	}
	bigger, _ := getSessions(t, spineOf(301), "?limit=10")
	if bigger.Total != small.Total+1 {
		t.Errorf("total = %d for a population of 301, want %d — it does not follow the population it claims "+
			"to measure", bigger.Total, small.Total+1)
	}
}

// TestAnUncountableSpineFailsClosedRatherThanReportingZero — a zeroed total tells the Knowledge model the
// estate has no incidents, which is a confident lie rather than an absence.
func TestAnUncountableSpineFailsClosedRatherThanReportingZero(t *testing.T) {
	broken := spineFake{rows: spineOf(300).rows, countFn: func() (int, error) { return 0, errors.New("count failed") }}
	page, code := getSessions(t, broken, "?limit=10")
	if code == http.StatusOK {
		t.Errorf("a spine whose count failed returned 200 with total=%d — serving a zero says the estate has "+
			"no sessions when the truth is that nobody could count them; fail closed instead", page.Total)
	}
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
}
