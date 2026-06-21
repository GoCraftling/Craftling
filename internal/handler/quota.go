package handler

import (
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// QuotaHandler serves the quota endpoints (P9): a user's self-view of their own
// quota and usage, and the admin view/set of any user's quota.
type QuotaHandler struct {
	quotas  *repository.QuotaRepository
	servers *repository.GameServerRepository
	users   *repository.UserRepository
}

// NewQuotaHandler constructs a QuotaHandler.
func NewQuotaHandler(quotas *repository.QuotaRepository, servers *repository.GameServerRepository, users *repository.UserRepository) *QuotaHandler {
	return &QuotaHandler{quotas: quotas, servers: servers, users: users}
}

// quotaResponse is the shape returned by every quota endpoint: the effective
// quota plus the user's current usage, so a caller can render headroom without a
// second request.
type quotaResponse struct {
	Quota model.UserQuota  `json:"quota"`
	Usage model.QuotaUsage `json:"usage"`
}

// setQuotaRequest is the admin payload to set a user's quota override. Each limit
// is required and must be non-negative; 0 means unlimited on that axis.
type setQuotaRequest struct {
	MaxServers  *int `json:"max_servers" binding:"required,gte=0"`
	MaxCPUs     *int `json:"max_cpus" binding:"required,gte=0"`
	MaxMemoryMB *int `json:"max_memory_mb" binding:"required,gte=0"`
}

// Mine returns the authenticated caller's effective quota and current usage.
func (h *QuotaHandler) Mine(c *gin.Context) {
	h.writeQuota(c, middleware.UserIDFromContext(c))
}

// GetForUser returns any user's effective quota and usage. Guarded by
// RequireRole(admin). Responds 404 if the user does not exist.
func (h *QuotaHandler) GetForUser(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.users.GetByID(c.Request.Context(), id); err != nil {
		h.userLookupError(c, err)
		return
	}
	h.writeQuota(c, id)
}

// SetForUser sets (upserts) a user's quota override. Guarded by
// RequireRole(admin). Responds 404 if the user does not exist.
func (h *QuotaHandler) SetForUser(c *gin.Context) {
	var req setQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	id := c.Param("id")
	if _, err := h.users.GetByID(c.Request.Context(), id); err != nil {
		h.userLookupError(c, err)
		return
	}

	stored, err := h.quotas.Set(c.Request.Context(), model.UserQuota{
		UserID:      id,
		MaxServers:  *req.MaxServers,
		MaxCPUs:     *req.MaxCPUs,
		MaxMemoryMB: *req.MaxMemoryMB,
	})
	if err != nil {
		logger.FromContext(c).Error("set user quota", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	usage, err := h.servers.OwnerUsage(c.Request.Context(), id)
	if err != nil {
		logger.FromContext(c).Error("owner usage", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, quotaResponse{Quota: stored, Usage: usage})
}

// DeleteForUser removes a user's override, reverting them to the system default.
// Guarded by RequireRole(admin). Responds 404 if the user does not exist.
func (h *QuotaHandler) DeleteForUser(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.users.GetByID(c.Request.Context(), id); err != nil {
		h.userLookupError(c, err)
		return
	}
	if err := h.quotas.Delete(c.Request.Context(), id); err != nil {
		logger.FromContext(c).Error("delete user quota", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	h.writeQuota(c, id)
}

// writeQuota loads and writes the effective quota + usage for a user id.
func (h *QuotaHandler) writeQuota(c *gin.Context, userID string) {
	ctx := c.Request.Context()
	quota, err := h.quotas.Get(ctx, userID)
	if err != nil {
		logger.FromContext(c).Error("get quota", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	usage, err := h.servers.OwnerUsage(ctx, userID)
	if err != nil {
		logger.FromContext(c).Error("owner usage", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, quotaResponse{Quota: quota, Usage: usage})
}

// userLookupError maps a user lookup failure to 404 (not found) or 500.
func (h *QuotaHandler) userLookupError(c *gin.Context, err error) {
	if !errors.Is(err, repository.ErrNotFound) {
		logger.FromContext(c).Error("get user", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
}
