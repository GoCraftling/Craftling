package agent

import (
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Server exposes a Runtime over HTTP so the control plane can drive local VMs.
// It is the host-side half of the agent split: the reconciler's RemoteProvisioner
// calls these endpoints instead of touching compute in-process.
//
// These routes are unauthenticated for now; per-host auth / mTLS is hardened in
// P10, alongside the control plane's matching agent-auth seam.
type Server struct {
	rt Runtime
}

// NewServer constructs an agent Server over the given runtime.
func NewServer(rt Runtime) *Server { return &Server{rt: rt} }

// NewRouter builds the agent's Gin engine: shared middleware plus the VM
// lifecycle routes the control plane calls.
func NewRouter(rt Runtime, log *zap.Logger) *gin.Engine {
	s := NewServer(rt)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.RequestLogger(log))

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	vms := r.Group("/vms")
	{
		vms.POST("", s.Provision)
		vms.POST("/:id/start", s.Start)
		vms.POST("/:id/stop", s.Stop)
		vms.POST("/:id/snapshot", s.Snapshot)
		vms.DELETE("/:id", s.Deprovision)
		vms.GET("/:id", s.Status)
	}
	return r
}

// Provision creates and boots a VM for the requested spec.
func (s *Server) Provision(c *gin.Context) {
	var spec VMSpec
	if err := c.ShouldBindJSON(&spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	vm, err := s.rt.Provision(c.Request.Context(), spec)
	if err != nil {
		logger.FromContext(c).Error("provision vm", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, vm)
}

// Start boots an existing stopped VM.
func (s *Server) Start(c *gin.Context) {
	vm, err := s.rt.Start(c.Request.Context(), c.Param("id"))
	if errors.Is(err, ErrVMNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "vm not found"})
		return
	}
	if err != nil {
		logger.FromContext(c).Error("start vm", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, vm)
}

// Stop halts a VM without destroying it (idempotent).
func (s *Server) Stop(c *gin.Context) {
	if err := s.rt.Stop(c.Request.Context(), c.Param("id")); err != nil {
		logger.FromContext(c).Error("stop vm", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Snapshot takes an on-demand world snapshot of a running VM (P5c).
func (s *Server) Snapshot(c *gin.Context) {
	err := s.rt.Snapshot(c.Request.Context(), c.Param("id"))
	if errors.Is(err, ErrVMNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "vm not found"})
		return
	}
	if err != nil {
		logger.FromContext(c).Error("snapshot vm", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Deprovision destroys a VM (idempotent).
func (s *Server) Deprovision(c *gin.Context) {
	if err := s.rt.Deprovision(c.Request.Context(), c.Param("id")); err != nil {
		logger.FromContext(c).Error("deprovision vm", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Status reports a VM's observed state (StateMissing for an unknown id).
func (s *Server) Status(c *gin.Context) {
	vm, err := s.rt.Status(c.Request.Context(), c.Param("id"))
	if err != nil {
		logger.FromContext(c).Error("vm status", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, vm)
}
