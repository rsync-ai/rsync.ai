package validators

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Document-mode aggregation pipeline validation: a pipeline preview in the Data
// Explorer and a MongoDB model, which writes a pipeline's result to a collection.
//
// The api-gateway validates a pipeline BEFORE any network hop; the MongoDB connector
// enforces the same rules again so it is safe when called directly. The two copies
// MUST stay in lockstep (TestDocumentAggregateListsMatchConnector parses the Python
// sets out of connector.py):
//
//	shared/mcp-connectors/public/database/mongodb/versions/<current>/connector.py
//	  AGGREGATE_STAGES · AGGREGATE_DENIED_OPERATORS · AGGREGATE_MAX_*
//
// Stages are an allowlist. $out and $merge are refused because the only write a model
// makes is the one the connector appends, to the model's own target. Expression
// operators are too many to allowlist, so the walk instead refuses, at ANY depth, the
// ones that run server-side JavaScript and the two write stages — none of them can
// hide inside $facet, $lookup.pipeline or $unionWith.pipeline.
//
// Error messages name paths and stages, never a pipeline VALUE.

// DocumentAggregateStages mirrors connector.py AGGREGATE_STAGES.
var DocumentAggregateStages = map[string]bool{
	"$match": true, "$project": true, "$addFields": true, "$set": true, "$unset": true, "$group": true, "$sort": true,
	"$limit": true, "$skip": true, "$unwind": true, "$count": true, "$lookup": true, "$unionWith": true,
	"$graphLookup": true, "$replaceRoot": true, "$replaceWith": true, "$sortByCount": true, "$bucket": true,
	"$bucketAuto": true, "$facet": true, "$sample": true, "$setWindowFields": true, "$densify": true, "$fill": true,
	"$redact": true,
}

// DocumentAggregateDeniedOperators mirrors connector.py AGGREGATE_DENIED_OPERATORS.
var DocumentAggregateDeniedOperators = map[string]bool{
	"$function": true, "$accumulator": true, "$where": true, "$out": true, "$merge": true,
}

// Limits, mirroring connector.py AGGREGATE_MAX_*. A preview's page size follows find's.
const (
	DocumentAggregateMaxStages        = 50
	DocumentAggregateMaxDepth         = 32
	DocumentAggregateMaxPipelineBytes = 128 * 1024
)

// DocumentAggregateRequest is the raw, user-supplied part of a pipeline preview or a
// model. Pipeline stays raw JSON: key order is meaning in a $sort or a compound
// $group _id, and a 64-bit integer must reach the connector as written.
type DocumentAggregateRequest struct {
	Database   string
	Collection string
	Pipeline   json.RawMessage
	Limit      int
}

// DocumentAggregateSpec is a validated pipeline, ready to forward. Pipeline is the
// request's own bytes, compacted, never re-encoded (which would sort keys).
type DocumentAggregateSpec struct {
	Database   string          `json:"database,omitempty"`
	Collection string          `json:"collection"`
	Pipeline   json.RawMessage `json:"pipeline"`
	Limit      int             `json:"limit,omitempty"`
}

// ValidateDocumentAggregate checks a pipeline against the stage allowlist, the denied
// operators and the limits, and returns the normalized spec or a *DocumentFindError
// carrying the connector's error_code vocabulary.
func ValidateDocumentAggregate(req DocumentAggregateRequest) (*DocumentAggregateSpec, *DocumentFindError) {
	database, ferr := validateFindDatabase(req.Database)
	if ferr != nil {
		return nil, ferr
	}
	collection, ferr := validateFindCollection(req.Collection)
	if ferr != nil {
		return nil, ferr
	}
	pipeline, ferr := validateAggregatePipeline(req.Pipeline)
	if ferr != nil {
		return nil, ferr
	}

	limit := req.Limit
	if limit <= 0 {
		limit = DocumentFindDefaultLimit
	}
	if limit > DocumentFindMaxLimit {
		limit = DocumentFindMaxLimit
	}
	return &DocumentAggregateSpec{Database: database, Collection: collection, Pipeline: pipeline, Limit: limit}, nil
}

