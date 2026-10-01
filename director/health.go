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
	"time"

	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
)

// The Director has two sources of information about whether a server is healthy,
// and until now it acted on only one of them.
//
// It runs its own probes, so it has a first-hand measurement. It then POSTed that
// measurement to the server and discarded its own copy. The server folded the
// result into its component health, collapsed that to a single word, and sent the
// word back on its next advertisement -- up to a minute later. Only then did the
// Director's routing logic see it.
//
// The consequence is that an origin which is an older version, drops the report,
// or simply does not echo it keeps its full routing weight no matter what the
// Director observed. The measurement existed the whole time; it just took a round
// trip through the component being judged before anyone acted on it.
//
// directorView closes that loop. It is the single place the Director's routing
// decisions ask "how healthy is this server", and it consults both observers.

// probeStalenessFactor bounds how long a first-hand probe result stays
// authoritative, as a multiple of the probe interval.
//
// A probe result that is several cycles old means the probe loop itself has
// stopped, which is a reason to stop trusting the result rather than to keep
// applying it. Three cycles tolerates a missed round or two without letting a
// dead loop pin a server to a stale verdict.
const probeStalenessFactor = 3

// directorProbeResult returns this Director's own most recent probe verdict for a
// server, and whether that verdict is fresh enough to act on.
//
// Callers must not already hold healthTestUtilsMutex. listServers in
// director_ui.go holds it across its whole loop, so code reached from there has
// to read the util directly rather than coming back through here.
func directorProbeResult(serverURL string) (server_structs.TestOutcome, bool) {
	healthTestUtilsMutex.RLock()
	defer healthTestUtilsMutex.RUnlock()

	util, ok := healthTestUtils[serverURL]
	if !ok || util == nil || util.Probe == nil {
		return "", false
	}

	interval := param.Director_OriginCacheHealthTestInterval.GetDuration()
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if time.Since(util.ProbeAt) > time.Duration(probeStalenessFactor)*interval {
		return "", false
	}

	return util.Probe.Outcome, true
}

// intersectEdges returns the edges present in every observer's uncovered set.
//
// An edge only counts as a real blind spot when nobody reached it. The Director
// and the server look at different things -- the Director probes the synthetic
// monitoring namespace while only the server's own self-test can touch its real
// exports -- so either report read alone overstates the gap.
func intersectEdges(sets [][]server_structs.Edge) []server_structs.Edge {
	if len(sets) == 0 {
		return nil
	}

	counts := map[server_structs.Edge]int{}
	for _, set := range sets {
		// Guard against an observer listing the same edge twice.
		seen := map[server_structs.Edge]bool{}
		for _, edge := range set {
			if seen[edge] {
				continue
			}
			seen[edge] = true
			counts[edge]++
		}
	}

	// Preserve the first set's order so the field is stable across polls.
	var shared []server_structs.Edge
	for _, edge := range sets[0] {
		if counts[edge] == len(sets) {
			shared = append(shared, edge)
			counts[edge] = 0 // do not emit duplicates
		}
	}
	return shared
}

// unobservedWeight is the multiplier applied to a server the Director cannot
// observe at all.
//
// It is close to 1 on purpose. This is a nudge, not a punishment: an unobserved
// server may well be perfectly healthy, and the Director has no evidence either
// way. What it must not do is treat "nobody is watching" as identical to
// "verified healthy", which is what a flat 1.0 did.
const unobservedWeight = 0.9

// probeOptOutIsAcceptable reports whether a server of this type may decline
// Director probes without it counting against them.
//
// This is the origin/cache asymmetry #3095 argues for, in one place rather than
// scattered across Go and TypeScript. An origin is often operated by someone
// other than the federation and has legitimate reasons to opt out; it should not
// be punished for exercising a supported configuration. A cache joins a
// federation specifically to serve that federation's traffic, so letting one keep
// full routing weight while refusing to be measured rewards precisely the
// behavior a federation cannot afford:
//
//	"We need to disincentivize allowing caches that join the federation to hide
//	 their status by disallowing the director to test them, because otherwise we
//	 cannot tell if it's a good cache and should rightfully deprioritize them."
func probeOptOutIsAcceptable(serverType string) bool {
	return serverType != server_structs.CacheType.String()
}

// uncertaintyDiscount returns a multiplier reflecting whether this Director is
// permitted to observe the server at all.
//
// getRawStatusWeight answers "how healthy is it", under the documented rule that
// the weight may only decrease a server's priority, never increase it. Within a
// single scalar that reasoning is sound, and giving an unmeasured status the
// multiplicative identity is the choice that does not distort sorting.
//
// The gap it leaves is that "penalize" and "do nothing" are the only two moves,
// so unobserved necessarily collapses onto observed-healthy. This is the third
// move, applied only where the Director positively knows it has been shut out --
// not merely where it has no result yet, since a server's first advertisement
// arrives before its probe loop has started and must not be penalized for that.
func uncertaintyDiscount(ad *server_structs.ServerAd) float64 {
	if ad == nil || !ad.DisableDirectorTest {
		return 1.0
	}
	if probeOptOutIsAcceptable(ad.Type) {
		return 1.0
	}
	return unobservedWeight
}

// directorView fuses what the Director measured with what the server reported
// about itself, and returns the status the routing logic should act on.
//
// The two observers are not interchangeable, so they are not averaged:
//
//   - A failed probe is authoritative and takes effect immediately. The Director
//     saw the transfer fail; no self-report can talk it out of that, and waiting
//     for the server to agree is what let a broken origin keep full weight.
//   - A passing probe does not override the self-report. The server can see
//     things a probe cannot -- IO saturation, a degraded backend, an imminent
//     shutdown -- so its own word still governs when the Director has no
//     contrary evidence.
//   - With no fresh probe, the self-report is all there is.
//
// reported is the aggregate word from the server's advertisement, which may be
// empty for servers predating it.
func directorView(serverURL, reported string) metrics.HealthStatusEnum {
	reportedStatus := metrics.ParseHealthStatus(reported)

	outcome, fresh := directorProbeResult(serverURL)
	if !fresh {
		return reportedStatus
	}

	switch outcome {
	case server_structs.TestOutcomeFail:
		return metrics.StatusCritical
	case server_structs.TestOutcomeDegrade:
		// Take whichever observer is more pessimistic: a degraded probe is real
		// evidence, but a server reporting critical knows something worse.
		if reportedStatus < metrics.StatusDegraded {
			return reportedStatus
		}
		return metrics.StatusDegraded
	default:
		return reportedStatus
	}
}
