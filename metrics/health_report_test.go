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

package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetHealthRegistry(t *testing.T) {
	t.Helper()
	healthStatus.Range(func(k, _ any) bool {
		healthStatus.Delete(k)
		return true
	})
}

func TestGetHealthReport(t *testing.T) {
	resetHealthRegistry(t)
	t.Cleanup(func() { resetHealthRegistry(t) })

	SetComponentHealthStatus(OriginCache_XRootD, StatusCritical,
		"self-test failed during read (storage) on /data/ligo: permission denied")
	SetComponentHealthStatus(OriginCache_Federation, StatusOK, "advertised successfully")

	t.Run("unredacted carries the reason", func(t *testing.T) {
		report := GetHealthReport(false)

		assert.Equal(t, StatusCritical.String(), report.Overall,
			"the overall status is the worst component, as before")
		assert.False(t, report.Redacted)
		require.Len(t, report.Components, 2)

		byName := map[string]string{}
		for _, c := range report.Components {
			byName[c.Name] = c.Message
			assert.NotZero(t, c.LastUpdate)
		}
		assert.Contains(t, byName[OriginCache_XRootD.String()], "permission denied",
			"the whole point of the transport is that the reason survives it")
	})

	// The Origin.ShareHealthDetails opt-out is bounded. An origin may withhold
	// free text; it may not withhold whether it works, or the Director cannot
	// route, and opting out would silently buy the benefit of the doubt.
	t.Run("redacted keeps every status and drops every message", func(t *testing.T) {
		report := GetHealthReport(true)

		assert.True(t, report.Redacted, "the gap must be declared, not silent")
		assert.Equal(t, StatusCritical.String(), report.Overall)
		require.Len(t, report.Components, 2)
		for _, c := range report.Components {
			assert.NotEmpty(t, c.Status, "status survives redaction")
			assert.Empty(t, c.Message, "free text does not")
		}
	})

	t.Run("components are ordered stably", func(t *testing.T) {
		first := GetHealthReport(false)
		second := GetHealthReport(false)
		require.Equal(t, len(first.Components), len(second.Components))
		for i := range first.Components {
			// sync.Map iteration order is undefined; an advertisement that
			// reshuffles every cycle is not diffable.
			assert.Equal(t, first.Components[i].Name, second.Components[i].Name)
		}
	})
}

func TestGetHealthReportEmptyRegistry(t *testing.T) {
	resetHealthRegistry(t)
	t.Cleanup(func() { resetHealthRegistry(t) })

	report := GetHealthReport(false)
	assert.Equal(t, StatusUnknown.String(), report.Overall)
	assert.Empty(t, report.Components)
}
