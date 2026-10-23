package pve

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the PVE topology reader's configuration schema so the console GENERATES its dialog
// rather than hand-rendering one that drifts from the binary.
//
// EFFECT PER FIELD, HONESTLY. The URL, the cascade alert and the TLS flag are read ONCE at boot
// (cmd/worker/main.go:1163-1169) and captured in the constructed EstateSource, so a save is durable but
// INERT until the worker restarts — the periodic estate refresh re-runs Edges() against the SAME object and
// never re-reads the environment. The token is different: pve.go's get() resolves the secret reference on
// every request, so a rotation written to the lane below is picked up by the next read.
//
// This is the READ-ONLY estate reader, not the proxmox ACTUATION module. Nothing configured here can start,
// stop or reboot a guest, and the credential this dialog writes belongs on the read-triage plane — boot
// fails closed if it is shared with an actuation reference.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "cmdb",
		SourceType: SourceType,
		Title:      "Proxmox VE (estate topology)",
		Summary: "Reads guest placement from the PVE cluster API and contributes `runs_on` edges — the " +
			"highest-confidence estate relationship TG has (0.95), because the live hypervisor is the source " +
			"of truth for what runs where.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_PVE_URL", Label: "PVE API base URL",
				Help: "Base URL of the Proxmox cluster API including its port, e.g. https://pve01.example:8006. " +
					"Left empty, no guest→node edge is seeded from the hypervisor at all: a failing node's " +
					"blast radius reaches none of the guests placed on it.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "cascade_alert", EnvKey: "TG_PVE_CASCADE_ALERT", Label: "Expected cascade alert",
				Help: "Name of the alert a node→guest cascade is expected to fire (default \"HostDown\"). It is " +
					"stamped on every edge this source emits and becomes the rule the verifier expects on each " +
					"predicted guest. Use the name YOUR monitoring actually sends: a predicted host that alerts " +
					"under an unpredicted rule verifies PARTIAL, not match — so a wrong name here downgrades " +
					"cascades TG predicted correctly. A different spelling of the same fault is forgiven only " +
					"when it is a known family sibling of the observed rule.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				// AUTHORITY, not an ordinary toggle. Switching this on means TG accepts ANY certificate for
				// the PVE endpoint — it decides WHAT is trusted to answer as the hypervisor, so it moves a
				// trust boundary and must be visibly marked. Rendered as a plain checkbox it is the setting
				// that gets flipped during a debugging session and is never flipped back.
				Name: "insecure_tls", EnvKey: "TG_PVE_INSECURE", Label: "Skip TLS verification",
				Help: "Accept the PVE endpoint's certificate WITHOUT verifying it (PVE serves a self-signed " +
					"cert on :8006). This disables endpoint authentication for the estate reader AND for the " +
					"PVE actor-evidence reader. It should agree with the actuation lane's TG_PROXMOX_INSECURE; " +
					"a disagreement is reported at boot rather than silently resolved in either direction.",
				Type: desc.TypeBool, Security: desc.SecAuthority, Effect: desc.EffectRestart,
			},
			{
				Name: "token_ref", EnvKey: "TG_PVE_TOKEN_REF", Label: "API-token reference",
				Help: "Where the PVE API token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer. CHECK IT NAMES THIS MODULE'S LANE — the reader reads whatever " +
					"this points at, so while it still points somewhere else (the env: default, say) a token " +
					"saved below is stored in the lane and never read. It is a READ-TRIAGE reference — if the " +
					"identical reference is also configured for an actuation credential the worker refuses to " +
					"start (plane split).",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "PVE API token",
				Help: "The full `user@realm!tokenid=secret` value TG reads the cluster with. Give it AUDIT " +
					"scope only (PVEAuditor): a token that can also mutate guests would put an actuation " +
					"credential on the triage plane. Write-only here — stored in the secret backend and never " +
					"read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path, because a
		// descriptor that could name one could point the console's writer at another module's secret — and
		// this cluster has three distinct tokens (read, actor-evidence, actuation) that must not converge.
		// TG_PVE_TOKEN_REF must point HERE for a Save to reach the reader; that is a one-time change to the
		// pointer, after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("cmdb", SourceType), Field: "token"},
		Test: desc.TestSpec{
			Verb:     "list the cluster's guests and the node each one runs on",
			Mutating: false,
		},
	}
}
