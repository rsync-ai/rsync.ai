package handlers

import (
	"context"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The removed-topic reaper held its jobs only in memory: an orchestrator restart
// while a job waited (for the old connector task to restart, or the sink to apply
// the topic) dropped the job, and the removed table's topic stayed for good. These
// tests pin the class: a job that has not ended outlives the process that started it,
// and one that has ended does not run again.

// memReapStore is the cdc_removed_topic_jobs table.
type memReapStore struct {
	mu   sync.Mutex
	rows map[string]removedTopicJob
}

func (s *memReapStore) Save(_ context.Context, j removedTopicJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[j.key()] = j
	return nil
}

func (s *memReapStore) Delete(_ context.Context, pipelineID, topic string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, pipelineID+"|"+topic)
	return nil
}

func (s *memReapStore) List(context.Context) ([]removedTopicJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []removedTopicJob
	for _, j := range s.rows {
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (s *memReapStore) topics() []string {
	jobs, _ := s.List(context.Background())
	var topics []string
	for _, j := range jobs {
		topics = append(topics, j.Topic)
	}
	return topics
}

// restart is a new orchestrator over the same Kafka, connector and table.
func (h *reapHarness) restart(t *testing.T, deadline time.Duration) {
	t.Helper()
	h.r = newReapedHarnessReaper(h, deadline)
	t.Cleanup(h.r.Stop)
	h.r.Resume()
}

func TestReaperJobOutlivesARestart(t *testing.T) {
	h := newReapHarness(t, time.Minute)
	h.admin.readers = []string{"sink-600b012e-stream"} // the old sink still reads it: wait
	h.r.Schedule(reapPipeline, reapConnector, connectorWithout(), []string{reapTable})
	time.Sleep(20 * time.Millisecond) // several polls, all waiting
	h.r.Stop()                        // shutdown, mid-wait
	if got := h.admin.deletedTopics(); len(got) != 0 {
		t.Fatalf("deleted %v while the sink still read it", got)
	}
	if got := h.store.topics(); !reflect.DeepEqual(got, []string{reapTopic}) {
		t.Fatalf("stored jobs = %v after shutdown; want the unfinished one [%s]", got, reapTopic)
	}

	h.admin.mu.Lock()
	h.admin.readers = nil // the sink restarted without the topic
	h.admin.mu.Unlock()
	h.restart(t, time.Minute)
	waitForJobs(t, h.r)
	if got := h.admin.deletedTopics(); !reflect.DeepEqual(got, []string{reapTopic}) {
		t.Fatalf("deleted = %v after the restart, want [%s]", got, reapTopic)
	}
	if got := h.store.topics(); len(got) != 0 {
		t.Fatalf("stored jobs = %v after the job ended, want none", got)
	}
}

// Every way a job ends clears it, so a restart does not run it again; and a restart
// does not give a job a new deadline.
func TestReaperEndedJobIsForgotten(t *testing.T) {
	t.Run("kept: the table was added back", func(t *testing.T) {
		h := newReapHarness(t, time.Minute)
		h.connect.configs = []map[string]interface{}{connectorWith()}
		h.r.Schedule(reapPipeline, reapConnector, connectorWithout(), []string{reapTable})
		waitForJobs(t, h.r)
		if got := h.store.topics(); len(got) != 0 {
			t.Fatalf("stored jobs = %v, want none", got)
		}
	})
	t.Run("kept: the deadline passed", func(t *testing.T) {
		h := newReapHarness(t, 20*time.Millisecond)
		h.admin.readers = []string{"someone"}
		h.r.Schedule(reapPipeline, reapConnector, connectorWithout(), []string{reapTable})
		waitForJobs(t, h.r)
		if got := h.store.topics(); len(got) != 0 {
			t.Fatalf("stored jobs = %v, want none", got)
		}
	})
	t.Run("a resumed job keeps its deadline", func(t *testing.T) {
		h := newReapHarness(t, time.Minute)
		h.admin.readers = []string{"someone"}
		_ = h.store.Save(context.Background(), removedTopicJob{PipelineID: reapPipeline, Connector: reapConnector,
			Table: reapTable, Topic: reapTopic, ScheduledAt: time.Now().Add(-2 * time.Minute)})
		h.restart(t, time.Minute)
		waitForJobs(t, h.r) // fails if the restart restarted the clock
		if got := h.admin.deletedTopics(); len(got) != 0 {
			t.Fatalf("deleted %v while it was still read", got)
		}
		if got := h.store.topics(); len(got) != 0 {
			t.Fatalf("stored jobs = %v, want none", got)
		}
	})
}

// The statements sqlRemovedTopicJobs sends, against migration 148's columns.
func TestSQLRemovedTopicJobs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store := sqlRemovedTopicJobs{db: db}
	ctx := context.Background()
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	job := removedTopicJob{PipelineID: reapPipeline, Connector: reapConnector, Table: reapTable, Topic: reapTopic, ScheduledAt: at}

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO cdc_removed_topic_jobs (pipeline_id, topic, connector_name, table_name, scheduled_at)`)).
		WithArgs(reapPipeline, reapTopic, reapConnector, reapTable, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pipeline_id::text, topic, connector_name, table_name, scheduled_at`)).
		WillReturnRows(sqlmock.NewRows([]string{"pipeline_id", "topic", "connector_name", "table_name", "scheduled_at"}).
			AddRow(reapPipeline, reapTopic, reapConnector, reapTable, at))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM cdc_removed_topic_jobs WHERE pipeline_id = $1 AND topic = $2`)).
		WithArgs(reapPipeline, reapTopic).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Save(ctx, job); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, err := store.List(ctx); err != nil || !reflect.DeepEqual(got, []removedTopicJob{job}) {
		t.Fatalf("List = %+v, %v; want [%+v]", got, err, job)
	}
	if err := store.Delete(ctx, reapPipeline, reapTopic); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
