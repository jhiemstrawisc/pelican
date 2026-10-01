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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

// Report the health status of test file transfer to storage server
func reportStatusToServer(ctx context.Context, serverWebUrl string, status string, message string, serverType string, fallback bool, report *server_structs.TestReport) error {
	fedInfo, err := config.GetFederation(ctx)
	if err != nil {
		return err
	}

	testTokenCfg := token.NewWLCGToken()
	testTokenCfg.Lifetime = time.Minute
	testTokenCfg.Issuer = fedInfo.DiscoveryEndpoint
	testTokenCfg.AddAudiences(serverWebUrl)
	testTokenCfg.Subject = "director"
	testTokenCfg.AddScopes(token_scopes.Pelican_DirectorTestReport)

	tok, err := testTokenCfg.CreateToken()
	if err != nil {
		return errors.Wrap(err, "failed to create director test report token")
	}

	reportUrl, err := url.Parse(serverWebUrl)
	if err != nil {
		return errors.Wrap(err, "the server URL is not parseable as an URL")
	}

	if status != "ok" && status != "error" {
		return errors.Errorf("bad status for reporting director test %s", status)
	}

	if serverType == server_structs.OriginType.String() {
		if fallback {
			reportUrl.Path = "/api/v1.0/origin-api/directorTest"
		} else {
			reportUrl.Path = "/api/v1.0/origin/directorTest"
		}
	} else if serverType == server_structs.CacheType.String() {
		reportUrl.Path = "/api/v1.0/cache/directorTest"
	}

	dt := server_structs.DirectorTestResult{
		Status:    status,
		Message:   message,
		Timestamp: time.Now().Unix(),
		Report:    report,
	}

	jsonData, err := json.Marshal(dt)
	if err != nil {
		return errors.Wrap(err, "failed to parse request body for reporting director test")
	}

	reqBody := bytes.NewBuffer(jsonData)

	log.Debugf("Director is sending %s server test result to %s", string(serverType), reportUrl.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reportUrl.String(), reqBody)
	if err != nil {
		return errors.Wrap(err, "failed to create POST request for reporting director test")
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pelican-director"+config.GetVersion())

	client := config.GetClient()
	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to start request for reporting director test")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read response body for reporting director test")
	}

	if resp.StatusCode != 200 {
		return errors.Errorf("error response %v from reporting director test to %s server at %s: %v", resp.StatusCode, string(serverType), reportUrl.String(), string(body))
	}

	return nil
}

// serverTypeOf converts an ad's type string back into the bitmask form used for
// coverage lookups.
func serverTypeOf(adType string) server_structs.ServerType {
	switch adType {
	case server_structs.OriginType.String():
		return server_structs.OriginType
	case server_structs.CacheType.String():
		return server_structs.CacheType
	default:
		return server_structs.ServerType(0)
	}
}

// originTestPlan builds the Director's probe of an origin: write an object into
// the monitoring namespace, read it back, and delete it.
//
// The binding it records is the monitoring namespace, not any namespace a client
// reads. On a POSIX origin that namespace resolves under the run location rather
// than the administrator's data volume, so this probe passing does not establish
// that the exported storage is reachable.
func originTestPlan(ad server_structs.ServerAd) server_utils.ProbePlan {
	originUrl := ad.URL.String()

	plan := server_utils.ProbePlan{
		Suite: server_structs.DirectorTestSuite,
		Binding: server_structs.BindingRef{
			Namespace:   server_utils.MonitoringBaseNs,
			StorageKind: string(ad.StorageType),
			Synthetic:   true,
		},
		BaseURL: originUrl,
		Prefix:  path.Join(server_utils.MonitoringBaseNs, server_utils.DirectorTestDir),
		Body:    server_utils.DirectorTestBody,
		// An empty issuer means the federation's discovery endpoint, with the
		// Director's own URL as a fallback for origins that predate it.
		Auth: server_utils.ProbeAuth{
			Audiences: []string{originUrl},
			RawScope:  "storage.read:/ storage.modify:/",
		},
		Expected: server_structs.ExpectedEdges(server_structs.OriginType),
		// The steps come from what the origin says it can do, not from what
		// backend it runs. An origin advertising no write capability is sent no
		// PUT: doing so guaranteed a failure and pinned the server critical
		// forever, which is what the AWS public-data origins in the OSDF have
		// been reporting.
		Steps: server_utils.OriginProbeSteps(ad.Caps),
	}

	if len(plan.Steps) == 0 {
		log.Debugf(
			"Origin %s advertises no write capability, so the director test has no probe to run "+
				"against %s; reporting it as not applicable rather than failing a write it never claimed to support",
			ad.Name, server_utils.MonitoringBaseNs)
	}

	return plan
}

