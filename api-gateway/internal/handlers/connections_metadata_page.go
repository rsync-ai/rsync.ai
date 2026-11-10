package handlers

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// metadataQuery is the filter and page a /connections/:id/metadata caller asked for.
type metadataQuery struct {
	Search         string
	SortBy         string
	IncludeColumns bool
	Tables         string // comma-separated exact names, bare or schema-qualified
	Limit          int
	Offset         int
}

func parseMetadataQuery(c *gin.Context) metadataQuery {
	q := metadataQuery{
		Search:         c.Query("search"),
		SortBy:         c.DefaultQuery("sort_by", "name"), // name, size, relevance
		IncludeColumns: c.DefaultQuery("include_columns", "true") == "true",
		Tables:         c.Query("tables"),
		Limit:          25,
	}
	if v, err := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("limit", "25"))); err == nil && v > 0 {
		q.Limit = v
	}
	if q.Limit > maxMetadataPageSize {
		q.Limit = maxMetadataPageSize
	}
	if v, err := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("offset", "0"))); err == nil && v >= 0 {
		q.Offset = v
	}
	return q
}

// metadataPage answers q from the full discovered table list: the named tables,
// then search, sort and columns, then the page. total counts matches before paging.
func metadataPage(tables []TableMetadata, q metadataQuery) (page []TableMetadata, total, offset int) {
	if q.Tables != "" {
		tables = onlyRequestedTables(tables, q.Tables)
	}
	filtered := filterAndSortTables(tables, q.Search, q.SortBy, q.IncludeColumns)
	total = len(filtered)
	offset = q.Offset
	if offset > total {
		offset = total
	}
	end := offset + q.Limit
	if end > total {
		end = total
	}
	return filtered[offset:end], total, offset
}

// onlyRequestedTables keeps the tables named in a comma-separated list. A name
// matches bare or schema-qualified, case-insensitively; a qualified request also
// matches its bare table name.
func onlyRequestedTables(tables []TableMetadata, list string) []TableMetadata {
	requested := make(map[string]bool)
	for _, t := range strings.Split(list, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		requested[t] = true
		if parts := strings.Split(t, "."); len(parts) > 1 {
			requested[parts[len(parts)-1]] = true
		}
	}
	out := make([]TableMetadata, 0)
	for _, table := range tables {
		name := strings.ToLower(table.Name)
		qualified := name
		if table.Schema != "" {
			qualified = strings.ToLower(table.Schema) + "." + name
		}
		if requested[name] || requested[qualified] {
			out = append(out, table)
		}
	}
	return out
}

// cachedMetadataResponse answers q from a cached discovery. The cache holds every
// table, so serving it as-is ignored ?tables=, search, limit, offset and
// include_columns: the AI suggestions dialog for a pipeline that selected
// public.subscriptions offered a transform on users.email (prod a6ca2103). ok is
// false when the cached payload cannot be read, so the caller rediscovers.
func cachedMetadataResponse(cached []byte, q metadataQuery) (body []byte, ok bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(cached, &envelope); err != nil {
		return nil, false
	}
	var tables []TableMetadata
	if raw, present := envelope["tables"]; present {
		if err := json.Unmarshal(raw, &tables); err != nil {
			return nil, false
		}
	}
	// Older entries can hold negative counts from upstream connectors (MySQL TABLE_ROWS).
	for i := range tables {
		if tables[i].RowCount < 0 {
			tables[i].RowCount = 0
			tables[i].IsExactCount = false
		}
	}
	page, total, offset := metadataPage(tables, q)
	out := make(map[string]interface{}, len(envelope)+3)
	for k, v := range envelope {
		out[k] = v
	}
	out["tables"] = page
	out["total"] = total
	out["limit"] = q.Limit
	out["offset"] = offset
	body, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return body, true
}
