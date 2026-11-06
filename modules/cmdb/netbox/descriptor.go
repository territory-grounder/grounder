package netbox

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes NetBox's configuration schema so the console GENERATES its dialog rather than
// hand-rendering one that drifts from the binary the first time this module gains a field.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. Every env-backed field here is read ONCE at boot in
// cmd/worker/main.go — line 919 constructs the CMDB resolver, line 1145 the VM-placement topology source,
// line 3084 the change-author reader — and each captures its value in the constructed object. A save is
// therefore durable but INERT until the worker restarts, and the dialog must say so instead of implying
// success. The token is the one genuine exception: netbox.go's do() resolves the secret reference inside
// every request, so a rotation written to the lane below is picked up by the next read with no restart.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "cmdb",
		SourceType: SourceType,
		Title:      "NetBox",
		Summary: "The authoritative entity source every ingested payload is re-read against before dispatch " +
			"(INV-05 — the payload is a claim, the NetBox record is the fact), and the VM-placement topology " +
			"the blast radius reasons over.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_NETBOX_URL", Label: "NetBox base URL",
				Help: "Base URL of the NetBox API, e.g. https://netbox.example. Left empty, NEITHER the " +
					"entity resolver nor the placement edges are registered at all: claimed fields go " +
					"unreconciled and a blast radius over NetBox placement comes back empty rather than wrong.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "cascade_alert", EnvKey: "TG_NETBOX_CASCADE_ALERT", Label: "Expected cascade alert",
				Help: "Name of the alert a host→VM cascade is expected to fire (default \"HostDown\"). It is " +
					"stamped on every edge this source emits and becomes the rule the verifier expects on each " +
					"predicted VM. Use the name YOUR monitoring actually sends: a predicted host that alerts " +
					"under an unpredicted rule verifies PARTIAL, not match — so a wrong name here downgrades " +
					"cascades TG predicted correctly. A different spelling of the same fault is forgiven only " +
					"when it is a known family sibling of the observed rule.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				// This toggle arms modules/actorevidence/netbox, which reads THIS deployment through the URL
				// and token above and declares no SourceType of its own — so it can never appear as a module
				// dialog anywhere else. Declaring it here is what puts "who changed this device" within an
				// operator's reach instead of a redeploy's.
				Name: "actor_evidence", EnvKey: "TG_NETBOX_ACTOREVIDENCE", Label: "Read change author from NetBox",
				Help: "When on, TG reads NetBox object-changes to answer WHO caused a change to a NetBox " +
					"subject. Off (the default), those subjects are unattributable — attribution reports no " +
					"actor rather than guessing one. Requires the URL above; with it empty the reader stays " +
					"unregistered and the boot log says so.",
				Type: desc.TypeBool, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
			},
			{
				Name: "token_ref", EnvKey: "TG_NETBOX_TOKEN_REF", Label: "API-token reference",
				Help: "Where the NetBox API token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer. CHECK IT NAMES THIS MODULE'S LANE — the reader reads whatever " +
					"this points at, so while it still points somewhere else (the env: default, say) a token " +
					"saved below is stored in the lane and never read. It is a READ-TRIAGE reference — if the " +
					"identical reference is also configured for an actuation credential the worker refuses to " +
					"start (plane split).",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "NetBox API token",
				Help: "The token TG reads NetBox with. Give it READ-ONLY scope: this credential sits on the " +
					"triage plane and must never be able to write the CMDB. Write-only here — it is stored in " +
					"the secret backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path, because a
		// descriptor that could name one could point the console's writer at another module's secret.
		// TG_NETBOX_TOKEN_REF must point HERE for a Save to reach the reader — that pointer is read at boot
		// and nothing rewrites it at runtime, so it is a one-time change; every rotation after it is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("cmdb", SourceType), Field: "token"},
		Test:   desc.TestSpec{Verb: "read one page of the NetBox virtual-machine list", Mutating: false},
	}
}
