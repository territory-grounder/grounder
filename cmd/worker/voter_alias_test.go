package main

// TG-463 drills — the voter-alias normalizer. The property under drill is NEVER-WIDEN: normalization may
// only rewrite a presented identity into the canonical spelling the frozen approve_by entries carry; it
// must never invent an identity, never empty one, and an ambiguous table must refuse in full.
//
// EXECUTED KILLING MUTATIONS (2026-08-15, each witnessed red then restored green):
//   1. matrix_vote_lane.go handle(): the normalize call dropped (the raw MXID signals — the exact live
//      failure the TG-496 approve-drill hit) → TestMatrixLaneSignalsTheCanonicalVoter red.
//   2. voter_alias.go normalizeVoter(): the unknown-alias passthrough replaced with returning the first
//      canonical (an inventing normalizer — the widen direction) → the stranger arm of
//      TestNormalizeVoterNeverInvents red.

import (
	"context"
	"encoding/json"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
)

func TestParseVoterAliases(t *testing.T) {
	t.Run("valid pairs parse case-insensitively", func(t *testing.T) {
		m := parseVoterAliases("@Kyr:Example.net=kyriakos, ops-bot=svc-ops")
		if len(m) != 2 || m["@kyr:example.net"] != "kyriakos" || m["ops-bot"] != "svc-ops" {
			t.Fatalf("parse: %v", m)
		}
	})
	t.Run("malformed pairs are skipped, the rest survive", func(t *testing.T) {
		m := parseVoterAliases("nonsense, =x, y=, @a:hs=alice")
		if len(m) != 1 || m["@a:hs"] != "alice" {
			t.Fatalf("only the well-formed pair may survive: %v", m)
		}
	})
	t.Run("a conflicting duplicate refuses the WHOLE table", func(t *testing.T) {
		if m := parseVoterAliases("@a:hs=alice,@A:HS=mallory"); m != nil {
			t.Fatalf("an ambiguous identity table on an authorization surface must refuse in full, got %v", m)
		}
	})
	t.Run("empty is nil", func(t *testing.T) {
		if parseVoterAliases("  ") != nil {
			t.Fatal("blank env must disable, not allocate")
		}
	})
}

func TestNormalizeVoterNeverInvents(t *testing.T) {
	m := parseVoterAliases("@kyr:hs=kyriakos")
	if got := normalizeVoter(m, "@KYR:hs"); got != "kyriakos" {
		t.Fatalf("a known alias must normalize (case-insensitive), got %q", got)
	}
	if got := normalizeVoter(m, "@stranger:hs"); got != "@stranger:hs" {
		t.Fatalf("an UNKNOWN identity must pass through UNCHANGED — the frozen set refuses it; an inventing "+
			"normalizer is the widen direction, got %q", got)
	}
	if got := normalizeVoter(m, ""); got != "" {
		t.Fatalf("an empty voter stays empty (VoterAdmitted refuses it), got %q", got)
	}
	if got := normalizeVoter(nil, "@kyr:hs"); got != "@kyr:hs" {
		t.Fatalf("no table ⇒ passthrough, got %q", got)
	}
}

// The lane-level oracle: a vote whose Sender is a configured alias reaches the workflow signal carrying
// the CANONICAL login — the exact seam the TG-496 approve-drill found dead (a raw chat identity
// string-compared against the frozen set as a stranger).
func TestMatrixLaneSignalsTheCanonicalVoter(t *testing.T) {
	aliases := parseVoterAliases("@kyriakos:hs=kyriakos")
	var gotVoter string
	lane := &matrixVoteLane{
		resolve: func(_ context.Context, _ []byte) (notifier.Vote, error) {
			return notifier.Vote{DecisionID: "TG-463-e2e", Approve: true, Sender: "@kyriakos:hs"}, nil
		},
		actionFor: func(_ context.Context, id string) (string, bool) { return "act-1", id == "TG-463-e2e" },
		signal: func(_ context.Context, _, _ string, _ bool, voter string) error {
			gotVoter = voter
			return nil
		},
		normalize: func(p string) string { return normalizeVoter(aliases, p) },
	}
	if out := lane.handle(context.Background(), json.RawMessage(`{}`)); out != voteDelivered {
		t.Fatalf("the vote must deliver, got %v", out)
	}
	if gotVoter != "kyriakos" {
		t.Fatalf("the signal must carry the CANONICAL login the frozen approve_by holds, got %q", gotVoter)
	}

	// The stranger arm rides the same lane: an unknown Sender signals UNCHANGED (the frozen set refuses).
	lane.resolve = func(_ context.Context, _ []byte) (notifier.Vote, error) {
		return notifier.Vote{DecisionID: "TG-463-e2e", Approve: true, Sender: "@mallory:hs"}, nil
	}
	if out := lane.handle(context.Background(), json.RawMessage(`{}`)); out != voteDelivered {
		t.Fatalf("the stranger's vote still signals (refusal is the WORKFLOW's frozen-set job), got %v", out)
	}
	if gotVoter != "@mallory:hs" {
		t.Fatalf("an unknown identity must reach the workflow unnormalized, got %q", gotVoter)
	}
}