// directorTestPlans returns every probe the Director runs against a server.
//
// A cache gets two, because the two differ in their authorization context and
// therefore in their binding: one public read-through, and one that forces the
// cache to obtain a federation token and present it upstream. They cannot be
// steps of a single plan -- a plan names one object and one token, and these
// need two of each.
func directorTestPlans(ad server_structs.ServerAd) ([]server_utils.ProbePlan, error) {
	switch ad.Type {
	case server_structs.OriginType.String():
		return []server_utils.ProbePlan{originTestPlan(ad)}, nil

	case server_structs.CacheType.String():
		publicPlan, err := cacheTestPlan(ad.URL)
		if err != nil {
			return nil, err
		}
		authPlan, err := cacheAuthTestPlan(ad.URL)
		if err != nil {
			return nil, err
		}
		return []server_utils.ProbePlan{publicPlan, authPlan}, nil

	default:
		return nil, errors.Errorf(
			"cannot health test server %s: unrecognized server type %q", ad.Name, ad.Type)
	}
}

// LaunchPeriodicDirectorTest runs periodic test file transfers against an origin or cache to ensure
// it's responding to director test requests. The test fetches the current server ad
// from the TTL cache on each cycle and stops when the ad is no longer present.
func LaunchPeriodicDirectorTest(ctx context.Context, serverUrlStr string) {
	// Option to disable touch on hit when fetching from cache to avoid extending TTL
	disableTouchOpt := ttlcache.WithDisableTouchOnHit[string, *server_structs.Advertisement]()

	// Fetch the initial server ad to set up metrics
	initialAdItem := serverAds.Get(serverUrlStr, disableTouchOpt)
	if initialAdItem == nil {
		log.Errorf("Failed to start director test suite: server ad not found in cache for URL %s. Test will not be started.", serverUrlStr)
		return
	}
	initialAd := initialAdItem.Value()
	serverAd := initialAd.ServerAd
	serverName := serverAd.Name
	serverUrl := serverAd.URL.String()
	serverWebUrl := serverAd.WebURL.String()

	log.Debug(fmt.Sprintf("Starting a new director test suite for %s server %s at %s", serverAd.Type, serverName, serverUrl))

	metrics.PelicanDirectorFileTransferTestSuite.With(
		prometheus.Labels{
			"server_name": serverName, "server_web_url": serverWebUrl, "server_type": string(serverAd.Type),
		}).Inc()

	metrics.PelicanDirectorActiveFileTransferTestSuite.With(
		prometheus.Labels{
			"server_name": serverName, "server_web_url": serverWebUrl, "server_type": string(serverAd.Type),
		}).Inc()

	customInterval := param.Director_OriginCacheHealthTestInterval.GetDuration()
	if customInterval < 15*time.Second {
		log.Warningf("You set Director.OriginCacheHealthTestInterval to a very small number %s, which will cause high traffic volume to xrootd servers.", customInterval.String())
	}
	if customInterval == 0 {
		customInterval = 15 * time.Second
		log.Error("Invalid config value: Director.OriginCacheHealthTestInterval is 0. Fallback to 15s.")
	}
	ticker := time.NewTicker(customInterval)

	defer ticker.Stop()

	// prevCacheTestFilePaths tracks the namespace-relative paths of the test objects created in
	// the previous successful cache test cycle. On the next successful cycle, each is evicted
	// from the cache via the xrdhttp-pelican evict API so that test files don't accumulate.
	//
	// This is a list because a cache probe runs more than one plan -- the public monitoring
	// namespace and the protected one -- and each read leaves an object resident.
	var prevCacheTestFilePaths []string

	// runDirectorTestCycle executes a single director test cycle and reports the result back to the server.
	// Extracted as a helper to allow running the first test immediately upon registration, avoiding the
	// race condition where the origin/cache 30s timeout fires before the first ticker-driven test.
	// Returns true if the test was run, false if it was skipped (e.g., server not in cache or in downtime).
	runDirectorTestCycle := func() bool {
		// Fetch the current server ad from the TTL cache
		adItem := serverAds.Get(serverUrlStr, disableTouchOpt)
		if adItem == nil {
			log.Infof("The Director doesn't have any advertisements for server with URL %s. Stopping director tests.", serverUrlStr)
			return false
		}
		currentServerAd := adItem.Value().ServerAd

		// Check if the server is in downtime by checking the filteredServers map
		if isServerInDowntime(currentServerAd.Name) {
			log.Debugf("Skipping director test cycle for %s server %s: server is in downtime", currentServerAd.Type, currentServerAd.Name)
			return true // Return true to continue the loop, but don't run the test
		}

		log.Debug(fmt.Sprintf("Starting a director test cycle for %s server %s at %s", currentServerAd.Type, currentServerAd.Name, currentServerAd.URL.String()))

		// Build the probes for whichever kind of server this is. An unrecognized
		// type yields no plan and is reported as such: the previous code left its
		// success flag initialized to true in that case and reported "ok" without
		// having run any test at all.
		plans, planErr := directorTestPlans(currentServerAd)
		var run server_utils.ProbeRun
		// Objects this cycle left resident on the server, to be evicted on
		// the next successful cycle.
		var leftBehind []string
		if planErr != nil {
			run.Report = server_structs.NewTestReport(
				server_structs.DirectorTestSuite,
				server_structs.ExpectedEdges(serverTypeOf(currentServerAd.Type)),
				[]server_structs.TestResult{{
					Outcome: server_structs.TestOutcomeFail,
					Reason:  server_structs.ReasonInternal,
					Detail:  planErr.Error(),
					At:      time.Now().Unix(),
				}})
		} else {
			// Fold every plan into one report, so the coverage ledger spans
			// everything this Director checked rather than whichever plan
			// happened to run last.
			var results []server_structs.TestResult
			for _, plan := range plans {
				planRun := plan.Run(ctx)
				results = append(results, planRun.Report.Results...)
				// Every plan that left an object behind contributes one to evict
				// next cycle. A cache probe runs two plans and each read leaves
				// an object resident, so tracking only the first let the other
				// tree grow without bound.
				if planRun.ObjectPath != "" {
					leftBehind = append(leftBehind, planRun.ObjectPath)
				}
			}
			run.Report = server_structs.NewTestReport(
				server_structs.DirectorTestSuite,
				server_structs.ExpectedEdges(serverTypeOf(currentServerAd.Type)),
				results)
		}

		reportStatus := "error"
		healthStatus := HealthStatusError
		if !run.Report.Outcome.IsFailure() {
			reportStatus = "ok"
			healthStatus = HealthStatusOK

			// Nothing ran, so the Director has no evidence either way. Reporting
			// "ok" on the wire keeps the server out of the red -- it has done
			// nothing wrong -- but the Director's own view must say "unknown"
			// rather than claim a pass it never observed.
			if run.Report.Outcome == server_structs.TestOutcomeSkip {
				healthStatus = HealthStatusUnknown
			}
			log.Debugf("Director file transfer test cycle succeeded at %s for %s server with URL at %s",
				time.Now().Format(time.RFC3339), currentServerAd.Type, currentServerAd.URL.String())

			// On a successful cache test, evict the test file from the previous cycle via the
			// cache's web API, which in turn calls the xrdhttp-pelican evict API locally.
			// This ensures test files don't accumulate on the cache, especially when
			// Server.DropPrivileges is true and the Pelican process cannot directly delete
			// xrootd-owned files.
			if currentServerAd.Type == server_structs.CacheType.String() {
				for _, stale := range prevCacheTestFilePaths {
					evictCacheTestFile(ctx, currentServerAd.WebURL.String(), stale)
				}
			}
			prevCacheTestFilePaths = leftBehind
		} else {
			log.Warningln("Director file transfer test cycle failed for ", currentServerAd.Type,
				" server: ", currentServerAd.URL.String())
		}

		// The message now names the step that broke, the reason, and the binding
		// it was probing, rather than only that "the test cycle failed".
		_, reportMessage := server_utils.SummarizeReport(run.Report)

		// Update healthTestUtils once per cycle.
		//
		// The full report is retained, not just the verdict. It is what
		// directorView reads so routing can act on this Director's own
		// observation, and what the admin API surfaces so a federation
		// administrator can see why a server is failing without having to ask the
		// server itself.
		func() {
			healthTestUtilsMutex.Lock()
			defer healthTestUtilsMutex.Unlock()
			if existingUtil, ok := healthTestUtils[currentServerAd.URL.String()]; ok {
				existingUtil.Status = healthStatus
				report := run.Report
				existingUtil.Probe = &report
				existingUtil.ProbeAt = time.Now()
			} else {
				log.Debugln("HealthTestUtil missing for ", currentServerAd.Type, " server: ", currentServerAd.URL.String(), " Failed to update internal status")
			}
		}()

		// Determine the metric status label based on test result
		testStatusMetric := metrics.MetricSucceeded
		if run.Report.Outcome.IsFailure() {
			testStatusMetric = metrics.MetricFailed
		}

		// Report the result of this Director-test back to origin/server (single call)
		probeReport := run.Report
		reportErr := reportStatusToServer(ctx, currentServerAd.WebURL.String(), reportStatus, reportMessage, currentServerAd.Type, false, &probeReport)

		// Determine report status metric and log if reporting failed
		reportStatusMetric := metrics.MetricSucceeded
		if reportErr != nil {
			reportStatusMetric = metrics.MetricFailed
			log.Warningf("Failed to report director test result to %s server at %s: %v", currentServerAd.Type, currentServerAd.WebURL.String(), reportErr)
		}

		// Record metrics once per cycle
		metrics.PelicanDirectorFileTransferTestsRuns.With(
			prometheus.Labels{
				"server_name":    currentServerAd.Name,
				"server_web_url": currentServerAd.WebURL.String(),
				"server_type":    string(currentServerAd.Type),
				"status":         string(testStatusMetric),
				"report_status":  string(reportStatusMetric),
			},
		).Inc()

		return true // Test was run successfully
	}

	// Run the first test immediately to avoid race with origin/cache 30s timeout.
	// Without this, time.NewTicker waits for the first interval before firing,
	// which could cause the origin/cache to report a missed test if registration
	// takes more than 15 seconds after the server started.
	runDirectorTestCycle()

	for {
		select {
		case <-ctx.Done():
			log.Debug(fmt.Sprintf("Stopped the Director test suite for %s server %s at %s", serverAd.Type, serverName, serverUrl))

			metrics.PelicanDirectorActiveFileTransferTestSuite.With(
				prometheus.Labels{
					"server_name": serverName, "server_web_url": serverWebUrl, "server_type": string(serverAd.Type),
				}).Dec()

			return
		case <-ticker.C:
			runDirectorTestCycle()
		}
	}
}
