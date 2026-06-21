package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// PlayerHandler serves the whitelist-roster endpoints (owner-scoped): a user's
// players and which of the user's servers each may use.
type PlayerHandler struct {
	players *repository.PlayerRepository
	servers *repository.GameServerRepository
}

// NewPlayerHandler constructs a PlayerHandler.
func NewPlayerHandler(players *repository.PlayerRepository, servers *repository.GameServerRepository) *PlayerHandler {
	return &PlayerHandler{players: players, servers: servers}
}

type createPlayerRequest struct {
	Username string `json:"username" binding:"required"`
	// ServerIDs is the set of the caller's servers this player may use; optional
	// on create (a player may start granted on nothing).
	ServerIDs []string `json:"server_ids"`
}

type updatePlayerRequest struct {
	// Both fields are optional; an absent field is left unchanged. When ServerIDs
	// is present it replaces the whole grant set (check/uncheck semantics).
	Username  *string   `json:"username"`
	ServerIDs *[]string `json:"server_ids"`
}

// Create adds a player to the caller's roster, optionally granting it servers.
func (h *PlayerHandler) Create(c *gin.Context) {
	var req createPlayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !model.ValidUsername(req.Username) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username must be 3–16 characters of letters, digits, or underscore"})
		return
	}

	ownerID := middleware.UserIDFromContext(c)
	ids, ok := h.validatedServerIDs(c, ownerID, req.ServerIDs)
	if !ok {
		return // validatedServerIDs wrote the error response
	}

	p := &model.Player{OwnerID: ownerID, Username: req.Username}
	if err := h.players.Create(c.Request.Context(), p, ids); err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a player with that username already exists"})
			return
		}
		logger.FromContext(c).Error("create player", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, p)
}

// List returns the caller's whitelist roster.
func (h *PlayerHandler) List(c *gin.Context) {
	players, err := h.players.ListByOwner(c.Request.Context(), middleware.UserIDFromContext(c))
	if err != nil {
		logger.FromContext(c).Error("list players", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"players": players})
}

// Get returns a single owned player.
func (h *PlayerHandler) Get(c *gin.Context) {
	p, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, p)
}

// Update edits a player's username and/or its set of granted servers. A present
// server_ids replaces the whole set, so checking/unchecking a server is a PATCH
// with the resulting list.
func (h *PlayerHandler) Update(c *gin.Context) {
	var req updatePlayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	p, ok := h.ownedOr404(c)
	if !ok {
		return
	}

	username := p.Username
	if req.Username != nil {
		if !model.ValidUsername(*req.Username) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "username must be 3–16 characters of letters, digits, or underscore"})
			return
		}
		username = *req.Username
	}

	ids := p.ServerIDs
	if req.ServerIDs != nil {
		validated, valid := h.validatedServerIDs(c, p.OwnerID, *req.ServerIDs)
		if !valid {
			return
		}
		ids = validated
	}

	if err := h.players.Update(c.Request.Context(), p.ID, username, ids); err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a player with that username already exists"})
			return
		}
		logger.FromContext(c).Error("update player", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	updated, err := h.players.GetByID(c.Request.Context(), p.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete removes a player from the caller's roster.
func (h *PlayerHandler) Delete(c *gin.Context) {
	p, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	if err := h.players.Delete(c.Request.Context(), p.ID); err != nil {
		logger.FromContext(c).Error("delete player", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusNoContent, nil)
}

// validatedServerIDs de-duplicates the requested server ids and verifies every
// one is a live server the caller owns, writing a 400 and returning ok=false on
// any foreign or unknown id (no existence leak: an unowned id reads the same as a
// missing one). An empty/absent request yields an empty set.
func (h *PlayerHandler) validatedServerIDs(c *gin.Context, ownerID string, requested []string) ([]string, bool) {
	if len(requested) == 0 {
		return []string{}, true
	}
	owned, err := h.ownedServerSet(c.Request.Context(), ownerID)
	if err != nil {
		logger.FromContext(c).Error("list owner servers", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return nil, false
	}
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, id := range requested {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !owned[id] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "one or more server_ids are not your servers"})
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// ownedServerSet returns the set of live server ids belonging to ownerID.
func (h *PlayerHandler) ownedServerSet(ctx context.Context, ownerID string) (map[string]bool, error) {
	servers, err := h.servers.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(servers))
	for i := range servers {
		set[servers[i].ID] = true
	}
	return set, nil
}

// ownedOr404 loads the player in the URL and verifies the caller owns it,
// writing a 404 for both missing and non-owned players.
func (h *PlayerHandler) ownedOr404(c *gin.Context) (*model.Player, bool) {
	p, err := h.players.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			logger.FromContext(c).Error("get player", zap.Error(err))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "player not found"})
		return nil, false
	}
	if p.OwnerID != middleware.UserIDFromContext(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "player not found"})
		return nil, false
	}
	return p, true
}
