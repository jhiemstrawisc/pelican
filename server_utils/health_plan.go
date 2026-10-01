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

// Deriving probe plans from bindings, and probing the storage clients actually use.
//
// Two things used to decide whether an origin was health tested, and both were
// the wrong thing:
//
//   - Origin.StorageType. config.go force-rewrote SelfTest and DirectorTest to
//     false for every non-POSIX backend, so S3-, HTTPS-, and Globus-backed
//     origins were unobservable by construction. The premise -- "a non-POSIX
//     origin cannot take a write probe" -- confuses cost with capability. A
//     writable S3 bucket takes writes perfectly well; they are merely dearer.
//
//   - Nothing at all, for which namespace got probed. Every probe went to the
//     synthetic /pelican/monitoring prefix, which on a POSIX origin resolves
//     under Origin.RunLocation while real exports are symlinks to the
//     administrator's volume. A probe there proved XRootD was up and proved
//     nothing whatever about the storage clients read.
//
// Plans are now derived from bindings. Which operations run comes from the
// capabilities the export declares; how often the expensive ones run comes from
// the backend's cost profile; and where they run is inside the export's own
// storage, so a passing probe means the thing a client depends on works.
//
// See https://github.com/PelicanPlatform/pelican/issues/2713 and
// docs/health-testing-design.md.
package server_utils

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/pelicanplatform/pelican/param"

	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// ProbeReservedPrefix is the directory, relative to an export's federation
// prefix, into which write probes place their scratch objects.
//
// Probing inside the export is the entire point: the object is written through
// the same serving layer, the same authorization decision, and into the same
// backing store a client uses, so a pass is evidence about the binding rather
// than about a namespace nobody reads. The leading dot keeps it out of ordinary
// listings, and the name is reserved -- an administrator who happens to have a
// collection by this name will find probes writing into it.
const ProbeReservedPrefix = ".pelican-monitoring"

// remoteWriteProbeInterval is the floor between write probes against a backend
// whose writes are not cheap.
//
// The gray-failure literature is explicit that overly active probing burdens an
// already degraded system, and a PUT to S3 is egress plus a credentialed round
// trip rather than a local disk write. The answer is a slower cadence, not the
// old answer of refusing to probe those backends at all: a five-minute write
// probe is enormously more observability than none.
const remoteWriteProbeInterval = 5 * time.Minute

// originDataEdges is the path a client read or write of an origin traverses.
var originDataEdges = []server_structs.Edge{
	server_structs.EdgeFront,
	server_structs.EdgeAuthz,
	server_structs.EdgeOriginData,
}

// OriginProbeConfig is everything plan derivation needs that is not part of an
// export.
type OriginProbeConfig struct {
	// StoreCaps is what the origin process can do to its backing store at all,
	// from the top-level Origin.Enable* settings. Namespace policy is bounded by
	// it: an export may offer less than the store can do, never more.
	StoreCaps server_structs.Capabilities
	// StorageType selects the backend cost profile.
	StorageType server_structs.OriginStorageType
	// BaseURL is the origin's data endpoint.
	BaseURL string
	// IssuerUrl is empty when the Director is planning, meaning the federation
	// issuer with the legacy fallback.
	IssuerUrl string
	// Audiences for the probe token.
	Audiences []string
}

// OriginStoreCapabilities reads the top-level Origin.Enable* settings, which
// express STORE ABILITY: what this process can do to its backing store at all,
// given its backend, its credentials, and how the storage is mounted.
//
// Distinct from an export's Capabilities, which express NAMESPACE POLICY: what
// the origin is willing to do on behalf of a particular federation prefix. The
// two share a struct and have been used interchangeably, with the top-level
// values copied down as defaults, which is what lost the distinction. See
// server_structs/capabilities.go.
func OriginStoreCapabilities() server_structs.Capabilities {
	publicReads := param.Origin_EnablePublicReads.GetBool()
	return server_structs.Capabilities{
		PublicReads: publicReads,
		Reads:       param.Origin_EnableReads.GetBool() || publicReads,
		Writes:      param.Origin_EnableWrites.GetBool(),
		Listings:    param.Origin_EnableListings.GetBool(),
		DirectReads: param.Origin_EnableDirectReads.GetBool(),
		Copies:      !param.Origin_DisableCopies.GetBool() && param.Origin_EnableWrites.GetBool(),
	}
}

