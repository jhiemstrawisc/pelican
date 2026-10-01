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

package xrootd

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
)

func schedPlan(ns string, minInterval time.Duration) server_utils.ProbePlan {
	return server_utils.ProbePlan{
		Suite:       server_structs.SelfTestSuite,
		Binding:     server_structs.BindingRef{Namespace: ns},
		MinInterval: minInterval,
		Steps: []server_utils.ProbeStep{
			{Op: server_utils.ProbeOpWrite},
			{Op: server_utils.ProbeOpRead},
		},
	}
}

func schedResults(outcome server_structs.TestOutcome) []server_structs.TestResult {
	return []server_structs.TestResult{
		{Op: string(server_utils.ProbeOpWrite), Outcome: outcome},
		{Op: string(server_utils.ProbeOpRead), Outcome: outcome},
	}
}

// A plan with no floor runs every cycle. This is the local-disk case, and
// throttling it would lose resolution for no benefit.
func TestScheduleCheapPlansAlwaysRun(t *testing.T) {
	s := newProbeSchedule()
	plan := schedPlan("/ns", 0)
	now := time.Now()

	assert.True(t, s.due(plan, now))
	s.record(plan, now, schedResults(server_structs.TestOutcomePass))
	assert.True(t, s.due(plan, now), "a zero MinInterval means no throttling at all")
}

// An expensive plan is throttled, not skipped. That distinction is the whole
// difference between "S3 origins are unobservable" and "observed less often".
func TestScheduleExpensivePlansAreThrottled(t *testing.T) {
	s := newProbeSchedule()
	plan := schedPlan("/bucket", 5*time.Minute)
	start := time.Now()

	require.True(t, s.due(plan, start), "the first run is always due")
	s.record(plan, start, schedResults(server_structs.TestOutcomePass))

	assert.False(t, s.due(plan, start.Add(time.Minute)), "still inside the floor")
	assert.False(t, s.due(plan, start.Add(5*time.Minute-time.Second)), "just inside the floor")
	assert.True(t, s.due(plan, start.Add(5*time.Minute)), "the floor is inclusive")
	assert.True(t, s.due(plan, start.Add(time.Hour)))
}

// Between runs, a throttled plan keeps contributing its most recent verdict.
//
// Without this the export would look uncovered for the nineteen cycles between
// five-minute probes -- reporting a blind spot that does not exist, which is
// exactly the kind of false signal this whole design is meant to remove.
func TestScheduleCarriesResultsBetweenRuns(t *testing.T) {
	s := newProbeSchedule()
	plan := schedPlan("/bucket", 5*time.Minute)
	now := time.Now()

	s.record(plan, now, schedResults(server_structs.TestOutcomePass))
	carried := s.carried(plan)

	require.Len(t, carried, 2, "one carried result per declared step")
	for _, res := range carried {
		assert.Equal(t, server_structs.TestOutcomePass, res.Outcome)
	}
}

// A carried failure must stay a failure. Silently aging a bad result into a good
// one would let a broken export look healthy between probes.
func TestScheduleCarriesFailuresToo(t *testing.T) {
	s := newProbeSchedule()
	plan := schedPlan("/bucket", 5*time.Minute)
	now := time.Now()

	s.record(plan, now, schedResults(server_structs.TestOutcomeFail))
	for _, res := range s.carried(plan) {
		assert.Equal(t, server_structs.TestOutcomeFail, res.Outcome)
	}
}

func TestScheduleCarriesNothingBeforeFirstRun(t *testing.T) {
	s := newProbeSchedule()
	assert.Empty(t, s.carried(schedPlan("/never-run", 5*time.Minute)))
}

// Plans are tracked per export, so throttling one must not throttle another.
func TestScheduleTracksPlansIndependently(t *testing.T) {
	s := newProbeSchedule()
	a := schedPlan("/ns-a", 5*time.Minute)
	b := schedPlan("/ns-b", 5*time.Minute)
	now := time.Now()

	s.record(a, now, schedResults(server_structs.TestOutcomePass))

	assert.False(t, s.due(a, now.Add(time.Minute)))
	assert.True(t, s.due(b, now.Add(time.Minute)), "recording one export must not throttle another")
	assert.Empty(t, s.carried(b))
}

// The schedule outlives any single cycle and is read from a ticker goroutine
// while the config may be reloaded, so it must be safe under concurrent use.
func TestScheduleIsConcurrencySafe(t *testing.T) {
	s := newProbeSchedule()
	plan := schedPlan("/ns", time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			s.due(plan, now)
			s.record(plan, now, schedResults(server_structs.TestOutcomePass))
			s.carried(plan)
		}()
	}
	wg.Wait()
}

// Exports can disappear across a config reload. The schedule is package-level and
// long-lived, so without pruning it accumulates entries for namespaces that no
// longer exist and keeps carrying their last verdict into reports forever.
func TestScheduleForgetsRemovedExports(t *testing.T) {
	s := newProbeSchedule()
	kept := schedPlan("/still-here", time.Minute)
	removed := schedPlan("/deleted", time.Minute)
	now := time.Now()

	s.record(kept, now, schedResults(server_structs.TestOutcomePass))
	s.record(removed, now, schedResults(server_structs.TestOutcomePass))

	s.retain([]server_utils.ProbePlan{kept})

	assert.NotEmpty(t, s.carried(kept), "a surviving export keeps its history")
	assert.Empty(t, s.carried(removed),
		"a removed export must not keep contributing results to the health report")
	assert.True(t, s.due(removed, now), "and its throttle state must not persist either")
}
