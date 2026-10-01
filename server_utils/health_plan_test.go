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
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/pelicanplatform/pelican/param"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/server_structs"
)

var (
	readWriteStore = server_structs.Capabilities{Reads: true, Writes: true, Listings: true}
	readOnlyStore  = server_structs.Capabilities{Reads: true, Listings: true}
)

func export(prefix string, caps server_structs.Capabilities, probeObject string) OriginExport {
	return OriginExport{
		FederationPrefix:  prefix,
		StoragePrefix:     "/data" + prefix,
		Capabilities:      caps,
		HealthProbeObject: probeObject,
	}
}

// nativeCfg uses a natively-served backend, where Pelican performs authorization
// itself and real-export probing is on by default. Using it here means these
// tests exercise the shipping default rather than a configuration override.
func posixCfg(store server_structs.Capabilities) OriginProbeConfig {
	return OriginProbeConfig{
		StoreCaps:   store,
		StorageType: server_structs.OriginStoragePosixv2,
		BaseURL:     "https://origin.example.com:8443",
		IssuerUrl:   "https://origin.example.com",
	}
}

// The probe writes into the export itself, not into a synthetic namespace on a
// different filesystem. That is the whole point: on a POSIX origin
// /pelican/monitoring resolves under Origin.RunLocation while exports are
// symlinks to the administrator's volume, so a probe there could pass forever
// with the data volume unmounted.
func TestWritableExportProbesRealStorage(t *testing.T) {
	plans := PlanOriginProbes(
		[]OriginExport{export("/ligo/frames", readWriteStore, "")},
		posixCfg(readWriteStore))

	require.Len(t, plans, 1)
	plan := plans[0]

	assert.False(t, plan.Binding.Synthetic, "a real export is not a synthetic binding")
	assert.Equal(t, "/ligo/frames", plan.Binding.Namespace)
	assert.True(t, strings.HasPrefix(plan.Prefix, "/ligo/frames/"),
		"the scratch object must live inside the export, got %q", plan.Prefix)
	assert.Contains(t, plan.Prefix, ProbeReservedPrefix)

	// Write, read back, clean up.
	require.Len(t, plan.Steps, 3)
	assert.Equal(t, ProbeOpWrite, plan.Steps[0].Op)
	assert.Equal(t, ProbeOpRead, plan.Steps[1].Op)
	assert.Equal(t, ProbeOpDelete, plan.Steps[2].Op)

	// A local disk can take this every cycle.
	assert.Zero(t, plan.MinInterval)

	// Scopes are narrowed to the reserved prefix, not the whole namespace.
	//
	// They are stored as absolute paths and made relative to the issuer's base
	// path when the token is minted; TestEveryPlanScopesRelativeToItsIssuer
	// checks the resolved form.
	require.NotEmpty(t, plan.Auth.Scopes)
	assert.Equal(t, "/ligo/frames", plan.Auth.IssuerBasePath)
	for _, scope := range plan.Auth.Scopes {
		assert.Equal(t, "/ligo/frames/"+ProbeReservedPrefix, scope.Path)
	}
}

// A full write-read-delete against the real export covers every edge a client
// depends on. This is the assertion the old model could not make.
func TestWritableExportProbeClosesTheCoverageLedger(t *testing.T) {
	plans := PlanOriginProbes(
		[]OriginExport{export("/ligo/frames", readWriteStore, "")},
		posixCfg(readWriteStore))
	require.Len(t, plans, 1)

	var results []server_structs.TestResult
	for _, step := range plans[0].Steps {
		results = append(results, server_structs.TestResult{
			Binding: plans[0].Binding,
			Edges:   step.Edges,
			Op:      string(step.Op),
			Outcome: server_structs.TestOutcomePass,
		})
	}
	report := server_structs.NewTestReport(plans[0].Suite, plans[0].Expected, results)

	assert.Equal(t, server_structs.TestOutcomePass, report.Outcome)
	assert.ElementsMatch(t, server_structs.ExpectedEdges(server_structs.OriginType), report.Covered)
	assert.Empty(t, report.Uncovered)
}

