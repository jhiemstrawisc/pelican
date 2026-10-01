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

// The shared vocabulary for Origin and Cache health probes.
//
// A data server does not have "health" of its own: it is a proxy for a storage
// resource, and what fails is the relationship between the two. That relationship
// is a *binding* -- a namespace, a storage resource, a serving layer, an
// authorization context, and a capability set -- and an operation on a binding
// traverses a *path* made of independently-failing *edges*.
//
// The consequence for this file: a probe result records the binding and the edges
// it covered, not merely whether it passed. A pass that does not say what it
// covered is how a server ends up advertising "ok" while every client request
// fails -- see docs/health-testing-design.md, which this vocabulary implements.
package server_structs

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"time"
)

type (
	// Which probe suite produced a result.
	//
	// The string values appear in probe object names on disk
	// (e.g. "self-test-2026-08-09T12:00:00Z.txt") and are parsed back out by the
	// Director when it serves synthetic probe objects to caches. They are wire
	// and on-disk format; do not change them.
	TestSuite string

	// One hop in the path a request takes from a client to bytes. Each edge is a
	// dependency that can fail on its own, so each is separately worth covering.
	Edge string

	// The result of a single probe step.
	TestOutcome string

	// Why a probe step did not pass. Coarse enough to aggregate across a
	// federation, specific enough to act on.
	TestFailureReason string
)

const (
	SelfTestSuite     TestSuite = "self-test"
	DirectorTestSuite TestSuite = "director-test"
)

const (
	// Common to origins and caches.
	EdgeFront Edge = "front" // TLS termination and HTTP front end
	EdgeAuthz Edge = "authz" // scitokens/authfile decision, including issuer trust

	// Origin only.
	EdgeOriginData Edge = "origin-data" // the serving layer reaching the backing store

	// Cache only. The cache path bifurcates on cache state, and the two branches
	// share almost nothing: a probe that lands on a hit says nothing about any of
	// the miss edges.
	EdgeCacheHit        Edge = "cache-hit"        // served from local disk + cinfo
	EdgeDirectorResolve Edge = "director-resolve" // asking the Director where upstream is
	EdgeFedToken        Edge = "fed-token"        // minting and presenting a federation token
	EdgeUpstreamFetch   Edge = "upstream-fetch"   // pulling bytes from the upstream origin
	EdgeCacheWrite      Edge = "cache-write"      // committing them to local disk
)

const (
	TestOutcomePass    TestOutcome = "pass"
	TestOutcomeDegrade TestOutcome = "degraded" // completed, but outside expected bounds
	TestOutcomeFail    TestOutcome = "fail"
	TestOutcomeSkip    TestOutcome = "skipped" // deliberately not run; not a failure
)

const (
	ReasonNone TestFailureReason = ""

	// Not failures. These distinguish "we did not look" from "we looked and it
	// was broken" -- a distinction the health model had no way to express, which
	// is why an origin that legitimately cannot run a probe showed up red.
	ReasonNotApplicable TestFailureReason = "not-applicable" // capabilities make this probe meaningless
	ReasonDisabled      TestFailureReason = "disabled"       // an administrator turned it off
	ReasonRedacted      TestFailureReason = "redacted"       // detail withheld by the server's admin

	// Failures.
	ReasonConnection      TestFailureReason = "connection"       // dial, DNS, TLS, reset
	ReasonTimeout         TestFailureReason = "timeout"          // deadline or cancellation
	ReasonAuthorization   TestFailureReason = "authorization"    // 401/403; token rejected
	ReasonNotFound        TestFailureReason = "not-found"        // 404 on read-back
	ReasonServerError     TestFailureReason = "server-error"     // 5xx, or an unclassified non-2xx
	ReasonSlow            TestFailureReason = "slow"             // completed, but far slower than a few bytes warrants
	ReasonContentMismatch TestFailureReason = "content-mismatch" // read back bytes we did not write
	ReasonStorage         TestFailureReason = "storage"          // local backend error, before any HTTP
	ReasonInternal        TestFailureReason = "internal"         // Pelican's own bug, e.g. a malformed URL
)

