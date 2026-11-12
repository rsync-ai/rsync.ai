package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

// Which lake pipeline feeds which warehouse table, across connections.
//
// A lake pipeline writes Parquet files to a bucket (its destination connection). A
// BigQuery external table reads those files, and models read the external table on
// the BigQuery connection. The asset graph matches a model's reads only within the
// model's own connection, so without this the pipeline and the models never meet:
// the models read "a table we can't trace" and the pipeline "refreshes nothing".
//
// The link is read, not guessed. BigQuery says where each external table's files are
// (its source URIs, from the same tables.get the Explorer reads columns with;
// warehouse_adapters.py _bigquery_external_files). A layout v2 pipeline writes each
// table under a folder of its own, <conn prefix>/<pipeline prefix>/<table key>/
// (backend-orchestrator storage.LayoutV2TablePrefix), and its object_load_counters
// row names that table key before the first file is written, batch and CDC alike. An
// external table whose files sit in that folder is fed by that pipeline.
//
// Only layout v2 is linked. A v1 pipeline (MinIO, and any pipeline from before v2)
// shares folders between pipelines, so a folder does not say which one wrote it.

// externalTableWarehouseTypes are the warehouses whose listing reports external
// tables. BigQuery only, for now: another warehouse's connector would have to report
// table_type and source_uris first.
var externalTableWarehouseTypes = []string{"bigquery"}

func warehouseKeepsExternalTables(connectorType string) bool {
	t := strings.ToLower(strings.TrimSpace(connectorType))
	for _, w := range externalTableWarehouseTypes {
		if t == w {
			return true
		}
	}
	return false
}

const (
	// externalTablesMaxAge is how old a saved list may be before the Lineage page
	// re-reads it in the background. The saved one is drawn meanwhile.
	externalTablesMaxAge = time.Hour
	// externalTableRefreshBackoff keeps a warehouse that failed to list from being
	// asked again on every page load.
	externalTableRefreshBackoff = 10 * time.Minute
	// externalTableRefreshTimeout bounds one background listing. A listing reads up to
	// warehouseSchemaMaxTables tables, and discoverSchemaIndexTables waits 90s.
	externalTableRefreshTimeout = 3 * time.Minute
	// externalTableMaxURIs caps the source URIs kept per table. One is usual; a table
	// can name thousands of files, and the folder is all the link needs.
	externalTableMaxURIs = 100
	// lakeTableFolderLimit caps the table folders read for one workspace.
	lakeTableFolderLimit = 5000
)

// warehouseExternalTable is one external table and where its files are, as the
// warehouse reports them.
type warehouseExternalTable struct {
	Schema          string   `json:"schema"`
	Name            string   `json:"name"`
	SourceURIs      []string `json:"source_uris"`
	SourceURIPrefix string   `json:"source_uri_prefix,omitempty"`
}

// warehouseTableListing is what one warehouse listing says about external tables.
type warehouseTableListing struct {
	External []warehouseExternalTable
	// Complete says the listing named every table in its scope and read each one:
	// no warning (a dataset or table it could not read, a read that ran out of
	// time) and no cap. Only a complete listing can say a saved table is gone.
	Complete bool
	// Scope is the one dataset the listing covered, or "" when it covered every
	// dataset the connection can see. Saved entries outside it are kept.
	Scope string
}

func externalTableKey(schema, name string) string {
	return strings.ToLower(schema) + "." + strings.ToLower(name)
}

// mergeExternalTables is the list to save after a listing: every external table it
// found, plus each saved one it could not have ruled out (outside its scope, or the
// listing was not complete). A complete listing drops a saved table it did not find
// external, which is how a dropped or replaced external table leaves the graph.
func mergeExternalTables(saved []warehouseExternalTable, listing warehouseTableListing) []warehouseExternalTable {
	out := make([]warehouseExternalTable, 0, len(listing.External)+len(saved))
	kept := map[string]bool{}
	for _, t := range listing.External {
		k := externalTableKey(t.Schema, t.Name)
		if kept[k] {
			continue
		}
		kept[k] = true
		if len(t.SourceURIs) > externalTableMaxURIs {
			t.SourceURIs = t.SourceURIs[:externalTableMaxURIs]
		}
		if t.SourceURIs == nil {
			t.SourceURIs = []string{}
		}
		out = append(out, t)
	}
	for _, t := range saved {
		k := externalTableKey(t.Schema, t.Name)
		if kept[k] {
			continue
		}
		outside := listing.Scope != "" && !strings.EqualFold(t.Schema, listing.Scope)
		if outside || !listing.Complete {
			kept[k] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return externalTableKey(out[i].Schema, out[i].Name) < externalTableKey(out[j].Schema, out[j].Name)
	})
	return out
}

