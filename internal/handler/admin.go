package handler

import (
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AdminHandler serves admin-only endpoints.
type AdminHandler struct {
	users   *repository.UserRepository
	servers *repository.GameServerRepository
	hosts   *repository.HostRepository
	logs    LogProvider
}

// NewAdminHandler constructs an AdminHandler. logs may be nil only if the admin
// logs endpoint is never exercised.
func NewAdminHandler(users *repository.UserRepository, servers *repository.GameServerRepository, hosts *repository.HostRepository, logs LogProvider) *AdminHandler {
	return &AdminHandler{users: users, servers: servers, hosts: hosts, logs: logs}
}

// ListUsers returns all users. Guarded by RequireRole(admin).
func (h *AdminHandler) ListUsers(c *gin.Context) {
	users, err := h.users.List(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list users", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

// ListServers returns every server across all owners. Guarded by RequireRole(admin).
func (h *AdminHandler) ListServers(c *gin.Context) {
	servers, err := h.servers.ListAll(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list all servers", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// ServerLogs returns the captured console output of any server's backing VM,
// regardless of owner. Guarded by RequireRole(admin).
func (h *AdminHandler) ServerLogs(c *gin.Context) {
	s, err := h.servers.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			logger.FromContext(c).Error("get server", zap.Error(err))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}
	writeLogs(c, h.logs, s)
}

// ListHosts returns the whole fleet inventory. Guarded by RequireRole(admin).
func (h *AdminHandler) ListHosts(c *gin.Context) {
	hosts, err := h.hosts.List(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list hosts", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hosts": hosts})
}

// DrainHost puts a host into the draining state (P8c): it takes no new placements
// and the reconciler migrates its running servers off. The endpoint only records
// the intent — the reconciler, the sole writer of compute side effects, does the
// migration. Guarded by RequireRole(admin).
func (h *AdminHandler) DrainHost(c *gin.Context) {
	if err := h.hosts.SetDraining(c.Request.Context(), c.Param("id")); err != nil {
		h.writeHostStateErr(c, err, "drain host")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "draining"})
}

// UndrainHost returns a draining host to ready so it accepts placements again.
// Guarded by RequireRole(admin).
func (h *AdminHandler) UndrainHost(c *gin.Context) {
	if err := h.hosts.Undrain(c.Request.Context(), c.Param("id")); err != nil {
		h.writeHostStateErr(c, err, "undrain host")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// writeHostStateErr maps a host state-change error to an HTTP response: 404 for an
// unknown host, 409 for a host that can't be drained (it is down), 500 otherwise.
func (h *AdminHandler) writeHostStateErr(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
	case errors.Is(err, repository.ErrHostNotReady):
		c.JSON(http.StatusConflict, gin.H{"error": "host is down; cannot drain"})
	default:
		logger.FromContext(c).Error(op, zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
