package knowledge

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// ParseCorpus reads a JSON array of prior Incident records — the knowledge corpus the retriever ranks over.
// It is the config-not-code feed (an operator-exported incident history) until a knowledge store feeds the
// retriever automatically. An entry with no external_ref is rejected loudly (a corpus row with no identity
// cannot be cited as precedent), never silently dropped.
func ParseCorpus(r io.Reader) ([]Incident, error) {
	var corpus []Incident
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&corpus); err != nil {
		return nil, fmt.Errorf("knowledge: malformed corpus JSON: %w", err)
	}
	for i, inc := range corpus {
		if strings.TrimSpace(inc.ExternalRef) == "" {
			return nil, fmt.Errorf("knowledge: corpus entry %d has no external_ref (cannot be cited as precedent)", i)
		}
	}
	return corpus, nil
}

// MergeCorpus merges new incidents into an existing corpus, deduplicated by ExternalRef.
// Deterministically ordered by ExternalRef so the serialized corpus is stable and diff-friendly. This is
// the write-side of the lessons loop: a resolved incident distilled by core/lessons is merged into the
// corpus the retriever reloads.
//
// LAST WRITE WINS, EXCEPT DOWNHILL (TG-172 item 1). The newer record updates precedent — that is the point,
// a re-resolved incident should supersede its own earlier lesson — but a write from a LESS verified source
// may not overwrite a more verified one for the same ExternalRef.
//
// This function is the single write primitive for the corpus and it has five callers: the lessons sink, the
// AWX runbook ingest, the compiled wiki, the worker's own reload, and the offline predecessor seeder. Only
// the lessons sink passes through a verification gate (lessons.Lesson: clean mechanical verdict AND confirmed clear).
// Without the rule below, any of the other four could silently replace a verified resolution with an
// unverified row carrying the same ref, and nothing downstream would show a difference — the corpus is
// re-serialized, reloaded, and retrieved with the substituted text intact. That is a one-hop
// trust-laundering path into the precedent block, and it is the concrete shape behind TG-172's
// "last-writer-wins with no provenance check".
//
// It is deliberately NOT a refusal. A downhill write is DROPPED for that ref and the merge continues, for
// the same reason retrieval drops rather than refuses: the corpus is optional enrichment, and failing a
// whole import because one row lost a precedence contest would make the safest configuration the one that
// stops learning. Equal rank still lets the newer record win, so the ordinary re-resolution case is
// unchanged.
func MergeCorpus(existing, added []Incident) []Incident {
	byRef := make(map[string]Incident, len(existing)+len(added))
	order := make([]string, 0, len(existing)+len(added))
	seen := map[string]struct{}{}
	remember := func(inc Incident) {
		ref := strings.TrimSpace(inc.ExternalRef)
		if ref == "" {
			return
		}
		if _, ok := seen[ref]; !ok {
			seen[ref] = struct{}{}
			order = append(order, ref)
		}
		if prev, ok := byRef[ref]; ok && inc.Source.rank() < prev.Source.rank() {
			return // downhill: an unverified row may not displace a verified one under the same ref
		}
		byRef[ref] = inc // last write wins → the newer record updates precedent
	}
	for _, inc := range existing {
		remember(inc)
	}
	for _, inc := range added {
		remember(inc)
	}
	sort.Strings(order)
	out := make([]Incident, 0, len(order))
	for _, ref := range order {
		out = append(out, byRef[ref])
	}
	return out
}

// MarshalJSON omits resolved_at when it is UNKNOWN (TG-341).
//
// `json:"resolved_at,omitempty"` on the struct is a SILENT NO-OP: encoding/json omits empty strings, zero
// numbers, false, nil pointers/interfaces and empty maps/slices/arrays — a zero time.Time is a non-empty
// STRUCT, so it was always emitted. Every corpus re-serialized through WriteCorpus therefore gained
// `"resolved_at": "0001-01-01T00:00:00Z"` on every undated row: 140/140 of the committed
// deploy/knowledge/corpus.seed.json rows, measured while stamping provenance for TG-172.
//
// Nothing downstream was misled — ParseCorpus reads it back to the zero time, recencyScore contributes
// nothing for zero, and stalenessNote renders "[age unknown …]", all correct. What it cost is the
// legibility of a committed, human-reviewed artifact: 0001-01-01 reads to a person as a CORRUPT date
// rather than an ABSENT one, and the docs/contracts JSON advertised the field as omittable when it never
// was. A file people review must not lie about which of its facts are known.
//
// The tag is left in place: it is what makes the field omittable on the pointer below, and removing it
// would make this method the only thing standing between the corpus and the fake date.
func (i Incident) MarshalJSON() ([]byte, error) {
	// `alias` sheds Incident's method set, so marshalling it does not re-enter this method.
	type alias Incident
	if !i.ResolvedAt.IsZero() {
		return json.Marshal(alias(i))
	}
	// The outer ResolvedAt shadows the embedded one (depth 0 beats depth 1), and a nil pointer with
	// omitempty is actually omitted — which is what the tag promised all along.
	return json.Marshal(struct {
		alias
		ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	}{alias: alias(i)})
}

// WriteCorpus serializes a corpus as the JSON array ParseCorpus reads back — a round-trippable, stable form.
func WriteCorpus(w io.Writer, corpus []Incident) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(corpus); err != nil {
		return fmt.Errorf("knowledge: write corpus: %w", err)
	}
	return nil
}

// WriteCorpusFile writes the corpus to `path` ATOMICALLY: a temp file (path + ".tmp") is written via
// WriteCorpus, then renamed over path, so a concurrent reader (the retriever's reload loop) never observes a
// torn corpus. It is the single write primitive extracted from the worker's five inline temp+rename blocks
// (TG-510): the maintained-corpus write path routes through it AND the caller records a tamper-evidence
// witness on top, so the write and its witness share one chokepoint. On any failure the temp file is
// removed and path is left untouched (the atomic-rename never happened), matching the prior inline behaviour
// byte-for-byte. It performs NO anchoring itself — that is layered by the caller — so this stays a pure
// file primitive, and a flag-off caller gets exactly the old bytes and the old on-disk effect.
func WriteCorpusFile(path string, corpus []Incident) error {
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("knowledge: write corpus temp %s: %w", tmp, err)
	}
	if werr := WriteCorpus(out, corpus); werr != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("knowledge: serialize corpus: %w", werr)
	}
	out.Close()
	if rerr := os.Rename(tmp, path); rerr != nil {
		os.Remove(tmp)
		return fmt.Errorf("knowledge: replace corpus %s: %w", path, rerr)
	}
	return nil
}
