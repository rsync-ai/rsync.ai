package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"api-gateway/internal/cache"
	"api-gateway/internal/db"

	"github.com/rsync-ai/backend-orchestrator/pkg/namespacemodel"
	"github.com/rsync-ai/shared/crypto"
	log "github.com/sirupsen/logrus"
)

// warehouseSchemaMaxTables caps the tables a warehouse's schema index lists. The
// connector reads one table's metadata per table it lists (BigQuery's tables.get),
// so the cap also bounds that.
const warehouseSchemaMaxTables = 200

// isDiscoveredWarehouse reports whether the Explorer's schema index for this
// connector comes from the connector's discover_schema: a SQL warehouse the
// gateway does not query directly (BigQuery, ClickHouse, Snowflake, Oracle).
func isDiscoveredWarehouse(connectorType string) bool {
	capability := ResolveExplorerCapability(connectorType)
	return capability.QueryLanguage == langSQL && capability.SchemaStrategy == schemaMCPDiscover
}

// buildWarehouseSchemaIndex lists a delegated warehouse's tables, with their
// columns and row counts, through the connector's discover_schema.
//
// A connection whose tables live in datasets (BigQuery) names one dataset, but
// the Explorer browses the warehouse: the dataset is left blank so the connector
// lists every dataset the connection can see. If that wider listing fails (a
// service account that can read its dataset but not list the project's), the
// connection's own dataset is listed, as before.
//
// Each listing also saves the warehouse's external tables, and where their files
// are, for the Lineage page (saveWarehouseExternalTables). A failed save is
// logged and never fails the index.
func buildWarehouseSchemaIndex(ctx context.Context, connectionID, connectorType, configEncrypted string) (*cache.ExplorerSchemaIndex, error) {
	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt config: %w", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	q := schemaIndexDiscovery{
		what:             "tables",
		includeRowCounts: true,
		tableDetails:     true,
		maxTables:        warehouseSchemaMaxTables,
	}
	if widened := warehouseWidenedConfig(connectorType, config); widened != nil {
		tables, listing, err := discoverSchemaIndexTables(ctx, connectionID, connectorType, widened, q)
		if err == nil {
			keepWarehouseExternalTables(ctx, connectionID, connectorType, listing)
			return warehouseSchemaIndex(connectionID, tables), nil
		}
		if requesterGone(ctx) {
			return nil, err
		}
		log.Warnf("[Explorer] connection %s: listing every dataset failed, listing its own dataset only: %v", connectionID, err)
	}
	tables, listing, err := discoverSchemaIndexTables(ctx, connectionID, connectorType, config, q)
	if err != nil {
		return nil, err
	}
	// This listing covers the connection's own dataset (or every dataset, when
	// it names none), so external tables saved from other datasets are kept.
	listing.Scope, _ = config["dataset_id"].(string)
	keepWarehouseExternalTables(ctx, connectionID, connectorType, listing)
	return warehouseSchemaIndex(connectionID, tables), nil
}

// keepWarehouseExternalTables saves a BigQuery listing's external tables. Other
// warehouses do not report them yet, and a save from one would only clear the row.
func keepWarehouseExternalTables(ctx context.Context, connectionID, connectorType string, listing warehouseTableListing) {
	if !warehouseKeepsExternalTables(connectorType) {
		return
	}
	database := db.GetDB()
	if database == nil {
		return
	}
	if err := saveWarehouseExternalTables(ctx, database, connectionID, listing); err != nil {
		log.Warnf("[Explorer] connection %s: could not save its external tables for Lineage: %v", connectionID, err)
	}
}

// warehouseWidenedConfig returns a copy of config with its dataset left blank, or
// nil when the connector's tables are not in datasets or none is named.
func warehouseWidenedConfig(connectorType string, config map[string]interface{}) map[string]interface{} {
	if namespacemodel.For(connectorType).TableNamespace != "dataset" {
		return nil
	}
	if ds, _ := config["dataset_id"].(string); ds == "" {
		return nil
	}
	widened := make(map[string]interface{}, len(config))
	for k, v := range config {
		widened[k] = v
	}
	widened["dataset_id"] = ""
	return widened
}

// warehouseSchemaIndex builds the index the way the direct engines' is built:
// rsync.ai's own bookkeeping tables left out, relationships inferred from names.
func warehouseSchemaIndex(connectionID string, discovered []cache.ExplorerTableIndex) *cache.ExplorerSchemaIndex {
	tables := make([]cache.ExplorerTableIndex, 0, len(discovered))
	for _, t := range discovered {
		if !isInternalExplorerTable(t.Name) {
			tables = append(tables, t)
		}
	}
	foreignKeys := cache.InferForeignKeysFromHeuristics(tables, nil)
	if foreignKeys == nil {
		foreignKeys = []cache.ExplorerForeignKeyIndex{}
	}
	return &cache.ExplorerSchemaIndex{
		ConnectionID:    connectionID,
		SchemaHash:      cache.ComputeSchemaHash(tables, foreignKeys),
		LastRefreshedAt: time.Now(),
		TableCount:      len(tables),
		Tables:          tables,
		ForeignKeys:     foreignKeys,
	}
}
