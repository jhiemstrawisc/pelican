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

package director

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
)

func mustParseURL(t *testing.T, raw string) url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return *parsed
}

func TestServerTypeOf(t *testing.T) {
	assert.Equal(t, server_structs.OriginType, serverTypeOf(server_structs.OriginType.String()))
	assert.Equal(t, server_structs.CacheType, serverTypeOf(server_structs.CacheType.String()))
	assert.Equal(t, server_structs.ServerType(0), serverTypeOf("registry"))
	assert.Equal(t, server_structs.ServerType(0), serverTypeOf(""))
}

func TestOriginTestPlan(t *testing.T) {
	ad := server_structs.ServerAd{
		Type:        server_structs.OriginType.String(),
		StorageType: server_structs.OriginStoragePosix,
		URL:         mustParseURL(t, "https://origin.example.com:8443"),
		Caps:        server_structs.Capabilities{Reads: true, Writes: true},
	}
	ad.Name = "test-origin"

	plan := originTestPlan(ad)

	require.Equal(t, server_structs.DirectorTestSuite, plan.Suite)
	assert.Equal(t, "https://origin.example.com:8443", plan.BaseURL)

	// The probe writes, reads back, and cleans up after itself.
	require.Len(t, plan.Steps, 3)
	assert.Equal(t, server_utils.ProbeOpWrite, plan.Steps[0].Op)
	assert.Equal(t, server_utils.ProbeOpRead, plan.Steps[1].Op)
	assert.Equal(t, server_utils.ProbeOpDelete, plan.Steps[2].Op)

	// An empty issuer means "federation discovery endpoint, with the Director's
	// own URL as a fallback for origins that predate it".
	assert.Empty(t, plan.Auth.Issuer)
	assert.False(t, plan.Auth.Anonymous)
	assert.Equal(t, []string{"https://origin.example.com:8443"}, plan.Auth.Audiences)

	// The binding must record that this probe touches the monitoring namespace
	// and not any namespace a client reads. On a POSIX origin the monitoring
	// prefix resolves under the run location rather than the administrator's
	// data volume, so mislabeling it here would let a passing probe be read as
	// proof the real export is healthy.
	assert.Equal(t, server_utils.MonitoringBaseNs, plan.Binding.Namespace)
	assert.True(t, plan.Binding.Synthetic)
}

// #2713: the Director kept PUTting to origins with no ability to write, which is
// a guaranteed failure and pinned those servers critical forever. What operations
// it attempts now follows from what the origin advertises.
func TestOriginTestPlanRespectsAdvertisedCapabilities(t *testing.T) {
	newAd := func(caps server_structs.Capabilities) server_structs.ServerAd {
		ad := server_structs.ServerAd{
			Type: server_structs.OriginType.String(),
			URL:  mustParseURL(t, "https://origin.example.com:8443"),
			Caps: caps,
		}
		ad.Name = "aws-public-data"
		return ad
	}

	t.Run("read-only origin is sent no write", func(t *testing.T) {
		plan := originTestPlan(newAd(server_structs.Capabilities{Reads: true, PublicReads: true}))
		for _, step := range plan.Steps {
			assert.NotEqual(t, server_utils.ProbeOpWrite, step.Op,
				"an origin that never claimed it could write must not be sent a PUT")
		}
	})

	// With no runnable step the report folds to "skipped", which is not a
	// failure -- so the origin no longer shows up red for declining to do
	// something it never advertised.
	t.Run("an unprobeable origin is not a failure", func(t *testing.T) {
		plan := originTestPlan(newAd(server_structs.Capabilities{Reads: true}))
		report := server_structs.NewTestReport(plan.Suite, plan.Expected, nil)
		assert.False(t, report.Outcome.IsFailure())
		assert.Equal(t, server_structs.TestOutcomeSkip, report.Outcome)
		// But the gap is recorded rather than passed off as health.
		assert.ElementsMatch(t, server_structs.ExpectedEdges(server_structs.OriginType), report.Uncovered)
	})

	// The backend is not consulted. A writable S3 origin is as probeable as a
	// writable POSIX one, which is the inversion #2713 asks for.
	t.Run("a writable non-POSIX origin is probed", func(t *testing.T) {
		ad := newAd(server_structs.Capabilities{Reads: true, Writes: true})
		ad.StorageType = server_structs.OriginStorageS3
		assert.Len(t, originTestPlan(ad).Steps, 3)
	})
}

