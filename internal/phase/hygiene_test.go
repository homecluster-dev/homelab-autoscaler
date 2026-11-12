/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package phase

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// The law this file asserts, in the words of docs/development/state.md: the observability the
// cluster-view work adds - the ring of recent transitions, the activation counts, the transition's
// own timestamp and the phase it came from - is a **derived, materialized report**, and the state
// machine must never read it.
//
// A field that is both a report and an input is the class of bug this repository has been patched
// for repeatedly (the 0.1.20 watchdog, the 0.1.22 observed-power precedence rule), and the new
// fields are exactly the shape of that bug: a record of the past that is easy to consult "just to
// know what happened last". So the rule is asserted on the source and not argued: these selectors
// may appear in the writer - this package - and nowhere else in internal/.
//
// The file asserts the writers are the ones the design names, too: phase.Record is the only writer
// of the report's history, and it is called from the four passes that already own a write of
// status.phase and from nowhere else.
//
// The granularity of the two allowlists below is the **file**, not the function: the greps render a
// whole file (the AST walk for the selectors, the raw text for the rest), so the law pinned here is
// "no file outside these writes the record", and a fifth writer inside a listed file - a second call
// to Record in node_controller.go, say - is green here and is a review question, not a test one.
// File granularity is the honest scope of a grep and it is deliberate: the class of bug this file
// exists for is the *new* reader of the history, the code that consults a record of the past "just to
// know what happened last", and that arrives as a new file (a new controller, a new package) far more
// often than as a new function in a file whose job the design already names.

// internalDir is the source tree the law is asserted over, relative to this file.
const internalDir = "../../internal"

// observabilitySelectors are the field accesses that mean "this code read the report's history".
var observabilitySelectors = []string{
	".Status.RecentTransitions",
	".Status.RecentActivations",
	".Status.ActivationsTotal",
	".Status.ActivationsLast24H",
	".Status.PreviousPhase",
	".Status.LastTransitionTime",
}

// recordCallers are the files allowed to call the writer of the history: each is a pass that
// already owns a phase write.
var recordCallers = []string{
	// applyObservation: the pass that sees the machine.
	"internal/controller/core/node_controller.go",
	// the first report of a freshly committed pool.
	"internal/controller/core/node_slot_controller.go",
	// refreshTransitionWindow: the pass the 24h expiry schedules.
	"internal/controller/infra/lifecycle.go",
	// stampDerivedPhase: the pass that bought the machine.
	"internal/fsm/claims.go",
}

// phaseAssigners are the files that assign to *any* `Status.Phase` field at all. Exactly one of
// them writes Node.status.phase (this package's record.go, which is why it is not in the list), and
// every entry here writes a NodeClaim's phase - the claim controller's recorder, the transition
// path's cached claim, the gRPC boundary's comparison - plus the FSM's test fixture. Freezing the
// set is the point: a fifth writer of a phase is the bug the model exists to close.
var phaseAssigners = []string{
	"internal/claim/recorder.go",
	"internal/controller/infra/claims.go",
	"internal/fsm/claims.go",
	"internal/fsm/test_utils.go",
	"internal/grpcserver/server.go",
}

// TestObservabilityIsNotAnInput is the grep the PR quotes, kept as a test so the law cannot rot.
func TestObservabilityIsNotAnInput(t *testing.T) {
	hits := grepSource(t, internalDir, selectorsOf, func(file, src string) []string {
		if isWriterFile(file) {
			return nil
		}

		var found []string

		for _, selector := range observabilitySelectors {
			if strings.Contains(src, selector) {
				found = append(found, relFromRepo(file)+" "+selector)
			}
		}

		return found
	})

	assert.Empty(t, hits, "the ring, the counts, previousPhase and lastTransitionTime are observability: nothing outside internal/phase may read them\n"+strings.Join(hits, "\n"))
}

// TestPhaseRecordHasTheWritersTheDesignNames pins the call sites of the single writer, so a new
// writer of the history cannot appear quietly next to the passes that own the report.
func TestPhaseRecordHasTheWritersTheDesignNames(t *testing.T) {
	got := grepSource(t, internalDir, rawOf, func(file, src string) []string {
		if isWriterFile(file) {
			return nil
		}

		if strings.Contains(src, "phase.Record(") {
			return []string{relFromRepo(file)}
		}

		return nil
	})

	assert.ElementsMatch(t, recordCallers, got, "phase.Record is the single writer of the report's history, called by the passes that own a phase write")
}

