# Territory Grounder — developer entrypoints.
.PHONY: build test bench vet lint deadcode release-gate migration-collision deploy-gate config-validate protected-paths protected-paths-test resume-budget boundary claims coldstart supply-chain spec check gen contracts contracts-check console-verify up down clean all eval-evidence eval-gate eval-gate-full eval-drift eval-holdout tier-ab tier-ab-preflight ledger estate-docs-corpus session-hygiene

all: vet lint protected-paths resume-budget boundary claims ci-shell session-hygiene eval-evidence supply-chain deadcode migration-collision deploy-gate config-validate spec contracts-check console-verify check test build ## run the full local gate

# NB: `all` does NOT run eval-gate — the LLM-judge eval needs the on-box model gateway (no Postgres/
# Temporal/model in CI). It DOES run eval-evidence (TG-237), which blocks an agent-behavior change that
# carries no on-box eval record and no named waiver — so the apparatus below now gates a merge instead of
# only reporting after one. eval-gate is the REQUIRED pre-merge step for prompt/skill/AGENT-BEHAVIOR
# changes ONLY; a purely deterministic change (Go/infra/docs/CI) is covered by `make all` and skips it (TG-117).
eval-evidence: ## the eval gate's BLOCKING limb (TG-237): an agent-behavior change must carry on-box eval evidence or a named waiver
	bash scripts/lint-eval-evidence_test.sh
	bash scripts/lint-eval-evidence.sh

ci-shell: ## every .gitlab-ci.yml script block must be parseable shell (a job that cannot parse fails on the runner and emails the owner), with its own drill
	bash scripts/lint-ci-shell_test.sh
	bash scripts/lint-ci-shell.sh

session-hygiene: ## claim-before-touch worktree-claim drill (TG-81): first-claim / sibling-refusal / release / stale-reclaim, all hermetic
	bash scripts/claim-worktree_test.sh

# The image-pin gate existed for weeks and `make all` never ran it — only CI did — so a developer could pin
# nothing locally and find out on the runner. It is here now, with its drill, because TG-283 showed the gate
# itself could be broken (keyword allowlist, vacuous PASS over zero files) while every pipeline stayed green.
# The cosign chain was retired by owner ruling 2026-08-10 (TG-417): the registry did not durably retain
# the signatures, so signing+verifying blocked every deploy. The orphaned scripts/cosign-* +
# scripts/check-deploy-host-cosign* family and deploy/cosign.pub were deleted (recover from git history if
# a signature-retaining registry ever arrives). supply-chain = the image-pin lint only.
supply-chain: ## supply-chain gate: third-party image pins, with its own drill (cosign retired, TG-417)
	bash scripts/lint-image-pins_test.sh
	bash scripts/lint-image-pins.sh

eval-gate: ## FAST pre-merge gate (on-box, TG-117): a corpus subset x1 run, candidate vs a FRESH origin/main base arm (drift-cancelled TG-64), ~10-15 min; non-zero on regression. Full rigor: make eval-gate-full
	bash eval/eval-gate.sh change

eval-gate-full: ## FULL-RIGOR pre-merge gate (on-box): the 3-run x full-corpus change gate (~1.5-2h) — for a high-risk agent-behavior change before merge
	TG_EVAL_FULL=1 bash eval/eval-gate.sh change

eval-drift: ## nightly TREND-WATCH (on-box): a clean main measurement vs the COMMITTED baseline, self-refreshing it; for the scheduled drift-watch, not a change gate
	bash eval/eval-gate.sh trend

release-gate: ## the Phase-4 §5 RELEASE gate (docs/TESTING-AND-BENCHMARK.md §5): ANDs the six release conditions over COMMITTED evidence. Runs no eval. exit 0 all-green / 1 any RED / 3 CANNOT CERTIFY. Deliberately NOT in `all` — it exits 3 until the eval-run + VISR + calibration evidence exists, and `make all` must stay a green developer gate
	bash scripts/release-gate_test.sh
	bash scripts/release-gate.sh

eval-holdout: ## sealed-holdout overfitting check (on-box): regression-vs-holdout gap, >20pt fails — docs/TESTING-AND-BENCHMARK.md §1.3
	bash eval/eval-gate.sh holdout

tier-ab: ## TG-204 three-arm model-tier A/B (on-box): does the reasoning tier buy diagnosis quality? Refuses to report a Δ unless the arms provably ran different models
	bash eval/tier-ab.sh

