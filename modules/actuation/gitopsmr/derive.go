package gitopsmr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// branchName derives the DETERMINISTIC tg/ branch a propose opens on. Deterministic (a stable hash of the
// op-class + the sorted edits) so a redelivery/retry targets the SAME branch rather than spraying tg/ branches
// — the true single-open idempotency is the deferred-verify channel's action_id Reserve (REQ-1712); this just
// keeps the branch stable. The prefix is the operator's reserved tg/ namespace so the sensor never fights
// renovate/* or Atlantis dirs.
func branchName(pol RepoPolicy, spec ProposeSpec) string {
	prefix := strings.TrimSpace(pol.BranchPrefix)
	if prefix == "" {
		prefix = "tg/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", strings.TrimSpace(spec.OpClass), strings.TrimSpace(spec.RepoID))
	for _, e := range sortedEdits(spec.Edits) {
		fmt.Fprintf(h, "%s=%s\n", e.FieldRuleID, e.NewValue)
	}
	return prefix + slugify(spec.OpClass) + "-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// mrText renders the MR title + body prose. It carries NO secret: the op-class, the target path, and the
// rationale + a rule-id→value summary (the values are already secret-guarded before this is called). The body
// is human-readable review context, never file content.
func mrText(pol RepoPolicy, spec ProposeSpec) (title, body string) {
	title = fmt.Sprintf("TG: %s on %s", strings.TrimSpace(spec.OpClass), strings.TrimSpace(pol.ProjectPath))
	var b strings.Builder
	fmt.Fprintf(&b, "Proposed by Territory Grounder (gitops-mr lane) for op-class `%s`.\n\n", strings.TrimSpace(spec.OpClass))
	if r := strings.TrimSpace(spec.Rationale); r != "" {
		fmt.Fprintf(&b, "Rationale: %s\n\n", r)
	}
	b.WriteString("Field edits:\n")
	for _, e := range sortedEdits(spec.Edits) {
		fmt.Fprintf(&b, "- `%s` = `%s`\n", e.FieldRuleID, e.NewValue)
	}
	b.WriteString("\nTG opens this MR and STOPS — it never merges, never comments `atlantis apply`, and never touches the cluster API. A human reviews the plan and applies.")
	return title, b.String()
}

// sortedEdits returns the edits in a stable order (by FieldRuleID) so the branch hash and MR body are
// deterministic regardless of the caller's slice order.
func sortedEdits(edits []FieldEdit) []FieldEdit {
	out := append([]FieldEdit(nil), edits...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].FieldRuleID != out[j].FieldRuleID {
			return out[i].FieldRuleID < out[j].FieldRuleID
		}
		return out[i].NewValue < out[j].NewValue
	})
	return out
}

// slugify reduces an op-class to a branch-safe token.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '/' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "change"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// secretMarkers are shapes that MUST NOT appear as decoded values in Git (design §4): PEM key blocks and
// well-known provider token prefixes. Only references (SecretRef / External-Secrets remoteRef plumbing) belong
// in Git. This is a CONSERVATIVE guard (no generic high-entropy check, so an image digest `sha256:…` or a git
// SHA is not a false positive); the concrete FieldRule-aware renderer enforces reference-only edits more
// precisely (a later increment). Case-insensitive on the marker.
var secretMarkers = []string{
	"-----BEGIN ",                                 // any PEM private key / certificate block
	"glpat-",                                      // GitLab personal access token
	"gldt-",                                       // GitLab deploy token
	"github_pat_", "ghp_", "gho_", "ghs_", "ghr_", // GitHub tokens
	"xoxb-", "xoxp-", "xapp-", // Slack
	"sk-",  // OpenAI-style
	"AKIA", // AWS access key id
	"ASIA", // AWS temporary key id
	"AIza", // Google API key
	"hvs.", // Vault/OpenBao service token
	"s.",   // (legacy Vault token prefix — narrow; see note)
}

// guardNoSecretValues hard-fails if any rendered file OR the rationale contains a decoded secret value. It is
// the pre-write guard of design §4: a decoded value in Git either leaks a literal or is a no-op under
// External-Secrets. Returns ErrSecretInPatch on the first hit.
func guardNoSecretValues(files map[string][]byte, rationale string) error {
	if hit, marker := scanSecret(rationale); hit {
		return fmt.Errorf("%w: rationale carries a %q-shaped value", ErrSecretInPatch, marker)
	}
	for path, content := range files {
		if hit, marker := scanSecret(string(content)); hit {
			return fmt.Errorf("%w: file %q carries a %q-shaped value", ErrSecretInPatch, path, marker)
		}
	}
	return nil
}

// scanSecret reports whether s contains any secret marker (case-insensitive for the alpha markers; the "s."
// legacy-Vault marker is matched only as a standalone token to avoid false positives on ordinary prose).
func scanSecret(s string) (bool, string) {
	lower := strings.ToLower(s)
	for _, m := range secretMarkers {
		if m == "s." { // narrow legacy-Vault marker: require it to look like a token (s.<24+ token chars>)
			if legacyVaultToken(s) {
				return true, m
			}
			continue
		}
		if strings.Contains(lower, strings.ToLower(m)) {
			return true, m
		}
	}
	return false, ""
}

// legacyVaultToken matches the narrow `s.<24+ base62>` legacy-Vault shape as a standalone token, so ordinary
// prose containing "s." (end of a sentence) is not a false positive.
func legacyVaultToken(s string) bool {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '"' || r == '\'' || r == '=' || r == ':'
	}) {
		if !strings.HasPrefix(tok, "s.") {
			continue
		}
		body := tok[2:]
		if len(body) < 24 {
			continue
		}
		ok := true
		for _, r := range body {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
