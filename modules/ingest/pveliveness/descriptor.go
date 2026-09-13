package pveliveness

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the PVE liveness detector's configuration schema so the console can GENERATE its
// dialog rather than hand-render one that drifts from the binary.
//
// Effect is restart for every field, and that is a fact about the wiring, not caution. cmd/worker/main.go
// reads all six at boot — the interval as the condition of the `if TG_PVE_LIVENESS_POLL_INTERVAL != ""`
// block, the other five inside it (through resolvePVELivenessPair / resolvePVELivenessGuests, which pick a
// primary or a fallback key per field) — and passes them to pveliveness.New, where base URL, token reference,
// guest allowlist and site label are captured on the Source at construction and the polling goroutine closes
// over it. Nothing re-reads them. A dialog claiming otherwise would produce a Save that reports success it
// did not achieve. (The token REFERENCE is re-resolved per poll, but the pointer itself is fixed at boot —
// which is why it is display-only below, not a live field.)
//
// THESE KEYS ARE THE DETECTOR'S OWN, AND THAT IS THE TG-350 CHANGE. Until 2026-08-06 the dialog described
// the ACTUATION lane's keys (TG_PROXMOX_*), because the poller read them — its endpoint, its guest-lifecycle
// WRITE token and its allowlist. That could not survive the credential plane split: this detector is
// triage-scoped and the write token is withheld from the triage plane, so it lost its credential on
// 2026-07-31 and delivered nothing for six days. It now reads with the estate READ pair, which is both the
// correct credential for a GET and the one a triage worker is allowed to hold, and falls back to the
// TG_PROXMOX_* pair on a `both`-plane worker. The fields below name the PRIMARY key; each help text names
// the fallback, because an operator on an unsplit install is still configuring the old one.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "ingest",
		SourceType: SourceType,
		Title:      "Proxmox guest liveness",
		Summary: "TG's own fastest detector: a read-only poll of Proxmox guest status that mints one " +
			"triage on each running→stopped transition of an allowlisted guest, ~85s instead of the " +
			"6–11 minutes a LibreNMS device-down push takes. It never actuates.",
		Fields: []desc.Field{
			{
				// The master switch. Unset is not "default interval" — it is the detector not existing,
				// and detection falls back entirely to the slow push path.
				Name: "poll_interval", EnvKey: "TG_PVE_LIVENESS_POLL_INTERVAL", Label: "Poll interval",
				Help: "How often to read guest status, e.g. 30s. EMPTY MEANS THE DETECTOR IS OFF — guest " +
					"faults are then only found by the LibreNMS device-down push, 6–11 minutes later, and " +
					"short faults that self-restore are never seen at all. Also the per-poll timeout, so " +
					"do not set it shorter than the Proxmox API answers in.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 32,
			},
			{
				Name: "base_url", EnvKey: "TG_PROXMOX_BASE_URL", Label: "Proxmox base URL",
				Help: "Base URL of the Proxmox API, e.g. https://pve.example:8006. SHARED WITH THE " +
					"ACTUATION LANE — changing it here repoints guest healing too. Unset (or with the " +
					"token reference unset) the poller stays idle, and the boot log names the PAIR " +
					"rather than which of the two is missing — check both.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// AUTHORITY: this is the set of guests TG treats as its own. Empty fires for NOTHING
				// (fail-safe), and every name added is a machine whose going-down now opens an
				// investigation — and, on the actuation lane that reads the SAME list, a machine TG may
				// start. That is a trust boundary, not a filter, and must not render as a text box.
				Name: "allowed_guests", EnvKey: "TG_PROXMOX_ALLOWED_GUESTS", Label: "Managed guests",
				Help: "Guest NAMES (not vmids) TG watches — anything else going down is ignored, which is " +
					"how infra guests stay out of TG's scope. EMPTY WATCHES NOTHING. The same list is what " +
					"the actuation lane may start, so a name added here is a guest TG may act on.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Required: true, Pattern: `^[A-Za-z0-9._-]+$`, MaxItems: 256, MaxLen: 128,
			},
			{
				Name: "site", EnvKey: "TG_PVE_LIVENESS_SITE", Label: "Site label",
				Help: "Estate label stamped on every incident this detector raises (e.g. nl). Leave it " +
					"blank and liveness incidents arrive site-less while the LibreNMS ones do not, so " +
					"per-site views under-count exactly the fastest intake. It is a descriptive label, " +
					"never a security boundary.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 64,
			},
			{
				// AUTHORITY for the same reason as LibreNMS's: this does not weaken the connection, it
				// removes the only check that the endpoint is the Proxmox it claims to be — and the header
				// TG sends it is the guest-lifecycle WRITE token.
				Name: "insecure", EnvKey: "TG_PROXMOX_INSECURE", Label: "Skip TLS verification",
				Help: "Accept the Proxmox certificate without verifying it. With it on, whatever answers " +
					"at the base URL receives TG's Proxmox write token. SHARED WITH THE ACTUATION LANE; TG " +
					"logs loudly if this and TG_PVE_INSECURE disagree rather than picking one for you. " +
					"BEFORE SETTING THIS, check the ADDRESS. This help text used to call :8006 a " +
					"self-signed endpoint by default, and on at least one deployment that was wrong: the " +
					"node served a publicly-trusted wildcard certificate that verifies under its FQDN and " +
					"cannot match a bare short hostname, so the flag was working around a URL rather than " +
					"a certificate, for the lifetime of that deployment (TG-367). A hostname mismatch and " +
					"an untrusted issuer need opposite fixes. TG now probes every skipped endpoint at boot " +
					"and logs whether the skip is NECESSARY.",
				Type: desc.TypeBool, Security: desc.SecAuthority, Effect: desc.EffectRestart,
			},
			{
				// DISPLAY ONLY, and there is deliberately NO secret VALUE field beside it. This pointer is
				// the Proxmox connector's guest-lifecycle WRITE credential, shared with the actuation lane;
				// this detector only borrows it for a GET. If this dialog offered a token box it would have
				// to write to the ingest/pve-liveness lane, and the one shared pointer cannot aim at two
				// modules' lanes at once — so one of the two dialogs would be writing a credential nothing
				// reads, reporting success and changing nothing. The credential is set where it is owned.
				Name: "token_ref", EnvKey: "TG_PROXMOX_TOKEN_REF", Label: "API-token reference",
				Help: "Where the Proxmox API token is read from. Displayed for provenance only: this is " +
					"the Proxmox connector's guest-lifecycle credential, which this read-only detector " +
					"borrows — set or rotate it there, not here.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
		},
		// No Secret lane: this module owns no credential of its own (see token_ref above).
		Test: desc.TestSpec{Verb: "read the Proxmox guest list once (GET /api2/json/cluster/resources) and report how many allowlisted guests it matched", Mutating: false},
	}
}
