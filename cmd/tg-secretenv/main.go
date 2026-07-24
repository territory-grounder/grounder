// Command tg-secretenv resolves a set of SecretRef references from a secret BACKEND (OpenBao/Vault via the
// bao: scheme, or store:/file:) and writes them as `ENVVAR=value` lines to an output file — the TG-native,
// dependency-free equivalent of a Vault-Agent template. It is the deploy-time mechanism that lets a
// THIRD-PARTY container (e.g. the LiteLLM model gateway, a fixed upstream image that reads its keys straight
// from process env) receive its secrets from the vault WITHOUT a plaintext value ever living in .env
// (spec/024 REQ-2403 for the other-container secrets — the case the in-process boot gate cannot cover).
//
// Contract: each argument is `ENVVAR=<secret-ref>` (the ref is env:/file:/store:/bao:/…). The tool wires the
// OpenBao delivery client from TG_OPENBAO_ADDR/_TOKEN_REF/_CA (the same env the worker/grounder use), resolves
// every ref, and writes the results to -out (default stdout) as `ENVVAR=<resolved-value>` lines suitable for
// `set -a; . <file>` or an env_file. It is FAIL-CLOSED: if ANY ref does not resolve, it writes nothing and
// exits non-zero — the container must not start with a partial/blank secret set. The output file is created
// 0600 (owner-only); a resolved value is NEVER logged (only the ENVVAR name + a redacted reason on failure).
//
// Provenance: [F] owner directive (no plaintext at rest, other-container secrets) · [O] INV-13, spec/024.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

func main() {
	out := flag.String("out", "", "output file for the ENVVAR=value lines (0600); empty ⇒ stdout")
	shell := flag.Bool("shell", false, "emit single-quoted `export ENVVAR='value'` lines for safe `. <file>` sourcing "+
		"(default: bare ENVVAR=value dotenv/env_file lines)")
	skipEmpty := flag.Bool("skip-empty", false, "skip ENVVAR= pairs whose ref is empty (an unconfigured optional "+
		"secret, e.g. a provider key whose ${TG_X_KEY_REF:-} default expanded to nothing) instead of failing")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("tg-secretenv: no ENVVAR=<secret-ref> arguments given")
	}

	// Wire the OpenBao delivery so bao: references resolve (the same substrate the worker/grounder use). An
	// unset address is a no-op: bao: refs then fail closed and env:/file:/store: still resolve.
	if err := openbao.WireDelivery(os.Getenv("TG_OPENBAO_ADDR"), envOr("TG_OPENBAO_TOKEN_REF", ""), os.Getenv("TG_OPENBAO_CA"), log.Printf); err != nil {
		log.Fatalf("tg-secretenv: wire secret delivery: %v", err)
	}

	names, err := run(flag.Args(), *out, *shell, *skipEmpty, func(ref string) (string, error) { return config.SecretRef(ref).Resolve() })
	if err != nil {
		log.Fatalf("tg-secretenv: %v", err)
	}
	// Report the NAMES only (never values, never the resolver error text), so a deploy log confirms coverage.
	log.Printf("tg-secretenv: resolved %d secret(s) from the backend: %s", len(names), strings.Join(names, ", "))
}

