package main

// discoverSchemaRequest is the /agent/discover-schema body.
type discoverSchemaRequest struct {
	Task          string                 `json:"task"`
	ConnectionID  string                 `json:"connection_id"`
	ConnectorType string                 `json:"connector_type"`
	Config        map[string]interface{} `json:"config"`
	UserID        string                 `json:"user_id"`
	// Optional v2 flags (backward compatible; older callers can omit)
	IncludeColumns       *bool `json:"include_columns,omitempty"`
	IncludeRowCounts     *bool `json:"include_row_counts,omitempty"`
	IncludeRelationships *bool `json:"include_relationships,omitempty"`
	IncludeIndexes       *bool `json:"include_indexes,omitempty"`
	// IncludeTableDetails asks a warehouse connector (BigQuery) for per-table
	// columns and row counts, one metadata read per table. Only the Explorer's
	// schema panel sets it; pipeline discovery never does.
	IncludeTableDetails *bool `json:"include_table_details,omitempty"`
	MaxTables           *int  `json:"max_tables,omitempty"`
}

// params returns the flags the caller set, for DiscoverSchemaEnvelope to merge
// over its defaults. An unset flag is left out so the default applies.
func (r discoverSchemaRequest) params() map[string]interface{} {
	params := map[string]interface{}{}
	if r.IncludeColumns != nil {
		params["include_columns"] = *r.IncludeColumns
	}
	if r.IncludeRowCounts != nil {
		params["include_row_counts"] = *r.IncludeRowCounts
	}
	if r.IncludeRelationships != nil {
		params["include_relationships"] = *r.IncludeRelationships
	}
	if r.IncludeIndexes != nil {
		params["include_indexes"] = *r.IncludeIndexes
	}
	if r.IncludeTableDetails != nil {
		params["include_table_details"] = *r.IncludeTableDetails
	}
	if r.MaxTables != nil {
		params["max_tables"] = *r.MaxTables
	}
	return params
}
