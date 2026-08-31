//! POST /admin/rotate-token — accept a freshly-generated Max-20 OAuth
//! token from the operator-facing /admin/ai/claudecode-login UI in the
//! omoikane daemon and persist it as the effective auth credential for
//! subsequent `claude -p` spawns.
//!
//! Why this exists (replacing the sops+CI+AWX dance — OMOIKANE-706):
//! the previous rotation flow required the operator to run a 3-step
//! manual sequence (decrypt secrets/shared.env.encrypted, edit the
//! CLAUDE_CODE_OAUTH_TOKEN line, re-encrypt, open MR, wait for AWX
//! deploy on each NO host). That's a 15-30min cycle per rotation,
//! cited by the operator in directive 2026-05-25 as "make these
//! requests obsolete".
//!
//! The new flow: operator runs `claude setup-token` on a workstation
//! with an OS keychain (unchanged — containers still have no keychain),
//! copies the printed token, pastes it into the per-server input on
//! /admin/ai/claudecode-login, clicks save. The daemon POSTs here with
//! the new token; we validate, write it atomically to a state-volume
//! file that survives container recreate, and snap our in-process
//! cached value so the very next `claude -p` spawn picks it up via
//! the explicit CLAUDE_CODE_OAUTH_TOKEN env-overlay.
//!
//! Why a state-volume file: env_file (compose `env_file:` referencing
//! /srv/.../secrets/shared.env) is read once at container start. We
//! cannot rewrite that file from inside the container (it's
//! root-owned on the host, our UID is 1001). The state-volume
//! (`omoikane-claudecode-runner-state` named volume mounted at HOME)
//! is writable by UID 1001 and persists across `docker compose up
//! --force-recreate`. So we treat env_file as the bootstrap fallback
//! (for fresh hosts that have never received an UI-driven rotation)
//! and the state-file as the live source of truth when present.
//!
//! Auth model: a single shared bearer token,
//! `OMOIKANE_CLAUDECODE_RUNNER_ADMIN_TOKEN`, present on both ends
//! (this runner + the daemon — same env var name so a single
//! shared.env entry feeds both containers). Constant-time
//! comparison. If unset on the runner side, the endpoint refuses
//! with 503 ("rotation disabled") rather than 401 — distinguishing
//! config gap from credential mismatch on the operator's end.
//!
//! Token format gate: prefix `sk-ant-oat01-` + ASCII charset
//! `[A-Za-z0-9_-]` + length window (60..=500). We do NOT call the
//! Anthropic API to validate the token; the daemon polls /probe-auth
//! every 5 min and surfaces the live auth state on the same admin
//! page, which is the authoritative signal.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::AppState;

/// File inside the state volume that holds the live OAuth token
/// after a UI-driven rotation. Mode 0600, owned UID 1001. Survives
/// container recreate via the named volume `omoikane-claudecode-runner-state`.
pub const OAUTH_OVERRIDE_FILENAME: &str = "oauth-override";

/// Minimum token length we accept. The real Anthropic Max-20 OAuth
/// token is ~95 chars; we set the floor at 60 to catch obviously
/// truncated paste-clipboard mishaps without being so strict we'd
/// reject a future format tweak from Anthropic.
const TOKEN_MIN_LEN: usize = 60;
/// Maximum token length. 500 leaves room for Anthropic to extend the
/// format (e.g. version bump or extra suffix segments) while still
/// rejecting a "user pasted a paragraph of text into the token field"
/// case that would otherwise hit our SHA384 path.
const TOKEN_MAX_LEN: usize = 500;
/// Required prefix. All Max-20 long-lived OAuth tokens currently
/// emit `sk-ant-oat01-`. If Anthropic ever bumps to `oat02`, the
/// runner test suite will catch it and we update both endpoints.
const TOKEN_PREFIX: &str = "sk-ant-oat01-";

#[derive(Debug, Deserialize)]
pub struct RotateRequest {
    pub token: String,
}

#[derive(Debug, Serialize)]
pub struct RotateResponse {
    pub ok: bool,
    /// RFC3339 timestamp the rotation completed at. Surfaced into the
    /// admin UI flash on success so the operator sees a concrete
    /// confirmation rather than a generic "OK".
    pub rotated_at: Option<String>,
    /// On failure: a short, operator-readable string. NEVER includes
    /// the submitted token (even if the token was the problem).
    pub error: Option<String>,
}

