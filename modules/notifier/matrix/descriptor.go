package matrix

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Matrix's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. matrix.go:121 resolves the token reference inside
// do(), on every call, so a secret written to the lane below takes effect on the NEXT send with no
// restart. The other fields are read at boot in cmd/worker/main.go, so a save is durable but inert until
// the worker restarts — and the dialog must say that instead of implying success.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Matrix",
		Summary: "Governance notices and approval polls to a Matrix room, and — when the inbound vote " +
			"reader is armed — the votes cast back on them.",
		Fields: []desc.Field{
			{
				Name: "homeserver", EnvKey: "TG_MATRIX_HOMESERVER", Label: "Homeserver URL",
				Help: "Base URL of the Matrix homeserver, e.g. https://matrix.example.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "default_room", EnvKey: "TG_MATRIX_DEFAULT_ROOM", Label: "Approvals room",
				Help: "Room every notice and poll posts to unless a per-project route overrides it.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectLive,
				Required: true, MaxLen: 256,
			},
			{
				Name: "rooms", EnvKey: "TG_MATRIX_ROOMS", Label: "Routed rooms",
				Help: "Optional per-project routing: \"#tg-approvals=!room:example, #ops=!ops:example\".",
				Type: desc.TypeKVMap, Security: desc.SecOrdinary, Effect: desc.EffectLive,
				MaxItems: 32, MaxLen: 256,
			},
			{
				// AUTHORITY, not ordinary text. This is the set of identities whose vote can release a
				// governed action (INV-12). Its ARMED status must be rendered from the vote.inbound
				// wiring seam — an approver list consumed by nothing is a control that looks enforced
				// and enforces nothing, and the field's own non-emptiness can never reveal that.
				Name: "approvers", EnvKey: "TG_MATRIX_APPROVERS", Label: "Allowed responders",
				Help: "Matrix ids whose vote TG accepts (e.g. @oncall:example). A reply or poll answer " +
					"from anyone else is refused. The bot's own id must never appear here.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectLive,
				Required: true, Pattern: `^@[^:\s]+:[^\s]+$`, MaxItems: 32, MaxLen: 256,
			},
			{
				Name: "token_ref", EnvKey: "TG_MATRIX_TOKEN_REF", Label: "Access-token reference",
				Help: "Where the bot's access token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Bot access token",
				Help: "The Matrix access token TG posts as. Write-only: it is stored in the secret backend " +
					"and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_MATRIX_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change per module. Every rotation after
		// that is a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring becomes
		// noise aimed at the room that has to stay readable during an incident.
		Test: desc.TestSpec{Verb: "post a test message to the approvals room", Mutating: false, Emits: true},
	}
}
