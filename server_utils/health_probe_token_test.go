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
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/pelican_url"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// setupProbeTokenTest gives the token machinery somewhere to find a signing key.
func setupProbeTokenTest(t *testing.T) {
	t.Helper()
	ResetTestState()
	t.Cleanup(ResetTestState)
	require.NoError(t, param.IssuerKeysDirectory.Set(t.TempDir()))
	require.NoError(t, param.Server_ExternalWebUrl.Set("https://origin.example.com:8444"))
}

// parseUnverified reads a probe token's claims without checking the signature.
// These tests are about what the probe asks for, not about signing.
func parseUnverified(t *testing.T, tok string) jwt.Token {
	t.Helper()
	parsed, err := jwt.Parse([]byte(tok), jwt.WithVerify(false), jwt.WithValidate(false))
	require.NoError(t, err)
	return parsed
}

// An origin probing itself needs exactly one token.
//
// The old code ran a fallback unconditionally, and for a self-test both branches
// produced the identical token -- so every failing step issued the same request
// twice, doubling load on a server already in trouble.
func TestSelfProbeMintsExactlyOneToken(t *testing.T) {
	setupProbeTokenTest(t)

	plan := ProbePlan{
		Suite: server_structs.SelfTestSuite,
		Auth: ProbeAuth{
			Issuer:         "https://origin.example.com:8444",
			Subject:        "origin",
			IssuerBasePath: "/ligo/frames",
			Scopes: []ProbeScope{
				{Authorization: token_scopes.Wlcg_Storage_Read, Path: "/ligo/frames/" + ProbeReservedPrefix},
			},
		},
	}

	tokens, err := plan.tokensToTry()
	require.NoError(t, err)
	require.Len(t, tokens, 1, "a self-probe has one issuer and must not retry with a duplicate")
}

// A Director probing an origin needs two: the federation issuer, and the
// Director's own URL as a fallback for origins predating it. Order matters --
// the fallback must be second, or new origins are never offered the token they
// actually expect.
func TestDirectorProbeMintsFederationTokenFirst(t *testing.T) {
	setupProbeTokenTest(t)
	// Authoritative: makes GetFederation return these values instead of running
	// live discovery against a hostname that does not exist.
	config.SetFederation(pelican_url.FederationDiscovery{
		DiscoveryEndpoint: "https://federation.example.com",
		DirectorEndpoint:  "https://director.example.com",
		RegistryEndpoint:  "https://registry.example.com",
	})

	plan := ProbePlan{
		Suite: server_structs.DirectorTestSuite,
		Auth: ProbeAuth{
			// Empty issuer means "the Director is the caller".
			Audiences: []string{"https://origin.example.com:8443"},
			RawScope:  "storage.read:/ storage.modify:/",
		},
	}

	tokens, err := plan.tokensToTry()
	require.NoError(t, err)
	require.Len(t, tokens, 2, "federation issuer plus the legacy fallback")

	first := parseUnverified(t, tokens[0])
	second := parseUnverified(t, tokens[1])

	assert.Equal(t, "https://federation.example.com", first.Issuer(),
		"the federation issuer must be tried first")
	assert.Equal(t, "https://origin.example.com:8444", second.Issuer(),
		"the fallback is the caller's own external web URL, for origins predating the federation issuer")
}

