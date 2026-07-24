package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/credential"
)

// ErrMalformedRule is the typed fail-closed error every load-time validation returns (REQ-1503, INV-09): a
// malformed rule NEVER loads as a permissive default — the whole rule set is refused so the engine keeps its
// prior known-good policy rather than falling open. Callers can errors.Is against it.
var ErrMalformedRule = errors.New("policy: malformed rule")

// RuleSet is a parsed, validated operator policy: an ordered rule list plus the global-default Params that
// unset rule fields inherit from (REQ-1507). It is the DATA the fixed Rego evaluator consumes — there is no
// Rego anywhere in it.
type RuleSet struct {
	Default Params
	Rules   []Rule
}

// EffectiveParams resolves r's params against the set's global default and the hard DefaultMinConfidence
// floor (REQ-1507): each field unset on r inherits from RuleSet.Default; a min_confidence still unset after
// that falls back to DefaultMinConfidence (0.60). The enforcement of the resolved values is a later leaf
// (T-015-3); this method is the inheritance resolution the scenario "an unset rule param inherits from the
// global-default rule" drives.
func (rs RuleSet) EffectiveParams(r Rule) Params {
	p := r.Params.inherit(rs.Default)
	if p.MinConfidence == nil {
		def := DefaultMinConfidence
		p.MinConfidence = &def
	}
	return p
}

// ---------------------------------------------------------------------------------------------------------
// JSON schema (wire form). Operators supply policy as this JSON DATA; ParseRuleSet validates it fail-closed.
// ---------------------------------------------------------------------------------------------------------

