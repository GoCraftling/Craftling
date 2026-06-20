package handler

import (
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AgentHandler serves the agent-facing host endpoints: a host agent registers
// itself and then heartbeats to prove liveness.
type AgentHandler struct {
	hosts   *repository.HostRepository
	servers *repository.GameServerRepository
}

// NewAgentHandler constructs an AgentHandler.
func NewAgentHandler(hosts *repository.HostRepository, servers *repository.GameServerRepository) *AgentHandler {
	return &AgentHandler{hosts: hosts, servers: servers}
}

type registerHostRequest struct {
	// ID is the agent's own stable identity. Optional, but supplying it lets a
	// host keep the same id across a control-plane restart (see HostRepository).
	ID            string `json:"id" binding:"omitempty,uuid"`
	Hostname      string `json:"hostname" binding:"required,min=1,max=253"`
	Address       string `json:"address" binding:"required"`
	Zone          string `json:"zone" binding:"omitempty,max=64"`
	CPUsTotal     int    `json:"cpus_total" binding:"required,min=1"`
	MemoryMBTotal int    `json:"memory_mb_total" binding:"required,min=1"`
	AgentVersion  string `json:"agent_version" binding:"omitempty,max=64"`
}

// Register adds (or re-registers) the calling host to the fleet inventory and
// returns the stored record, including its assigned id.
func (h *AgentHandler) Register(c *gin.Context) {
	var req registerHostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Reconstruct any capacity already committed to this host from the durable
	// record, so a host re-registering after a control-plane restart comes back
	// with its real allocatable rather than a clean slate. Only meaningful when
	// the agent supplies its stable id (otherwise there is nothing to match).
	usedCPUs, usedMemMB, err := h.servers.UsedCapacity(ctx, req.ID)
	if err != nil {
		logger.FromContext(c).Error("reconstruct host capacity", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	host, err := h.hosts.RegisterReserved(ctx, &model.Host{
		ID:            req.ID,
		Hostname:      req.Hostname,
		Address:       req.Address,
		Zone:          req.Zone,
		CPUsTotal:     req.CPUsTotal,
		MemoryMBTotal: req.MemoryMBTotal,
		AgentVersion:  req.AgentVersion,
	}, usedCPUs, usedMemMB)
	if err != nil {
		logger.FromContext(c).Error("register host", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, host)
}

// Heartbeat refreshes the liveness timestamp for the host named in the path. A
// host the control plane has never seen (or has forgotten) gets a 404 so the
// agent knows to re-register.
func (h *AgentHandler) Heartbeat(c *gin.Context) {
	err := h.hosts.Heartbeat(c.Request.Context(), c.Param("id"))
	if errors.Is(err, repository.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	if err != nil {
		logger.FromContext(c).Error("host heartbeat", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