type (
	// Which binding a probe step exercised.
	//
	// Recording this is what keeps a passing probe honest. The origin probes
	// historically wrote to /pelican/monitoring, which on a POSIX origin lives on
	// the run location rather than the administrator's data volume; a result that
	// does not say which namespace and storage it touched cannot be distinguished
	// from one that exercised the namespace a client actually reads.
	BindingRef struct {
		Namespace   string `json:"namespace"`             // the prefix actually probed
		StorageKind string `json:"storageKind,omitempty"` // OriginStorageType, or "cache"
		Issuer      string `json:"issuer,omitempty"`      // which trust chain was exercised
		Synthetic   bool   `json:"synthetic"`             // true for /pelican/monitoring
	}

	// The outcome of one probe step.
	TestResult struct {
		Binding    BindingRef        `json:"binding"`
		Edges      []Edge            `json:"edges"` // what this step actually traversed
		Op         string            `json:"op"`    // read | write | delete | list | stat
		Outcome    TestOutcome       `json:"outcome"`
		Reason     TestFailureReason `json:"reason,omitempty"`
		Detail     string            `json:"detail,omitempty"` // free text; dropped when redacted
		HTTPStatus int               `json:"httpStatus,omitempty"`
		LatencyMs  int64             `json:"latencyMs,omitempty"`
		At         int64             `json:"at"`
	}

	// A full probe run: every step, plus what the run did and did not cover.
	TestReport struct {
		Suite   TestSuite   `json:"suite"`
		Outcome TestOutcome `json:"outcome"` // worst of Results
		// Covered is the union of edges exercised by the steps that ran.
		Covered []Edge `json:"covered"`
		// Uncovered is what a client of this server role depends on that no step
		// reached. It is the gray-failure surface, made into a value the Director
		// can display, alert on, and weight -- rather than something an operator
		// has to reconstruct by reading source.
		Uncovered []Edge       `json:"uncovered"`
		Results   []TestResult `json:"results"`
		At        int64        `json:"at"`
	}

	// One entry from the metrics package's component-health registry, in a form
	// that can cross the wire to the Director.
	ComponentHealth struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Message    string `json:"message,omitempty"` // omitted when Redacted
		LastUpdate int64  `json:"lastUpdate"`
	}

	// What a server tells the Director about itself, beyond the single aggregate
	// word carried by ServerAd.Status.
	ServerHealth struct {
		Overall string `json:"overall"`
		// Redacted records that the server's administrator opted out of sharing
		// diagnostic detail. It is always sent when true: an unannounced gap is
		// worse than a declared one, because the Director cannot account for what
		// it does not know it is missing.
		Redacted   bool              `json:"redacted"`
		Components []ComponentHealth `json:"components,omitempty"`
		SelfTest   *TestReport       `json:"selfTest,omitempty"`
	}
)

func (s TestSuite) String() string   { return string(s) }
func (e Edge) String() string        { return string(e) }
func (o TestOutcome) String() string { return string(o) }

// severity orders outcomes for the worst-of fold. TestOutcomeSkip is deliberately
// the lowest: a step that was never run must not drag a report down, or every
// read-only origin reports failure for the write it correctly declined to attempt.
func (o TestOutcome) severity() int {
	switch o {
	case TestOutcomeFail:
		return 3
	case TestOutcomeDegrade:
		return 2
	case TestOutcomePass:
		return 1
	default: // TestOutcomeSkip and the zero value
		return 0
	}
}

// WorseOf returns whichever outcome is more severe.
func WorseOf(a, b TestOutcome) TestOutcome {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// IsFailure reports whether an outcome should count against a server.
//
// Note that a skipped step is not a failure. Conflating the two is what made an
// origin that cannot run a write probe indistinguishable from one whose storage
// is broken.
func (o TestOutcome) IsFailure() bool {
	return o == TestOutcomeFail
}

// reachedEdges returns the prefix of a step's declared path that the step
// actually observed.
//
// A step declares the edges it intends to traverse, but a failure stops it
// partway: a rejected token proves the authorization edge was reached and says
// nothing about the storage behind it. Counting the whole declared path as
// covered would let a server that fails every probe at its front door report full
// coverage of a data plane nobody ever touched -- which is the exact shape of the
// bug this vocabulary exists to prevent.
//
// Note that "covered" means observed, not working. A step that reached an edge
// and found it broken has still removed that edge from the unknown.
func (r TestResult) reachedEdges() []Edge {
	if r.Outcome == TestOutcomeSkip {
		return nil
	}

	edges := r.Edges
	if r.Binding.Synthetic {
		// A probe of /pelican/monitoring exercises the real front door and the
		// real authorization chain, but not the storage a client reads: on a
		// POSIX origin that namespace resolves under Origin.RunLocation while the
		// exports are symlinks to the administrator's volume. Letting it claim
		// EdgeOriginData is precisely how an origin whose data volume has
		// unmounted goes on reporting full coverage.
		edges = withoutEdge(edges, EdgeOriginData)
	}

	if !r.Outcome.IsFailure() {
		return edges
	}

	// How far a failure got is implied by why it failed.
	var deepest Edge
	switch r.Reason {
	case ReasonConnection, ReasonTimeout, ReasonInternal:
		// No answer came back, so only the front door was exercised.
		deepest = EdgeFront
	case ReasonAuthorization:
		// The server answered and refused: authorization was reached.
		deepest = EdgeAuthz
	default:
		// A 404, a 5xx, a content mismatch, or a storage error all mean the
		// request got past authorization and into the data plane.
		return edges
	}

	for i, edge := range edges {
		if edge == deepest {
			return edges[:i+1]
		}
	}
	// The step never declared the edge we stopped at, so we cannot say which of
	// its edges were reached. Claim none rather than over-claim.
	return nil
}

// withoutEdge returns edges with every occurrence of drop removed, leaving the
// input untouched.
func withoutEdge(edges []Edge, drop Edge) []Edge {
	if !containsEdge(edges, drop) {
		return edges
	}
	kept := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		if edge != drop {
			kept = append(kept, edge)
		}
	}
	return kept
}

