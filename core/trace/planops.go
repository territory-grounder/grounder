package trace

import (
	"sort"
	"strings"
)

// planops.go projects a sealed action (its op-class/op/target/params) to the structured end-state ops the
// tracer's commit boundary renders. PURE and observe-only: it classifies the op's mutation polarity and formats
// a human-readable target — it re-enters no decision path, actuates nothing, and fabricates no before→after
// delta (INV-15). Credential/secret material never reaches here (params are the manifest's non-secret args,
// INV-13); still, only the target + op-class + param keys/values the manifest already committed are formatted.

// change/destroy/add token sets. Classification order matters: CHANGE is tested BEFORE add so "restart"/"reload"
// resolve to change rather than falling through to the "start" add-token; read-only tokens map to no op at all.
var (
	planReadTokens    = []string{"get", "list", "show", "describe", "status", "read", "watch", "inspect", "diag", "ping", "check", "lookup", "query"}
	planChangeTokens  = []string{"restart", "reload", "set", "update", "patch", "scale", "rotate", "modify", "change", "reconcile", "apply", "tune", "adjust", "cordon", "drain"}
	planDestroyTokens = []string{"delete", "destroy", "remove", "prune", "drop", "purge", "uninstall", "deprovision", "teardown", "evict", "stop", "disable", "kill", "revoke"}
	planAddTokens     = []string{"create", "add", "provision", "install", "deploy", "enable", "start", "bootstrap", "register", "mount", "attach"}
)

// planTokenize splits an op-class/op into its lowercase alphanumeric segments (on any non-alphanumeric
// boundary: '-', '_', '/', space). Whole-token matching — NOT substring — so a mutating class whose NAME
// merely CONTAINS a read token ("healthcheck-restart" contains "check", "set-status" contains "status") is not
// mis-read as read-only.
func planTokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func planHasToken(tokens, want []string) bool {
	for _, tk := range tokens {
		for _, w := range want {
			if tk == w {
				return true
			}
		}
	}
	return false
}

// ClassifyPlanOp maps an op-class (falling back to the op verb) to a mutation polarity: "add" | "change" |
// "destroy", or "" for a read-only op (which mutates no end-state and yields no plan op). An unknown mutating
// op resolves to "change" — the honest "this changes state, polarity unknown" answer, never a fabricated
// add/destroy. A MUTATION token anywhere wins over a read token, and read-only is returned ONLY when no
// mutation token is present — a tracer must never UNDER-report a mutation as read-only (the safe direction).
func ClassifyPlanOp(opClass, op string) string {
	s := strings.ToLower(strings.TrimSpace(opClass))
	if s == "" {
		s = strings.ToLower(strings.TrimSpace(op))
	}
	if s == "" {
		return ""
	}
	tokens := planTokenize(s)
	switch {
	case planHasToken(tokens, planChangeTokens): // BEFORE add so restart/reload ≠ start
		return "change"
	case planHasToken(tokens, planDestroyTokens):
		return "destroy"
	case planHasToken(tokens, planAddTokens):
		return "add"
	case planHasToken(tokens, planReadTokens):
		return "" // read-only — ONLY when no mutation token is present
	default:
		return "change"
	}
}

// ProjectPlanOps returns the sealed action as structured end-state ops. A read-only action yields nil (no
// end-state change to commit). One sealed action targets one resource today, so a single op is returned; the
// slice keeps the contract future-proof.
func ProjectPlanOps(target, opClass, op string, params map[string]string) []PlanOp {
	pol := ClassifyPlanOp(opClass, op)
	if pol == "" {
		return nil
	}
	var b strings.Builder
	if target != "" {
		b.WriteString(target)
	}
	if opClass != "" {
		if b.Len() > 0 {
			b.WriteString(" — ")
		}
		b.WriteString(opClass)
	}
	if op != "" && !strings.EqualFold(op, opClass) {
		b.WriteString(" (" + op + ")")
	}
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic order (INV — reproducible projection)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+params[k])
		}
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(strings.Join(parts, " "))
	}
	t := strings.TrimSpace(b.String())
	if t == "" {
		t = pol // never an empty target
	}
	return []PlanOp{{Op: pol, T: t}}
}

// proposeProvenance formats the composed-seed identity (session_identity, migration 0027) into the propose
// step's prompts provenance list (REQ-2009/REQ-2000): the prompt version, the seed HASH, and the model tier —
// only the fields the session actually recorded, each labeled. Non-secret identifiers (a version/tier string
// and a content hash, never key material — INV-13).
func proposeProvenance(promptVersion, seedHash, modelTier string) []string {
	var out []string
	if promptVersion != "" {
		out = append(out, "prompt: "+promptVersion)
	}
	if seedHash != "" {
		out = append(out, "seed: "+seedHash)
	}
	if modelTier != "" {
		out = append(out, "model: "+modelTier)
	}
	return out
}

// screenSignals formats the classifier's admission-screen signals (session_risk_audit.signals_json) into a
// stable, human-readable list, sorted by key for a deterministic projection. Non-secret by construction — the
// normalized signals that drove the band (poll_reason / never-auto-floor / blast-radius), INV-08.
func screenSignals(sig map[string]string) []string {
	if len(sig) == 0 {
		return nil
	}
	keys := make([]string, 0, len(sig))
	for k := range sig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := sig[k]; v != "" {
			out = append(out, k+": "+v)
		} else {
			out = append(out, k)
		}
	}
	return out
}
