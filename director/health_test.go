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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
)

const testServerURL = "https://server.example.com:8443"

// setProbeResult installs a first-hand probe verdict as though a test cycle had
// just recorded one.
func setProbeResult(t *testing.T, outcome server_structs.TestOutcome, at time.Time) {
	t.Helper()
	healthTestUtilsMutex.Lock()
	defer healthTestUtilsMutex.Unlock()
	healthTestUtils[testServerURL] = &healthTestUtil{
		Probe:   &server_structs.TestReport{Outcome: outcome},
		ProbeAt: at,
	}
}

func clearProbeResults(t *testing.T) {
	t.Helper()
	healthTestUtilsMutex.Lock()
	defer healthTestUtilsMutex.Unlock()
	delete(healthTestUtils, testServerURL)
}

func TestDirectorView(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(func() {
		clearProbeResults(t)
		server_utils.ResetTestState()
	})
	require.NoError(t, param.Director_OriginCacheHealthTestInterval.Set(15*time.Second))

	t.Run("no probe result falls back to the self-report", func(t *testing.T) {
		clearProbeResults(t)
		assert.Equal(t, metrics.StatusOK, directorView(testServerURL, "ok"))
		assert.Equal(t, metrics.StatusCritical, directorView(testServerURL, "critical"))
	})

	// The whole point of keeping the Director's own measurement: a server that
	// insists it is fine does not get to overrule a transfer the Director watched
	// fail. Previously the Director's verdict only reached routing if the server
	// chose to echo it back.
	t.Run("a failed probe overrides a healthy self-report", func(t *testing.T) {
		setProbeResult(t, server_structs.TestOutcomeFail, time.Now())
		assert.Equal(t, metrics.StatusCritical, directorView(testServerURL, "ok"))
	})

	// The converse does not hold. A server can see saturation or an imminent
	// shutdown that a single successful probe cannot, so its own word still
	// governs when the Director has no contrary evidence.
	t.Run("a passing probe does not override an unhealthy self-report", func(t *testing.T) {
		setProbeResult(t, server_structs.TestOutcomePass, time.Now())
		assert.Equal(t, metrics.StatusCritical, directorView(testServerURL, "critical"))
		assert.Equal(t, metrics.StatusShuttingDown, directorView(testServerURL, "shutting down"))
		assert.Equal(t, metrics.StatusOK, directorView(testServerURL, "ok"))
	})

	t.Run("a degraded probe takes the more pessimistic view", func(t *testing.T) {
		setProbeResult(t, server_structs.TestOutcomeDegrade, time.Now())
		assert.Equal(t, metrics.StatusDegraded, directorView(testServerURL, "ok"))
		assert.Equal(t, metrics.StatusCritical, directorView(testServerURL, "critical"))
	})

	// A probe result several cycles old means the probe loop has stopped, which
	// is a reason to stop trusting the verdict rather than to keep applying it.
	t.Run("a stale probe result is ignored", func(t *testing.T) {
		setProbeResult(t, server_structs.TestOutcomeFail, time.Now().Add(-10*time.Minute))
		assert.Equal(t, metrics.StatusOK, directorView(testServerURL, "ok"))
	})

	t.Run("a probe within the freshness window still counts", func(t *testing.T) {
		setProbeResult(t, server_structs.TestOutcomeFail, time.Now().Add(-20*time.Second))
		assert.Equal(t, metrics.StatusCritical, directorView(testServerURL, "ok"))
	})

	// Servers predating the status field send nothing, and an unknown word parses
	// to StatusUnknown. Neither should be turned into a failure by this function.
	t.Run("an empty self-report is not treated as a failure", func(t *testing.T) {
		clearProbeResults(t)
		assert.Equal(t, metrics.StatusUnknown, directorView(testServerURL, ""))
	})
}

// #3095 argues for an asymmetry: an origin may decline probes without penalty,
// a cache may not. Expressed once, here, rather than scattered across Go and
// TypeScript.
func TestUncertaintyDiscount(t *testing.T) {
	origin := func(disabled bool) *server_structs.ServerAd {
		return &server_structs.ServerAd{
			Type: server_structs.OriginType.String(), DisableDirectorTest: disabled,
		}
	}
	cache := func(disabled bool) *server_structs.ServerAd {
		return &server_structs.ServerAd{
			Type: server_structs.CacheType.String(), DisableDirectorTest: disabled,
		}
	}

	assert.Equal(t, 1.0, uncertaintyDiscount(origin(false)))
	assert.Equal(t, 1.0, uncertaintyDiscount(cache(false)))

	// An origin is often run by someone other than the federation and should not
	// be punished for a supported configuration.
	assert.Equal(t, 1.0, uncertaintyDiscount(origin(true)))

	// A cache that refuses to be measured must not thereby rank equal to one the
	// Director has watched succeed.
	assert.Less(t, uncertaintyDiscount(cache(true)), 1.0)

	// Servers whose first advertisement arrives before their probe loop starts
	// must not be discounted for it -- the discount keys on the opt-out, not on
	// the absence of a result.
	assert.Equal(t, 1.0, uncertaintyDiscount(nil))
}

