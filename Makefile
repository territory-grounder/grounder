# Territory Grounder — developer entrypoints.
.PHONY: build test vet lint spec check gen contracts contracts-check console-verify up down clean all eval-gate eval-gate-full eval-drift eval-holdout

all: vet lint spec contracts-check console-verify test build ## run the full local gate

# NB: `all` deliberately does NOT run eval-gate — the LLM-judge eval needs the on-box model gateway (no
# Postgres/Temporal/model in CI). eval-gate is the REQUIRED pre-merge step for prompt/skill/AGENT-BEHAVIOR
# changes ONLY; a purely deterministic change (Go/infra/docs/CI) is covered by `make all` and skips it (TG-117).
eval-gate: ## FAST pre-merge gate (on-box, TG-117): a corpus subset x1 run, candidate vs a FRESH origin/main base arm (drift-cancelled TG-64), ~10-15 min; non-zero on regression. Full rigor: make eval-gate-full
	bash eval/eval-gate.sh change

eval-gate-full: ## FULL-RIGOR pre-merge gate (on-box): the 3-run x full-corpus change gate (~1.5-2h) — for a high-risk agent-behavior change before merge
	TG_EVAL_FULL=1 bash eval/eval-gate.sh change

eval-drift: ## nightly TREND-WATCH (on-box): a clean main measurement vs the COMMITTED baseline, self-refreshing it; for the scheduled drift-watch, not a change gate
	bash eval/eval-gate.sh trend

eval-holdout: ## sealed-holdout overfitting check (on-box): regression-vs-holdout gap, >20pt fails — docs/TESTING-AND-BENCHMARK.md §1.3
	bash eval/eval-gate.sh holdout

build: ## compile all packages + the grounder binary
	go build -o bin/grounder ./cmd/grounder
	go build ./...

test: ## run unit tests
	go test ./... -count=1

vet: ## go vet
	go vet ./...

lint: ## the forbidden-pattern security gate (no shell / no string-SQL / migration pairs)
	bash scripts/lint-forbidden.sh

spec: ## validate the executable spec lattice + spec<->code lockstep (EARS/traceability/DAG/drift)
	go run ./tools/specvalidate
	go run ./tools/specvalidate lockstep --check

check: ## run the boot preflight (no infra needed)
	go run ./cmd/grounder --check

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
