package mcp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ConnectorDeployingMarker is a stable phrase present in every "still being set up"
// message. The api-gateway and the frontend key off it to show the result as a
// retryable wait rather than a credential/connectivity failure — keep it in lockstep
// with api-gateway handlers.isConnectorDeployingMessage and the frontend's
// isConnectorDeployingMessage (src/lib/errors/connector-deploying.ts).
const ConnectorDeployingMarker = "connector is still being set up"

// ConnectorDeployingMessage is the user-facing text for a connector whose container
// is still being built or started. It names no transport, interpreter or module: the
// only thing the user can do is wait and retry.
func ConnectorDeployingMessage(displayName string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = "This"
	}
	return fmt.Sprintf("The %s %s for first use — this can take a minute or two. Please try again shortly.",
		displayName, ConnectorDeployingMarker)
}

// ConnectorDeployingError is returned by StartServer (for callers that set
// NoStdioWhileDeploying) when a container deploy was requested but the container is
// not reachable yet. It is retryable by definition.
type ConnectorDeployingError struct {
	Connector   string
	Version     string
	DisplayName string
}

func (e *ConnectorDeployingError) Error() string {
	return ConnectorDeployingMessage(e.DisplayName)
}

// IsConnectorDeploying reports whether err is (or wraps) a *ConnectorDeployingError.
func IsConnectorDeploying(err error) bool {
	var de *ConnectorDeployingError
	return errors.As(err, &de)
}

// IsConnectorDeployingMessage reports whether a flattened error string carries the
// "still being set up" message (errors cross HTTP/JSON boundaries as strings).
func IsConnectorDeployingMessage(msg string) bool {
	return strings.Contains(msg, ConnectorDeployingMarker)
}

// ConnectorBuildFailedMarker starts tool-generator's answer for a connector image that
// failed to build (deployment/routes.py BUILD_FAILED_PREFIX), which carries the end of
// the build log. A build failure is not a wait: the same files fail the same way, so
// it is shown with its cause and never as "still being set up". Keep it in lockstep
// with api-gateway handlers.connectorBuildFailedMarker and the frontend's
// CONNECTOR_BUILD_FAILED_MARKER (src/lib/errors/connector-deploying.ts).
const ConnectorBuildFailedMarker = "connector image failed to build"

// IsConnectorBuildFailedMessage reports whether a flattened error string carries a
// failed image build.
func IsConnectorBuildFailedMessage(msg string) bool {
	return strings.Contains(msg, ConnectorBuildFailedMarker)
}

// connectorBuildFailedMessage is the user-facing text for a failed build: the
// connector's name, then tool-generator's answer from the marker on, without the
// "Connection test failed: Failed to start server: … could not be deployed:" chain.
func connectorBuildFailedMessage(displayName, errMsg string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = "This"
	}
	return fmt.Sprintf("The %s %s", displayName, errMsg[strings.Index(errMsg, ConnectorBuildFailedMarker):])
}

// missingModuleRe matches a Python import failure for a third-party dependency:
// "No module named 'pymongo'" / "ModuleNotFoundError: No module named pymongo.errors".
var missingModuleRe = regexp.MustCompile(`(?i)(ModuleNotFoundError|No module named\s)`)

// IsMissingModuleError reports whether msg is a Python "No module named" failure.
func IsMissingModuleError(msg string) bool {
	return missingModuleRe.MatchString(msg)
}

// FriendlyTestConnectionError maps a failed connection test's raw error to the
// user-facing "still being set up" message when the failure is a symptom of the
// connector's container not being ready: either StartServer said so directly, or
// the request reached a stdio fallback whose interpreter lacks the connector's
// dependencies ("No module named 'X'"). A connector running in its own image always
// has its dependencies, so an import error on a test is never the user's to fix.
// A failed image build is checked first: its build log can name a missing module,
// and it is the one failure here that waiting does not fix.
// Returns (mapped, true) when mapped; (errMsg, false) otherwise.
func FriendlyTestConnectionError(displayName, errMsg string) (string, bool) {
	if IsConnectorBuildFailedMessage(errMsg) {
		return connectorBuildFailedMessage(displayName, errMsg), true
	}
	if IsConnectorDeployingMessage(errMsg) || IsMissingModuleError(errMsg) {
		return ConnectorDeployingMessage(displayName), true
	}
	return errMsg, false
}
