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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/pelican_url"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/test_utils"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// setupMonitoringNsTest stands up a Director with a signing key and a federation
// root that really serves the matching JWKS.
//
// A mock that only names a JWKS URI is not enough here: verifying a probe token
// fetches that document, so the tests would pass or fail on DNS rather than on
// the authorization logic. Returns the context and the federation's discovery
// endpoint, which is the issuer these tokens must carry.
func setupMonitoringNsTest(t *testing.T) (*gin.Context, string) {
	t.Helper()
	setGinTestMode()
	t.Cleanup(test_utils.SetupTestLogging(t))

	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	confDir := t.TempDir()
	require.NoError(t, param.ConfigBase.Set(confDir))
	require.NoError(t, param.IssuerKeysDirectory.Set(filepath.Join(confDir, "keys")))
	require.NoError(t, param.TLSSkipVerify.Set(true))

	pKeySet, err := config.GetIssuerPublicJWKS()
	require.NoError(t, err, "failed to load the public JWKS the mock federation will serve")

	test_utils.MockFederationRoot(t, nil, &pKeySet)
	require.NoError(t, initServerForTest(t, c, server_structs.DirectorType))

	fedInfo, err := config.GetFederation(c)
	require.NoError(t, err)
	require.NotEmpty(t, fedInfo.DiscoveryEndpoint)

	return c, fedInfo.DiscoveryEndpoint
}

// The capabilities on these two namespace ads are the entire mechanism. A cache
// reads them to decide whether to demand a token from its client and whether to
// present one upstream, so getting PublicReads wrong silently turns the
// authenticated probe back into an anonymous one.
func TestMonitoringNamespaceAdCapabilities(t *testing.T) {
	c, fedDiscovery := setupMonitoringNsTest(t)

	t.Run("the public namespace stays public", func(t *testing.T) {
		ad, err := monitoringNamespaceAd(c, server_utils.MonitoringBaseNs+"/directorTest/x.txt")
		require.NoError(t, err)

		assert.Equal(t, server_utils.MonitoringBaseNs, ad.Path)
		assert.True(t, ad.Caps.PublicReads)
		assert.Empty(t, ad.Issuer, "a public namespace needs no issuer")
	})

	t.Run("the auth namespace is protected and names the federation issuer", func(t *testing.T) {
		ad, err := monitoringNamespaceAd(c, server_utils.MonitoringAuthNs+"/director-1/2026-01-01/x.txt")
		require.NoError(t, err)

		assert.Equal(t, server_utils.MonitoringAuthNs, ad.Path)
		assert.False(t, ad.Caps.PublicReads,
			"a public namespace never causes a cache to present a token, which is the whole probe")
		assert.True(t, ad.Caps.Reads)

		// GenerateCacheIssuers only collects issuers from non-public namespaces,
		// and keys the cache's scitokens entry by issuer with these base paths.
		require.Len(t, ad.Issuer, 1)
		assert.Equal(t, fedDiscovery, ad.Issuer[0].IssuerUrl.String())
		assert.Equal(t, []string{server_utils.MonitoringAuthNs}, ad.Issuer[0].BasePaths)
	})
}

// A cache builds its scitokens configuration from this listing, so the protected
// namespace has to appear in it or the cache has no trust configured for the
// probe's token and refuses every authenticated read.
func TestMonitoringNamespaceAdsIncludeBoth(t *testing.T) {
	c, _ := setupMonitoringNsTest(t)

	ads := monitoringNamespaceAds(c)
	require.Len(t, ads, 2)

	paths := []string{ads[0].Path, ads[1].Path}
	assert.Contains(t, paths, server_utils.MonitoringBaseNs)
	assert.Contains(t, paths, server_utils.MonitoringAuthNs)
}

// A Director that cannot name its own federation issuer should still serve the
// public namespace it always has, rather than failing the listing and taking
// every cache's configuration down with it.
func TestMonitoringNamespaceAdsDegradeWithoutAFederationIssuer(t *testing.T) {
	setGinTestMode()
	t.Cleanup(test_utils.SetupTestLogging(t))
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	config.ResetFederationForTest()
	config.SetFederation(pelican_url.FederationDiscovery{
		DirectorEndpoint: "https://dne-director.example.com",
	})

	ads := monitoringNamespaceAds(c)
	require.Len(t, ads, 1)
	assert.Equal(t, server_utils.MonitoringBaseNs, ads[0].Path)
}