// RealExportProbesEnabled reports whether probes may touch the storage behind an
// export, as opposed to only the synthetic /pelican/monitoring namespace.
//
// The default is per-backend rather than global, because the obstacle is
// per-backend.
//
// Native backends are served by the pelican process, which authorizes a request
// by verifying the token itself in origin_serve/authz.go: the issuer must be one
// the export lists, the export must declare the capability, and the scope is
// resolved against the federation prefix. A probe token issued by the origin's
// own external web URL satisfies all three, so nothing stands in the way.
//
// XRootD-fronted backends are not served by that code at all. Authorization there
// is the generated scitokens.cfg plus the generated authfile, and probes into an
// export are refused with EACCES for reasons that could not be established by
// reading the generation code -- see docs/health-testing-design.md. Every
// candidate fix has real blast radius on an authorization path that is known to
// be fragile, so the default there is off until someone can demonstrate a change
// is safe.
//
// Origin.ProbeRealExports overrides this in either direction; "auto" (the
// default) selects per backend as described above.
//
// That parameter is a string rather than a boolean on purpose. viper reports a
// key as "set" as soon as a default exists, so a boolean could not distinguish
// "the administrator chose false" from "nobody chose anything" -- and the
// per-backend branch became unreachable in a running server while still passing
// its unit test, because the test harness clears defaults.
func RealExportProbesEnabled(st server_structs.OriginStorageType) bool {
	switch strings.ToLower(strings.TrimSpace(param.Origin_ProbeRealExports.GetString())) {
	case "true", "yes", "on", "enabled":
		return true
	case "false", "no", "off", "disabled":
		return false
	default: // "auto", and anything unrecognized
		return !st.Profile().ServedByXRootD
	}
}

// PlanOriginProbes returns one plan per export, derived from what that export
// declares and what the store can actually do.
//
// The returned plans are ordered by federation prefix so a report's results are
// stable across cycles and diffable in the UI.
func PlanOriginProbes(exports []OriginExport, cfg OriginProbeConfig) []ProbePlan {
	plans := make([]ProbePlan, 0, len(exports))
	for _, export := range exports {
		if plan, ok := planExportProbe(export, cfg); ok {
			plans = append(plans, plan)
		}
	}
	return plans
}

// planExportProbe builds the probe for one export, or reports that none applies.
func planExportProbe(export OriginExport, cfg OriginProbeConfig) (ProbePlan, bool) {
	// Namespace policy bounded by store ability. An export declaring writes that
	// its store cannot perform must not be probed for writes: the probe would
	// fail every cycle and report a broken origin, when what is actually broken
	// is the configuration. ValidateOriginCapabilities surfaces that separately,
	// at startup, where an administrator will see it.
	effective := export.Capabilities.Intersect(cfg.StoreCaps)

	steps, minInterval := probeSteps(effective, cfg.StorageType)
	if len(steps) == 0 {
		return ProbePlan{}, false
	}

	binding := server_structs.BindingRef{
		Namespace:   export.FederationPrefix,
		StorageKind: string(cfg.StorageType),
		Issuer:      cfg.IssuerUrl,
		// Not synthetic. This is the storage a client reads.
		Synthetic: false,
	}

	plan := ProbePlan{
		Suite:       server_structs.SelfTestSuite,
		Binding:     binding,
		BaseURL:     cfg.BaseURL,
		Expected:    server_structs.ExpectedEdges(server_structs.OriginType),
		Steps:       steps,
		MinInterval: minInterval,
	}

	if effective.Writes {
		// Write probes mint their own object inside the export's reserved prefix,
		// so nothing needs to pre-exist and the object is cleaned up by the
		// delete step.
		probeDir := path.Join(export.FederationPrefix, ProbeReservedPrefix)
		plan.Prefix = probeDir
		plan.Body = SelfTestBody
		// Scope paths are absolute here and made relative to IssuerBasePath when
		// the token is minted. See ProbeScope: stating the path once, where it is
		// meant, is what keeps the issuer's base path from being subtracted zero
		// times or twice.
		plan.Auth = ProbeAuth{
			Issuer:         cfg.IssuerUrl,
			Subject:        "origin",
			Audiences:      cfg.Audiences,
			IssuerBasePath: export.FederationPrefix,
			Scopes: []ProbeScope{
				{Authorization: token_scopes.Wlcg_Storage_Read, Path: probeDir},
				{Authorization: token_scopes.Wlcg_Storage_Modify, Path: probeDir},
			},
		}
		return plan, true
	}

	// Read-only: the probe has to read something that already exists, and only
	// the administrator knows what that may be. With nothing named there is no
	// probe to run -- reported as an uncovered export by UnprobedExports, not as
	// a plan pointing at a path we invented.
	if export.HealthProbeObject == "" {
		return ProbePlan{}, false
	}
	plan.ObjectPath = path.Join(export.FederationPrefix, path.Clean(export.HealthProbeObject))
	// The object's contents are the administrator's business; that a read returns
	// bytes at all is the whole assertion.
	plan.Body = ""
	plan.Auth = ProbeAuth{
		Issuer:         cfg.IssuerUrl,
		Subject:        "origin",
		Audiences:      cfg.Audiences,
		IssuerBasePath: export.FederationPrefix,
		Scopes: []ProbeScope{
			{Authorization: token_scopes.Wlcg_Storage_Read, Path: plan.ObjectPath},
		},
	}
	return plan, true
}

