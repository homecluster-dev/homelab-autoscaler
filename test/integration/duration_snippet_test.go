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

package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runbook's duration snippet, run.
//
// The docs of the cluster-view work (`docs/troubleshooting/common-issues.md`, "Read the Pool at a
// Glance") state a command an operator runs because a printer column cannot compute a duration: a
// JSONPath selects a value, it does not subtract one. A command in a doc has no test, and the two
// ways it rots are the two ways a runbook becomes a trap: the jq program stops parsing (a rename of
// status.phase's values, a field that moves), and the sample output stops matching what the command
// prints.
//
// So this test reads the doc, extracts the jq program and the sample output from the fenced blocks,
// runs the program over the fixture of a pool in the documented shape, and asserts the doc's own
// sample comes out. Nothing here is a copy of the doc: both the program and its expected output are
// parsed out of it, which is what makes the doc the tested artifact.

const (
	runbook  = "../../docs/troubleshooting/common-issues.md"
	fixture  = "testdata/pool-shape/nodes.json"
	section  = "### How long has this machine been up?"
	prologue = "# How long has each slot been in the phase it reports?"
)

// TestRunbookDurationSnippetRuns is the doc's command, executed.
func TestRunbookDurationSnippetRuns(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not on PATH: the runbook snippet cannot be executed here")
	}

	doc := readFile(t, runbook)

	program, output, clock := durationSnippet(t, doc)

	// The doc states the clock its sample is measured against ("With `now` at ..."), so the run is
	// reproducible and says so: no test asserts a duration against the minute it happens to run in.
	// The program is the repo's own doc, and the clock is the one the doc states: nothing here is
	// user input, and nothing is shell-interpolated.
	cmd := exec.Command(jq, "-r", "--arg", "now", clock, program, fixture)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "jq failed on the runbook program:\n"+stderr.String())

	assert.Equal(t, normalizeRows(stdout.String()), normalizeRows(output),
		"the sample output of the runbook is what its command prints")
}

// TestRunbookDurationSnippetQuotesTheFixture makes the fixture and the doc the same pool: every slot
// the doc's sample output names must exist in the JSON the command is run over. A sample that
// invents a row is the second way a runbook rots.
func TestRunbookDurationSnippetQuotesTheFixture(t *testing.T) {
	doc := readFile(t, runbook)

	_, output, _ := durationSnippet(t, doc)

	nodes := readFile(t, fixture)

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		require.GreaterOrEqual(t, len(fields), 3, "the sample line has a name, a verb and a duration: %q", line)

		assert.Contains(t, nodes, `"`+fields[0]+`"`, "the sample names a slot the fixture carries")
	}

	// The verbs are the phases of the model: the doc's own rule is `ready` = up, `off` = down, and
	// every other phase is named by itself.
	assert.Contains(t, output, "up for")
	assert.Contains(t, output, "down for")
	assert.Contains(t, output, "booting for")
	assert.Contains(t, output, "draining for")
}

// durationSnippet extracts (jq program, sample output) from the section: the program is the quoted
// argument of the fenced command, the sample is the fenced block that follows the sentence
// introducing it.
func durationSnippet(t *testing.T, doc string) (string, string, string) {
	t.Helper()

	start := strings.Index(doc, section)
	require.Greater(t, start, -1, "the runbook has the section %q", section)

	body := doc[start:]

	// The section, not the rest of the runbook: the fences of the *next* section are not this
	// section's sample.
	if end := strings.Index(body[1:], "\n## "); end > 0 {
		body = body[:end+1]
	}

	// The clock the sample was measured against, read out of the sentence that introduces it.
	clock := docClock(t, body)

	fences := fencedBlocks(t, body)
	require.GreaterOrEqual(t, len(fences), 2, "the section has the command block and the output block")

	command := ""

	for _, fence := range fences {
		if strings.Contains(fence, prologue) {
			command = fence

			break
		}
	}

	require.NotEmpty(t, command, "the section carries the command fenced with %q", prologue)

	// The command must take its clock from the shell, not from a literal: a snippet with a hard-coded
	// `now` prints a duration that was true once.
	const clockArg = `--arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)"`
	require.Contains(t, command, clockArg, "the documented command reads the clock at run time")

	program := jqProgram(t, command)
	require.Contains(t, program, "up for", "the program renders the duration sentence, not only the timestamp")
	require.Contains(t, program, "$now", "and it takes the clock from --arg, as the command line passes it")

	output := fences[len(fences)-1]
	require.NotContains(t, output, "jq", "the last block of the section is the sample output")

	return program, output, clock
}

// docClock reads the instant the doc says its sample was measured against.
func docClock(t *testing.T, body string) string {
	t.Helper()

	match := clockPattern.FindStringSubmatch(body)
	require.Len(t, match, 2, `the section states the clock of its sample: "With `+"`now`"+` at 2026-11-12T15:00:00Z ..."`)

	return match[1]
}

// clockPattern is the timestamp of that sentence.
var clockPattern = regexp.MustCompile("`now` at `([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)`")

// fencedBlocks returns the content of every ``` fence of a markdown body.
func fencedBlocks(t *testing.T, body string) []string {
	t.Helper()

	var blocks []string

	open := -1

	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if open < 0 {
				open = i

				continue
			}

			blocks = append(blocks, strings.Join(bodyLines(body)[open+1:i], "\n"))

			open = -1
		}
	}

	require.Equal(t, -1, open, "unbalanced fence in the runbook section")

	return blocks
}

func bodyLines(body string) []string {
	return strings.Split(body, "\n")
}

// jqProgram pulls the single-quoted program out of the command block.
func jqProgram(t *testing.T, command string) string {
	t.Helper()

	first := strings.Index(command, "'")
	require.Greater(t, first, -1, "the command quotes its jq program")

	last := strings.LastIndex(command, "'")
	require.Greater(t, last, first, "the quoted jq program is closed")

	return command[first+1 : last]
}

// normalizeRows compares two tables of rows field by field, so the doc can align its sample with
// spaces where the command prints tabs.
func normalizeRows(s string) []string {
	rows := strings.Split(strings.TrimSpace(s), "\n")

	out := make([]string, 0, len(rows))

	for _, row := range rows {
		out = append(out, strings.Join(strings.Fields(row), "|"))
	}

	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "the runbook and its fixture are part of the repo")

	return string(raw)
}
