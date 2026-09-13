// Command tgledger is Territory Grounder's generated progress meter for Definition of done v1.1
// (docs/BOARD.md, owner-ruled 2026-08-10, TG-428).
//
// It reports how much of `project: TG` is done under the v1.1 bar — every issue delivered → deployed →
// e2e-tested → evaluated → QA ≥0.90, with evidence on the ticket — and it is deliberately HONEST about
// what it can and cannot measure. v1 measures three things: the tracker totals (total/unresolved/resolved),
// the delivery-bar comment census over issues resolved since the convention was adopted (2026-08-10), and
// the deployed-sync state when the caller supplies both shas. The e2e/evaluated/QA stages are visible only
// THROUGH the delivery-bar convention; pre-existing resolved issues are grandfathered until the
// resolved-issue verification sweep re-verifies them (TG-339 precedent), and the report says so every run.
//
// When the instrument is disconnected — no token, an empty-project reading, a count endpoint stuck at -1 —
// it prints LEDGER BLIND and exits 3. NEVER a fail-safe 0: eval/ci/open-regression-issue.sh exits 0 without
// a tracker by design because it merely records, but this tool MEASURES, and a measurement that fakes a
// reading is worse than none. It is pure-stdlib Go so it runs in the same golang CI image as the build,
// adding no runtime dependency.
//
// Usage:
//
//	go run ./tools/tgledger    # needs YOUTRACK_URL + YOUTRACK_TOKEN (or YT_URL/YT_TOKEN); read-only
//
// Optional env: LEDGER_DEPLOYED_SHA + LEDGER_MAIN_SHA enable the deployed-sync line.
// Exit codes: 0 = report produced; 3 = LEDGER BLIND (could not measure; the report says why).
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	report, code := run(os.Getenv, &http.Client{Timeout: 60 * time.Second}, time.Second)
	fmt.Print(report)
	os.Exit(code)
}
