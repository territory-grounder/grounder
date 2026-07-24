// The SERVER's LedgerVerifier walks the hash chain and returns its verdict. The console NEVER re-walks the
// chain or recomputes/compares hashes in the browser — it only MAPS the server's verdict to a display
// status (spec/010 T-010-5; the browser never enforces a safety decision, it reflects one). The verifier is
// delivered either as the structured result {verified} or as a bare status string — both are server-produced
// values, never something this module derives from the ledger entries.
export interface LedgerVerifierResult {
  verified: boolean;
  // Optional server-provided detail, shown as-is; it is never consulted to pick the verdict.
  checked_through_seq?: number;
  broken_at_seq?: number | null;
}

// What the server hands the console for the chain verdict: the structured result OR a bare status string.
export type LedgerVerification = LedgerVerifierResult | string;

export type ChainDisplayStatus = "verified" | "tampered" | "unknown";

export interface ChainStatusView {
  status: ChainDisplayStatus;
  tampered: boolean;
  label: string;
  detail: string | null;
}

// Recognised server status strings. This is a lookup of the server's own vocabulary, not a recomputation:
// the console does not decide verified/tampered, it only translates the word the server chose into a label.
const VERIFIED_STRINGS = new Set(["verified", "ok", "intact", "valid"]);
const TAMPERED_STRINGS = new Set([
  "tampered",
  "tamper-detected",
  "tamper_detected",
  "broken",
  "invalid",
  "failed",
]);

// Pure display mapping of a server verdict — no chain walking, hashing, or comparison happens here.
export function chainStatusFrom(result: LedgerVerification | null | undefined): ChainStatusView {
  if (result === null || result === undefined) {
    return { status: "unknown", tampered: false, label: "Chain status unavailable", detail: null };
  }

  if (typeof result === "string") {
    const key = result.trim().toLowerCase();
    if (VERIFIED_STRINGS.has(key)) {
      return { status: "verified", tampered: false, label: "Chain verified", detail: null };
    }
    if (TAMPERED_STRINGS.has(key)) {
      return { status: "tampered", tampered: true, label: "Tamper detected", detail: null };
    }
    return { status: "unknown", tampered: false, label: `Chain status: ${result}`, detail: null };
  }

  if (result.verified === true) {
    const detail =
      typeof result.checked_through_seq === "number"
        ? `Verified through seq ${result.checked_through_seq}`
        : null;
    return { status: "verified", tampered: false, label: "Chain verified", detail };
  }

  const detail =
    typeof result.broken_at_seq === "number" ? `Chain broken at seq ${result.broken_at_seq}` : null;
  return { status: "tampered", tampered: true, label: "Tamper detected", detail };
}