func TestCacheTestPlan(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	plan, err := cacheTestPlan(mustParseURL(t, "https://cache.example.com:8442"))
	require.NoError(t, err)

	// Two reads of the same object: cold, then warm. A cache is two different
	// systems depending on whether it already holds what was asked for.
	require.Len(t, plan.Steps, 2)
	assert.Equal(t, server_utils.ProbeOpRead, plan.Steps[0].Op)
	assert.Equal(t, server_utils.ProbeOpRead, plan.Steps[1].Op)

	// Both steps must address the same object, or the second one misses too and
	// the hit path stays unprobed.
	assert.NotEmpty(t, plan.ObjectPath,
		"the plan has to name the object up front, or each read invents its own and both miss")

	// The monitoring namespace is public, so a client reading it presents no
	// token; presenting one would test a path the client never takes.
	assert.True(t, plan.Auth.Anonymous)

	// The object name is fresh on every cycle, which is what forces the cache
	// off its hit path and onto the miss path the probe means to exercise.
	assert.Contains(t, plan.ObjectPath, server_utils.MonitoringBaseNs)
	assert.Contains(t, plan.ObjectPath, server_utils.DirectorTestDir)
	assert.Contains(t, plan.ObjectPath, "director-1.example.com",
		"probe objects are scoped per Director so several sharing a cache do not collide")
	assert.Contains(t, plan.ObjectPath, server_structs.DirectorTestSuite.String())

	assert.Equal(t, []server_structs.Edge{
		server_structs.EdgeFront,
		server_structs.EdgeDirectorResolve,
		server_structs.EdgeUpstreamFetch,
		server_structs.EdgeCacheWrite,
	}, plan.Steps[0].Edges)

	assert.Equal(t, []server_structs.Edge{
		server_structs.EdgeFront,
		server_structs.EdgeCacheHit,
	}, plan.Steps[1].Edges,
		"the second read is served from local disk, so it covers the hit path and nothing upstream")
}

// Both branches of the cache path have to end up covered by one run.
//
// The operational failure behind #3503 was object fetches through caches
// breaking for hits alone, which every probe missed because they all ran cold.
// A single cold read leaves EdgeCacheHit uncovered; a cold read followed by a
// warm one covers both.
func TestCacheTestPlanCoversHitAndMiss(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	plan, err := cacheTestPlan(mustParseURL(t, "https://cache.example.com:8442"))
	require.NoError(t, err)

	results := make([]server_structs.TestResult, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		results = append(results, server_structs.TestResult{
			Edges:   step.Edges,
			Op:      string(step.Op),
			Outcome: server_structs.TestOutcomePass,
		})
	}
	report := server_structs.NewTestReport(plan.Suite, plan.Expected, results)

	for _, edge := range []server_structs.Edge{
		server_structs.EdgeFront,
		server_structs.EdgeCacheHit,
		server_structs.EdgeDirectorResolve,
		server_structs.EdgeUpstreamFetch,
		server_structs.EdgeCacheWrite,
	} {
		assert.Contains(t, report.Covered, edge,
			"a run that reads cold then warm must cover %s", edge)
	}

	// What remains genuinely unprobed is the authenticated miss path, since the
	// upstream here is the Director's own public-read synthetic origin.
	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeAuthz, server_structs.EdgeFedToken},
		report.Uncovered)
}

// The authenticated plan is the only probe that makes a cache obtain a
// federation token and present it upstream.
func TestCacheAuthTestPlan(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	plan, err := cacheAuthTestPlan(mustParseURL(t, "https://cache.example.com:8442"))
	require.NoError(t, err)

	// The binding is the protected namespace, not the public one. Recording the
	// wrong one would let this probe's coverage be credited to a read that never
	// involved a token.
	assert.Equal(t, server_utils.MonitoringAuthNs, plan.Binding.Namespace)
	assert.Contains(t, plan.ObjectPath, server_utils.MonitoringAuthNs)
	assert.Contains(t, plan.ObjectPath, "director-1.example.com")

	// A token is presented, and it is federation-issued: an empty Issuer means
	// the federation discovery endpoint, which is what the cache trusts for this
	// namespace.
	require.False(t, plan.Auth.Anonymous)
	assert.Empty(t, plan.Auth.Issuer)

	// Scope relativity: the cache's scitokens entry for this namespace is based
	// at the namespace, so the minted scope must not repeat it.
	require.Equal(t, server_utils.MonitoringAuthNs, plan.Auth.IssuerBasePath)
	scopes, err := plan.ResolvedScopes()
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.NotContains(t, scopes[0].String(), server_utils.MonitoringAuthNs,
		"scope %q repeats the issuer's base path", scopes[0].String())

	require.Len(t, plan.Steps, 1, "the hit path is already covered by the public plan")
	assert.Equal(t, []server_structs.Edge{
		server_structs.EdgeFront,
		server_structs.EdgeAuthz,
		server_structs.EdgeDirectorResolve,
		server_structs.EdgeUpstreamFetch,
		server_structs.EdgeCacheWrite,
	}, plan.Steps[0].Edges)

	// EdgeFedToken is deliberately absent. A cache forwards its client's token
	// upstream and attaches its own federation token only when the origin
	// demands one, which the Director's synthetic origin does not. Claiming the
	// edge here would report coverage of a mechanism this probe never touches.
	assert.NotContains(t, plan.Steps[0].Edges, server_structs.EdgeFedToken,
		"this probe does not exercise the cache's federation token; see the comment on cacheAuthTestPlan")
}

