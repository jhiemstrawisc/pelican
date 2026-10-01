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

package xrootd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/pelicanplatform/pelican/cache"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/token_scopes"
)

const (
	selfTestBody   string = server_utils.SelfTestBody
	selfTestPrefix string = "self-test-"
)

// selfTestDir is the namespace-relative directory both self-tests probe.
var selfTestDir = server_utils.SelfTestPrefix()

// Add self-test directories to xrootd data location of the cache
func InitSelfTestDir() error {
	uid, err := config.GetDaemonUID()
	if err != nil {
		return err
	}

	gid, err := config.GetDaemonGID()
	if err != nil {
		return err
	}

	basePath := param.Cache_NamespaceLocation.GetString()
	selfTestPath := filepath.Join(basePath, selfTestDir)
	err = config.MkdirAll(selfTestPath, 0750, uid, gid)
	if err != nil {
		return errors.Wrap(err, "failed to create directory for the self-test")
	}
	log.Debugf("Created cache self-test directory at %s", selfTestPath)

	return nil
}

func generateTestFile() (string, error) {
	basePath := param.Cache_NamespaceLocation.GetString()
	if basePath == "" {
		return "", errors.New("failed to generate self-test file for cache: Cache.NamespaceLocation is not set.")
	}
	selfTestPath := filepath.Join(basePath, selfTestDir)
	_, err := os.Stat(selfTestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			uid, err := config.GetDaemonUID()
			if err != nil {
				return "", err
			}

			gid, err := config.GetDaemonGID()
			if err != nil {
				return "", err
			}

			if err := config.MkdirAll(selfTestPath, 0750, uid, gid); err != nil {
				return "", errors.Wrap(err, "failed to create self-test directory")
			}
		} else {
			return "", errors.Wrap(err, "failed to stat self-test directory")
		}
	}
	uid, err := config.GetDaemonUID()
	if err != nil {
		return "", err
	}

	gid, err := config.GetDaemonGID()
	if err != nil {
		return "", err
	}

	now := time.Now()
	testFileBytes := []byte(selfTestBody)
	fileSize := len(testFileBytes)
	cinfo := cache.Cinfo{
		Store: cache.Store{
			FileSize:     int64(fileSize),
			CreationTime: now.Unix(),
			Status:       2, // CSChk_None = 0
		},
	}
	cinfoBytes, err := cinfo.Serialize()
	if err != nil {
		return "", err
	}

	testFileName := selfTestPrefix + now.Format(time.RFC3339) + ".txt"
	testFileCinfoName := selfTestPrefix + now.Format(time.RFC3339) + ".txt.cinfo"

	finalFilePath := filepath.Join(selfTestPath, testFileName)

	tmpFileCinfoPath := filepath.Join(selfTestPath, testFileCinfoName+".tmp")
	finalFileCinfoPath := filepath.Join(selfTestPath, testFileCinfoName)

	// This is for web URL path, do not use filepath
	extFilePath := path.Join(selfTestDir, testFileName)

	file, err := os.OpenFile(finalFilePath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create self-test file %s", finalFilePath)
	}
	defer file.Close()
	defer log.Debug("Cache self-test file created at: ", finalFilePath)

	cinfoFile, err := os.OpenFile(tmpFileCinfoPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create self-test cinfo file %s", tmpFileCinfoPath)
	}
	defer cinfoFile.Close()

	if _, err := file.Write(testFileBytes); err != nil {
		return "", errors.Wrapf(err, "failed to write test content to self-test file %s", finalFilePath)
	}
	if _, err := cinfoFile.Write(cinfoBytes); err != nil {
		return "", errors.Wrapf(err, "failed to write cinfo content to self-test cinfo file %s", tmpFileCinfoPath)
	}

	if err = file.Chown(uid, gid); err != nil {
		return "", errors.Wrapf(err, "unable to change ownership of self-test file %v to desired daemon gid %v", file, gid)
	}
	if err = cinfoFile.Chown(uid, gid); err != nil {
		return "", errors.Wrapf(err, "unable to change ownership of self-test cinfo file %v to desired daemon gid %v", file, gid)
	}

	if err := os.Rename(tmpFileCinfoPath, finalFileCinfoPath); err != nil {
		return "", errors.Wrapf(err, "unable to move self-test cinfo file from temp location %q to desired location %q", tmpFileCinfoPath, finalFileCinfoPath)
	}

	cachePort := param.Cache_Port.GetInt()
	baseUrlStr := fmt.Sprintf("https://%s:%d", param.Server_Hostname.GetString(), cachePort)
	baseUrl, err := url.Parse(baseUrlStr)
	if err != nil {
		return "", errors.Wrap(err, "failed to validate the base url for self-test download")
	}
	baseUrl.Path = extFilePath

	if baseUrl.String() == "" {
		return "", errors.New("generated self-test file URL is empty")
	}

	return baseUrl.String(), nil
}

