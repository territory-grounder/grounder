# spec/030 threat model (DRAFT, TG-58)

| STRIDE | Threat | Mitigation | REQ |
|---|---|---|---|
| Elevation | The model smuggles extra steps into a plan | Plans exist only in the compiled recipe registry; the model's single action can at most SELECT one by op-class; no model token names, orders or extends steps | REQ-3001 |
| Elevation | A plan smuggles an op-class past the graduation ladder | The plan lane refuses unless every step's op-class is individually admissible; per-step classification + every interceptor gate still run | REQ-3003, REQ-3007 |
| Tampering | The executed plan differs from the approved one | The vote binds a content-addressed plan_id over the ordered step tuples; the workflow re-derives and refuses on mismatch — the INV-07 argument one level up | REQ-3002 |
| Spoofing | A "compensation" that is really a new mutation slips past approval | Compensations are registry rollback templates presented IN the plan poll; approving the plan is approving them; each still traverses the full chain | REQ-3002, REQ-3004 |
| Repudiation | Nobody can reconstruct what a plan did to which machines | Every transition ledger-appended with plan_id AND step action_id; per-step manifests keep INV-07 identity | REQ-3006 |
| DoS (self) | A failing step loops compensation against a broken estate | Compensations run once, in reverse order; a compensation failure STOPS, pages, and trips the mutation breaker — no retry storm | REQ-3005 |
| Info disclosure | The plan poll leaks more than the single-action poll | The poll renders the same fields per step the single-action poll renders today (op/class/target/reversibility); no new content class crosses | REQ-3002 |
| DoS | Half-reverted estate presented as clean | The revert-failed terminal records exactly which steps remain applied; the session record carries it; autonomy is tripped until a human clears | REQ-3005, REQ-3006 |
