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

// The Director standing in as an origin, for the probes that need one.
//
// A cache cannot be probed the way an origin can. The interesting thing a cache
// does is fetch what it does not have, and that needs something upstream to
// fetch from. Rather than conscript a real origin -- which makes a failure
// ambiguous between two servers and requires every origin to cooperate -- the
// Director serves the probe object itself, out of two synthetic namespaces:
//
//	/pelican/monitoring            public.    Covers resolve, fetch, cache write.
//	/pelican/monitoring/authTest   protected. Additionally covers the cache
//	                               obtaining a federation token and presenting it
//	                               to an upstream that demands one.
//
// The second exists because the first cannot be made to cover those edges. A
// cache decides whether to demand a token from its own client by consulting the
// capabilities of the namespace the Director advertised, and there is one answer
// per namespace -- so a protected object inside a public namespace is not a
// thing that can be expressed. Two namespaces is what the model allows.
//
// The pairing is load-bearing and easy to break: this file must advertise a
// namespace as protected *and* refuse to serve its objects without a token. If
// those two ever disagree, the probe passes while proving nothing, which is the
// exact failure this whole subsystem exists to prevent. They are kept in one
// file for that reason.
package director

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// publicMonitoringNamespaceAd is the namespace the anonymous cache probe uses.
func publicMonitoringNamespaceAd() server_structs.NamespaceAd {
	return server_structs.NamespaceAd{
		Path: server_utils.MonitoringBaseNs,
		Caps: server_structs.Capabilities{
			PublicReads: true,
			Reads:       true,
		},
	}
}

// authMonitoringNamespaceAd is the protected namespace.
//
// The issuer is the federation, and the base path is this namespace. Both
// matter to the cache: GenerateCacheIssuers in xrootd/authorization.go collects
// issuers only from namespaces that are *not* public reads, and writes them into
// the cache's scitokens.cfg keyed by issuer with these base paths. Without the
// issuer entry the cache has no configured trust for the namespace and refuses
// the probe's token; without the namespace being non-public it never consults
// the issuer at all.
func authMonitoringNamespaceAd(ctx *gin.Context) (server_structs.NamespaceAd, error) {
	fedInfo, err := config.GetFederation(ctx)
	if err != nil {
		return server_structs.NamespaceAd{}, errors.Wrap(err, "federation issuer could not be determined")
	}
	if fedInfo.DiscoveryEndpoint == "" {
		return server_structs.NamespaceAd{}, errors.New("federation issuer is not set")
	}
	issuerUrl, err := url.Parse(fedInfo.DiscoveryEndpoint)
	if err != nil {
		return server_structs.NamespaceAd{}, errors.Wrap(err, "federation issuer is not parseable as a URL")
	}

	return server_structs.NamespaceAd{
		Path: server_utils.MonitoringAuthNs,
		Caps: server_structs.Capabilities{
			// Not public. This single field is what makes the cache ask for a
			// token on the way in and present one on the way out.
			PublicReads: false,
			Reads:       true,
		},
		Issuer: []server_structs.TokenIssuer{{
			BasePaths: []string{server_utils.MonitoringAuthNs},
			IssuerUrl: *issuerUrl,
		}},
	}, nil
}

// monitoringNamespaceAd returns the namespace ad matching a monitoring path.
func monitoringNamespaceAd(ctx *gin.Context, reqPath string) (server_structs.NamespaceAd, error) {
	if server_utils.IsAuthMonitoringPath(reqPath) {
		return authMonitoringNamespaceAd(ctx)
	}
	return publicMonitoringNamespaceAd(), nil
}