// probeSteps decides which operations to run against a binding, and the floor
// between runs.
//
// Capability decides what runs; cost decides how often. Those were previously
// the same decision, made by storage type, which is why a writable S3 origin was
// not probed at all rather than probed less often.
func probeSteps(caps server_structs.Capabilities, st server_structs.OriginStorageType) ([]ProbeStep, time.Duration) {
	if caps.Writes {
		steps := []ProbeStep{
			{Op: ProbeOpWrite, Edges: originDataEdges},
			{Op: ProbeOpRead, Edges: originDataEdges},
			{Op: ProbeOpDelete, Edges: originDataEdges},
		}
		if st.HasCheapWrites() {
			return steps, 0
		}
		return steps, remoteWriteProbeInterval
	}

	if caps.AnyReads() {
		// A read probe with nothing to read is not a probe. The caller reports
		// this as an uncovered export rather than as a failure.
		return []ProbeStep{{Op: ProbeOpRead, Edges: originDataEdges}}, 0
	}

	return nil, 0
}

// ExportIsProbeable reports whether an export has enough configuration to be
// probed at all, and if not, why.
//
// A read-only export with no HealthProbeObject is the common case: there is
// nothing the origin knows it may read, so no probe can be planned. That is a
// legitimate state -- not every administrator can spare an object -- but it must
// be visible, because a federation administrator reading "ok" is otherwise
// entitled to assume the whole server was checked.
func ExportIsProbeable(export OriginExport, storeCaps server_structs.Capabilities) (bool, string) {
	effective := export.Capabilities.Intersect(storeCaps)

	switch {
	case effective.Writes:
		return true, ""
	case !effective.AnyReads():
		return false, "the namespace offers neither reads nor writes"
	case export.HealthProbeObject == "":
		return false, fmt.Sprintf(
			"the namespace is read-only and no HealthProbeObject is configured, so no object is known to be readable under %s",
			export.FederationPrefix)
	default:
		return true, ""
	}
}

// UnprobedExports names the exports no probe covers, and why.
func UnprobedExports(exports []OriginExport, storeCaps server_structs.Capabilities) map[string]string {
	gaps := map[string]string{}
	for _, export := range exports {
		if ok, why := ExportIsProbeable(export, storeCaps); !ok {
			gaps[export.FederationPrefix] = why
		}
	}
	return gaps
}

// ValidateOriginCapabilities checks every export's declared policy against what
// the store can do.
//
// Called at startup so a contradiction is reported once, loudly, to the person
// who can fix it -- rather than as a health test that fails every fifteen
// seconds and blames the storage for a configuration error.
func ValidateOriginCapabilities(exports []OriginExport, storeCaps server_structs.Capabilities) []error {
	var errs []error
	for _, export := range exports {
		if err := server_structs.ValidateNamespaceCapabilities(
			export.FederationPrefix, export.Capabilities, storeCaps); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// MonitoringProbePlan builds the probe of the synthetic /pelican/monitoring
// namespace.
//
// This remains the Director's probe of an origin, because the Director cannot
// safely write into an arbitrary export it does not administer, and it remains
// how caches are probed, since the Director serves the object itself. Its
// binding is marked synthetic, so it can never claim coverage of the storage a
// client reads -- that is what PlanOriginProbes is for.
func MonitoringProbePlan(suite server_structs.TestSuite, baseURL, issuerUrl string, audiences []string, storageKind string, steps []ProbeStep) ProbePlan {
	prefix := SelfTestPrefix()
	if suite == server_structs.DirectorTestSuite {
		prefix = path.Join(MonitoringBaseNs, DirectorTestDir)
	}

	return ProbePlan{
		Suite: suite,
		Binding: server_structs.BindingRef{
			Namespace:   MonitoringBaseNs,
			StorageKind: storageKind,
			Issuer:      issuerUrl,
			Synthetic:   true,
		},
		BaseURL: baseURL,
		Prefix:  prefix,
		Body:    ProbeBodyFor(suite),
		Auth: ProbeAuth{
			Issuer:         issuerUrl,
			Audiences:      audiences,
			IssuerBasePath: MonitoringBaseNs,
			// RawScope is already relative: "/" means the issuer's base path,
			// which is the monitoring namespace. Recorded here anyway so that a
			// later switch to typed Scopes inherits the right base path rather
			// than silently scoping to the federation root.
			RawScope: "storage.read:/ storage.modify:/",
		},
		Expected: server_structs.ExpectedEdges(server_structs.OriginType),
		Steps:    steps,
	}
}

// OriginProbeSteps returns the operations the Director may perform against an
// origin's monitoring namespace, given what that origin advertises.
//
// The Director probes the synthetic namespace, so the capability it must respect
// is store ability: can this origin write to its backing store at all. An origin
// advertising no write capability is sent no PUT, whatever backend it runs.
func OriginProbeSteps(caps server_structs.Capabilities) []ProbeStep {
	steps, _ := probeSteps(caps, server_structs.OriginStoragePosix)
	if !caps.Writes {
		// Without writes the Director has no object of its own in the monitoring
		// namespace to read, so there is nothing for it to run. The origin's own
		// self-test covers its real exports.
		return nil
	}
	return steps
}
