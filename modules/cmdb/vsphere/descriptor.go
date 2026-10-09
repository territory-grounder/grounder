package vsphere

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the vSphere topology reader's configuration schema so the console GENERATES its
// dialog rather than hand-rendering one that drifts from the binary.
//
// EFFECT PER FIELD, HONESTLY. The URL, username and TLS flag are read ONCE at boot (vsphereEstateSource in
// cmd/worker/vsphere_wiring.go) and captured in the constructed EstateSource, so a save is durable but INERT
// until the worker restarts — the periodic estate refresh re-runs Edges() against the SAME object and never
// re-reads them. The password is different: Edges() resolves the secret reference on every refresh, so a
// rotation written to the lane below is picked up by the next read with no restart.
//
// This is the READ-ONLY estate reader — nothing configured here can start, stop or reconfigure a VM, and the
// credential this dialog writes belongs on the read-triage plane (boot fails closed if it is shared with an
// actuation reference).
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "cmdb",
		SourceType: SourceType,
		Title:      "VMware vSphere / vCenter (estate topology)",
		Summary: "Reads VM placement from a vCenter Server via the govmomi SDK and contributes `runs_on` " +
			"edges — each VM depends on the physical ESXi host it runs on — at the 0.94 live-hypervisor " +
			"confidence tier, alongside Proxmox.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_VSPHERE_URL", Label: "vCenter base URL",
				Help: "Base URL of the vCenter Server, e.g. https://vcenter.example.com — govmomi appends the " +
					"/sdk path. Left empty, the vSphere source is not registered at all: no VM→host edge reaches " +
					"the graph, and a failing ESXi host's blast radius reaches none of the VMs placed on it.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "username", EnvKey: "TG_VSPHERE_USER", Label: "vCenter login username",
				Help: "The vCenter SSO username TG logs in as, e.g. svc-tg@vsphere.local. Give it a READ-ONLY " +
					"role (the built-in Read-only role, or a custom role with just System.View + VM read): this " +
					"identity sits on the triage plane and must never be able to reconfigure inventory. The " +
					"password is set below, not here.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				// AUTHORITY, not an ordinary toggle. Switching this on means TG accepts ANY certificate for the
				// vCenter endpoint — it decides WHAT is trusted to answer as the hypervisor, so it moves a trust
				// boundary and must be visibly marked, not flipped in a debugging session and never flipped back.
				Name: "insecure_tls", EnvKey: "TG_VSPHERE_INSECURE", Label: "Skip TLS verification",
				Help: "Accept the vCenter endpoint's certificate WITHOUT verifying it (many vCenters serve a " +
					"self-signed cert). This disables endpoint authentication for the estate reader — leave it " +
					"OFF unless you have deliberately accepted a self-signed cert; prefer installing the CA.",
				Type: desc.TypeBool, Security: desc.SecAuthority, Effect: desc.EffectRestart,
			},
			{
				Name: "token_ref", EnvKey: "TG_VSPHERE_TOKEN_REF", Label: "Password reference",
				Help: "Where the vCenter password is read from. Displayed for provenance: set the password " +
					"itself below, not this pointer. CHECK IT NAMES THIS MODULE'S LANE — the reader reads " +
					"whatever this points at, so while it still points somewhere else a password saved below is " +
					"stored in the lane and never read. It is a READ-TRIAGE reference — if the identical " +
					"reference is also configured for an actuation credential the worker refuses to start " +
					"(plane split).",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "vCenter password",
				Help: "The password for the vCenter login username above, resolved per refresh and sent over the " +
					"SOAP session login. Give the account READ-ONLY scope — a credential that could reconfigure " +
					"VMs would put an actuation credential on the triage plane. Write-only here — stored in the " +
					"secret backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared (desc.Validate refuses a module that names its own secret path).
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("cmdb", SourceType), Field: "token"},
		Test: desc.TestSpec{
			Verb:     "log in to vCenter and list its VMs and the ESXi host each one runs on",
			Mutating: false,
		},
	}
}