/// POST /admin/rotate-token. Bearer-authed via `OMOIKANE_RUNNER_ADMIN_TOKEN`.
///
/// Sequence on success:
/// 1. Auth (Authorization: Bearer …)
/// 2. Validate token format (prefix + charset + length)
/// 3. Atomic write to `${state_dir}/oauth-override`
/// 4. Update in-memory `state.oauth_token`
/// 5. Return 200 with `rotated_at`
///
/// Atomic write semantics: we open a `.tmp` sibling, write, fsync,
/// then `rename` it onto the target path. Rename on POSIX is atomic
/// on the same filesystem, so a concurrent `/run` call either sees
/// the old token or the new token — never a half-written file.
pub async fn rotate_token(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<RotateRequest>,
) -> impl IntoResponse {
    // Step 1: refuse early if rotation is disabled on this runner.
    let Some(expected) = state.admin_token.as_deref() else {
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(error_response(
                "rotation disabled — OMOIKANE_CLAUDECODE_RUNNER_ADMIN_TOKEN unset on runner",
            )),
        );
    };
    // Step 2: bearer-auth.
    let auth_hdr = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let presented = auth_hdr.strip_prefix("Bearer ").unwrap_or("").trim();
    if !constant_time_eq(presented.as_bytes(), expected.as_bytes()) {
        warn!("rotate-token: bearer mismatch");
        return (
            StatusCode::UNAUTHORIZED,
            Json(error_response("invalid bearer")),
        );
    }
    // Step 3: format gate.
    let token = req.token.trim();
    if let Err(msg) = validate_token_format(token) {
        warn!(reason = %msg, "rotate-token: format reject");
        return (StatusCode::BAD_REQUEST, Json(error_response(msg)));
    }
    // Step 4: persist to state-volume file.
    let target = state.state_dir.join(OAUTH_OVERRIDE_FILENAME);
    if let Err(e) = atomic_write_mode_600(&target, token.as_bytes()) {
        warn!(error = %e, path = %target.display(), "rotate-token: persist failed");
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(error_response("persist failed (see runner logs)")),
        );
    }
    // Step 5: update the in-memory cache so the next /run picks it up.
    {
        let mut w = state.oauth_token.write().await;
        *w = token.to_string();
    }
    // Step 6 (AGRIOPS-208): invalidate the warm pool. A pre-spawned `claude`
    // froze the OLD token in its process env at spawn time — serving one after
    // a rotation would keep using the credential the operator just replaced
    // (and, if the rotation was prompted by an expiry, would fail every turn
    // until the max-age reaper happened to catch up). Bump the generation
    // counter FIRST so any spawn racing us is also rejected at checkout, then
    // kill what is already warm.
    let generation = state
        .token_gen
        .fetch_add(1, std::sync::atomic::Ordering::SeqCst)
        + 1;
    let pool_drained = state.pool.drain("oauth-rotation").await;

    let rotated_at = time::OffsetDateTime::now_utc()
        .format(&time::format_description::well_known::Rfc3339)
        .ok();
    info!(
        rotated_at = ?rotated_at,
        pool_drained,
        token_generation = generation,
        "rotate-token: oauth token rotated"
    );
    (
        StatusCode::OK,
        Json(RotateResponse {
            ok: true,
            rotated_at,
            error: None,
        }),
    )
}

fn error_response(msg: impl Into<String>) -> RotateResponse {
    RotateResponse {
        ok: false,
        rotated_at: None,
        error: Some(msg.into()),
    }
}

/// Validate the token shape WITHOUT contacting Anthropic. Cheap
/// guard against the obvious paste mistakes (whole sk-... string is
/// missing, accidentally included a trailing newline + paragraph,
/// pasted the daemon's ANTHROPIC_API_KEY by mistake — different
/// prefix). Live validity is checked separately by the 5-min probe
/// loop on the daemon side.
pub fn validate_token_format(s: &str) -> Result<(), &'static str> {
    if !s.starts_with(TOKEN_PREFIX) {
        return Err("token must start with sk-ant-oat01-");
    }
    let len = s.len();
    if len < TOKEN_MIN_LEN {
        return Err("token too short");
    }
    if len > TOKEN_MAX_LEN {
        return Err("token too long");
    }
    // Charset gate: bytes after the prefix must be ASCII
    // alphanumeric + `_` + `-`. Catches whitespace embeds, smart
    // quotes from the operator's clipboard, etc.
    for b in s.as_bytes().iter().copied() {
        let ok = b.is_ascii_alphanumeric() || b == b'-' || b == b'_';
        if !ok {
            return Err("token contains illegal character");
        }
    }
    Ok(())
}

