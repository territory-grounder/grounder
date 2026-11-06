package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE DERIVATION MUST REACH THE REPORT, AND ONLY THE CALL SITE CAN PROVE THAT.
//
// attribution.CarveOutExpiryRisk finds the carve-outs whose lapse would produce false SECURITY escalations
// instead of a stand-down. It is unit-tested in core/attribution and it was, briefly, completely unwired: I
// proved a mutation control by replacing the argument at the call site with `nil`, and EVERY test still passed.
// The finding was computed correctly and thrown away — the exact "implemented is not reachable" shape, in code
// I had just written, with an oracle that could not see it.
//
// A behavioural test cannot reach this: the wiring lives in main(), which no test calls. So the call site
// itself is the subject. This parses the real file and asserts that the value handed to
// appendConfigGapReport's expiryRisks parameter is a call to attribution.CarveOutExpiryRisk — not nil, not a
// variable someone left at its zero value.
//
// A test that asserts over SOURCE STRUCTURE is a blunt instrument and is justified only where the runtime path
// is unreachable from a test. That is the case here, and the alternative — trusting that a correct function is
// called — is what produced the defect.
func TestCarveOutExpiryRiskIsActuallyWiredIntoTheBootReport(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var found, wired int
	var argDesc []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "appendConfigGapReport" {
			return true
		}
		found++
		// expiryRisks is the LAST parameter: (ledger, uncoveredHosts, domainGaps, tlsDetail, expiryRisks)
		if len(call.Args) < 5 {
			argDesc = append(argDesc, "call has fewer than 5 args — the parameter is missing entirely")
			return true
		}
		last := call.Args[len(call.Args)-1]
		inner, ok := last.(*ast.CallExpr)
		if !ok {
			// nil, an identifier, anything that is not a call: the derivation is not happening here
			argDesc = append(argDesc, describeArg(last))
			return true
		}
		sel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "CarveOutExpiryRisk" {
			argDesc = append(argDesc, describeArg(last))
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "attribution" {
			argDesc = append(argDesc, "CarveOutExpiryRisk called on an unexpected package qualifier")
			return true
		}
		wired++
		return true
	})

	if found == 0 {
		t.Fatal("no call to appendConfigGapReport in main.go — the boot config-gap report is not invoked at all, " +
			"so every finding it folds together (host coverage, domain identity gaps, TLS disagreement, expiry " +
			"risk) reaches no surface")
	}
	if wired != found {
		t.Fatalf("appendConfigGapReport is called %d time(s) but only %d pass attribution.CarveOutExpiryRisk(...): %v\n"+
			"A correctly-computed finding that is never handed to the reporter is invisible — it does not reach the "+
			"governance ledger, and therefore does not reach the console's control-plane configuration panel.",
			found, wired, argDesc)
	}
}

func describeArg(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return "identifier " + v.Name + " (a nil or zero-valued variable computes nothing)"
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil {
			return "call to " + sel.Sel.Name
		}
		return "some other call"
	default:
		return "a non-call expression"
	}
}
