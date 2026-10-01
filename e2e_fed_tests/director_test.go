//go:build !windows

/***************************************************************
*
* Copyright (C) 2025, Pelican Project, Morgridge Institute for Research
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

package fed_tests

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/cache"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/fed_test_utils"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/pelican_url"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/test_utils"
	"github.com/pelicanplatform/pelican/token"
)

//go:embed resources/both-public.yml
var bothPubNamespaces string

type serverAdUnmarshal struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Queries the cache for a director test file -- this mimics the way the Pelican
// process at the cache behaves, as its responsible for requesting files from its
// own XRootD component
func TestDirectorCacheHealthTest(t *testing.T) {
	t.Cleanup(test_utils.SetupTestLogging(t))
	// Spin up a federation
	_ = fed_test_utils.NewFedTest(t, bothPubNamespaces)

	ctx := context.Background()
	ctx, _, _ = test_utils.TestContext(ctx, t)
	fedInfo, err := config.GetFederation(ctx)
	require.NoError(t, err, "Failed to get federation service info")

	directorUrlStr := fedInfo.DirectorEndpoint
	directorUrl, err := url.Parse(directorUrlStr)
	require.NoError(t, err, "Failed to parse director URL")

	// There is no cache that will advertise the /pelican/monitoring namespace directly,
	// so we first discover a cache, then ask for the file. To do that, hit the Director's
	// server list endpoint and iterate through the servers until we find a cache.
	listPath, err := url.JoinPath("api", "v1.0", "director_ui", "servers")
	require.NoError(t, err, "Failed to join server list path")
	directorUrl.Path = listPath
	request, err := http.NewRequest("GET", directorUrl.String(), nil)
	require.NoError(t, err, "Failed to create HTTP request against server list path")

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := http.Client{Transport: tr}
	resp, err := client.Do(request)
	assert.NoError(t, err, "Failed to get response")
	defer func() {
		_ = resp.Body.Close()
	}()
	require.Equal(t, resp.StatusCode, http.StatusOK, "Failed to get server list from director")
	dirBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read server list body")

	// Unmarshal the body into a slice of dummy server ads. Can't use the actual server_structs.ServerAd
	// struct without a custom unmarshaler (because of string --> url conversion)
	var serverAds []serverAdUnmarshal
	err = json.Unmarshal(dirBody, &serverAds)
	require.NoError(t, err, "Failed to unmarshal server ads")
	var cacheUrlStr string
	found := false
	for _, serverAd := range serverAds {
		if serverAd.Type == server_structs.CacheType.String() {
			cacheUrlStr = serverAd.URL
			found = true
			break
		}
	}
	require.True(t, found, "Failed to find a cache server in the server list")
	cacheUrl, err := url.Parse(cacheUrlStr)
	require.NoError(t, err, "Failed to parse cache URL")

	// Now ask the cache for the director test file. When it gets the request,
	// it'll turn around and ask the director for the file, exactly as it would
	// if the cache's own self-test utility requested the file.
	cachePath, err := url.JoinPath(server_utils.MonitoringBaseNs, "directorTest",
		server_structs.DirectorTestSuite.String()+"-2006-01-02T15:04:10Z.txt")
	require.NoError(t, err, "Failed to join path")

	cacheUrl.Path = cachePath
	request, err = http.NewRequest("GET", cacheUrl.String(), nil)
	require.NoError(t, err, "Failed to create HTTP request against cache")

	resp, err = client.Do(request)
	assert.NoError(t, err, "Failed to get response")
	defer func() {
		_ = resp.Body.Close()
	}()
	cacheBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read cache body")
	assert.Equal(t, resp.StatusCode, http.StatusOK, "Failed to get director test file from cache")
	assert.Contains(t, string(cacheBody), "This object was created by the Pelican director-test functionality")
}

// healthDetailUnmarshal is the slice of the Director's server response this test
// cares about. The full struct is unexported in the director package.
type healthDetailUnmarshal struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	HealthStatus string `json:"healthStatus"`
	HealthDetail *struct {
		DirectorProbe *struct {
			Outcome   string   `json:"outcome"`
			Covered   []string `json:"covered"`
			Uncovered []string `json:"uncovered"`
			Results   []struct {
				Op      string `json:"op"`
				Outcome string `json:"outcome"`
				Reason  string `json:"reason"`
				Detail  string `json:"detail"`
				Binding struct {
					Namespace string `json:"namespace"`
				} `json:"binding"`
			} `json:"results"`
		} `json:"directorProbe"`
	} `json:"healthDetail"`
}

// fetchCacheHealth polls the Director's server list until it has a first-hand
// probe result for a cache.
func fetchCacheHealth(t *testing.T, client *http.Client, directorUrlStr string) healthDetailUnmarshal {
	t.Helper()

	listUrl, err := url.Parse(directorUrlStr)
	require.NoError(t, err)
	listUrl.Path = "/api/v1.0/director_ui/servers"

	var last healthDetailUnmarshal
	require.Eventually(t, func() bool {
		resp, err := client.Get(listUrl.String())
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			return false
		}

		var servers []healthDetailUnmarshal
		if err := json.Unmarshal(body, &servers); err != nil {
			return false
		}
		for _, server := range servers {
			if server.Type != server_structs.CacheType.String() {
				continue
			}
			last = server
			if server.HealthDetail != nil && server.HealthDetail.DirectorProbe != nil {
				return true
			}
		}
		return false
	}, 60*time.Second, 500*time.Millisecond,
		"the Director never produced a first-hand probe result for a cache")

	return last
}

// The Director's probe of a cache must cover both branches of the cache path and
// the authenticated miss, end to end against a real cache.
//
// This is the assertion that the coverage ledger is telling the truth rather
// than merely being self-consistent. Every other test of this machinery builds a
// report by hand from a plan; this one stands up a real federation, lets the
// Director run its real probe loop against a real XRootD cache, and reads what
// the Director concluded.
//
// Three distinct things have to work for it to pass, and none of them is
// exercised anywhere else:
//
//   - The protected namespace must reach the cache's scitokens configuration,
//     so the cache accepts the probe's federation-issued token on the way in.
//   - The cache must then fetch an object it does not hold from a namespace that
//     requires authorization, carrying that token upstream.
//   - The Director must serve that object only to a caller holding such a token,
//     resolving the scope the same way it told the cache to.
//
// It also pins the one edge that remains uncovered. See the comment on
// cacheAuthTestPlan: a cache presents its own federation token only to an origin
// that demands one, and the Director's synthetic origin does not.
func TestDirectorCacheProbeCoversEveryEdge(t *testing.T) {
	t.Cleanup(test_utils.SetupTestLogging(t))
	_ = fed_test_utils.NewFedTest(t, bothPubNamespaces)

	ctx := context.Background()
	ctx, _, _ = test_utils.TestContext(ctx, t)
	fedInfo, err := config.GetFederation(ctx)
	require.NoError(t, err, "Failed to get federation service info")

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}

	cacheHealth := fetchCacheHealth(t, client, fedInfo.DirectorEndpoint)
	probe := cacheHealth.HealthDetail.DirectorProbe
	require.NotNil(t, probe)

	// Report every failing step before asserting, so a failure here says which
	// hop broke rather than only that coverage was short.
	for _, res := range probe.Results {
		if res.Outcome != "pass" {
			t.Logf("probe step %q on %q: %s (%s) %s",
				res.Op, res.Binding.Namespace, res.Outcome, res.Reason, res.Detail)
		}
	}

	assert.Equal(t, "pass", probe.Outcome,
		"the Director's probe of a real cache should pass on every step")

	// The ledger, pinned exactly. Everything but the cache's own federation
	// token is now reached by some plan.
	assert.Equal(t, []string{"fed-token"}, probe.Uncovered,
		"the federation-token edge is the only one left; if this changed, the docs need to change too")

	for _, edge := range []string{
		"front", "authz", "cache-hit",
		"director-resolve", "upstream-fetch", "cache-write",
	} {
		assert.Contains(t, probe.Covered, edge, "edge %q was not covered", edge)
	}

	// And the authenticated plan really ran, against the protected namespace.
	var sawProtected bool
	for _, res := range probe.Results {
		if res.Binding.Namespace == server_utils.MonitoringAuthNs {
			sawProtected = true
		}
	}
	assert.True(t, sawProtected,
		"no probe step touched %s, so the federation-token edge was not really exercised",
		server_utils.MonitoringAuthNs)
}

// The protected namespace must actually be protected.
//
// If the Director advertises it as requiring a token but serves it to anyone,
// the probe above passes while proving nothing -- the exact failure this whole
// subsystem exists to prevent.
func TestDirectorProtectedMonitoringNamespaceRefusesAnonymousReads(t *testing.T) {
	t.Cleanup(test_utils.SetupTestLogging(t))
	_ = fed_test_utils.NewFedTest(t, bothPubNamespaces)

	ctx := context.Background()
	ctx, _, _ = test_utils.TestContext(ctx, t)
	fedInfo, err := config.GetFederation(ctx)
	require.NoError(t, err, "Failed to get federation service info")

	objectUrl, err := url.Parse(fedInfo.DirectorEndpoint)
	require.NoError(t, err)
	objectUrl.Path = "/api/v1.0/director/healthTest" + server_utils.MonitoringAuthNs +
		"/some-director/2026-01-01/" + server_structs.DirectorTestSuite.String() +
		"-2026-01-01T00:00:00Z.txt"

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := client.Get(objectUrl.String())
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.GreaterOrEqual(t, resp.StatusCode, http.StatusUnauthorized,
		"the Director served a protected probe object without a token")
	assert.Less(t, resp.StatusCode, 500,
		"refusing an unauthenticated read should be a 401 or 403, not a server error")
}

func updateAllowedPrefixesForCache(t *testing.T, dbPath string, cacheHost string, allowedPrefixes []string) {
	// We treat the absence of this custom field differently than its presence
	// and an empty list.
	if len(allowedPrefixes) == 0 {
		return
	}

	db, err := server_utils.InitSQLiteDB(dbPath)
	require.NoError(t, err, "Failed to connect to registry database")
	defer func() {
		_ = server_utils.ShutdownDB(db)
	}()

	var namespace server_structs.Registration
	result := db.Where("prefix = ?", "/caches/"+cacheHost).First(&namespace)
	require.NoError(t, result.Error, "Failed to find namespace for host %s: %v", cacheHost, result.Error)

	if namespace.CustomFields == nil {
		namespace.CustomFields = make(map[string]interface{})
	}
	namespace.CustomFields["AllowedPrefixes"] = allowedPrefixes

	result = db.Model(&namespace).Updates(server_structs.Registration{
		CustomFields: namespace.CustomFields,
	})
	require.NoError(t, result.Error, "Failed to update namespace for host %s: %v", cacheHost, result.Error)
	if result.RowsAffected == 0 {
		require.Fail(t, "No rows affected when updating namespace for host %s", cacheHost)
	}
}

// Test that registered services can grab a token from the Director
// using a valid advertise token. For now this only tests Caches because
// we aren't actively using fed tokens in the Origin yet.
// monitoringReadScope is the read scope on the Director's protected
// monitoring namespace, which restricted caches receive in addition to their
// allowed data prefixes.
var monitoringReadScope = "storage.read:" + server_utils.MonitoringAuthNs

func TestDirectorFedTokenCacheAPI(t *testing.T) {
	t.Cleanup(test_utils.SetupTestLogging(t))
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)

	testCases := []struct {
		name               string
		shouldSetSitename  bool
		allowedPrefixes    []string
		scopeShouldHave    []string
		scopeShouldNotHave []string
	}{
		// A cache restricted to a subset of the federation additionally gets the
		// protected monitoring namespace, so that restricting which data it may
		// serve does not also switch off its health testing. An unrestricted
		// cache needs no such entry: "storage.read:/" already covers it.
		{
			name:               "AllowFirstNamespace",
			shouldSetSitename:  false,
			allowedPrefixes:    []string{"/first/namespace"},
			scopeShouldHave:    []string{"storage.read:/first/namespace", monitoringReadScope},
			scopeShouldNotHave: []string{"/second/namespace"},
		},
		{
			name:               "WithSitename",
			shouldSetSitename:  true,
			allowedPrefixes:    []string{"/first/namespace"},
			scopeShouldHave:    []string{"storage.read:/first/namespace", monitoringReadScope},
			scopeShouldNotHave: []string{"/second/namespace"},
		},
		{
			name:              "AllowBothNamespaces",
			shouldSetSitename: false,
			allowedPrefixes:   []string{"/first/namespace", "/second/namespace"},
			scopeShouldHave: []string{
				"storage.read:/first/namespace", "storage.read:/second/namespace", monitoringReadScope,
			},
			scopeShouldNotHave: []string{},
		},
		{
			name:               "NoCustomField",
			shouldSetSitename:  false,
			allowedPrefixes:    []string{},
			scopeShouldHave:    []string{"storage.read:/"}, // Absence of field means no namespace restrictions
			scopeShouldNotHave: []string{},
		},
		{
			name:              "EmptyCustomField",
			shouldSetSitename: false,
			allowedPrefixes:   []string{""},
			// Empty field means no read permissions, and that includes the
			// monitoring namespace: a cache granted nothing stays granted
			// nothing, and one that may serve no data is not worth probing.
			scopeShouldHave:    []string{},
			scopeShouldNotHave: []string{},
		},
		{
			name:               "GlobNamespace",
			shouldSetSitename:  false,
			allowedPrefixes:    []string{"*"},
			scopeShouldHave:    []string{"storage.read:/"},
			scopeShouldNotHave: []string{},
		},
		// After some discussion with Sarthak, we decided there's no point in testing
		// the case where the Registry is configured with an invalid namespace -- we
		// make the assumption that namespace info is validated by the Registry before
		// insertion in its database.
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.shouldSetSitename {
				require.NoError(t, param.Xrootd_Sitename.Set("fed-test"))
			}

			require.NoError(t, param.Director_RegistryQueryInterval.SetString("1s"))
			_ = fed_test_utils.NewFedTest(t, bothPubNamespaces)

			// If the sitename is not set, this fetches the server's hostname.
			// Since all servers running in the fed test have the same hostname,
			// this lets us inject allowed prefixes in the registry database.
			registrationName := param.Xrootd_Sitename.GetString()
			require.NotEmpty(t, registrationName, "Failed to determine server's XRootD sitename")

			// Inject our "AllowedPrefixes" data into the registry database under
			// the /caches/<registration name> namespace
			dbLoc := param.Server_DbLocation.GetString()
			require.NotEmpty(t, dbLoc, "Failed to determine registry database location")
			updateAllowedPrefixesForCache(t, dbLoc, registrationName, tc.allowedPrefixes)

			// Now sleep for 2 seconds so the Director has time to populate the changes
			time.Sleep(2 * time.Second)

			// Grab the service's key and create an advertise token
			ctx := context.Background()
			ctx, _, _ = test_utils.TestContext(ctx, t)
			cache := cache.CacheServer{}
			tokStr, err := server_utils.CreateFedTok(ctx, &cache)
			require.NoError(t, err, "Failed to get cache's advertisement token")
			require.NotEmpty(t, tokStr, "Got an empty token")

			tok, err := token.UnsafeParseClaims(tokStr)
			require.NoError(t, err, "Failed to parse token")
			// The fed-test utility uses a separate HTTP server for hosting federation metadata,
			// and sets it as the Discovery endpoint -- tokens need to be issued by that endpoint
			fedInfo, err := config.GetFederation(ctx)
			require.NoError(t, err, "Failed to get federation info")
			discoveryUrlStr := fedInfo.DiscoveryEndpoint
			assert.Equal(t, discoveryUrlStr, tok.Issuer())
			var scopes []string
			if rawScopes, exists := tok.Get("scope"); exists {
				if scopeStr, ok := rawScopes.(string); ok {
					scopes = strings.Split(scopeStr, " ")
				}
			}
			assert.ElementsMatch(t, tc.scopeShouldHave, scopes)
		})
	}
}

// Test that the Director.EnableFederationMetadataHosting knob correctly
// toggles hosting of the federation discovery metadata at the Director.
func TestDirectorMetadataHosting(t *testing.T) {
	server_utils.ResetTestState()
	t.Cleanup(server_utils.ResetTestState)

	discoveryPath, err := url.JoinPath(".well-known", "pelican-configuration")
	require.NoError(t, err)

	newInsecureClient := func() *http.Client {
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}

	// Helper function that tries to fetch the federation discovery metadata
	// from a given URL, and checks for the expected status code.
	//
	// If the expected status code is http.StatusOK, it also returns the response body
	// containing the metadata JSON
	fetchDiscovery := func(
		t *testing.T,
		client *http.Client,
		baseURL string,
		expectedStatus int,
	) []byte {
		t.Helper()

		u, err := url.Parse(baseURL)
		require.NoError(t, err, "Failed to parse base URL")

		u.Path = discoveryPath

		t.Log("Fetching discovery URL:", u.String())

		req, err := http.NewRequest("GET", u.String(), nil)
		require.NoError(t, err, "Failed to create request")

		resp, err := client.Do(req)
		require.NoError(t, err, "Failed to perform request")
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, expectedStatus, resp.StatusCode)

		if expectedStatus != http.StatusOK {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Failed to read response body")
		return body
	}

	tests := []struct {
		name           string
		enableHosting  bool
		expectedStatus int
	}{
		{
			name:           "director-hosts-metadata",
			enableHosting:  true,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "director-does-not-host-metadata",
			enableHosting:  false,
			expectedStatus: http.StatusGone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, param.Director_EnableFederationMetadataHosting.Set(tt.enableHosting))
			_ = fed_test_utils.NewFedTest(t, bothPubNamespaces)

			ctx := context.Background()
			ctx, _, _ = test_utils.TestContext(ctx, t)

			fedInfo, err := config.GetFederation(ctx)
			require.NoError(t, err)

			client := newInsecureClient()

			// Always test the Director endpoint
			directorBody := fetchDiscovery(
				t,
				client,
				fedInfo.DirectorEndpoint,
				tt.expectedStatus,
			)

			// If hosting is enabled at the Director, also test the Discovery endpoint
			if tt.enableHosting {
				// Because the fed tests set up a separate Discovery server,
				// the Director and Discovery endpoints should differ
				require.NotEqual(
					t,
					fedInfo.DirectorEndpoint,
					fedInfo.DiscoveryEndpoint,
					"Director and Discovery endpoints must differ",
				)

				discoveryBody := fetchDiscovery(
					t,
					client,
					fedInfo.DiscoveryEndpoint,
					http.StatusOK,
				)

				// Contents must match exactly
				require.JSONEq(
					t,
					string(discoveryBody),
					string(directorBody),
					"Metadata from Director and Discovery endpoints must match",
				)

				// Optional: still unmarshal once for semantic validation
				var fedMetadata pelican_url.FederationDiscovery
				require.NoError(
					t,
					json.Unmarshal(directorBody, &fedMetadata),
				)

				require.Equal(
					t,
					fedInfo.DiscoveryEndpoint,
					fedMetadata.DiscoveryEndpoint,
				)
			} else {
				// Here we don't actually care about the content because there's
				// nothing to compare it against -- we only want to verify that
				// discovery resulted in an okay status code.
				_ = fetchDiscovery(
					t,
					client,
					fedInfo.DiscoveryEndpoint,
					http.StatusOK,
				)
			}
		})
	}
}
