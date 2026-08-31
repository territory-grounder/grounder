//! tg-claude-proxy — OpenAI-compatible completion proxy over the Claude Code CLI,
//! authenticated via the operator's Max subscription (NOT an API key).
//!
//! FORKED 2026-07-31 from omoikane.coach/daemon/claudecode-runner (Rust axum harness
//! around `claude -p`, subscription OAuth, rotation, probe-auth) with one addition:
//! a `/v1/chat/completions` + `/v1/models` OpenAI surface so LiteLLM can route to it
//! as a provider — which lets Territory Grounder's agent loop run Claude Opus 5 for
//! the model-identical head-to-head (TG <-> LiteLLM <-> this sidecar <-> Opus 5).
//!
//! The two subscription-path lessons inherited from the omoikane fork (hard-won):
//!   1. NO `--bare` flag — `--bare` disables the OAuth/keychain read the
//!      subscription path REQUIRES (~/.claude/.credentials.json).
//!   2. ANTHROPIC_API_KEY must be ABSENT on the container — if set, the CLI
//!      prefers it and bills per-token instead of the subscription. We also blank
//!      it explicitly on every spawn (belt-and-braces; setup_token.rs always did).
//!
//! COMPLETION PURITY: the OpenAI path is a PURE single-shot completion — the CLI is
//! invoked with the built-in tool set disabled and MCP pinned to the (empty) CLI
//! config. TG's own Go ReAct loop must remain the only agent; the sidecar's brain
//! answers, it never acts. (`/run` keeps the original omoikane semantics.)
//!
//! AGRIOPS-208 (2026-08-03) — latency + safety rework. Measured on this host:
//!   * Fix 1  — break the stdout read loop the instant the `type=="result"`
//!     envelope lands and SIGKILL the child instead of `child.wait()`. The CLI
//!     holds stdout open ~0.40s after the result to flush its telemetry; that
//!     wait was pure dead time on the voice path.
//!   * Fix 2  — use-once warm pool (`SIDECAR_POOL_SIZE`, 0 disables). CLI init is
//!     EAGER: with `--input-format stream-json` and an open-but-empty stdin the CLI
//!     does git detect / ripgrep scan / IDE detection / TLS handshake with ZERO
//!     input, then waits. Warm write->init measured 0.055s vs 1.050s cold, with
//!     IDENTICAL prompt tokens. A warm process serving a SECOND request was proven
//!     to leak the FIRST request's content (same session_id), so the pool contract
//!     is strictly USE-ONCE: checkout removes, serve one turn, kill, replace.
//!   * Fix 3  — the user prompt moves OFF argv onto stdin as stream-json NDJSON.
//!     Coupled to fix 1: with stdin as a pipe the CLI would otherwise wait for the
//!     next user message forever.
//!   * Fix 4  — `--tools ""` (188 prompt tokens) replaces the stale hardcoded
//!     `--disallowedTools` list (~14249 prompt tokens, 98.7% built-in tool schemas).
//!     `--tools` is VARIADIC, so it must never be the last argv element; the
//!     `--strict-mcp-config` that always follows it is what keeps that safe.
//!   * Fix 5  — `--exclude-dynamic-system-prompt-sections` dropped (the CLI's own
//!     help says it is ignored whenever `--system-prompt` is set, which is always).
//!     `--strict-mcp-config` is KEPT: `--tools ""` disables only the BUILT-IN set,
//!     so a stray `.mcp.json` in the writable bind-mounted HOME would otherwise
//!     re-arm an agent-in-agent inside the sidecar. It costs zero tokens.
//!
//! PROCESS HYGIENE NOTE (compose-side, not fixable here): because the success path
//! no longer reaps via `child.wait()`, grandchildren the CLI forks at init (git,
//! ripgrep) reparent to PID 1. PID 1 in this container is this binary, and tokio
//! only ever `waitpid`s pids it knows — so they would become permanent zombies.
//! The deployment MUST set `init: true` (tini as PID 1) in compose.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use axum::{
    extract::State,
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    routing::{get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStderr, ChildStdin, ChildStdout, Command};
use tokio::sync::{Mutex, OwnedSemaphorePermit, RwLock, Semaphore};
use tokio::time::timeout;
use tracing::{error, info, warn};

mod oauth_rotate;
mod setup_token;

/// LEGACY fallback for `SIDECAR_TOOLS_MODE=disallowed-list` when
/// `SIDECAR_DISALLOWED_TOOLS` is unset. Deliberately NOT maintained: this list
/// rotted once already (it predates several built-in tools, and the container's
/// own `sdk-tools.d.ts` shows the schema names have drifted from the CLI tool
/// names — `FileEditInput` vs `Edit`, `AgentInput` vs `Task`). A hardcoded list
/// is the bug, not the fix. Supply the live list via the env var; the default
/// mode (`none` -> `--tools ""`) needs no list at all.
const LEGACY_DISALLOWED_TOOLS: &str = "Bash,Edit,Write,Read,Glob,Grep,WebFetch,WebSearch,Task,NotebookEdit,TodoWrite,BashOutput,KillShell,EnterPlanMode,ExitPlanMode";

/// Cap on the per-child stderr ring buffer. stderr is drained CONCURRENTLY into
/// this ring (previously it was read only after exit, so a child writing more
/// than the 64 KiB pipe buffer blocked forever).
const STDERR_RING_CAP: usize = 8 * 1024;

/// How long a top-up child must survive to be considered healthy. A child that
/// dies faster than this means a broken credential / broken image, and spawning
/// two processes per request forever makes that worse, not better.
const SPAWN_STORM_PROBE: Duration = Duration::from_secs(5);
/// Backoff applied after a spawn-storm probe trips.
const SPAWN_STORM_BACKOFF: Duration = Duration::from_secs(30);
/// Grace given to a FAILED child so its exit status (and therefore the
/// `claude exit=exit status: 1` runbook diagnostic) survives. The SUCCESS path
/// never waits.
const FAILED_CHILD_WAIT: Duration = Duration::from_secs(2);
/// Default patience for an in-flight slot before we shed load, overridable with
/// `SIDECAR_QUEUE_WAIT_MS`.
///
/// ★ THIS WAS 5 SECONDS, AND 5 SECONDS IS SHORTER THAN ONE COMPLETION.
/// Measured on the live sidecar 2026-08-06: `sidecar_duration_ms_sum / completions`
/// = **9.0 s mean** across 2371 completions, with `SIDECAR_MAX_INFLIGHT=4`. A caller
/// that finds all four slots busy therefore waits, on average, longer than the
/// patience it is given — so the queue could essentially never succeed under
/// contention. It was a load-shed gate wearing a queue's name, and it shed 334
/// requests in 24 h. Downstream that surfaces as `503 sidecar at capacity`, which
/// LiteLLM cannot mask (no fallback absorbs it) and TG's model breaker treats as a
/// failure — the eval change gate lost 4 of 8 sessions to it on the first arm and
/// 1 of 8 on the second, and the `judge-death` breaker has been latched OPEN
/// against a brain that was answering fine, just refusing to wait (TG-357).
///
/// The semaphore's job is bounding MEMORY — every spawn is ~245 MiB RSS, and that
/// bound is unchanged. Patience is a separate knob and it belongs on the caller's
/// side of the trade: TG's gateway, its per-call timeouts and its breaker already
/// decide how long a triage may take. Shedding at 5 s did not protect the box; it
/// converted a slow answer into a failed one.
const DEFAULT_INFLIGHT_QUEUE_WAIT_MS: u64 = 120_000;

/// Monotonic worker id, unique for the process lifetime. Logged as
/// `pool_worker_id`; the pool's use-once invariant is stated in terms of it.
static WORKER_SEQ: AtomicU64 = AtomicU64::new(1);
/// Monotonic request counter backing the OpenAI response id. The old
/// `chatcmpl-tg-{created}-{elapsed_seconds}` collided for any two requests that
/// landed in the same second with the same whole-second duration.
static RESPONSE_SEQ: AtomicU64 = AtomicU64::new(1);
/// Correlation id counter. Before the result envelope exists there is no
/// session_id to correlate on, and concurrent requests were previously
/// completely unattributable — the timeout / non-zero-exit / spawn-failure
/// warnings carried no identifying field at all.
static REQUEST_SEQ: AtomicU64 = AtomicU64::new(1);

fn next_request_id() -> String {
    format!("req-{:06x}", REQUEST_SEQ.fetch_add(1, Ordering::Relaxed))
}

// ---------------------------------------------------------------------------
// Metrics — hand-rolled Prometheus text format, no new dependency.
//
// Nothing scraped this service before AGRIOPS-208, while it is the single most
// expensive component on a host that already runs Prometheus + Grafana. Costs
// in particular were recorded NOWHERE: `total_cost_usd` arrives on every result
// envelope and was discarded, so "what did the advisor cost last week" was
// unanswerable.
// ---------------------------------------------------------------------------

/// Per-model spend. The CLI bills a hidden `claude-haiku-4-5` helper on top of
/// the model actually asked for; without this breakdown that spend is invisible.
#[derive(Default, Clone)]
struct ModelAgg {
    cost_nano_usd: u64,
    input_tokens: u64,
    output_tokens: u64,
    cache_read_tokens: u64,
    cache_creation_tokens: u64,
}

#[derive(Default)]
pub struct Metrics {
    completions_ok: AtomicU64,
    completions_error: AtomicU64,
    completions_timeout: AtomicU64,
    /// Nano-dollars (1e-9 USD) so cents-scale calls accumulate without float drift.
    cost_nano_usd: AtomicU64,
    prompt_tokens: AtomicU64,
    completion_tokens: AtomicU64,
    pool_hits: AtomicU64,
    pool_misses: AtomicU64,
    pool_spawns: AtomicU64,
    /// Warm workers killed by a drain (rotation / warm-failure / shutdown).
    pool_drains: AtomicU64,
    /// Requests shed because no in-flight slot came free within the queue patience.
    shed: AtomicU64,
    duration_ms_sum: AtomicU64,
    duration_count: AtomicU64,
    overhead_ms_sum: AtomicU64,
    overhead_count: AtomicU64,
    rate_limit_events: StdMutex<HashMap<String, u64>>,
    model_usage: StdMutex<HashMap<String, ModelAgg>>,
}

/// Prometheus label-value escaping: backslash, double-quote, newline.
fn esc(v: &str) -> String {
    v.replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

fn usd_to_nano(usd: f64) -> u64 {
    if usd.is_finite() && usd > 0.0 {
        (usd * 1e9).round() as u64
    } else {
        0
    }
}

impl Metrics {
    fn record_completion(
        &self,
        outcome: &str,
        duration_ms: u64,
        overhead_ms: Option<u64>,
        prompt_tokens: u64,
        completion_tokens: u64,
        cost_usd: Option<f64>,
    ) {
        match outcome {
            "ok" => &self.completions_ok,
            "timeout" => &self.completions_timeout,
            _ => &self.completions_error,
        }
        .fetch_add(1, Ordering::Relaxed);
        self.duration_ms_sum
            .fetch_add(duration_ms, Ordering::Relaxed);
        self.duration_count.fetch_add(1, Ordering::Relaxed);
        if let Some(o) = overhead_ms {
            self.overhead_ms_sum.fetch_add(o, Ordering::Relaxed);
            self.overhead_count.fetch_add(1, Ordering::Relaxed);
        }
        self.prompt_tokens
            .fetch_add(prompt_tokens, Ordering::Relaxed);
        self.completion_tokens
            .fetch_add(completion_tokens, Ordering::Relaxed);
        if let Some(c) = cost_usd {
            self.cost_nano_usd
                .fetch_add(usd_to_nano(c), Ordering::Relaxed);
        }
    }

    fn record_rate_limit(&self, status: &str) {
        if let Ok(mut m) = self.rate_limit_events.lock() {
            *m.entry(status.to_string()).or_insert(0) += 1;
        }
    }

    fn record_model_usage(&self, model: &str, v: &serde_json::Value) {
        let n = |k: &str| v.get(k).and_then(|x| x.as_u64()).unwrap_or(0);
        let cost = v.get("costUSD").and_then(|x| x.as_f64()).unwrap_or(0.0);
        if let Ok(mut m) = self.model_usage.lock() {
            let e = m.entry(model.to_string()).or_default();
            e.cost_nano_usd += usd_to_nano(cost);
            e.input_tokens += n("inputTokens");
            e.output_tokens += n("outputTokens");
            e.cache_read_tokens += n("cacheReadInputTokens");
            e.cache_creation_tokens += n("cacheCreationInputTokens");
        }
    }

    async fn render(&self, state: &AppState) -> String {
        let g = |a: &AtomicU64| a.load(Ordering::Relaxed);
        let mut o = String::with_capacity(4096);

        o.push_str("# HELP sidecar_up 1 when the sidecar process is serving.\n");
        o.push_str("# TYPE sidecar_up gauge\nsidecar_up 1\n");

        o.push_str("# HELP sidecar_completions_total Completions by outcome.\n");
        o.push_str("# TYPE sidecar_completions_total counter\n");
        let _ = writeln!(
            o,
            "sidecar_completions_total{{outcome=\"ok\"}} {}",
            g(&self.completions_ok)
        );
        let _ = writeln!(
            o,
            "sidecar_completions_total{{outcome=\"error\"}} {}",
            g(&self.completions_error)
        );
        let _ = writeln!(
            o,
            "sidecar_completions_total{{outcome=\"timeout\"}} {}",
            g(&self.completions_timeout)
        );

        o.push_str(
            "# HELP sidecar_cost_usd_total Subscription-equivalent spend reported by the CLI.\n",
        );
        o.push_str("# TYPE sidecar_cost_usd_total counter\n");
        let _ = writeln!(
            o,
            "sidecar_cost_usd_total {:.9}",
            g(&self.cost_nano_usd) as f64 / 1e9
        );

        o.push_str("# HELP sidecar_tokens_total Tokens by kind.\n");
        o.push_str("# TYPE sidecar_tokens_total counter\n");
        let _ = writeln!(
            o,
            "sidecar_tokens_total{{kind=\"prompt\"}} {}",
            g(&self.prompt_tokens)
        );
        let _ = writeln!(
            o,
            "sidecar_tokens_total{{kind=\"completion\"}} {}",
            g(&self.completion_tokens)
        );

        o.push_str("# HELP sidecar_pool_hits_total Completions served by a warm worker.\n");
        o.push_str("# TYPE sidecar_pool_hits_total counter\n");
        let _ = writeln!(o, "sidecar_pool_hits_total {}", g(&self.pool_hits));
        o.push_str(
            "# HELP sidecar_pool_misses_total Completions that fell back to a cold spawn.\n",
        );
        o.push_str("# TYPE sidecar_pool_misses_total counter\n");
        let _ = writeln!(o, "sidecar_pool_misses_total {}", g(&self.pool_misses));
        o.push_str("# HELP sidecar_pool_spawns_total Warm workers spawned ahead of demand.\n");
        o.push_str("# TYPE sidecar_pool_spawns_total counter\n");
        let _ = writeln!(o, "sidecar_pool_spawns_total {}", g(&self.pool_spawns));
        o.push_str("# HELP sidecar_pool_drains_total Warm workers killed by a drain.\n");
        o.push_str("# TYPE sidecar_pool_drains_total counter\n");
        let _ = writeln!(o, "sidecar_pool_drains_total {}", g(&self.pool_drains));
        o.push_str(
            "# HELP sidecar_shed_total Requests refused with 503 because no in-flight slot came free.\n",
        );
        o.push_str("# TYPE sidecar_shed_total counter\n");
        let _ = writeln!(o, "sidecar_shed_total {}", g(&self.shed));

        o.push_str(
            "# HELP sidecar_rate_limit_events_total Subscription rate-limit events by status.\n",
        );
        o.push_str("# TYPE sidecar_rate_limit_events_total counter\n");
        let rl = self
            .rate_limit_events
            .lock()
            .map(|m| m.clone())
            .unwrap_or_default();
        for (status, n) in &rl {
            let _ = writeln!(
                o,
                "sidecar_rate_limit_events_total{{status=\"{}\"}} {}",
                esc(status),
                n
            );
        }

        let mu = self
            .model_usage
            .lock()
            .map(|m| m.clone())
            .unwrap_or_default();
        o.push_str("# HELP sidecar_model_cost_usd_total Spend per model, including the CLI's hidden helper model.\n");
        o.push_str("# TYPE sidecar_model_cost_usd_total counter\n");
        for (model, a) in &mu {
            let _ = writeln!(
                o,
                "sidecar_model_cost_usd_total{{model=\"{}\"}} {:.9}",
                esc(model),
                a.cost_nano_usd as f64 / 1e9
            );
        }
        o.push_str("# HELP sidecar_model_tokens_total Tokens per model and kind.\n");
        o.push_str("# TYPE sidecar_model_tokens_total counter\n");
        for (model, a) in &mu {
            let m = esc(model);
            let _ = writeln!(
                o,
                "sidecar_model_tokens_total{{model=\"{m}\",kind=\"input\"}} {}",
                a.input_tokens
            );
            let _ = writeln!(
                o,
                "sidecar_model_tokens_total{{model=\"{m}\",kind=\"output\"}} {}",
                a.output_tokens
            );
            let _ = writeln!(
                o,
                "sidecar_model_tokens_total{{model=\"{m}\",kind=\"cache_read\"}} {}",
                a.cache_read_tokens
            );
            let _ = writeln!(
                o,
                "sidecar_model_tokens_total{{model=\"{m}\",kind=\"cache_creation\"}} {}",
                a.cache_creation_tokens
            );
        }

        // Sum/count pairs instead of histograms: Grafana can average them and we
        // stay dependency-free.
        o.push_str("# HELP sidecar_duration_ms_sum Total wall time across completions.\n");
        o.push_str("# TYPE sidecar_duration_ms_sum counter\n");
        let _ = writeln!(o, "sidecar_duration_ms_sum {}", g(&self.duration_ms_sum));
        o.push_str("# TYPE sidecar_duration_ms_count counter\n");
        let _ = writeln!(o, "sidecar_duration_ms_count {}", g(&self.duration_count));
        o.push_str("# HELP sidecar_overhead_ms_sum Wall time minus the CLI's own duration_api_ms — the sidecar's own cost.\n");
        o.push_str("# TYPE sidecar_overhead_ms_sum counter\n");
        let _ = writeln!(o, "sidecar_overhead_ms_sum {}", g(&self.overhead_ms_sum));
        o.push_str("# TYPE sidecar_overhead_ms_count counter\n");
        let _ = writeln!(o, "sidecar_overhead_ms_count {}", g(&self.overhead_count));

        o.push_str("# HELP sidecar_pool_size Configured warm-pool size (0 = disabled).\n");
        o.push_str("# TYPE sidecar_pool_size gauge\n");
        let _ = writeln!(o, "sidecar_pool_size {}", state.pool.size);
        o.push_str("# HELP sidecar_pool_warm Warm workers currently parked.\n");
        o.push_str("# TYPE sidecar_pool_warm gauge\n");
        let _ = writeln!(o, "sidecar_pool_warm {}", state.pool.warm_count().await);
        o.push_str("# HELP sidecar_inflight Completions currently executing.\n");
        o.push_str("# TYPE sidecar_inflight gauge\n");
        let inflight = state
            .max_inflight
            .saturating_sub(state.inflight.available_permits());
        let _ = writeln!(o, "sidecar_inflight {inflight}");
        o
    }
}

// ---------------------------------------------------------------------------
// Tool lockdown mode
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ToolsMode {
    /// `--tools ""` — disables the whole built-in tool set in one flag.
    NoTools,
    /// `--disallowedTools <list>` — the pre-AGRIOPS-208 shape, kept as a
    /// no-rebuild escape hatch. The list comes from `SIDECAR_DISALLOWED_TOOLS`.
    DisallowedList,
}

impl ToolsMode {
    pub fn from_env_str(raw: &str) -> ToolsMode {
        match raw.trim().to_ascii_lowercase().as_str() {
            "" | "none" => ToolsMode::NoTools,
            "disallowed-list" | "disallowed_list" | "disallowedlist" => ToolsMode::DisallowedList,
            other => {
                warn!(value = %other, "unknown SIDECAR_TOOLS_MODE — falling back to 'none'");
                ToolsMode::NoTools
            }
        }
    }
}

// ---------------------------------------------------------------------------
// argv builder (pure — unit-tested)
// ---------------------------------------------------------------------------

/// Which Claude models this proxy may run, and how an OpenAI-shaped request maps onto them.
///
/// ONE PROXY, MANY MODELS (2026-08-04). Until now the CLI model was fixed at boot from `OPUS_MODEL` and
/// the request's `model` field was echoed back but ignored — "the sidecar exists to serve exactly one
/// brain". Running haiku beside opus therefore meant a second container on a second port. The warm pool
/// was already keyed on `(model, system, completion_mode)`, so per-request models pool independently with
/// no further work: an opus worker is never handed to a haiku request.
///
/// THE ALLOWLIST IS A SAFETY BOUNDARY, NOT A CONVENIENCE. The resolved value is pushed straight into the
/// `claude` argv after `--model`. An unvalidated passthrough would let a caller send `--dangerously-...`
/// or any other flag as the "model" and have it land in the child's argument vector. Only names that
/// appear in this map are ever emitted, so an argv element can never originate from an untrusted request.
#[derive(Debug, Clone)]
pub struct ModelPolicy {
    /// request name (lowercased) -> the CLI model alias actually run.
    pub allowed: std::collections::BTreeMap<String, String>,
    /// Served when the request names nothing this proxy knows. Backward compatibility is deliberate:
    /// TG sends `claude-opus-5` today and must keep getting the same brain, not a 400.
    pub default_model: String,
}

impl ModelPolicy {
    /// Resolve a requested model to the CLI alias to run, and say whether it was RECOGNISED. The bool is
    /// not decoration: an unrecognised model silently served by the default is exactly how a caller comes
    /// to believe it is A/B-testing two tiers while measuring one, so the caller logs it.
    pub fn resolve(&self, requested: &str) -> (String, bool) {
        let want = requested.trim().to_ascii_lowercase();
        if want.is_empty() {
            return (self.default_model.clone(), false);
        }
        if let Some(cli) = self.allowed.get(&want) {
            return (cli.clone(), true);
        }
        (self.default_model.clone(), false)
    }

    /// Parse `CLAUDE_PROXY_MODELS`: comma-separated `request=cli` pairs, or a bare name meaning both.
    /// e.g. "opus,haiku,sonnet,fable" or "claude-opus-5=opus,claude-haiku-4-5=haiku".
    pub fn parse(spec: &str, default_model: &str) -> Self {
        let mut allowed = std::collections::BTreeMap::new();
        for raw in spec.split(',') {
            let entry = raw.trim();
            if entry.is_empty() {
                continue;
            }
            let (req, cli) = match entry.split_once('=') {
                Some((a, b)) => (a.trim(), b.trim()),
                None => (entry, entry),
            };
            // A model alias is a bare token. Refusing anything else is what keeps an argv element from
            // ever being attacker-shaped — a leading '-' would become a FLAG, not a value.
            if req.is_empty()
                || cli.is_empty()
                || !cli
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '.' || c == '_')
                || cli.starts_with('-')
            {
                continue;
            }
            allowed.insert(req.to_ascii_lowercase(), cli.to_string());
        }
        // The default is always resolvable by its own name, so a caller can ask for it explicitly.
        allowed
            .entry(default_model.to_ascii_lowercase())
            .or_insert_with(|| default_model.to_string());
        Self {
            allowed,
            default_model: default_model.to_string(),
        }
    }
}

