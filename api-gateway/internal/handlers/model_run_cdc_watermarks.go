package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"api-gateway/internal/validators"
)

// What a model run was built from (saved_query_runs.cdc_watermarks, migration 137).
//
// A batch upstream's run says how far it got; a CDC pipeline never finishes a run, so
// a model built from one recorded nothing about the data it read. As the run starts,
// it reads the watermark the sink keeps for each CDC table its SQL reads
// (cdc_table_watermarks) and records them with the run, keyed by destination table:
//
//	{"analytics.orders": {"pipeline_id": "…", "source_time": "2026-10-03T10:07:00Z"}}
//
// Read before the rebuild, so it is a lower bound: the run saw at least the rows up to
// source_time, and may have seen later ones that landed while it ran. source_time is
// null when the sink had no source time for that table.
//
// Which tables count is the one shared matching rule (matchTableReference), against the
// CDC pipelines of the model's workspace that land into the model's own connection. A
// watermark with no destination name cannot be placed in the model's warehouse and is
// left out, as the upstream suggestion leaves it out.
//
// Best-effort: a failed read records no map and never fails the run.

// cdcTableWatermark is one CDC table's watermark, as the sink records it.
type cdcTableWatermark struct {
	PipelineID  string
	Destination string // destination qualified name
	SourceTime  sql.NullTime
}

// modelRunCDCWatermark is one entry of the recorded map.
type modelRunCDCWatermark struct {
	PipelineID string     `json:"pipeline_id"`
	SourceTime *time.Time `json:"source_time"`
}

// buildModelRunCDCWatermarks keeps the watermarks of the tables refs read and renders
// them as the recorded map; nil when none matched. Two pipelines landing one table
// (a misconfiguration) keep the earlier source time, the bound the run is sure of. Pure.
func buildModelRunCDCWatermarks(refs []validators.TableRef, marks []cdcTableWatermark) json.RawMessage {
	if len(refs) == 0 || len(marks) == 0 {
		return nil
	}
	produced := make([]producedTable, 0, len(marks))
	byProducer := make(map[string]cdcTableWatermark, len(marks))
	for _, w := range marks {
		dest := strings.TrimSpace(w.Destination)
		if dest == "" {
			continue
		}
		name := dest
		if i := strings.LastIndex(dest, "."); i >= 0 {
			name = dest[i+1:]
		}
		key := w.PipelineID + "\x00" + dest
		produced = append(produced, producedTable{ProducerID: key, DestQualified: dest, TableName: name})
		byProducer[key] = w
	}

	out := map[string]modelRunCDCWatermark{}
	for _, ref := range refs {
		matches, _ := matchTableReference(ref, produced)
		for _, p := range matches {
			w := byProducer[p.ProducerID]
			entry := modelRunCDCWatermark{PipelineID: w.PipelineID}
			if w.SourceTime.Valid {
				t := w.SourceTime.Time.UTC()
				entry.SourceTime = &t
			}
			prev, seen := out[p.DestQualified]
			if seen && !earlierCDCWatermark(entry, prev) {
				continue
			}
			out[p.DestQualified] = entry
		}
	}
	if len(out) == 0 {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

// earlierCDCWatermark orders a against b: an unknown source time first, then the older,
// then the lower pipeline id, so the kept entry does not depend on row order.
func earlierCDCWatermark(a, b modelRunCDCWatermark) bool {
	switch {
	case a.SourceTime == nil && b.SourceTime != nil:
		return true
	case a.SourceTime != nil && b.SourceTime == nil:
		return false
	case a.SourceTime != nil && !a.SourceTime.Equal(*b.SourceTime):
		return a.SourceTime.Before(*b.SourceTime)
	}
	return a.PipelineID < b.PipelineID
}

// modelRunCDCWatermarks reads the watermarks of the CDC tables m reads, for the run
// about to start. nil when it reads none, or when they cannot be read.
func modelRunCDCWatermarks(ctx context.Context, database *sql.DB, m *savedQueryModel) json.RawMessage {
	if database == nil || m == nil {
		return nil
	}
	refs := modelReadReferences(m.ConnectorType, m.SQLText)
	if len(refs) == 0 {
		return nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT w.pipeline_id::text, w.destination_qualified_name, w.source_time
		FROM cdc_table_watermarks w
		JOIN pipelines p ON p.id = w.pipeline_id AND `+pipelineRowIsCDCSQL+`
		WHERE p.workspace_id = $1::uuid
		  AND p.destination_connection_id = $2::uuid
		  AND w.destination_qualified_name IS NOT NULL`,
		m.WorkspaceID, m.ConnectionID)
	if err != nil {
		if !isUndefinedTable(err) {
			log.WithError(err).WithField("model_id", m.ID).Warn("model run: could not read CDC watermarks; the run records none")
		}
		return nil
	}
	defer rows.Close()
	var marks []cdcTableWatermark
	for rows.Next() {
		var w cdcTableWatermark
		if err := rows.Scan(&w.PipelineID, &w.Destination, &w.SourceTime); err != nil {
			log.WithError(err).WithField("model_id", m.ID).Warn("model run: could not read CDC watermarks; the run records none")
			return nil
		}
		marks = append(marks, w)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).WithField("model_id", m.ID).Warn("model run: could not read CDC watermarks; the run records none")
		return nil
	}
	sort.Slice(marks, func(i, j int) bool {
		if marks[i].Destination != marks[j].Destination {
			return marks[i].Destination < marks[j].Destination
		}
		return marks[i].PipelineID < marks[j].PipelineID
	})
	return buildModelRunCDCWatermarks(refs, marks)
}