// The coverage ledger for a cache, pinned exactly.
//
// Every edge but one is now reached by some plan. The exception is the cache's
// own federation token, which nothing probes because a cache only presents one
// to an origin that demands it, and the Director's synthetic origin does not.
//
// This test is written to fail in both directions on purpose. If a probe is
// later narrowed, Uncovered grows and it fails. If someone closes the federation
// token gap, Uncovered shrinks and it fails too -- which is the right moment to
// be told, because the docs and the issue tracker both need updating then.
func TestCacheCoverageLedgerIsPinned(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	ad := server_structs.ServerAd{
		Type: server_structs.CacheType.String(),
		URL:  mustParseURL(t, "https://cache.example.com:8442"),
	}
	plans, err := directorTestPlans(ad)
	require.NoError(t, err)

	var results []server_structs.TestResult
	for _, plan := range plans {
		for _, step := range plan.Steps {
			results = append(results, server_structs.TestResult{
				Binding: plan.Binding,
				Edges:   step.Edges,
				Op:      string(step.Op),
				Outcome: server_structs.TestOutcomePass,
			})
		}
	}

	report := server_structs.NewTestReport(
		server_structs.DirectorTestSuite,
		server_structs.ExpectedEdges(server_structs.CacheType),
		results)

	assert.Equal(t,
		[]server_structs.Edge{server_structs.EdgeFedToken},
		report.Uncovered,
		"the cache's own federation token is the one edge no probe reaches")

	for _, edge := range []server_structs.Edge{
		server_structs.EdgeFront,
		server_structs.EdgeAuthz,
		server_structs.EdgeCacheHit,
		server_structs.EdgeDirectorResolve,
		server_structs.EdgeUpstreamFetch,
		server_structs.EdgeCacheWrite,
	} {
		assert.Contains(t, report.Covered, edge, "edge %q should be covered", edge)
	}
}

// The Director drives the cache to its own synthetic origin, which is served
// public-read. Nothing mints a federation token or contacts a real origin, so a
// cache serving authenticated namespaces has that path unprobed -- and the report
// has to say so rather than reading as full coverage.
func TestCacheTestPlanDeclaresItsBlindSpots(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	plan, err := cacheTestPlan(mustParseURL(t, "https://cache.example.com:8442"))
	require.NoError(t, err)

	report := server_structs.NewTestReport(plan.Suite, plan.Expected, []server_structs.TestResult{{
		Edges:   plan.Steps[0].Edges,
		Op:      string(plan.Steps[0].Op),
		Outcome: server_structs.TestOutcomePass,
	}})

	assert.Equal(t, server_structs.TestOutcomePass, report.Outcome)
	assert.Contains(t, report.Uncovered, server_structs.EdgeFedToken,
		"the authenticated miss path has no probe anywhere in Pelican")
	assert.Contains(t, report.Uncovered, server_structs.EdgeAuthz)
	assert.Contains(t, report.Uncovered, server_structs.EdgeCacheHit,
		"the cold step on its own says nothing about serving a resident object; "+
			"it is the warm step that covers this, see TestCacheTestPlanCoversHitAndMiss")
}

func TestCacheTestPlanRequiresAHostname(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set(""))

	_, err := cacheTestPlan(mustParseURL(t, "https://cache.example.com:8442"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), param.Server_Hostname.GetName())
}

func TestDirectorTestPlanSelection(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)
	require.NoError(t, param.Server_Hostname.Set("director-1.example.com"))

	t.Run("origin", func(t *testing.T) {
		ad := server_structs.ServerAd{
			Type: server_structs.OriginType.String(),
			URL:  mustParseURL(t, "https://o:8443"),
			Caps: server_structs.Capabilities{Reads: true, Writes: true},
		}
		plans, err := directorTestPlans(ad)
		require.NoError(t, err)
		require.Len(t, plans, 1)
		assert.Len(t, plans[0].Steps, 3)
	})

	t.Run("cache", func(t *testing.T) {
		ad := server_structs.ServerAd{Type: server_structs.CacheType.String(), URL: mustParseURL(t, "https://c:8442")}
		plans, err := directorTestPlans(ad)
		require.NoError(t, err)
		require.Len(t, plans, 2, "a cache gets a public plan and an authenticated one")

		assert.Len(t, plans[0].Steps, 2, "the public plan reads cold then warm")
		assert.True(t, plans[0].Auth.Anonymous)

		assert.Len(t, plans[1].Steps, 1, "the authenticated plan needs only a cold read")
		assert.False(t, plans[1].Auth.Anonymous,
			"the whole point of the second plan is that a token is involved")
		assert.Equal(t, server_utils.MonitoringAuthNs, plans[1].Binding.Namespace)
	})

	// The previous implementation initialized its success flag to true and only
	// assigned it on the origin branch, so a server that was neither an origin
	// nor a cache fell through every branch and was reported to itself as "ok"
	// with no test having run. An unrecognized type must be an error.
	t.Run("unrecognized type is an error, not a silent pass", func(t *testing.T) {
		ad := server_structs.ServerAd{Type: "registry", URL: mustParseURL(t, "https://r:8444")}
		ad.Name = "somewhere-odd"

		_, err := directorTestPlans(ad)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "unrecognized server type")
		assert.Contains(t, err.Error(), "somewhere-odd")
	})
}