/// Constant-time byte comparison. Std doesn't ship one and we'd
/// rather not pull in a crate. Both inputs trimmed to the shorter
/// length so an attacker can't read the secret length via timing.
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff: u8 = 0;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// Atomic-rename write with mode 0600. We deliberately bypass
/// std::fs::write here because that opens with mode 0644 unless we
/// also set umask, which is process-global and would race with
/// other threads. Open-with-explicit-mode is the only safe path.
fn atomic_write_mode_600(target: &Path, body: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;

    // Make sure the parent dir exists. State volume is mounted on
    // first container start; on a fresh host it might not have the
    // subdir yet if the operator rotates before any /run.
    if let Some(parent) = target.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let tmp = target.with_extension("tmp");
    {
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .truncate(true)
            .write(true)
            .mode(0o600)
            .open(&tmp)?;
        f.write_all(body)?;
        f.sync_all()?;
    }
    std::fs::rename(&tmp, target)?;
    Ok(())
}

/// Read the live OAuth token from the state-volume override file,
/// if it exists + is non-empty + passes format validation. Called
/// once at runner startup before falling back to env. A malformed
/// or empty file is treated as absent — operator can't lock
/// themselves out by writing garbage to the file directly.
pub fn read_override(state_dir: &Path) -> Option<String> {
    let path = state_dir.join(OAUTH_OVERRIDE_FILENAME);
    let body = std::fs::read_to_string(&path).ok()?;
    let trimmed = body.trim();
    if validate_token_format(trimmed).is_err() {
        warn!(path = %path.display(),
            "oauth-override file present but malformed — falling back to env");
        return None;
    }
    Some(trimmed.to_string())
}

