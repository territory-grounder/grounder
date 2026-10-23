package deploy

// THE CONSOLE'S nginx IS TG'S ENTIRE EXTERNAL API SURFACE, AND NOTHING GUARDED IT (TG-372).
//
// The grounder publishes on 127.0.0.1:8081 — loopback only. The one TG listener exposed off-host is the
// console container, and deploy/console/nginx.conf decides what that listener does with a request:
//
//	location /     { try_files $uri $uri/ /index.html; }      <- SPA fallback: 200 HTML for GET, 405 for POST
//	location /api/ { proxy_pass http://grounder:8080/; }      <- the ONLY route to the control plane
//
// So `/api/` is the path every push source uses — LibreNMS transports, the estate Alertmanager, anything
// holding an ingest_token_ref. Verified live: POST /api/v1/ingest/librenms -> 401 (reaches the grounder and
// is refused for auth), while POST /v1/ingest/librenms -> 405 (nginx refusing to POST at a static file).
//
// Delete or rename that one location block and EVERY push source starts getting 405 from a static file
// server. TG would show nothing: there is no ingest-rejection counter (TG-371), and the arrival gauges
// simply go quiet, which is indistinguishable from a calm estate. The whole external surface rested on four
// lines that no test read.
//
// It also cost an investigation. On 2026-08-06 I probed `/v1/ingest/...` repeatedly, got 405, checked DNS,
// host listeners, cron, npm and open connections, and filed TG-372 concluding no reachable path existed —
// having never read this file, which is in this repository and answers the question in one line. Probing is
// not a substitute for reading the routing definition.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func consoleNginxConf(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("console/nginx.conf")
	if err != nil {
		t.Fatalf("read console/nginx.conf: %v", err)
	}
	var kept []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// KILLING MUTATIONS: delete the `location /api/` block; point proxy_pass somewhere else; rename the prefix.
// All RED — each one silently severs every push source from TG.
func TestTheConsoleProxiesTheAPIPrefixToTheGrounder(t *testing.T) {
	conf := consoleNginxConf(t)

	// VACUITY FLOOR: a comment-stripping bug or a moved file must fail loudly, not pass over an empty string.
	if len(strings.TrimSpace(conf)) < 200 {
		t.Fatalf("console/nginx.conf read back as %d bytes of non-comment content — this guard is examining "+
			"nothing and would pass on any configuration", len(strings.TrimSpace(conf)))
	}
	if !strings.Contains(conf, "listen 8080") {
		t.Fatal("the console no longer listens on 8080 — the port compose publishes. Either this file stopped " +
			"describing the deployment, or the external surface moved and nothing here knows it")
	}

	loc := regexp.MustCompile(`location\s+/api/\s*\{[^}]*\}`).FindString(conf)
	if loc == "" {
		t.Fatal("there is no `location /api/` block. The grounder is published on 127.0.0.1 only, so this " +
			"proxy is the ONLY route from the estate to the control plane: every push source (LibreNMS " +
			"transports, the estate Alertmanager) posts through it. Without it they receive 405 from a " +
			"static file server, and TG records nothing — there is no ingest-rejection counter (TG-371), " +
			"so the arrival gauges just go quiet, which reads exactly like a calm estate.")
	}
	if !strings.Contains(loc, "proxy_pass http://grounder:8080/") {
		t.Errorf("the /api/ proxy no longer targets the grounder container.\nGot: %s", loc)
	}
	// The trailing slash on proxy_pass is what STRIPS /api, so /api/v1/ingest/x reaches the grounder as
	// /v1/ingest/x. Without it every proxied path arrives with an /api prefix the router has no route for,
	// and every source gets 404 instead of being served.
	if !strings.Contains(loc, "grounder:8080/") {
		t.Errorf("proxy_pass has lost its trailing slash, so /api is no longer stripped and every proxied "+
			"request reaches the grounder with a prefix its router does not serve.\nGot: %s", loc)
	}
}

// The SPA fallback must not be the thing that answers an API path. `location /` with try_files matches by
// prefix and `location /api/` wins only because nginx prefers the longer prefix — so a change that shortens
// or removes the API location silently hands every API request to the static file server.
//
// KILLING MUTATION: change `location /api/` to `location /api` (no trailing slash) — still a prefix match,
// but it stops matching the `/api/...` requests sources actually send. RED.
func TestTheSPAFallbackDoesNotShadowTheAPIPrefix(t *testing.T) {
	conf := consoleNginxConf(t)
	if !strings.Contains(conf, "location /api/ {") {
		t.Error("the API location is not exactly `location /api/ {`. nginx picks the LONGEST matching prefix, " +
			"so any variation risks the SPA fallback (`location /`) answering API requests with index.html — " +
			"which returns 200 for a GET and 405 for a POST, and looks like a routing success from the outside")
	}
	if !strings.Contains(conf, "try_files $uri $uri/ /index.html") {
		t.Error("the SPA fallback changed shape; re-check that it still cannot shadow /api/")
	}
}

// ─── UNAUTHENTICATED-HARDENING GUARD ──────────────────────────────────────────────────────────────────────
// An SRE console must leak NOTHING to an unauthenticated request: the static shell is served ONLY behind an
// auth subrequest, an unauthenticated caller gets ONLY a minimal login page, and hardened headers ship on
// every response. Each test below names the killing mutation that re-opens the leak or breaks sign-in.

