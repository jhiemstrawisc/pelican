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

package server_structs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyTestError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		resp *http.Response
		want TestFailureReason
	}{
		{"no error, 200", nil, &http.Response{StatusCode: 200}, ReasonNone},
		{"no error, 201", nil, &http.Response{StatusCode: 201}, ReasonNone},
		{"no error, 204", nil, &http.Response{StatusCode: 204}, ReasonNone},
		{"401", nil, &http.Response{StatusCode: 401}, ReasonAuthorization},
		{"403", nil, &http.Response{StatusCode: 403}, ReasonAuthorization},
		{"404", nil, &http.Response{StatusCode: 404}, ReasonNotFound},
		{"410", nil, &http.Response{StatusCode: 410}, ReasonNotFound},
		{"408", nil, &http.Response{StatusCode: 408}, ReasonTimeout},
		{"504", nil, &http.Response{StatusCode: 504}, ReasonTimeout},
		{"500", nil, &http.Response{StatusCode: 500}, ReasonServerError},
		{"507", nil, &http.Response{StatusCode: 507}, ReasonServerError},
		{"429", nil, &http.Response{StatusCode: 429}, ReasonServerError},
		{"deadline exceeded", context.DeadlineExceeded, nil, ReasonTimeout},
		{"canceled", context.Canceled, nil, ReasonTimeout},
		{
			"deadline wrapped in url.Error",
			&url.Error{Op: "Get", URL: "https://x", Err: context.DeadlineExceeded},
			nil,
			ReasonTimeout,
		},
		{
			"connection refused",
			&net.OpError{Op: "dial", Err: errors.New("connection refused")},
			nil,
			ReasonConnection,
		},
		{
			"dns failure",
			&net.DNSError{Err: "no such host", Name: "nope.invalid"},
			nil,
			ReasonConnection,
		},
		{
			"connection reset wrapped in url.Error",
			&url.Error{Op: "Put", URL: "https://x", Err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}},
			nil,
			ReasonConnection,
		},
		{"permission denied", os.ErrPermission, nil, ReasonStorage},
		{"file not found locally", os.ErrNotExist, nil, ReasonStorage},
		{"unclassified error", errors.New("something odd"), nil, ReasonConnection},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := ClassifyTestError(tt.err, tt.resp)
			assert.Equal(t, tt.want, got)
			if tt.err != nil {
				assert.NotEmpty(t, detail, "a failure must carry a human-readable detail")
			}
		})
	}
}

// A DNS timeout is both a net.Error with Timeout() true and a *net.DNSError.
// "timeout" is the more actionable label, so it wins.
func TestClassifyTestErrorPrefersTimeoutOverConnection(t *testing.T) {
	got, _ := ClassifyTestError(&net.DNSError{Err: "i/o timeout", IsTimeout: true}, nil)
	assert.Equal(t, ReasonTimeout, got)
}

func TestWorseOf(t *testing.T) {
	assert.Equal(t, TestOutcomeFail, WorseOf(TestOutcomePass, TestOutcomeFail))
	assert.Equal(t, TestOutcomeFail, WorseOf(TestOutcomeFail, TestOutcomePass))
	assert.Equal(t, TestOutcomeDegrade, WorseOf(TestOutcomePass, TestOutcomeDegrade))
	assert.Equal(t, TestOutcomeFail, WorseOf(TestOutcomeDegrade, TestOutcomeFail))
	// A skipped step must never outrank one that ran, or a read-only origin
	// reports failure for the write it correctly declined to attempt.
	assert.Equal(t, TestOutcomePass, WorseOf(TestOutcomePass, TestOutcomeSkip))
	assert.Equal(t, TestOutcomePass, WorseOf(TestOutcomeSkip, TestOutcomePass))
}

func TestExpectedEdges(t *testing.T) {
	// The cache path bifurcates on cache state, so a cache has strictly more to
	// prove than an origin -- four edges that exist only on a miss.
	cacheEdges := ExpectedEdges(CacheType)
	assert.Subset(t, cacheEdges, []Edge{
		EdgeDirectorResolve, EdgeFedToken, EdgeUpstreamFetch, EdgeCacheWrite,
	})
	assert.Contains(t, cacheEdges, EdgeCacheHit)

	originEdges := ExpectedEdges(OriginType)
	assert.ElementsMatch(t, []Edge{EdgeFront, EdgeAuthz, EdgeOriginData}, originEdges)

	assert.Nil(t, ExpectedEdges(RegistryType))
}

