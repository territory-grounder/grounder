package notifier

import "regexp"

// The channel-agnostic obligations every notifier-and-approval backend inherits (REQ-806/808/809): a body
// posted to a human channel is credential- and PII-redacted first, a vote is accepted only from a sender
// in the approver set, and each vote binds to the specific pending decision it answers. These live on the
// interface package so no backend can ship without them. [O] INV-12/INV-19, spec/008.

// redactRule is a (pattern, replacement) pair. Order matters: the bearer/basic rule runs BEFORE the
// key=value rule so "Authorization: Bearer <token>" is fully consumed (otherwise key=value would stop at
// the first whitespace and leave the token exposed).
type redactRule struct {
	re   *regexp.Regexp
	repl string
}

var redactRules = []redactRule{
	// bearer / basic credential headers — redact the whole "Bearer <token>" run first.
	{regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=\-]+`), "[REDACTED]"},
	// JSON quoted credential fields: "password":"…", "api_key": "…", "token":"…" — mask the value, keep
	// the key so structured notices (API error bodies, logs) do not leak credentials to the channel.
	{regexp.MustCompile(`(?i)("(?:pass(?:word|wd)?|pwd|secret|api[_-]?key|access[_-]?key|token|authorization|auth)"\s*:\s*)"[^"]*"`), `${1}"[REDACTED]"`},
	// key=value / key: value credential pairs (non-JSON).
	{regexp.MustCompile(`(?i)\b(?:pass(?:word|wd)?|pwd|secret|api[_-]?key|access[_-]?key|token|authorization|auth)\b\s*[:=]\s*\S+`), "[REDACTED]"},
	// email addresses (PII).
	{regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), "[REDACTED]"},
}

// Redact masks credentials and PII in a message body before it is posted to a human channel. It handles
// key=value and key: value pairs, quoted JSON credential fields (endemic to API error bodies), bearer/basic
// headers, and email addresses. It is a channel-agnostic obligation every notifier backend inherits — a
// backend calls Redact on the Notice.Body before posting so no secret or PII reaches the channel. A bare
// high-entropy token with no keyword is inherently undetectable and out of scope. [O] INV-19.
func Redact(body string) string {
	out := body
	for _, r := range redactRules {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	return out
}

// Authenticate reports whether sender is in the approver set — the sender-authentication obligation every
// approval backend discharges BEFORE accepting a vote, so an unauthorized sender's vote is never counted
// (INV-12). An empty sender or an empty approver set denies (fail closed).
func Authenticate(sender string, approvers []string) bool {
	if sender == "" {
		return false
	}
	for _, a := range approvers {
		if a == sender {
			return true
		}
	}
	return false
}