// #2713's core case, and the reason the old "capability = storage type" proxy was
// wrong: an origin that never claimed it could write must not be sent a PUT.
func TestReadOnlyExportIsNeverSentAWrite(t *testing.T) {
	plans := PlanOriginProbes(
		[]OriginExport{export("/aws/public", readOnlyStore, "index.html")},
		posixCfg(readOnlyStore))

	require.Len(t, plans, 1)
	require.Len(t, plans[0].Steps, 1)
	assert.Equal(t, ProbeOpRead, plans[0].Steps[0].Op)
	assert.Equal(t, "/aws/public/index.html", plans[0].ObjectPath)
	assert.Empty(t, plans[0].Body, "the object's contents are the administrator's business")
}

// A read-only export with nothing named to read yields no plan at all, rather
// than a plan pointing at a path Pelican invented.
func TestReadOnlyExportWithoutProbeObjectYieldsNoPlan(t *testing.T) {
	exports := []OriginExport{export("/aws/public", readOnlyStore, "")}

	assert.Empty(t, PlanOriginProbes(exports, posixCfg(readOnlyStore)))

	ok, why := ExportIsProbeable(exports[0], readOnlyStore)
	assert.False(t, ok)
	assert.Contains(t, why, "HealthProbeObject")

	gaps := UnprobedExports(exports, readOnlyStore)
	require.Contains(t, gaps, "/aws/public")
}

// Cost selects a cadence; it no longer decides whether a backend is probed. This
// is the difference between "S3 origins are unobservable" and "S3 origins are
// observed less often".
func TestExpensiveBackendsAreThrottledNotSkipped(t *testing.T) {
	cfg := posixCfg(readWriteStore)
	cfg.StorageType = server_structs.OriginStorageS3v2

	plans := PlanOriginProbes(
		[]OriginExport{export("/bucket/data", readWriteStore, "")}, cfg)

	require.Len(t, plans, 1, "a writable S3 export must still be probed")
	assert.Len(t, plans[0].Steps, 3, "including its writes")
	assert.Positive(t, plans[0].MinInterval, "but not on every cycle")
}

func TestCheapBackendsRunEveryCycle(t *testing.T) {
	for _, st := range []server_structs.OriginStorageType{
		server_structs.OriginStoragePosixv2,
		server_structs.OriginStoragePStore,
	} {
		cfg := posixCfg(readWriteStore)
		cfg.StorageType = st
		plans := PlanOriginProbes([]OriginExport{export("/ns", readWriteStore, "")}, cfg)
		require.Len(t, plans, 1)
		assert.Zero(t, plans[0].MinInterval, "%s writes are cheap", st)
	}
}

// Namespace policy is bounded by store ability. An export declaring writes its
// store cannot perform must not be write-probed: the probe would fail forever
// and blame the storage for a configuration error.
func TestNamespacePolicyIsBoundedByStoreAbility(t *testing.T) {
	overreaching := export("/ns", server_structs.Capabilities{Reads: true, Writes: true}, "sentinel.txt")

	plans := PlanOriginProbes([]OriginExport{overreaching}, posixCfg(readOnlyStore))

	require.Len(t, plans, 1)
	require.Len(t, plans[0].Steps, 1)
	assert.Equal(t, ProbeOpRead, plans[0].Steps[0].Op,
		"the store cannot write, so no write is attempted whatever the export claims")

	// And the contradiction is reported to the administrator separately.
	errs := ValidateOriginCapabilities([]OriginExport{overreaching}, readOnlyStore)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "Writes")
	assert.Contains(t, errs[0].Error(), "/ns")
}

func TestValidateOriginCapabilitiesAcceptsNarrowerNamespaces(t *testing.T) {
	// Offering less than the store can do is the normal, correct case.
	assert.Empty(t, ValidateOriginCapabilities(
		[]OriginExport{export("/ns", readOnlyStore, "s.txt")}, readWriteStore))
}