func TestIntersectEdges(t *testing.T) {
	t.Run("no observers means no claim either way", func(t *testing.T) {
		assert.Nil(t, intersectEdges(nil))
	})

	t.Run("a single observer's gaps stand alone", func(t *testing.T) {
		got := intersectEdges([][]server_structs.Edge{
			{server_structs.EdgeOriginData, server_structs.EdgeAuthz},
		})
		assert.Equal(t,
			[]server_structs.Edge{server_structs.EdgeOriginData, server_structs.EdgeAuthz}, got)
	})

	// The Director probes the synthetic monitoring namespace while only the
	// origin's own self-test can reach its real exports. Reading either report
	// alone would overstate the blind spot.
	t.Run("an edge is only a blind spot if nobody reached it", func(t *testing.T) {
		got := intersectEdges([][]server_structs.Edge{
			{server_structs.EdgeOriginData, server_structs.EdgeFedToken},
			{server_structs.EdgeFedToken},
		})
		assert.Equal(t, []server_structs.Edge{server_structs.EdgeFedToken}, got)
	})

	t.Run("duplicates within one observer do not fake agreement", func(t *testing.T) {
		got := intersectEdges([][]server_structs.Edge{
			{server_structs.EdgeAuthz, server_structs.EdgeAuthz},
			{server_structs.EdgeOriginData},
		})
		assert.Empty(t, got)
	})
}

// The mechanism behind #3095's screenshot: "Health Test Disabled" is only
// reachable when no healthTestUtils entry exists, and nothing used to remove one.
// A server that failed a probe and then had probes turned off kept its stale
// "Error" forever, so no display-level fix could have closed the issue.
func TestDisablingProbesClearsAStaleResult(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(func() {
		clearProbeResults(t)
		server_utils.ResetTestState()
	})

	ad := server_structs.ServerAd{
		Type: server_structs.CacheType.String(),
		URL:  mustParseURL(t, testServerURL),
	}
	ad.Name = "a-cache"

	// It failed a probe while tests were on.
	healthTestUtilsMutex.Lock()
	healthTestUtils[testServerURL] = &healthTestUtil{
		Status:  HealthStatusError,
		Probe:   &server_structs.TestReport{Outcome: server_structs.TestOutcomeFail},
		ProbeAt: time.Now(),
	}
	healthTestUtilsMutex.Unlock()

	advertisement := &server_structs.Advertisement{ServerAd: ad}
	healthTestUtilsMutex.RLock()
	before := advertisementToServerResponse(advertisement)
	healthTestUtilsMutex.RUnlock()
	require.Equal(t, HealthStatusError, before.HealthStatus, "precondition: it is red")

	// Now the administrator turns director tests off and the server re-registers.
	ad.DisableDirectorTest = true
	recordAd(context.Background(), ad, &[]server_structs.NamespaceAd{})

	healthTestUtilsMutex.RLock()
	after := advertisementToServerResponse(&server_structs.Advertisement{ServerAd: ad})
	healthTestUtilsMutex.RUnlock()

	assert.Equal(t, HealthStatusDisabled, after.HealthStatus,
		"a server with probes disabled must report Disabled, not a stale Error")
	assert.False(t, after.HealthStatus.IsFailure())
}

// recordAd returned a zero-valued advertisement on the disabled path, where the
// adjacent topology branch returned the computed one.
func TestRecordAdReturnsAPopulatedAdWhenProbesAreDisabled(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(func() {
		clearProbeResults(t)
		server_utils.ResetTestState()
	})

	ad := server_structs.ServerAd{
		Type:                server_structs.OriginType.String(),
		URL:                 mustParseURL(t, testServerURL),
		DisableDirectorTest: true,
	}
	ad.Name = "quiet-origin"

	got := recordAd(context.Background(), ad, &[]server_structs.NamespaceAd{})

	assert.Equal(t, "quiet-origin", got.Name)
	assert.Equal(t, testServerURL, got.URL.String())
}

func TestDirectorProbeResultFreshness(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(func() {
		clearProbeResults(t)
		server_utils.ResetTestState()
	})

	// A zero or unset interval must not make every result look stale.
	require.NoError(t, param.Director_OriginCacheHealthTestInterval.Set(time.Duration(0)))
	setProbeResult(t, server_structs.TestOutcomePass, time.Now())
	_, fresh := directorProbeResult(testServerURL)
	assert.True(t, fresh, "a zero interval should fall back to a sane default, not discard results")

	clearProbeResults(t)
	_, fresh = directorProbeResult(testServerURL)
	assert.False(t, fresh)

	// An entry that exists but has never recorded a probe (a registration whose
	// first cycle has not finished) is not a verdict.
	healthTestUtilsMutex.Lock()
	healthTestUtils[testServerURL] = &healthTestUtil{Status: HealthStatusInit}
	healthTestUtilsMutex.Unlock()
	_, fresh = directorProbeResult(testServerURL)
	assert.False(t, fresh)
}