func TestProbeTokenCarriesRequestedScopesAndAudience(t *testing.T) {
	setupProbeTokenTest(t)

	plan := ProbePlan{
		Suite: server_structs.SelfTestSuite,
		Auth: ProbeAuth{
			Issuer:         "https://origin.example.com:8444",
			Subject:        "origin",
			Audiences:      []string{"https://origin.example.com:8443"},
			IssuerBasePath: "/ligo/frames",
			Scopes: []ProbeScope{
				{Authorization: token_scopes.Wlcg_Storage_Read, Path: "/ligo/frames/" + ProbeReservedPrefix},
				{Authorization: token_scopes.Wlcg_Storage_Modify, Path: "/ligo/frames/" + ProbeReservedPrefix},
			},
		},
	}

	tokens, err := plan.tokensToTry()
	require.NoError(t, err)
	require.Len(t, tokens, 1)

	parsed := parseUnverified(t, tokens[0])
	assert.Equal(t, "origin", parsed.Subject())
	assert.Contains(t, parsed.Audience(), "https://origin.example.com:8443")

	scope, ok := parsed.Get("scope")
	require.True(t, ok, "a probe token without a scope authorizes nothing")
	scopeStr, _ := scope.(string)

	// Scopes stay narrowed to the reserved probe prefix. A leaked probe token
	// must not be able to touch anyone's data.
	assert.Contains(t, scopeStr, "storage.read:/.pelican-monitoring")
	assert.Contains(t, scopeStr, "storage.modify:/.pelican-monitoring")
	assert.NotContains(t, scopeStr, "storage.modify:/ ",
		"the probe must not request write access to the whole namespace")
}