/// Everything that shapes a `claude` argv. Note what is NOT here: the user
/// prompt. Since AGRIOPS-208 it travels on stdin as stream-json, so there is no
/// positional argument at all and `-p` is a bare boolean.
pub struct ArgvSpec<'a> {
    pub system: &'a str,
    pub model: &'a str,
    pub session_id: Option<&'a str>,
    pub completion_mode: bool,
    pub tools_mode: ToolsMode,
    /// Whitespace/NUL-separated tool names; only read in `DisallowedList` mode.
    pub disallowed_tools: &'a str,
}

/// Build the argv for one `claude` invocation.
///
/// Hard CLI constraints encoded here (each one is an error string the binary
/// emits if violated): `--input-format=stream-json` requires `--print`, requires
/// `--output-format=stream-json`, and requires `--verbose`.
///
/// INVARIANT: `--tools` / `--disallowedTools` are VARIADIC (`<tools...>`), so
/// they greedily eat every following non-flag argv element and must never be
/// last. `--strict-mcp-config` is emitted immediately after the tool flag's
/// values for exactly that reason — do not reorder without reading the tests.
pub fn build_argv(spec: &ArgvSpec<'_>) -> Vec<String> {
    let mut a: Vec<String> = Vec::with_capacity(16);
    // NO `--bare`: the subscription auth path requires the CLI's OAuth read.
    a.push("-p".into());
    a.push("--input-format".into());
    a.push("stream-json".into());
    a.push("--output-format".into());
    a.push("stream-json".into());
    a.push("--verbose".into());
    a.push("--model".into());
    a.push(spec.model.into());
    a.push("--system-prompt".into());
    a.push(spec.system.into());

    if spec.completion_mode {
        match spec.tools_mode {
            ToolsMode::NoTools => {
                a.push("--tools".into());
                a.push(String::new()); // `""` = disable every built-in tool
            }
            ToolsMode::DisallowedList => {
                a.push("--disallowedTools".into());
                for t in spec
                    .disallowed_tools
                    .split(|c: char| c == '\0' || c.is_whitespace())
                    .filter(|s| !s.is_empty())
                {
                    a.push(t.into());
                }
            }
        }
        // MUST stay immediately after the variadic tool flag (see INVARIANT).
        a.push("--strict-mcp-config".into());
    } else {
        // ★ UNCONDITIONAL (TG-279). `--strict-mcp-config` is load-bearing on its own, separately from
        // the tool flags: a stray `.mcp.json` in the writable bind-mounted HOME re-arms MCP tools
        // inside the sidecar. It used to be reachable only through the completion_mode branch, so
        // /run — the endpoint that deliberately leaves BUILT-IN tools at CLI defaults — also left the
        // MCP door open. Leaving built-ins default is a documented contract; letting an attacker
        // plant tool config is not.
        a.push("--strict-mcp-config".into());
    }

    if let Some(sid) = spec.session_id {
        if !sid.is_empty() {
            a.push("-r".into());
            a.push(sid.into());
        }
    }
    a
}

/// Unique-per-process OpenAI response id.
fn next_response_id(created: u64) -> String {
    let n = RESPONSE_SEQ.fetch_add(1, Ordering::Relaxed);
    format!("chatcmpl-tg-{created}-{n:08}")
}

// ---------------------------------------------------------------------------
// Bounded stderr ring
// ---------------------------------------------------------------------------

type StderrRing = Arc<StdMutex<Vec<u8>>>;

/// Append to the ring, dropping from the front once it exceeds the cap.
fn ring_push(ring: &StderrRing, bytes: &[u8]) {
    if let Ok(mut g) = ring.lock() {
        g.extend_from_slice(bytes);
        let len = g.len();
        if len > STDERR_RING_CAP {
            g.drain(..len - STDERR_RING_CAP);
        }
    }
}

/// Snapshot the first `max_chars` characters (matching the pre-AGRIOPS-208
/// diagnostic shape, which operators grep for).
fn ring_head(ring: &StderrRing, max_chars: usize) -> String {
    let bytes = ring.lock().map(|g| g.clone()).unwrap_or_default();
    String::from_utf8_lossy(&bytes)
        .chars()
        .take(max_chars)
        .collect()
}

/// Drain a child's stderr into a bounded ring for the child's whole lifetime.
async fn drain_stderr(stderr: Option<ChildStderr>, ring: StderrRing) {
    let Some(mut s) = stderr else { return };
    let mut buf = [0u8; 4096];
    loop {
        match s.read(&mut buf).await {
            Ok(0) | Err(_) => break,
            Ok(n) => ring_push(&ring, &buf[..n]),
        }
    }
}

// ---------------------------------------------------------------------------
// Warm-pool
// ---------------------------------------------------------------------------

/// A warm process is only interchangeable with another warm process built from
/// the SAME argv, so the key is exactly the argv-shaping inputs that vary per
/// request. (`session_id` never pools — a resumed session is by definition not
/// interchangeable.)
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct PoolKey {
    pub model: String,
    pub system: String,
    pub completion_mode: bool,
}

/// A live `claude` process plus everything needed to drive exactly one turn.
struct SpawnHandles {
    worker_id: u64,
    child: Child,
    stdin: Option<ChildStdin>,
    stdout: Option<BufReader<ChildStdout>>,
    stderr_ring: StderrRing,
    stderr_task: tokio::task::JoinHandle<()>,
    spawned_at: Instant,
    /// Snapshot of `AppState::token_gen` at spawn time. A pre-spawned child has
    /// the OAuth token frozen in its env, so a rotation invalidates it.
    token_gen: u64,
}

struct WarmEntry {
    key: PoolKey,
    handles: SpawnHandles,
}

struct PoolInner {
    entries: Vec<WarmEntry>,
    /// Top-ups currently in flight, so concurrent top-up tasks cannot overshoot.
    spawning: usize,
    backoff_until: Option<Instant>,
}

pub struct Pool {
    inner: Mutex<PoolInner>,
    size: usize,
    max_age: Duration,
    metrics: Arc<Metrics>,
    pub enabled: bool,
}

impl Pool {
    pub fn new(size: usize, max_age: Duration, metrics: Arc<Metrics>) -> Pool {
        Pool {
            inner: Mutex::new(PoolInner {
                entries: Vec::new(),
                spawning: 0,
                backoff_until: None,
            }),
            size,
            max_age,
            metrics,
            enabled: size > 0,
        }
    }

    /// Take a warm worker for `key`, or None. USE-ONCE: the entry is REMOVED, is
    /// never returned to the pool, and the caller must kill it after one turn.
    /// A warm process that served a second request was proven to leak the first
    /// request's content, so there is deliberately no check-in path.
    ///
    /// Also enforces liveness (`try_wait`), freshness (`max_age`) and token
    /// generation before handing anything out: a child that died at init on a
    /// stale credential must never be served to a caller.
    async fn checkout(&self, key: &PoolKey, token_gen: u64) -> Option<SpawnHandles> {
        if !self.enabled {
            return None;
        }
        let mut g = self.inner.lock().await;
        let now = Instant::now();
        let mut i = 0usize;
        let mut hit: Option<usize> = None;
        while i < g.entries.len() {
            let e = &mut g.entries[i];
            let dead = !matches!(e.handles.child.try_wait(), Ok(None));
            let stale = now.duration_since(e.handles.spawned_at) > self.max_age
                || e.handles.token_gen != token_gen;
            if dead || stale {
                let ev = g.entries.remove(i);
                ev.handles.stderr_task.abort();
                continue;
            }
            if e.key == *key {
                hit = Some(i);
                break;
            }
            i += 1;
        }
        hit.map(|i| g.entries.remove(i).handles)
    }

    /// Kill and forget every warm worker. Returns how many were dropped.
    pub async fn drain(&self, reason: &str) -> usize {
        let mut g = self.inner.lock().await;
        let n = g.entries.len();
        for e in g.entries.drain(..) {
            e.handles.stderr_task.abort(); // Child::kill_on_drop does the SIGKILL
        }
        if n > 0 {
            self.metrics
                .pool_drains
                .fetch_add(n as u64, Ordering::Relaxed);
            info!(pool_drained = n, reason = %reason, "warm pool drained");
        }
        n
    }

    /// Evict dead / aged-out / token-stale workers. Runs on a timer.
    async fn reap(&self, token_gen: u64) {
        if !self.enabled {
            return;
        }
        let mut g = self.inner.lock().await;
        let now = Instant::now();
        let before = g.entries.len();
        let max_age = self.max_age;
        let mut kept: Vec<WarmEntry> = Vec::with_capacity(before);
        for mut e in g.entries.drain(..) {
            let dead = !matches!(e.handles.child.try_wait(), Ok(None));
            let stale = now.duration_since(e.handles.spawned_at) > max_age
                || e.handles.token_gen != token_gen;
            if dead || stale {
                e.handles.stderr_task.abort();
            } else {
                kept.push(e);
            }
        }
        let reaped = before - kept.len();
        g.entries = kept;
        if reaped > 0 {
            info!(
                pool_reaped = reaped,
                pool_warm = g.entries.len(),
                "warm pool reaped"
            );
        }
    }

