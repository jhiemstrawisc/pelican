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

package director

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

type requestInfo struct {
	Host  string
	SType server_structs.ServerType
	Tok   string
}

func validateFedTokRequest(ginCtx *gin.Context) (rInfo requestInfo, err error) {
	// Parse the incoming request parameters, from which we will extract the token
	// and the hostname of the cache.
	// NOTE -- this function will also grab tokens from an Authorization header and pass
	// them back as a request parameter.
	reqParams := getRequestParameters(ginCtx.Request)
	hNames, exists := reqParams["host"]
	if !exists || len(hNames) == 0 {
		err = fmt.Errorf("no hostname found in the 'host' url parameter: %s", ginCtx.Request.URL.String())
		return
	} else if len(hNames) > 1 {
		err = fmt.Errorf("multiple hostnames found in the 'host' url parameter: %s", ginCtx.Request.URL.String())
		return
	}
	rInfo.Host = hNames[0]

	sTypes, exists := reqParams["sType"]
	var sType server_structs.ServerType
	if !exists || len(sTypes) == 0 {
		err = fmt.Errorf("host '%s' generated request with no server type found in the 'sType' url parameter: %s", rInfo.Host, ginCtx.Request.URL.String())
		return
	} else if len(sTypes) > 1 {
		err = fmt.Errorf("host '%s' generated request with multiple server types in the 'sType' url parameter: %s", rInfo.Host, ginCtx.Request.URL.String())
		return
	}
	valid := sType.SetString(sTypes[0])
	if !valid || (sType != server_structs.CacheType && sType != server_structs.OriginType) {
		err = fmt.Errorf("host '%s' generated request with invalid server type '%s' as value of 'sType' url parameter: %s", rInfo.Host, sTypes[0], ginCtx.Request.URL.String())
		return
	}
	rInfo.SType = sType

	// Note that our getRequestParameters function will also check the Authorization header, but multiple tokens are stripped
	// such that we only look at the first one.
	tok, exists := reqParams["authz"]
	if !exists || len(tok) == 0 {
		err = fmt.Errorf("host '%s' generated request with no authorization token in 'Authorization' header or 'authz' url parameter", rInfo.Host)
		return
	}
	rInfo.Tok = tok[0]

	return
}

func createFedTok(ginCtx *gin.Context, rInfo requestInfo) (tok string, err error) {
	// The federation token will be signed by the Director on behalf of the federation, so
	// we still use the Discovery endpoint as the issuer.
	fed, err := config.GetFederation(ginCtx)
	if err != nil {
		err = errors.Wrap(err, "federation issuer could not be determined")
		return
	}
	if fed.DiscoveryEndpoint == "" {
		err = errors.New("federation issuer is not set")
		return
	}
	fToken := token.NewWLCGToken()
	fToken.Lifetime = param.Director_FedTokenLifetime.GetDuration()
	fToken.Subject = rInfo.Host
	fToken.Issuer = fed.DiscoveryEndpoint
	// This token is meant to be consumed by any origin in the system. However, without
	// knowing every origin in the system ahead of time, we can't add them all so we
	// use the more permissive "ANY" audience.
	fToken.AddAudienceAny()

	// The token should be scoped such that the cache only has permission for the namespaces
	// indicated by the Director
	allowedPrefixesPtr := allowedPrefixesForCaches.Load()
	if allowedPrefixesPtr == nil {
		err = errors.New("the Director could not determine allowed prefixes for the provided host")
		return
	}
	allowedPrefixes := *allowedPrefixesPtr

	hostPrefixes, exists := allowedPrefixes[rInfo.Host]
	if !exists {
		// If the entry does not exist, we assume there are no prefix restrictions
		hostPrefixes = map[string]struct{}{"/": {}}
	}

	scopes := make([]token_scopes.TokenScope, 0, len(allowedPrefixes[rInfo.Host])+1)
	for prefix := range hostPrefixes {
		// Provide at least _some_ basic validation before we use this value to mint read tokens...
		if !strings.HasPrefix(prefix, "/") {
			log.Warningf("Invalid prefix from registry's 'AllowedPrefixes' custom field found while creating "+
				"federation token for host '%s': '%s'", rInfo.Host, prefix)
			continue
		}
		var readScope token_scopes.TokenScope
		readScope, err = token_scopes.Wlcg_Storage_Read.Path(prefix)
		if err != nil {
			err = errors.Wrap(err, "token scopes could not be created")
			return
		}
		scopes = append(scopes, readScope)
	}

	// The protected monitoring namespace is always readable, whatever the
	// registry says about this host's prefixes.
	//
	// Those prefixes express which of the federation's *data* a cache may serve.
	// The monitoring namespace is not data: it holds nothing but probe objects
	// the Director itself generates, and reading one is how the Director
	// establishes that this cache can serve a namespace requiring authorization.
	// Leaving it to AllowedPrefixes would mean that restricting a cache to a
	// subset of the federation silently disables part of its health testing, and
	// the probe would fail for a reason that has nothing to do with the cache.
	//
	// Two cases are deliberately left alone.
	//
	// An existing prefix that already covers it, which is the common case: an
	// unrestricted cache holds "storage.read:/", and appending a narrower scope
	// beneath it would be noise on every token.
	//
	// And a cache with no readable prefix at all. An administrator who granted
	// a cache nothing meant nothing, and a cache that may serve no data is not
	// worth health testing anyway. Health testing follows the ability to serve;
	// it does not create it.
	wantMonitoring := token_scopes.NewResourceScope(token_scopes.Wlcg_Storage_Read, server_utils.MonitoringAuthNs)
	held := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		held = append(held, sc.String())
	}
	alreadyCovered := slices.ContainsFunc(token_scopes.ParseResourceScopes(held),
		func(rs token_scopes.ResourceScope) bool { return rs.Contains(wantMonitoring) })

	if len(scopes) > 0 && !alreadyCovered {
		var monitoringScope token_scopes.TokenScope
		monitoringScope, err = token_scopes.Wlcg_Storage_Read.Path(server_utils.MonitoringAuthNs)
		if err != nil {
			err = errors.Wrap(err, "monitoring token scope could not be created")
			return
		}
		scopes = append(scopes, monitoringScope)
	}

	fToken.AddScopes(scopes...)

	tok, err = fToken.CreateToken()
	if err != nil {
		err = errors.Wrap(err, "could not create/sign token")
		return
	}

	return
}

