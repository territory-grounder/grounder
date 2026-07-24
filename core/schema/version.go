// Package schema is the canonical schema-version registry for Territory Grounder's governed rows.
//
// Provenance: [O] INV-15 (one generated source of truth per entity), INV-16 (integrity by
// construction; DATA-MODEL §1.3 CHECK schema_version > 0), spec/006 REQ-505 · [F] the predecessor
// scripts/lib/schema_version.py CURRENT_SCHEMA_VERSION map + check_row, re-expressed in typed Go.
//
// Every governed row carries the schema_version its writer stamped. A reader that decodes a row whose
// stored version EXCEEDS the reader's compiled version fails closed with a SchemaVersionError rather
// than mis-reading a shape it does not understand — a future writer never silently corrupts an older
// reader. The registry below is the single place a table's version is declared; a migration that
// changes a governed row's shape bumps that table's version here in the same change.
package schema

import "fmt"

// Version is a monotonically increasing schema version for a governed table. The zero value is
// INVALID: a stamped row must carry a version > 0 (enforced in Postgres by CHECK schema_version > 0),
// so an unstamped/zero row fails the reader guard rather than passing as version 0.
type Version int

// Table identifies a governed, schema-stamped table. It is the canonical table name used both in the
// registry and in the Postgres DDL.
type Table string

// The governed tables. Each is append-only or integrity-constrained and stamped with its schema
// version on write (see docs/DATA-MODEL.md §4).
const (
	TableActionManifest             Table = "action_manifest"
	TableSessionRiskAudit           Table = "session_risk_audit"
	TableInfragraphPrediction       Table = "infragraph_prediction"
	TableActionVerdict              Table = "action_verdict"
	TableGovernanceLedger           Table = "governance_ledger"
	TableDiscoveredScheduledReboots Table = "discovered_scheduled_reboots"
	TableEscalationQueue            Table = "escalation_queue"
	TableChatEvents                 Table = "chat_events"
	TableEstateSnapshot             Table = "estate_snapshot"
	TableSkillVersion               Table = "skill_version"
	TableSkillTrial                 Table = "skill_trial"
	TableSessionTriage              Table = "session_triage"
	TableSessionJudgment            Table = "session_judgment"
	TableControlPlaneConfig         Table = "control_plane_config"
	TableSealedSecret               Table = "sealed_secret"
	TableKnowledgeEmbedding         Table = "knowledge_embedding"
)

// current is the canonical registry: every governed table's current compiled schema version. Bump a
// table's entry in the same change that ships the migration altering its row shape. This map is the
// single source of truth INV-15 requires — DDL, JSON Schema, validators, and counts derive from it.
var current = map[Table]Version{
	TableActionManifest:             1,
	TableSessionRiskAudit:           1,
	TableInfragraphPrediction:       1,
	TableActionVerdict:              1,
	TableGovernanceLedger:           1,
	TableDiscoveredScheduledReboots: 1,
	TableEstateSnapshot:             1,
	TableEscalationQueue:            1,
	TableChatEvents:                 1,
	TableSkillVersion:               1,
	TableSkillTrial:                 1,
	TableSessionTriage:              1,
	TableSessionJudgment:            1,
	TableControlPlaneConfig:         1,
	TableSealedSecret:               1,
	TableKnowledgeEmbedding:         1,
}

// ErrUnknownTable is returned for a table absent from the canonical registry — a governed row must
// belong to a registered table, so an unknown table fails closed rather than defaulting to a version.
type ErrUnknownTable struct{ Table Table }

func (e ErrUnknownTable) Error() string {
	return fmt.Sprintf("schema: table %q is not in the canonical schema-version registry", e.Table)
}

// Current returns the compiled schema version for a governed table, or ErrUnknownTable.
func Current(t Table) (Version, error) {
	v, ok := current[t]
	if !ok {
		return 0, ErrUnknownTable{Table: t}
	}
	return v, nil
}

// Tables returns the registered governed tables (used by the contract generator, INV-15). The order
// is not defined; callers that need a stable order sort.
func Tables() []Table {
	out := make([]Table, 0, len(current))
	for t := range current {
		out = append(out, t)
	}
	return out
}
