package handlers

// Workspace layers: a workspace's own model layers (migration 135), such as
// Silver and Gold, or Staging and Marts. A layer is a label on a model and
// nothing more: it changes nothing about how a model runs or who may run it.
// Only a workspace owner or admin adds, renames, orders or deletes one; anyone
// in the workspace sees them, and a model takes one by name through
// PUT /explorer/saved/:id/materialization. A table a pipeline writes is not in
// a layer: the lineage graph marks it a pipeline table.
//
// Every write holds one advisory lock per workspace, so two admins adding a
// layer at once cannot both take the next position, and the 20-layer cap holds.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	workspaceLayerNameMax = 40
	workspaceLayerDescMax = 200
	workspaceLayerMax     = 20
)

// WorkspaceLayer is a layer as the API shows it. ModelCount counts only the
// models the caller can see: a private model is its author's alone.
type WorkspaceLayer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Position    int    `json:"position"`
	ModelCount  int    `json:"model_count"`
}

// workspaceLayerRequest: on create, name is required and position is ignored
// (a new layer goes last); on update, a field left out keeps its value.
type workspaceLayerRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Position    *int    `json:"position"`
}

// cleanLayerText collapses whitespace runs to one space and refuses control
// characters, which strings.Fields keeps when they are not whitespace.
func cleanLayerText(raw string) (string, bool) {
	s := strings.Join(strings.Fields(raw), " ")
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func validWorkspaceLayerName(raw string) (string, error) {
	name, ok := cleanLayerText(raw)
	if !ok {
		return "", errors.New("a layer name cannot contain control characters")
	}
	if name == "" {
		return "", errors.New("a layer needs a name, such as Silver or Marts")
	}
	if len([]rune(name)) > workspaceLayerNameMax {
		return "", fmt.Errorf("a layer name may be at most %d characters", workspaceLayerNameMax)
	}
	return name, nil
}

func validWorkspaceLayerDescription(raw string) (string, error) {
	desc, ok := cleanLayerText(raw)
	if !ok {
		return "", errors.New("a layer description cannot contain control characters")
	}
	if len([]rune(desc)) > workspaceLayerDescMax {
		return "", fmt.Errorf("a layer description may be at most %d characters", workspaceLayerDescMax)
	}
	return desc, nil
}

// ListWorkspaceLayers lists the workspace's layers in order.
// GET /api/v1/model-layers
func ListWorkspaceLayers(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	role, ok := requireWorkspaceRole(c, security.WSViewer)
	if !ok {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	layers, err := loadWorkspaceLayers(c.Request.Context(), database, ws, userID)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list layers"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"layers": layers, "can_manage": role.Meets(security.WSAdmin)})
}

func loadWorkspaceLayers(ctx context.Context, database *sql.DB, ws, userID string) ([]WorkspaceLayer, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT wl.id, wl.name, wl.description, wl.position,
		       (SELECT COUNT(*) FROM saved_queries sq
		        WHERE sq.workspace_id = wl.workspace_id AND sq.layer_id = wl.id
		          AND (sq.visibility = 'workspace' OR sq.created_by = $2))
		FROM workspace_layers wl
		WHERE wl.workspace_id = $1
		ORDER BY wl.position
	`, ws, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkspaceLayer{}
	for rows.Next() {
		var l WorkspaceLayer
		if err := rows.Scan(&l.ID, &l.Name, &l.Description, &l.Position, &l.ModelCount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// beginLayerWrite opens a transaction holding the workspace's layer lock.
func beginLayerWrite(ctx context.Context, database *sql.DB, ws string) (*sql.Tx, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "workspace_layers:"+ws); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// CreateWorkspaceLayer adds a layer after the last one. Owner or admin.
// POST /api/v1/model-layers
func CreateWorkspaceLayer(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	var req workspaceLayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Name == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a layer needs a name, such as Silver or Marts"})
		return
	}
	name, err := validWorkspaceLayerName(*req.Name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	desc := ""
	if req.Description != nil {
		if desc, err = validWorkspaceLayerDescription(*req.Description); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ctx := c.Request.Context()
	tx, err := beginLayerWrite(ctx, database, ws)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add the layer"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var count, next int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(position) + 1, 0)
		FROM workspace_layers WHERE workspace_id = $1`, ws).Scan(&count, &next); err != nil {
		log.WithError(err).Error("[workspace_layers] count failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add the layer"})
		return
	}
	if count >= workspaceLayerMax {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
			"A workspace can have at most %d layers. Delete one you no longer use first.", workspaceLayerMax)})
		return
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workspace_layers (workspace_id, name, description, position, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, ws, name, desc, next, userID).Scan(&id)
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("This workspace already has a layer named %q.", name)})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workspace_layers] insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add the layer"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workspace_layers] commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add the layer"})
		return
	}
	logAudit(c, "create_workspace_layer", "workspace_layer", id, map[string]interface{}{
		"name": name, "position": next,
	})
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