    /// Spawn warm workers for `key` until the pool is full. Idempotent and
    /// concurrency-safe; skipped entirely while a spawn-storm backoff is active.
    async fn top_up(self: &Arc<Self>, state: &Arc<AppState>, key: PoolKey) {
        if !self.enabled {
            return;
        }
        loop {
            {
                let mut g = self.inner.lock().await;
                if let Some(until) = g.backoff_until {
                    if Instant::now() < until {
                        return;
                    }
                    g.backoff_until = None;
                }
                if g.entries.len() + g.spawning >= self.size {
                    // Full. Make room only by evicting a worker of a DIFFERENT
                    // key (the workload's dominant system prompt should win the
                    // pool); otherwise we are already warm for this key.
                    let victim = g
                        .entries
                        .iter()
                        .enumerate()
                        .filter(|(_, e)| e.key != key)
                        .min_by_key(|(_, e)| e.handles.spawned_at)
                        .map(|(i, _)| i);
                    match victim {
                        Some(pos) => {
                            let ev = g.entries.remove(pos);
                            ev.handles.stderr_task.abort();
                            info!(
                                pool_worker_id = ev.handles.worker_id,
                                "warm worker evicted to make room for a hotter key"
                            );
                        }
                        None => return,
                    }
                }
                g.spawning += 1;
            }

            let spec = ArgvSpec {
                system: &key.system,
                model: &key.model,
                session_id: None,
                completion_mode: key.completion_mode,
                tools_mode: state.tools_mode,
                disallowed_tools: &state.disallowed_tools,
            };
            let spawned = spawn_process(state, &spec).await;
            let mut g = self.inner.lock().await;
            g.spawning -= 1;
            match spawned {
                Err(e) => {
                    warn!(error = %e, backoff_seconds = SPAWN_STORM_BACKOFF.as_secs(),
                        "warm pool top-up spawn failed — backing off");
                    g.backoff_until = Some(Instant::now() + SPAWN_STORM_BACKOFF);
                    return;
                }
                Ok(h) => {
                    let id = h.worker_id;
                    g.entries.push(WarmEntry {
                        key: key.clone(),
                        handles: h,
                    });
                    let warm = g.entries.len();
                    drop(g);
                    self.metrics.pool_spawns.fetch_add(1, Ordering::Relaxed);
                    info!(pool_worker_id = id, pool_warm = warm, "warm worker spawned");
                    // Spawn-storm guard: a child that dies within 5s means a
                    // broken credential/image, and a pool would otherwise turn
                    // that into two spawns per request forever.
                    let me = self.clone();
                    tokio::spawn(async move {
                        tokio::time::sleep(SPAWN_STORM_PROBE).await;
                        me.probe_early_death(id).await;
                    });
                }
            }
        }
    }

    async fn probe_early_death(&self, worker_id: u64) {
        let mut g = self.inner.lock().await;
        let Some(pos) = g
            .entries
            .iter()
            .position(|e| e.handles.worker_id == worker_id)
        else {
            return; // already checked out (use-once) or already reaped
        };
        let dead = !matches!(g.entries[pos].handles.child.try_wait(), Ok(None));
        if dead {
            let ev = g.entries.remove(pos);
            let tail = ring_head(&ev.handles.stderr_ring, 300);
            ev.handles.stderr_task.abort();
            g.backoff_until = Some(Instant::now() + SPAWN_STORM_BACKOFF);
            warn!(
                pool_worker_id = worker_id,
                stderr = %tail,
                backoff_seconds = SPAWN_STORM_BACKOFF.as_secs(),
                "warm worker died within 5s of spawn — pausing top-ups"
            );
        }
    }

    /// Fire-and-forget top-up. Never blocks the request path.
    fn schedule_top_up(state: Arc<AppState>, key: PoolKey) {
        if !state.pool.enabled {
            return;
        }
        tokio::spawn(async move {
            let pool = state.pool.clone();
            pool.top_up(&state, key).await;
        });
    }

    async fn warm_count(&self) -> usize {
        self.inner.lock().await.entries.len()
    }
}

// ---------------------------------------------------------------------------

#[derive(Clone)]
pub struct AppState {
    pub claude_bin: String,
    /// Live OAuth token. Sourced at startup from the override file (previous
    /// rotation) else CLAUDE_CODE_OAUTH_TOKEN env. Rotated in-place by
    /// POST /admin/rotate-token; read on every spawn.
    pub oauth_token: Arc<RwLock<String>>,
    /// Bumped on every rotation. A warm child froze the token in its env at
    /// spawn time, so any worker whose snapshot differs is unusable.
    pub token_gen: Arc<AtomicU64>,
    /// Shared-secret bearer accepted by /admin/rotate-token. None = disabled.
    pub admin_token: Option<String>,
    /// Directory where the rotation override file lives.
    pub state_dir: PathBuf,
    /// Bearer required on /v1/* (SIDECAR_API_KEY env). None = no auth (LAN-only
    /// deployments); set it in production — this endpoint spends subscription quota.
    pub api_key: Option<String>,
    /// The claude model alias/id the completions path runs (OPUS_MODEL env,
    /// default "opus"). The OpenAI request's model field is echoed back but the
    /// CLI always runs this one — the sidecar exists to serve exactly one brain.
    pub cli_model: String,
    /// Which models this proxy may run, and how a request name maps onto one (2026-08-04).
    pub models: ModelPolicy,
    /// Use-once warm pool (SIDECAR_POOL_SIZE=0 disables -> pure cold path).
    pub pool: Arc<Pool>,
    /// Bounded concurrency. Every spawn is ~245 MiB RSS; unbounded forking is
    /// how this box would OOM under a burst.
    pub inflight: Arc<Semaphore>,
    /// Permit total, so `/metrics` can report in-flight as a gauge.
    pub max_inflight: usize,
    /// How long a caller queues for a slot before we shed (SIDECAR_QUEUE_WAIT_MS).
    pub queue_wait: Duration,
    pub tools_mode: ToolsMode,
    pub disallowed_tools: String,
    /// Process-lifetime counters exposed on GET /metrics.
    pub metrics: Arc<Metrics>,
}

// ---------------------------------------------------------------------------
// Original omoikane /run contract (kept verbatim for compatibility)
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
struct RunRequest {
    system: String,
    user: String,
    model: String,
    max_tokens: Option<u32>,
    session_id: Option<String>,
    timeout_seconds: Option<u64>,
}

#[derive(Debug, Serialize)]
struct RunResponse {
    ok: bool,
    result: Option<String>,
    session_id: Option<String>,
    cost_usd: Option<f64>,
    num_turns: Option<u32>,
    /// SECONDS — unchanged unit, external contract. `duration_ms` is the
    /// precise one; this field stays for any caller already reading it.
    duration_seconds: u64,
    /// AGRIOPS-208: millisecond timing, added alongside (never replacing)
    /// `duration_seconds`.
    duration_ms: u64,
    model: Option<String>,
    error: Option<String>,
    input_tokens: Option<u32>,
    output_tokens: Option<u32>,
    cache_creation_input_tokens: Option<u32>,
    cache_read_input_tokens: Option<u32>,
}

// ---------------------------------------------------------------------------
// OpenAI-compatible contract (the LiteLLM-facing surface)
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
struct OaiMessage {
    role: String,
    /// Content is a string in every call TG/LiteLLM makes. (Array-of-parts
    /// content is not used by this estate's callers; reject it loudly.)
    content: serde_json::Value,
}

#[derive(Debug, Deserialize)]
struct OaiChatRequest {
    #[serde(default)]
    model: String,
    messages: Vec<OaiMessage>,
    #[serde(default)]
    stream: Option<bool>,
    #[serde(default)]
    user: Option<String>,
    // temperature / top_p / max_tokens etc. are accepted-and-ignored: the CLI
    // controls sampling. LiteLLM's drop_params handles most; tolerate the rest.
    #[serde(flatten)]
    _rest: serde_json::Map<String, serde_json::Value>,
}

#[derive(Debug, Serialize)]
struct OaiChoiceMessage {
    role: &'static str,
    content: String,
}

#[derive(Debug, Serialize)]
struct OaiChoice {
    index: u32,
    message: OaiChoiceMessage,
    finish_reason: &'static str,
}

#[derive(Debug, Serialize)]
struct OaiUsage {
    prompt_tokens: u32,
    completion_tokens: u32,
    total_tokens: u32,
}

#[derive(Debug, Serialize)]
struct OaiChatResponse {
    id: String,
    object: &'static str,
    created: u64,
    model: String,
    choices: Vec<OaiChoice>,
    usage: OaiUsage,
}

fn oai_error(status: StatusCode, msg: &str) -> (StatusCode, Json<serde_json::Value>) {
    (
        status,
        Json(serde_json::json!({"error": {"message": msg, "type": "invalid_request_error"}})),
    )
}

// oai_rate_limit_error builds the 429 a subscription rate-limit must return (TG-426). The `type` is
// `rate_limit_error` (what litellm and OpenAI clients key off, not `invalid_request_error`), and the
// reset window rides the body as `retry_after` seconds when known — the response stays the same
// `(StatusCode, Json<Value>)` shape every arm of chat_completions returns, so the handler's single
// concrete return type is preserved. The HTTP 429 STATUS is the load-bearing part: it is what flips
// litellm's fallback ladder, the model-tier breaker, and the judge from "success" to a typed error.
fn oai_rate_limit_error(
    msg: &str,
    retry_after_secs: Option<u64>,
) -> (StatusCode, Json<serde_json::Value>) {
    let mut err = serde_json::json!({"message": msg, "type": "rate_limit_error"});
    if let Some(s) = retry_after_secs {
        err["retry_after"] = serde_json::json!(s);
    }
    (
        StatusCode::TOO_MANY_REQUESTS,
        Json(serde_json::json!({ "error": err })),
    )
}

// ---------------------------------------------------------------------------

#[tokio::main]
async fn main() -> Result<()> {
    if std::env::args().any(|a| a == "--version") {
        println!("tg-claude-proxy {}", env!("CARGO_PKG_VERSION"));
        return Ok(());
    }

    // AGRIOPS-208: JSON by default. The human-formatted output was unparseable,
    // so no log pipeline could ever consume it. SIDECAR_LOG_FORMAT=text restores
    // the old shape for an interactive operator.
    //
    // NOTE for whoever reads the logs: the default filter below is only a
    // DEFAULT. RUST_LOG in the environment overrides it wholesale, so a compose
    // `RUST_LOG: info` silently suppresses every `debug!` this service emits.
    // Everything operationally load-bearing is therefore logged at `info`.
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| "info,claudecode_runner=debug".parse().unwrap());
    let text_logs = std::env::var("SIDECAR_LOG_FORMAT")
        .map(|v| v.trim().eq_ignore_ascii_case("text"))
        .unwrap_or(false);
    if text_logs {
        tracing_subscriber::fmt().with_env_filter(filter).init();
    } else {
        tracing_subscriber::fmt()
            .with_env_filter(filter)
            .json()
            .flatten_event(true)
            .init();
    }

    // Subscription path requires ANTHROPIC_API_KEY to be ABSENT.
    if std::env::var("ANTHROPIC_API_KEY")
        .map(|s| !s.is_empty())
        .unwrap_or(false)
    {
        warn!(
            "ANTHROPIC_API_KEY is set — claude CLI will use API auth, NOT \
             subscription. Unset it to bill against the Max subscription. \
             (spawns blank it explicitly, but fix the env.)"
        );
    }

    let claude_bin = std::env::var("CLAUDE_BIN").unwrap_or_else(|_| "claude".to_string());
    let listen_addr = std::env::var("LISTEN_ADDR").unwrap_or_else(|_| "127.0.0.1:8094".to_string());

    let probe = Command::new(&claude_bin).arg("--version").output().await;
    match probe {
        Ok(o) if o.status.success() => {
            let v = String::from_utf8_lossy(&o.stdout).trim().to_string();
            info!(claude_version = %v, "claude CLI reachable");
        }
        Ok(o) => warn!(status = %o.status, "claude --version returned non-zero"),
        Err(e) => warn!(error = %e, "claude --version probe failed (will surface on /readyz)"),
    }

    let state_dir = oauth_rotate::resolve_state_dir();
    let initial_token = oauth_rotate::read_override(&state_dir)
        .or_else(|| {
            std::env::var("CLAUDE_CODE_OAUTH_TOKEN")
                .ok()
                .filter(|s| !s.is_empty())
        })
        .unwrap_or_default();
    if initial_token.is_empty() {
        info!(
            "no OAuth token env/override — the CLI will read the bind-mounted \
             ~/.claude/.credentials.json (host keeps it fresh)"
        );
    }

    let admin_token = std::env::var("OMOIKANE_CLAUDECODE_RUNNER_ADMIN_TOKEN")
        .ok()
        .filter(|s| !s.is_empty());

    let api_key = std::env::var("SIDECAR_API_KEY")
        .ok()
        .filter(|s| !s.is_empty());
    if api_key.is_none() {
        warn!("SIDECAR_API_KEY unset — /v1/* is unauthenticated (LAN-only mode)");
    }
    let cli_model = std::env::var("OPUS_MODEL").unwrap_or_else(|_| "opus".to_string());
    // The models this proxy will run. Defaults keep the historical single-brain behaviour AND make the
    // common aliases work out of the box, so a caller asking for haiku beside opus needs no config change.
    let model_policy = ModelPolicy::parse(
        &std::env::var("CLAUDE_PROXY_MODELS").unwrap_or_else(|_| {
            "opus,haiku,sonnet,fable,claude-opus-5=opus,claude-haiku-4-5=haiku,claude-sonnet-5=sonnet,claude-fable-5=fable".to_string()
        }),
        &cli_model,
    );

    let pool_size = env_usize("SIDECAR_POOL_SIZE", 2);
    let pool_max_age_ms = env_u64("SIDECAR_POOL_MAX_AGE_MS", 900_000);
    let max_inflight = env_usize("SIDECAR_MAX_INFLIGHT", 4).max(1);
    let queue_wait_ms = env_u64("SIDECAR_QUEUE_WAIT_MS", DEFAULT_INFLIGHT_QUEUE_WAIT_MS);
    let tools_mode =
        ToolsMode::from_env_str(&std::env::var("SIDECAR_TOOLS_MODE").unwrap_or_default());
    let disallowed_tools = std::env::var("SIDECAR_DISALLOWED_TOOLS")
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| LEGACY_DISALLOWED_TOOLS.to_string());
    if tools_mode == ToolsMode::DisallowedList && std::env::var("SIDECAR_DISALLOWED_TOOLS").is_err()
    {
        warn!(
            "SIDECAR_TOOLS_MODE=disallowed-list with no SIDECAR_DISALLOWED_TOOLS — \
             using the known-STALE built-in list; set the env var to the live tool names"
        );
    }
    info!(
        pool_size,
        pool_max_age_ms,
        max_inflight,
        queue_wait_ms,
        tools_mode = ?tools_mode,
        "sidecar runtime configuration"
    );

    let metrics = Arc::new(Metrics::default());
    let state = Arc::new(AppState {
        claude_bin,
        oauth_token: Arc::new(RwLock::new(initial_token)),
        token_gen: Arc::new(AtomicU64::new(0)),
        admin_token,
        state_dir,
        api_key,
        cli_model,
        models: model_policy,
        pool: Arc::new(Pool::new(
            pool_size,
            Duration::from_millis(pool_max_age_ms),
            metrics.clone(),
        )),
        inflight: Arc::new(Semaphore::new(max_inflight)),
        max_inflight,
        queue_wait: Duration::from_millis(queue_wait_ms),
        tools_mode,
        disallowed_tools,
        metrics,
    });

    // Reaper: evict aged-out / dead / token-stale warm workers.
    if state.pool.enabled {
        let reaper_state = state.clone();
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(30));
            tick.tick().await; // interval fires immediately; skip that one
            loop {
                tick.tick().await;
                let gen = reaper_state.token_gen.load(Ordering::SeqCst);
                reaper_state.pool.reap(gen).await;
            }
        });
    }

    let app = build_router(state.clone());

    let listener = tokio::net::TcpListener::bind(&listen_addr)
        .await
        .with_context(|| format!("bind {listen_addr}"))?;
    info!(addr = %listen_addr, "tg-claude-proxy listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(state))
        .await
        .context("axum::serve")?;
    Ok(())
}

fn env_usize(key: &str, default: usize) -> usize {
    std::env::var(key)
        .ok()
        .and_then(|s| s.trim().parse::<usize>().ok())
        .unwrap_or(default)
}

fn env_u64(key: &str, default: u64) -> u64 {
    std::env::var(key)
        .ok()
        .and_then(|s| s.trim().parse::<u64>().ok())
        .unwrap_or(default)
}

/// SIGTERM/SIGINT -> stop accepting, then kill every pooled child. Restarts are
/// routine on this host; without this the warm workers are orphaned on every
/// `docker compose up -d`.
async fn shutdown_signal(state: Arc<AppState>) {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    #[cfg(unix)]
    let term = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut s) => {
                s.recv().await;
            }
            Err(e) => {
                warn!(error = %e, "cannot install SIGTERM handler");
                std::future::pending::<()>().await;
            }
        }
    };
    #[cfg(not(unix))]
    let term = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => info!("SIGINT received — draining"),
        _ = term => info!("SIGTERM received — draining"),
    }
    let n = state.pool.drain("shutdown").await;
    info!(pool_drained = n, "graceful shutdown complete");
}

async fn healthz() -> impl IntoResponse {
    (StatusCode::OK, Json(serde_json::json!({"status": "ok"})))
}

async fn readyz(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let ok = Command::new(&state.claude_bin)
        .arg("--version")
        .output()
        .await
        .map(|o| o.status.success())
        .unwrap_or(false);
    if ok {
        (
            StatusCode::OK,
            Json(serde_json::json!({"status": "ok", "claude": true})),
        )
    } else {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(serde_json::json!({"status": "not_ready", "claude": false})),
        )
    }
}

