package control

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// reconciliationKinds decides which gauges exist. If a new Discrepancy* kind is
// added to billing_reconcile.go and not to that list, the kind is still
// detected and still reported by the endpoint, but its gauge is never written
// -- so the alert for it silently never fires. That is the same failure mode
// E1's alertrules_test.go exists to catch, one layer down.
func TestReconciliationKindsCoversEveryDiscrepancyConstant(t *testing.T) {
	const path = "billing_reconcile.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{}
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Discrepancy") || index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				declared[name.Name] = strings.Trim(literal.Value, `"`)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("no Discrepancy* constants found in %s; this guard has stopped guarding", path)
	}
	published := map[string]bool{}
	for _, kind := range reconciliationKinds {
		published[kind] = true
	}
	for name, kind := range declared {
		if !published[kind] {
			t.Errorf("%s (%q) is detected but never published as a gauge; add it to reconciliationKinds", name, kind)
		}
	}
	if len(reconciliationKinds) != len(declared) {
		t.Errorf("reconciliationKinds has %d entries but %s declares %d constants", len(reconciliationKinds), path, len(declared))
	}
}

// The loop must stay read-only for the same reason the algorithm does: a
// scheduled job that could repair what it audits is no longer a witness.
func TestReconciliationLoopSourceContainsNoWrites(t *testing.T) {
	const path = "billing_reconcile_loop.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution); err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, statement := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "TRUNCATE ", ".Exec("} {
		if strings.Contains(text, statement) {
			t.Errorf("%s contains %q: the scheduled reconciliation must never write", path, strings.TrimSpace(statement))
		}
	}
}

// A lag shorter than the billing interval makes every run report rows the
// deduction job has not reached yet, which is a permanently firing alert. The
// loop refuses instead of publishing a meaningless signal.
func TestReconciliationLoopRefusesALagInsideTheBillingWindow(t *testing.T) {
	loop := ReconciliationLoop{Lag: billingRunInterval}
	if _, err := loop.RunOnce(context.Background(), time.Now()); err == nil {
		t.Fatal("a lag equal to the billing interval was accepted; every run would report unsettled rows")
	}
	loop.Lag = billingRunInterval / 2
	if _, err := loop.RunOnce(context.Background(), time.Now()); err == nil {
		t.Fatal("a lag inside the billing interval was accepted")
	}
}

// The defaults have to satisfy their own rule, and the window has to overlap
// the interval. A window that exactly tiled the interval would drop rows that
// landed between the last query and the tick, and the run would then report
// "balanced" over a stretch it never read.
func TestReconciliationDefaultsOverlapAndClearTheBillingWindow(t *testing.T) {
	if reconcileLag <= billingRunInterval {
		t.Errorf("reconcileLag %s must exceed billingRunInterval %s", reconcileLag, billingRunInterval)
	}
	if reconcileWindow <= reconcileRunInterval {
		t.Errorf("reconcileWindow %s must exceed reconcileRunInterval %s so consecutive runs overlap", reconcileWindow, reconcileRunInterval)
	}
}
