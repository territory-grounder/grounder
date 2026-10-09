<!-- docs/OPERATOR-QUICKSTART.md — provenance: [F] foundation (the shipped fail-closed defaults). -->

# Operator quickstart — from a fresh deploy to governed actuation

> **A fresh Territory Grounder actuates _nothing_.** It boots **Shadow** (read-only): it ingests, triages,
> reasons, and records rationale, but it does **not** touch your estate until you deliberately configure and
> escalate it. This guide is the exact, honest path from that zero-config default to governed auto-actuation
> of the conservative reversible op-class family — and the gates that still stand in the way by design.

There are **three independent things** you must supply. The out-of-box *config triad* grants **authorization**
(the op-classes become auto-*eligible*); it does **not** by itself make TG actuate. Actuation additionally
requires an **effect leaf** (where/how TG may act) and a **real incident** that clears every runtime gate.

---

## 0. The zero-config default (what you get with no setup)

| Axis | Default | Effect |
|------|---------|--------|
| Mode | **Shadow** (`policy_mode` absent → Shadow) | the mode chokepoint refuses every mutation |
| Effect leaf | **read-only reference** (no `TG_ACTUATION_SSH_HOST`) | there is no mutating effect leaf at all; it refuses to actuate |
| Policy ruleset | curated seed on a fresh DB: `restart-service`, `reload-service`, `restart-container` → `auto` | authorization only |
| Graduation | those 3 classes seeded to `LevelAuto` | authorization only |
| Allowlist | **empty** (`TG_ACTUATION_ALLOWED_UNITS`/`_CONTAINERS`) | every target is refused |

Corrupt/absent/partial state on any axis **fails closed to Shadow / refuse** — verified by the integration
audit. Nothing here actuates.

---

## 1. Authorization — the config triad (already seeded; one optional knob)

On a **fresh database** TG seeds the curated Semi-auto baseline automatically (absent-only; it never
overrides an operator's own config): the three reversible classes above become policy-`auto` and
graduation-`LevelAuto`. The only knob is the **initial mode**:

```bash
# Optional. Default (unset) = Shadow. Seeded ONCE on a fresh DB; a no-op on an existing estate.
TG_INITIAL_MODE=semi-auto        # shadow (default) | hitl | semi-auto | full-auto
```

Escalating into an actuating mode still requires the **green boot preflight** (the actuation chain must prove
itself wired) whether you set it here or flip it later via the RBAC-gated `POST /v1/mode`
(`TG_MODE_TRANSITION_OPERATORS`).

> After this step the three classes are **auto-eligible**. TG **still actuates nothing** — there is no effect
> leaf yet.

---

## 2. Actuation — configure the effect leaf (where + what TG may touch)

This is the operator's **scope + safety control**, deliberately separate from the triad. Without it,
`BuildEffectActuator` returns the read-only reference leaf and no mutation can execute.

```bash
TG_ACTUATION_SSH_HOST=host.example.net      # the target to act on (native Go crypto/ssh; NO subprocess)
TG_ACTUATION_SSH_IDENTITY=tg-actuator       # the scoped remote username (never a key)
TG_ACTUATION_SSH_KEY=file:/run/secrets/act  # key REFERENCE (env:NAME / file:/path) — never a literal
TG_ACTUATION_SSH_KNOWN_HOSTS=/path/known_hosts   # pinned host key; dial fails closed without it

# The allowlist IS the primary safety control. Allowlist ONLY non-stateful units/containers — the
# stateful-workload floor matches by NAME (postgres/redis/…), so a stateful app in a differently-named
# unit/container is NOT auto-floored; your allowlist is what keeps it out.
TG_ACTUATION_ALLOWED_UNITS=nginx;nginx.service
TG_ACTUATION_ALLOWED_CONTAINERS=              # e.g. app01;web02  (registry-gated; empty = none)
```

An **empty** allowlist means TG is auto-eligible with **zero resolvable targets** — it still actuates nothing.

---

## 3. What happens on the first real fault

With the triad + effect leaf configured and the mode escalated, on a monitored fault that classifies to one
of the three classes on an **allowlisted, non-stateful** target:

1. **First occurrence of each `(host, alert_rule)` → POLL_PAUSE** (a human approves once). This is the
   per-incident *novelty* gate — the earned-trust half, faithful to the predecessor. It is **not** a bug.
2. After a verified-clean resolution, the flywheel **de-novels** that `(host, rule)`; the **next** occurrence
   is eligible for **AUTO** — governed, zero human vote — and executes the fixed argv
   (`systemctl restart <unit>` / `systemctl reload <unit>` / `docker restart <container>`), then verifies.

So a fresh TG **earns** its autonomy per context; it does not act blindly on first sight.

---

## 4. What it will and will not do

- **Will auto-execute** (once earned): reversible `restart-service` / `reload-service` / `restart-container`
  against an operator-allowlisted, **non-stateful** unit/container, off the never-auto floor, past the novelty
  poll and risk band, while the mode is actuating and the preflight is green.
- **Never auto-executes** (non-configurable never-auto floor, triple-enforced): `reboot`/`shutdown`/`halt`,
  `mkfs`/`dropdb`/`zpool destroy`, `kubectl delete`/`drain`, `terraform destroy`, config-file overwrite,
  credential-revoke, a **stateful-workload** restart, a **self-protected control-plane** restart, or any
  unrecognized op. The floor is server-derived from the *actual* op — a plan cannot hide a mutation.

Every actuation traverses the interceptor + mode chokepoint (never a bypass); a verification deviation trips
the durable breaker and **force-Shadows** every worker. See [`CONSTITUTION.md`](CONSTITUTION.md) and
[`GOVERNED-BEHAVIORS.md`](GOVERNED-BEHAVIORS.md).