async fn metrics_endpoint(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let body = state.metrics.render(&state).await;
    (
        StatusCode::OK,
        [(
            axum::http::header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        body,
    )
}

async fn models(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    (
        StatusCode::OK,
        Json(serde_json::json!({
            "object": "list",
            "data": state.models.allowed.keys()
                .map(|id| serde_json::json!({"id": id, "object": "model", "owned_by": "tg-claude-proxy"}))
                .collect::<Vec<_>>()
        })),
    )
}

// ---------------------------------------------------------------------------
// Spawn + one-turn drive
// ---------------------------------------------------------------------------

struct SpawnOutcome {
    envelope: Option<serde_json::Value>,
    /// Wall time from request start to this struct being returned.
    elapsed_ms: u64,
    /// Wall time to the `type=="result"` line. `elapsed_ms - t_envelope_ms` is
    /// the dead time fix 1 removed; it should now be ~1-3 ms.
    t_envelope_ms: Option<u64>,
    t_return_ms: u64,
    error: Option<String>,
    /// True only for the timeout path — a timeout must NOT trigger the cold
    /// retry (the model is simply slow; retrying doubles the spend).
    timed_out: bool,
    pool_hit: bool,
    pool_worker_id: Option<u64>,
    warm_age_ms: Option<u64>,
    /// Last `rate_limit_event.rate_limit_info` seen on the stream.
    rate_limit: Option<serde_json::Value>,
}

impl SpawnOutcome {
    fn failed(started: Instant, msg: String, meta: &TurnMeta) -> SpawnOutcome {
        let ms = started.elapsed().as_millis() as u64;
        SpawnOutcome {
            envelope: None,
            elapsed_ms: ms,
            t_envelope_ms: None,
            t_return_ms: ms,
            error: Some(msg),
            timed_out: false,
            pool_hit: meta.pool_hit,
            pool_worker_id: meta.worker_id,
            warm_age_ms: meta.warm_age_ms,
            rate_limit: None,
        }
    }
}

#[derive(Clone, Copy, Default)]
struct TurnMeta {
    pool_hit: bool,
    worker_id: Option<u64>,
    warm_age_ms: Option<u64>,
}

/// Spawn a `claude` process and wire up stdin/stdout plus the concurrent stderr
/// drain. Does NOT write anything to stdin — that is what makes a pooled child
/// warm: the CLI runs its whole eager init (git detect, ripgrep scan, IDE probe,
/// TLS handshake) on zero input and then waits.
async fn spawn_process(state: &Arc<AppState>, spec: &ArgvSpec<'_>) -> Result<SpawnHandles, String> {
    let current_token = state.oauth_token.read().await.clone();
    let token_gen = state.token_gen.load(Ordering::SeqCst);
    let mut cmd = Command::new(&state.claude_bin);
    cmd.env("CLAUDE_CODE_OAUTH_TOKEN", &current_token)
        // Belt-and-braces, matching setup_token.rs: if shared.env ever leaks an
        // ANTHROPIC_API_KEY into our env the CLI would silently prefer it and
        // bill per-token instead of the subscription.
        .env("ANTHROPIC_API_KEY", "")
        .args(build_argv(spec))
        // stdin is a PIPE now (AGRIOPS-208 fix 3): `--input-format stream-json`
        // requires a readable stdin for the session lifetime, and the prompt
        // travels on it instead of argv.
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);

    let mut child = cmd.spawn().map_err(|e| format!("spawn: {e}"))?;
    let stdin = child.stdin.take();
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| "child stdout missing".to_string())?;
    let stderr = child.stderr.take();
    let ring: StderrRing = Arc::new(StdMutex::new(Vec::new()));
    let stderr_task = tokio::spawn(drain_stderr(stderr, ring.clone()));

    Ok(SpawnHandles {
        worker_id: WORKER_SEQ.fetch_add(1, Ordering::Relaxed),
        child,
        stdin,
        stdout: Some(BufReader::new(stdout)),
        stderr_ring: ring,
        stderr_task,
        spawned_at: Instant::now(),
        token_gen,
    })
}

/// Drive exactly ONE turn on an already-spawned child, then kill it.
async fn run_turn(
    state: &Arc<AppState>,
    rid: &str,
    mut h: SpawnHandles,
    user_prompt: &str,
    timeout_secs: u64,
    started: Instant,
    meta: TurnMeta,
) -> SpawnOutcome {
    // --- stdin: one NDJSON stream-json user message, then EOF ---------------
    // Wire format proven against claude 2.1.220 on this host:
    //   {"type":"user","message":{"role":"user","content":"<prompt>"}}\n
    // Built with serde_json (never hand-rolled — the prompt is arbitrary text
    // and escaping is the whole point). Closing stdin (EOF) is what tells the
    // CLI the turn is complete; with stdin held open it answers but then waits
    // for the next message forever.
    let mut line = match serde_json::to_vec(&serde_json::json!({
        "type": "user",
        "message": {"role": "user", "content": user_prompt}
    })) {
        Ok(v) => v,
        Err(e) => {
            return SpawnOutcome::failed(started, format!("encode stdin message: {e}"), &meta)
        }
    };
    line.push(b'\n');

    let mut write_err: Option<String> = None;
    match h.stdin.take() {
        None => write_err = Some("child stdin missing".into()),
        Some(mut si) => {
            if let Err(e) = si.write_all(&line).await {
                write_err = Some(format!("write stdin: {e}"));
            } else if let Err(e) = si.flush().await {
                write_err = Some(format!("flush stdin: {e}"));
            }
            drop(si); // EOF
        }
    }
    if let Some(msg) = write_err {
        // A CLI that dies at STARTUP (stale credential, broken install, missing
        // node) makes this write fail with EPIPE. Now that the prompt travels on
        // stdin, reporting the raw "Broken pipe" would bury the actual cause —
        // which is the single most common catastrophic failure this service
        // has. Fall through to the same diagnostic path as any other failure so
        // the operator still gets `claude exit=<status>: <stderr>`.
        return finish_failed(h, started, meta, rid, msg, None).await;
    }

    // --- stdout: read until the result envelope, then STOP ------------------
    let Some(mut reader) = h.stdout.take() else {
        return SpawnOutcome::failed(started, "child stdout missing".into(), &meta);
    };
    let read_envelope = async {
        let mut envelope: Option<serde_json::Value> = None;
        let mut rate_limit: Option<serde_json::Value> = None;
        let mut buf = String::new();
        loop {
            buf.clear();
            match reader.read_line(&mut buf).await {
                Ok(0) => break, // EOF without a result
                Ok(_) => {
                    let trimmed = buf.trim();
                    if trimmed.is_empty() {
                        continue;
                    }
                    let Ok(obj) = serde_json::from_str::<serde_json::Value>(trimmed) else {
                        continue;
                    };
                    match obj.get("type").and_then(|t| t.as_str()) {
                        Some("result") => {
                            envelope = Some(obj);
                            // FIX 1: the CLI holds stdout open ~0.40s past this
                            // line to flush telemetry. We have everything we
                            // need — stop reading and let the caller kill it.
                            break;
                        }
                        Some("rate_limit_event") => {
                            rate_limit = obj.get("rate_limit_info").cloned();
                        }
                        _ => {}
                    }
                }
                Err(e) => {
                    warn!(error = %e, "read_line error");
                    break;
                }
            }
        }
        (envelope, rate_limit)
    };

    let (envelope, rate_limit) =
        match timeout(Duration::from_secs(timeout_secs), read_envelope).await {
            Ok(pair) => pair,
            Err(_) => {
                warn!(
                    request_id = %rid,
                    timeout_seconds = timeout_secs,
                    pool_hit = meta.pool_hit,
                    pool_worker_id = meta.worker_id.unwrap_or(0),
                    "claude exceeded timeout — killing"
                );
                let _ = h.child.start_kill();
                h.stderr_task.abort();
                let ms = started.elapsed().as_millis() as u64;
                return SpawnOutcome {
                    envelope: None,
                    elapsed_ms: ms,
                    t_envelope_ms: None,
                    t_return_ms: ms,
                    error: Some(format!("claude timed out after {timeout_secs}s")),
                    timed_out: true,
                    pool_hit: meta.pool_hit,
                    pool_worker_id: meta.worker_id,
                    warm_age_ms: meta.warm_age_ms,
                    rate_limit: None,
                };
            }
        };

    if let Some(rl) = &rate_limit {
        // On a live phone line "97% of the 5-hour window" is the single most
        // valuable observable this service can emit — and a warm pool makes
        // hitting the wall likelier, not less likely.
        let status = rl.get("status").and_then(|v| v.as_str()).unwrap_or("");
        let util = rl
            .get("utilization")
            .and_then(|v| v.as_f64())
            .unwrap_or(0.0);
        let rl_type = rl
            .get("rateLimitType")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let resets_at = rl.get("resetsAt").and_then(|v| v.as_u64()).unwrap_or(0);
        state.metrics.record_rate_limit(status);
        if util > 0.8 {
            warn!(
                request_id = %rid, rl_status = %status, rl_type = %rl_type,
                rl_utilization = util, rl_resets_at = resets_at,
                "subscription rate-limit window is nearly exhausted"
            );
        } else {
            info!(
                request_id = %rid, rl_status = %status, rl_type = %rl_type,
                rl_utilization = util, rl_resets_at = resets_at,
                "claude subscription rate-limit window"
            );
        }
    }

    if let Some(env) = envelope {
        let t_envelope_ms = started.elapsed().as_millis() as u64;
        // SUCCESS: kill, do NOT wait. `kill_on_drop` plus tokio's orphan queue
        // reaps the direct child; grandchildren need `init: true` in compose.
        let _ = h.child.start_kill();
        h.stderr_task.abort();
        let t_return_ms = started.elapsed().as_millis() as u64;
        return SpawnOutcome {
            envelope: Some(env),
            elapsed_ms: t_return_ms,
            t_envelope_ms: Some(t_envelope_ms),
            t_return_ms,
            error: None,
            timed_out: false,
            pool_hit: meta.pool_hit,
            pool_worker_id: meta.worker_id,
            warm_age_ms: meta.warm_age_ms,
            rate_limit,
        };
    }

    finish_failed(
        h,
        started,
        meta,
        rid,
        "no result envelope".into(),
        rate_limit,
    )
    .await
}

/// The one failure tail every non-timeout failure goes through.
///
/// Gives the child a brief moment to exit so the operator-facing
/// `claude exit=exit status: 1: <stderr>` diagnostic survives — that exact
/// string is the signature of the stale-credential incident, and losing it
/// costs hours of diagnosis. The SUCCESS path never waits.
async fn finish_failed(
    mut h: SpawnHandles,
    started: Instant,
    meta: TurnMeta,
    rid: &str,
    fallback: String,
    rate_limit: Option<serde_json::Value>,
) -> SpawnOutcome {
    let exit = match timeout(FAILED_CHILD_WAIT, h.child.wait()).await {
        Ok(Ok(s)) => Some(s),
        Ok(Err(e)) => {
            warn!(request_id = %rid, error = %e, "wait on failed child");
            None
        }
        Err(_) => {
            let _ = h.child.start_kill();
            None
        }
    };
    // Let the concurrent drain reach EOF before snapshotting. Without this the
    // snapshot races the drain task: a child that fails FAST (exec error, bad
    // credential) can exit before the drain has been polled even once, and we
    // would log `claude exit=exit status: 1: ` with an EMPTY stderr — losing
    // the diagnostic exactly when it matters most. Bounded by the pipe flush of
    // an already-dead child, not by the model.
    let _ = timeout(Duration::from_millis(500), &mut h.stderr_task).await;
    let stderr_text = ring_head(&h.stderr_ring, 400);
    h.stderr_task.abort();
    let ms = started.elapsed().as_millis() as u64;
    let msg = match exit {
        Some(s) if !s.success() => {
            warn!(
                request_id = %rid, status = %s, stderr = %stderr_text,
                cause = %fallback, pool_hit = meta.pool_hit,
                "claude exited non-zero"
            );
            format!("claude exit={s}: {stderr_text}")
        }
        Some(_) => fallback,
        None => {
            warn!(
                request_id = %rid, stderr = %stderr_text, cause = %fallback,
                "child did not exit"
            );
            format!("{fallback} (child unresponsive): {stderr_text}")
        }
    };
    SpawnOutcome {
        envelope: None,
        elapsed_ms: ms,
        t_envelope_ms: None,
        t_return_ms: ms,
        error: Some(msg),
        timed_out: false,
        pool_hit: meta.pool_hit,
        pool_worker_id: meta.worker_id,
        warm_age_ms: meta.warm_age_ms,
        rate_limit,
    }
}

/// One completion: warm worker if one is available for this exact argv shape,
/// otherwise a cold spawn. A warm FAILURE (not a timeout) drains the pool and
/// retries cold exactly once — without that, one stale pool entry is a 100%
/// outage rather than one slow request.
async fn spawn_claude(
    state: &Arc<AppState>,
    rid: &str,
    system: &str,
    user_prompt: &str,
    model: &str,
    session_id: Option<&str>,
    timeout_secs: u64,
    completion_mode: bool,
) -> SpawnOutcome {
    let started = Instant::now();
    let resumes = session_id.map(|s| !s.is_empty()).unwrap_or(false);
    let key = PoolKey {
        model: model.to_string(),
        system: system.to_string(),
        completion_mode,
    };

    if state.pool.enabled && !resumes {
        let gen = state.token_gen.load(Ordering::SeqCst);
        let warm = state.pool.checkout(&key, gen).await;
        Pool::schedule_top_up(state.clone(), key.clone());
        if let Some(h) = warm {
            state.metrics.pool_hits.fetch_add(1, Ordering::Relaxed);
            let meta = TurnMeta {
                pool_hit: true,
                worker_id: Some(h.worker_id),
                warm_age_ms: Some(h.spawned_at.elapsed().as_millis() as u64),
            };
            let out = run_turn(state, rid, h, user_prompt, timeout_secs, started, meta).await;
            if out.envelope.is_some() || out.timed_out {
                return out;
            }
            let drained = state.pool.drain("warm-worker-failure").await;
            warn!(
                request_id = %rid,
                pool_worker_id = meta.worker_id.unwrap_or(0),
                pool_drained = drained,
                error = %out.error.as_deref().unwrap_or(""),
                "warm worker failed — retrying once on a cold spawn"
            );
        } else {
            state.metrics.pool_misses.fetch_add(1, Ordering::Relaxed);
        }
    } else if !resumes {
        state.metrics.pool_misses.fetch_add(1, Ordering::Relaxed);
    }

    let spec = ArgvSpec {
        system,
        model,
        session_id,
        completion_mode,
        tools_mode: state.tools_mode,
        disallowed_tools: &state.disallowed_tools,
    };
    let meta = TurnMeta::default();
    match spawn_process(state, &spec).await {
        Err(e) => {
            error!(request_id = %rid, error = %e, "spawn claude failed");
            SpawnOutcome::failed(started, e, &meta)
        }
        Ok(h) => {
            let meta = TurnMeta {
                worker_id: Some(h.worker_id),
                ..meta
            };
            run_turn(state, rid, h, user_prompt, timeout_secs, started, meta).await
        }
    }
}

/// Bounded concurrency: take a slot now, or queue briefly, or shed load. Every
/// `claude` spawn is ~245 MiB RSS — unbounded forking is an OOM waiting for a
/// burst.
async fn acquire_slot(state: &Arc<AppState>) -> Option<OwnedSemaphorePermit> {
    if let Ok(p) = state.inflight.clone().try_acquire_owned() {
        return Some(p);
    }
    match timeout(state.queue_wait, state.inflight.clone().acquire_owned()).await {
        Ok(Ok(p)) => Some(p),
        // A SHED IS A COUNTED EVENT, NOT A LOG LINE. Both call sites warn!() on this path and nothing
        // else recorded it, so the failure that broke every eval arm today was visible only to whoever
        // read the container log — `sidecar_up` stayed 1 and `sidecar_completions_total{outcome="error"}`
        // stayed 0, because a shed request never becomes a completion at all.
        _ => {
            state.metrics.shed.fetch_add(1, Ordering::Relaxed);
            None
        }
    }
}

// ---------------------------------------------------------------------------
// POST /v1/chat/completions — the LiteLLM-facing surface
// ---------------------------------------------------------------------------

