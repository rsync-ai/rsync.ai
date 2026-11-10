package main

import (
	"context"
	"database/sql"

	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/backend-orchestrator/internal/handlers"
	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

// startSinkWorkerReaper starts the sink-worker reaper (KI-CDC-SINK-WORKER-NO-REAPER).
// It stops the workers of deleted pipelines, of pipelines 'stopped' past a grace
// period, and of finished batch runs, and re-runs a deleted pipeline's topic cleanup
// once its last worker is gone (KI-CDC-ORPHAN-TOPICS-RECREATED). On unless
// SINK_WORKER_REAPER_MODE is off; dry_run only logs. It lives in its own file so
// main.go line numbers cited across the docs do not shift. The goroutine runs for the
// life of the process.
func startSinkWorkerReaper(db *sql.DB, mcpManager *mcp.ServerManager, tm *kafka.TopologyManager) {
	reaper := handlers.NewSinkWorkerReaperFromEnv(db, mcpManager).
		WithKafkaCleanup(handlers.PipelineKafkaCleanup(db, tm))
	if reaper == nil {
		return
	}
	go reaper.Run(context.Background())
	log.WithField("mode", reaper.Mode()).Info("✅ Orphan sink-worker reaper started")
}