// mintFedToken signs a token as the federation with the given scope string.
func mintFedToken(t *testing.T, issuer, scope string) string {
	t.Helper()
	cfg := token.NewWLCGToken()
	cfg.Lifetime = time.Minute
	cfg.Issuer = issuer
	cfg.Subject = "some-cache.example.com"
	cfg.AddAudienceAny()
	if scope != "" {
		cfg.AddRawScope(scope)
	}
	tok, err := cfg.CreateToken()
	require.NoError(t, err)
	return tok
}

// requestWithToken builds a gin context carrying a bearer token, as the cache
// and the probe both do.
func requestWithToken(t *testing.T, objectPath, tok string) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1.0/director/healthTest"+objectPath, nil)
	if tok != "" {
		c.Request.Header.Set("Authorization", "Bearer "+tok)
	}
	return c
}

// The Director advertises the protected namespace as needing a token; it has to
// actually require one, or the probe passes without a token ever being
// exercised and reports coverage it did not earn.
func TestAuthorizeMonitoringRead(t *testing.T) {
	_, fedDiscovery := setupMonitoringNsTest(t)

	objectPath := server_utils.MonitoringAuthNs + "/director-1/2026-01-01/director-test-x.txt"

	t.Run("no token is refused", func(t *testing.T) {
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, ""), objectPath)
		require.Error(t, err)
		assert.GreaterOrEqual(t, status, http.StatusUnauthorized)
	})

	t.Run("a token with no scope authorizes nothing", func(t *testing.T) {
		tok := mintFedToken(t, fedDiscovery, "")
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.Error(t, err)
		assert.Equal(t, http.StatusForbidden, status)
	})

	t.Run("a token scoped elsewhere is refused", func(t *testing.T) {
		tok := mintFedToken(t, fedDiscovery, "storage.read:/somebody/elses/data")
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.Error(t, err)
		assert.Equal(t, http.StatusForbidden, status)
	})

	// This is the case that matters most in practice: the federation token a
	// cache holds is scoped to the prefixes it may serve, most often just "/".
	// The shared scope validator compares strings for equality and would reject
	// it, which is why this check uses ResourceScope containment instead.
	t.Run("a root-scoped federation token is accepted", func(t *testing.T) {
		tok := mintFedToken(t, fedDiscovery, "storage.read:/")
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("a token scoped to the monitoring namespace is accepted", func(t *testing.T) {
		tok := mintFedToken(t, fedDiscovery, "storage.read:"+server_utils.MonitoringAuthNs)
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	// Segment boundaries: a sibling prefix must not be read as covering this one.
	t.Run("a sibling prefix does not cover the namespace", func(t *testing.T) {
		tok := mintFedToken(t, fedDiscovery, "storage.read:"+server_utils.MonitoringAuthNs+"-other")
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.Error(t, err)
		assert.Equal(t, http.StatusForbidden, status)
	})

	// A scope relative to the namespace, which is how a token minted for a client
	// of this namespace is written and how the cache resolves it. When the cache
	// forwards that token upstream the Director has to apply the base path it
	// advertised, or it rejects a token it just told the cache to accept.
	t.Run("a scope relative to the namespace is accepted", func(t *testing.T) {
		relative := strings.TrimPrefix(objectPath, server_utils.MonitoringAuthNs)
		tok := mintFedToken(t, fedDiscovery, "storage.read:"+relative)
		status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})
}

// Three callers reach this handler and they do not agree on where a token goes.
// XRootD's client plugin sends "authz", the persistent cache and the Pelican
// client send "access_token", and a direct caller uses the Authorization header.
// Missing any one of them fails a probe for a reason that looks like
// authorization and is really plumbing.
func TestAuthorizeMonitoringReadAcceptsEveryTokenLocation(t *testing.T) {
	_, fedDiscovery := setupMonitoringNsTest(t)

	objectPath := server_utils.MonitoringAuthNs + "/director-1/2026-01-01/director-test-x.txt"
	tok := mintFedToken(t, fedDiscovery, "storage.read:/")

	newRequest := func() *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet,
			"/api/v1.0/director/healthTest"+objectPath, nil)
		return c
	}

	t.Run("Authorization header", func(t *testing.T) {
		c := newRequest()
		c.Request.Header.Set("Authorization", "Bearer "+tok)
		status, err := authorizeMonitoringRead(c, objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("authz query parameter", func(t *testing.T) {
		c := newRequest()
		q := c.Request.URL.Query()
		q.Set("authz", tok)
		c.Request.URL.RawQuery = q.Encode()
		status, err := authorizeMonitoringRead(c, objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	// The one that was missing. The persistent cache presents its federation
	// token here, so without this the authenticated probe failed against it.
	t.Run("access_token query parameter", func(t *testing.T) {
		c := newRequest()
		q := c.Request.URL.Query()
		q.Set("access_token", tok)
		c.Request.URL.RawQuery = q.Encode()
		status, err := authorizeMonitoringRead(c, objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	// Tolerated because callers that build the parameter from an Authorization
	// header sometimes leave the scheme on.
	t.Run("access_token with a Bearer prefix", func(t *testing.T) {
		c := newRequest()
		q := c.Request.URL.Query()
		q.Set("access_token", "Bearer "+tok)
		c.Request.URL.RawQuery = q.Encode()
		status, err := authorizeMonitoringRead(c, objectPath)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})
}

// Restricting which of the federation's data a cache may serve must not silently
// switch off health testing for that cache.
//
// AllowedPrefixes is about data. The monitoring namespace holds nothing but
// probe objects the Director generates, and reading one is how the Director
// learns the cache can handle a federation token at all.
func TestFedTokenAlwaysReachesTheMonitoringNamespace(t *testing.T) {
	c, _ := setupMonitoringNsTest(t)

	restricted := map[string]map[string]struct{}{
		"test-cache.example.com": {"/foo": struct{}{}},
	}
	allowedPrefixesForCaches.Store(&restricted)

	tok, err := createFedTok(c, requestInfo{
		Host:  "test-cache.example.com",
		SType: server_structs.CacheType,
	})
	require.NoError(t, err)

	parsed, err := token.UnsafeParseClaims(tok)
	require.NoError(t, err)
	rawScope, present := parsed.Get("scope")
	require.True(t, present)
	scope, _ := rawScope.(string)

	assert.Contains(t, scope, "storage.read:/foo",
		"the registry's restriction must still be honored for data")
	assert.Contains(t, scope, "storage.read:"+server_utils.MonitoringAuthNs,
		"a cache restricted to a subset of the federation must still be health testable")

	// And the token it produces must actually satisfy the Director's own check.
	objectPath := server_utils.MonitoringAuthNs + "/director-1/2026-01-01/director-test-x.txt"
	status, err := authorizeMonitoringRead(requestWithToken(t, objectPath, tok), objectPath)
	require.NoError(t, err, "the token the Director mints for a cache must pass the Director's own door")
	assert.Equal(t, http.StatusOK, status)
}

// An unrestricted cache gets "/" and needs no separate monitoring scope, but a
// duplicate would be harmless noise on every token. Assert we do not add one.
func TestFedTokenDoesNotDuplicateScopes(t *testing.T) {
	c, _ := setupMonitoringNsTest(t)

	monitoringOnly := map[string]map[string]struct{}{
		"test-cache.example.com": {server_utils.MonitoringAuthNs: struct{}{}},
	}
	allowedPrefixesForCaches.Store(&monitoringOnly)

	tok, err := createFedTok(c, requestInfo{
		Host:  "test-cache.example.com",
		SType: server_structs.CacheType,
	})
	require.NoError(t, err)

	parsed, err := token.UnsafeParseClaims(tok)
	require.NoError(t, err)
	rawScope, _ := parsed.Get("scope")
	scope, _ := rawScope.(string)

	want, err := token_scopes.Wlcg_Storage_Read.Path(server_utils.MonitoringAuthNs)
	require.NoError(t, err)
	assert.Equal(t, want.String(), scope)
}