/// authorized reports whether a request carries the configured bearer.
///
/// ★ EXTRACTED SO A HANDLER CANNOT SIMPLY FORGET (TG-279). The check used to be inline in
/// chat_completions and NOWHERE ELSE. `/run` did not merely skip it — its signature took no
/// `HeaderMap` at all, so it was structurally incapable of seeing an Authorization header, and
/// nothing in the type system or the review said so. Proven live on 2026-08-04: an unauthenticated
/// POST /run from the LAN returned 200 and ran a real Claude session, on the same listener where
/// /v1/chat/completions correctly answered 401.
///
/// There is no auth middleware in this service (by design — it is a two-endpoint sidecar), so the
/// only thing standing between an anonymous caller and a tool-enabled agent session is a handler
/// remembering to call this. Keep it one function, and keep every spending endpoint calling it.
/// build_router assembles the HTTP surface. Extracted from main() so a test can drive the REAL router —
/// the auth-ordering defect this exists to prevent is invisible to any test that calls a handler directly,
/// because calling a handler bypasses the extractor pipeline where the bug lives.
pub fn build_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        // Unauthenticated on purpose, exactly like /healthz: it exposes counters,
        // never prompts or credentials, and the listener is host-local.
        .route("/metrics", get(metrics_endpoint))
        .with_state(state.clone())
        // ★ THE BEARER, AS A LAYER (TG-279 follow-up). Every spending endpoint is mounted on a sub-router
        // carrying `require_bearer`, so the check runs BEFORE any extractor on the handler.
        //
        // WHY THIS IS NOT REDUNDANT WITH THE IN-HANDLER CHECKS. Axum runs extractors in signature order,
        // ahead of the handler body. `run` takes `Json(req): Json<RunRequest>` LAST, so a body that fails
        // to deserialize returns 422 from the extractor and the body of the function — including its
        // `authorized(&state, &headers)` guard — NEVER RUNS.
        //
        // Measured against the live sidecar on 2026-08-05, with no credentials at all:
        //     POST /run {}                              -> 422  "missing field `system`"
        //     POST /run {"system":..,"user":..}          -> 422  "missing field `model`"
        // An anonymous caller enumerated the request schema field by field. The in-handler check was
        // present, correct, and unreachable — and the source-scanning test asserting every spending
        // handler "can see the Authorization header" passed the whole time, because presence is not
        // position.
        //
        // The in-handler checks are KEPT. They are cheap, they are the thing tested by unit tests, and a
        // route accidentally mounted outside this layer then still fails closed.
        .merge(
            Router::new()
                .route("/run", post(run))
                .route("/v1/chat/completions", post(chat_completions))
                .route("/v1/models", get(models))
                .route("/probe-auth", get(setup_token::probe_auth))
                .route("/admin/rotate-token", post(oauth_rotate::rotate_token))
                .layer(axum::middleware::from_fn_with_state(
                    state.clone(),
                    require_bearer,
                ))
                .with_state(state.clone()),
        )
}

/// require_bearer rejects an unauthenticated request BEFORE any handler extractor runs.
///
/// This exists because `authorized()` inside a handler body is unreachable when an earlier extractor
/// rejects the request: axum runs extractors in signature order and returns their error directly. A
/// malformed body therefore produced a 422 that leaked the expected field names to an anonymous caller,
/// while every in-handler check sat behind it doing nothing.
///
/// Returning the SAME 401 shape as the in-handler checks is deliberate: a caller must not be able to tell
/// from the response whether it was stopped by the layer or the handler.
pub async fn require_bearer(
    axum::extract::State(state): axum::extract::State<Arc<AppState>>,
    headers: HeaderMap,
    request: axum::extract::Request,
    next: axum::middleware::Next,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return (
            StatusCode::UNAUTHORIZED,
            Json(error_response(0, "invalid api key".into())),
        )
            .into_response();
    }
    next.run(request).await
}

pub fn authorized(state: &AppState, headers: &HeaderMap) -> bool {
    let Some(expected) = &state.api_key else {
        return true; // unset key = auth disabled, the documented local-dev posture
    };
    headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .map(|v| v.strip_prefix("Bearer ").unwrap_or(v) == expected)
        .unwrap_or(false)
}

async fn chat_completions(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<OaiChatRequest>,
) -> impl IntoResponse {
    let rid = next_request_id();
    let span = tracing::info_span!("completion", request_id = %rid);
    let _guard = span.enter();

    // Bearer auth when configured — this endpoint spends subscription quota.
    if !authorized(&state, &headers) {
        return oai_error(StatusCode::UNAUTHORIZED, "invalid api key");
    }
    if req.stream.unwrap_or(false) {
        return oai_error(
            StatusCode::BAD_REQUEST,
            "streaming is not supported by this sidecar; send stream=false",
        );
    }
    if req.messages.is_empty() {
        return oai_error(StatusCode::BAD_REQUEST, "messages must be non-empty");
    }

    // Split the conversation: system messages -> --system-prompt; the rest is
    // flattened into one role-tagged transcript. Callers here (TG's Go loop) are
    // stateless per call and send the whole conversation each time, so a flat
    // transcript preserves exactly what the model is meant to see.
    let mut system_parts: Vec<String> = Vec::new();
    let mut convo_parts: Vec<String> = Vec::new();
    for m in &req.messages {
        let text = match &m.content {
            serde_json::Value::String(s) => s.clone(),
            other => {
                warn!(content_type = %other, "non-string message content rejected");
                return oai_error(
                    StatusCode::BAD_REQUEST,
                    "only string message content is supported",
                );
            }
        };
        match m.role.as_str() {
            "system" => system_parts.push(text),
            "user" => convo_parts.push(text),
            "assistant" => convo_parts.push(format!("[Assistant]\n{text}")),
            other => convo_parts.push(format!("[{other}]\n{text}")),
        }
    }
    let system = if system_parts.is_empty() {
        // The CLI requires a --system-prompt argument; an explicit minimal one
        // keeps the slate clean (and keeps the pool key stable).
        "You are a helpful assistant.".to_string()
    } else {
        system_parts.join("\n\n")
    };
    let user_prompt = convo_parts.join("\n\n");

    let Some(_permit) = acquire_slot(&state).await else {
        warn!(request_id = %rid, "in-flight limit reached — shedding request");
        return oai_error(StatusCode::SERVICE_UNAVAILABLE, "sidecar at capacity");
    };

    // PER-REQUEST MODEL (2026-08-04). The pool is keyed on model, so an opus worker is never handed to a
    // haiku request and both tiers stay warm side by side. An unrecognised name falls back to the default
    // rather than 400-ing — TG sends `claude-opus-5` today — but the fallback is LOGGED, because silently
    // serving one brain to a caller who asked for another is how an A/B measures itself.
    let (cli_model, recognised) = state.models.resolve(&req.model);
    if !recognised && !req.model.trim().is_empty() {
        warn!(request_id = %rid, requested = %req.model, served = %cli_model,
            "requested model is not in CLAUDE_PROXY_MODELS — serving the default");
    }
    let outcome = spawn_claude(
        &state,
        &rid,
        &system,
        &user_prompt,
        &cli_model,
        None,
        600,
        true, // completion mode: built-in tools off, MCP pinned to the CLI config
    )
    .await;

    let Some(env) = outcome.envelope else {
        let msg = outcome.error.unwrap_or_else(|| "unknown failure".into());
        state.metrics.record_completion(
            if outcome.timed_out {
                "timeout"
            } else {
                "error"
            },
            outcome.elapsed_ms,
            None,
            0,
            0,
            None,
        );
        error!(
            request_id = %rid,
            error = %msg,
            duration_ms = outcome.elapsed_ms,
            pool_hit = outcome.pool_hit,
            pool_worker_id = outcome.pool_worker_id.unwrap_or(0),
            timed_out = outcome.timed_out,
            "completion failed"
        );
        return oai_error(StatusCode::BAD_GATEWAY, &msg);
    };

    let text = env
        .get("result")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let usage = env.get("usage");
    let read_u32 = |key: &str| -> u32 {
        usage
            .and_then(|u| u.get(key))
            .and_then(|v| v.as_u64())
            .unwrap_or(0) as u32
    };
    // Bill cache tokens into prompt_tokens so TG's usage accounting sees the
    // real context size, mirroring how LiteLLM normalizes Anthropic usage.
    let prompt_tokens = read_u32("input_tokens")
        + read_u32("cache_creation_input_tokens")
        + read_u32("cache_read_input_tokens");
    let completion_tokens = read_u32("output_tokens");

    // The model actually served. The CLI's envelope lists the MAIN model plus its
    // tiny internal haiku helper (housekeeping tokens), so neither "first key" nor
    // "most output tokens" is reliable (a 4-token reply loses to the helper).
    // The truthful pick: the modelUsage entry matching the model we ASKED the CLI
    // to run (canonical-id prefix match) — falling back to most-output-tokens.
    let served_model = env
        .get("modelUsage")
        .and_then(|v| v.as_object())
        .and_then(|o| {
            o.keys()
                .find(|k| k.starts_with(&state.cli_model) || state.cli_model.starts_with(*k))
                .cloned()
                .or_else(|| {
                    o.iter()
                        .max_by_key(|(_, v)| {
                            v.get("outputTokens").and_then(|t| t.as_u64()).unwrap_or(0)
                        })
                        .map(|(k, _)| k.clone())
                })
        })
        .unwrap_or_else(|| state.cli_model.clone());

    let created = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let rl = outcome.rate_limit.as_ref();

    // The result envelope carries 21 fields; six used to be logged and every
    // expensive one was thrown away. `duration_api_ms` in particular makes this
    // service self-diagnosing: wall time MINUS the CLI's own API time IS the
    // sidecar's overhead, so every latency claim is provable from one log line
    // without a benchmark harness.
    let e_u64 = |k: &str| env.get(k).and_then(|v| v.as_u64());
    let e_str = |k: &str| env.get(k).and_then(|v| v.as_str()).unwrap_or("");
    let duration_api_ms = e_u64("duration_api_ms");
    let overhead_ms = duration_api_ms.map(|api| outcome.elapsed_ms.saturating_sub(api));
    let total_cost_usd = env.get("total_cost_usd").and_then(|v| v.as_f64());
    let session_id = e_str("session_id");

    // TG-426: a `result` envelope the CLI itself marked is_error must NEVER be served as an HTTP 200
    // success. The Max subscription's weekly-limit hit returns exactly this shape — is_error=true,
    // terminal_reason="api_error", `result` = "You've hit your weekly limit · resets ..." — and emitted
    // as a 200 it defeats every error-based safeguard at once: litellm's fallback ladder (status-only),
    // the model-tier breaker (adapters/model classifies any 2xx as ok), and the judge (parses the prose,
    // finds no JSON object → 0 judgments → judge-death). The CLI already knows internally it is an error;
    // this makes the HTTP status say so. A subscription/usage rate-limit becomes 429 (so callers back off
    // and fail over); any other CLI-flagged error becomes 502 (an upstream failure, not a completion).
    let is_error = env
        .get("is_error")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    if is_error {
        let terminal_reason = e_str("terminal_reason");
        let api_error_status = e_str("api_error_status");
        // A subscription/usage rate-limit → 429 (callers back off + fail over); any other CLI error → 502.
        // The KEY guard (TG-438): a DEFINITIVE non-429 api_error_status is authoritative — a context-length
        // 400 whose prose merely contains the word "limit" is NOT a rate limit and must not be typed 429.
        // The prose fallback stays BROAD (`contains "limit"`) on purpose, and only runs when the status is
        // empty/unknown: the CLI's rate-limit prose variants (weekly / 5-hour / usage) are NOT verified
        // against live behaviour, so requiring one of a few exact phrases here would risk misrouting a REAL
        // rate-limit to 502 — reintroducing the exact TG-426 outage-amplifier this path exists to kill.
        let status_is_429 = api_error_status.starts_with("429");
        let status_is_other = !status_is_429 && !api_error_status.is_empty();
        let rate_limited = terminal_reason == "api_error"
            && (status_is_429 || (!status_is_other && text.to_lowercase().contains("limit")));
        if rate_limited {
            // Clamp retry_after: the CLI's resetsAt unit (seconds vs milliseconds) is unverified against a
            // live weekly-limit envelope, and mixing units into resetsAt-created yields a multi-billion
            // garbage value. Bound it to a sane max so the 429 body is honest regardless of unit (TG-438).
            // retry_after is advisory — nothing reads it today; the 429 STATUS is the load-bearing part.
            const MAX_RETRY_AFTER_SECS: u64 = 7 * 24 * 60 * 60; // 7 days
            let resets_at = rl.and_then(|r| r.get("resetsAt")).and_then(|v| v.as_u64());
            let retry_after = resets_at.map(|r| r.saturating_sub(created).min(MAX_RETRY_AFTER_SECS));
            state.metrics.record_completion(
                "rate_limited",
                outcome.elapsed_ms,
                overhead_ms,
                prompt_tokens as u64,
                completion_tokens as u64,
                total_cost_usd,
            );
            warn!(
                request_id = %rid,
                terminal_reason = %terminal_reason,
                api_error_status = %api_error_status,
                resets_at = resets_at.unwrap_or(0),
                retry_after_s = retry_after.unwrap_or(0),
                requested_model = %req.model,
                caller = %req.user.as_deref().unwrap_or(""),
                "subscription rate-limit — returning HTTP 429, not a 200-with-prose (TG-426)"
            );
            return oai_rate_limit_error(&text, retry_after);
        }
        state.metrics.record_completion(
            "error",
            outcome.elapsed_ms,
            overhead_ms,
            prompt_tokens as u64,
            completion_tokens as u64,
            total_cost_usd,
        );
        error!(
            request_id = %rid,
            terminal_reason = %terminal_reason,
            api_error_status = %api_error_status,
            requested_model = %req.model,
            "CLI marked the result is_error — returning HTTP 502, not a 200 (TG-426)"
        );
        return oai_error(
            StatusCode::BAD_GATEWAY,
            if text.is_empty() {
                "upstream model error"
            } else {
                &text
            },
        );
    }

    state.metrics.record_completion(
        "ok",
        outcome.elapsed_ms,
        overhead_ms,
        prompt_tokens as u64,
        completion_tokens as u64,
        total_cost_usd,
    );
    // Per-model spend, including the CLI's hidden haiku helper — billed on every
    // single request and invisible until now.
    if let Some(mu) = env.get("modelUsage").and_then(|v| v.as_object()) {
        for (model, v) in mu {
            state.metrics.record_model_usage(model, v);
            info!(
                request_id = %rid,
                model = %model,
                input_tokens = v.get("inputTokens").and_then(|x| x.as_u64()).unwrap_or(0),
                output_tokens = v.get("outputTokens").and_then(|x| x.as_u64()).unwrap_or(0),
                cache_read_tokens = v.get("cacheReadInputTokens").and_then(|x| x.as_u64()).unwrap_or(0),
                cost_usd = v.get("costUSD").and_then(|x| x.as_f64()).unwrap_or(0.0),
                "model usage"
            );
        }
    }
    // A non-empty permission_denials means the model tried to use a tool it was
    // not allowed — i.e. the `--tools ""` lockdown has regressed. Previously
    // invisible.
    if let Some(d) = env.get("permission_denials").and_then(|v| v.as_array()) {
        if !d.is_empty() {
            warn!(
                request_id = %rid,
                denials = %serde_json::Value::Array(d.clone()),
                "model attempted a denied tool — the completion-mode lockdown may have regressed"
            );
        }
    }

    info!(
        request_id = %rid,
        session_id = %session_id,
        duration_seconds = outcome.elapsed_ms / 1000,
        duration_ms = outcome.elapsed_ms,
        duration_api_ms = duration_api_ms.unwrap_or(0),
        overhead_ms = overhead_ms.unwrap_or(0),
        ttft_ms = e_u64("ttft_ms").unwrap_or(0),
        ttft_stream_ms = e_u64("ttft_stream_ms").unwrap_or(0),
        time_to_request_ms = e_u64("time_to_request_ms").unwrap_or(0),
        t_envelope_ms = outcome.t_envelope_ms.unwrap_or(0),
        t_return_ms = outcome.t_return_ms,
        num_turns = e_u64("num_turns").unwrap_or(0),
        stop_reason = %e_str("stop_reason"),
        terminal_reason = %e_str("terminal_reason"),
        is_error = env.get("is_error").and_then(|v| v.as_bool()).unwrap_or(false),
        api_error_status = %e_str("api_error_status"),
        fast_mode_state = %e_str("fast_mode_state"),
        total_cost_usd = total_cost_usd.unwrap_or(0.0),
        pool_hit = outcome.pool_hit,
        pool_worker_id = outcome.pool_worker_id.unwrap_or(0),
        warm_age_ms = outcome.warm_age_ms.unwrap_or(0),
        rl_utilization = rl.and_then(|r| r.get("utilization")).and_then(|v| v.as_f64()).unwrap_or(0.0),
        rl_status = %rl.and_then(|r| r.get("status")).and_then(|v| v.as_str()).unwrap_or(""),
        prompt_tokens,
        completion_tokens,
        served_model = %served_model,
        caller = %req.user.as_deref().unwrap_or(""),
        requested_model = %req.model,
        "completion served"
    );
    (
        StatusCode::OK,
        Json(serde_json::json!(OaiChatResponse {
            id: next_response_id(created),
            object: "chat.completion",
            created,
            model: served_model,
            choices: vec![OaiChoice {
                index: 0,
                message: OaiChoiceMessage {
                    role: "assistant",
                    content: text
                },
                finish_reason: "stop",
            }],
            usage: OaiUsage {
                prompt_tokens,
                completion_tokens,
                total_tokens: prompt_tokens + completion_tokens,
            },
        })),
    )
}

// ---------------------------------------------------------------------------
// POST /run — original omoikane contract, now via the shared spawn helper
// ---------------------------------------------------------------------------