func validateAggregatePipeline(raw json.RawMessage) (json.RawMessage, *DocumentFindError) {
	if isJSONNull(raw) {
		return nil, findErr("invalid_pipeline", "pipeline", "Missing 'pipeline' parameter")
	}
	if len(raw) > 4*DocumentAggregateMaxPipelineBytes {
		// Refuse before decoding something absurd; the exact check is below.
		return nil, findErr("pipeline_too_large", "pipeline", "pipeline exceeds %d KB", DocumentAggregateMaxPipelineBytes/1024)
	}
	v, err := decodeFindJSON(raw)
	if err != nil {
		return nil, findErr("invalid_pipeline", "pipeline", "a pipeline must be a JSON array of stages")
	}
	if ferr := walkAggregateStages(v, "pipeline", 1); ferr != nil {
		return nil, ferr
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, findErr("invalid_pipeline", "pipeline", "pipeline must be plain JSON")
	}
	// As with a find filter, the connector's json.dumps writes \u escapes where this
	// counts UTF-8, so a non-ASCII pipeline near the limit can pass here and still be
	// refused there.
	if compact.Len() > DocumentAggregateMaxPipelineBytes {
		return nil, findErr("pipeline_too_large", "pipeline", "pipeline exceeds %d KB", DocumentAggregateMaxPipelineBytes/1024)
	}
	return compact.Bytes(), nil
}

func checkAggregateKey(key, path string) *DocumentFindError {
	if strings.ContainsRune(key, 0) {
		return findErr("invalid_pipeline", path, "field names must be strings without NUL")
	}
	return nil
}

func walkAggregateStages(stages interface{}, path string, depth int) *DocumentFindError {
	if depth > DocumentAggregateMaxDepth {
		return findErr("pipeline_too_deep", path, "pipeline nesting exceeds %d levels", DocumentAggregateMaxDepth)
	}
	list, ok := stages.([]interface{})
	if !ok {
		return findErr("invalid_pipeline", path, "a pipeline must be a JSON array of stages")
	}
	if len(list) > DocumentAggregateMaxStages {
		return findErr("pipeline_too_long", path, "a pipeline accepts at most %d stages", DocumentAggregateMaxStages)
	}
	for i, stage := range list {
		spath := fmt.Sprintf("%s[%d]", path, i)
		obj, ok := stage.(map[string]interface{})
		if !ok || len(obj) != 1 {
			return findErr("invalid_pipeline", spath, "each stage must be an object with exactly one key")
		}
		var name string
		var body interface{}
		for k, v := range obj {
			name, body = k, v
		}
		if ferr := checkAggregateKey(name, spath); ferr != nil {
			return ferr
		}
		bpath := findPath(spath, name)
		if !DocumentAggregateStages[name] {
			code := "stage_not_allowed"
			if DocumentAggregateDeniedOperators[name] {
				code = "operator_not_allowed"
			}
			return findErr(code, bpath, "stage %s is not allowed", truncateRunes(name, documentFindMaxOperatorEchoRune))
		}
		var nested map[string]interface{}
		switch name {
		case "$lookup", "$graphLookup", "$unionWith":
			var ferr *DocumentFindError
			if body, nested, ferr = splitForeignStage(name, body, bpath); ferr != nil {
				return ferr
			}
		case "$facet":
			facets, ok := body.(map[string]interface{})
			if !ok || len(facets) == 0 {
				return findErr("invalid_pipeline", bpath, "$facet must map names to pipelines")
			}
			body, nested = nil, facets
		}
		if body != nil {
			if ferr := walkAggregateNode(body, bpath, depth+1); ferr != nil {
				return ferr
			}
		}
		for key, sub := range nested {
			if ferr := checkAggregateKey(key, bpath); ferr != nil {
				return ferr
			}
			if strings.HasPrefix(key, "$") {
				return findErr("invalid_pipeline", bpath, "a $facet output name cannot start with '$'")
			}
			if ferr := walkAggregateStages(sub, findPath(bpath, key), depth+2); ferr != nil {
				return ferr
			}
		}
	}
	return nil
}

