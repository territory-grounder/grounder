// Package screen is the pure, deterministic prompt-injection / jailbreak detector — a cheap first-line gate
// on untrusted text (an alert narrative, an issue body, a model turn). It is wired at BOTH trust
// boundaries: on the way IN (seed composition screens the alert summary, ticket, CMDB and precedent
// text before the model reasons over it — a hit is neutralized in place via Scrub and flagged, never
// dropped, so an embedded injection string cannot suppress triage) and on the way OUT (a detection on
// the model's proposal forces the session to the never-auto floor, POLL_PAUSE: a real jailbreak is
// never an auto-resolvable op).
//
// Provenance: [F] scripts/lib/jailbreak_detector.py (IFRNLLEI01PRD-748), the pure-regex 5-category detector
// re-expressed in Go. The port-fidelity audit flagged TG carrying a dead `jailbreak` never-auto slug
// (safety.go) with NO code producing it; this is that code. No LLM, no network, no allocation of trust to
// model output — INV-08.
//
// The five derailment categories: asterisk-obfuscation, persona-shift, retroactive-history-edit,
// context-injection, and lost-in-middle-bait. Input is normalized (zero-width strip + NFKC + a Cyrillic
// homoglyph fold) before matching so disguised tokens still match; Greek is a first-class operator language
// and is deliberately NOT folded to Latin.
//
// Beyond injection, Scrub also runs a deterministic secret/PII redaction pass (Redact) over the same
// untrusted text: a leaked credential embedded in an alert body (a bearer token, a labeled
// password/api_key/token value, a provider-prefixed key, a PEM private key, basic-auth URL userinfo, a
// high-entropy run in a key/secret/token context) is replaced with a stable [REDACTED:<kind>] marker
// before the text reaches the model or any log/ledger (spec/001 REQ-010, SK 6.3 — a live NetBox token was
// once found in a predecessor plaintext log). It is conservative: a benign alert body of hostnames,
// addresses, rule names, and numbers is left byte-identical.
package screen

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Category names the derailment class a match belongs to.
type Category string

const (
	CategoryAsterisk    Category = "asterisk-obfuscation"
	CategoryPersona     Category = "persona-shift"
	CategoryRetroactive Category = "retroactive-history-edit"
	CategoryContext     Category = "context-injection"
	CategoryLostMiddle  Category = "lost-in-middle-bait"
)

// Match is one detected pattern hit.
type Match struct {
	Category Category
	Pattern  string
}

// zeroWidth are the zero-width / joiner code points adversaries use to split banned tokens mid-word
// (U+200B..U+200D zero-width space/non-joiner/joiner, U+FEFF BOM/no-break, U+2060 word-joiner).
var zeroWidth = strings.NewReplacer(
	"\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "", "\u2060", "",
)

// homoglyphs folds the most-abused Cyrillic look-alikes of ASCII letters to Latin AFTER NFKC (NFKC does not
// fold cross-script homoglyphs). Greek is intentionally absent — it is a supported operator language.
var homoglyphs = strings.NewReplacer(
	"а", "a", "А", "A", "е", "e", "Е", "E", "о", "o", "О", "O",
	"р", "p", "Р", "P", "с", "c", "С", "C", "у", "y", "У", "Y",
	"х", "x", "Х", "X", "к", "k", "К", "K", "м", "M", "М", "M",
	"н", "H", "Н", "H", "т", "T", "Т", "T", "в", "B", "В", "B",
	"і", "i", "І", "I", "ѕ", "s", "Ѕ", "S", "ј", "j", "Ј", "J", "д", "g",
)

// Normalize folds disguised text toward canonical Latin so hidden tokens match: strip zero-width joiners,
// NFKC-fold compatibility/fullwidth forms, then fold Cyrillic homoglyphs. Used only for matching; callers
// keep the original for logging.
func Normalize(text string) string {
	if text == "" {
		return ""
	}
	return homoglyphs.Replace(norm.NFKC.String(zeroWidth.Replace(text)))
}