async fn run(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<RunRequest>,
) -> impl IntoResponse {
    // ★ THE BEARER (TG-279). This endpoint spawns a tool-enabled agent session on a host that holds
    // an OpenBao root token, and it accepted anonymous callers. `headers` exists on this signature
    // ONLY for this check — do not remove the parameter believing it unused.
    if !authorized(&state, &headers) {
        return (
            StatusCode::UNAUTHORIZED,
            Json(error_response(0, "invalid api key".into())),
        );
    }
    let timeout_secs = req.timeout_seconds.unwrap_or(600);
    let _ = req.max_tokens; // telemetry only; the CLI has no --max-tokens flag
    let rid = next_request_id();
    let span = tracing::info_span!("run", request_id = %rid);
    let _guard = span.enter();

    let Some(_permit) = acquire_slot(&state).await else {
        warn!(request_id = %rid, "in-flight limit reached — shedding /run request");
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(error_response(0, "sidecar at capacity".into())),
        );
    };

    // /run took the caller's model string and pushed it STRAIGHT into the child argv after `--model`.
    // That is an untrusted request field becoming an argument vector element: a value beginning with `-`
    // is a flag, not a model. Routing it through the same allowlist as /v1/chat/completions closes that
    // without changing any legitimate call — every model this proxy actually runs is in the map, and an
    // unrecognised one now serves the default instead of being handed to the CLI verbatim.
    let (run_model, run_recognised) = state.models.resolve(&req.model);
    if !run_recognised && !req.model.trim().is_empty() {
        warn!(request_id = %rid, requested = %req.model, served = %run_model,
            "requested model is not in CLAUDE_PROXY_MODELS — serving the default");
    }
    let outcome = spawn_claude(
        &state,
        &rid,
        &req.system,
        &req.user,
        &run_model,
        req.session_id.as_deref(),
        timeout_secs,
        false, // original /run semantics: tools as the CLI defaults them
    )
    .await;

    let Some(env) = outcome.envelope else {
        state.metrics.record_completion(
            if outcome.timed_out {
                "timeout"
            } else {
                "error"
            },
            outcome.elapsed_ms,
            None,
            0,
            0,
            None,
        );
        return (
            StatusCode::OK,
            Json(error_response(
                outcome.elapsed_ms,
                outcome.error.unwrap_or_else(|| "unknown failure".into()),
            )),
        );
    };

    let usage = env.get("usage");
    let read_u32 = |key: &str| -> Option<u32> {
        usage
            .and_then(|u| u.get(key))
            .and_then(|v| v.as_u64())
            .map(|n| n as u32)
    };
    let resp = RunResponse {
        ok: true,
        result: env.get("result").and_then(|v| v.as_str()).map(String::from),
        session_id: env
            .get("session_id")
            .and_then(|v| v.as_str())
            .map(String::from),
        cost_usd: env
            .get("total_cost_usd")
            .or_else(|| env.get("cost_usd"))
            .and_then(|v| v.as_f64()),
        num_turns: env
            .get("num_turns")
            .and_then(|v| v.as_u64())
            .map(|n| n as u32),
        duration_seconds: outcome.elapsed_ms / 1000,
        duration_ms: outcome.elapsed_ms,
        model: env.get("model").and_then(|v| v.as_str()).map(String::from),
        error: None,
        input_tokens: read_u32("input_tokens"),
        output_tokens: read_u32("output_tokens"),
        cache_creation_input_tokens: read_u32("cache_creation_input_tokens"),
        cache_read_input_tokens: read_u32("cache_read_input_tokens"),
    };
    let duration_api_ms = env.get("duration_api_ms").and_then(|v| v.as_u64());
    let overhead_ms = duration_api_ms.map(|api| outcome.elapsed_ms.saturating_sub(api));
    state.metrics.record_completion(
        "ok",
        outcome.elapsed_ms,
        overhead_ms,
        resp.input_tokens.unwrap_or(0) as u64,
        resp.output_tokens.unwrap_or(0) as u64,
        resp.cost_usd,
    );
    if let Some(mu) = env.get("modelUsage").and_then(|v| v.as_object()) {
        for (model, v) in mu {
            state.metrics.record_model_usage(model, v);
        }
    }
    info!(
        request_id = %rid,
        session_id = %resp.session_id.as_deref().unwrap_or(""),
        duration_seconds = resp.duration_seconds,
        duration_ms = resp.duration_ms,
        duration_api_ms = duration_api_ms.unwrap_or(0),
        overhead_ms = overhead_ms.unwrap_or(0),
        ttft_ms = env.get("ttft_ms").and_then(|v| v.as_u64()).unwrap_or(0),
        time_to_request_ms = env.get("time_to_request_ms").and_then(|v| v.as_u64()).unwrap_or(0),
        t_envelope_ms = outcome.t_envelope_ms.unwrap_or(0),
        t_return_ms = outcome.t_return_ms,
        stop_reason = %env.get("stop_reason").and_then(|v| v.as_str()).unwrap_or(""),
        is_error = env.get("is_error").and_then(|v| v.as_bool()).unwrap_or(false),
        total_cost_usd = resp.cost_usd.unwrap_or(0.0),
        pool_hit = outcome.pool_hit,
        pool_worker_id = outcome.pool_worker_id.unwrap_or(0),
        warm_age_ms = outcome.warm_age_ms.unwrap_or(0),
        num_turns = resp.num_turns.unwrap_or(0),
        "claude run completed"
    );
    (StatusCode::OK, Json(resp))
}

fn error_response(duration_ms: u64, msg: String) -> RunResponse {
    RunResponse {
        ok: false,
        result: None,
        session_id: None,
        cost_usd: None,
        num_turns: None,
        duration_seconds: duration_ms / 1000,
        duration_ms,
        model: None,
        error: Some(msg),
        input_tokens: None,
        output_tokens: None,
        cache_creation_input_tokens: None,
        cache_read_input_tokens: None,
    }
}