// KILLING MUTATION: delete `auth_request` — every unauthenticated browser is handed the full app bundle,
// fixtures and /v1/* endpoint names again, which is exactly the leak this closes.
func TestTheStaticShellIsAuthGated(t *testing.T) {
	conf := consoleNginxConf(t)
	root := regexp.MustCompile(`location\s+/\s*\{[^}]*\}`).FindString(conf)
	if root == "" {
		t.Fatal("no `location /` block — the app shell is unrouted")
	}
	if !strings.Contains(root, "auth_request /_authcheck") {
		t.Errorf("`location /` no longer gates on auth_request — an unauthenticated request is served the full "+
			"app shell (bundle + fixtures + /v1/* endpoint names). That is the leak.\nGot: %s", root)
	}
	// The fail path must land on the login page for EVERY non-2xx auth outcome — not just 401/403 but the
	// 5xx an auth_request raises when the grounder is down/timing-out. Without the 5xx codes a control-plane
	// blip reaches the operator as a bare nginx 500, locking out even a valid session (reviewer HIGH finding).
	if !strings.Contains(root, "error_page 401 403 500 502 503 504 = @login") {
		t.Errorf("`location /` error_page does not fail EVERY auth outcome (401/403 AND 5xx) to @login — a "+
			"grounder-down auth_request returns 5xx, which without these codes reaches the operator as a bare "+
			"500, locking out even a valid session during a control-plane blip.\nGot: %s", root)
	}
}

// KILLING MUTATION: drop `internal` (the auth oracle becomes a public endpoint) or drop the Cookie header
// (every request then looks unauthenticated, so no one can ever load the app).
func TestTheAuthCheckIsInternalAndHitsGrounderWhoami(t *testing.T) {
	conf := consoleNginxConf(t)
	ac := regexp.MustCompile(`location\s*=\s*/_authcheck\s*\{[^}]*\}`).FindString(conf)
	if ac == "" {
		t.Fatal("no `location = /_authcheck` block — the auth_request target does not exist, so nginx returns " +
			"500 for every gated request and the whole console is unreachable")
	}
	for _, must := range []string{"internal;", "proxy_pass http://grounder:8080/v1/whoami", "proxy_set_header Cookie $http_cookie"} {
		if !strings.Contains(ac, must) {
			t.Errorf("the auth subrequest is missing %q — without it the gate is wrong by construction.\nGot: %s", must, ac)
		}
	}
}

// KILLING MUTATION: auth_request-gate the login page itself, or move it into the SPA root. The first makes
// signing in impossible; the second turns it into dead weight under served_console_test's reachability guard.
func TestTheLoginPageIsServedUnauthenticatedFromItsOwnRoot(t *testing.T) {
	conf := consoleNginxConf(t)
	if !strings.Contains(conf, "location @login") || !strings.Contains(conf, "try_files /login.html") {
		t.Error("the @login fallback that serves the login page is gone — an unauthenticated request would 401 " +
			"with nothing to sign in with")
	}
	if !strings.Contains(conf, "root /usr/share/nginx/login") {
		t.Error("the login page is no longer served from its own root (/usr/share/nginx/login); if it moved into " +
			"the SPA document root it becomes dead weight under served_console_test's reachability guard")
	}
	if loginLoc := regexp.MustCompile(`location\s*=\s*/login\.html\s*\{[^}]*\}`).FindString(conf); strings.Contains(loginLoc, "auth_request") {
		t.Errorf("the login page is itself behind auth_request — an unauthenticated operator can never reach it.\nGot: %s", loginLoc)
	}
}

// KILLING MUTATION: remove any header, or move an add_header into a location (nginx add_header does NOT
// inherit into a location that sets its own, so the whole set silently stops covering that location).
func TestTheConsoleShipsHardenedSecurityHeaders(t *testing.T) {
	conf := consoleNginxConf(t)
	for _, must := range []string{
		"server_tokens off",
		`add_header X-Frame-Options "DENY"`,
		`add_header X-Content-Type-Options "nosniff"`,
		"add_header Referrer-Policy",
		"add_header Content-Security-Policy",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(conf, must) {
			t.Errorf("the console no longer ships %q — an SRE console must send hardened headers on every response", must)
		}
	}
	// The security headers must be at SERVER scope (before the first location), or nginx's add_header
	// inheritance rule silently strips them from any location that sets its own.
	if firstLoc, csp := strings.Index(conf, "location"), strings.Index(conf, "add_header Content-Security-Policy"); firstLoc >= 0 && csp > firstLoc {
		t.Error("the Content-Security-Policy add_header appears AFTER the first location — nginx add_header does " +
			"not inherit into a location that sets its own, so the security set must be at server scope")
	}
}

// The pre-auth page is the ONE thing an unauthenticated caller sees, so it must carry nothing usable: no app
// code, no fixtures, no real estate hostnames/IPs, no operator names, and no external asset.
func TestTheLoginPageLeaksNothing(t *testing.T) {
	b, err := os.ReadFile("console/login.html")
	if err != nil {
		t.Fatalf("read console/login.html: %v", err)
	}
	page := string(b)
	if len(page) < 200 {
		t.Fatalf("login.html is %d bytes — too small to be the real page; this guard would pass vacuously", len(page))
	}
	if ext := regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']https?://`).FindString(page); ext != "" {
		t.Errorf("login.html references an external origin (%q) — the pre-auth page must be self-contained", ext)
	}
	for _, bad := range []string{"Papadopoulos", "dc1fw01", "dc1pve01", "cloudbeaver01", "const SESSIONS", "const LEDGER", "kill-switch"} {
		if strings.Contains(page, bad) {
			t.Errorf("login.html leaks %q — the pre-auth page must reveal nothing about the estate or the app", bad)
		}
	}
	if !strings.Contains(page, "/api/v1/session") {
		t.Error("login.html does not POST to /api/v1/session — it cannot actually sign anyone in")
	}
}