var (
	asteriskRE = regexp.MustCompile(`\b[a-zA-Z](?:\*[a-zA-Z]){2,}\b`)

	personaREs = compileAll(`(?i)`, []string{
		`\bignore (all |the |any )?previous (instructions|context|messages|prompts)\b`,
		`\byou are (now|actually) (a|an|the) `,
		`\bact as (if you (are|were)|a |an )`,
		`\bforget (everything|all|your) (above|prior|previous|instructions|context)\b`,
		`\bdisregard (your|the|all) (prior|previous|original) (instructions|prompt|directives)\b`,
		`\b(jailbreak|do anything now|DAN mode|developer mode)\b`,
		`\bpretend (you|that you) (have no|don't have) (restrictions|guardrails|limits)\b`,
		`αγνόησε [^.\n]{0,60}?(προηγούμενες|προηγούμενα|προηγούμενο)\s+(οδηγίες|μηνύματα|prompts|εντολές|εντολή|απαντήσεις)`,
		`ξέχασε [^.\n]{0,40}?(ξέρεις|έχω πει|έκανα|παραπάνω|είπα|έχουμε πει)`,
		`είσαι (τώρα|ουσιαστικά) (ένας|μία|μια|ο|η)`,
		`παρίστανε (ότι|πως)`,
		`παράκαμψε [^.\n]{0,40}?(περιορισμούς|κανόνες|guardrails|ασφαλείας)`,
		`αγνόησε (ότι|πως) είσαι`,
	})

	retroactiveREs = compileAll(`(?i)`, []string{
		`\bas (you|we) (already|previously) (said|agreed|decided|approved)\b`,
		`\bthe user (?:has |had |already )+(approved|authorized|consented)\b`,
		`\b(you said|you told me|you confirmed) (yes|that's fine|to proceed|approval)\b`,
		`\bwe (already|previously) (discussed|agreed) that (this|it) (is|was) (fine|ok|approved)\b`,
		`\bin (your|the) (previous|prior|earlier) (response|message) (you|we) (said|agreed|confirmed|told|decided)\b`,
		`όπως (ήδη |έχουμε )?συμφωνήσαμε`,
		`ο χρήστης (έχει |είχε )?ήδη (εγκρίνει|εξουσιοδοτήσει|αποδεχθεί|αποδεχτεί)`,
		`(είπες|είπατε|μου είπες) (ναι|προχώρα|εντάξει|να προχωρήσω)`,
		`στην προηγούμενη (απάντηση|αποκριση) (είπες|επιβεβαίωσες|συμφώνησες)`,
	})

	contextREs = compileAll(`(?im)`, []string{
		`<\s*system\s*>`,
		`<\s*/\s*system\s*>`,
		`\[INST\]`,
		`<\s*\|im_start\|\s*>`,
		`###\s*system\b`,
		`^\s*system:\s`,
		`\bSYSTEM PROMPT:\s`,
	})

	instructionVerbRE = regexp.MustCompile(`(?i)\b(execute|run|delete|remove|drop|exec|kill|shutdown|reboot|reset|wipe|format)\b`)
)

const (
	lostInMiddleThresholdChars = 800
	lostInMiddleTailChars      = 200
)

func compileAll(flags string, pats []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile(flags + p)
	}
	return out
}