// ExpectedEdges returns the edges a client of this server role depends on.
//
// This is the denominator for coverage: subtracting the edges a probe run
// actually reached leaves the region in which a client can fail while the
// Director sees green. Callers that know a binding cannot support an edge (a
// public namespace never mints a federation token, say) may narrow the result
// before differencing.
func ExpectedEdges(role ServerType) []Edge {
	switch {
	case role.IsEnabled(CacheType):
		return []Edge{
			EdgeFront, EdgeAuthz, EdgeCacheHit,
			EdgeDirectorResolve, EdgeFedToken, EdgeUpstreamFetch, EdgeCacheWrite,
		}
	case role.IsEnabled(OriginType):
		return []Edge{EdgeFront, EdgeAuthz, EdgeOriginData}
	default:
		return nil
	}
}

// NewTestReport folds a set of step results into a report, computing the overall
// outcome and both sides of the coverage ledger.
//
// expected is normally ExpectedEdges(role), optionally narrowed by the caller for
// capabilities the server does not claim.
func NewTestReport(suite TestSuite, expected []Edge, results []TestResult) TestReport {
	report := TestReport{
		Suite:   suite,
		Outcome: TestOutcomeSkip,
		Results: results,
		At:      time.Now().Unix(),
	}

	covered := make(map[Edge]bool, len(results))
	for _, res := range results {
		report.Outcome = WorseOf(report.Outcome, res.Outcome)
		for _, edge := range res.reachedEdges() {
			covered[edge] = true
		}
	}

	// Preserve the caller's edge order rather than ranging the map, so reports
	// are stable across runs and diffable in the UI.
	for _, edge := range expected {
		if covered[edge] {
			report.Covered = append(report.Covered, edge)
		} else {
			report.Uncovered = append(report.Uncovered, edge)
		}
	}
	// Anything covered that the caller did not expect is still coverage.
	for _, res := range results {
		if res.Outcome == TestOutcomeSkip {
			continue
		}
		for _, edge := range res.Edges {
			if !containsEdge(expected, edge) && !containsEdge(report.Covered, edge) {
				report.Covered = append(report.Covered, edge)
			}
		}
	}

	return report
}

func containsEdge(edges []Edge, want Edge) bool {
	for _, edge := range edges {
		if edge == want {
			return true
		}
	}
	return false
}

// ClassifyTestError maps a probe failure onto a structured reason and a
// human-readable detail string.
//
// Every probe classifies through this one function. Previously each of the three
// probe implementations wrapped errors into its own prose, so the only way to ask
// "how many origins are failing for the same reason" was to pattern-match log
// lines.
//
// resp may be nil when the failure happened before a response was received.
func ClassifyTestError(err error, resp *http.Response) (TestFailureReason, string) {
	if err != nil {
		detail := err.Error()

		// Check cancellation before the net errors: a context deadline surfaces
		// wrapped in a *url.Error whose Timeout() is also true, and "timeout" is
		// the more actionable of the two labels.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return ReasonTimeout, detail
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return ReasonTimeout, detail
		}

		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return ReasonConnection, detail
		}
		var recordErr tls.RecordHeaderError
		if errors.As(err, &recordErr) {
			return ReasonConnection, detail
		}

		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return ReasonConnection, detail
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return ReasonConnection, detail
		}

		// Local filesystem trouble reaching the backing store, seen by probes that
		// seed an object before reading it back.
		if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) {
			return ReasonStorage, detail
		}

		if resp != nil {
			// An error alongside a response is normally a read failure partway
			// through the body; the status still tells us who was at fault.
			if reason, ok := classifyStatus(resp.StatusCode); ok {
				return reason, detail
			}
		}
		return ReasonConnection, detail
	}

	if resp != nil {
		if reason, ok := classifyStatus(resp.StatusCode); ok {
			return reason, resp.Status
		}
	}

	return ReasonNone, ""
}

// classifyStatus maps an HTTP status onto a reason, reporting false for the 2xx
// range, which is not a failure.
func classifyStatus(code int) (TestFailureReason, bool) {
	switch {
	case code >= 200 && code < 300:
		return ReasonNone, false
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return ReasonAuthorization, true
	case code == http.StatusNotFound, code == http.StatusGone:
		return ReasonNotFound, true
	case code == http.StatusRequestTimeout, code == http.StatusGatewayTimeout:
		return ReasonTimeout, true
	default:
		// Everything else -- including 3xx a probe should never have been handed,
		// and 4xx other than the ones above -- is the server failing to do what
		// was asked.
		return ReasonServerError, true
	}
}