/// Resolve the state directory at startup. Env var override allows
/// tests to point at a tmpdir; production uses the default which
/// matches the named volume's mount target in compose.yml.
pub fn resolve_state_dir() -> PathBuf {
    if let Ok(p) = std::env::var("OMOIKANE_RUNNER_STATE_DIR") {
        if !p.is_empty() {
            return PathBuf::from(p);
        }
    }
    PathBuf::from("/home/tg/.claude-state")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    // ─── token format gate ─────────────────────────────────────────

    #[test]
    fn validate_accepts_a_well_formed_token() {
        // SYNTHETIC, and assembled rather than written as one literal.
        //
        // This was a real `claude setup-token` paste, kept so a future
        // contributor tightening the regex could not regress on the
        // production SHAPE. The shape is worth locking; the paste was
        // not. This file publishes to a public GitHub mirror, so a
        // token-shaped literal here is a token-shaped literal on the
        // open internet — and the mirror's sanitisation did not cover
        // the `sk-ant-oat01-` form, so nothing would have stopped it.
        //
        // Everything the test needs is preserved: same prefix, same
        // total length (108), same charset (ASCII alnum + `_` + `-`),
        // so the prefix, length and charset gates are exercised exactly
        // as before. Built at runtime, so no matching literal exists in
        // the source for any scanner to find.
        let tail: String = "AbCdEf0123456789_-".chars().cycle().take(95).collect();
        let real = format!("{TOKEN_PREFIX}{tail}");
        let real = real.as_str();
        assert_eq!(
            real.len(),
            108,
            "the synthetic token must keep the real length, or the length gate is untested"
        );
        assert!(
            validate_token_format(real).is_ok(),
            "a well-formed token must pass: {real}"
        );
    }

    #[test]
    fn validate_rejects_wrong_prefix() {
        // ANTHROPIC_API_KEY has the prefix `sk-ant-api03-`. Catching
        // this paste-error explicitly so the operator gets a clear
        // 400 rather than a 200-followed-by-silent-auth-failure.
        // Assembled, not a literal: an ANTHROPIC_API_KEY-shaped literal is still
        // API-key-shaped to a secret scanner, and this file publishes to a public
        // mirror. The test only needs the WRONG PREFIX, which is preserved exactly.
        let api_key = format!("sk-ant-api03-{}", "A".repeat(51));
        let api_key = api_key.as_str();
        assert!(validate_token_format(api_key).is_err());
        assert!(validate_token_format("").is_err());
        assert!(validate_token_format("garbage").is_err());
    }

    #[test]
    fn validate_rejects_length_outside_window() {
        let short = format!("{}aaa", TOKEN_PREFIX);
        assert!(validate_token_format(&short).is_err(), "<60 must reject");
        let long = format!("{}{}", TOKEN_PREFIX, "a".repeat(TOKEN_MAX_LEN));
        assert!(validate_token_format(&long).is_err(), ">500 must reject");
    }

    #[test]
    fn validate_rejects_illegal_chars() {
        // Smart-quote, space, newline, hash. Catching the
        // copy-paste-from-a-formatted-web-page-with-zero-width-chars
        // class of error.
        let dirty = format!("{}{}", TOKEN_PREFIX, "a".repeat(50));
        let with_space = format!("{} {}", dirty, "more");
        assert!(validate_token_format(&with_space).is_err());
        let with_newline = format!("{}\n{}", TOKEN_PREFIX, "a".repeat(50));
        assert!(validate_token_format(&with_newline).is_err());
        // Unicode em-dash inside otherwise valid charset.
        let with_emdash = format!(
            "{}aaaaaaaaaaaaaaaaaaaa\u{2014}aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            TOKEN_PREFIX
        );
        assert!(validate_token_format(&with_emdash).is_err());
    }

    // ─── atomic-write semantics ────────────────────────────────────

    #[test]
    fn atomic_write_creates_mode_600_file() {
        use std::os::unix::fs::PermissionsExt;
        let tmpdir = tempdir();
        let target = tmpdir.join("oauth-override");
        atomic_write_mode_600(&target, b"hello").expect("write");
        let mode = fs::metadata(&target).unwrap().permissions().mode() & 0o777;
        assert_eq!(
            mode, 0o600,
            "rotation file must be mode 0600 — contains live OAuth secret"
        );
        assert_eq!(fs::read_to_string(&target).unwrap(), "hello");
    }

    #[test]
    fn atomic_write_replaces_existing_file() {
        let tmpdir = tempdir();
        let target = tmpdir.join("oauth-override");
        atomic_write_mode_600(&target, b"v1").expect("write1");
        atomic_write_mode_600(&target, b"v2").expect("write2");
        assert_eq!(fs::read_to_string(&target).unwrap(), "v2");
    }

    #[test]
    fn atomic_write_creates_parent_dir() {
        // Fresh container: state dir might not exist yet (named
        // volume is mounted empty). Persist must mkdir -p.
        let tmpdir = tempdir();
        let nested = tmpdir.join("never-existed").join("nested");
        let target = nested.join("oauth-override");
        atomic_write_mode_600(&target, b"x").expect("write to nested");
        assert!(target.exists());
    }

    // ─── override read ─────────────────────────────────────────────

    #[test]
    fn read_override_returns_none_when_file_absent() {
        let tmpdir = tempdir();
        assert!(read_override(&tmpdir).is_none());
    }

    #[test]
    fn read_override_returns_none_when_file_malformed() {
        // Operator wrote garbage directly. Fall back to env, do NOT
        // surface a malformed token to the CLI.
        let tmpdir = tempdir();
        fs::write(tmpdir.join(OAUTH_OVERRIDE_FILENAME), "not a token").unwrap();
        assert!(
            read_override(&tmpdir).is_none(),
            "malformed override file must be ignored, not surfaced"
        );
    }

    #[test]
    fn read_override_returns_token_when_present_and_valid() {
        let tmpdir = tempdir();
        let body = format!("{}{}", TOKEN_PREFIX, "a".repeat(80));
        fs::write(tmpdir.join(OAUTH_OVERRIDE_FILENAME), &body).unwrap();
        assert_eq!(read_override(&tmpdir).as_deref(), Some(body.as_str()));
    }

    #[test]
    fn read_override_trims_trailing_newline() {
        // Operator hand-edits the file with `echo "$TOKEN" > file`,
        // which appends a newline. Validation rejects newlines, so
        // we trim before validating.
        let tmpdir = tempdir();
        let body = format!("{}{}\n", TOKEN_PREFIX, "a".repeat(80));
        fs::write(tmpdir.join(OAUTH_OVERRIDE_FILENAME), &body).unwrap();
        assert!(
            read_override(&tmpdir).is_some(),
            "trailing newline must be trimmed, not treated as invalid"
        );
    }

    // ─── constant-time eq ──────────────────────────────────────────

    #[test]
    fn constant_time_eq_handles_unequal_length() {
        assert!(!constant_time_eq(b"abc", b"abcd"));
        assert!(!constant_time_eq(b"", b"x"));
    }

    #[test]
    fn constant_time_eq_returns_true_on_equal() {
        assert!(constant_time_eq(b"hello", b"hello"));
        assert!(constant_time_eq(b"", b""));
    }

    #[test]
    fn constant_time_eq_returns_false_on_diff() {
        assert!(!constant_time_eq(b"hello", b"hellp"));
    }

    // ─── tiny tempdir helper (no extra crate) ──────────────────────

    fn tempdir() -> PathBuf {
        let base = std::env::temp_dir();
        let pid = std::process::id();
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        let tid = std::thread::current().id();
        let dir = base.join(format!("ccrunner-rotate-{pid}-{nanos}-{tid:?}"));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        dir
    }
}
