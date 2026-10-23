package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// THE EXPANSION MUST REACH THE QUERY, AND ONLY THE CALL SITE CAN PROVE THAT.
//
// core/knowledge has an oracle proving RuleFamilyAliases expands a rule to its family siblings and to nothing
// else. That oracle would pass UNCHANGED if RecoveredSince never called it — the belt would stay unable to
// confirm a liveness-sourced recovery, and every test would still be green. That is the exact shape this repo
// keeps rediscovering: a correct derivation thrown away at the last hop, with a control that encodes a repair
// path the system does not use.
//
// RecoveredSince runs a SQL query, so a behavioural test needs a live Postgres and is not available here. The
// call site is therefore the subject: this parses the real file and asserts that RecoveredSince both expands
// the rule through knowledge.RuleFamilyAliases AND matches with `= ANY(` on the result. Asserting over source
// structure is a blunt instrument, justified only where the runtime path is unreachable from a test — which is
// the case here, and the alternative is trusting that a correct function is called.
func TestRecoveredSinceActuallyMatchesOnTheRuleFamily(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "transition_log.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse transition_log.go: %v", err)
	}

	var found bool
	var callsExpander, usesAnyPredicate, keepsEmptyRuleGuard bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "RecoveredSince" {
			return true
		}
		found = true
		ast.Inspect(fn, func(in ast.Node) bool {
			// the expansion call
			if call, ok := in.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "RuleFamilyAliases" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "knowledge" {
						callsExpander = true
					}
				}
			}
			// the SQL must consume it as a SET, not as one string
			if lit, ok := in.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if strings.Contains(lit.Value, "ingest_transition") && strings.Contains(lit.Value, "= ANY(") {
					usesAnyPredicate = true
				}
			}
			// the fail-closed empty-rule guard must survive
			if sel, ok := in.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "TrimSpace" {
				keepsEmptyRuleGuard = true
			}
			return true
		})
		return true
	})

	if !found {
		t.Fatal("no RecoveredSince in transition_log.go — the recovery belt is gone entirely")
	}
	if !callsExpander {
		t.Error("RecoveredSince does not call knowledge.RuleFamilyAliases — it is matching a single spelling " +
			"again, so an incident raised under TG's own \"Device-Down\" label can never be confirmed recovered " +
			"by a LibreNMS-named recovery transition, and its poll parks until the vote window expires")
	}
	if !usesAnyPredicate {
		t.Error("the ingest_transition query does not match with `= ANY(`, so even a correct expansion cannot " +
			"reach the predicate — the family set would be computed and discarded")
	}
	if !keepsEmptyRuleGuard {
		t.Error("the empty-alertRule fail-closed guard is gone — an unscoped belt read would answer \"did " +
			"ANYTHING on this host recover\", which is the fail-OPEN that counted a heal TG never achieved into " +
			"the A3 numerator")
	}
}
