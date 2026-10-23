package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ---- fake YouTrack (helpers named tgledger* — helper-name collisions have reddened main before) ----

type tgledgerFakeYT struct {
	counts       map[string]int      // count-endpoint query -> count
	stuckCount   bool                // count endpoint answers -1 forever
	censusIDs    []string            // idReadable roster for the resolved-since census
	commentsByID map[string][]string // idReadable -> comment texts (unknown id -> no comments)
}

func tgledgerFakeServer(t *testing.T, f tgledgerFakeYT) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/issuesGetter/count", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("count endpoint hit with %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("count endpoint Authorization = %q", got)
		}
		if f.stuckCount {
			fmt.Fprint(w, `{"count":-1}`)
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("count endpoint: unparseable body: %v", err)
		}
		n, ok := f.counts[body.Query]
		if !ok {
			t.Errorf("count endpoint: unexpected query %q", body.Query)
		}
		fmt.Fprintf(w, `{"count":%d}`, n)
	})
	mux.HandleFunc("/api/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, "#Resolved") || !strings.Contains(q, "updated: "+censusSince+" .. *") {
			t.Errorf("census hit with unexpected query %q", q)
		}
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		if skip > len(f.censusIDs) {
			skip = len(f.censusIDs)
		}
		end := skip + top
		if end > len(f.censusIDs) {
			end = len(f.censusIDs)
		}
		page := make([]issueRef, 0, end-skip)
		for _, id := range f.censusIDs[skip:end] {
			page = append(page, issueRef{IDReadable: id})
		}
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Errorf("census page encode: %v", err)
		}
	})
	mux.HandleFunc("/api/issues/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/issues/"), "/comments")
		out := make([]issueComment, 0, len(f.commentsByID[id]))
		for _, text := range f.commentsByID[id] {
			out = append(out, issueComment{Text: text})
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Errorf("comments encode for %s: %v", id, err)
		}
	})
	return httptest.NewServer(mux)
}

func tgledgerEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func tgledgerRunAgainst(t *testing.T, f tgledgerFakeYT, extraEnv map[string]string) (string, int) {
	t.Helper()
	srv := tgledgerFakeServer(t, f)
	defer srv.Close()
	env := map[string]string{"YOUTRACK_URL": srv.URL, "YOUTRACK_TOKEN": "test-token"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return run(tgledgerEnv(env), srv.Client(), 0)
}

// ---- arm (a): happy path ----

func TestLedgerHappyPathFinalLine(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts:    map[string]int{"project: TG": 10, "project: TG #Unresolved": 4},
		censusIDs: []string{"TG-1", "TG-2"},
		commentsByID: map[string][]string{
			"TG-1": {"ordinary chatter", "## delivery-bar\n- delivered: MR !9 merged\n- QA: 0.93"},
			"TG-2": {"closed, looks fine to me"},
		},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
	want := "tgledger: 10 total · 4 unresolved · 6 resolved · 1 of 2 evidence-bearing closes since 2026-08-10 · 1 bare"
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if got := lines[len(lines)-1]; got != want {
		t.Fatalf("final line = %q, want %q", got, want)
	}
	if !strings.Contains(out, "evidence-bearing closes since 2026-08-10: 1 of 2") {
		t.Fatalf("census line missing; output:\n%s", out)
	}
	// TG-484: the bare close is NAMED, not just counted — and the line says what to do about it.
	if !strings.Contains(out, "BARE closes") || !strings.Contains(out, "TG-2") {
		t.Fatalf("bare close not named; output:\n%s", out)
	}
}

// TG-484: zero bare closes prints NO bare line (a clean census is not nagged) while the final line
// still carries the honest zero — absence of the warning is earned, not silent.
func TestLedgerZeroBareIsCleanButCounted(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts:    map[string]int{"project: TG": 10, "project: TG #Unresolved": 4},
		censusIDs: []string{"TG-1"},
		commentsByID: map[string][]string{
			"TG-1": {"## delivery-bar\n- delivered: MR !9 merged\n- QA: 0.93"},
		},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d; output:\n%s", code, out)
	}
	if strings.Contains(out, "BARE closes") {
		t.Fatalf("clean census must not print the bare warning; output:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "· 0 bare") {
		t.Fatalf("final line must carry the honest zero; output:\n%s", out)
	}
}

// ---- arm (b): no token is BLIND with exit 3, never a fail-safe 0 ----

func TestLedgerNoTokenIsBlindExit3(t *testing.T) {
	out, code := run(tgledgerEnv(map[string]string{"YOUTRACK_URL": "http://example.invalid"}), http.DefaultClient, 0)
	if code != exitBlind {
		t.Fatalf("exit = %d, want %d (a measurement must not fail-safe to 0)", code, exitBlind)
	}
	want := "LEDGER BLIND: no YouTrack token (YOUTRACK_TOKEN / YT_TOKEN) — refusing to report\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestLedgerYTTokenFallbackAccepted(t *testing.T) {
	// The eval/ci scripts export YT_URL/YT_TOKEN; the ledger must accept those names too.
	srv := tgledgerFakeServer(t, tgledgerFakeYT{
		counts: map[string]int{"project: TG": 3, "project: TG #Unresolved": 1},
	})
	defer srv.Close()
	out, code := run(tgledgerEnv(map[string]string{"YT_URL": srv.URL, "YT_TOKEN": "test-token"}), srv.Client(), 0)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
}

// ---- arm (c): count endpoint stuck at -1 is BLIND, not a guessed number ----

func TestLedgerCountStuckAtMinusOneIsBlind(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{stuckCount: true}, nil)
	if code != exitBlind {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitBlind, out)
	}
	if !strings.Contains(out, "LEDGER BLIND") || !strings.Contains(out, "stuck at -1") {
		t.Fatalf("want a BLIND stuck-at--1 report, got:\n%s", out)
	}
}

