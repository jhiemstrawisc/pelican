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

// One probe engine for every health test in Pelican.
//
// Before this, the same write-read-delete probe existed three times -- here, in
// xrootd/self_monitor.go for caches, and in director/cache_monitor.go -- with two
// identically named generateFileTestScitoken functions in different packages and
// an interface that nothing implemented. Each copy invented its own error prose,
// so the only way to ask "how many origins are failing for the same reason" was
// to grep logs. See https://github.com/PelicanPlatform/pelican/issues/3107.
//
// A plan describes what to probe and how to authorize it; Run executes the steps
// and returns a report that records which edges of the client's path were
// actually traversed. See docs/health-testing-design.md.
package server_utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_structs"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
)

type (
	// ProbeOp is the operation a step performs against the serving layer.
	ProbeOp string

	// ProbeStep is one operation plus the edges of the client path it traverses.
	//
	// The edges are declared rather than inferred because only the caller knows
	// what a given request actually exercises: a GET against a cache covers the
	// hit path if the object is already resident and the miss path if it is not.
	ProbeStep struct {
		Op    ProbeOp
		Edges []server_structs.Edge
	}

	// ProbeScope is one authorization a probe needs, named by the ABSOLUTE
	// namespace path it applies to.
	//
	// Absolute is the entire point. A WLCG scope's resource is resolved relative
	// to the base path its issuer is registered under, so the string that belongs
	// on the wire is never the path a caller has in mind. Asking every call site
	// to perform that subtraction by hand is what produced a token authorizing
	// /pelican/monitoring/pelican/monitoring/selfTest -- the bug in
	// https://github.com/PelicanPlatform/pelican/issues/3136, which survived a
	// full rewrite of this subsystem because the convention lived in comments
	// rather than in a type.
	//
	// So callers state the path they actually mean. The subtraction happens once,
	// in ProbeAuth.resourceScopes, which refuses to mint a token for a path the
	// issuer has no authority over rather than silently emitting a scope that
	// points somewhere else.
	ProbeScope struct {
		Authorization token_scopes.TokenScope
		// Path is absolute within the federation namespace, e.g.
		// "/ligo/frames/.pelican-monitoring".
		Path string
	}

	// ProbeAuth describes how to authorize probe requests.
	ProbeAuth struct {
		// Anonymous sends no Authorization header at all. Used for probes against
		// public namespaces, where presenting a token would test something the
		// client path does not use.
		Anonymous bool
		// Issuer is the token issuer. Empty means the federation's discovery
		// endpoint, with a fallback described on tokensToTry.
		Issuer string
		// Subject defaults to Server.ExternalWebUrl.
		Subject string
		// Audiences empty means "any".
		Audiences []string
		// IssuerBasePath is the base path Issuer is registered under in the
		// serving layer's token configuration -- the origin's federation prefix
		// for an export probe, /pelican/monitoring for a monitoring probe.
		//
		// Scopes are made relative to it exactly once, when the token is minted.
		// Empty means the issuer is authorized at the root, so scope paths stand
		// as written.
		IssuerBasePath string
		// Scopes and RawScope are alternatives; RawScope exists because the origin
		// probes have always requested the un-parsed "storage.read:/ storage.modify:/"
		// and narrowing that is a behavior change owed its own review.
		Scopes   []ProbeScope
		RawScope string
	}

	// ProbeSeeder places a probe object without going through the serving layer.
	//
	// A cache has no upload path, so its self-test cannot write the object it
	// intends to read. Seeding is the honest name for what that probe does; it is
	// also the reason a cache self-test proves less than an origin's, which the
	// report now records rather than leaving to the reader.
	ProbeSeeder interface {
		Seed(ctx context.Context) (objectURL string, err error)
		Cleanup(ctx context.Context, objectURL string) error
	}

	// ProbePlan is a single probe run against a single binding.
	ProbePlan struct {
		Suite   server_structs.TestSuite
		Binding server_structs.BindingRef

		// BaseURL is the data endpoint to probe (an XRootD URL, or a cache URL).
		BaseURL string
		// Prefix is the namespace-relative directory in which to mint a probe
		// object name. Ignored when ObjectPath is set.
		Prefix string
		// ObjectPath names an exact namespace-relative object to probe rather than
		// minting one. Callers that must clean up afterwards (the Director evicts
		// its cache probe objects) set this so they know the name in advance.
		ObjectPath string
		// Body is the content written, and the content a read is checked against.
		Body string

		Auth  ProbeAuth
		Steps []ProbeStep
		Seed  ProbeSeeder

		// Expected is the denominator for coverage: the edges a client of this
		// server depends on. What the run does not reach lands in
		// TestReport.Uncovered.
		Expected []server_structs.Edge

		// UserAgent overrides the default, which is derived from the suite.
		UserAgent string

		// MinInterval is the floor between runs of this plan.
		//
		// Cost, not capability. A write probe against S3 is egress and a
		// credentialed round trip, so it runs on a slower cadence than one against
		// a local disk -- but it does run, where the old model refused to probe
		// those backends at all. Zero means every cycle.
		MinInterval time.Duration

		// SlowThreshold marks a completed step as degraded when it takes longer
		// than this. Zero disables the check.
		//
		// Probes move a handful of bytes, so any threshold here is generous by
		// construction; the point is to make the degraded middle of a failure
		// representable at all, not to measure throughput.
		SlowThreshold time.Duration
	}

	// ProbeRun is a completed probe: the report that crosses the wire, plus the
	// object the probe left behind, if any, for callers that cannot delete it
	// inline.
	ProbeRun struct {
		Report     server_structs.TestReport
		ObjectPath string
	}
)