// run parses the ENVVAR=<ref> args, resolves every ref via `resolve`, and writes the results to outPath (or
// stdout when empty). It is the testable core: `resolve` is injected so a test can drive success + failure
// without a live backend. FAIL-CLOSED and LEAK-SAFE: it resolves ALL refs BEFORE writing anything, so a
// failure leaves NOTHING written; and a failure names only the ENVVAR + the ref's SCHEME (never the ref
// value, never the resolver's error text — the underlying error can name a path/field, and though the
// SecretRef resolvers never echo a resolved value on an error path, this tool declines to forward that text
// at all rather than depend on that contract). Returns the resolved ENVVAR names (sorted) on success.
func run(args []string, outPath string, shell, skipEmpty bool, resolve func(ref string) (string, error)) ([]string, error) {
	pairs, err := parseArgs(args, skipEmpty)
	if err != nil {
		return nil, err
	}
	resolved := make(map[string]string, len(pairs))
	for _, p := range pairs {
		v, rerr := resolve(p.ref)
		if rerr != nil {
			// Leak-safe diagnostic: the ENVVAR + the ref scheme (env/file/store/bao — derived from the ref, not
			// the value) is enough for a deploy operator to fix it, and cannot carry secret material.
			return nil, fmt.Errorf("%s did not resolve from the %q backend (fail closed, nothing written)", p.envVar, config.SchemeOf(config.SecretRef(p.ref)))
		}
		resolved[p.envVar] = v
	}
	if err := writeEnv(outPath, pairs, resolved, shell); err != nil {
		return nil, fmt.Errorf("write output: %w", err) // an os/path error, never a secret value
	}
	names := make([]string, 0, len(resolved))
	for k := range resolved {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

type pair struct {
	envVar string
	ref    string
}

// parseArgs parses `ENVVAR=<secret-ref>` arguments, preserving order and rejecting a malformed pair or an
// invalid env var name (fail closed — a typo must not silently drop a secret).
func parseArgs(args []string, skipEmpty bool) ([]pair, error) {
	var out []pair
	seen := map[string]bool{}
	for _, a := range args {
		k, ref, ok := strings.Cut(a, "=")
		k = strings.TrimSpace(k)
		ref = strings.TrimSpace(ref) // a secret ref never carries meaningful surrounding whitespace; trim once so
		//                              the empty-check and the STORED ref agree (no " bao:x " reaching the resolver)
		if !ok || k == "" {
			return nil, fmt.Errorf("argument %q is not ENVVAR=<secret-ref>", a)
		}
		// Validate the name FIRST so a malformed arg (bad name) errors even under -skip-empty — skipping is only
		// for a WELL-FORMED ENVVAR= with an empty ref, never a cover for a typo'd variable name.
		if !validEnvName(k) {
			return nil, fmt.Errorf("invalid env var name %q (letters, digits, underscore; not leading digit)", k)
		}
		if ref == "" {
			// An empty ref is an UNCONFIGURED optional secret (e.g. a provider key whose install-time
			// `${TG_X_KEY_REF:-}` default expanded to nothing). Under -skip-empty that is not an error — the
			// secret is simply not delivered; without it, an empty ref stays a hard error (a config typo).
			if skipEmpty {
				continue
			}
			return nil, fmt.Errorf("argument %q is not ENVVAR=<secret-ref>", a)
		}
		if seen[k] {
			return nil, fmt.Errorf("env var %q given more than once", k)
		}
		seen[k] = true
		out = append(out, pair{envVar: k, ref: ref})
	}
	return out, nil
}

// writeEnv writes the resolved pairs as `ENVVAR=value` lines, in the argument order, to path (0600) or
// stdout. Values are written verbatim (a secret may contain any byte except a newline, which a shell env
// value cannot carry anyway); a value containing a newline is rejected fail-closed rather than truncated.
func writeEnv(path string, pairs []pair, resolved map[string]string, shell bool) error {
	var b strings.Builder
	for _, p := range pairs {
		v := resolved[p.envVar]
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("resolved value for %s contains a newline (cannot be a shell env value)", p.envVar)
		}
		if shell {
			// `export NAME='value'` with POSIX single-quote escaping (' → '\'') so ANY value round-trips through
			// `set -a; . <file>` verbatim — no word-splitting, glob, or $-expansion on special characters.
			b.WriteString("export ")
			b.WriteString(p.envVar)
			b.WriteString("='")
			b.WriteString(strings.ReplaceAll(v, "'", `'\''`))
			b.WriteString("'\n")
			continue
		}
		b.WriteString(p.envVar)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if path == "" {
		_, err := os.Stdout.WriteString(b.String())
		return err
	}
	// 0600: the env file carries plaintext secret values at rest ONLY on a memory-backed path the operator
	// chooses (tmpfs) — owner-only, and short-lived by deployment convention.
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func validEnvName(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