// Detect returns every pattern match across the five categories (empty slice = no detection = allow).
func Detect(text string) []Match {
	n := Normalize(text)
	var out []Match
	if m := asteriskRE.FindString(n); m != "" {
		out = append(out, Match{CategoryAsterisk, m})
	}
	scan := func(res []*regexp.Regexp, cat Category) {
		for _, r := range res {
			if m := r.FindString(n); m != "" {
				out = append(out, Match{cat, m})
			}
		}
	}
	scan(personaREs, CategoryPersona)
	scan(retroactiveREs, CategoryRetroactive)
	scan(contextREs, CategoryContext)
	// lost-in-middle: a long body whose only instruction verb sits in the trailing window. The threshold and
	// tail window are measured in CODE POINTS (runes), not bytes — the constants are named "...Chars" and the
	// predecessor uses Python str length/slicing (code points). A byte measure over-fires on multibyte text
	// (an 800-byte body is far fewer than 800 characters, so a short non-ASCII prompt trips the gate) and can
	// slice mid-rune, producing an invalid-UTF-8 boundary that mis-places the tail window.
	if r := []rune(n); len(r) > lostInMiddleThresholdChars {
		tailStart := len(r) - lostInMiddleTailChars
		head, tail := string(r[:tailStart]), string(r[tailStart:])
		if !instructionVerbRE.MatchString(head) && instructionVerbRE.MatchString(tail) {
			out = append(out, Match{CategoryLostMiddle, "long-preamble-trailing-instruction"})
		}
	}
	return out
}

// IsJailbreak reports whether text carries any jailbreak / prompt-injection signal — the boolean gate the
// risk classifier consults to force the never-auto floor.
func IsJailbreak(text string) bool { return len(Detect(text)) > 0 }

// Marker renders the in-place neutralization marker the input screen substitutes for a detected span.
// The substitution is mechanical policy in code — never model-decided (INV-08).
func Marker(c Category) string { return "[SCREENED:" + string(c) + "]" }

// CategorySecretRedaction flags a span Scrub replaced because it matched a high-confidence secret /
// credential shape (spec/001 REQ-010, SK 6.3). It rides the same neutralize-and-flag channel the
// injection categories use — so the caller records that a credential was stripped and emits the scrubbed
// text — but it is deliberately NOT a Detect category: a leaked secret is a hygiene failure, not an
// attempted jailbreak, so redacting one MUST NOT force POLL_PAUSE (INV-08 — Detect/IsJailbreak unchanged).
const CategorySecretRedaction Category = "secret-redaction"

// SecretKind names the class of secret a redaction rule matched — the <kind> in the [REDACTED:<kind>]
// marker, so a scrubbed body still says WHAT kind of credential was removed without leaking its value.
type SecretKind string

const (
	SecretPrivateKey SecretKind = "private-key"
	SecretBasicAuth  SecretKind = "basic-auth"
	SecretBearer     SecretKind = "bearer-token"
	SecretAPIKey     SecretKind = "api-key"
	SecretToken      SecretKind = "token"
	SecretPassword   SecretKind = "password"
	SecretGeneric    SecretKind = "secret"
)

// RedactMarker renders the stable, model-safe substitution for a redacted secret span. Distinct from the
// injection [SCREENED:...] marker so a reader can tell a stripped credential from a defanged injection.
func RedactMarker(k SecretKind) string { return "[REDACTED:" + string(k) + "]" }

type redactRule struct {
	kind SecretKind
	re   *regexp.Regexp
	repl string
}