// ---------------------------------------------------------------------------
// Tests — hermetic. No network, no real CLI: where a process is genuinely
// needed, a tiny `/bin/sh` fixture stands in for `claude` and speaks the same
// stream-json contract.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    // ─── argv builder ──────────────────────────────────────────────────

    fn spec<'a>(
        mode: ToolsMode,
        completion: bool,
        sid: Option<&'a str>,
        list: &'a str,
    ) -> ArgvSpec<'a> {
        ArgvSpec {
            system: "SYS",
            model: "claude-opus-4-7",
            session_id: sid,
            completion_mode: completion,
            tools_mode: mode,
            disallowed_tools: list,
        }
    }

    fn pos(argv: &[String], needle: &str) -> Option<usize> {
        argv.iter().position(|a| a == needle)
    }

    #[test]
    fn variadic_tool_flags_are_never_last() {
        // `--tools` / `--disallowedTools` are VARIADIC: as the last element they
        // swallow nothing, but any argv reorder that puts a value after them
        // silently arms/disarms the wrong thing. Lock the invariant for EVERY
        // mode + session combination.
        for mode in [ToolsMode::NoTools, ToolsMode::DisallowedList] {
            for sid in [None, Some("11111111-2222-3333-4444-555555555555")] {
                let argv = build_argv(&spec(mode, true, sid, "Bash Edit Write"));
                let last = argv.last().expect("argv non-empty");
                assert_ne!(last, "--tools", "mode={mode:?} sid={sid:?}");
                assert_ne!(last, "--disallowedTools", "mode={mode:?} sid={sid:?}");
                for (i, a) in argv.iter().enumerate() {
                    if a == "--tools" || a == "--disallowedTools" {
                        assert!(i + 1 < argv.len(), "variadic tool flag must not be last");
                    }
                }
            }
        }
    }

    #[test]
    fn tools_none_mode_emits_empty_string_then_a_flag() {
        let argv = build_argv(&spec(ToolsMode::NoTools, true, None, ""));
        let i = pos(&argv, "--tools").expect("--tools present");
        assert_eq!(argv[i + 1], "", "`--tools \"\"` disables the built-in set");
        assert_eq!(
            argv[i + 2],
            "--strict-mcp-config",
            "the variadic --tools MUST be terminated by a flag"
        );
    }

    #[test]
    fn disallowed_list_mode_splits_the_env_list_and_terminates_with_a_flag() {
        let argv = build_argv(&spec(
            ToolsMode::DisallowedList,
            true,
            None,
            "Bash Edit\0Write\n Grep",
        ));
        let i = pos(&argv, "--disallowedTools").expect("--disallowedTools present");
        assert_eq!(&argv[i + 1..i + 5], &["Bash", "Edit", "Write", "Grep"]);
        assert_eq!(argv[i + 5], "--strict-mcp-config");
        assert!(pos(&argv, "--tools").is_none());
    }

    #[test]
    fn strict_mcp_config_is_kept_and_dynamic_sections_flag_is_gone() {
        // `--tools ""` disables only the BUILT-IN set; a stray .mcp.json in the
        // writable bind-mounted HOME would re-arm MCP tools without this flag.
        // `--exclude-dynamic-system-prompt-sections` is inert (the CLI ignores
        // it whenever --system-prompt is set, which is always here).
        let argv = build_argv(&spec(ToolsMode::NoTools, true, None, ""));
        assert!(pos(&argv, "--strict-mcp-config").is_some());
        assert!(pos(&argv, "--exclude-dynamic-system-prompt-sections").is_none());
    }

    #[test]
    fn non_completion_mode_leaves_builtin_tools_alone_but_still_locks_mcp() {
        // /run's documented contract is "tools as the CLI defaults them", so no --tools and no
        // --disallowedTools. That stays.
        //
        // What CHANGED (TG-279): this test used to also assert --strict-mcp-config was absent, which
        // conflated two unrelated things. `--tools ""` governs the BUILT-IN set; --strict-mcp-config
        // stops a stray .mcp.json in the writable bind-mounted HOME from adding MCP servers. Leaving
        // built-ins at CLI defaults is a contract; letting a caller plant tool configuration never
        // was. The old assertion recorded an accident of where the flag happened to sit in the
        // branch, and it is the reason /run shipped with the MCP door open.
        let argv = build_argv(&spec(ToolsMode::NoTools, false, None, ""));
        assert!(pos(&argv, "--tools").is_none());
        assert!(pos(&argv, "--disallowedTools").is_none());
        assert!(
            pos(&argv, "--strict-mcp-config").is_some(),
            "--strict-mcp-config must be emitted on EVERY spawn"
        );
    }

    #[test]
    fn stream_json_stdin_contract_is_complete_and_prompt_free() {
        // The CLI rejects --input-format=stream-json without ALL of these.
        let argv = build_argv(&spec(ToolsMode::NoTools, true, None, ""));
        assert_eq!(argv[0], "-p", "--input-format=stream-json requires --print");
        assert_eq!(argv[1], "--input-format");
        assert_eq!(argv[2], "stream-json");
        assert_eq!(argv[3], "--output-format");
        assert_eq!(argv[4], "stream-json");
        assert_eq!(argv[5], "--verbose");
        // And `-p` must NOT be followed by a positional prompt any more — the
        // prompt lives on stdin. `build_argv` cannot even see it.
        assert!(
            !argv.iter().any(|a| a == "--bare"),
            "never --bare (kills OAuth)"
        );
    }

    #[test]
    fn session_resume_is_appended_after_the_lockdown_block() {
        let argv = build_argv(&spec(ToolsMode::NoTools, true, Some("abc"), ""));
        let r = pos(&argv, "-r").expect("-r present");
        assert_eq!(argv[r + 1], "abc");
        assert!(r > pos(&argv, "--strict-mcp-config").unwrap());
        // empty session id must not emit -r at all
        let argv = build_argv(&spec(ToolsMode::NoTools, true, Some(""), ""));
        assert!(pos(&argv, "-r").is_none());
    }

    #[test]
    fn tools_mode_parses_and_defaults_safely() {
        assert_eq!(ToolsMode::from_env_str(""), ToolsMode::NoTools);
        assert_eq!(ToolsMode::from_env_str("none"), ToolsMode::NoTools);
        assert_eq!(ToolsMode::from_env_str(" NONE "), ToolsMode::NoTools);
        assert_eq!(
            ToolsMode::from_env_str("disallowed-list"),
            ToolsMode::DisallowedList
        );
        assert_eq!(
            ToolsMode::from_env_str("disallowed_list"),
            ToolsMode::DisallowedList
        );
        // Unknown must fail CLOSED (no tools), never open.
        assert_eq!(ToolsMode::from_env_str("everything"), ToolsMode::NoTools);
    }

    // ─── response id ───────────────────────────────────────────────────

    #[test]
    fn response_ids_are_unique_within_a_second() {
        // The old id was chatcmpl-tg-{created}-{elapsed_seconds}: two requests
        // in the same second with the same whole-second duration collided.
        let created = 1_785_000_000u64;
        let ids: HashSet<String> = (0..5000).map(|_| next_response_id(created)).collect();
        assert_eq!(ids.len(), 5000, "response ids must be unique per process");
    }

    // ─── stderr ring ───────────────────────────────────────────────────

    #[test]
    fn stderr_ring_is_bounded_and_keeps_the_tail() {
        let ring: StderrRing = Arc::new(StdMutex::new(Vec::new()));
        for _ in 0..40 {
            ring_push(&ring, &vec![b'x'; 1024]);
        }
        ring_push(&ring, b"TAIL");
        let len = ring.lock().unwrap().len();
        assert!(len <= STDERR_RING_CAP, "ring must stay bounded, got {len}");
        let s = ring_head(&ring, STDERR_RING_CAP);
        assert!(s.ends_with("TAIL"), "ring must keep the most recent bytes");
    }

    // ─── pool ──────────────────────────────────────────────────────────

    /// Hermetic stand-ins for the Claude Code CLI.
    ///
    /// `ok` blocks on stdin exactly like the real CLI under
    /// `--input-format stream-json` (eager init, then wait), emits the same
    /// three line types, then holds stdout open for 30s — which is how the
    /// tests prove fix 1 (we must not wait for it).
    ///
    /// `failing` writes to stderr and exits non-zero.
    struct Fixtures {
        ok: PathBuf,
        failing: PathBuf,
        // TG-426: the Max subscription weekly-limit turn — the CLI emits a `result` envelope marked
        // is_error with terminal_reason "api_error" and the limit prose as `result`, plus a rejected
        // rate_limit_event carrying resetsAt. This is the exact shape that used to be served as HTTP 200.
        weekly_limit: PathBuf,
        // TG-438: a NON-rate-limit CLI error — is_error, terminal_reason "api_error", a definitive non-429
        // api_error_status (400), and prose that happens to contain the word "limit" (a context-length
        // error). It must become HTTP 502, not 429: the rate_limited predicate must not misclassify it.
        context_length_error: PathBuf,
    }

    /// Both scripts are written in ONE critical section, before any test has
    /// spawned a process. Writing an executable in one thread while another
    /// thread sits between fork() and exec() makes the exec fail with ETXTBSY
    /// (rust-lang/rust#17070) — a harness artifact with nothing to do with the
    /// sidecar, but it made the pool tests fail ~40% of runs. Every spawning
    /// test goes through this accessor first, so the writes can never overlap a
    /// spawn.
    fn fixtures() -> &'static Fixtures {
        static F: std::sync::OnceLock<Fixtures> = std::sync::OnceLock::new();
        F.get_or_init(|| {
            let dir = std::env::temp_dir()
                .join(format!("sidecar-fixtures-{}", std::process::id()));
            std::fs::create_dir_all(&dir).unwrap();
            Fixtures {
                ok: write_script(
                    &dir.join("claude-ok"),
                    b"#!/bin/sh\n\
                      IFS= read -r _line || exit 7\n\
                      printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake\"}'\n\
                      printf '%s\\n' '{\"type\":\"rate_limit_event\",\"rate_limit_info\":{\"status\":\"allowed\",\"utilization\":0.42}}'\n\
                      printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ok\",\"session_id\":\"fake\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n\
                      sleep 30\n",
                ),
                failing: write_script(
                    &dir.join("claude-failing"),
                    b"#!/bin/sh\necho 'Invalid API key' >&2\nexit 1\n",
                ),
                weekly_limit: write_script(
                    &dir.join("claude-weekly-limit"),
                    b"#!/bin/sh\n\
                      IFS= read -r _line || exit 7\n\
                      printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake\"}'\n\
                      printf '%s\\n' '{\"type\":\"rate_limit_event\",\"rate_limit_info\":{\"status\":\"rejected\",\"utilization\":1.0,\"resetsAt\":9999999999}}'\n\
                      printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"terminal_reason\":\"api_error\",\"api_error_status\":\"429\",\"result\":\"You have hit your weekly limit \\u00b7 resets Aug 12, 5am\",\"session_id\":\"fake\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}'\n\
                      sleep 30\n",
                ),
                context_length_error: write_script(
                    &dir.join("claude-context-length-error"),
                    b"#!/bin/sh\n\
                      IFS= read -r _line || exit 7\n\
                      printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake\"}'\n\
                      printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"terminal_reason\":\"api_error\",\"api_error_status\":\"400\",\"result\":\"prompt is too long: it exceeds the context length limit for this model\",\"session_id\":\"fake\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}'\n\
                      sleep 30\n",
                ),
            }
        })
    }

    fn write_script(path: &std::path::Path, body: &[u8]) -> PathBuf {
        use std::io::Write;
        use std::os::unix::fs::PermissionsExt;
        let mut f = std::fs::File::create(path).unwrap();
        f.write_all(body).unwrap();
        f.sync_all().unwrap();
        drop(f);
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
        path.to_path_buf()
    }

    fn fake_claude() -> PathBuf {
        fixtures().ok.clone()
    }

    // ================================================================================================
    // ORACLES FOR THE QUEUE PATIENCE (2026-08-06).
    //
    // THE DEFECT, measured on the live sidecar: INFLIGHT_QUEUE_WAIT was a hardcoded 5 s while the mean
    // completion took 9.0 s (sidecar_duration_ms_sum / 2371 completions) against SIDECAR_MAX_INFLIGHT=4.
    // A caller finding all four slots busy waits, on average, LONGER than its patience — so the queue
    // could not succeed under contention and simply shed. 334 sheds in 24 h; downstream, `503 sidecar at
    // capacity` that LiteLLM cannot mask and TG's model breaker counts as a failure. The eval change gate
    // lost 4 of 8 sessions on its first arm, and `judge-death` latched OPEN against a brain that was
    // answering fine (TG-357).
    //
    // Nothing counted it. `sidecar_up` stayed 1 and completions{outcome="error"} stayed 0, because a shed
    // request never becomes a completion at all — it was visible only in the container log.
    // ================================================================================================

    /// A caller that would have shed under the old patience now WAITS and is served — and the wait is
    /// bounded by the CONFIGURED patience, not by a constant.
    ///
    /// The first version of this test was defeated by its own mutation: it used the default patience and a
    /// 30 ms holder, so hardcoding `Duration::from_secs(5)` back into acquire_slot still passed (the slot
    /// freed either way). It proved only "some patience ≥ 30 ms exists". Both halves now pin the
    /// configured VALUE: this one is served after 300 ms under a 2 s patience, and its sibling must SHED
    /// after 20 ms even though the slot frees at 400 ms — which a 5 s hardcode cannot do.
    ///
    /// KILLING MUTATION: hardcode any patience in acquire_slot instead of reading `state.queue_wait`. The
    /// sibling test goes RED (a 5 s hardcode serves a caller that must have been shed).
    #[tokio::test]
    async fn a_queued_caller_waits_for_a_slot_instead_of_shedding() {
        let state = test_state_wait(fake_claude(), 0, 2_000);
        let mut held = Vec::new();
        for _ in 0..state.max_inflight {
            held.push(
                state
                    .inflight
                    .clone()
                    .try_acquire_owned()
                    .expect("slots must be free at the start of the test"),
            );
        }
        let releaser = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(300)).await;
            drop(held.pop());
        });
        let started = std::time::Instant::now();
        let got = acquire_slot(&state).await;
        let waited = started.elapsed();
        releaser.await.unwrap();
        assert!(
            got.is_some(),
            "a caller queued behind a slot that freed after 300 ms was SHED under a 2 s patience \
             (sheds: {})",
            state.metrics.shed.load(Ordering::Relaxed)
        );
        assert!(
            waited < Duration::from_millis(1_500),
            "the caller was served but waited {waited:?} — acquire_slot is not waking on the released \
             permit"
        );
        assert_eq!(
            state.metrics.shed.load(Ordering::Relaxed),
            0,
            "a served request must not count as a shed"
        );
    }

    /// A caller that genuinely cannot be served within ITS OWN configured patience is still shed — and
    /// COUNTED. Bounded concurrency is the memory guard (~245 MiB per spawn) and must survive; only the
    /// patience became configurable.
    ///
    /// The slot frees at 400 ms against a 20 ms patience, so this ALSO pins that acquire_slot reads
    /// `state.queue_wait`: under a hardcoded 5 s (the old constant) the caller would be served and this
    /// goes RED.
    ///
    /// KILLING MUTATIONS: drop the `state.metrics.shed.fetch_add`; hardcode the patience; remove the
    /// bound entirely (the last hangs the suite rather than failing it — a timeout, still a kill).
    #[tokio::test]
    async fn an_unservable_caller_is_shed_and_counted() {
        let state = test_state_wait(fake_claude(), 0, 20);
        let mut held = Vec::new();
        for _ in 0..state.max_inflight {
            held.push(state.inflight.clone().try_acquire_owned().unwrap());
        }
        let releaser = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(400)).await;
            drop(held.pop());
        });
        let started = std::time::Instant::now();
        let got = acquire_slot(&state).await;
        let waited = started.elapsed();
        releaser.await.unwrap();
        assert!(
            got.is_none(),
            "a caller with a 20 ms patience was served by a slot that freed at 400 ms (waited {waited:?}) \
             — acquire_slot is using some other patience than state.queue_wait, or the semaphore stopped \
             bounding concurrency (every spawn is ~245 MiB; this is the OOM guard)"
        );
        assert!(
            waited < Duration::from_millis(300),
            "the shed took {waited:?} against a 20 ms patience — the configured value is not the bound"
        );
        assert_eq!(
            state.metrics.shed.load(Ordering::Relaxed),
            1,
            "a shed request was not counted; the failure that broke every eval arm today was visible only \
             in the container log because of exactly this"
        );
    }

    /// The patience must exceed the measured mean service time, or the queue is decorative. 9.0 s measured
    /// 2026-08-06; the default is asserted to clear it with room, not to equal it.
    ///
    /// KILLING MUTATION: set DEFAULT_INFLIGHT_QUEUE_WAIT_MS back to 5_000. RED.
    #[test]
    fn the_default_patience_exceeds_the_measured_service_time() {
        const MEASURED_MEAN_SERVICE_MS: u64 = 9_030; // 21_415_904 ms / 2371 completions, live, 2026-08-06
        assert!(
            DEFAULT_INFLIGHT_QUEUE_WAIT_MS > MEASURED_MEAN_SERVICE_MS,
            "the default queue patience ({DEFAULT_INFLIGHT_QUEUE_WAIT_MS} ms) is not longer than one \
             average completion ({MEASURED_MEAN_SERVICE_MS} ms) — a caller that queues would time out \
             before the slot ahead of it finishes, which is a load-shed gate wearing a queue's name"
        );
    }

    /// The shed counter must reach /metrics. A counter nothing renders is the same blindness one layer in.
    ///
    /// KILLING MUTATION: delete the sidecar_shed_total block from Metrics::render. RED.
    #[tokio::test]
    async fn metrics_render_exposes_the_shed_counter() {
        let state = test_state(fake_claude(), 0);
        state.metrics.shed.fetch_add(7, Ordering::Relaxed);
        let body = state.metrics.render(&state).await;
        assert!(
            body.contains("sidecar_shed_total 7"),
            "sidecar_shed_total missing from /metrics:\n{body}"
        );
        assert!(
            body.contains("# TYPE sidecar_shed_total counter"),
            "sidecar_shed_total has no TYPE line, so Prometheus reads it untyped:\n{body}"
        );
    }

    fn test_state(bin: PathBuf, pool_size: usize) -> Arc<AppState> {
        test_state_wait(bin, pool_size, DEFAULT_INFLIGHT_QUEUE_WAIT_MS)
    }

    /// Same state with an explicit queue patience, so a shed can be provoked in milliseconds instead of
    /// making the suite sit out the real default.
    fn test_state_wait(bin: PathBuf, pool_size: usize, queue_wait_ms: u64) -> Arc<AppState> {
        let metrics = Arc::new(Metrics::default());
        Arc::new(AppState {
            claude_bin: bin.to_string_lossy().to_string(),
            oauth_token: Arc::new(RwLock::new(String::new())),
            token_gen: Arc::new(AtomicU64::new(0)),
            admin_token: None,
            state_dir: std::env::temp_dir(),
            api_key: None,
            cli_model: "claude-opus-4-7".into(),
            models: ModelPolicy::parse("opus,haiku,sonnet,fable", "claude-opus-4-7"),
            pool: Arc::new(Pool::new(
                pool_size,
                Duration::from_millis(900_000),
                metrics.clone(),
            )),
            inflight: Arc::new(Semaphore::new(4)),
            max_inflight: 4,
            queue_wait: Duration::from_millis(queue_wait_ms),
            tools_mode: ToolsMode::NoTools,
            disallowed_tools: String::new(),
            metrics,
        })
    }

    // ================================================================================================
    // ORACLES FOR THE BEARER (TG-279).
    //
    // THE DEFECT, proven live on 2026-08-04 against dc1claude01: an unauthenticated
    //     POST http://192.168.181.111:8094/run  -d '{"system":"...","user":"..."}'
    // returned HTTP 200 and ran a real Claude session (cost $0.033), while the SAME listener answered
    // 401 to an unauthenticated /v1/chat/completions. `/run`'s signature took no HeaderMap at all, so
    // it could not have checked a header even in principle, and no auth middleware exists in this
    // service. The host it runs on holds an OpenBao root token.
    //
    // These tests are about the authoriser and the ROUTE TABLE, because the failure was never in the
    // check's logic — the check was correct and simply absent from one handler.
    // ================================================================================================

    pub(super) fn keyed_state() -> Arc<AppState> {
        let mut st = (*test_state(fake_claude(), 1)).clone();
        st.api_key = Some("s3cret".into());
        Arc::new(st)
    }

    fn hdrs(v: Option<&str>) -> HeaderMap {
        let mut h = HeaderMap::new();
        if let Some(v) = v {
            h.insert("authorization", v.parse().unwrap());
        }
        h
    }

    // KILLING MUTATION: return true when the header is absent. RED.
    #[test]
    fn an_anonymous_request_is_not_authorized() {
        assert!(!authorized(&keyed_state(), &hdrs(None)),
            "a request with NO Authorization header was authorized — this is exactly the live defect: \
             an anonymous LAN caller got a tool-enabled agent session on the host holding the \
             OpenBao root token");
    }

    #[test]
    fn a_wrong_bearer_is_not_authorized() {
        assert!(!authorized(&keyed_state(), &hdrs(Some("Bearer wrong"))));
        assert!(!authorized(&keyed_state(), &hdrs(Some("Bearer "))));
        assert!(!authorized(&keyed_state(), &hdrs(Some("s3cretx"))));
    }

    // The control: the right key must work, bare or Bearer-prefixed, or every caller breaks.
    #[test]
    fn the_configured_bearer_is_authorized() {
        assert!(authorized(&keyed_state(), &hdrs(Some("Bearer s3cret"))));
        assert!(authorized(&keyed_state(), &hdrs(Some("s3cret"))));
    }

    // An unset key disables auth — the documented local-dev posture. Asserted so the behaviour is a
    // decision on the record rather than an accident someone "fixes" into a hard failure.
    #[test]
    fn no_configured_key_means_auth_is_disabled_deliberately() {
        assert!(authorized(&test_state(fake_claude(), 1), &hdrs(None)));
    }

    // KILLING MUTATION: drop `headers` from run()/probe_auth() again. RED — a handler that takes no
    // HeaderMap cannot check a bearer, and nothing else in this service will notice.
    //
    // This reads the source because the property IS about the handler signatures. There is no auth
    // middleware here, so "every spending endpoint sees the headers" is not enforced by any type.
    #[test]
    fn every_spending_endpoint_can_see_the_authorization_header() {
        let main_src = include_str!("main.rs");
        let setup_src = include_str!("setup_token.rs");
        for (name, src, sig) in [
            ("run", main_src, "async fn run("),
            ("chat_completions", main_src, "async fn chat_completions("),
            ("probe_auth", setup_src, "pub async fn probe_auth("),
        ] {
            let at = src.find(sig).unwrap_or_else(|| panic!(
                "handler {name} not found by signature {sig:?} — this gate is scanning for something \
                 that no longer exists and would pass by matching nothing"));
            let body_start = src[at..]
                .find(") -> impl IntoResponse")
                .unwrap_or_else(|| panic!("could not find the end of {name}'s parameter list"));
            let params = &src[at..at + body_start];
            assert!(params.contains("HeaderMap"),
                "handler `{name}` takes no HeaderMap, so it CANNOT check the bearer. It spawns the \
                 CLI against a paid subscription on a host holding an OpenBao root token. This is \
                 the exact shape of the live defect found on 2026-08-04.");
            let after = &src[at..];
            let end = after.find("\n}\n").unwrap_or(after.len());
            assert!(
                after[..end].contains("authorized(&state, &headers)"),
                "handler `{name}` receives the headers but never calls authorized() — taking the \
                 parameter is not checking it."
            );
        }
    }

    // VACUITY FLOOR: if the route table shrinks or the names change, the scan above could pass by
    // examining a handler nobody serves.
    #[test]
    fn the_scanned_handlers_are_actually_routed() {
        let main_src = include_str!("main.rs");
        for r in [
            "\"/run\", post(run)",
            "\"/v1/chat/completions\", post(chat_completions)",
            "\"/probe-auth\", get(setup_token::probe_auth)",
        ] {
            assert!(
                main_src.contains(r),
                "route {r:?} is not registered — the signature gate is guarding a handler that is \
                 not actually reachable, which makes it decorative"
            );
        }
    }

    // `--strict-mcp-config` must be emitted on EVERY spawn, not only in completion_mode. `--tools ""`
    // disables built-ins only; without strict-mcp a stray .mcp.json in the writable bind-mounted HOME
    // re-arms MCP tools. /run deliberately leaves built-ins at CLI defaults — that is a contract —
    // but leaving the MCP door open was never part of it.
    //
    // KILLING MUTATION: move the flag back inside the `if spec.completion_mode` branch. RED.
    #[test]
    fn strict_mcp_config_is_emitted_even_when_tools_are_left_at_cli_defaults() {
        for completion_mode in [true, false] {
            let argv = build_argv(&ArgvSpec {
                model: "claude-opus-4-7",
                system: "SYS",
                completion_mode,
                tools_mode: ToolsMode::NoTools,
                disallowed_tools: "",
                session_id: None,
            });
            assert!(
                argv.iter().any(|a| a == "--strict-mcp-config"),
                "completion_mode={completion_mode}: --strict-mcp-config absent, so a planted \
                 .mcp.json in the writable HOME can re-arm MCP tools inside the sidecar"
            );
        }
    }

    fn test_key() -> PoolKey {
        PoolKey {
            model: "claude-opus-4-7".into(),
            system: "SYS".into(),
            completion_mode: true,
        }
    }

    #[tokio::test]
    async fn pool_is_use_once_a_worker_never_serves_twice() {
        // A warm process that served a SECOND request was proven to return the
        // FIRST request's planted secret (same session_id). Checkout therefore
        // REMOVES, and there is no check-in path at all.
        let state = test_state(fake_claude(), 2);
        state.pool.clone().top_up(&state, test_key()).await;
        assert_eq!(state.pool.warm_count().await, 2);

        let mut seen: HashSet<u64> = HashSet::new();
        for _ in 0..2 {
            let h = state
                .pool
                .checkout(&test_key(), 0)
                .await
                .expect("a warm worker");
            assert!(
                seen.insert(h.worker_id),
                "worker {} served twice",
                h.worker_id
            );
            h.stderr_task.abort();
        }
        assert_eq!(state.pool.warm_count().await, 0, "checkout must remove");
        assert!(
            state.pool.checkout(&test_key(), 0).await.is_none(),
            "an exhausted pool must miss, not recycle"
        );
    }

    #[tokio::test]
    async fn pool_misses_on_a_different_key_and_on_a_rotated_token() {
        let state = test_state(fake_claude(), 1);
        state.pool.clone().top_up(&state, test_key()).await;

        let other = PoolKey {
            system: "DIFFERENT".into(),
            ..test_key()
        };
        assert!(
            state.pool.checkout(&other, 0).await.is_none(),
            "key must match"
        );
        // A pre-spawned child froze the OAuth token in its env: a rotation
        // (token_gen bump) must invalidate every warm worker.
        assert!(
            state.pool.checkout(&test_key(), 1).await.is_none(),
            "token rotation must invalidate warm workers"
        );
    }

    #[tokio::test]
    async fn pool_size_zero_disables_the_pool_entirely() {
        let state = test_state(fake_claude(), 0);
        assert!(!state.pool.enabled);
        state.pool.clone().top_up(&state, test_key()).await;
        assert_eq!(state.pool.warm_count().await, 0);
        assert!(state.pool.checkout(&test_key(), 0).await.is_none());
    }

    #[tokio::test]
    async fn drain_empties_the_pool_and_reports_the_count() {
        let state = test_state(fake_claude(), 2);
        state.pool.clone().top_up(&state, test_key()).await;
        assert_eq!(state.pool.drain("test").await, 2);
        assert_eq!(state.pool.warm_count().await, 0);
        assert_eq!(state.pool.drain("test-again").await, 0);
    }

    #[tokio::test]
    async fn cold_turn_returns_at_the_result_envelope_without_waiting_for_exit() {
        // The fixture holds stdout open for 30s after the result, mirroring the
        // real CLI's ~0.40s telemetry flush. If we ever go back to child.wait()
        // this test takes 30s instead of milliseconds.
        let state = test_state(fake_claude(), 0);
        let out = spawn_claude(
            &state,
            "req-test",
            "SYS",
            "hello",
            "claude-opus-4-7",
            None,
            25,
            true,
        )
        .await;
        assert!(out.error.is_none(), "unexpected error: {:?}", out.error);
        let env = out.envelope.expect("result envelope");
        assert_eq!(env.get("result").and_then(|v| v.as_str()), Some("ok"));
        assert!(!out.pool_hit);
        assert!(
            out.elapsed_ms < 10_000,
            "took {}ms — did we wait for exit?",
            out.elapsed_ms
        );
        assert!(out.t_envelope_ms.is_some());
        // rate_limit_event is captured, not discarded.
        let rl = out.rate_limit.expect("rate_limit_info captured");
        assert_eq!(rl.get("utilization").and_then(|v| v.as_f64()), Some(0.42));
    }

    #[tokio::test]
    async fn warm_turn_reports_pool_hit_and_reuses_nothing() {
        let state = test_state(fake_claude(), 1);
        state.pool.clone().top_up(&state, test_key()).await;
        let first = state.pool.warm_count().await;
        assert_eq!(first, 1);

        let out = spawn_claude(
            &state,
            "req-test",
            "SYS",
            "hello",
            "claude-opus-4-7",
            None,
            25,
            true,
        )
        .await;
        assert!(out.envelope.is_some(), "warm turn failed: {:?}", out.error);
        assert!(out.pool_hit, "should have been served warm");
        let served = out.pool_worker_id.expect("worker id");
        assert!(out.warm_age_ms.is_some());

        // The served worker is gone from the pool; any replacement is a NEW id.
        for _ in 0..50 {
            if state.pool.warm_count().await > 0 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        if let Some(h) = state.pool.checkout(&test_key(), 0).await {
            assert_ne!(h.worker_id, served, "a served worker must never come back");
            h.stderr_task.abort();
        }
        state.pool.drain("test-teardown").await;
    }

    #[tokio::test]
    async fn a_failing_binary_surfaces_the_exit_status_diagnostic() {
        // `claude exit=exit status: N: <stderr>` is the operator-facing
        // signature of the stale-credential incident — the failure path still
        // waits briefly for the exit status AND for the concurrent stderr drain
        // to reach EOF, so the message never arrives empty.
        //
        // The fixture exits BEFORE reading stdin, so this also races the two
        // startup-death paths on purpose: sometimes the prompt write lands in
        // the pipe buffer, sometimes it fails with EPIPE. Both must produce the
        // same diagnostic — a raw "Broken pipe" would bury the real cause.
        let state = test_state(fixtures().failing.clone(), 0);
        let out = spawn_claude(&state, "req-test", "SYS", "hello", "m", None, 20, true).await;
        assert!(out.envelope.is_none());
        let err = out.error.expect("error");
        assert!(err.starts_with("claude exit="), "got: {err}");
        assert!(
            err.contains("Invalid API key"),
            "stderr must be captured concurrently: {err}"
        );
    }

    // ─── metrics ───────────────────────────────────────────────────────

    #[tokio::test]
    async fn metrics_render_exposes_every_documented_series() {
        let state = test_state(fake_claude(), 2);
        state
            .metrics
            .record_completion("ok", 1200, Some(340), 194, 8, Some(0.0079));
        state
            .metrics
            .record_completion("timeout", 600_000, None, 0, 0, None);
        state.metrics.record_rate_limit("allowed_warning");
        state.metrics.record_model_usage(
            "claude-haiku-4-5-20251001",
            &serde_json::json!({"inputTokens": 528, "outputTokens": 14, "costUSD": 0.000598}),
        );
        let body = state.metrics.render(&state).await;
        for series in [
            "sidecar_up 1",
            "sidecar_completions_total{outcome=\"ok\"} 1",
            "sidecar_completions_total{outcome=\"timeout\"} 1",
            "sidecar_tokens_total{kind=\"prompt\"} 194",
            "sidecar_rate_limit_events_total{status=\"allowed_warning\"} 1",
            "sidecar_pool_size 2",
            "sidecar_overhead_ms_sum 340",
            "sidecar_duration_ms_count 2",
        ] {
            assert!(body.contains(series), "missing series {series} in:\n{body}");
        }
        // Cost is real money and was recorded nowhere before AGRIOPS-208.
        assert!(
            body.contains("sidecar_cost_usd_total 0.007900000"),
            "cost:\n{body}"
        );
        // The CLI's hidden helper model must be broken out separately.
        assert!(body.contains(
            "sidecar_model_cost_usd_total{model=\"claude-haiku-4-5-20251001\"} 0.000598000"
        ));
        // Every series must be preceded by a TYPE line (Prometheus text format).
        for l in body
            .lines()
            .filter(|l| !l.starts_with('#') && !l.is_empty())
        {
            assert!(l.contains(' '), "malformed metric line: {l}");
        }
    }

    #[test]
    fn metric_label_values_are_escaped() {
        assert_eq!(esc("a\"b\\c\nd"), "a\\\"b\\\\c\\nd");
    }

    #[test]
    fn cost_conversion_is_lossless_at_cent_scale_and_rejects_nonsense() {
        assert_eq!(usd_to_nano(0.0079), 7_900_000);
        assert_eq!(usd_to_nano(0.0), 0);
        assert_eq!(usd_to_nano(-1.0), 0);
        assert_eq!(usd_to_nano(f64::NAN), 0);
    }

    #[tokio::test]
    async fn request_ids_are_unique() {
        let ids: HashSet<String> = (0..2000).map(|_| next_request_id()).collect();
        assert_eq!(ids.len(), 2000, "request ids must correlate uniquely");
    }

    // TG-426 — the outage amplifier. A subscription weekly-limit turn arrives from the CLI as a `result`
    // envelope marked is_error (terminal_reason "api_error", the limit prose as `result`). It MUST become
    // HTTP 429, never a 200-with-prose: a 200 defeats litellm's fallback ladder, the model-tier breaker,
    // and the judge (which parses the prose, finds no JSON → judge-death) all at once.
    //
    // KILLING MUTATION: delete the is_error guard in chat_completions — control falls through to the OK
    // builder, the response is 200, and this test goes RED.
    #[tokio::test]
    async fn a_subscription_weekly_limit_is_429_not_200() {
        use tower::ServiceExt;
        let state = test_state(fixtures().weekly_limit.clone(), 1); // api_key None ⇒ auth off
        let body = serde_json::json!({
            "model": "opus",
            "messages": [{"role": "user", "content": "hi"}],
            "stream": false
        })
        .to_string();
        let req = axum::http::Request::builder()
            .method("POST")
            .uri("/v1/chat/completions")
            .header("content-type", "application/json")
            .body(axum::body::Body::from(body))
            .unwrap();
        let resp = build_router(state).oneshot(req).await.unwrap();
        let status = resp.status();
        assert_eq!(
            status,
            StatusCode::TOO_MANY_REQUESTS,
            "a weekly-limit turn returned {status}, not 429 — a 200-with-prose is the 2026-08-08 outage \
             amplifier: litellm never fails over, the model breaker never trips, the judge parses prose \
             as its verdict (TG-426)"
        );
        let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        let v: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(
            v["error"]["type"], "rate_limit_error",
            "the 429 body must be a rate_limit_error (what litellm/OpenAI clients key off), got {v}"
        );
        assert!(
            v.get("choices").is_none(),
            "a limit response must NOT be shaped like a chat.completion, got {v}"
        );
        // TG-438: retry_after is CLAMPED to a sane max. The fixture's resetsAt (9999999999) minus the real
        // request time is a multi-billion garbage value; the clamp bounds it to 7 days so the body is honest
        // regardless of the (unverified) resetsAt unit. Killing mutation: drop `.min(MAX_RETRY_AFTER_SECS)`
        // and retry_after is billions — this goes RED.
        if let Some(ra) = v["error"]["retry_after"].as_u64() {
            assert!(
                ra <= 7 * 24 * 60 * 60,
                "retry_after = {ra}s exceeds the 7-day clamp — an unclamped resetsAt-created (especially if \
                 the CLI emits resetsAt in milliseconds) puts a garbage multi-billion wait on the 429 body (TG-438)"
            );
        }
    }

    // TG-438: the counterpart to the weekly-limit test. A CLI error that is NOT a rate-limit — a definitive
    // non-429 status (400) whose prose happens to contain the word "limit" (a context-length error) — must
    // become HTTP 502, not 429. Before the predicate was tightened, `text.contains("limit")` alone typed this
    // as a rate_limit_error/429, telling the caller to back off and fail over on a request that will fail
    // identically forever. Killing mutation: drop the `!status_is_other` guard so a definitive non-429 status
    // no longer wins, and this goes RED — the context-length 400 whose prose contains "limit" becomes 429.
    #[tokio::test]
    async fn a_non_rate_limit_cli_error_is_502_not_429() {
        use tower::ServiceExt;
        let state = test_state(fixtures().context_length_error.clone(), 1); // api_key None ⇒ auth off
        let body = serde_json::json!({
            "model": "opus",
            "messages": [{"role": "user", "content": "hi"}],
            "stream": false
        })
        .to_string();
        let req = axum::http::Request::builder()
            .method("POST")
            .uri("/v1/chat/completions")
            .header("content-type", "application/json")
            .body(axum::body::Body::from(body))
            .unwrap();
        let resp = build_router(state).oneshot(req).await.unwrap();
        let status = resp.status();
        // The STATUS is the load-bearing assertion (the response `type` is hardcoded by oai_error and cannot
        // falsify the predicate). A 429 here would reintroduce the TG-426 outage-amplifier: litellm backs off
        // and fails over, and the caller retries into a wall on a request that will fail identically forever.
        assert_eq!(
            status,
            StatusCode::BAD_GATEWAY,
            "a non-rate-limit is_error (400 context-length, prose contains 'limit') returned {status}, not \
             502 — the rate_limited predicate must not misclassify a definitive non-429 status as a \
             subscription rate-limit (TG-438)"
        );
    }

    // The vacuity floor for the guard: a genuinely-successful turn must STILL be a 200 chat.completion.
    // If the guard over-fired (treated every turn as an error), the fix would take the whole surface down.
    #[tokio::test]
    async fn a_successful_turn_is_still_200() {
        use tower::ServiceExt;
        let state = test_state(fake_claude(), 1);
        let body = serde_json::json!({
            "model": "opus",
            "messages": [{"role": "user", "content": "hi"}],
            "stream": false
        })
        .to_string();
        let req = axum::http::Request::builder()
            .method("POST")
            .uri("/v1/chat/completions")
            .header("content-type", "application/json")
            .body(axum::body::Body::from(body))
            .unwrap();
        let resp = build_router(state).oneshot(req).await.unwrap();
        assert_eq!(
            resp.status(),
            StatusCode::OK,
            "a clean completion must stay a 200 — the TG-426 guard must fire only on is_error envelopes"
        );
    }
}

