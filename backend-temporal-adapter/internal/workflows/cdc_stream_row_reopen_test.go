package workflows

import (
	"strings"
	"testing"
)

// After Stop or Cancel then Resume, a CDC pipeline streamed again while its
// stream row (id = pipeline id, the sink's long-lived anchor) still read
// 'cancelled': Stop and Cancel close it, and the sink only inserts it when it is
// missing. A run reaching the streaming handoff now reopens it. These tests use
// the recording driver to pin when the handoff asks for the reopen. What the
// reopen itself changes is proven against PostgreSQL in
// cdc_stream_row_reopen_pg_test.go.

const reopenPrefix = "UPDATE executions SET status = 'running'"

func TestStreamingHandoff_ReopensTheCDCStreamRow(t *testing.T) {
	state := &recordedDB{}
	withRecordedDB(t, state)

	runStatusActivity(t, "streaming_active")

	args, ok := state.argsOf(reopenPrefix)
	if !ok {
		t.Fatalf("the streaming handoff never reopened the stream row; a resumed "+
			"pipeline streams under a row that reads cancelled:\n%s", state.all())
	}
	if len(args) != 1 || args[0] != supPipelineID {
		t.Fatalf("reopen args = %v, want only the pipeline id %s (the stream row's id)", args, supPipelineID)
	}
	closeAt, reopenAt := -1, -1
	for i, s := range strings.Split(state.all(), "\n") {
		switch {
		case strings.HasPrefix(s, "UPDATE executions SET status = 'completed'") && closeAt < 0:
			closeAt = i
		case strings.HasPrefix(s, reopenPrefix) && reopenAt < 0:
			reopenAt = i
		}
	}
	if closeAt < 0 || reopenAt < closeAt {
		t.Fatalf("the reopen must follow the close of the run's own row (close at %d, reopen at %d):\n%s",
			closeAt, reopenAt, state.all())
	}
}

func TestStreamingHandoff_AStopFirstLeavesTheStreamRowClosed(t *testing.T) {
	state := &recordedDB{runClosedFirst: true}
	withRecordedDB(t, state)

	runStatusActivity(t, "streaming_active")

	if state.ran(reopenPrefix) {
		t.Fatalf("a run whose row a Stop or Cancel closed first reopened the stream row:\n%s", state.all())
	}
}

// Only the streaming handoff reopens: a batch run's terminal write never touches
// the stream row.
func TestTerminalWrites_NeverReopenTheStreamRow(t *testing.T) {
	for _, status := range []string{"failed", "stopped"} {
		t.Run(status, func(t *testing.T) {
			state := &recordedDB{}
			withRecordedDB(t, state)

			runStatusActivity(t, status)

			if state.ran(reopenPrefix) {
				t.Fatalf("a %s run reopened the stream row:\n%s", status, state.all())
			}
		})
	}
}