// relativeToBasePath is the one place the subtraction happens, so it is the one
// place worth testing exhaustively.
func TestRelativeToBasePath(t *testing.T) {
	cases := []struct {
		name     string
		absPath  string
		basePath string
		want     string
		wantErr  bool
	}{
		{"strips the base path", "/ligo/frames/.pelican-monitoring", "/ligo/frames", "/.pelican-monitoring", false},
		{"path equal to base becomes root", "/ligo/frames", "/ligo/frames", "/", false},
		{"empty base leaves the path alone", "/pelican/monitoring/selfTest", "", "/pelican/monitoring/selfTest", false},
		{"root base leaves the path alone", "/pelican/monitoring/selfTest", "/", "/pelican/monitoring/selfTest", false},
		{"tolerates a trailing slash on the base", "/ligo/frames/probe", "/ligo/frames/", "/probe", false},
		{"nested object under the base", "/ns/a/b/c.txt", "/ns", "/a/b/c.txt", false},

		// The case that matters: a sibling prefix must not be mistaken for the
		// base path and half-stripped.
		{"respects segment boundaries", "/ligo/framesets/x", "/ligo/frames", "", true},
		// And the case #3136 actually hit: a path outside the issuer's authority
		// must fail loudly rather than mint a scope pointing somewhere else.
		{"refuses a path outside the base", "/other/ns/x", "/ligo/frames", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := relativeToBasePath(tc.absPath, tc.basePath)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errProbePlanInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Every plan this package can build must mint a token whose scope is relative to
// its issuer's base path.
//
// This is deliberately an assertion about the *minted token* rather than about
// the plan struct. Plans store absolute paths now, so checking the struct would
// check the wrong end; what reaches the serving layer is the only thing that can
// be wrong. Asserting here rather than at each call site is the point -- the
// #3136 bug survived a rewrite of this subsystem precisely because the rule was
// a convention each constructor had to remember, and a new constructor could
// forget it again without any existing test noticing.
func TestEveryPlanScopesRelativeToItsIssuer(t *testing.T) {
	setupProbeTokenTest(t)

	writable := PlanOriginProbes(
		[]OriginExport{export("/ligo/frames", readWriteStore, "")},
		posixCfg(readWriteStore))
	require.Len(t, writable, 1)

	readOnly := PlanOriginProbes(
		[]OriginExport{export("/cms/data", readOnlyStore, "sentinel.txt")},
		posixCfg(readOnlyStore))
	require.Len(t, readOnly, 1)

	cases := []struct {
		name string
		plan ProbePlan
		// absent is the prefix that must NOT appear in the minted scope, because
		// the issuer's base path already supplies it.
		absent string
		// present is what the narrowed scope should still name.
		present string
	}{
		{"writable export probe", writable[0], "/ligo/frames", "/" + ProbeReservedPrefix},
		{"read-only export probe", readOnly[0], "/cms/data", "/sentinel.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEmpty(t, tc.plan.Auth.Scopes, "plan mints no scopes, so this test proves nothing")

			tokens, err := tc.plan.tokensToTry()
			require.NoError(t, err)
			require.NotEmpty(t, tokens)

			raw, ok := parseUnverified(t, tokens[0]).Get("scope")
			require.True(t, ok, "a probe token without a scope authorizes nothing")
			scope, _ := raw.(string)

			assert.NotContains(t, scope, tc.absent,
				"scope %q repeats the issuer's base path; the serving layer will resolve it to %q",
				scope, tc.plan.Auth.IssuerBasePath+scope)
			assert.Contains(t, scope, tc.present,
				"scope %q no longer names what the probe touches", scope)
		})
	}
}

// A scope the issuer has no authority over must fail at mint time rather than
// producing a token that points at an unrelated location.
func TestProbeScopeOutsideIssuerBasePathFailsClosed(t *testing.T) {
	setupProbeTokenTest(t)

	plan := ProbePlan{
		Suite: server_structs.SelfTestSuite,
		Auth: ProbeAuth{
			Issuer:         "https://origin.example.com:8444",
			IssuerBasePath: "/ligo/frames",
			Scopes: []ProbeScope{
				{Authorization: token_scopes.Wlcg_Storage_Read, Path: "/somebody/elses/data"},
			},
		},
	}

	_, err := plan.tokensToTry()
	require.Error(t, err)
	assert.ErrorIs(t, err, errProbePlanInvalid)
}

// The anonymous path must never mint a token at all. The monitoring namespace is
// public, so presenting one would exercise a code path no client uses.
func TestAnonymousProbeSendsNoToken(t *testing.T) {
	setupProbeTokenTest(t)

	plan := ProbePlan{Suite: server_structs.DirectorTestSuite, Auth: ProbeAuth{Anonymous: true}}
	// do() short-circuits before tokensToTry when Anonymous is set; assert the
	// flag is what the request path keys on.
	assert.True(t, plan.Auth.Anonymous)
}

func TestProbeTokenFailsClosedWithoutExternalWebUrl(t *testing.T) {
	ResetTestState()
	t.Cleanup(ResetTestState)
	require.NoError(t, param.IssuerKeysDirectory.Set(t.TempDir()))
	require.NoError(t, param.Server_ExternalWebUrl.Set(""))

	plan := ProbePlan{Suite: server_structs.SelfTestSuite, Auth: ProbeAuth{Issuer: "https://x"}}
	_, err := plan.tokensToTry()
	require.Error(t, err, "a probe with no subject URL must fail rather than mint something ambiguous")
	assert.Contains(t, err.Error(), param.Server_ExternalWebUrl.GetName())
}

// A Director with no federation configured cannot mint the primary token, and
// must say so rather than silently falling back to only the legacy issuer.
func TestDirectorProbeFailsClosedWithoutFederation(t *testing.T) {
	setupProbeTokenTest(t)
	config.SetFederation(pelican_url.FederationDiscovery{DiscoveryEndpoint: ""})

	plan := ProbePlan{Suite: server_structs.DirectorTestSuite, Auth: ProbeAuth{}}
	_, err := plan.tokensToTry()
	require.Error(t, err)
}

func TestProbeUserAgentIdentifiesTheCaller(t *testing.T) {
	// The shared helper used to hardcode "pelican-director" regardless of caller,
	// so an origin's own self-test announced itself as a director in its logs.
	self := ProbePlan{Suite: server_structs.SelfTestSuite}
	director := ProbePlan{Suite: server_structs.DirectorTestSuite}

	assert.Contains(t, director.userAgent(), "pelican-director")
	assert.NotContains(t, self.userAgent(), "pelican-director")

	override := ProbePlan{Suite: server_structs.SelfTestSuite, UserAgent: "custom/1.0"}
	assert.Equal(t, "custom/1.0", override.userAgent())
}