const (
	ProbeOpWrite  ProbeOp = "write"
	ProbeOpRead   ProbeOp = "read"
	ProbeOpDelete ProbeOp = "delete"
)

// The synthetic namespace every data server exports for probing, and the two
// subdirectories within it.
//
// Objects here are deliberately *not* in any namespace a client reads. On a POSIX
// origin this prefix resolves under the run location rather than the
// administrator's data volume, so a probe confined to it proves far less than it
// appears to -- which is why every result records its binding, and why probes
// against real exports are the next thing to build.
const (
	MonitoringBaseNs string = "/pelican/monitoring"
	DirectorTestDir  string = "directorTest"
	SelfTestDir      string = "selfTest"
)

const (
	SelfTestBody     string = "This object was created by the Pelican self-test functionality"
	DirectorTestBody string = "This object was created by the Pelican director-test functionality"
)

// SelfTestPrefix and DirectorTestPrefix are the namespace-relative directories
// the two suites write into.
func SelfTestPrefix() string { return path.Join(MonitoringBaseNs, SelfTestDir) }

// DirectorTestPrefix scopes each Director's probe objects under its own hostname
// and day, so several Directors sharing a cache do not collide and old objects
// are easy to sweep.
func DirectorTestPrefix(directorID string, when time.Time) string {
	return path.Join(MonitoringBaseNs, DirectorTestDir, directorID, when.Format("2006-01-02"))
}

// ProbeBodyFor returns the object contents a suite writes and expects to read back.
func ProbeBodyFor(suite server_structs.TestSuite) string {
	if suite == server_structs.DirectorTestSuite {
		return DirectorTestBody
	}
	return SelfTestBody
}

// lastSelfTest holds the most recent self-test report so the advertisement
// builder can attach it.
//
// The report has to outlive the probe cycle that produced it: probes run every
// 15s while advertisements go out every 60s, so the two are not in step and the
// ad needs the latest result rather than one it triggers itself.
var lastSelfTest atomic.Pointer[server_structs.TestReport]

// RecordSelfTestReport publishes a completed self-test for the advertisement
// builder to pick up.
func RecordSelfTestReport(report server_structs.TestReport) {
	lastSelfTest.Store(&report)
}