// generateTestFileViaPlugin creates a test file and its .cinfo file in a temp location (birthplace),
// then copies them to the selfTestDir using the xrdhttp-pelican plugin. This function is used
// when drop privileges is enabled, as the pelican server is running as an unprivileged user
// and cannot directly create files in the selfTestDir.
//
// Note: The self-test directory should already exist, created by InitSelfTestDir() which runs
// before privileges are dropped. We don't attempt to create it here because after privilege
// drop, the pelican user doesn't have permission to create directories in xrootd's runtime location.
func generateTestFileViaPlugin() (string, error) {
	user, err := config.GetPelicanUser()
	if err != nil {
		return "", errors.Wrap(err, "failed to get user")
	}

	// Create a temp directory own by pelican user to bypass privilege restrictions, named "birthplace"
	selfTestBirthplace := filepath.Join(param.Monitoring_DataLocation.GetString(), "selfTest")
	err = config.MkdirAll(selfTestBirthplace, 0750, user.Uid, user.Gid)
	if err != nil {
		return "", errors.Wrap(err, "failed to create self-test directory")
	}

	// Create a test file and its cinfo in the birthplace
	testFileBytes := []byte(selfTestBody)
	fileSize := len(testFileBytes)
	cinfo := cache.Cinfo{
		Store: cache.Store{
			FileSize:     int64(fileSize),
			CreationTime: time.Now().Unix(),
			Status:       2,
		},
	}
	cinfoBytes, err := cinfo.Serialize()
	if err != nil {
		return "", err
	}

	file, err := os.CreateTemp(selfTestBirthplace, selfTestPrefix+"*.txt")
	if err != nil {
		return "", errors.Wrap(err, "failed to create test file")
	}
	cinfoFile, err := os.Create(file.Name() + ".cinfo")
	if err != nil {
		return "", errors.Wrap(err, "failed to create test file cinfo")
	}
	defer func() {
		file.Close()
		cinfoFile.Close()
		// Delete the test file and its cinfo in the birthplace
		if err := os.Remove(file.Name()); err != nil {
			log.Warningf("Failed to remove test file %s: %v", file.Name(), err)
		}
		if err := os.Remove(cinfoFile.Name()); err != nil {
			log.Warningf("Failed to remove test file cinfo %s: %v", cinfoFile.Name(), err)
		}
	}()

	// Write test data and cinfo to the files
	if _, err := file.Write(testFileBytes); err != nil {
		return "", errors.Wrapf(err, "failed to write test content to self-test file %s", file.Name())
	}
	if _, err := cinfoFile.Write(cinfoBytes); err != nil {
		return "", errors.Wrapf(err, "failed to write cinfo content to self-test cinfo %s", cinfoFile.Name())
	}

	// After writing the test content to the file, the file pointer remains at the end.
	// Seek back to the beginning of the file so that the copy operation reads from the start.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", errors.Wrap(err, "failed to seek to beginning of test file")
	}
	if _, err := cinfoFile.Seek(0, io.SeekStart); err != nil {
		return "", errors.Wrap(err, "failed to seek to beginning of cinfo file")
	}

	// Transplant the test file to selfTestDir using the xrdhttp-pelican plugin.
	// Command "4" instructs the plugin to put the test file into the designated location, which is specified in `xrootd/launch.go`.
	// Check `src/XrdHttpPelican.cc` in https://github.com/PelicanPlatform/xrdhttp-pelican for the counterpart in the plugin.
	if err = FileCopyToXrootdDir(false, 4, file); err != nil {
		return "", errors.Wrap(err, "failed to copy the test file to the self-test directory")
	}
	// Transplant the cinfo file to selfTestDir using the xrdhttp-pelican plugin.
	// Command "5" instructs the plugin to put the cinfo file into the designated location, which is specified in `xrootd/launch.go`.
	if err = FileCopyToXrootdDir(false, 5, cinfoFile); err != nil {
		return "", errors.Wrap(err, "failed to copy the test cinfo file to the self-test directory")
	}

	// Construct and return the URL of the copied test file in the selfTestDir
	baseUrlStr := fmt.Sprintf("https://%s:%d", param.Server_Hostname.GetString(), param.Cache_Port.GetInt())
	baseUrl, err := url.Parse(baseUrlStr)
	if err != nil {
		return "", errors.Wrap(err, "failed to validate the base url for self-test download")
	}
	// This is for web URL path, do not use filepath pkg.
	// The file name of the test file is always the same in the selfTestDir,
	// no matter what's the file name in the birthplace.
	extFilePath := path.Join(selfTestDir, selfTestPrefix+"cache-server.txt")
	baseUrl.Path = extFilePath

	return baseUrl.String(), nil
}