// redactRules is the ORDERED, curated set of high-confidence secret/credential shapes. Order matters:
// the whole-span rules (PEM, basic-auth userinfo, bearer header, provider-prefixed keys) run BEFORE the
// labeled key=value rules so a token is fully consumed and a key=value rule cannot stop at the first
// space and leave the tail exposed. The rules are DELIBERATELY conservative — every shape is either a
// labeled credential field, a provider-prefixed key, a PEM header, basic-auth userinfo, a bearer header,
// or a long high-entropy run in an explicit key/secret/token context. There is NO bare "any long hex is a
// secret" rule: a benign hostname, address, rule name, or number is never in one of these shapes, so a
// clean alert body is returned byte-identical (prefer a false-negative to mangling honest triage text).
// The labeled-value char class excludes '[' and ']' so a rule never re-wraps an already-placed
// [REDACTED:...] / [SCREENED:...] marker.
var redactRules = []redactRule{
	// PEM private-key block: collapse header..footer (any key type) to a single marker. RE2 has no
	// backreferences; the non-greedy body stops at the first END line.
	{SecretPrivateKey, regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), RedactMarker(SecretPrivateKey)},

	// basic-auth credentials embedded in a URL: keep the scheme + host, strip the user:password@ userinfo.
	{SecretBasicAuth, regexp.MustCompile(`(?i)((?:https?|ftp|ssh|redis|rediss|postgres(?:ql)?|mongodb(?:\+srv)?|amqps?|mysql)://)[^\s/:@]+:[^\s/@]+@`), `${1}` + RedactMarker(SecretBasicAuth) + `@`},

	// bearer / basic Authorization header token — consume the whole token so nothing trails. The {8,}
	// bound keeps benign prose ("basic auth is required", "bearer of bad news") from tripping.
	{SecretBearer, regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), RedactMarker(SecretBearer)},

	// provider-prefixed keys: GitLab PAT, GitHub token, AWS access-key id, Slack token. Distinctive
	// prefixes → high confidence with no keyword needed.
	{SecretAPIKey, regexp.MustCompile(`\b(?:glpat-[A-Za-z0-9_-]{20,}|gh[posur]_[A-Za-z0-9]{36,}|(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA)[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`), RedactMarker(SecretAPIKey)},

	// labeled api-key / access-key / secret-key / client-secret = value (optional surrounding quotes,
	// covers query-string, key: value, and JSON "key":"value" forms).
	{SecretAPIKey, regexp.MustCompile(`(?i)"?\b(?:api[_-]?key|apikey|access[_-]?key|secret[_-]?key|client[_-]?secret)\b"?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&")'\][]+)`), RedactMarker(SecretAPIKey)},

	// labeled token / authorization / auth = value.
	{SecretToken, regexp.MustCompile(`(?i)"?\b(?:access[_-]?token|private[_-]?token|token|authorization|auth)\b"?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&")'\][]+)`), RedactMarker(SecretToken)},

	// labeled password / passwd / pwd = value.
	{SecretPassword, regexp.MustCompile(`(?i)"?\b(?:pass(?:word|wd)?|pwd)\b"?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&")'\][]+)`), RedactMarker(SecretPassword)},

	// bare "secret = value".
	{SecretGeneric, regexp.MustCompile(`(?i)"?\bsecret\b"?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&")'\][]+)`), RedactMarker(SecretGeneric)},

	// a long high-entropy hex/base64 run in an explicit key/secret/token CONTEXT that the key=value rules
	// miss (space-separated form, e.g. "rotate the token <40-hex>"). Only the run is redacted; the label
	// survives. The {32,}/{40,} bounds keep short ids and ordinary numbers out.
	{SecretToken, regexp.MustCompile(`(?i)(\b(?:secret|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret)\b[ \t]+)([0-9A-Fa-f]{32,}|[A-Za-z0-9+/_-]{40,}={0,2})\b`), `${1}` + RedactMarker(SecretToken)},
}

// Redact deterministically replaces high-confidence secret/credential shapes in untrusted text with a
// stable [REDACTED:<kind>] marker and reports the kinds it stripped (nil = clean). It is the PII/secret
// half of the input screen (SK 6.3): no model call, no network, conservative by construction. Clean text
// is returned byte-identical (each rule is probed with a cheap MatchString first, so the common no-secret
// path allocates nothing and returns the original string). A live NetBox token once landed in a
// predecessor plaintext log; this is the pass that keeps that class of leak out of the model seed and the
// governance ledger.
func Redact(text string) (string, []SecretKind) {
	if text == "" {
		return text, nil
	}
	out := text
	var kinds []SecretKind
	seen := map[SecretKind]bool{}
	for _, r := range redactRules {
		if !r.re.MatchString(out) {
			continue
		}
		out = r.re.ReplaceAllString(out, r.repl)
		if !seen[r.kind] {
			seen[r.kind] = true
			kinds = append(kinds, r.kind)
		}
	}
	if len(kinds) == 0 {
		return text, nil // byte-identical, no retained allocation
	}
	return out, kinds
}