// saveWarehouseExternalTables merges one listing into the connection's saved list.
func saveWarehouseExternalTables(ctx context.Context, database *sql.DB, connectionID string, listing warehouseTableListing) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT tables FROM warehouse_external_tables WHERE connection_id = $1::uuid FOR UPDATE`,
		connectionID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var saved []warehouseExternalTable
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &saved); err != nil {
			// A row this build cannot read is replaced by what the warehouse says now.
			saved = nil
		}
	}
	merged, err := json.Marshal(mergeExternalTables(saved, listing))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO warehouse_external_tables (connection_id, tables, read_at)
		VALUES ($1::uuid, $2::jsonb, NOW())
		ON CONFLICT (connection_id) DO UPDATE
		SET tables = EXCLUDED.tables, read_at = EXCLUDED.read_at`,
		connectionID, string(merged)); err != nil {
		return err
	}
	return tx.Commit()
}

// lakeLocation is a folder in object storage. Store is the URI scheme BigQuery uses
// for it ("gs", "s3", "azure"); Bucket is the bucket, or "<account>/<container>" on
// Azure; Folder is the key prefix, ending in "/", or "" for the bucket's root.
type lakeLocation struct {
	Store  string
	Bucket string
	Folder string
}

func (l lakeLocation) key() string {
	return l.Store + "\x00" + l.Bucket + "\x00" + l.Folder
}

// azureBlobHost is the host of an Azure storage account's blob endpoint.
const azureBlobHost = ".blob.core.windows.net"

// externalURIFolder is the folder an external table's URI reads from: the URI cut
// before its first wildcard or {partition key}, back to the last "/". A hive prefix
// is a folder already, with or without its trailing "/". URIs in any other store
// (Google Drive, Bigtable) are not lake folders.
func externalURIFolder(uri string, isFolder bool) (lakeLocation, bool) {
	uri = strings.TrimSpace(uri)
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return lakeLocation{}, false
	}
	loc := lakeLocation{Store: strings.ToLower(scheme)}
	var key string
	switch loc.Store {
	case "gs", "s3":
		loc.Bucket, key, _ = strings.Cut(rest, "/")
	case "azure":
		host, path, _ := strings.Cut(rest, "/")
		account, isBlob := strings.CutSuffix(strings.ToLower(host), azureBlobHost)
		if !isBlob || account == "" {
			return lakeLocation{}, false
		}
		container, k, _ := strings.Cut(path, "/")
		if container == "" {
			return lakeLocation{}, false
		}
		loc.Bucket, key = account+"/"+container, k
	default:
		return lakeLocation{}, false
	}
	if loc.Bucket == "" {
		return lakeLocation{}, false
	}
	if isFolder && key != "" && !strings.HasSuffix(key, "/") {
		key += "/"
	}
	if i := strings.IndexAny(key, "*{"); i >= 0 {
		key = key[:i]
	}
	if i := strings.LastIndex(key, "/"); i >= 0 {
		key = key[:i+1]
	} else {
		key = ""
	}
	loc.Folder = key
	return loc, true
}

// externalTableFolders is every folder an external table reads. A hive table reads
// its partition prefix; any other reads the folder of each source URI.
func externalTableFolders(t warehouseExternalTable) []lakeLocation {
	if t.SourceURIPrefix != "" {
		if loc, ok := externalURIFolder(t.SourceURIPrefix, true); ok {
			return []lakeLocation{loc}
		}
	}
	var out []lakeLocation
	seen := map[string]bool{}
	for _, u := range t.SourceURIs {
		loc, ok := externalURIFolder(u, false)
		if !ok || seen[loc.key()] {
			continue
		}
		seen[loc.key()] = true
		out = append(out, loc)
	}
	return out
}

// lakeStoreFor is the URI scheme of a layout v2 destination, or "" for any other
// connector type. The v2 types are the orchestrator's objectLayoutV2DestSupported.
func lakeStoreFor(connectorType string) string {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(connectorType)), "_", "-") {
	case "gcs":
		return "gs"
	case "aws-s3":
		return "s3"
	case "azure-blob":
		return "azure"
	}
	return ""
}

// lakeConnPrefixKeys are the destination config keys the sink reads its base prefix
// from, first non-empty wins (kafka-sink-worker, reload_object_cleanup.go
// reloadObjectPrefixKeys).
var lakeConnPrefixKeys = []string{"path_prefix", "prefix", "base_prefix", "key_prefix", "base_path", "path"}

