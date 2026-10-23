//! Auth-state probe for the operator's Max-20 OAuth.
//!
//! We used to spawn `claude setup-token` in a PTY pair from inside the
//! container so the operator could refresh OAuth via a web form. That
//! design fought the platform: claude-code's setup-token expects a real
//! TTY + an OS keychain, and the container has neither. The flow appeared
//! to complete on screen but never persisted creds (anthropics/claude-code#50743).
//!
//! Replaced with the official Anthropic-documented headless pattern:
//! operator runs `claude setup-token` ONCE on a workstation with a
//! keychain (laptop or claude-runner host), gets a 1-year long-lived
//! token, encrypts it into `secrets/shared.env.encrypted` (sops + age),
//! and pushes. AWX deploy decrypts to a host file mounted into this
//! container as `CLAUDE_CODE_OAUTH_TOKEN`. The token survives container
//! recreates, host reboots, and CI redeploys.
//!
//! All this module does now is `/probe-auth` — a tiny `claude -p` call
//! that the daemon polls every 5 minutes to populate the
//! `ai_runner_health` row for this host. It does NOT pass `--bare`:
//! `--bare` skips OAuth + keychain + `CLAUDE_CODE_OAUTH_TOKEN`, restricting
//! auth to `ANTHROPIC_API_KEY` only — false negative for our subscription
//! path.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::Serialize;
use std::sync::Arc;

use crate::AppState;

/// Path inside the container where claude-code persists creds when a
/// keychain IS available. We bind-mount this from the host (where the
/// operator originally logged in via `claude /login`); even though
/// `claude -p` actually authenticates from `CLAUDE_CODE_OAUTH_TOKEN` env,
/// surfacing the file's mtime here helps diagnose state drift.
///
/// AGRIOPS-208: this used to be the hardcoded `/home/tg/.claude/...`
/// (the contribution's own copy said `/home/tg/`, which is ITS fork's path). Every -ng deployment mounts the credential under
/// `/home/sidecar/`, so `creds_present` was permanently `false` and the probe's
/// most useful field was dead. Resolve it instead: explicit
/// `SIDECAR_CREDS_PATH` wins, else `$HOME/.claude/.credentials.json`, else the
/// historical default so a fork that never sets HOME still behaves as before.
fn creds_path() -> PathBuf {
    if let Ok(p) = std::env::var("SIDECAR_CREDS_PATH") {
        if !p.trim().is_empty() {
            return PathBuf::from(p);
        }
    }
    if let Ok(home) = std::env::var("HOME") {
        if !home.trim().is_empty() {
            return PathBuf::from(home)
                .join(".claude")
                .join(".credentials.json");
        }
    }
    PathBuf::from("/home/tg/.claude/.credentials.json")
}

#[derive(Debug, Serialize)]
pub struct ProbeResponse {
    pub authenticated: bool,
    pub message: String,
    pub creds_present: bool,
    pub creds_mtime_rfc3339: Option<String>,
    /// The path actually probed — without this the operator cannot tell a
    /// missing credential from a mis-resolved path.
    pub creds_path: String,
    pub probe_duration_ms: u64,
}

/// Lightweight auth probe — `echo hi | claude -p --output-format json`.
/// Detects "Not logged in" + surfaces it; daemons poll this every 5 min.
pub async fn probe_auth(
    State(state): State<Arc<AppState>>,
    headers: axum::http::HeaderMap,
) -> impl IntoResponse {
    // ★ THE BEARER (TG-279). This endpoint SPAWNS THE CLI on every call, using the live OAuth token.
    // It was anonymous, and it does not take an in-flight slot — so an unauthenticated caller could
    // spawn processes in a loop against the single subscription that is now the sole brain for both
    // the `primary` and `fast` tiers. Daemons that poll this every 5 min already hold the key they
    // use for /v1/chat/completions, so requiring it costs them nothing.
    if !crate::authorized(&state, &headers) {
        return (
            axum::http::StatusCode::UNAUTHORIZED,
            Json(ProbeResponse {
                authenticated: false,
                message: "invalid api key".into(),
                creds_present: false,
                creds_mtime_rfc3339: None,
                creds_path: String::new(),
                probe_duration_ms: 0,
            }),
        );
    }
    let started = Instant::now();
    let creds = creds_path();
    let creds_present = creds.exists();
    let creds_mtime_rfc3339 = creds_mtime().and_then(|t| {
        t.duration_since(std::time::UNIX_EPOCH)
            .ok()
            .and_then(|d| time::OffsetDateTime::from_unix_timestamp(d.as_secs() as i64).ok())
            .and_then(|odt| {
                odt.format(&time::format_description::well_known::Rfc3339)
                    .ok()
            })
    });

    // Snapshot the live OAuth token under the read-lock and inject
    // explicitly. The /probe-auth endpoint must reflect the post-
    // rotation auth state, not the startup env value — OMOIKANE-706.
    let current_token = state.oauth_token.read().await.clone();
    let output = tokio::process::Command::new(&state.claude_bin)
        .args(["-p", "--output-format", "json"])
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        // Belt-and-braces: even if shared.env leaked an ANTHROPIC_API_KEY into
        // our env (shouldn't, the compose `environment:` block blanks it),
        // we explicitly clear it here so the CLI MUST use
        // `CLAUDE_CODE_OAUTH_TOKEN` from the parent env (inherited).
        .env("ANTHROPIC_API_KEY", "")
        .env("CLAUDE_CODE_OAUTH_TOKEN", &current_token)
        .spawn();

    let (authenticated, message) = match output {
        Err(e) => (false, format!("spawn claude: {e}")),
        Ok(mut child) => {
            if let Some(stdin) = child.stdin.as_mut() {
                use tokio::io::AsyncWriteExt;
                let _ = stdin.write_all(b"hi\n").await;
            }
            match tokio::time::timeout(Duration::from_secs(20), child.wait_with_output()).await {
                Err(_) => (false, "claude probe timed out (20s)".into()),
                Ok(Err(e)) => (false, format!("wait_with_output: {e}")),
                Ok(Ok(out)) => {
                    let stdout = String::from_utf8_lossy(&out.stdout);
                    if let Ok(parsed) = serde_json::from_str::<serde_json::Value>(stdout.trim()) {
                        let is_err = parsed
                            .get("is_error")
                            .and_then(|v| v.as_bool())
                            .unwrap_or(false);
                        let result_text = parsed
                            .get("result")
                            .and_then(|v| v.as_str())
                            .unwrap_or("")
                            .to_string();
                        if is_err {
                            (false, result_text)
                        } else {
                            (true, "ok".into())
                        }
                    } else {
                        let trimmed = stdout.trim().to_string();
                        if trimmed.is_empty() {
                            (false, "claude returned empty output".into())
                        } else {
                            (
                                false,
                                format!(
                                    "non-JSON output: {}",
                                    &trimmed.chars().take(160).collect::<String>()
                                ),
                            )
                        }
                    }
                }
            }
        }
    };

    let probe_duration_ms = started.elapsed().as_millis() as u64;

    (
        axum::http::StatusCode::OK,
        Json(ProbeResponse {
            authenticated,
            message,
            creds_present,
            creds_mtime_rfc3339,
            creds_path: creds.to_string_lossy().to_string(),
            probe_duration_ms,
        }),
    )
}

fn creds_mtime() -> Option<std::time::SystemTime> {
    std::fs::metadata(creds_path())
        .ok()
        .and_then(|m| m.modified().ok())
}