// walkAggregateNode checks depth, the Extended JSON single-key rule and the denied
// operators inside a stage's body.
func walkAggregateNode(node interface{}, path string, depth int) *DocumentFindError {
	switch n := node.(type) {
	case []interface{}:
		if depth > DocumentAggregateMaxDepth {
			return findErr("pipeline_too_deep", path, "pipeline nesting exceeds %d levels", DocumentAggregateMaxDepth)
		}
		for i, item := range n {
			if ferr := walkAggregateNode(item, fmt.Sprintf("%s[%d]", path, i), depth+1); ferr != nil {
				return ferr
			}
		}
	case map[string]interface{}:
		if depth > DocumentAggregateMaxDepth {
			return findErr("pipeline_too_deep", path, "pipeline nesting exceeds %d levels", DocumentAggregateMaxDepth)
		}
		for key := range n {
			if DocumentFindExtJSONWrappers[key] {
				if len(n) != 1 {
					return findErr("invalid_pipeline", findPath(path, key), "%s must be the only key in its object", key)
				}
				return nil // the wrapped value's shape is checked by the connector when decoded
			}
		}
		for key, value := range n {
			if ferr := checkAggregateKey(key, path); ferr != nil {
				return ferr
			}
			child := findPath(path, key)
			if DocumentAggregateDeniedOperators[key] {
				return findErr("operator_not_allowed", child, "operator %s is not allowed", key)
			}
			if ferr := walkAggregateNode(value, child, depth+1); ferr != nil {
				return ferr
			}
		}
	}
	return nil
}

// splitForeignStage checks the collection a $lookup / $graphLookup / $unionWith reads
// and splits off its sub-pipeline, so that is walked as stages rather than values. It
// returns the rest of the body and {"pipeline": sub-pipeline} (or nil).
func splitForeignStage(name string, body interface{}, path string) (interface{}, map[string]interface{}, *DocumentFindError) {
	if s, ok := body.(string); ok && name == "$unionWith" {
		return nil, nil, validateAggregateCollection(s, path)
	}
	obj, ok := body.(map[string]interface{})
	if !ok {
		return nil, nil, findErr("invalid_pipeline", path, "%s must be an object", name)
	}
	sourceKey := "from"
	if name == "$unionWith" {
		sourceKey = "coll"
	}
	if ferr := validateAggregateCollection(obj[sourceKey], findPath(path, sourceKey)); ferr != nil {
		return nil, nil, ferr
	}
	sub, has := obj["pipeline"]
	if name == "$graphLookup" || !has {
		return obj, nil, nil
	}
	rest := make(map[string]interface{}, len(obj)-1)
	for k, v := range obj {
		if k != "pipeline" {
			rest[k] = v
		}
	}
	return rest, map[string]interface{}{"pipeline": sub}, nil
}

// validateAggregateCollection checks a collection a stage reads from. A plain name is
// always a collection in the pipeline's own database, so find's rules apply.
func validateAggregateCollection(raw interface{}, path string) *DocumentFindError {
	s, ok := raw.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return findErr("invalid_collection", path, "must name a collection in this database")
	}
	if _, ferr := validateFindCollection(s); ferr != nil {
		return &DocumentFindError{Code: ferr.Code, Path: path, Message: ferr.Message}
	}
	return nil
}

// DocumentAggregateSources is every collection a validated pipeline reads: the one it
// runs on, then each $lookup / $graphLookup `from` and $unionWith `coll` (or string
// body), at any depth — inside $lookup.pipeline, $unionWith.pipeline and every $facet
// branch. It is what lineage knows a MongoDB model's inputs by; the SQL extractor finds
// nothing in a pipeline.
//
// All of them are plain names in the pipeline's own database (validateAggregateCollection),
// so the caller qualifies them with spec.Database. Each is listed once, in order of first
// appearance, with $facet branches taken in name order so the answer is stable.
//
// It walks the same stage shapes walkAggregateStages accepts and expects a spec that
// passed it; a nil spec reads nothing.
func DocumentAggregateSources(spec *DocumentAggregateSpec) []string {
	if spec == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw interface{}) {
		name, _ := raw.(string)
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	add(spec.Collection)
	if stages, err := decodeFindJSON(spec.Pipeline); err == nil {
		collectAggregateSources(stages, add)
	}
	return out
}

func collectAggregateSources(stages interface{}, add func(interface{})) {
	list, _ := stages.([]interface{})
	for _, stage := range list {
		obj, _ := stage.(map[string]interface{})
		for name, body := range obj {
			switch name {
			case "$lookup", "$graphLookup", "$unionWith":
				if name == "$unionWith" {
					if s, ok := body.(string); ok {
						add(s)
						continue
					}
				}
				foreign, _ := body.(map[string]interface{})
				sourceKey := "from"
				if name == "$unionWith" {
					sourceKey = "coll"
				}
				add(foreign[sourceKey])
				if name != "$graphLookup" {
					collectAggregateSources(foreign["pipeline"], add)
				}
			case "$facet":
				facets, _ := body.(map[string]interface{})
				keys := make([]string, 0, len(facets))
				for k := range facets {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					collectAggregateSources(facets[k], add)
				}
			}
		}
	}
}