// LastSelfTestReport returns the most recent self-test report, or nil if none has
// completed yet. The caller must not mutate it.
func LastSelfTestReport() *server_structs.TestReport {
	return lastSelfTest.Load()
}

// lastDirectorTest holds the most recent report the Director sent us about
// itself testing this server.
var lastDirectorTest atomic.Pointer[server_structs.TestReport]

// RecordDirectorTestReport stores the Director's structured verdict so this
// server's own admin UI can show what the Director saw.
//
// The field crossed the wire before this and was dropped on arrival, so an
// origin administrator could read the Director's prose but not which step failed
// or what the Director had been unable to cover.
func RecordDirectorTestReport(report server_structs.TestReport) {
	lastDirectorTest.Store(&report)
}

// LastDirectorTestReport returns the Director's most recent report about this
// server, or nil. The caller must not mutate it.
func LastDirectorTestReport() *server_structs.TestReport {
	return lastDirectorTest.Load()
}

// RedactReport returns a copy of a report with free-text detail removed, leaving
// every outcome, reason, and coverage claim intact.
//
// This is what Origin.ShareHealthDetails=false does to a self-test. An origin may
// decline to explain itself; it may not decline to say whether it works or what
// it checked. Reason survives because a one-word classification is not
// site-identifying the way a filesystem path in a Go error string can be.
func RedactReport(report *server_structs.TestReport) *server_structs.TestReport {
	if report == nil {
		return nil
	}
	redacted := *report
	redacted.Results = make([]server_structs.TestResult, len(report.Results))
	for i, res := range report.Results {
		res.Detail = ""
		redacted.Results[i] = res
	}
	return &redacted
}

// SummarizeReport renders a probe report into the status and message the
// component-health registry expects.
//
// The message names the reason and the failing step rather than restating that
// "the self-test failed", which is what the previous messages did -- leaving an
// operator to work out from logs which of upload, download, or delete broke and
// why. Uncovered edges are appended when any remain, because a report that
// passed everything it tried is not the same as one that tried everything.
func SummarizeReport(report server_structs.TestReport) (metrics.HealthStatusEnum, string) {
	when := time.Unix(report.At, 0).Format(time.RFC3339)

	switch report.Outcome {
	case server_structs.TestOutcomePass:
		msg := fmt.Sprintf("%s succeeded at %s", report.Suite, when)
		return metrics.StatusOK, appendCoverageGap(msg, report)

	case server_structs.TestOutcomeDegrade:
		return metrics.StatusDegraded, appendCoverageGap(describeWorst(report, "degraded"), report)

	case server_structs.TestOutcomeFail:
		return metrics.StatusCritical, appendCoverageGap(describeWorst(report, "failed"), report)

	default:
		// Nothing ran -- but there are two reasons for that, and conflating them
		// is expensive in opposite directions.
		//
		// If every step was inapplicable (a read-only origin gives the Director no
		// object of its own to probe), nothing is wrong and saying "warning"
		// deprioritizes a healthy server in routing and trains operators to ignore
		// the field. Most origins in a public-data federation are read-only.
		//
		// If steps should have run and did not, that genuinely is a gap, and
		// recording it as success is how a server nobody is watching comes to look
		// identical to a verified one.
		//
		// Either way the coverage ledger still reports what went unchecked, so the
		// "we did not look" signal is never lost -- it is carried by Uncovered
		// rather than by an alarming status.
		if notApplicable(report) {
			return metrics.StatusOK, appendCoverageGap(
				fmt.Sprintf("%s is not applicable to this server at %s", report.Suite, when), report)
		}
		return metrics.StatusWarning, appendCoverageGap(
			fmt.Sprintf("%s did not run at %s", report.Suite, when), report)
	}
}