func result(op string, outcome TestOutcome, reason TestFailureReason, edges ...Edge) TestResult {
	return TestResult{Op: op, Outcome: outcome, Reason: reason, Edges: edges}
}

func TestNewTestReportCoverage(t *testing.T) {
	expected := []Edge{EdgeFront, EdgeAuthz, EdgeOriginData}

	t.Run("all passing covers everything", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("write", TestOutcomePass, ReasonNone, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		assert.Equal(t, TestOutcomePass, rep.Outcome)
		assert.ElementsMatch(t, expected, rep.Covered)
		assert.Empty(t, rep.Uncovered)
	})

	t.Run("skipped steps cover nothing", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("write", TestOutcomeSkip, ReasonNotApplicable, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		// Skipping is not failing -- the report must not read as broken.
		assert.Equal(t, TestOutcomeSkip, rep.Outcome)
		assert.Empty(t, rep.Covered)
		assert.ElementsMatch(t, expected, rep.Uncovered)
	})

	t.Run("authorization failure stops at authz", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("read", TestOutcomeFail, ReasonAuthorization, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		assert.Equal(t, TestOutcomeFail, rep.Outcome)
		assert.ElementsMatch(t, []Edge{EdgeFront, EdgeAuthz}, rep.Covered)
		assert.ElementsMatch(t, []Edge{EdgeOriginData}, rep.Uncovered)
	})

	t.Run("connection failure stops at the front door", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("read", TestOutcomeFail, ReasonConnection, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		assert.ElementsMatch(t, []Edge{EdgeFront}, rep.Covered)
		assert.ElementsMatch(t, []Edge{EdgeAuthz, EdgeOriginData}, rep.Uncovered)
	})

	t.Run("a broken data plane is still an observed data plane", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("read", TestOutcomeFail, ReasonContentMismatch, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		assert.ElementsMatch(t, expected, rep.Covered)
		assert.Empty(t, rep.Uncovered)
	})

	t.Run("coverage accumulates across steps", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("write", TestOutcomeFail, ReasonConnection, EdgeFront, EdgeAuthz, EdgeOriginData),
			result("read", TestOutcomePass, ReasonNone, EdgeFront, EdgeAuthz, EdgeOriginData),
		})
		assert.Equal(t, TestOutcomeFail, rep.Outcome)
		assert.ElementsMatch(t, expected, rep.Covered)
		assert.Empty(t, rep.Uncovered)
	})

	t.Run("covering something unexpected still counts", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("read", TestOutcomePass, ReasonNone, EdgeFront, EdgeAuthz, EdgeOriginData, EdgeCacheHit),
		})
		assert.Contains(t, rep.Covered, EdgeCacheHit)
		assert.Empty(t, rep.Uncovered)
	})

	t.Run("covered order follows the expected order", func(t *testing.T) {
		rep := NewTestReport(SelfTestSuite, expected, []TestResult{
			result("read", TestOutcomePass, ReasonNone, EdgeOriginData, EdgeAuthz, EdgeFront),
		})
		// Stable ordering keeps reports diffable in the UI across runs.
		assert.Equal(t, expected, rep.Covered)
	})
}

// The coverage ledger is only useful if a report cannot claim more than it
// reached. This is the property the whole vocabulary exists to guarantee.
func TestNewTestReportNeverOverClaims(t *testing.T) {
	expected := ExpectedEdges(CacheType)

	for _, reason := range []TestFailureReason{
		ReasonConnection, ReasonTimeout, ReasonInternal, ReasonAuthorization,
	} {
		t.Run(string(reason), func(t *testing.T) {
			rep := NewTestReport(DirectorTestSuite, expected, []TestResult{
				result("read", TestOutcomeFail, reason, expected...),
			})
			require.NotEmpty(t, rep.Uncovered,
				fmt.Sprintf("a %s failure cannot have exercised the whole client path", reason))
			assert.NotContains(t, rep.Covered, EdgeUpstreamFetch)
		})
	}
}