tier-ab-preflight: ## TG-204 arm-distinctness check ONLY (~4 completions, ~15s): CAN a three-arm tier A/B be run on today's config? Exits non-zero when the arms collapse onto one model
	TG_TIERAB_PREFLIGHT_ONLY=1 bash eval/tier-ab.sh

build: ## compile all packages + the grounder binary
	go build -o bin/grounder ./cmd/grounder
	go build ./...

estate-docs-corpus: ## TG-86 slice 1c: produce the PRIVATE on-box estate-doc grounding corpus, runner-side, with a fail-closed denylist no-leak gate. Usage: make estate-docs-corpus IAC_DIR=<docs-dir> OUT=<out.json> (OUT must be OUTSIDE the repo)
	bash scripts/estate-docs-corpus.sh "$(IAC_DIR)" "$(OUT)"

# `make all` USED TO REPORT GREEN WHILE 83 OF core/db's 125 TESTS NEVER RAN (TG-258).
#
# They gate on a Postgres DSN that only .gitlab-ci.yml's harness job sets, so on a developer box they all
# call t.Skip — and t.Skip is invisible here, because `go test` without -v folds a skip into the same "ok"
# it prints for a pass. Among the 83: the single-writer oracle that is the only executed proof of the
# [P1][integrity] fix TG-184. Every local "make all is green" on a DB-layer change was a statement about
# a third of that package.
#
# core/db now FAILS rather than skipping silently, because `go test` discards a passing package's output
# entirely without -v (measured on go1.25.12), leaving the exit status as the only channel that reaches
# anyone. TG_DB_TESTS_MAY_SKIP=1 is this target accepting that a developer box has no Postgres — and the
# line above it is the price of that acceptance: the banner is printed HERE, by make, where `go test`
# cannot swallow it, so `make all` states the count in its own output on every run. Do not add the
# variable without the notice; the acknowledgement is not a mute button.
#
# `-run '^$$'` selects no tests, so the notice costs one already-cached build and no test time. It cannot
# fail the target (`|| true`) because it is a report, not a gate — the gate is the `go test ./...` below,
# which reds if core/db's roster of DSN-gated oracles has been disconnected.
#
# To run them for real, mirror the harness job: a migrated database on TG_TEST_DSN and a second, EMPTY
# one on TG_TEST_POSTGRES_DSN (they are two fixtures, not two names for one). See core/db/dsn_gate_test.go.
test: ## run unit tests
	@TG_DB_TESTS_MAY_SKIP=1 go test ./core/db/ -run '^$$' -count=1 -v 2>&1 | grep '^!!' || true
	TG_DB_TESTS_MAY_SKIP=1 go test ./... -count=1
	bash scripts/verify-pipeline_test.sh

bench: ## run the deterministic perf benchmarks on-demand (TG-80; latency is machine-dependent, so NOT a CI gate)
	TG_GATE_BENCH=1 go test -run 'Throughput' -v -count=1 ./perf/
	go test -run '^$$' -bench=. -benchtime=2s ./perf/

loadharness: ## TG-80 P1-2: the real-run e2e platform harness against the in-process rig (1/4/8/16/32 sweep, p50/p95/max, exit non-zero on ANY failure). Against a live box: go run ./tools/loadharness/cmd/loadharness -base-url https://<box>/api ... (see tools/loadharness/README.md)
	go run ./tools/loadharness/cmd/loadharness -selftest -runs 8

vet: ## go vet
	go vet ./...

lint: ## the forbidden-pattern security gate (no shell / no string-SQL / migration pairs), with its own drill
	bash scripts/lint-forbidden_test.sh
	bash scripts/lint-forbidden.sh

deadcode: ## the TG-4 retired-but-present gate: dead code vs the one-way baseline ratchet (check), with its own drill; regen with scripts/deadcode-gate.sh write
	bash scripts/deadcode-gate_test.sh
	bash scripts/deadcode-gate.sh check

# A LOCAL GATE WEAKER THAN CI turns "make all is green" into a claim that does not survive push — the same
# reasoning the `spec` target above records for ratify/opcover. These three ran ONLY on the runner: their
# checks are pure local scripts with no infra, so there was never a reason for a developer to find out on CI.
# (deadcode was in the same state: a target nobody's `make all` invoked, while the deadcode-gate job voted.)
migration-collision: ## two migrations claiming the same number never both merge (CI parity), with its own drill
	bash scripts/lint-migration-collision_test.sh
	bash scripts/lint-migration-collision.sh