// describeWorst names the first step with the report's overall outcome.
func describeWorst(report server_structs.TestReport, verb string) string {
	for _, res := range report.Results {
		if res.Outcome != report.Outcome {
			continue
		}
		msg := fmt.Sprintf("%s %s during %s", report.Suite, verb, res.Op)
		if res.Reason != "" {
			msg += " (" + string(res.Reason) + ")"
		}
		if res.Binding.Namespace != "" {
			msg += " on " + res.Binding.Namespace
		}
		if res.Detail != "" {
			msg += ": " + res.Detail
		}
		return msg
	}
	return fmt.Sprintf("%s %s", report.Suite, verb)
}

func appendCoverageGap(msg string, report server_structs.TestReport) string {
	if len(report.Uncovered) == 0 {
		return msg
	}
	gaps := make([]string, 0, len(report.Uncovered))
	for _, edge := range report.Uncovered {
		gaps = append(gaps, edge.String())
	}
	return msg + "; not covered by this test: " + strings.Join(gaps, ", ")
}

// Key identifies a plan across cycles, so a scheduler can honor MinInterval.
//
// Suite plus binding namespace is enough: an origin runs at most one plan per
// export per suite.
func (p ProbePlan) Key() string {
	return string(p.Suite) + "|" + p.Binding.Namespace
}

// Run executes every step in order and returns a coverage-bearing report.
//
// Unlike the (bool, error) pairs it replaces, there is exactly one way to ask
// whether a probe succeeded: report.Outcome. The old shape allowed
// ok == false alongside err == nil, which produced a dead branch in one caller
// and a nil dereference in another.
//
// Execution stops at the first failing step, since every later step depends on it
// -- but a cleanup step still runs, so a failed read does not leak the object it
// was reading.
func (p ProbePlan) Run(ctx context.Context) ProbeRun {
	run := ProbeRun{ObjectPath: p.ObjectPath}
	results := make([]server_structs.TestResult, 0, len(p.Steps))

	objectURL, seedResult := p.resolveObject(ctx)
	if seedResult != nil {
		results = append(results, *seedResult)
		run.Report = server_structs.NewTestReport(p.Suite, p.Expected, results)
		return run
	}
	if p.ObjectPath == "" && objectURL != "" {
		if parsed, err := url.Parse(objectURL); err == nil {
			run.ObjectPath = parsed.Path
		}
	}

	failed := false
	for _, step := range p.Steps {
		if failed && step.Op != ProbeOpDelete {
			results = append(results, p.skipped(step, server_structs.ReasonNotApplicable,
				"skipped because an earlier step failed"))
			continue
		}
		res := p.runStep(ctx, step, objectURL)
		results = append(results, res)
		if res.Outcome.IsFailure() {
			failed = true
		}
	}

	if p.Seed != nil && objectURL != "" {
		// Seeded objects are removed by whoever created them; the serving layer
		// never saw them go in and cannot be asked to take them out.
		if err := p.Seed.Cleanup(ctx, objectURL); err != nil {
			log.Warningf("Failed to clean up seeded probe object %s: %v", objectURL, err)
		}
	}

	run.Report = server_structs.NewTestReport(p.Suite, p.Expected, results)
	return run
}

