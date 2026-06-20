package handler

import (
	"net/http"
	"time"

	"github.com/aarani/craftling-go/internal/auth"
	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// NewRouter builds the Gin engine with middleware and routes wired up. The host
// inventory is passed in (rather than built here) so the host reaper can share
// the same in-memory store.
func NewRouter(cfg *config.Config, log *zap.Logger, pool *pgxpool.Pool, hostRepo *repository.HostRepository) *gin.Engine {
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	jwtManager := auth.NewManager(cfg.JWTSecret, cfg.AccessTTL)
	userRepo := repository.NewUserRepository(pool)
	refreshRepo := repository.NewRefreshTokenRepository(pool)
	gameServerRepo := repository.NewGameServerRepository(pool)
	authHandler := NewAuthHandler(userRepo, refreshRepo, jwtManager, cfg.RefreshTTL)
	adminHandler := NewAdminHandler(userRepo, gameServerRepo, hostRepo)
	// One registry client backs both the template browse endpoints and the
	// server-side template resolution the create handler performs.
	registryClient := registry.New(cfg.TemplateIndexURL, &http.Client{Timeout: 10 * time.Second})
	// The scheduler is stateless over the shared in-memory host inventory, so the
	// handler builds its own; the reconciler builds another over the same store.
	serverHandler := NewServerHandler(gameServerRepo, scheduler.New(hostRepo), registryClient)
	templateHandler := NewTemplateHandler(registryClient)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.RequestLogger(log))

	r.GET("/healthz", Health)

	api := r.Group("/api/v1")
	{
		api.GET("/ping", Ping)
		api.POST("/auth/register", authHandler.Register)
		api.POST("/auth/login", authHandler.Login)
		api.POST("/auth/refresh", authHandler.Refresh)
		api.POST("/auth/logout", authHandler.Logout)

		// Routes requiring a valid access token.
		protected := api.Group("")
		protected.Use(middleware.Auth(jwtManager))
		{
			protected.GET("/me", authHandler.Me)
		}

		// Game server CRUD (owner-scoped).
		servers := api.Group("/servers")
		servers.Use(middleware.Auth(jwtManager))
		{
			servers.POST("", serverHandler.Create)
			servers.GET("", serverHandler.List)
			servers.GET("/:id", serverHandler.Get)
			servers.PATCH("/:id", serverHandler.Update)
			servers.POST("/:id/snapshot", serverHandler.RequestBackup)
			servers.DELETE("/:id", serverHandler.Delete)
		}

		// Template registry / marketplace (owner- and operator-accessible).
		templates := api.Group("/templates")
		templates.Use(middleware.Auth(jwtManager))
		{
			templates.GET("", templateHandler.List)
			templates.GET("/:id", templateHandler.Get)
		}

		// Admin-only routes.
		admin := api.Group("/admin")
		admin.Use(middleware.Auth(jwtManager), middleware.RequireRole(model.RoleAdmin))
		{
			admin.GET("/users", adminHandler.ListUsers)
			admin.GET("/servers", adminHandler.ListServers)
			admin.GET("/hosts", adminHandler.ListHosts)
		}

		// Hosts no longer register/heartbeat over HTTP: each agent holds a
		// persistent gRPC stream to the control plane (see internal/agentlink),
		// which both delivers commands and serves as the host's liveness signal.
	}

	return r
}