// Token minting and the download itself now come from the shared probe engine
// in server_utils. This file previously carried its own generateFileTestScitoken
// and downloadTestFile, identically named to -- but different in signature and
// behavior from -- the ones in server_utils/test_file_transfer.go. See
// https://github.com/PelicanPlatform/pelican/issues/3107.

func deleteTestFile(fileUrlStr string) error {
	// If drop privileges is enabled, the test file at selfTestDir will be overwritten by the next self-test,
	// because the test file and cinfo at selfTestDir always have the same name.
	// Also, the test file and cinfo in their birthplace were deleted right after generateTestFileViaPlugin,
	// so this function can be skipped.
	if param.Server_DropPrivileges.GetBool() {
		return nil
	}

	basePath := param.Cache_NamespaceLocation.GetString()
	fileUrl, err := url.Parse(fileUrlStr)
	if err != nil {
		return errors.Wrap(err, "unable to delete self-test file due to invalid URL")
	}
	// Clean first so the prefix check sees the same path that path.Join
	// below will resolve; only paths strictly inside selfTestDir qualify.
	relativePath := path.Clean(fileUrl.Path)
	if !strings.HasPrefix(relativePath, selfTestDir+"/") {
		return fmt.Errorf("unable to delete file '%s' because it's not a valid self-test path", relativePath)
	}
	filePath := path.Join(basePath, relativePath)
	// remove .cinfo file first
	if err := os.Remove(filePath + ".cinfo"); err != nil {
		return errors.Wrap(err, "failed to delete the self-test file's cinfo file")
	}
	// remove the self-test file
	if err := os.Remove(filePath); err != nil {
		return errors.Wrap(err, "fail to remove the self-test file")
	}

	return nil
}

// cacheSeeder places the cache's probe object and its cinfo directly on disk.
//
// A cache has no upload path, so its self-test cannot write the object it intends
// to read; it fabricates the cinfo that XRootD's pfc layer would normally have
// produced. That is worth stating plainly, because it bounds what a passing cache
// self-test proves: that XRootD can serve a file someone placed on disk. It says
// nothing about resolving an object through the Director, obtaining a federation
// token, or fetching from an upstream origin -- which is why the report it
// produces lists those edges as uncovered.
type cacheSeeder struct{}

func (cacheSeeder) Seed(ctx context.Context) (string, error) {
	if param.Server_DropPrivileges.GetBool() {
		return generateTestFileViaPlugin()
	}
	return generateTestFile()
}

func (cacheSeeder) Cleanup(ctx context.Context, objectURL string) error {
	return deleteTestFile(objectURL)
}