// resolveObject determines the URL the steps operate on, seeding it first when
// the plan has a seeder. A non-nil result means seeding failed and no step can run.
func (p ProbePlan) resolveObject(ctx context.Context) (string, *server_structs.TestResult) {
	if p.Seed != nil {
		start := time.Now()
		objectURL, err := p.Seed.Seed(ctx)
		if err != nil {
			reason, detail := server_structs.ClassifyTestError(err, nil)
			if reason == server_structs.ReasonConnection {
				// Seeding is local work, so a non-specific failure is storage,
				// not network.
				reason = server_structs.ReasonStorage
			}
			res := server_structs.TestResult{
				Binding:   p.Binding,
				Op:        string(ProbeOpWrite),
				Outcome:   server_structs.TestOutcomeFail,
				Reason:    reason,
				Detail:    detail,
				LatencyMs: time.Since(start).Milliseconds(),
				At:        time.Now().Unix(),
			}
			return "", &res
		}
		return objectURL, nil
	}

	objectPath := p.ObjectPath
	if objectPath == "" {
		objectPath = path.Join(p.Prefix, p.Suite.String()+"-"+time.Now().Format(time.RFC3339)+".txt")
	}
	base, err := url.Parse(p.BaseURL)
	if err != nil {
		res := server_structs.TestResult{
			Binding: p.Binding,
			Outcome: server_structs.TestOutcomeFail,
			Reason:  server_structs.ReasonInternal,
			Detail:  errors.Wrapf(err, "probe base URL %q is not parseable", p.BaseURL).Error(),
			At:      time.Now().Unix(),
		}
		return "", &res
	}
	return base.JoinPath(objectPath).String(), nil
}

func (p ProbePlan) skipped(step ProbeStep, reason server_structs.TestFailureReason, detail string) server_structs.TestResult {
	return server_structs.TestResult{
		Binding: p.Binding,
		Edges:   step.Edges,
		Op:      string(step.Op),
		Outcome: server_structs.TestOutcomeSkip,
		Reason:  reason,
		Detail:  detail,
		At:      time.Now().Unix(),
	}
}

// runStep performs one operation and classifies the result.
func (p ProbePlan) runStep(ctx context.Context, step ProbeStep, objectURL string) server_structs.TestResult {
	res := server_structs.TestResult{
		Binding: p.Binding,
		Edges:   step.Edges,
		Op:      string(step.Op),
		At:      time.Now().Unix(),
	}

	method := http.MethodGet
	body := ""
	switch step.Op {
	case ProbeOpWrite:
		method = http.MethodPut
		body = p.Body
	case ProbeOpRead:
		method = http.MethodGet
	case ProbeOpDelete:
		method = http.MethodDelete
	default:
		res.Outcome = server_structs.TestOutcomeFail
		res.Reason = server_structs.ReasonInternal
		res.Detail = "unsupported probe operation " + string(step.Op)
		return res
	}

	start := time.Now()
	status, respBody, err := p.do(ctx, objectURL, method, body)
	res.LatencyMs = time.Since(start).Milliseconds()
	res.HTTPStatus = status

	if err != nil {
		res.Outcome = server_structs.TestOutcomeFail
		if errors.Is(err, errProbePlanInvalid) {
			// The request never left this process, so nothing was learned about
			// the server. Saying "connection" here would point an operator at the
			// network for what is a bug in Pelican's own plan construction.
			res.Reason, res.Detail = server_structs.ReasonInternal, err.Error()
			return res
		}
		res.Reason, res.Detail = server_structs.ClassifyTestError(err, nil)
		if res.Reason == server_structs.ReasonNone {
			res.Reason = server_structs.ReasonInternal
		}
		return res
	}

	if status < 200 || status > 299 {
		res.Outcome = server_structs.TestOutcomeFail
		res.Reason, res.Detail = server_structs.ClassifyTestError(nil, &http.Response{StatusCode: status, Status: respBody})
		return res
	}

	// A read that returns the wrong bytes is a distinct failure from one that
	// errors: the serving layer answered, so the fault is in what it is serving.
	if step.Op == ProbeOpRead && p.Body != "" && trimTrailingNewline(respBody) != p.Body {
		res.Outcome = server_structs.TestOutcomeFail
		res.Reason = server_structs.ReasonContentMismatch
		res.Detail = "probe object contents do not match what was written"
		return res
	}

	// A step that completed but took far longer than a probe of a few bytes ought
	// to is neither a pass nor a failure. Recording it as a plain pass is what
	// makes the middle of the latent-fault to complete-failure progression
	// invisible: a server serving at a crawl looks identical to a healthy one
	// right up until it stops answering altogether.
	res.Outcome = server_structs.TestOutcomePass
	if p.SlowThreshold > 0 && res.LatencyMs > p.SlowThreshold.Milliseconds() {
		res.Outcome = server_structs.TestOutcomeDegrade
		res.Reason = server_structs.ReasonSlow
		res.Detail = fmt.Sprintf("%s took %dms, over the %s threshold",
			step.Op, res.LatencyMs, p.SlowThreshold)
	}
	return res
}