// redactionMatch summarizes a redaction as one neutralize-and-flag Match carrying the stripped kinds in
// its Pattern, so screenCategories renders "secret-redaction" in the seed-provenance note.
func redactionMatch(kinds []SecretKind) Match {
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = string(k)
	}
	return Match{Category: CategorySecretRedaction, Pattern: strings.Join(parts, ",")}
}

// Scrub neutralizes detected prompt-injection / jailbreak spans AND redacts leaked secrets in untrusted
// text — the NEUTRALIZE half of the input screen's neutralize-and-flag policy. Clean text (no injection,
// no secret) is returned byte-identical. On an injection detection the text is folded to its normalized
// form (zero-width strip + NFKC + homoglyph fold — so a disguised span cannot survive the substitution)
// and every span any category's patterns match is replaced with its [SCREENED:<category>] marker; a
// lost-in-middle hit additionally defangs the instruction verbs in the trailing window. INDEPENDENTLY —
// on every path, whether or not an injection fired — a high-confidence secret/credential (bearer token,
// labeled password/token/api_key, provider-prefixed key, PEM private key, basic-auth URL userinfo, a
// high-entropy run in a key/secret/token context) is REDACTED to a [REDACTED:<kind>] marker before the
// text is returned, so no credential reaches the model, a log, or the governance ledger (spec/001
// REQ-010, SK 6.3). A redaction contributes a CategorySecretRedaction Match so the returned slice is
// non-empty and the caller emits the scrubbed text and flags the redaction (redacting a secret is NOT a
// Detect hit and NEVER forces POLL_PAUSE). The returned matches are the caller's log / session-record
// flag. Scrub never drops the text: an attacker must not be able to suppress triage by embedding an
// injection string (under-triage is the worse failure), so the alert survives with the hostile span
// defanged and any leaked credential stripped.
func Scrub(text string) (string, []Match) {
	ms := Detect(text)
	if len(ms) == 0 {
		// No injection — but the untrusted text may still carry a leaked secret. Redact it so no
		// credential reaches the model or a log/ledger; a clean, secret-free body stays byte-identical.
		if red, kinds := Redact(text); len(kinds) > 0 {
			return red, []Match{redactionMatch(kinds)}
		}
		return text, nil
	}
	n := Normalize(text)
	// Replace every match of EVERY category's patterns (not only the categories that fired): the pattern
	// sets are the closed detection grammar, so one pass over all of them leaves nothing Detect could
	// still literally match — the neutralized text cannot re-trip the screen downstream.
	n = asteriskRE.ReplaceAllString(n, Marker(CategoryAsterisk))
	for _, r := range personaREs {
		n = r.ReplaceAllString(n, Marker(CategoryPersona))
	}
	for _, r := range retroactiveREs {
		n = r.ReplaceAllString(n, Marker(CategoryRetroactive))
	}
	for _, r := range contextREs {
		n = r.ReplaceAllString(n, Marker(CategoryContext))
	}
	for _, m := range ms {
		if m.Category != CategoryLostMiddle {
			continue
		}
		// Lost-in-middle has no literal span — the structure (a long preamble whose only instruction verb
		// trails) is the signal. Defang the instruction verbs in the trailing window (recomputed over the
		// current text, rune-measured like Detect) so the buried instruction is inert but the body survives.
		r := []rune(n)
		tailStart := 0
		if len(r) > lostInMiddleTailChars {
			tailStart = len(r) - lostInMiddleTailChars
		}
		n = string(r[:tailStart]) + instructionVerbRE.ReplaceAllString(string(r[tailStart:]), Marker(CategoryLostMiddle))
		break
	}
	// Redact any leaked secret that survived the neutralized text before it is emitted (feeds the model
	// AND the log/ledger). Runs over the normalized fold so a disguised credential cannot slip past.
	if red, kinds := Redact(n); len(kinds) > 0 {
		n = red
		ms = append(ms, redactionMatch(kinds))
	}
	return n, ms
}