// cacheSelfTestPlan probes the one path a cache self-test can reach: a read of an
// object already resident on local disk.
func cacheSelfTestPlan() server_utils.ProbePlan {
	return server_utils.ProbePlan{
		Suite: server_structs.SelfTestSuite,
		Binding: server_structs.BindingRef{
			Namespace:   selfTestDir,
			StorageKind: "cache",
			Issuer:      param.Server_ExternalWebUrl.GetString(),
			Synthetic:   true,
		},
		Body: selfTestBody,
		Auth: server_utils.ProbeAuth{
			Issuer:  param.Server_ExternalWebUrl.GetString(),
			Subject: "cache",
			// The cache's monitoring issuer is registered under the monitoring
			// namespace (see buildCacheNsAdsWithIssuer), so the scope below is
			// made relative to it when the token is minted.
			//
			// This used to pass the absolute path straight to NewResourceScope,
			// which produced a token authorizing
			// /pelican/monitoring/pelican/monitoring/selfTest -- the prefix
			// applied twice. It went unnoticed for as long as the cache authfile
			// still permitted public reads of the monitoring namespace, and
			// became a hard failure when that changed. See
			// https://github.com/PelicanPlatform/pelican/issues/3136.
			IssuerBasePath: server_utils.MonitoringBaseNs,
			Scopes: []server_utils.ProbeScope{
				{Authorization: token_scopes.Wlcg_Storage_Read, Path: selfTestDir},
			},
		},
		Seed:     cacheSeeder{},
		Expected: server_structs.ExpectedEdges(server_structs.CacheType),
		Steps: []server_utils.ProbeStep{{
			Op:    server_utils.ProbeOpRead,
			Edges: []server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz, server_structs.EdgeCacheHit},
		}},
	}
}

// originSelfTestPlans returns every probe the origin runs, one per export.
//
// Probes now go *into the exports themselves* rather than into the synthetic
// /pelican/monitoring namespace. That prefix resolves under Origin.RunLocation
// while real exports are symlinks to the administrator's volume, so a probe
// there established that XRootD was up and nothing whatever about the storage
// clients read: unmount the data volume and every probe still passed while every
// client request failed.
//
// A writable export gets a write-read-delete cycle in a reserved sub-prefix
// inside it, which traverses the real serving layer, the real authorization
// decision for that namespace, and the real backing store. A read-only export
// gets a read of whatever object the administrator named in HealthProbeObject,
// since the origin cannot write one of its own to read back.
func originSelfTestPlans(issuerUrl string) ([]server_utils.ProbePlan, error) {
	exports, err := server_utils.GetOriginExports()
	if err != nil {
		return nil, errors.Wrap(err, "failed to read origin exports for health probes")
	}

	storageType := server_structs.OriginStorageType(param.Origin_StorageType.GetString())
	originUrl := param.Origin_Url.GetString()

	// A natively-served origin mounts its exports under a route prefix when a
	// director shares the process, so probes have to address it the same way the
	// advertised data URL does. Probing the bare origin URL would 404 on every
	// request and report a healthy origin as broken.
	probeBaseUrl := originUrl
	if !storageType.Profile().ServedByXRootD && config.IsServerEnabled(server_structs.DirectorType) {
		if parsed, err := url.Parse(originUrl); err == nil {
			parsed.Path = server_structs.OriginDataRoutePrefix
			probeBaseUrl = parsed.String()
		}
	}

	cfg := server_utils.OriginProbeConfig{
		StoreCaps:   server_utils.OriginStoreCapabilities(),
		StorageType: storageType,
		BaseURL:     probeBaseUrl,
		IssuerUrl:   issuerUrl,
		Audiences:   []string{originUrl, param.Origin_TokenAudience.GetString()},
	}

	// The synthetic /pelican/monitoring namespace exists only on XRootD-fronted
	// origins, where the config template mounts it with an explicit
	// `xrootd.export`. A natively-served origin registers handlers for exactly its
	// configured exports (origin_serve.RegisterHandlers keys them by federation
	// prefix), so probing that namespace there is a guaranteed 404 -- reporting a
	// healthy origin as critical for failing to serve a path it was never asked
	// to serve.
	//
	// The two halves are complementary rather than redundant: fronted origins get
	// the monitoring probe, native ones get probes against their real exports.
	// Each backend has exactly one probe path that works.
	var plans []server_utils.ProbePlan
	if cfg.StorageType.Profile().ServedByXRootD {
		plans = append(plans, server_utils.MonitoringProbePlan(
			server_structs.SelfTestSuite, probeBaseUrl, issuerUrl, cfg.Audiences,
			string(cfg.StorageType),
			server_utils.OriginProbeSteps(cfg.StoreCaps)))
	}

	// Probing the real exports is what actually covers the storage edge. Whether
	// it runs is decided per-backend by RealExportProbesEnabled: on for the
	// natively-served backends, whose authorization Pelican performs itself, and
	// off for XRootD-fronted ones until their scitokens/authfile path is
	// understood.
	if server_utils.RealExportProbesEnabled(cfg.StorageType) {
		plans = append(plans, server_utils.PlanOriginProbes(exports, cfg)...)
		for prefix, why := range server_utils.UnprobedExports(exports, cfg.StoreCaps) {
			log.Debugf("Export %s is not covered by any health probe: %s", prefix, why)
		}
	}

	return plans, nil
}