// trimTrailingNewline drops a single trailing newline. The Director serves its
// synthetic probe object with one appended, so a byte-exact comparison against
// the expected body would fail for every cache in the federation.
func trimTrailingNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

// do issues the request, retrying with a fallback token when the server rejects
// the first one.
func (p ProbePlan) do(ctx context.Context, objectURL, method, body string) (status int, respBody string, err error) {
	if p.Auth.Anonymous {
		return p.request(ctx, objectURL, method, body, "")
	}

	tokens, err := p.tokensToTry()
	if err != nil {
		return 0, "", err
	}

	for i, tok := range tokens {
		status, respBody, err = p.request(ctx, objectURL, method, body, tok)
		if err != nil {
			// A transport failure is not something a different issuer can fix,
			// and retrying doubles the load on a server that is already
			// struggling. Report it as-is.
			return status, respBody, err
		}
		if status >= 200 && status <= 299 {
			return status, respBody, nil
		}
		if i == len(tokens)-1 {
			return status, respBody, nil
		}
	}
	return status, respBody, err
}

func (p ProbePlan) request(ctx context.Context, objectURL, method, body, tok string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, objectURL, bytes.NewBufferString(body))
	if err != nil {
		return 0, "", errors.Wrapf(err, "failed to create %s request for health probe", method)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("User-Agent", p.userAgent())

	resp, err := config.GetClient().Do(req)
	if err != nil {
		return 0, "", errors.Wrapf(err, "failed to %s health probe object", method)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", errors.Wrap(err, "failed to read health probe response body")
	}
	return resp.StatusCode, string(raw), nil
}

func (p ProbePlan) userAgent() string {
	if p.UserAgent != "" {
		return p.UserAgent
	}
	// The old shared helper hardcoded "pelican-director" regardless of caller, so
	// an origin's own self-test announced itself as a director.
	if p.Suite == server_structs.DirectorTestSuite {
		return "pelican-director/" + config.GetVersion()
	}
	return "pelican-server/" + config.GetVersion()
}