func TestCapabilitiesIntersectAndSubset(t *testing.T) {
	ns := server_structs.Capabilities{Reads: true, Writes: true, Listings: true}

	assert.True(t, readOnlyStore.Subset(readWriteStore))
	assert.False(t, ns.Subset(readOnlyStore))

	got := ns.Intersect(readOnlyStore)
	assert.True(t, got.Reads)
	assert.False(t, got.Writes, "a namespace cannot write through a store that cannot")
	assert.True(t, got.Listings)

	// PublicReads is an authorization property, but it still implies a read.
	pub := server_structs.Capabilities{PublicReads: true, Reads: true}
	assert.False(t, pub.Intersect(server_structs.Capabilities{}).PublicReads)
}

func TestAnyReads(t *testing.T) {
	assert.True(t, server_structs.Capabilities{Reads: true}.AnyReads())
	assert.True(t, server_structs.Capabilities{PublicReads: true}.AnyReads())
	assert.False(t, server_structs.Capabilities{Writes: true}.AnyReads())
}

// The Director probes the synthetic namespace, which stays synthetic and so can
// never claim the real data plane.
func TestMonitoringProbePlanStaysSynthetic(t *testing.T) {
	plan := MonitoringProbePlan(
		server_structs.DirectorTestSuite, "https://o:8443", "", nil, "posix",
		OriginProbeSteps(readWriteStore))

	require.True(t, plan.Binding.Synthetic)
	require.Len(t, plan.Steps, 3)

	var results []server_structs.TestResult
	for _, step := range plan.Steps {
		results = append(results, server_structs.TestResult{
			Binding: plan.Binding, Edges: step.Edges,
			Op: string(step.Op), Outcome: server_structs.TestOutcomePass,
		})
	}
	report := server_structs.NewTestReport(plan.Suite, plan.Expected, results)

	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz},
		report.Covered)
	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeOriginData},
		report.Uncovered,
		"only a probe of a real export can establish the storage edge")
}

func TestOriginProbeStepsRespectsWrites(t *testing.T) {
	assert.Len(t, OriginProbeSteps(readWriteStore), 3)
	assert.Empty(t, OriginProbeSteps(readOnlyStore),
		"with no write capability the Director has no object of its own to read")
}

func TestProbePlanKeyIsStablePerExportAndSuite(t *testing.T) {
	a := ProbePlan{Suite: server_structs.SelfTestSuite,
		Binding: server_structs.BindingRef{Namespace: "/ns1"}}
	b := ProbePlan{Suite: server_structs.SelfTestSuite,
		Binding: server_structs.BindingRef{Namespace: "/ns2"}}
	c := ProbePlan{Suite: server_structs.DirectorTestSuite,
		Binding: server_structs.BindingRef{Namespace: "/ns1"}}

	assert.NotEqual(t, a.Key(), b.Key())
	assert.NotEqual(t, a.Key(), c.Key())
	assert.Equal(t, a.Key(), ProbePlan{Suite: server_structs.SelfTestSuite,
		Binding: server_structs.BindingRef{Namespace: "/ns1"}}.Key())
}

func TestPlanOriginProbesCoversEveryProbeableExport(t *testing.T) {
	exports := []OriginExport{
		export("/writable", readWriteStore, ""),
		export("/readable", readOnlyStore, "probe.txt"),
		export("/opaque", readOnlyStore, ""),
		export("/inert", server_structs.Capabilities{}, ""),
	}

	plans := PlanOriginProbes(exports, posixCfg(readWriteStore))
	require.Len(t, plans, 2)

	gaps := UnprobedExports(exports, readWriteStore)
	assert.Contains(t, gaps, "/opaque")
	assert.Contains(t, gaps, "/inert")
	assert.NotContains(t, gaps, "/writable")
	assert.NotContains(t, gaps, "/readable")
}

func TestRemoteWriteIntervalIsSane(t *testing.T) {
	// A guard against someone setting this to something that hammers a bucket.
	assert.GreaterOrEqual(t, remoteWriteProbeInterval, time.Minute)
}

