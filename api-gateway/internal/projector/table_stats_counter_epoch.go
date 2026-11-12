package projector

import "database/sql"

// kafka-mcp-sink keeps counts.bytes_committed and counts.dlq_rows as running
// totals in memory, so a restarted sink reports both from zero again (its row
// counts are seeded from the ack ledger; these two are not). It stamps every
// TABLE_STATS event with metadata.counter_epoch_ms, the time its process started
// them at zero, and the projector stores the epoch and the raw totals it last
// saw on the row (migration 134). Comparing a reported total with the stored
// one, as GREATEST() does, charged nothing after a restart until the new
// process passed the old total.

// counterEpochOf is the event's metadata.counter_epoch_ms, or 0 when the sink
// sent none (a sink older than migration 134, or another producer).
func counterEpochOf(meta map[string]interface{}) int64 {
	v, ok := meta["counter_epoch_ms"].(float64)
	if !ok || v <= 0 {
		return 0
	}
	return int64(v)
}

// storedEpoch is a stats row's counter_epoch_ms and the raw totals of its last
// event; all NULL on a row from before migration 134 or with no row yet.
type storedEpoch struct {
	epoch, bytesRaw, dlqRaw sql.NullInt64
}

// epochOutcome is what one event adds to the stored totals and, when record is
// set, the epoch state to write back.
type epochOutcome struct {
	bytesDelta, dlqDelta int64
	record               bool
	bytesRaw, dlqRaw     sql.NullInt64
}

// epochDeltas turns an event's epoch and raw totals into what it adds:
//   - a newer epoch (or none stored): the totals restarted at zero, so all of it;
//   - the same epoch: the increase since the last event, so a replay adds nothing;
//   - an older epoch: a replay from before the restart, nothing, and the stored
//     state is kept.
//
// A total the event did not send adds nothing and leaves its stored raw value.
func epochDeltas(stored storedEpoch, epoch int64, bytesRaw, dlqRaw sql.NullInt64) epochOutcome {
	if stored.epoch.Valid && epoch < stored.epoch.Int64 {
		return epochOutcome{}
	}
	restarted := !stored.epoch.Valid || epoch > stored.epoch.Int64
	out := epochOutcome{record: true}
	out.bytesDelta, out.bytesRaw = epochDelta(restarted, stored.bytesRaw, bytesRaw)
	out.dlqDelta, out.dlqRaw = epochDelta(restarted, stored.dlqRaw, dlqRaw)
	return out
}

func epochDelta(restarted bool, stored, raw sql.NullInt64) (int64, sql.NullInt64) {
	if !raw.Valid {
		if restarted {
			return 0, sql.NullInt64{}
		}
		return 0, stored
	}
	if restarted || !stored.Valid {
		return max(raw.Int64, 0), raw
	}
	if raw.Int64 <= stored.Int64 {
		return 0, stored
	}
	return raw.Int64 - stored.Int64, raw
}