// tokensToTry returns the tokens to present, in order.
//
// When the plan names an issuer -- an origin or cache probing itself -- there is
// exactly one. When it does not, the caller is a Director, and there are two: one
// issued by the federation's discovery endpoint, and a fallback issued by the
// Director's own external URL for origins predating the federation issuer. The
// fallback is only reached if the first is rejected.
//
// The old code ran this fallback unconditionally, including for self-tests where
// both branches produced the identical token -- so every failed self-test step
// issued the same request twice.
func (p ProbePlan) tokensToTry() ([]string, error) {
	externalUrl := param.Server_ExternalWebUrl.GetString()
	if externalUrl == "" {
		return nil, errors.Errorf("failed to create health probe token: %s is empty",
			param.Server_ExternalWebUrl.GetName())
	}

	issuers := []string{}
	if p.Auth.Issuer != "" {
		issuers = append(issuers, p.Auth.Issuer)
	} else {
		fedInfo, err := config.GetFederation(context.Background())
		if err != nil {
			return nil, errors.Wrap(err, "failed to get federation info for the health probe token")
		}
		if fedInfo.DiscoveryEndpoint == "" {
			return nil, errors.New("failed to create health probe token: federation discovery endpoint is empty")
		}
		issuers = append(issuers, fedInfo.DiscoveryEndpoint, externalUrl)
	}

	tokens := make([]string, 0, len(issuers))
	for _, issuer := range issuers {
		cfg := token.NewWLCGToken()
		cfg.Lifetime = time.Minute
		cfg.Issuer = issuer
		cfg.Subject = p.Auth.Subject
		if cfg.Subject == "" {
			cfg.Subject = externalUrl
		}
		if len(p.Auth.Audiences) == 0 {
			cfg.AddAudienceAny()
		} else {
			cfg.AddAudiences(p.Auth.Audiences...)
		}
		if p.Auth.RawScope != "" {
			cfg.AddRawScope(p.Auth.RawScope)
		}
		if len(p.Auth.Scopes) > 0 {
			scopes, err := p.Auth.resourceScopes()
			if err != nil {
				return nil, err
			}
			cfg.AddResourceScopes(scopes...)
		}

		tok, err := cfg.CreateToken()
		if err != nil {
			return nil, errors.Wrap(err, "failed to create health probe token")
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

// errProbePlanInvalid marks a failure that is Pelican's own bug rather than
// anything wrong with the server being probed.
//
// It matters for the report: a probe that never left the process must not be
// classified as a connection failure, or a misconfigured plan reads in the admin
// UI as an unreachable server and sends an operator looking at the network.
var errProbePlanInvalid = errors.New("probe plan is invalid")

// ResolvedScopes returns the scope resources this plan's token will carry, in
// the issuer-relative form that goes on the wire.
//
// Exported so a caller can see what a probe is about to ask for without minting
// a token, which needs a signing key. That is what makes the relativity rule
// checkable from the packages that build plans, rather than only from inside
// this one.
func (p ProbePlan) ResolvedScopes() ([]token_scopes.ResourceScope, error) {
	return p.Auth.resourceScopes()
}

// resourceScopes converts the plan's absolute scope paths into the
// issuer-relative resources that belong on the wire.
func (a ProbeAuth) resourceScopes() ([]token_scopes.ResourceScope, error) {
	scopes := make([]token_scopes.ResourceScope, 0, len(a.Scopes))
	for _, scope := range a.Scopes {
		relative, err := relativeToBasePath(scope.Path, a.IssuerBasePath)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, token_scopes.NewResourceScope(scope.Authorization, relative))
	}
	return scopes, nil
}

// relativeToBasePath subtracts an issuer's base path from an absolute namespace
// path, yielding the resource a WLCG scope should carry.
//
// It fails rather than guessing when the path lies outside the base path. That
// case is always a bug -- the issuer has no authority there -- and the failure
// mode it replaces is silent: NewResourceScope would happily build a scope that
// the serving layer then resolves to a wholly different location, which is the
// shape of #3136. A loud error at mint time costs one failed probe cycle; the
// quiet version cost a release.
func relativeToBasePath(absPath, basePath string) (string, error) {
	cleaned := path.Clean("/" + strings.TrimSpace(absPath))
	base := strings.TrimRight(path.Clean("/"+strings.TrimSpace(basePath)), "/")
	if base == "" {
		// The issuer is authorized at the root, so the path stands as written.
		return cleaned, nil
	}

	if cleaned != base && !strings.HasPrefix(cleaned, base+"/") {
		return "", errors.Wrapf(errProbePlanInvalid,
			"cannot scope a probe token to %q: its issuer is only authorized under %q, "+
				"so the scope would resolve to %q",
			cleaned, base, path.Join(base, cleaned))
	}

	relative := strings.TrimPrefix(cleaned, base)
	if relative == "" {
		relative = "/"
	}
	return relative, nil
}

// notApplicable reports whether a run produced no verdict because nothing could
// meaningfully be probed, as opposed to failing to run something that should
// have.
//
// A report with no results at all counts: that is what a plan with no applicable
// steps produces.
func notApplicable(report server_structs.TestReport) bool {
	for _, res := range report.Results {
		if res.Outcome != server_structs.TestOutcomeSkip {
			return false
		}
		switch res.Reason {
		case server_structs.ReasonNotApplicable, server_structs.ReasonDisabled:
		default:
			return false
		}
	}
	return true
}