#[cfg(test)]
mod model_policy_tests {
    use super::*;

    // KILLING MUTATION: pass the requested model through to argv unvalidated. RED — the resolved value is
    // pushed after `--model` in the child argv, so a request field beginning with `-` would become a FLAG.
    #[test]
    fn an_unknown_model_never_reaches_argv() {
        let p = ModelPolicy::parse("opus,haiku", "opus");
        for hostile in [
            "--dangerously-skip-permissions",
            "-p",
            "--system-prompt",
            "; rm -rf /",
            "",
        ] {
            let (served, recognised) = p.resolve(hostile);
            assert!(!recognised, "{hostile:?} was treated as a known model");
            assert_eq!(
                served, "opus",
                "{hostile:?} resolved to {served:?} instead of the default"
            );
        }
    }

    // A flag-shaped entry in the CONFIG is refused too — the allowlist must not be a way to smuggle one in.
    #[test]
    fn a_flag_shaped_alias_is_refused_by_the_parser() {
        let p = ModelPolicy::parse("opus,--dangerously-skip-permissions,evil=-p", "opus");
        assert!(
            !p.allowed.values().any(|v| v.starts_with('-')),
            "a flag-shaped alias survived: {:?}",
            p.allowed
        );
    }

    // The point of the change: several models, side by side, each resolving to itself.
    #[test]
    fn parallel_models_each_resolve_to_their_own_cli_alias() {
        let p = ModelPolicy::parse("opus,haiku,sonnet,fable", "opus");
        for (req, want) in [
            ("opus", "opus"),
            ("haiku", "haiku"),
            ("sonnet", "sonnet"),
            ("fable", "fable"),
        ] {
            let (got, ok) = p.resolve(req);
            assert!(ok && got == want, "{req} -> {got} (recognised={ok})");
        }
    }

    // Backward compatibility, stated as a test because TG sends this exact string today.
    #[test]
    fn the_openai_style_names_tg_sends_still_map_to_a_brain() {
        let p = ModelPolicy::parse("claude-opus-5=opus,claude-haiku-4-5=haiku", "opus");
        assert_eq!(p.resolve("claude-opus-5"), ("opus".to_string(), true));
        assert_eq!(p.resolve("claude-haiku-4-5"), ("haiku".to_string(), true));
    }

    // Case must not decide which brain runs.
    #[test]
    fn resolution_is_case_insensitive() {
        let p = ModelPolicy::parse("Haiku", "opus");
        assert_eq!(p.resolve("HAIKU"), ("Haiku".to_string(), true));
    }

    // KILLING MUTATION: drop the model from PoolKey. RED — a warm opus worker handed to a haiku request
    // would serve the wrong brain silently, which is worse than a cold start.
    #[test]
    fn the_pool_key_separates_models() {
        let opus = PoolKey {
            model: "opus".into(),
            system: "S".into(),
            completion_mode: true,
        };
        let haiku = PoolKey {
            model: "haiku".into(),
            system: "S".into(),
            completion_mode: true,
        };
        assert_ne!(
            opus, haiku,
            "two models share a pool key — a warm worker could serve the wrong brain"
        );
    }
}

#[cfg(test)]
mod auth_ordering_tests {
    //! THE AUTH CHECK MUST RUN BEFORE THE BODY EXTRACTOR.
    //!
    //! `run` takes `Json(req): Json<RunRequest>` as its last parameter. Axum runs extractors in signature
    //! order, ahead of the handler body, and returns an extractor's error directly — so a body that fails
    //! to deserialize returned 422 and the `authorized(&state, &headers)` guard inside the function never
    //! executed.
    //!
    //! Measured against the live sidecar on 2026-08-05 with NO credentials:
    //!
    //!     POST /run {}                          -> 422  "missing field `system`"
    //!     POST /run {"system":..,"user":..}      -> 422  "missing field `model`"
    //!
    //! An anonymous caller enumerated the request schema field by field, on a listener published to
    //! 0.0.0.0, against a service that spawns a tool-enabled agent on a host holding an OpenBao root token.
    //!
    //! The existing source-scanning test asserted every spending handler "can see the Authorization
    //! header" and passed throughout, because PRESENCE IS NOT POSITION. These tests drive the real router,
    //! which is the only way to observe extractor ordering.
    use super::tests::keyed_state;
    use super::*;
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use tower::ServiceExt;

    async fn status_for(body: &'static str, auth: Option<&str>) -> StatusCode {
        let state = keyed_state();
        let mut req = Request::builder()
            .method("POST")
            .uri("/run")
            .header("content-type", "application/json");
        if let Some(a) = auth {
            req = req.header("authorization", a);
        }
        let resp = build_router(state)
            .oneshot(req.body(Body::from(body)).unwrap())
            .await
            .unwrap();
        resp.status()
    }

    #[tokio::test]
    async fn a_malformed_body_without_credentials_is_401_not_422() {
        let got = status_for("{}", None).await;
        assert_eq!(
            got,
            StatusCode::UNAUTHORIZED,
            "an anonymous request with a malformed body returned {got} instead of 401. If this is 422, \
             the Json extractor ran before the auth check and rejected the body first — which leaks the \
             request schema field-by-field to a caller holding no credentials, and is the exact live \
             defect measured on 2026-08-05."
        );
    }

    #[tokio::test]
    async fn a_malformed_body_with_a_wrong_bearer_is_also_401() {
        assert_eq!(
            status_for("{}", Some("Bearer wrong")).await,
            StatusCode::UNAUTHORIZED,
            "a wrong bearer must be refused by the layer before the body is parsed"
        );
    }

    // THE VACUITY FLOOR. If the fixture state carried no key, `authorized` might return true for everyone
    // and both tests above would pass while asserting nothing about the ordering.
    #[tokio::test]
    async fn the_fixture_state_actually_requires_a_key() {
        let state = keyed_state();
        assert!(
            !authorized(&state, &HeaderMap::new()),
            "the test fixture authorizes anonymous callers, so the assertions above prove nothing — \
             they would pass with the middleware removed entirely"
        );
    }
}