// UpdateWorkspaceLayer renames a layer, changes its description, or moves it
// to another position (the others shift to make room). Owner or admin.
// PATCH /api/v1/model-layers/:id
func UpdateWorkspaceLayer(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "layer not found"})
		return
	}
	id := parsed.String()
	var req workspaceLayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Name == nil && req.Description == nil && req.Position == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to change: send a name, a description or a position"})
		return
	}
	var name, desc *string
	if req.Name != nil {
		n, err := validWorkspaceLayerName(*req.Name)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		name = &n
	}
	if req.Description != nil {
		d, err := validWorkspaceLayerDescription(*req.Description)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		desc = &d
	}
	if req.Position != nil && *req.Position < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "position must be 0 or more"})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ctx := c.Request.Context()
	tx, err := beginLayerWrite(ctx, database, ws)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the layer"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	order, err := layerOrder(ctx, tx, ws)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] load failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the layer"})
		return
	}
	from := slices.Index(order, id)
	if from < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "layer not found"})
		return
	}
	details := map[string]interface{}{}
	if name != nil || desc != nil {
		_, err := tx.ExecContext(ctx, `UPDATE workspace_layers
			SET name = COALESCE($3, name), description = COALESCE($4, description)
			WHERE id = $1 AND workspace_id = $2`, id, ws, name, desc)
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("This workspace already has a layer named %q.", *name)})
			return
		}
		if err != nil {
			log.WithError(err).Error("[workspace_layers] update failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the layer"})
			return
		}
		if name != nil {
			details["name"] = *name
		}
	}
	if req.Position != nil {
		to := min(*req.Position, len(order)-1)
		if to != from {
			moved := append(append([]string{}, order[:from]...), order[from+1:]...)
			moved = append(moved[:to], append([]string{id}, moved[to:]...)...)
			if err := writeLayerOrder(ctx, tx, ws, moved); err != nil {
				log.WithError(err).Error("[workspace_layers] reorder failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to move the layer"})
				return
			}
		}
		details["position"] = to
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workspace_layers] commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the layer"})
		return
	}
	logAudit(c, "update_workspace_layer", "workspace_layer", id, details)
	c.JSON(http.StatusOK, gin.H{"id": id})
}

// DeleteWorkspaceLayer deletes a layer. Its models stay, with no layer: a
// layer is only a label. Owner or admin.
// DELETE /api/v1/model-layers/:id
func DeleteWorkspaceLayer(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "layer not found"})
		return
	}
	id := parsed.String()
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ctx := c.Request.Context()
	tx, err := beginLayerWrite(ctx, database, ws)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE conflicts with the key-share lock a model's foreign-key check
	// takes, so a model being put in this layer right now either lands before
	// the models are cleared below, or fails its check after the delete.
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM workspace_layers
		WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, id, ws).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "layer not found"})
			return
		}
		log.WithError(err).Error("[workspace_layers] load failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	res, err := tx.ExecContext(ctx, `UPDATE saved_queries SET layer_id = NULL
		WHERE workspace_id = $1 AND layer_id = $2`, ws, id)
	if err != nil {
		log.WithError(err).Error("[workspace_layers] clear models failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	cleared, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_layers WHERE id = $1 AND workspace_id = $2`, id, ws); err != nil {
		log.WithError(err).Error("[workspace_layers] delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	order, err := layerOrder(ctx, tx, ws)
	if err == nil {
		err = writeLayerOrder(ctx, tx, ws, order)
	}
	if err != nil {
		log.WithError(err).Error("[workspace_layers] renumber failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workspace_layers] commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the layer"})
		return
	}
	logAudit(c, "delete_workspace_layer", "workspace_layer", id, map[string]interface{}{
		"name": name, "models_unlabelled": cleared,
	})
	c.JSON(http.StatusOK, gin.H{"deleted": true, "models_unlabelled": cleared})
}

// layerOrder is the workspace's layer ids, first to last.
func layerOrder(ctx context.Context, tx *sql.Tx, ws string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM workspace_layers WHERE workspace_id = $1 ORDER BY position`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// writeLayerOrder sets each layer's position to its index in ids. The unique
// position constraint is checked at commit, so the positions may cross.
func writeLayerOrder(ctx context.Context, tx *sql.Tx, ws string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE workspace_layers wl SET position = v.ord - 1
		FROM unnest(string_to_array($2, ',')::uuid[]) WITH ORDINALITY AS v(id, ord)
		WHERE wl.workspace_id = $1 AND wl.id = v.id AND wl.position <> v.ord - 1
	`, ws, strings.Join(ids, ","))
	return err
}