// monitoringNamespaceAds returns both synthetic namespaces for the namespace
// listing a cache fetches at startup.
//
// The protected one is dropped rather than fatal when the federation issuer
// cannot be determined: a Director that cannot name its own issuer should still
// serve the public namespace it has always served, instead of failing the whole
// listing and taking every cache's configuration with it.
func monitoringNamespaceAds(ctx *gin.Context) []server_structs.NamespaceAd {
	ads := []server_structs.NamespaceAd{publicMonitoringNamespaceAd()}
	if authAd, err := authMonitoringNamespaceAd(ctx); err == nil {
		ads = append(ads, authAd)
	} else {
		log.Warningf("Not advertising the protected monitoring namespace, so caches will not be "+
			"probed for federation-token handling: %v", err)
	}
	return ads
}

// authorizeMonitoringRead checks the bearer token on a request for a protected
// probe object.
//
// Two different callers present a token here, and one check covers both because
// both tokens are federation-issued:
//
//   - the cache, fetching the object upstream with the federation token it holds
//     from getFedToken. This is the edge being probed.
//   - the Director's own probe, reading through the cache, whose token the
//     Director minted as the federation issuer.
//
// Scope checking is done here rather than through AuthOption.Scopes because the
// shared validator compares scope strings for equality. A federation token
// carrying `storage.read:/` would be rejected against an expected scope naming
// the full object path, even though it plainly authorizes it. ResourceScope
// understands the hierarchy; token_scopes.ScopeContains does not.
func authorizeMonitoringRead(ctx *gin.Context, objectPath string) (int, error) {
	// Three sources, because three different callers reach this handler and they
	// do not agree on where a token goes. XRootD's client plugin sends "authz",
	// the persistent cache and the Pelican client send "access_token", and a
	// direct caller uses the Authorization header.
	result, status, ok, err := token.VerifyAndExtract(ctx, token.AuthOption{
		Sources: []token.TokenSource{token.Header, token.Authz, token.AccessToken},
		Issuers: []token.TokenIssuer{token.FederationIssuer},
	})
	if !ok {
		if err == nil {
			err = errors.New("no acceptable federation token was presented")
		}
		if status == 0 || status == http.StatusOK {
			status = http.StatusForbidden
		}
		return status, errors.Wrapf(err,
			"a token issued by the federation is required to read %s", objectPath)
	}
	if result == nil || result.Token == nil {
		return http.StatusInternalServerError, errors.New("token verification returned no token")
	}

	rawScope, present := result.Token.Get("scope")
	if !present {
		return http.StatusForbidden, errors.New("the presented token carries no scope, so it authorizes nothing")
	}
	scopeStr, _ := rawScope.(string)

	// A presented scope is read two ways, because two things legitimately produce
	// one and they disagree about what the path means.
	//
	//   absolute  a federation token from getFedToken carries "storage.read:/",
	//             or a prefix of the federation namespace. There is no base path
	//             in play; the path means itself.
	//
	//   relative  a token minted for a client of this namespace is scoped
	//             relative to the base path the Director itself advertised for it
	//             (see authMonitoringNamespaceAd), because that is how the cache's
	//             scitokens layer resolves it. When the cache then forwards that
	//             same token upstream, the Director has to apply the base path it
	//             declared or it rejects a token it just told the cache to accept.
	//
	// Accepting both is safe here specifically because of where this runs.
	// validateHealthTestRequest has already confined objectPath to the monitoring
	// namespace and to a well-formed probe filename, and this handler serves
	// nothing else: the only thing a widened reading can reach is a synthetic
	// object the Director generated. It must not be copied to a handler that
	// serves real data.
	want := token_scopes.NewResourceScope(token_scopes.Wlcg_Storage_Read, objectPath)
	relativePath := strings.TrimPrefix(objectPath, server_utils.MonitoringAuthNs)
	wantRelative := token_scopes.NewResourceScope(token_scopes.Wlcg_Storage_Read, relativePath)

	for _, held := range token_scopes.ParseResourceScopes(strings.Fields(scopeStr)) {
		if held.Contains(want) || held.Contains(wantRelative) {
			return http.StatusOK, nil
		}
	}

	return http.StatusForbidden, errors.Errorf(
		"the presented token is not scoped to read %s", objectPath)
}
