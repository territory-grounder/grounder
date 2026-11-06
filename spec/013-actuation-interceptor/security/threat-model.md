<!-- spec/013 — per-feature STRIDE slice. Provenance: [F]/[R]/[O] as elsewhere. -->

# spec/013 — Threat model: actuation interceptor + mutation gate

STRIDE slice for the mutation keystone. The controlling invariants are INV-21/S8-5 (wired-by-construction,
fail loud, no dark control), INV-09 (mutation off + never-auto floor), INV-07/11 (action identity +
evidence), and INV-19 (audit).

| Threat | Vector | Control | Ref |
|---|---|---|---|
| **Elevation (bypass)** | A caller executes a mutation without traversing the gate | The actuator is an UNEXPORTED field; the only path to Execute is Interceptor.Do, so there is no reachable bypass of admission→floor→gate→evidence→execute | REQ-1201, INV-21/S8-5 |
| **Elevation (dark control)** | A collaborator is left unwired so a check silently no-ops | SelfTest and Do fail LOUD (refuse/error) on any nil collaborator; EnableMutation refuses unless SelfTest passes, so mutation cannot turn on onto an unwired base | REQ-1202/1206, INV-21/S8-5 |
| **Tampering (floor)** | An irreversible/destructive op is executed under a flag | The mechanical never-auto floor is enforced at the adapter as defense in depth; a floor or irreversible op is refused even with mutation on, and no flag lifts it | REQ-1203, INV-09 |
| **Tampering (action substitution)** | A different action is executed than the one predicted/approved | The interceptor asserts the sealed manifest action_id and refuses an ungated action; identity, not existence, is gated | REQ-1204, INV-06/07 |
| **Spoofing (evidence)** | A mutation proceeds on the agent's asserted success | A mutating action must cite a bound orchestrator-captured tool-result; agent free-text is not admissible | REQ-1205, INV-11 |
| **Repudiation** | An execution or refusal cannot be reconstructed | Every execute AND every refuse appends a governed decision to the tamper-evident hash-chained ledger | REQ-1207, INV-19 |
| **Elevation (self-verdict)** | The acting model marks its own outcome a success | The deterministic verifier writes the only verdict post-execution; the model has no write path | REQ-1207, INV-10 |
| **Elevation (unapproved actuation)** | An `approve`-verdict action (which routes to a human vote) executes with no recorded vote, or an approval is used to lift a `deny` | Step 4d honors the policy verdict by its REQ-1506 meaning: `deny` refuses UNCONDITIONALLY (no approval lifts it); `approve` proceeds ONLY with a recorded human approval (`Request.Approved`, INV-12) and otherwise refuses (a second floor beneath admission 1b); the never-auto floor (step 2) still refuses any irreversible/destructive op BEFORE 4d and the mode chokepoint gates beneath — so honoring a recorded approval opens no floor bypass. Honoring the vote is what lets an ungraduated op-class earn its verified-clean runs toward `auto` (spec/015 REQ-1514) instead of dead-locking | REQ-1506, INV-12/09 |

## Cost/budget spend guard (the $-ceiling breaker, REQ-1211..1215)

| STRIDE | Threat | Mitigation | Refs |
|---|---|---|---|
| **Denial of Wallet** | Runaway/looping agents or a prompt-injection amplification burn unbounded LLM spend | The cost breaker accrues approximate USD per completion into a durable, cross-process daily+session accumulator and force-Shadows on a daily-budget/session-ceiling trip; the trip is shared, so one worker's breach halts every sibling on its next spend | REQ-1211/1212/1213, INV-09 |
| **Denial of Service (self-inflicted)** | A fail-CLOSED cost breaker turns a metrics/DB blip into a global halt | Deliberately FAILS OPEN: an unreadable cost store is treated as NOT tripped and LOGGED loudly, never force-Shadows on a read error — a spend guard is not a safety floor, so a cost-store outage must degrade to no-enforcement, never a halt (the inverse of the mutation breaker's fail-closed, REQ-1210) | REQ-1215, INV-15 |
| **Tampering (safety weakening)** | The spend guard is mistaken for, or dilutes, the safety controls | The cost breaker is INDEPENDENT: it never enables actuation, never touches the never-auto floor / mutation breaker / chokepoint semantics, and does not route through Execute — it only ADDS a Shadow-drop; the mutation keystone is unchanged | REQ-1212, INV-09/INV-21 |
| **Repudiation** | A budget halt cannot be reconstructed | Every trip appends a `cost:breaker-trip` decision to the tamper-evident hash-chained ledger, distinct from `safety:breaker-trip`, so a spend halt is auditable apart from a safety halt | REQ-1212, INV-19 |
| **Information disclosure** | Spend rows leak argv/host/secret material | `cost_accrual`/`cost_breaker_state` (0023) store only a bucket key (a UTC date or an external_ref slug), USD amounts, a state, and a human reason — no argv/host/credential column; the token count is the same approximation core/observe already emits, never the content | REQ-1211, INV-13 |

## Adversarial acceptance

The oracle drives the real Interceptor: mutation-off refuses; an unwired chain fails loud; a floor op is
refused with mutation on; ungated / action_id-mismatch / evidence-unbound are refused; mutation enables
only through the proven wired gate; and a fully-admissible request executes exactly once, verifies, and
audits. For the policy-authorize step (spec/015 REQ-1506) it drives the real chain with a scripted decider: an
`approve` verdict WITH a recorded human approval executes (the graduation-earning path), an `approve` with NO
recorded approval refuses, a `deny` refuses even when approved, and the never-auto floor still refuses an
irreversible op even when approved and policy-`auto`. No refusal path reaches Execute (asserted on a recording actuator). It also drives the real
`core/cost.Accountant`: cost accrues into the day+session accumulators; exceeding the daily budget or a
session ceiling trips and force-Shadows and records a `cost:breaker-trip` ledger note; a shared trip
force-Shadows a sibling; a 0 budget never enforces; and an unreadable store fails OPEN (no halt) and logs.
No source-string assertion is used — the actual code path runs (INV-22).