// recordSelfTest folds a probe report into the component-health registry.
//
// Both self-tests report through here, so the message an operator sees names the
// step that broke and why, rather than restating that the self-test failed.
func recordSelfTest(report server_structs.TestReport) {
	// Publish the structured report for the advertisement builder as well as the
	// message for the local registry. The Director needs the former to tell a
	// broken data volume from a broken token; a one-word status cannot.
	server_utils.RecordSelfTestReport(report)

	status, msg := server_utils.SummarizeReport(report)
	if status == metrics.StatusOK {
		log.Debugln(msg)
	} else {
		log.Warningln(msg)
	}
	metrics.SetComponentHealthStatus(metrics.OriginCache_XRootD, status, msg)
}

// Run a cache self-test and log/record metrics with the test result
func doSelfMonitorCache(ctx context.Context) {
	log.Debug("Starting a new self-test monitoring cycle")
	recordSelfTest(cacheSelfTestPlan().Run(ctx).Report)
}

// probeSchedule remembers when each plan last ran, so plans carrying a
// MinInterval are not re-run every cycle.
//
// Expensive probes are throttled rather than skipped entirely, which is the
// whole difference between "S3 origins are unobservable" and "S3 origins are
// observed less often".
type probeSchedule struct {
	mu       sync.Mutex
	lastRun  map[string]time.Time
	lastSeen map[string]server_structs.TestResult
}

func newProbeSchedule() *probeSchedule {
	return &probeSchedule{
		lastRun:  map[string]time.Time{},
		lastSeen: map[string]server_structs.TestResult{},
	}
}

// due reports whether a plan should run this cycle.
func (s *probeSchedule) due(plan server_utils.ProbePlan, now time.Time) bool {
	if plan.MinInterval <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastRun[plan.Key()]
	return !ok || now.Sub(last) >= plan.MinInterval
}

// record stores a run's results so a throttled plan keeps contributing its most
// recent verdict to the aggregate report.
//
// Without this, a five-minute write probe would leave its export looking
// uncovered for the nineteen cycles in between -- reporting a blind spot that
// does not exist.
func (s *probeSchedule) record(plan server_utils.ProbePlan, now time.Time, results []server_structs.TestResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRun[plan.Key()] = now
	for _, res := range results {
		s.lastSeen[plan.Key()+"|"+res.Op] = res
	}
}

// carried returns the most recent results for a plan that did not run this cycle.
func (s *probeSchedule) carried(plan server_utils.ProbePlan) []server_structs.TestResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []server_structs.TestResult
	for _, step := range plan.Steps {
		if res, ok := s.lastSeen[plan.Key()+"|"+string(step.Op)]; ok {
			out = append(out, res)
		}
	}
	return out
}

