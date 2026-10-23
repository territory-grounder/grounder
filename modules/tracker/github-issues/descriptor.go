package githubissues

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes GitHub Issues' configuration schema so the console can GENERATE its dialog rather
// than hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. github_issues.go:79 resolves the token reference
// inside do(), on every request, so a secret written to the lane below takes effect on the NEXT call with
// no restart. EVERY OTHER FIELD IS BOOT-ONLY: cmd/worker/main.go:834-837 reads them once and passes them
// to New, which captures them in the Module — a save is durable but inert until the worker restarts, and
// the dialog must say so rather than implying success.
//
// There are exactly four ENV-BACKED fields because the composition root reads exactly four keys (the
// fifth field is the secret value, which carries no env key by construction). This module has no state
// mapping to configure: GitHub issues are open|closed, and the fold onto the tracker's three states is
// fixed in code, so there is nothing here for an operator to get wrong.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "tracker",
		SourceType: SourceType,
		Title:      "GitHub Issues",
		Summary: "One GitHub repository's issues as the incident tracker: the issue TG is triggered by, " +
			"closes or reopens, and comments on as the session's terminal audit sink.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_GITHUB_URL", Label: "API base URL",
				Help: "https://api.github.com for github.com, or https://<host>/api/v3 for GitHub " +
					"Enterprise Server. Empty means this tracker is NOT a capability of this deployment: " +
					"it is never registered and no session can be anchored to a GitHub issue.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// owner/repo is the module's entire blast radius: it is scoped to ONE repository and every
				// path is built from these two values (github_issues.go issuePath). The dangerous typo is
				// NOT the one that 404s — it is the one naming a repository that exists and the token can
				// reach, which succeeds silently against the wrong estate.
				Name: "owner", EnvKey: "TG_GITHUB_OWNER", Label: "Repository owner",
				Help: "User or organisation that owns the repository. With the repo below it is the only " +
					"place TG reads or writes — every request path is built from these two.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 128,
			},
			{
				Name: "repo", EnvKey: "TG_GITHUB_REPO", Label: "Repository name",
				Help: "Repository holding the incident issues, without the owner prefix. A name that does " +
					"not exist fails loudly with a 404; a name that DOES exist and this token can reach " +
					"does not fail at all — TG comments on and closes issues in that repository instead.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 128,
			},
			{
				Name: "token_ref", EnvKey: "TG_GITHUB_TOKEN_REF", Label: "Access-token reference",
				Help: "Where the access token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Access token",
				Help: "Token TG authenticates as, scoped to the repository above. Its scopes are the real " +
					"ceiling on what TG can do here. Write-only: it is stored in the secret backend and " +
					"never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_GITHUB_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("tracker", SourceType), Field: "token"},
		// The verb says what selftest.go actually does, because the dialog shows it BEFORE the press and a
		// promise the probe cannot honour is the defect this capability exists to close. It still reads
		// one issue — but from the LIST endpoint, because a hardcoded issue number would turn "that issue
		// was deleted" into something indistinguishable from "this token is on the wrong repository",
		// which is the very mistake the owner/repo help text above warns about.
		Test: desc.TestSpec{
			Verb: "read the configured repository, one issue from its list, and the account this token " +
				"belongs to — three read-only GETs; nothing is created, closed, or commented on",
			Mutating: false,
		},
	}
}