// TestNodePhaseFieldHasOneWriter pins the set of files that assign to a `Status.Phase` field.
// Node.status.phase itself is written in exactly one place, Record, in this package: the test
// asserts it there, so the claim writes and the fixture cannot be confused with it.
func TestNodePhaseFieldHasOneWriter(t *testing.T) {
	got := grepSource(t, internalDir, rawOf, func(file, src string) []string {
		if isWriterFile(file) {
			return nil
		}

		if strings.Contains(src, "Status.Phase =") || strings.Contains(src, "Status.Phase=") {
			return []string{relFromRepo(file)}
		}

		return nil
	})

	assert.ElementsMatch(t, phaseAssigners, got, "a second writer of a phase is the bug this model exists to close")

	// The one writer, asserted where it lives.
	src := readSource(t, "record.go")
	require.Contains(t, src, "node.Status.Phase = derived", "Record is the writer of Node.status.phase")
	require.Contains(t, src, "node.Status.PreviousPhase = from")
	require.Contains(t, src, "node.Status.LastTransitionTime = &stamp")
	require.Contains(t, src, "node.Status.ActivationsTotal++")
	require.Contains(t, src, "node.Status.ActivationsLast24H = count")
	require.Contains(t, src, "node.Status.RecentTransitions = append(node.Status.RecentTransitions, infrav2alpha1.NodeTransition{")
	require.Contains(t, src, "node.Status.RecentActivations = append(node.Status.RecentActivations, stamp)")
	require.NotContains(t, src, "activationsInWindow", "the 24 h count is not read through the display ring's cap")
}

// TestRecordIsTheWriterOfEveryNewField is the round trip of the API fields: Record writes them, and
// the generated deepcopy copies the ring by value, so a status write cannot alias a slot's history
// into another object's copy.
func TestRecordIsTheWriterOfEveryNewField(t *testing.T) {
	node := &infrav2alpha1.Node{
		Status: infrav2alpha1.NodeStatus{Phase: infrav2alpha1.NodePhaseOff},
	}

	at := t0

	require.True(t, Record(node, infrav2alpha1.NodePhaseBooting, at, "claim c1, observed off"))
	require.True(t, Record(node, infrav2alpha1.NodePhaseReady, at.Add(time.Minute), "observed on, usable"))

	require.NotNil(t, node.Status.LastTransitionTime)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.PreviousPhase)
	require.Len(t, node.Status.RecentTransitions, 2)
	assert.Equal(t, int32(1), node.Status.ActivationsTotal)
	assert.Equal(t, int32(1), node.Status.ActivationsLast24H)

	require.Len(t, node.Status.RecentActivations, 1, "the boot is recorded in the ledger the count is read from")
	assert.True(t, node.Status.RecentActivations[0].Time.Equal(at))
	assert.Equal(t, int32(1), node.Status.ActivationsLast24H, "and the count is its length")

	clone := node.DeepCopy()
	clone.Status.RecentTransitions[0].Reason = "mutated"
	clone.Status.RecentActivations[0] = metav1.NewTime(at.Add(time.Hour))
	clone.Status.ActivationsTotal = 99

	assert.Equal(t, "claim c1, observed off", node.Status.RecentTransitions[0].Reason, "the ring is deep-copied, not aliased")
	assert.True(t, node.Status.RecentActivations[0].Time.Equal(at), "so is the activation ledger")
	assert.Equal(t, int32(1), node.Status.ActivationsTotal)
}

// --- the grep machinery ---------------------------------------------------------------------

// grepSource walks the non-test Go files under root, renders each one with `render`, and collects
// what `classify` returns for it.
func grepSource(t *testing.T, root string, render func(path string) (string, error), classify func(file, src string) []string) []string {
	t.Helper()

	var (
		hits  []string
		seen  int
		walkE = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			seen++

			src, err := render(path)
			if err != nil {
				return err
			}

			hits = append(hits, classify(path, src)...)

			return nil
		})
	)

	require.NoError(t, walkE)
	require.Greater(t, seen, 30, "the walk must cover the operator, not an empty tree")

	return hits
}

// rawOf is the file's text.
func rawOf(path string) (string, error) {
	data, err := os.ReadFile(path)

	return string(data), err
}

// selectorsOf renders the dotted selector expressions of a file - `fresh.Status.Phase` becomes
// `.Status.Phase` - so a doc comment that quotes a field name (the docs quote these names a lot) is
// not counted as a read of it. Only the AST is walked.
func selectorsOf(path string) (string, error) {
	const mode = parser.ParseComments | parser.SkipObjectResolution

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, mode)
	if err != nil {
		return "", err
	}

	var out strings.Builder

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		base := exprName(sel.X)
		if base == "" {
			return true
		}

		out.WriteString(base)
		out.WriteString(".")
		out.WriteString(sel.Sel.Name)
		out.WriteString("\n")

		return true
	})

	return out.String(), nil
}

// exprName renders the receiver of a selector as a flat name (`node`, `nm.node`, `fresh`), and the
// empty string for anything that is not the `x.Status.Field` shape these greps are about.
func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		base := exprName(x.X)
		if base == "" {
			return ""
		}

		return base + "." + x.Sel.Name
	default:
		return ""
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)

	return string(data)
}

func isWriterFile(file string) bool {
	return strings.Contains(filepath.ToSlash(file), "internal/phase/")
}

// relFromRepo turns a walk path (relative to this package's directory) into the repo-relative path
// the allowlists are written in.
func relFromRepo(file string) string {
	clean := filepath.ToSlash(filepath.Clean(file))

	if i := strings.Index(clean, "internal/"); i >= 0 {
		return clean[i:]
	}

	return clean
}
