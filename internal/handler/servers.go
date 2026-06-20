package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Default resource allocation for a new server.
const (
	defaultCPUs     = 2
	defaultMemoryMB = 2048
)

// TemplateResolver resolves a marketplace template into the inputs a server needs.
// registry.Client satisfies it; tests supply a fake. ManifestParsed surfaces
// registry.ErrNotFound for an unknown id.
type TemplateResolver interface {
	ManifestParsed(ctx context.Context, id string) (*registry.Manifest, error)
}

// ServerHandler serves the game-server CRUD endpoints.
type ServerHandler struct {
	servers   *repository.GameServerRepository
	sched     *scheduler.Scheduler
	templates TemplateResolver
}

// NewServerHandler constructs a ServerHandler. templates may be nil only if the
// template-launch path is never exercised.
func NewServerHandler(servers *repository.GameServerRepository, sched *scheduler.Scheduler, templates TemplateResolver) *ServerHandler {
	return &ServerHandler{servers: servers, sched: sched, templates: templates}
}

// createServerRequest is the create payload. A request takes one of two shapes:
// a template launch (TemplateID set, with Answers + EULAAccepted) where the
// control plane resolves the image and env server-side; or a direct create
// (Version set) that boots the agent's default image. Validation that the right
// fields are present for the chosen shape happens in Create, not via binding tags,
// since the requirement is conditional.
type createServerRequest struct {
	Name         string            `json:"name" binding:"required,min=1,max=64"`
	Version      string            `json:"version"`
	TemplateID   string            `json:"template_id"`
	Answers      map[string]string `json:"answers"`
	EULAAccepted bool              `json:"eula_accepted"`
	CPUs         int               `json:"cpus" binding:"omitempty,min=1,max=16"`
	MemoryMB     int               `json:"memory_mb" binding:"omitempty,min=512,max=65536"`
}

type updateServerRequest struct {
	Name         *string `json:"name" binding:"omitempty,min=1,max=64"`
	Version      *string `json:"version" binding:"omitempty,min=1"`
	DesiredState *string `json:"desired_state" binding:"omitempty,oneof=running stopped"`
}

// Create provisions a new game server (desired state: running). A request with a
// template_id is resolved server-side against the trusted registry (image + env);
// otherwise version selects the agent's default image directly.
func (h *ServerHandler) Create(c *gin.Context) {
	var req createServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s := &model.GameServer{
		OwnerID:      middleware.UserIDFromContext(c),
		Name:         req.Name,
		Game:         model.GameMinecraft,
		CPUs:         orDefault(req.CPUs, defaultCPUs),
		MemoryMB:     orDefault(req.MemoryMB, defaultMemoryMB),
		DesiredState: model.DesiredRunning,
		Status:       model.StatusPending,
	}

	if req.TemplateID != "" {
		if !h.applyTemplate(c, &req, s) {
			return // applyTemplate wrote the error response
		}
	} else {
		if req.Version == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version is required when no template_id is given"})
			return
		}
		s.Version = req.Version
	}

	// Reject a spec no host could ever run, rather than admitting a server that
	// would sit unschedulable forever. With no hosts yet, creation is allowed —
	// the server waits for a host to join.
	if ok, err := h.sched.CanEverFit(c.Request.Context(), s.CPUs, s.MemoryMB); err != nil {
		logger.FromContext(c).Error("capacity check", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	} else if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "requested resources exceed the capacity of any host in the fleet"})
		return
	}

	if err := h.servers.Create(c.Request.Context(), s); err != nil {
		logger.FromContext(c).Error("create server", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, s)
}

// applyTemplate resolves req's template into s (image ref, env, version), writing
// the appropriate error response and returning false on any failure. The image
// and env come only from the trusted registry, never from the client.
func (h *ServerHandler) applyTemplate(c *gin.Context, req *createServerRequest, s *model.GameServer) bool {
	if h.templates == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "template registry is not configured"})
		return false
	}
	m, err := h.templates.ManifestParsed(c.Request.Context(), req.TemplateID)
	if errors.Is(err, registry.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
		return false
	}
	if err != nil {
		logger.FromContext(c).Error("fetch template manifest", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not reach the template registry"})
		return false
	}
	if m.EULANeeded && !req.EULAAccepted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this template requires accepting its EULA"})
		return false
	}
	env, imageRef, err := registry.Resolve(m, req.Answers)
	if errors.Is(err, registry.ErrInvalidAnswer) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if err != nil {
		logger.FromContext(c).Error("resolve template", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return false
	}

	tid := req.TemplateID
	s.TemplateID = &tid
	s.ImageRef = &imageRef
	s.Env = env
	// version is display/metadata for a template server (image_ref is authoritative);
	// the image tag is the most meaningful value to surface.
	s.Version = m.ImageTag
	return true
}

// List returns the authenticated user's servers.
func (h *ServerHandler) List(c *gin.Context) {
	servers, err := h.servers.ListByOwner(c.Request.Context(), middleware.UserIDFromContext(c))
	if err != nil {
		logger.FromContext(c).Error("list servers", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// Get returns a single owned server.
func (h *ServerHandler) Get(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s)
}

// Update edits the spec and/or desired state of an owned server.
func (h *ServerHandler) Update(c *gin.Context) {
	var req updateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	if req.Name != nil || req.Version != nil {
		name, version := s.Name, s.Version
		if req.Name != nil {
			name = *req.Name
		}
		if req.Version != nil {
			version = *req.Version
		}
		if err := h.servers.UpdateSpec(ctx, s.ID, name, version); err != nil {
			logger.FromContext(c).Error("update server spec", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}

	if req.DesiredState != nil {
		if err := h.servers.SetDesiredState(ctx, s.ID, *req.DesiredState); err != nil {
			logger.FromContext(c).Error("set desired state", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}

	updated, err := h.servers.GetByID(ctx, s.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete marks an owned server for teardown; the reconciler removes it.
func (h *ServerHandler) Delete(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	if err := h.servers.SetDesiredState(c.Request.Context(), s.ID, model.DesiredDeleted); err != nil {
		logger.FromContext(c).Error("mark server deleted", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "deleting"})
}

// RequestBackup flags an owned server for an on-demand world snapshot. It only
// records intent; the reconciler (the sole writer of compute side effects) takes
// the snapshot via the agent on its next tick.
func (h *ServerHandler) RequestBackup(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	if err := h.servers.RequestBackup(c.Request.Context(), s.ID); err != nil {
		logger.FromContext(c).Error("request backup", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "backup requested"})
}

// ownedOr404 loads the server in the URL and verifies the caller owns it.
// It writes a 404 for both missing and non-owned servers (no existence leak).
func (h *ServerHandler) ownedOr404(c *gin.Context) (*model.GameServer, bool) {
	s, err := h.servers.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			logger.FromContext(c).Error("get server", zap.Error(err))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return nil, false
	}
	if s.OwnerID != middleware.UserIDFromContext(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return nil, false
	}
	return s, true
}

func orDefault(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
