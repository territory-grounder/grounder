package ssh

import "fmt"

// ExecutionLog is one recorded mutating command with its rollback, bound to the ActionManifest action id
// (INV-07). One row is recorded per mutating command: the forward command that ran and the inverse the
// reconciler runs to undo it. Both are full ssh invocations — no shell, no string concatenation.
type ExecutionLog struct {
	ActionID string
	Command  []string // the forward ssh invocation that was executed
	Rollback []string // the inverse ssh invocation bound to the same action id
}

// RecordExec builds the execution_log row for a mutating command, binding both the forward command and its
// rollback to the action id. A row with an empty action id or no rollback is rejected — a mutating command
// must be undoable and attributable (INV-07).
func (m *Module) RecordExec(actionID string, command, rollback []string) (ExecutionLog, error) {
	if actionID == "" {
		return ExecutionLog{}, fmt.Errorf("ssh: execution_log requires an action id (INV-07)")
	}
	if len(command) == 0 || len(rollback) == 0 {
		return ExecutionLog{}, fmt.Errorf("ssh: a mutating command must record both its command and its rollback")
	}
	return ExecutionLog{
		ActionID: actionID,
		Command:  m.sshArgv(command),
		Rollback: m.sshArgv(rollback),
	}, nil
}