// ---- arm (d): an empty project is BLIND — a broken query and an empty tracker look the same ----

func TestLedgerEmptyProjectIsBlind(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts: map[string]int{"project: TG": 0, "project: TG #Unresolved": 0},
	}, nil)
	if code != exitBlind {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitBlind, out)
	}
	want := "LEDGER BLIND: tracker returned an empty project — a broken query and an empty tracker are indistinguishable, refusing\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

// ---- arm (e): the census counts ONLY marker-bearing issues, case-insensitively ----

func TestLedgerCensusCountsOnlyMarkerBearing(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts:    map[string]int{"project: TG": 5, "project: TG #Unresolved": 2},
		censusIDs: []string{"TG-10", "TG-11"},
		commentsByID: map[string][]string{
			"TG-10": {"## DELIVERY-BAR\n- e2e: oracle red->green"}, // case-insensitive match
			"TG-11": {"resolved without evidence"},
		},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "evidence-bearing closes since 2026-08-10: 1 of 2") {
		t.Fatalf("want 1 of 2 (only the marker-bearing close counts), got:\n%s", out)
	}
	if strings.Contains(out, "2 of 2") {
		t.Fatalf("census counted a marker-less close as evidence-bearing:\n%s", out)
	}
}

// ---- arm (f): 0 of 0 is a VALID early state, worded distinctly from BLIND ----

func TestLedgerZeroOfZeroIsValidNotBlind(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts: map[string]int{"project: TG": 10, "project: TG #Unresolved": 9},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "0 of 0 (convention adopted 2026-08-10; nothing closed since)") {
		t.Fatalf("want the explicit 0-of-0 wording, got:\n%s", out)
	}
	if strings.Contains(out, "LEDGER BLIND") {
		t.Fatalf("0 of 0 must not read as BLIND:\n%s", out)
	}
}

// ---- census pagination follows $skip until a short page ----

func TestLedgerCensusFollowsPagination(t *testing.T) {
	ids := make([]string, 0, pageSize+1)
	for i := 0; i < pageSize+1; i++ {
		ids = append(ids, fmt.Sprintf("TG-%d", i+1))
	}
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts:       map[string]int{"project: TG": 300, "project: TG #Unresolved": 50},
		censusIDs:    ids,
		commentsByID: map[string][]string{"TG-201": {"## delivery-bar\n- QA: 0.91"}},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
	want := fmt.Sprintf("evidence-bearing closes since 2026-08-10: 1 of %d", pageSize+1)
	if !strings.Contains(out, want) {
		t.Fatalf("want %q (second page reached), got:\n%s", want, out)
	}
}

// ---- deployed-sync line: all three states, never omitted ----

func TestLedgerDeployedSyncLineStates(t *testing.T) {
	cases := []struct {
		name, deployed, main, want string
	}{
		{"absent", "", "", "deployed-sync: not measured this run (see the delivery-witnesses scheduled job)\n"},
		{"prefix-sync", "63e2ba60", "63e2ba60deadbeef", "deployed-sync: in sync (deployed 63e2ba60, main 63e2ba60deadbeef)\n"},
		{"drift", "63e2ba60", "18fccca1", "deployed-sync: DRIFT (deployed 63e2ba60, main 18fccca1)\n"},
	}
	for _, c := range cases {
		got := deployedSyncLine(tgledgerEnv(map[string]string{
			"LEDGER_DEPLOYED_SHA": c.deployed, "LEDGER_MAIN_SHA": c.main,
		}))
		if got != c.want {
			t.Errorf("%s: deployedSyncLine = %q, want %q", c.name, got, c.want)
		}
	}
}

// ---- the honesty section is printed on every successful run ----

func TestLedgerAlwaysPrintsNotYetMeasurableSection(t *testing.T) {
	out, code := tgledgerRunAgainst(t, tgledgerFakeYT{
		counts: map[string]int{"project: TG": 2, "project: TG #Unresolved": 1},
	}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; output:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "e2e/evaluated/QA stages: measured only via the delivery-bar comment convention (adopted 2026-08-10) — pre-existing resolved issues are grandfathered until the resolved-issue verification sweep re-verifies them (TG-339 precedent)") {
		t.Fatalf("not-yet-measurable section missing:\n%s", out)
	}
	if !strings.Contains(out, "deployed-sync: not measured this run") {
		t.Fatalf("deployed-sync state omitted:\n%s", out)
	}
}