// The obstacle to probing real storage is per-backend, so the default is too.
//
// Natively-served backends are authorized by Pelican itself in
// origin_serve/authz.go, which the probe token satisfies. XRootD-fronted ones go
// through the generated scitokens.cfg and authfile, where probes into an export
// are refused for reasons not yet established -- so they stay off rather than
// producing failures that are really configuration artifacts.
func TestRealExportProbesDefaultPerBackend(t *testing.T) {
	// Apply the generated default the way a running server does. Without this the
	// test exercises an empty config, which is how the previous version of this
	// logic passed its unit test while being unreachable in production: viper
	// reports a key as "set" once a default exists, so the per-backend branch
	// never ran outside the test harness.
	ResetTestState()
	t.Cleanup(ResetTestState)
	viper.SetDefault(param.Origin_ProbeRealExports.GetName(), "auto")
	_, err := param.Refresh()
	require.NoError(t, err)
	require.Equal(t, "auto", param.Origin_ProbeRealExports.GetString())

	for _, native := range []server_structs.OriginStorageType{
		server_structs.OriginStoragePosixv2,
		server_structs.OriginStoragePStore,
		server_structs.OriginStorageS3v2,
		server_structs.OriginStorageHTTPSv2,
		server_structs.OriginStorageGlobusv2,
		server_structs.OriginStorageSSH,
	} {
		assert.True(t, RealExportProbesEnabled(native),
			"%s is served by Pelican, which authorizes the probe itself", native)
	}

	for _, fronted := range []server_structs.OriginStorageType{
		server_structs.OriginStoragePosix,
		server_structs.OriginStorageS3,
		server_structs.OriginStorageHTTPS,
		server_structs.OriginStorageGlobus,
		server_structs.OriginStorageXRoot,
	} {
		assert.False(t, RealExportProbesEnabled(fronted),
			"%s goes through XRootD authorization, which is not yet understood", fronted)
	}
}

// #3445: posixv2 origins never ran self tests. The blocker was the launcher, not
// config -- but simply starting the probe is not enough, because the synthetic
// /pelican/monitoring namespace only exists on XRootD-fronted origins. A native
// origin registers handlers for exactly its configured exports, so probing that
// namespace there is a guaranteed 404 that would report a healthy origin as
// broken.
//
// The two probe kinds are therefore complementary, and every backend must have
// exactly one that applies.
func TestEveryBackendHasExactlyOneApplicableProbeKind(t *testing.T) {
	all := []server_structs.OriginStorageType{
		server_structs.OriginStoragePosix,
		server_structs.OriginStoragePosixv2,
		server_structs.OriginStoragePStore,
		server_structs.OriginStorageSSH,
		server_structs.OriginStorageS3,
		server_structs.OriginStorageS3v2,
		server_structs.OriginStorageHTTPS,
		server_structs.OriginStorageHTTPSv2,
		server_structs.OriginStorageGlobus,
		server_structs.OriginStorageGlobusv2,
		server_structs.OriginStorageXRoot,
	}

	for _, st := range all {
		fronted := st.Profile().ServedByXRootD
		realExports := RealExportProbesEnabled(st)

		// The monitoring namespace is mounted only by the XRootD config template,
		// so it is probeable exactly when the backend is fronted.
		assert.NotEqual(t, fronted, realExports,
			"%s must have exactly one applicable probe kind: monitoring for fronted "+
				"backends, real exports for native ones", st)
	}
}

// The override must work in both directions, and an unrecognized value must fall
// back to the per-backend choice rather than to a silent "off".
func TestRealExportProbesExplicitOverride(t *testing.T) {
	ResetTestState()
	t.Cleanup(ResetTestState)

	fronted := server_structs.OriginStoragePosix
	native := server_structs.OriginStoragePosixv2

	for _, tc := range []struct {
		value       string
		wantFronted bool
		wantNative  bool
	}{
		{"true", true, true},
		{"false", false, false},
		{"auto", false, true},
		{"", false, true},         // unset behaves as auto
		{"nonsense", false, true}, // and so does anything unrecognized
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			require.NoError(t, param.Origin_ProbeRealExports.Set(tc.value))
			assert.Equal(t, tc.wantFronted, RealExportProbesEnabled(fronted))
			assert.Equal(t, tc.wantNative, RealExportProbesEnabled(native))
		})
	}
}
