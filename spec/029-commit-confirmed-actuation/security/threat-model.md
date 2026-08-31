# spec/029 threat model (DRAFT, TG-82)

| STRIDE | Threat | Mitigation | REQ |
|---|---|---|---|
| Tampering | The armed-revert record is deleted/edited so the dead-man's switch never fires | Durable single-writer state + the Temporal timer as the authority; ledger append at arm; a missing record at window end resolves as revert-failed → page + breaker | REQ-2901, REQ-2906 |
| Spoofing | A confirm arrives from something other than the mechanical verifier (model text, operator prose, alert silence) | The confirm signal is derivable ONLY from the terminus verify verdict; no API surface accepts an external confirm | REQ-2902 |
| Elevation | The inverse bypasses gates "because it is a rollback" | The inverse is a first-class mutation through the FULL interceptor chain — argv from the registry template, floors, mode chokepoint, ledger | REQ-2903 |
| DoS (self) | A fired revert during a monitoring outage re-mutates a healthy target | REQ-2902's marked draft position (revert-to-known-good as fail-safe); the HOLD+page alternative is the sign-off question and amends REQ-2902 if chosen | REQ-2902 |
| Repudiation | A change survives unconfirmed and nobody can say why | Armed/confirmed/fired/revert-outcome each ledger-append bound to the action_id; the console timeline renders the chain | REQ-2906 |
| Info disclosure | The commit_confirm row leaks secrets | The row carries op-class/action ids/timestamps only — never argv params or credentials (INV-13 shape) | REQ-2906 |