// Schema is the human-readable JSON Schema (draft-07) for an operator policy document. It is exported so the
// console policy surface (T-015-12) and operators can validate a document before submission; the AUTHORITY
// on acceptance is ParseRuleSet, which additionally enforces cross-field rules (single estate selector, at
// least one match dimension, closed enums) that JSON Schema alone cannot.
const Schema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "Territory Grounder operator policy (rules-as-data, spec/015 REQ-1505)",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "default": { "$ref": "#/definitions/params" },
    "rules": { "type": "array", "items": { "$ref": "#/definitions/rule" } }
  },
  "definitions": {
    "params": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "min_confidence": { "type": "number", "minimum": 0, "maximum": 1 },
        "band_mode": { "enum": ["respect", "force"] },
        "rate_limit": { "type": "integer", "minimum": 0 }
      }
    },
    "match": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "host": { "type": "string" },
        "host_glob": { "type": "string" },
        "group": { "type": "string" },
        "device_class": { "type": "string" },
        "resource": { "type": "string" },
        "op_class": { "type": "string" },
        "argv_pattern": { "type": "string" },
        "territory": { "type": "string" },
        "reversible": { "type": "boolean" }
      }
    },
    "rule": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "verdict"],
      "properties": {
        "id": { "type": "string", "minLength": 1 },
        "match": { "$ref": "#/definitions/match" },
        "verdict": { "enum": ["auto", "approve", "deny"] },
        "params": { "$ref": "#/definitions/params" },
        "approve_by": { "type": "array", "items": { "type": "string" } },
        "is_default": { "type": "boolean" }
      }
    }
  }
}`

type paramsDoc struct {
	MinConfidence *float64 `json:"min_confidence,omitempty"`
	BandMode      string   `json:"band_mode,omitempty"`
	RateLimit     *int     `json:"rate_limit,omitempty"`
}

type matchDoc struct {
	Host        string `json:"host,omitempty"`
	HostGlob    string `json:"host_glob,omitempty"`
	Group       string `json:"group,omitempty"`
	DeviceClass string `json:"device_class,omitempty"`
	Resource    string `json:"resource,omitempty"`
	OpClass     string `json:"op_class,omitempty"`
	ArgvPattern string `json:"argv_pattern,omitempty"`
	Territory   string `json:"territory,omitempty"`
	Reversible  *bool  `json:"reversible,omitempty"`
}

type ruleDoc struct {
	ID        string     `json:"id"`
	Match     matchDoc   `json:"match"`
	Verdict   string     `json:"verdict"`
	Params    *paramsDoc `json:"params,omitempty"`
	ApproveBy []string   `json:"approve_by,omitempty"`
	IsDefault bool       `json:"is_default,omitempty"`
}

type ruleSetDoc struct {
	Default *paramsDoc `json:"default,omitempty"`
	Rules   []ruleDoc  `json:"rules"`
}

// ParseRuleSet parses and validates an operator policy JSON document into a RuleSet, FAILING CLOSED on any
// malformation (REQ-1503, INV-09): an unknown key, an unknown verdict or band_mode (closed-enum violation),
// an out-of-range min_confidence or negative rate_limit, more than one estate selector on a match, or a
// non-default rule with no match dimension each return ErrMalformedRule and NO partial RuleSet. It never
// coerces a bad rule into a permissive default.
func ParseRuleSet(data []byte) (RuleSet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // an unexpected field is a malformed rule, not a silently-ignored one.

	var doc ruleSetDoc
	if err := dec.Decode(&doc); err != nil {
		return RuleSet{}, fmt.Errorf("%w: decode: %v", ErrMalformedRule, err)
	}

	def, err := parseParams(doc.Default)
	if err != nil {
		return RuleSet{}, fmt.Errorf("%w: default params: %v", ErrMalformedRule, err)
	}

	rs := RuleSet{Default: def, Rules: make([]Rule, 0, len(doc.Rules))}
	seen := map[string]bool{}
	for i, rd := range doc.Rules {
		r, err := ruleFromDoc(rd)
		if err != nil {
			return RuleSet{}, fmt.Errorf("%w: rule[%d] %q: %v", ErrMalformedRule, i, rd.ID, err)
		}
		if seen[r.ID] {
			return RuleSet{}, fmt.Errorf("%w: rule[%d]: duplicate rule id %q", ErrMalformedRule, i, r.ID)
		}
		seen[r.ID] = true
		rs.Rules = append(rs.Rules, r)
	}
	return rs, nil
}

// NewRule is the validating constructor for a single rule built in Go (not from JSON) — the console rule
// editor (T-015-12) and templates (T-015-7) use it. It applies the SAME closed-enum and match validation as
// ParseRuleSet and fails closed with ErrMalformedRule.
func NewRule(r Rule) (Rule, error) {
	if r.ID == "" {
		return Rule{}, fmt.Errorf("%w: empty rule id", ErrMalformedRule)
	}
	if !validVerdict(r.Verdict) {
		return Rule{}, fmt.Errorf("%w: rule %q: unknown verdict %q", ErrMalformedRule, r.ID, r.Verdict)
	}
	if !validBandMode(r.Params.BandMode) {
		return Rule{}, fmt.Errorf("%w: rule %q: unknown band_mode %q", ErrMalformedRule, r.ID, r.Params.BandMode)
	}
	if r.Params.MinConfidence != nil && (*r.Params.MinConfidence < 0 || *r.Params.MinConfidence > 1) {
		return Rule{}, fmt.Errorf("%w: rule %q: min_confidence %v out of [0,1]", ErrMalformedRule, r.ID, *r.Params.MinConfidence)
	}
	if r.Params.RateLimit != nil && *r.Params.RateLimit < 0 {
		return Rule{}, fmt.Errorf("%w: rule %q: negative rate_limit %d", ErrMalformedRule, r.ID, *r.Params.RateLimit)
	}
	// A non-default rule must constrain at least one dimension; a match that specifies nothing would match
	// every action, which is exactly the fail-open shape the engine refuses. The global-default rule is
	// exempt — it contributes params, not a verdict match.
	if !r.IsDefault && !r.Match.specifiesAny() {
		return Rule{}, fmt.Errorf("%w: rule %q: match specifies no dimension", ErrMalformedRule, r.ID)
	}
	return r, nil
}

// specifiesAny reports whether the match constrains at least one dimension.
func (m Match) specifiesAny() bool {
	return m.Selector != nil || m.OpClass != "" || m.ArgvPattern != "" || m.Territory != "" || m.Reversible != nil
}

func ruleFromDoc(rd ruleDoc) (Rule, error) {
	p, err := parseParams(rd.Params)
	if err != nil {
		return Rule{}, err
	}
	sel, err := selectorFromDoc(rd.Match)
	if err != nil {
		return Rule{}, err
	}
	r := Rule{
		ID: rd.ID,
		Match: Match{
			Selector:    sel,
			OpClass:     rd.Match.OpClass,
			ArgvPattern: rd.Match.ArgvPattern,
			Territory:   rd.Match.Territory,
			Reversible:  rd.Match.Reversible,
		},
		Verdict:   Verdict(rd.Verdict),
		Params:    p,
		ApproveBy: rd.ApproveBy,
		IsDefault: rd.IsDefault,
	}
	return NewRule(r) // one validation path for JSON- and Go-built rules.
}

// selectorFromDoc builds AT MOST one shared-object-model credential.Selector from the estate dimensions of a
// match doc. Specifying more than one estate dimension is ambiguous and fails closed (REQ-1605): the shared
// grammar carries exactly one Selector per rule.
func selectorFromDoc(md matchDoc) (*credential.Selector, error) {
	var (
		sel   *credential.Selector
		count int
	)
	set := func(kind credential.SelectorKind, pattern string) {
		count++
		s := credential.Selector{Kind: kind, Pattern: pattern}
		sel = &s
	}
	if md.Host != "" {
		set(credential.KindHost, md.Host)
	}
	if md.HostGlob != "" {
		set(credential.KindHostGlob, md.HostGlob)
	}
	if md.Group != "" {
		set(credential.KindGroup, md.Group)
	}
	if md.DeviceClass != "" {
		set(credential.KindDeviceClass, md.DeviceClass)
	}
	if md.Resource != "" {
		set(credential.KindResource, md.Resource)
	}
	if count > 1 {
		return nil, fmt.Errorf("match specifies %d estate selectors; at most one is allowed", count)
	}
	return sel, nil
}

func parseParams(pd *paramsDoc) (Params, error) {
	if pd == nil {
		return Params{}, nil
	}
	p := Params{
		MinConfidence: pd.MinConfidence,
		BandMode:      BandMode(pd.BandMode),
		RateLimit:     pd.RateLimit,
	}
	if !validBandMode(p.BandMode) {
		return Params{}, fmt.Errorf("unknown band_mode %q", pd.BandMode)
	}
	if p.MinConfidence != nil && (*p.MinConfidence < 0 || *p.MinConfidence > 1) {
		return Params{}, fmt.Errorf("min_confidence %v out of [0,1]", *p.MinConfidence)
	}
	if p.RateLimit != nil && *p.RateLimit < 0 {
		return Params{}, fmt.Errorf("negative rate_limit %d", *p.RateLimit)
	}
	return p, nil
}