// retain drops bookkeeping for plans that no longer exist.
//
// The schedule is package-level and outlives any single cycle, while the set of
// exports can change under a config reload. Without pruning, a removed export
// keeps its throttle state and keeps contributing its last verdict to every
// subsequent report -- health for a namespace the origin no longer serves.
func (s *probeSchedule) retain(plans []server_utils.ProbePlan) {
	live := make(map[string]bool, len(plans))
	for _, plan := range plans {
		live[plan.Key()] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.lastRun {
		if !live[key] {
			delete(s.lastRun, key)
		}
	}
	for key := range s.lastSeen {
		// lastSeen keys are "<plan key>|<op>", so match on the plan-key prefix.
		planKey := key
		if idx := strings.LastIndex(key, "|"); idx >= 0 {
			planKey = key[:idx]
		}
		if !live[planKey] {
			delete(s.lastSeen, key)
		}
	}
}

var originProbeSchedule = newProbeSchedule()

// Run an origin self-test and log/record metrics with the test result
func doSelfMonitorOrigin(ctx context.Context) {
	log.Debug("Starting a new self-test monitoring cycle")

	plans, err := originSelfTestPlans(param.Server_ExternalWebUrl.GetString())
	if err != nil {
		// Without exports there is nothing to probe, and reporting a pass would be
		// worse than reporting nothing.
		log.Warningf("Origin self-test could not build probe plans: %v", err)
		metrics.SetComponentHealthStatus(metrics.OriginCache_XRootD, metrics.StatusCritical,
			"self-test could not determine what to probe: "+err.Error())
		return
	}

	// Forget exports that have gone away since the last cycle before consulting
	// any carried results.
	originProbeSchedule.retain(plans)

	now := time.Now()
	results := make([]server_structs.TestResult, 0, len(plans)*3)
	for _, plan := range plans {
		if !originProbeSchedule.due(plan, now) {
			results = append(results, originProbeSchedule.carried(plan)...)
			continue
		}
		stepResults := plan.Run(ctx).Report.Results
		originProbeSchedule.record(plan, now, stepResults)
		results = append(results, stepResults...)
	}

	// Fold every export's plan into one report, so the coverage ledger spans the
	// whole server rather than whichever export happened to be probed last.
	recordSelfTest(server_structs.NewTestReport(
		server_structs.SelfTestSuite,
		server_structs.ExpectedEdges(server_structs.OriginType),
		results,
	))
}

// Start self-test monitoring of the origin/cache.  This will upload, download, and delete
// a generated filename every 15 seconds to the local origin.  On failure, it will
// set the xrootd component's status to critical.
func PeriodicSelfTest(ctx context.Context, ergp *errgroup.Group, isOrigin bool) {
	customInterval := param.Cache_SelfTestInterval.GetDuration()
	doSelfMonitor := doSelfMonitorCache
	maxAge := param.Cache_SelfTestMaxAge.GetDuration()
	if isOrigin {
		doSelfMonitor = doSelfMonitorOrigin
		customInterval = param.Origin_SelfTestInterval.GetDuration()
		maxAge = param.Origin_SelfTestMaxAge.GetDuration()
	}

	ticker := time.NewTicker(customInterval)
	firstRound := time.After(5 * time.Second)

	ergp.Go(func() error {
		defer ticker.Stop()
		for {
			select {
			case <-firstRound:
				doSelfMonitor(ctx)
				// This is a failsafe mechanism to the self-test metric (OriginCache_XRootD):
				// have the server status reported as critical if the check for age is too old.
				// TODO: if OriginCache_XRootD component hasn't been set for `maxAge` time,
				// it means no self-test has been run. Maybe we should shutdown the server instead?
				metrics.ValidateComponentHealthAge(metrics.OriginCache_XRootD, maxAge)
			case <-ticker.C:
				doSelfMonitor(ctx)
				metrics.ValidateComponentHealthAge(metrics.OriginCache_XRootD, maxAge)
			case <-ctx.Done():
				return nil
			}
		}
	})
}
