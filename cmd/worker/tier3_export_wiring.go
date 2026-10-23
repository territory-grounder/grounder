package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/screen"
	exportlangfuse "github.com/territory-grounder/grounder/modules/export/langfuse"
	exportotel "github.com/territory-grounder/grounder/modules/export/otel"
)

// The Tier-3 LLM-observability export lane (spec/020 REQ-2020), wired DARK by default.
//
// This is the composition-root arm for modules/export/otel + modules/export/langfuse. It wraps the agent's
// model completer so that — ONLY when an org admin has turned the lane on — the REDACTED LLM subset of each
// model call (model slug + prompt + completion, secrets Scrubbed) is shipped to Langfuse over an OTel span
// backbone. It is additive and OFF the decision path: the wrap runs AFTER the completion is in hand, ships it
// on a detached goroutine with a bounded-timeout client, returns the completion UNCHANGED, and swallows every
// export error. When the lane is unconfigured the completer is returned untouched — zero overhead.

// tier3ExportConfig is the lane's env-driven configuration. Every field is read via os.Getenv so the
// compose-env-parity gate (deploy TestComposeEnvParity) sees the keys and forces them into the compose file
// — the key can ARRIVE. The Langfuse keys are SecretRef references (env:/bao:…), never literals (INV-13).
type tier3ExportConfig struct {
	enabled   bool
	endpoint  string
	publicRef config.SecretRef
	secretRef config.SecretRef
}

// Enabled reports whether the lane should ship: the toggle is on AND the endpoint + both key refs are set.
func (c tier3ExportConfig) Enabled() bool {
	return c.enabled && c.endpoint != "" && c.publicRef != "" && c.secretRef != ""
}

func readTier3ExportConfig() tier3ExportConfig {
	// getenv routes through the boot-config resolver (never raw os.Getenv), so an operator-saved override of
	// these settings is honored.
	return tier3ExportConfig{
		enabled:   getenv("TG_TRACE_LLM_EXPORT_ENABLED", "") == "1",
		endpoint:  getenv("TG_TRACE_LLM_EXPORT_LANGFUSE_ENDPOINT", ""),
		publicRef: config.SecretRef(getenv("TG_TRACE_LLM_EXPORT_LANGFUSE_PUBLIC", "")),
		secretRef: config.SecretRef(getenv("TG_TRACE_LLM_EXPORT_LANGFUSE_SECRET", "")),
	}
}

// scrubRedactor is the injected redaction path REQ-2020 requires (REQ-2008/REQ-2015): core/screen.Scrub
// strips any leaked secret and defangs any injection span before the text becomes a span attribute, so no
// credential and no hostile payload reaches the external store verbatim.
func scrubRedactor(s string) (string, int) {
	out, matches := screen.Scrub(s)
	return out, len(matches)
}

// recordingCompleter is the completer decorator. It records the redacted LLM subset of a SUCCESSFUL model call
// on a detached goroutine (off the decision path) and returns the inner result unchanged.
type recordingCompleter struct {
	inner agent.Completer
	bb    *exportotel.Backbone
}

func (r *recordingCompleter) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	out, err := r.inner.Complete(ctx, user, modelName, msgs)
	if err == nil {
		// Detached + best-effort: the export never blocks the model call, never changes the completion, and a
		// slow/absent Langfuse is bounded by the exporter's HTTP timeout, not by this call.
		sub := exportotel.LLMSubset{Model: modelName, Input: joinMessages(msgs), Output: out}
		go r.bb.RecordModelCall(context.Background(), user, sub)
	}
	return out, err
}

// joinMessages renders the prompt messages into a single input string (role: content per line). Redaction
// runs on this inside the backbone, so an estate secret in any message is scrubbed before export.
func joinMessages(msgs []model.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
	}
	return b.String()
}

// wrapTier3Export wraps inner with the Tier-3 recorder when the lane is enabled, and returns the (possibly
// wrapped) completer plus the boot-log line that states the posture. DARK by default: unconfigured ⇒ inner is
// returned untouched. The boot log names the exact keys that ARM it, so an operator reading "DARK" knows the
// key path exists and how to turn it on (never a silent "not configured").
func wrapTier3Export(inner agent.Completer, cfg tier3ExportConfig) (agent.Completer, string) {
	if !cfg.Enabled() {
		return inner, "tier-3 LLM-observability export: DARK — optional, off by default; set " +
			"TG_TRACE_LLM_EXPORT_ENABLED=1 + TG_TRACE_LLM_EXPORT_LANGFUSE_ENDPOINT + _PUBLIC + _SECRET (secret " +
			"refs) to arm the redacted-LLM-subset lane to Langfuse (never on the decision path)"
	}
	exp := exportlangfuse.New(cfg.endpoint, cfg.publicRef, cfg.secretRef,
		exportlangfuse.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	bb := exportotel.NewBackbone(exp, true, scrubRedactor)
	return &recordingCompleter{inner: inner, bb: bb},
		"tier-3 LLM-observability export ARMED — the redacted LLM subset (secrets Scrubbed) ships to Langfuse " +
			cfg.endpoint + " over an OTel span backbone; additive, best-effort, off the decision path"
}

// wireTier3Export is the composition-root one-liner: it reads the config, applies wrapTier3Export, and emits
// the posture boot log. Kept out of main() so the god-file LOC ratchet (TG-501) stays flat.
func wireTier3Export(inner agent.Completer) agent.Completer {
	got, bootLog := wrapTier3Export(inner, readTier3ExportConfig())
	log.Print(bootLog)
	return got
}
