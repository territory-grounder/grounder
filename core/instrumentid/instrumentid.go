// Package instrumentid names the INSTRUMENT a triage session ran under — every configured component that can
// move what the session decides or how it is graded — as one hex SHA-256 the session row carries (TG-672, F23;
// spec/025 REQ-2535).
//
// WHY A SECOND IDENTITY BESIDE harness_fp. The harness fingerprint (TG-575) names the system half of the
// prompt: protocol prose, tool catalog, op-class catalog with its ratified overlay, and the composed guidance.
// It does not name the build that interprets the reply, the policy bundle that bands the proposal, the model the
// aliases resolve to, the rubric that grades the record, or the learned state that feeds prediction and
// grounding. The G3 soak cohort of 2026-11-06/07 carried ONE harness fingerprint over 140 sessions while
// eighteen behaviour-changing deploys landed inside it; the fingerprint said "same instrument" and it was not.
//
// WHAT IS HASHED, AND WHAT IS ONLY DISCLOSED. The identity hashes the CONFIGURED components — build SHA,
// harness fingerprint, op-schema catalog hash, policy bundle version, model alias map, rubric version, production
// skill set, knowledge corpus head: what an operator deployed, ratified or promoted. It does NOT hash the observed
// estate topology, and it does NOT hash the learner's co-occurrence state: both are the world the instrument
// measures (the learner moves on every ingested alert and every decay tick), and hashing either would make
// "exactly one identity across a cohort" unreachable by construction while recording nothing an operator chose.
// The report discloses them beside the identity instead — how many topology snapshots were published inside the
// window, that learned state evolved — as the uncertainty they are. Provider model VERSIONS behind an alias are
// not part of the identity either: the gateway reports them per call, the judge records its own
// (session_judgment.judge_model, TG-670), and the report labels the rest an uncertainty.
//
// UNRECORDED IS A VALUE. A component the process could not read hashes as the empty string and is NAMED in
// Unrecorded(); two sessions missing the same component share an identity whose row says what it lacks. Nothing
// here ever substitutes a guess for a missing component.
package instrumentid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Components are the instrument's configured parts. Every field is a content hash or a version string; "" means
// the process could not read it.
type Components struct {
	BuildSHA       string `json:"build_sha"`
	HarnessFP      string `json:"harness_fp"`
	OpSchemaHash   string `json:"opschema_hash"`
	PolicyBundle   string `json:"policy_bundle"`
	ModelAliasHash string `json:"model_alias_hash"`
	RubricVersion  string `json:"rubric_version"`
	SkillSetHash   string `json:"skill_set_hash"`
	KnowledgeHash  string `json:"knowledge_hash"`
}

// Fields is the hashed field list, in canonical order. The names are the row's column names.
var Fields = []string{"build_sha", "harness_fp", "opschema_hash", "policy_bundle", "model_alias_hash", "rubric_version",
	"skill_set_hash", "knowledge_hash"}

func (c Components) values() []string {
	return []string{c.BuildSHA, c.HarnessFP, c.OpSchemaHash, c.PolicyBundle, c.ModelAliasHash, c.RubricVersion,
		c.SkillSetHash, c.KnowledgeHash}
}

// Canonical is the text the identity hashes: one `name=value` line per field, in Fields order. A value is
// hashed as given — a component hash never re-hashed, a version string never normalised — so the identity is
// recomputable from the row by anyone.
func (c Components) Canonical() string {
	v := c.values()
	b := &strings.Builder{}
	for i, name := range Fields {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(v[i])
		b.WriteByte('\n')
	}
	return b.String()
}

// ID is the hex SHA-256 of Canonical().
func (c Components) ID() string {
	sum := sha256.Sum256([]byte(c.Canonical()))
	return hex.EncodeToString(sum[:])
}

// Unrecorded names the components that are empty, in Fields order.
func (c Components) Unrecorded() []string {
	var out []string
	for i, v := range c.values() {
		if v == "" {
			out = append(out, Fields[i])
		}
	}
	return out
}

// HashStrings is the content hash of a SET of strings: sorted, newline-joined, SHA-256 hex. Order-free so two
// processes reading the same set in different orders agree; an empty set hashes too (it is a recorded "none",
// not an unrecorded component).
func HashStrings(parts []string) string {
	s := append([]string(nil), parts...)
	sort.Strings(s)
	sum := sha256.Sum256([]byte(strings.Join(s, "\n")))
	return hex.EncodeToString(sum[:])
}

// Record is what the identity store persists: the id and the components it was derived from.
type Record struct {
	ID string
	Components
}

// Source supplies the process-level components (policy bundle, model aliases, skills, seed knowledge) at
// session time. It is fail-soft by contract: a component it cannot read is "" and the session proceeds.
type Source interface {
	Components(ctx context.Context) Components
}

// Sink persists an identity's components once (idempotent on ID).
type Sink interface {
	Ensure(ctx context.Context, rec Record) error
}
