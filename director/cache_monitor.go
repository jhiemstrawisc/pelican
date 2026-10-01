/***************************************************************
 *
 * Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// cacheTestPlan builds the Director's probe of a cache.
//
// The probe asks for an object the cache cannot already hold -- the name carries
// a fresh timestamp -- so serving it forces the cache onto its miss path:
// resolve upstream through the Director, fetch, and commit to disk. The
// per-director subdirectory keeps several Directors sharing one cache from
// colliding, and makes stale objects easy to sweep by day.
//
// What this probe does *not* reach is as important as what it does. It only
// ever takes the miss branch, so it says nothing about serving an object that
// is already resident; and the upstream it drives the cache to is the
// Director's own synthetic origin, served public-read, so no federation token
// is minted and no real origin is contacted. Those edges are listed as expected
// and land in the report's Uncovered set.
func cacheTestPlan(cacheUrl url.URL) (server_utils.ProbePlan, error) {
	directorID := param.Server_Hostname.GetString()
	if directorID == "" {
		return server_utils.ProbePlan{}, errors.Errorf(
			"%s is empty; cannot construct director test file path", param.Server_Hostname.GetName())
	}

	now := time.Now()
	objectPath := path.Join(
		server_utils.DirectorTestPrefix(directorID, now),
		server_structs.DirectorTestSuite.String()+"-"+now.Format(time.RFC3339)+".txt")

	return server_utils.ProbePlan{
		Suite: server_structs.DirectorTestSuite,
		Binding: server_structs.BindingRef{
			Namespace:   server_utils.MonitoringBaseNs,
			StorageKind: "cache",
			Synthetic:   true,
		},
		BaseURL:    cacheUrl.String(),
		ObjectPath: objectPath,
		Body:       server_utils.DirectorTestBody,
		// The monitoring namespace is public, so a client reading it presents no
		// token. Presenting one here would test something the client path does
		// not use.
		Auth:     server_utils.ProbeAuth{Anonymous: true},
		Expected: server_structs.ExpectedEdges(server_structs.CacheType),
		Steps: []server_utils.ProbeStep{{
			Op: server_utils.ProbeOpRead,
			Edges: []server_structs.Edge{
				server_structs.EdgeFront,
				server_structs.EdgeDirectorResolve,
				server_structs.EdgeUpstreamFetch,
				server_structs.EdgeCacheWrite,
			},
		}},
	}, nil
}

// evictCacheTestFile asks the cache to evict a previously-created director test file.
// Rather than calling the xrdhttp-pelican evict API directly (which would require the
// cache's scitokens config to trust the federation issuer), this sends the eviction
// request to the cache's Pelican web API. The cache then calls its own xrdhttp-pelican
// evict API using a locally-minted token — matching the self-test authorization pattern.
//
// This avoids the privilege problem where the Pelican process cannot delete xrootd-owned
// files when Server.DropPrivileges is true: XRootD removes the file as its own user.
//
// Eviction failures are logged but not returned as errors, since a missed eviction is not
// critical — the cache-side LaunchDirectorTestFileCleanup serves as a backup.
func evictCacheTestFile(ctx context.Context, cacheWebUrl string, testFilePath string) {
	fedInfo, err := config.GetFederation(ctx)
	if err != nil {
		log.Warningf("Failed to get federation info for evict token: %v", err)
		return
	}

	testTokenCfg := token.NewWLCGToken()
	testTokenCfg.Lifetime = time.Minute
	testTokenCfg.Issuer = fedInfo.DiscoveryEndpoint
	testTokenCfg.AddAudiences(cacheWebUrl)
	testTokenCfg.Subject = "director"
	testTokenCfg.AddScopes(token_scopes.Pelican_DirectorTestReport)

	tok, err := testTokenCfg.CreateToken()
	if err != nil {
		log.Warningf("Failed to create evict token for cache %s: %v", cacheWebUrl, err)
		return
	}

	evictUrl, err := url.Parse(cacheWebUrl)
	if err != nil {
		log.Warningf("Failed to parse cache web URL %s: %v", cacheWebUrl, err)
		return
	}
	evictUrl.Path = "/api/v1.0/cache/evictTestFile"

	reqBody, _ := json.Marshal(map[string]string{"path": testFilePath})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, evictUrl.String(), bytes.NewReader(reqBody))
	if err != nil {
		log.Warningf("Failed to create evict request for cache %s: %v", cacheWebUrl, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	client := config.GetClient()
	resp, err := client.Do(req)
	if err != nil {
		log.Warningf("Failed to send evict request to cache %s: %v", cacheWebUrl, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Debugf("Successfully evicted test file %s from cache %s", testFilePath, cacheWebUrl)
	} else {
		body, _ := io.ReadAll(resp.Body)
		log.Warningf("Evict request to cache %s returned status %d: %s", cacheWebUrl, resp.StatusCode, string(body))
	}
}
