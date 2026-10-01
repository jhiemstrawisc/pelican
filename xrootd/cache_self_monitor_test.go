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

// Unit tests for the cache's self-test plan.
//
// The cache self-test had no unit test of any kind, which is how it shipped a
// token authorizing /pelican/monitoring/pelican/monitoring/selfTest -- the
// monitoring prefix applied twice, once by the scope and once again by the
// issuer's base path. Nothing failed visibly for as long as the cache authfile
// still permitted public reads of that namespace, so the broken token was never
// the thing that answered the request.
//
// See https://github.com/PelicanPlatform/pelican/issues/3136.
package xrootd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
)

func setupCacheProbeTest(t *testing.T) {
	t.Helper()
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_ExternalWebUrl.Set("https://cache.example.com:8444"))
}

// resolvedScope returns the single scope string the plan's token will carry.
func resolvedScope(t *testing.T, plan server_utils.ProbePlan) string {
	t.Helper()
	scopes, err := plan.ResolvedScopes()
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	return scopes[0].String()
}

// The scope the cache actually puts on the wire must be relative to the
// monitoring namespace, because buildCacheNsAdsWithIssuer registers the cache's
// monitoring issuer with that base path.
//
// This asserts on the minted token rather than on the plan struct. The plan
// stores an absolute path on purpose now; what can be wrong is the string the
// serving layer resolves, and that is only visible after minting.
func TestCacheSelfTestScopeIsRelativeToTheMonitoringIssuer(t *testing.T) {
	setupCacheProbeTest(t)

	plan := cacheSelfTestPlan()
	require.NotEmpty(t, plan.Auth.Scopes, "a self-test with no scope authorizes nothing")
	require.Equal(t, server_utils.MonitoringBaseNs, plan.Auth.IssuerBasePath,
		"the cache's monitoring issuer is registered under the monitoring namespace")

	scope := resolvedScope(t, plan)

	// The regression itself: the monitoring prefix must appear exactly zero
	// times in the scope, because the issuer's base path already supplies it.
	assert.NotContains(t, scope, server_utils.MonitoringBaseNs,
		"scope %q repeats the monitoring prefix; XRootD will resolve it to %q",
		scope, server_utils.MonitoringBaseNs+scope)
	assert.Contains(t, scope, "/"+server_utils.SelfTestDir,
		"scope %q no longer names the directory the probe reads", scope)
}

// Spelled out as an exact string so the test reads as documentation of the
// contract, and so a future change to SelfTestPrefix cannot quietly reintroduce
// the duplication while still satisfying a substring check.
func TestCacheSelfTestResolvedScopeMatchesTheObjectItReads(t *testing.T) {
	setupCacheProbeTest(t)

	plan := cacheSelfTestPlan()
	assert.Equal(t, "storage.read:/selfTest", resolvedScope(t, plan))

	// And the resolved authorization must actually contain the objects the probe
	// reads, which live at <monitoring ns>/selfTest/self-test-<timestamp>.txt.
	assert.Equal(t, server_utils.SelfTestPrefix(),
		server_utils.MonitoringBaseNs+"/"+server_utils.SelfTestDir,
		"the probe reads somewhere other than where the token authorizes")
}

// A cache self-test is the cache probing itself, so it has exactly one issuer
// and must not retry with a second, identical token.
func TestCacheSelfTestMintsOneToken(t *testing.T) {
	setupCacheProbeTest(t)

	plan := cacheSelfTestPlan()
	assert.Equal(t, param.Server_ExternalWebUrl.GetString(), plan.Auth.Issuer)
	assert.False(t, plan.Auth.Anonymous, "the cache self-test presents a token")
	assert.Equal(t, "cache", plan.Auth.Subject)
}

// The plan must say what it does not cover.
//
// A cache self-test seeds an object onto local disk with a fabricated cinfo and
// reads it back, so it establishes that XRootD can serve a file someone placed
// there. It says nothing about resolving through the Director, minting a
// federation token, or fetching from an upstream origin. Those have to show up
// as uncovered, or a passing self-test reads as a verified cache.
func TestCacheSelfTestDeclaresItsCoverageHonestly(t *testing.T) {
	setupCacheProbeTest(t)

	plan := cacheSelfTestPlan()
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, server_utils.ProbeOpRead, plan.Steps[0].Op)

	// The binding is synthetic: the monitoring namespace is not one a client reads.
	assert.True(t, plan.Binding.Synthetic)

	report := server_structs.NewTestReport(
		server_structs.SelfTestSuite,
		plan.Expected,
		[]server_structs.TestResult{{
			Binding: plan.Binding,
			Edges:   plan.Steps[0].Edges,
			Op:      string(server_utils.ProbeOpRead),
			Outcome: server_structs.TestOutcomePass,
		}},
	)

	assert.ElementsMatch(t,
		[]server_structs.Edge{
			server_structs.EdgeFront,
			server_structs.EdgeAuthz,
			server_structs.EdgeCacheHit,
		},
		report.Covered)
	assert.ElementsMatch(t,
		[]server_structs.Edge{
			server_structs.EdgeDirectorResolve,
			server_structs.EdgeFedToken,
			server_structs.EdgeUpstreamFetch,
			server_structs.EdgeCacheWrite,
		},
		report.Uncovered,
		"a seeded read must not claim any part of the cache miss path")
}