deploy-gate: ## the deploy-verification drill (CI parity): the deploy gate's own oracle
	bash scripts/verify-deploy_test.sh

config-validate: ## deployed-config validators (CI parity): the litellm model config, the monitoring alert guards, and the dashboards
	python3 deploy/ci/validate-litellm-config.py
	python3 deploy/monitoring/ci/validate-alert-guards.py
	python3 deploy/monitoring/ci/validate-dashboards.py

protected-paths: ## the law-surface gate (TG-185): a change to the constitution/safety-core/ADRs/spec/CI needs an owner approval trailer
	bash scripts/lint-protected-paths.sh

protected-paths-test: ## unit test for the protected-path gate (deterministic; no git state needed)
	bash scripts/lint-protected-paths_test.sh

resume-budget: ## the orientation-budget gate (TG-428): the resume path must fit the DoD's 10k-token budget and carry no retired claims, with its own drill
	bash scripts/lint-resume-budget_test.sh
	bash scripts/lint-resume-budget.sh

boundary: ## the autonomy-boundary gate (TG-488): every board owner-list entry cites its reserved clause [R1]..[R7], with its own drill
	bash scripts/lint-autonomy-boundary_test.sh
	bash scripts/lint-autonomy-boundary.sh

claims: ## the parallel-session claim gate (TG-488/TG-81a): a worktree branch must hold its claim file, with its own drill
	bash scripts/lint-claim-before-touch_test.sh
	bash scripts/lint-claim-before-touch.sh

# The parent-dir resume kit (router CLAUDE.md + .claude/agents symlink) is machine-local and
# uncommitted (public mirror; estate coordinates) — this drill+gate is its only falsifiable spec.
coldstart: ## BOX-LOCAL cold-start acceptance (TG-428): drill the drill, then gate the real parent kit. The kit does not exist on CI runners, so this is deliberately NOT in 'all:'
	bash scripts/coldstart-drill_test.sh
	bash scripts/coldstart-drill.sh
	bash scripts/check-nightly-drift_test.sh
	bash scripts/check-nightly-drift.sh

spec: ## validate the executable spec lattice + lockstep + ratify + opcover (EARS/traceability/DAG/drift)
	go run ./tools/specvalidate
	go run ./tools/specvalidate lockstep --check
	# ratify and opcover run in CI's `harness` job. They belong here too: a local gate WEAKER than CI turns
	# "make all is green" into a claim that does not survive push, which is how a missing task `status` — the
	# exact closed-enumeration check ratify exists to make — reached a pipeline instead of the working tree.
	go run ./tools/specvalidate ratify --check
	go run ./tools/specvalidate opcover --check
	go run ./tools/specvalidate tally --check

# CI runs this as the `preflight-smoke` job. It needs no infra (it fails closed to POLL_PAUSE off-box), so
# there was never a reason for a developer to learn on the runner that the composition root stopped booting.
check: ## run the boot preflight (no infra needed; CI parity with the preflight-smoke job)
	go run ./cmd/grounder --check

# Not in `all`: it needs live YouTrack creds (YOUTRACK_URL/YOUTRACK_TOKEN, or YT_URL/YT_TOKEN), and
# without them it is LEDGER BLIND with exit 3 — a measurement never fail-safes to green (TG-428).
ledger: ## the Definition-of-done v1.1 progress meter (TG-428); needs YouTrack creds, BLIND without them
	go run ./tools/tgledger

gen: contracts ## regenerate all generated artifacts (sqlc + wire contracts)
	@command -v sqlc >/dev/null 2>&1 && sqlc generate || echo "sqlc not installed; skipping (CI runs it)"

contracts: ## regenerate docs/contracts (openapi/asyncapi/JSON-schema) from the canonical model (INV-15)
	go run ./tools/gencontracts/cmd/gencontracts

contracts-check: ## fail if the committed wire contracts drifted from the served surface (INV-15)
	go run ./tools/gencontracts/cmd/gencontracts -check

console-verify: ## fail if the DEPLOYED deploy/console/v2/index.html drifted from its assemble.py source (#55)
	python3 deploy/console/v2/assemble.py --check

up: ## bring up the single-node compose stack (needs deploy/.env)
	cd deploy && docker compose up -d --build

down: ## tear down the compose stack
	cd deploy && docker compose down

clean:
	rm -rf bin
