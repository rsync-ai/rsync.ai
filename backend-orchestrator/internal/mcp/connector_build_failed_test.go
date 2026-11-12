package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A connector image that fails to build is reported with the end of its build log,
// never as "still being set up". Before, a build failure reached no caller: Test
// Connection and Save answered "still being set up — try again shortly" on every
// retry, and a batch run fell back to stdio on the orchestrator's own interpreter.
// The class is "a failure the caller is never told about, so it is retried as if it
// were progress" (tool-generator side: test_a_failed_image_build_is_reported.py).

// testBuildFailure is what tool-generator's /v1/deploy answers for a failed build
// (deployment/routes.py BUILD_FAILED_PREFIX). Its log names a missing module on
// purpose: an import error is otherwise mapped to "still being set up".
const testBuildFailure = "connector image failed to build. End of the build log:\n" +
	"#9 [4/4] RUN python -c \"import acme\"\n" +
	"ModuleNotFoundError: No module named 'acme'"

func TestABuildFailureIsNeverReportedAsStillBeingSetUp(t *testing.T) {
	raw := "Connection test failed: Failed to start server: connector acme@v1.0.0 could not be deployed: " + testBuildFailure
	got, ok := FriendlyTestConnectionError("Acme", raw)
	if IsConnectorDeployingMessage(got) {
		t.Fatalf("a build failure was reported as still being set up: %q", got)
	}
	want := "The Acme " + testBuildFailure
	if !ok || got != want {
		t.Errorf("FriendlyTestConnectionError = (%q, %v)\nwant (%q, true)", got, ok, want)
	}
	if !IsConnectorBuildFailedMessage(got) {
		t.Errorf("the mapped message lost the build-failed marker: %q", got)
	}

	// Control: an import error with no build failure is still the stdio fallback's.
	if got, _ := FriendlyTestConnectionError("Acme", "No module named 'acme'"); !IsConnectorDeployingMessage(got) {
		t.Errorf("a plain import error is no longer mapped to still being set up: %q", got)
	}
}

func TestStartServerReportsABuildFailureToEveryCaller(t *testing.T) {
	const id = "zzfakedeployconn"
	tools := writeFakeConnector(t, id, "Fake DB")
	srv := failedToolGenerator(t, testBuildFailure)
	t.Setenv("TOOL_GENERATOR_URL", srv.URL)

	for _, tc := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"require HTTP (CDC)", ServerConfig{Name: id, Version: "latest", RequireHTTP: true, DeployWaitTimeout: time.Second}},
		{"no stdio while deploying (Test Connection)", ServerConfig{Name: id, Version: "latest", NoStdioWhileDeploying: true, DeployWaitTimeout: time.Second}},
		// A batch caller can use stdio when no container can be had, but an image
		// that does not build means the connector's own dependencies do not install:
		// stdio would run it without them.
		{"batch", ServerConfig{Name: id, Version: "latest", DeployWaitTimeout: time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServerManager(tools).StartServer(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), testBuildFailure) {
				t.Fatalf("expected the build failure, got %v", err)
			}
			if IsConnectorDeploying(err) {
				t.Errorf("a failed build was reported as still deploying: %v", err)
			}
		})
	}
}

// buildingThenFailedToolGenerator answers "building" to the first deploy call and the
// build failure to every later one, the way tool-generator answers a build it is
// running and then a build that failed.
func buildingThenFailedToolGenerator(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = w.Write([]byte(`{"success":true,"building":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"building":false,"error_stage":"build","error_message":` +
			`"` + strings.ReplaceAll(strings.ReplaceAll(testBuildFailure, `"`, `\"`), "\n", `\n`) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestStartServerLearnsOfABuildThatFailsWhileItWaits(t *testing.T) {
	const id = "zzfakedeployconn"
	tools := writeFakeConnector(t, id, "Fake DB")

	old := deployRecheckInterval
	deployRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() { deployRecheckInterval = old })

	for _, tc := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"no stdio while deploying (Test Connection)", ServerConfig{Name: id, Version: "latest", NoStdioWhileDeploying: true, DeployWaitTimeout: 3 * time.Second}},
		// A building batch caller waits at least five minutes for the container.
		{"batch", ServerConfig{Name: id, Version: "latest", DeployWaitTimeout: 3 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := buildingThenFailedToolGenerator(t)
			t.Setenv("TOOL_GENERATOR_URL", srv.URL)

			done := make(chan error, 1)
			go func() {
				_, err := NewServerManager(tools).StartServer(tc.cfg)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), testBuildFailure) {
					t.Fatalf("expected the build failure, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("still waiting for a container after the build failed (deploy calls: %d)", atomic.LoadInt32(calls))
			}
		})
	}
}
