package schema

import "fmt"

// SchemaVersionError is returned when a reader decodes a governed row whose stored schema version it
// cannot safely interpret — either a future version (stored > compiled) or an invalid version
// (stored <= 0). The reader fails closed with this error instead of mis-reading the row. [O] REQ-505.
type SchemaVersionError struct {
	Table    Table
	Stored   Version
	Compiled Version
}

func (e SchemaVersionError) Error() string {
	return fmt.Sprintf("schema: %s row has schema_version %d the reader (compiled %d) cannot interpret — refusing to mis-read",
		e.Table, e.Stored, e.Compiled)
}

// Stamp returns the version a writer must stamp on a new row of table t — the current compiled
// version from the canonical registry. Every governed-table writer calls this so a row's version is
// never hand-set. An unregistered table fails closed via ErrUnknownTable.
func Stamp(t Table) (Version, error) {
	return Current(t)
}

// CheckRow is the reader guard: it validates a row's stored schema_version against the reader's
// compiled version for table t. It returns:
//   - ErrUnknownTable   if t is not registered,
//   - SchemaVersionError if stored <= 0 (invalid/unstamped) or stored > compiled (written by a future
//     version the reader cannot interpret),
//   - nil                if 0 < stored <= compiled (the reader can safely decode the row).
//
// A stored version LOWER than compiled is accepted: the reader understands older shapes. Only a
// FUTURE version is rejected — that is the fail-closed direction. [O] REQ-505, INV-16.
func CheckRow(t Table, stored Version) error {
	compiled, err := Current(t)
	if err != nil {
		return err
	}
	if stored <= 0 || stored > compiled {
		return SchemaVersionError{Table: t, Stored: stored, Compiled: compiled}
	}
	return nil
}