func getFedToken(ginCtx *gin.Context) {
	rInfo, err := validateFedTokRequest(ginCtx)
	if err != nil {
		log.Debugf("Error validating incoming request: %s", err)
		ginCtx.JSON(http.StatusForbidden, server_structs.SimpleApiResp{
			Status: server_structs.RespFailed,
			Msg:    err.Error(),
		})
		return
	}

	// Validate the token by talking to the Registry. Note that server type has already been validated
	// and is either Cache or Origin.
	var registryPrefix string
	if rInfo.SType == server_structs.CacheType {
		registryPrefix = fmt.Sprintf("%s%s", server_structs.CachePrefix, rInfo.Host)
	} else if rInfo.SType == server_structs.OriginType {
		registryPrefix = fmt.Sprintf("%s%s", server_structs.OriginPrefix, rInfo.Host)
	}
	// Any token that grants authorization to advertise within a federation should be enough
	// to determine that the server is part of the federation.
	if ok, err := verifyAdvertiseToken(ginCtx, rInfo.Tok, registryPrefix); err != nil {
		if errors.Is(err, adminApprovalErr) {
			log.Debugf("Host '%s' has not been approved by an administrator", rInfo.Host)
			ginCtx.JSON(http.StatusForbidden, server_structs.SimpleApiResp{
				Status: server_structs.RespFailed,
				Msg:    fmt.Sprintf("Host '%s' has not been approved by an administrator", rInfo.Host),
			})
			return
		}

		// An otherwise unexpected error occurred
		log.Warningf("Failed to verify advertise token from host '%s': %v", rInfo.Host, err)
		ginCtx.JSON(http.StatusInternalServerError, server_structs.SimpleApiResp{
			Status: server_structs.RespFailed,
			Msg:    "Failed to verify advertise token",
		})
		return
	} else if !ok {
		// We read the token, but we don't like it
		log.Debugf("Advertise token from host '%s' was rejected", rInfo.Host)
		ginCtx.JSON(http.StatusForbidden, server_structs.SimpleApiResp{
			Status: server_structs.RespFailed,
			Msg:    "The provided advertise token was rejected",
		})
		return
	}

	// We've validated the incoming token and decided to issue the federation token
	tok, err := createFedTok(ginCtx, rInfo)
	if err != nil {
		log.Warningf("Failed to create federation token for host '%s': %v", rInfo.Host, err)
		ginCtx.JSON(http.StatusInternalServerError, server_structs.SimpleApiResp{
			Status: server_structs.RespFailed,
			Msg:    "Attempted to create federation token but failed unexpectedly",
		})
		return
	}

	// Respond with the token
	ginCtx.JSON(http.StatusOK, server_structs.TokenResponse{
		AccessToken: tok,
	})
}
