//go:build !windows

/***************************************************************
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package server_utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/server_structs"
)

func reportWith(outcome server_structs.TestOutcome, reason server_structs.TestFailureReason, detail string) server_structs.TestReport {
	return server_structs.NewTestReport(
		server_structs.SelfTestSuite,
		server_structs.ExpectedEdges(server_structs.OriginType),
		[]server_structs.TestResult{{
			Binding: server_structs.BindingRef{Namespace: "/ligo/frames"},
			Edges:   originDataEdges,
			Op:      "read",
			Outcome: outcome,
			Reason:  reason,
			Detail:  detail,
		}},
	)
}

// The status mapping is where a wrong choice re-creates the original bug: if a
// report that never ran maps to OK, a server nobody watched looks identical to
// one that was verified.
func TestSummarizeReportStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		outcome server_structs.TestOutcome
		want    metrics.HealthStatusEnum
	}{
		{"pass", server_structs.TestOutcomePass, metrics.StatusOK},
		{"degraded", server_structs.TestOutcomeDegrade, metrics.StatusDegraded},
		{"fail", server_structs.TestOutcomeFail, metrics.StatusCritical},
		{"nothing ran", server_structs.TestOutcomeSkip, metrics.StatusWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, msg := SummarizeReport(reportWith(tc.outcome, "", ""))
			assert.Equal(t, tc.want, status)
			assert.NotEmpty(t, msg)
		})
	}
}

// The message has to name the step, the reason, and the binding. "The self-test
// failed" is what operators had before, and it is not actionable.
func TestSummarizeReportNamesWhatBroke(t *testing.T) {
	_, msg := SummarizeReport(reportWith(
		server_structs.TestOutcomeFail,
		server_structs.ReasonAuthorization,
		"403 Forbidden from the storage backend"))

	assert.Contains(t, msg, "read", "names the step")
	assert.Contains(t, msg, string(server_structs.ReasonAuthorization), "names the reason")
	assert.Contains(t, msg, "/ligo/frames", "names the binding")
	assert.Contains(t, msg, "403 Forbidden", "includes the detail")
}

// A report that passed everything it tried is not the same as one that tried
// everything, and the message must not let those read alike.
func TestSummarizeReportAnnouncesCoverageGaps(t *testing.T) {
	partial := server_structs.NewTestReport(
		server_structs.SelfTestSuite,
		server_structs.ExpectedEdges(server_structs.OriginType),
		[]server_structs.TestResult{{
			Binding: server_structs.BindingRef{Namespace: "/pelican/monitoring", Synthetic: true},
			Edges:   originDataEdges,
			Op:      "read",
			Outcome: server_structs.TestOutcomePass,
		}},
	)

	status, msg := SummarizeReport(partial)
	assert.Equal(t, metrics.StatusOK, status)
	assert.Contains(t, msg, "not covered", "a passing-but-partial report must say what it missed")
	assert.Contains(t, msg, string(server_structs.EdgeOriginData))
}

// Redaction is bounded: an origin may withhold free text, never the outcome and
// never the coverage. Suppressing coverage would silently buy a server the
// benefit of the doubt from the Director's uncertainty weighting.
func TestRedactReportKeepsOutcomesAndCoverage(t *testing.T) {
	original := reportWith(server_structs.TestOutcomeFail,
		server_structs.ReasonStorage, "/mnt/data/ligo: permission denied")

	redacted := RedactReport(&original)
	require.NotNil(t, redacted)

	assert.Equal(t, original.Outcome, redacted.Outcome, "the outcome always survives")
	assert.Equal(t, original.Covered, redacted.Covered, "coverage always survives")
	assert.Equal(t, original.Uncovered, redacted.Uncovered)

	require.Len(t, redacted.Results, len(original.Results))
	for _, res := range redacted.Results {
		assert.Empty(t, res.Detail, "free text is withheld")
		assert.NotEmpty(t, res.Reason, "the classification is not site-identifying and survives")
		assert.NotEmpty(t, res.Outcome)
	}

	// A filesystem path in a Go error is exactly the kind of thing an origin
	// admin may not wish to publish.
	for _, res := range redacted.Results {
		assert.NotContains(t, res.Detail, "/mnt/data")
	}
}

// Redaction must not mutate the caller's report -- the origin still shows the
// full detail in its own UI.
func TestRedactReportDoesNotMutateTheOriginal(t *testing.T) {
	original := reportWith(server_structs.TestOutcomeFail,
		server_structs.ReasonStorage, "permission denied")

	_ = RedactReport(&original)

	require.Len(t, original.Results, 1)
	assert.Equal(t, "permission denied", original.Results[0].Detail,
		"the local copy keeps its detail")
}

func TestRedactReportHandlesNil(t *testing.T) {
	assert.Nil(t, RedactReport(nil))
}

// The self-test and director-test reports are handed between goroutines: probes
// write them, the advertisement builder reads them on a different cadence.
func TestReportHandoff(t *testing.T) {
	self := reportWith(server_structs.TestOutcomePass, "", "")
	RecordSelfTestReport(self)
	got := LastSelfTestReport()
	require.NotNil(t, got)
	assert.Equal(t, server_structs.TestOutcomePass, got.Outcome)

	// The Director's verdict about this server used to cross the wire and be
	// dropped on arrival; it is now retained for the origin's own UI.
	dir := reportWith(server_structs.TestOutcomeFail, server_structs.ReasonTimeout, "timed out")
	dir.Suite = server_structs.DirectorTestSuite
	RecordDirectorTestReport(dir)
	gotDir := LastDirectorTestReport()
	require.NotNil(t, gotDir)
	assert.Equal(t, server_structs.DirectorTestSuite, gotDir.Suite)
	assert.Equal(t, server_structs.ReasonTimeout, gotDir.Results[0].Reason)
}

// These strings are on-disk and on-wire format: the Director parses them back out
// of probe object names, and the cache's evict endpoint matches them with a
// regex. Renaming one silently breaks cache eviction, so pin them.
func TestProbeNamingIsStableFormat(t *testing.T) {
	assert.Equal(t, "self-test", server_structs.SelfTestSuite.String())
	assert.Equal(t, "director-test", server_structs.DirectorTestSuite.String())
	assert.Equal(t, "/pelican/monitoring", MonitoringBaseNs)
	assert.Equal(t, "directorTest", DirectorTestDir)
	assert.Equal(t, "/pelican/monitoring/selfTest", SelfTestPrefix())

	when := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, "/pelican/monitoring/directorTest/dir-1.example.com/2026-06-16",
		DirectorTestPrefix("dir-1.example.com", when))

	assert.Equal(t, SelfTestBody, ProbeBodyFor(server_structs.SelfTestSuite))
	assert.Equal(t, DirectorTestBody, ProbeBodyFor(server_structs.DirectorTestSuite))
}

// The Director's structured report used to cross the wire and be dropped on
// arrival -- an origin admin could read the prose but not which step failed or
// what the Director had been unable to cover.
func TestInterpretDirectorTestResultPrefersTheReport(t *testing.T) {
	report := reportWith(server_structs.TestOutcomeFail,
		server_structs.ReasonContentMismatch, "read back bytes we did not write")
	report.Suite = server_structs.DirectorTestSuite

	status, msg := InterpretDirectorTestResult(server_structs.DirectorTestResult{
		Status:  "error",
		Message: "Director file transfer test cycle failed for server: https://o:8443",
		Report:  &report,
	}, time.Now())

	assert.Equal(t, metrics.StatusCritical, status)
	assert.Contains(t, msg, string(server_structs.ReasonContentMismatch),
		"the structured reason beats the flattened prose")
	assert.Contains(t, msg, "/ligo/frames", "and names the binding")
}

// A Director predating the report field sends only Status and Message, and that
// must keep working rather than degrading to a blank verdict.
func TestInterpretDirectorTestResultFallsBackToMessage(t *testing.T) {
	when := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

	okStatus, okMsg := InterpretDirectorTestResult(server_structs.DirectorTestResult{
		Status: "ok",
	}, when)
	assert.Equal(t, metrics.StatusOK, okStatus)
	assert.Contains(t, okMsg, "2026-06-16")

	failStatus, failMsg := InterpretDirectorTestResult(server_structs.DirectorTestResult{
		Status:  "error",
		Message: "upload failed: 403",
	}, when)
	assert.Equal(t, metrics.StatusCritical, failStatus)
	assert.Contains(t, failMsg, "upload failed: 403")
}

// A report whose steps were all skipped is not a failure. Reporting critical
// there is what made origins that legitimately cannot be probed show up red.
func TestInterpretDirectorTestResultSkippedIsNotFailure(t *testing.T) {
	skipped := server_structs.NewTestReport(
		server_structs.DirectorTestSuite,
		server_structs.ExpectedEdges(server_structs.OriginType),
		nil)

	status, msg := InterpretDirectorTestResult(server_structs.DirectorTestResult{
		Status: "ok",
		Report: &skipped,
	}, time.Now())

	assert.NotEqual(t, metrics.StatusCritical, status)
	assert.Contains(t, msg, "not covered", "but the gap is still reported")
}
