package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---- the convention this tool anchors ----

// censusSince is the adoption date of the delivery-bar comment convention: an issue resolved on or after
// this date counts as evidence-bearing iff a comment on it carries deliveryBarMarker (the evidence block:
// delivered / deployed / e2e-tested / evaluated / QA verdict). Issues resolved before this date are
// grandfathered until the resolved-issue verification sweep re-verifies them (TG-339 precedent).
const censusSince = "2026-08-10"

// deliveryBarMarker is matched case-insensitively inside comment text. Keep it lowercase here: the
// census lowercases the comment before the contains check.
const deliveryBarMarker = "## delivery-bar"

const (
	exitOK    = 0
	exitBlind = 3
)

// pageSize is the census page size; pagination follows $skip until a short page arrives.
const pageSize = 200

// countAttempts bounds the retry loop on YouTrack's documented count endpoint, which returns -1 while it
// is still computing. A -1 that survives every attempt is BLIND — a count still computing is not a count.
const countAttempts = 5

// ---- wire shapes (YouTrack REST) ----

type issueRef struct {
	IDReadable string `json:"idReadable"`
}

type issueComment struct {
	Text string `json:"text"`
}

// ---- the measurement ----

// run performs the whole measurement and returns the report text plus the process exit code. It never
// calls os.Exit, so every BLIND path is unit-testable; main is the only caller that terminates the
// process. A failure to measure is LEDGER BLIND with exit 3 — NEVER a fail-safe 0 (contrast
// eval/ci/open-regression-issue.sh, which exits 0 without a tracker BY DESIGN: it records, we measure).
func run(getenv func(string) string, client *http.Client, retrySleep time.Duration) (string, int) {
	token := firstNonEmpty(getenv("YOUTRACK_TOKEN"), getenv("YT_TOKEN"))
	if token == "" {
		return "LEDGER BLIND: no YouTrack token (YOUTRACK_TOKEN / YT_TOKEN) — refusing to report\n", exitBlind
	}
	baseURL := firstNonEmpty(getenv("YOUTRACK_URL"), getenv("YT_URL"))
	if baseURL == "" {
		return "LEDGER BLIND: no YouTrack URL (YOUTRACK_URL / YT_URL) — refusing to report\n", exitBlind
	}

	yt := &ytClient{base: strings.TrimRight(baseURL, "/"), token: token, client: client, retrySleep: retrySleep}

	total, err := yt.count("project: TG")
	if err != nil {
		return blind(err), exitBlind
	}
	unresolved, err := yt.count("project: TG #Unresolved")
	if err != nil {
		return blind(err), exitBlind
	}
	// Sanity: TG has issues, and always will. A zero total (or a negative unresolved reading) is not a
	// finished project — it is a query the tracker did not understand, an auth scope that hides the
	// project, or an empty instance; from here those are indistinguishable, so we refuse to pick one.
	if total == 0 || unresolved < 0 {
		return "LEDGER BLIND: tracker returned an empty project — a broken query and an empty tracker are indistinguishable, refusing\n", exitBlind
	}
	resolved := total - unresolved
	if resolved < 0 {
		return fmt.Sprintf("LEDGER BLIND: tracker counts are inconsistent (%d unresolved > %d total) — refusing to report\n", unresolved, total), exitBlind
	}

	// Delivery-bar census: every issue resolved AND updated since the convention date, checked for the
	// marker comment. This is the going-forward evidence channel for delivered/deployed/e2e/evaluated/QA.
	refs, err := yt.resolvedUpdatedSince(censusSince)
	if err != nil {
		return blind(err), exitBlind
	}
	bearing := 0
	var bare []string
	for _, ref := range refs {
		comments, err := yt.comments(ref.IDReadable)
		if err != nil {
			return blind(err), exitBlind
		}
		if carriesMarker(comments) {
			bearing++
		} else {
			bare = append(bare, ref.IDReadable)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "tgledger — the generated meter for Definition of done v1.1 (TG-428)\n\n")
	fmt.Fprintf(&b, "project: TG — %d total · %d unresolved · %d resolved\n", total, unresolved, resolved)
	if len(refs) == 0 {
		// A valid state, distinct from BLIND: the instrument works, and the convention is simply young.
		fmt.Fprintf(&b, "evidence-bearing closes since %s: 0 of 0 (convention adopted 2026-08-10; nothing closed since)\n", censusSince)
	} else {
		fmt.Fprintf(&b, "evidence-bearing closes since %s: %d of %d\n", censusSince, bearing, len(refs))
		// TG-484: a bare close is NAMED, never just counted — an unnamed denominator gap is how 85 of
		// them accumulated unnoticed. Bounded print; the count carries the truth either way.
		if len(bare) > 0 {
			shown := bare
			more := ""
			if len(shown) > 10 {
				more = fmt.Sprintf(" (+%d more)", len(shown)-10)
				shown = shown[:10]
			}
			fmt.Fprintf(&b, "BARE closes (no delivery-bar marker — must reach 0; sweep them or reopen): %s%s\n",
				strings.Join(shown, " "), more)
		}
	}
	b.WriteString(deployedSyncLine(getenv))
	b.WriteString("e2e/evaluated/QA stages: measured only via the delivery-bar comment convention (adopted 2026-08-10) — pre-existing resolved issues are grandfathered until the resolved-issue verification sweep re-verifies them (TG-339 precedent)\n")
	fmt.Fprintf(&b, "tgledger: %d total · %d unresolved · %d resolved · %d of %d evidence-bearing closes since %s · %d bare\n",
		total, unresolved, resolved, bearing, len(refs), censusSince, len(bare))
	return b.String(), exitOK
}

// deployedSyncLine reports the deployed-vs-main sha comparison when the caller supplies both hooks, and
// says OUT LOUD that it was not measured when they are absent — the state is never omitted, because a
// missing line reads as "fine" (merged+green is not deployed; the estate sat on a stale sha for days
// while CD silently skipped, TG-417).
func deployedSyncLine(getenv func(string) string) string {
	deployed, mainSHA := getenv("LEDGER_DEPLOYED_SHA"), getenv("LEDGER_MAIN_SHA")
	if deployed == "" || mainSHA == "" {
		return "deployed-sync: not measured this run (see the delivery-witnesses scheduled job)\n"
	}
	// Prefix match either direction: the two witnesses may report different sha lengths.
	if strings.HasPrefix(deployed, mainSHA) || strings.HasPrefix(mainSHA, deployed) {
		return fmt.Sprintf("deployed-sync: in sync (deployed %s, main %s)\n", deployed, mainSHA)
	}
	return fmt.Sprintf("deployed-sync: DRIFT (deployed %s, main %s)\n", deployed, mainSHA)
}

// carriesMarker reports whether any comment carries the delivery-bar marker, case-insensitively.
func carriesMarker(comments []issueComment) bool {
	for _, c := range comments {
		if strings.Contains(strings.ToLower(c.Text), deliveryBarMarker) {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func blind(err error) string {
	return fmt.Sprintf("LEDGER BLIND: %v — refusing to report\n", err)
}

// ---- YouTrack REST client (read-only: two GETs and the documented count POST) ----

type ytClient struct {
	base       string
	token      string
	client     *http.Client
	retrySleep time.Duration
}

func (y *ytClient) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+y.token)
	req.Header.Set("Accept", "application/json")
	resp, err := y.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading body: %v", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

// count asks the documented count endpoint (POST /api/issuesGetter/count) for the issue count of a
// query. YouTrack returns {"count": -1} while still computing; retry up to countAttempts with the
// configured sleep, and report a stuck -1 as an error (=> BLIND) rather than guessing.
func (y *ytClient) count(query string) (int, error) {
	payload, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return 0, err
	}
	for attempt := 1; attempt <= countAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(y.retrySleep)
		}
		req, err := http.NewRequest(http.MethodPost, y.base+"/api/issuesGetter/count?fields=count", bytes.NewReader(payload))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		body, err := y.do(req)
		if err != nil {
			return 0, err
		}
		var out struct {
			Count *int `json:"count"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return 0, fmt.Errorf("count endpoint for %q: unparseable response: %v", query, err)
		}
		if out.Count == nil {
			return 0, fmt.Errorf("count endpoint for %q: response carried no count field", query)
		}
		if *out.Count >= 0 {
			return *out.Count, nil
		}
	}
	return 0, fmt.Errorf("count endpoint stuck at -1 after %d attempts for %q — a count still computing is not a count", countAttempts, query)
}

// resolvedUpdatedSince lists idReadable for every TG issue that is resolved and was updated on or after
// the given date, following $skip pagination until a short page.
func (y *ytClient) resolvedUpdatedSince(since string) ([]issueRef, error) {
	query := fmt.Sprintf("project: TG #Resolved updated: %s .. *", since)
	var all []issueRef
	for skip := 0; ; skip += pageSize {
		u := fmt.Sprintf("%s/api/issues?query=%s&fields=idReadable&$top=%d&$skip=%d",
			y.base, url.QueryEscape(query), pageSize, skip)
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		body, err := y.do(req)
		if err != nil {
			return nil, err
		}
		var page []issueRef
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("census page ($skip=%d): unparseable response: %v", skip, err)
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

// comments fetches the comment texts of one issue.
func (y *ytClient) comments(id string) ([]issueComment, error) {
	u := fmt.Sprintf("%s/api/issues/%s/comments?fields=text&$top=500", y.base, url.PathEscape(id))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	body, err := y.do(req)
	if err != nil {
		return nil, err
	}
	var out []issueComment
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("comments of %s: unparseable response: %v", id, err)
	}
	return out, nil
}