// lakePipelinePrefixRe is the pipeline prefix layout v2 accepts
// (storage.layoutV2PipelinePrefixRe).
var lakePipelinePrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var azureAccountNameRe = regexp.MustCompile(`(?i)(?:^|;)\s*AccountName=([^;]+)`)

func configString(cfg map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := cfg[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// lakeConnectionRoot is where a lake connection's folders start: the store, its
// bucket and its base prefix. ok is false when the config names no bucket.
func lakeConnectionRoot(store string, cfg map[string]interface{}) (loc lakeLocation, ok bool) {
	loc.Store = store
	bucket := configString(cfg, "bucket", "bucket_name")
	if store == "azure" {
		bucket = configString(cfg, "bucket", "container")
		account := configString(cfg, "account_name")
		if account == "" {
			// Only the account's name is read from a connection string.
			if m := azureAccountNameRe.FindStringSubmatch(configString(cfg, "connection_string")); m != nil {
				account = strings.TrimSpace(m[1])
			}
		}
		if account == "" || bucket == "" {
			return lakeLocation{}, false
		}
		bucket = strings.ToLower(account) + "/" + bucket
	}
	if bucket == "" {
		return lakeLocation{}, false
	}
	loc.Bucket = bucket
	if p := strings.Trim(strings.Trim(configString(cfg, lakeConnPrefixKeys...), " \t\n\r\v\f"), "/"); p != "" {
		loc.Folder = p + "/"
	}
	return loc, true
}

// lakeTableFolder is one table folder a layout v2 pipeline writes.
type lakeTableFolder struct {
	PipelineID   string
	PipelineName string
	ConnectionID string
	// TableKey is object_load_counters.table_key: <db>/[<schema>/]<table>, each
	// segment encoded the way the sink names folders.
	TableKey string
	Location lakeLocation
}

// lakeTableFolderAt places a table key under a pipeline: <conn prefix>/<pipeline
// prefix>/<table key>/. ok is false for a pipeline prefix layout v2 would refuse.
func lakeTableFolderAt(root lakeLocation, pipelinePrefix, tableKey string) (lakeLocation, bool) {
	pipelinePrefix = strings.TrimSpace(pipelinePrefix)
	tableKey = strings.Trim(tableKey, "/")
	if !lakePipelinePrefixRe.MatchString(pipelinePrefix) || tableKey == "" {
		return lakeLocation{}, false
	}
	root.Folder += pipelinePrefix + "/" + tableKey + "/"
	return root, true
}

// lakeTableKeyName is a table key as a dotted name, each segment decoded:
// "fintech/public/transactions" is "fintech.public.transactions".
func lakeTableKeyName(tableKey string) string {
	return strings.Join(lakeTableKeySegments(tableKey), ".")
}

func lakeTableKeySegments(tableKey string) []string {
	parts := strings.Split(strings.Trim(tableKey, "/"), "/")
	for i, p := range parts {
		if d, err := url.PathUnescape(p); err == nil {
			parts[i] = d
		}
	}
	return parts
}

// lakeTableKeyEndsWith reports whether a captured table name ("public.transactions",
// "transactions") is the end of a table key ("fintech/public/transactions").
func lakeTableKeyEndsWith(tableKey, captured string) bool {
	captured = strings.ToLower(strings.TrimSpace(captured))
	if captured == "" {
		return false
	}
	segs := lakeTableKeySegments(tableKey)
	for i := range segs {
		if strings.ToLower(strings.Join(segs[i:], ".")) == captured {
			return true
		}
	}
	return false
}

// savedExternalTables is one warehouse connection's saved list.
type savedExternalTables struct {
	ConnectionID string
	Tables       []warehouseExternalTable
}

// lakeTableLink says a warehouse's external table reads a lake pipeline's table folder.
type lakeTableLink struct {
	PipelineID       string
	PipelineName     string
	LakeConnectionID string
	TableKey         string

	WarehouseConnectionID string
	Schema                string
	Name                  string
}

// matchLakeTableLinks pairs each external table with the table folders it reads. A
// URI folder at or inside a table folder (a dt= partition, one file) reads that
// table; one above it (a pipeline's whole root) reads no single table and links
// nothing. Where folders nest, the deepest wins.
func matchLakeTableLinks(folders []lakeTableFolder, warehouses []savedExternalTables) []lakeTableLink {
	byFolder := map[string][]lakeTableFolder{}
	for _, f := range folders {
		byFolder[f.Location.key()] = append(byFolder[f.Location.key()], f)
	}
	var links []lakeTableLink
	seen := map[string]bool{}
	for _, w := range warehouses {
		for _, t := range w.Tables {
			for _, loc := range externalTableFolders(t) {
				for _, f := range lakeFoldersHolding(byFolder, loc) {
					k := f.PipelineID + "\x00" + f.TableKey + "\x00" + w.ConnectionID + "\x00" + externalTableKey(t.Schema, t.Name)
					if seen[k] {
						continue
					}
					seen[k] = true
					links = append(links, lakeTableLink{
						PipelineID:            f.PipelineID,
						PipelineName:          f.PipelineName,
						LakeConnectionID:      f.ConnectionID,
						TableKey:              f.TableKey,
						WarehouseConnectionID: w.ConnectionID,
						Schema:                t.Schema,
						Name:                  t.Name,
					})
				}
			}
		}
	}
	sort.Slice(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.WarehouseConnectionID != b.WarehouseConnectionID {
			return a.WarehouseConnectionID < b.WarehouseConnectionID
		}
		if ka, kb := externalTableKey(a.Schema, a.Name), externalTableKey(b.Schema, b.Name); ka != kb {
			return ka < kb
		}
		if a.PipelineID != b.PipelineID {
			return a.PipelineID < b.PipelineID
		}
		return a.TableKey < b.TableKey
	})
	return links
}

// lakeFoldersHolding is the deepest table folder at or above loc.
func lakeFoldersHolding(byFolder map[string][]lakeTableFolder, loc lakeLocation) []lakeTableFolder {
	folder := loc.Folder
	for {
		if hit := byFolder[lakeLocation{Store: loc.Store, Bucket: loc.Bucket, Folder: folder}.key()]; len(hit) > 0 {
			return hit
		}
		if folder == "" {
			return nil
		}
		// Up one level: "a/b/c/" to "a/b/".
		trimmed := strings.TrimSuffix(folder, "/")
		if i := strings.LastIndex(trimmed, "/"); i >= 0 {
			folder = trimmed[:i+1]
		} else {
			folder = ""
		}
	}
}

// loadLakeTableFolders reads every table folder the workspace's layout v2 pipelines
// write. A destination whose config cannot be read is skipped: its folders cannot be
// placed, and the graph draws those tables unlinked, as before.
func loadLakeTableFolders(ctx context.Context, database *sql.DB, workspaceID string) ([]lakeTableFolder, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT p.id::text,
		       COALESCE(p.name, ''),
		       c.id::text,
		       COALESCE(c.connector_type, ''),
		       COALESCE(c.config, ''),
		       COALESCE(p.config->>'destination_namespace', ''),
		       k.table_key
		FROM pipelines p
		JOIN connections c ON c.id = p.destination_connection_id
		JOIN object_load_counters k ON k.pipeline_id = p.id
		WHERE p.workspace_id = $1::uuid
		  AND p.storage_layout_version = 2
		ORDER BY 1, 7
		LIMIT $2`, workspaceID, lakeTableFolderLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roots := map[string]*lakeLocation{}
	var folders []lakeTableFolder
	for rows.Next() {
		var f lakeTableFolder
		var connectorType, configEncrypted, pipelinePrefix string
		if err := rows.Scan(&f.PipelineID, &f.PipelineName, &f.ConnectionID, &connectorType,
			&configEncrypted, &pipelinePrefix, &f.TableKey); err != nil {
			return nil, err
		}
		root, read := roots[f.ConnectionID]
		if !read {
			root = lakeRootFromConfig(f.ConnectionID, connectorType, configEncrypted)
			roots[f.ConnectionID] = root
		}
		if root == nil {
			continue
		}
		loc, ok := lakeTableFolderAt(*root, pipelinePrefix, f.TableKey)
		if !ok {
			continue
		}
		f.Location = loc
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

func lakeRootFromConfig(connectionID, connectorType, configEncrypted string) *lakeLocation {
	store := lakeStoreFor(connectorType)
	if store == "" {
		return nil
	}
	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		log.Warnf("[Lineage] lake connection %s: could not read its config: %v", connectionID, err)
		return nil
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		log.Warnf("[Lineage] lake connection %s: could not parse its config: %v", connectionID, err)
		return nil
	}
	root, ok := lakeConnectionRoot(store, cfg)
	if !ok {
		return nil
	}
	return &root
}

// lakeLinkLoad is the links for one graph, and whether a warehouse with no saved
// list is being read right now (the page asks again shortly).
type lakeLinkLoad struct {
	Links   []lakeTableLink
	Pending bool
}

// loadLakeTableLinks is every link from the workspace's lake pipelines to the
// external tables of its warehouses, or of the one warehouse named. A list that is
// missing or more than an hour old is re-read in the background; the saved one is
// used meanwhile. A workspace with no layout v2 pipeline reads no warehouse.
func loadLakeTableLinks(ctx context.Context, database *sql.DB, workspaceID, warehouseID string) (lakeLinkLoad, error) {
	var out lakeLinkLoad
	folders, err := loadLakeTableFolders(ctx, database, workspaceID)
	if err != nil || len(folders) == 0 {
		return out, err
	}

	rows, err := database.QueryContext(ctx, `
		SELECT c.id::text,
		       COALESCE(c.connector_type, ''),
		       COALESCE(c.config, ''),
		       w.tables,
		       w.connection_id IS NOT NULL,
		       COALESCE(w.read_at < NOW() - make_interval(secs => $4), true)
		FROM connections c
		LEFT JOIN warehouse_external_tables w ON w.connection_id = c.id
		WHERE c.workspace_id = $1::uuid
		  AND ($2 = '' OR c.id::text = $2)
		  AND lower(c.connector_type) = ANY($3)
		ORDER BY 1`,
		workspaceID, warehouseID, pgdriver.StringArray(externalTableWarehouseTypes),
		externalTablesMaxAge.Seconds())
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var warehouses []savedExternalTables
	for rows.Next() {
		var w savedExternalTables
		var connectorType, configEncrypted string
		var raw []byte
		var saved, stale bool
		if err := rows.Scan(&w.ConnectionID, &connectorType, &configEncrypted, &raw, &saved, &stale); err != nil {
			return out, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &w.Tables); err != nil {
				log.Warnf("[Lineage] connection %s: could not read its saved external tables: %v", w.ConnectionID, err)
			}
		}
		if stale && refreshExternalTables(w.ConnectionID, connectorType, configEncrypted) && !saved {
			out.Pending = true
		}
		warehouses = append(warehouses, w)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Links = matchLakeTableLinks(folders, warehouses)
	return out, nil
}

var (
	// externalTableRefreshing holds the connections being re-read now, so a page
	// that loads twice does not list a warehouse twice.
	externalTableRefreshing sync.Map
	// externalTableRefreshFailed holds when a connection's last re-read failed.
	externalTableRefreshFailed sync.Map
)

// refreshExternalTables re-reads a warehouse's tables in the background, which saves
// its external tables (buildWarehouseSchemaIndex). It reports whether a read is
// under way. A var so tests can stand in for the warehouse.
var refreshExternalTables = func(connectionID, connectorType, configEncrypted string) bool {
	if at, ok := externalTableRefreshFailed.Load(connectionID); ok && time.Since(at.(time.Time)) < externalTableRefreshBackoff {
		return false
	}
	if _, busy := externalTableRefreshing.LoadOrStore(connectionID, struct{}{}); busy {
		return true
	}
	go func() {
		defer externalTableRefreshing.Delete(connectionID)
		ctx, cancel := context.WithTimeout(context.Background(), externalTableRefreshTimeout)
		defer cancel()
		if _, err := buildWarehouseSchemaIndex(ctx, connectionID, connectorType, configEncrypted); err != nil {
			externalTableRefreshFailed.Store(connectionID, time.Now())
			log.Warnf("[Lineage] connection %s: could not re-read its tables for lake links: %v", connectionID, err)
			return
		}
		externalTableRefreshFailed.Delete(connectionID)
	}()
	return true
}

// lakeLinkProducers are the tables a lake pipeline feeds in one warehouse, as
// producers on that warehouse: the upstream suggestion matches a model's reads
// against them exactly as it does a pipeline's own tables.
func lakeLinkProducers(links []lakeTableLink, warehouseID string) []tableProducer {
	var out []tableProducer
	for _, l := range links {
		if l.WarehouseConnectionID != warehouseID {
			continue
		}
		out = append(out, tableProducer{
			ConnectionID: l.WarehouseConnectionID,
			Kind:         assetKindPipeline,
			Table: producedTable{
				ProducerID:    l.PipelineID,
				ProducerName:  l.PipelineName,
				DestQualified: fmt.Sprintf("%s.%s", l.Schema, l.Name),
				TableName:     l.Name,
			},
		})
	}
	return out
}
