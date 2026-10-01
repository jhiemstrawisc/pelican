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
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelicanplatform/pelican/server_structs"
)

// originEdges is what a client of an origin depends on, used as the coverage
// denominator throughout these tests.
var originEdges = []server_structs.Edge{
	server_structs.EdgeFront,
	server_structs.EdgeAuthz,
	server_structs.EdgeOriginData,
}

// recordingServer is a probe target that remembers what it was asked to do.
type recordingServer struct {
	mu      sync.Mutex
	methods []string
	objects map[string]string
	// status, when non-zero, is returned instead of performing the operation.
	status int
}

func newRecordingServer(t *testing.T) (*recordingServer, *httptest.Server) {
	t.Helper()
	rec := &recordingServer{objects: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.methods = append(rec.methods, r.Method+" "+r.URL.Path)

		if rec.status != 0 {
			w.WriteHeader(rec.status)
			return
		}

		switch r.Method {
		case http.MethodPut:
			buf := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				if _, err := r.Body.Read(buf); err != nil && err.Error() != "EOF" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			rec.objects[r.URL.Path] = string(buf)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			body, ok := rec.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case http.MethodDelete:
			delete(rec.objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *recordingServer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...)
}

// anonPlan builds a write/read/delete plan that needs no token machinery, so the
// engine's mechanics can be tested without a configured issuer.
//
// The binding is deliberately a real one. Whether a *synthetic* binding may claim
// coverage of the data plane is a policy question, tested in health_plan_test.go;
// mixing it in here would make every mechanics assertion depend on it.
func anonPlan(baseURL string) ProbePlan {
	return ProbePlan{
		Suite:    server_structs.SelfTestSuite,
		Binding:  server_structs.BindingRef{Namespace: "/data/ns", Synthetic: false},
		BaseURL:  baseURL,
		Prefix:   "/data/ns/probe",
		Body:     "probe body",
		Auth:     ProbeAuth{Anonymous: true},
		Expected: originEdges,
		Steps: []ProbeStep{
			{Op: ProbeOpWrite, Edges: []server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz, server_structs.EdgeOriginData}},
			{Op: ProbeOpRead, Edges: []server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz, server_structs.EdgeOriginData}},
			{Op: ProbeOpDelete, Edges: []server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz, server_structs.EdgeOriginData}},
		},
	}
}

func TestProbePlanRunHappyPath(t *testing.T) {
	rec, srv := newRecordingServer(t)

	run := anonPlan(srv.URL).Run(context.Background())

	require.Equal(t, server_structs.TestOutcomePass, run.Report.Outcome)
	require.Len(t, run.Report.Results, 3)
	for _, res := range run.Report.Results {
		assert.Equal(t, server_structs.TestOutcomePass, res.Outcome, "step %s", res.Op)
		assert.Empty(t, res.Reason)
	}

	// The whole point of the report: it says what it covered.
	assert.ElementsMatch(t, originEdges, run.Report.Covered)
	assert.Empty(t, run.Report.Uncovered)

	// And it names the object, so a caller that must evict later can.
	assert.Contains(t, run.ObjectPath, "/data/ns/probe/self-test-")

	seen := rec.seen()
	require.Len(t, seen, 3)
	assert.Contains(t, seen[0], "PUT")
	assert.Contains(t, seen[1], "GET")
	assert.Contains(t, seen[2], "DELETE")
}

func TestProbePlanRunClassifiesHTTPFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantReason server_structs.TestFailureReason
	}{
		{"unauthorized", http.StatusUnauthorized, server_structs.ReasonAuthorization},
		{"forbidden", http.StatusForbidden, server_structs.ReasonAuthorization},
		{"not found", http.StatusNotFound, server_structs.ReasonNotFound},
		{"server error", http.StatusInternalServerError, server_structs.ReasonServerError},
		{"insufficient storage", http.StatusInsufficientStorage, server_structs.ReasonServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, srv := newRecordingServer(t)
			rec.status = tt.status

			run := anonPlan(srv.URL).Run(context.Background())

			require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
			require.NotEmpty(t, run.Report.Results)
			assert.Equal(t, tt.wantReason, run.Report.Results[0].Reason)
			assert.Equal(t, tt.status, run.Report.Results[0].HTTPStatus)
		})
	}
}

// A failed write means the read and delete cannot mean anything. They are
// recorded as skipped rather than failed, so a single broken step does not
// inflate into three.
func TestProbePlanRunShortCircuitsAfterFailure(t *testing.T) {
	rec, srv := newRecordingServer(t)
	rec.status = http.StatusForbidden

	run := anonPlan(srv.URL).Run(context.Background())

	require.Len(t, run.Report.Results, 3)
	assert.Equal(t, server_structs.TestOutcomeFail, run.Report.Results[0].Outcome)
	assert.Equal(t, server_structs.TestOutcomeSkip, run.Report.Results[1].Outcome)
	// The delete still runs, so a failed read does not leak the probe object.
	assert.NotEqual(t, server_structs.TestOutcomeSkip, run.Report.Results[2].Outcome)

	// A rejected token proves the front door and authorization were reached and
	// says nothing about the storage behind them, so the data plane stays
	// unknown rather than being claimed as covered.
	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeFront, server_structs.EdgeAuthz},
		run.Report.Covered)
	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeOriginData},
		run.Report.Uncovered)
}

func TestProbePlanRunDetectsContentMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte("somebody else's bytes"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	run := anonPlan(srv.URL).Run(context.Background())

	require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
	readResult := run.Report.Results[1]
	assert.Equal(t, string(ProbeOpRead), readResult.Op)
	assert.Equal(t, server_structs.ReasonContentMismatch, readResult.Reason)
	// The server answered; the fault is in what it served, not in reaching it.
	assert.Equal(t, http.StatusOK, readResult.HTTPStatus)
	// A wrong answer is still an answer: the data plane was observed, so it is
	// covered even though it is broken.
	assert.ElementsMatch(t, originEdges, run.Report.Covered)
	assert.Empty(t, run.Report.Uncovered)
}

// The Director appends a newline to the synthetic probe object it serves to
// caches, so a byte-exact comparison would fail for every cache in a federation.
func TestProbePlanRunToleratesTrailingNewline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("probe body\n"))
	}))
	t.Cleanup(srv.Close)

	plan := anonPlan(srv.URL)
	plan.Steps = plan.Steps[1:2] // read only
	run := plan.Run(context.Background())

	assert.Equal(t, server_structs.TestOutcomePass, run.Report.Outcome)
}

func TestProbePlanRunUsesExactObjectPathWhenGiven(t *testing.T) {
	rec, srv := newRecordingServer(t)

	plan := anonPlan(srv.URL)
	plan.ObjectPath = "/pelican/monitoring/directorTest/host/2026-08-09/director-test-x.txt"
	run := plan.Run(context.Background())

	assert.Equal(t, plan.ObjectPath, run.ObjectPath)
	for _, seen := range rec.seen() {
		assert.Contains(t, seen, plan.ObjectPath)
	}
}

func TestProbePlanRunReportsUnreachableTarget(t *testing.T) {
	plan := anonPlan("http://127.0.0.1:1") // reserved port; nothing listens
	plan.Steps = plan.Steps[1:2]

	run := plan.Run(context.Background())

	require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
	assert.Equal(t, server_structs.ReasonConnection, run.Report.Results[0].Reason)
	// Nothing answered, so only the front door was exercised.
	assert.ElementsMatch(t, []server_structs.Edge{server_structs.EdgeFront}, run.Report.Covered)
	assert.ElementsMatch(t,
		[]server_structs.Edge{server_structs.EdgeAuthz, server_structs.EdgeOriginData},
		run.Report.Uncovered)
}

func TestProbePlanRunReportsCancellationAsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan := anonPlan(srv.URL)
	plan.Steps = plan.Steps[1:2]
	run := plan.Run(ctx)

	require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
	assert.Equal(t, server_structs.ReasonTimeout, run.Report.Results[0].Reason)
}

func TestProbePlanRunRejectsUnparsableBaseURL(t *testing.T) {
	plan := anonPlan("://not-a-url")

	run := plan.Run(context.Background())

	require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
	assert.Equal(t, server_structs.ReasonInternal, run.Report.Results[0].Reason)
}

// stubSeeder stands in for the cache's local object seeding.
type stubSeeder struct {
	url         string
	err         error
	cleanedUp   bool
	cleanupCall int
}

func (s *stubSeeder) Seed(ctx context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.url, nil
}

func (s *stubSeeder) Cleanup(ctx context.Context, objectURL string) error {
	s.cleanedUp = true
	s.cleanupCall++
	return nil
}

func TestProbePlanRunSeedsThenReads(t *testing.T) {
	rec, srv := newRecordingServer(t)
	rec.objects["/pelican/monitoring/selfTest/seeded.txt"] = "probe body"

	seeder := &stubSeeder{url: srv.URL + "/pelican/monitoring/selfTest/seeded.txt"}
	plan := anonPlan(srv.URL)
	plan.Seed = seeder
	plan.Steps = plan.Steps[1:2] // read only; a cache has no upload path

	run := plan.Run(context.Background())

	assert.Equal(t, server_structs.TestOutcomePass, run.Report.Outcome)
	assert.True(t, seeder.cleanedUp, "a seeded object must be removed by whoever seeded it")
	assert.Equal(t, 1, seeder.cleanupCall)
}

// Seeding is local work, so a failure there is storage trouble rather than a
// network problem -- the distinction an operator needs to know where to look.
func TestProbePlanRunClassifiesSeedFailureAsStorage(t *testing.T) {
	_, srv := newRecordingServer(t)

	plan := anonPlan(srv.URL)
	plan.Seed = &stubSeeder{err: errors.New("no space left on device")}

	run := plan.Run(context.Background())

	require.Equal(t, server_structs.TestOutcomeFail, run.Report.Outcome)
	require.Len(t, run.Report.Results, 1, "no step can run if the object was never placed")
	assert.Equal(t, server_structs.ReasonStorage, run.Report.Results[0].Reason)
	// The seed failure declared no edges, so the whole client path stays unknown.
	assert.Empty(t, run.Report.Covered)
	assert.ElementsMatch(t, originEdges, run.Report.Uncovered)
}
