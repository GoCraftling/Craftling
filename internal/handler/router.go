package handler

import (
	"net/http"
	"time"

	"github.com/aarani/craftling-go/internal/auth"
	"github.com/aarani/craftling-go/internal/billing"
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
func NewRouter(cfg *config.Config, log *zap.Logger, pool *pgxpool.Pool, hostRepo *repository.HostRepository, logs LogProvider) *gin.Engine {
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	jwtManager := auth.NewManager(cfg.JWTSecret, cfg.AccessTTL)
	userRepo := repository.NewUserRepository(pool)
	refreshRepo := repository.NewRefreshTokenRepository(pool)
	gameServerRepo := repository.NewGameServerRepository(pool)
	// The quota repository carries the system default quota; a user without an
	// admin-set override is held to it (P9).
	quotaRepo := repository.NewQuotaRepository(pool, model.UserQuota{
		MaxServers:  cfg.Quota.MaxServers,
		MaxCPUs:     cfg.Quota.MaxCPUs,
		MaxMemoryMB: cfg.Quota.MaxMemoryMB,
	})
	billingRepo := repository.NewBillingRepository(pool)
	authHandler := NewAuthHandler(userRepo, refreshRepo, jwtManager, cfg.RefreshTTL)
	adminHandler := NewAdminHandler(userRepo, gameServerRepo, hostRepo, logs)
	quotaHandler := NewQuotaHandler(quotaRepo, gameServerRepo, userRepo)
	playerHandler := NewPlayerHandler(repository.NewPlayerRepository(pool), gameServerRepo)
	billingHandler := NewBillingHandler(billingRepo, userRepo, billing.Rates{
		CPUHour:      cfg.Billing.CPUHour,
		MemoryGBHour: cfg.Billing.MemoryGBHour,
		Currency:     cfg.Billing.Currency,
	})
	// One registry client backs both the template browse endpoints and the
	// server-side template resolution the create handler performs.
	registryClient := registry.New(cfg.TemplateIndexURL, &http.Client{Timeout: 10 * time.Second})
	// The scheduler is stateless over the shared in-memory host inventory, so the
	// handler builds its own; the reconciler builds another over the same store.
	serverHandler := NewServerHandler(gameServerRepo, scheduler.New(hostRepo), quotaRepo, registryClient, logs)
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
			// A user's own effective quota and current usage (P9).
			protected.GET("/quota", quotaHandler.Mine)
			// A user's own pay-as-you-go bill for the current period (P9).
			protected.GET("/billing", billingHandler.Mine)
		}

		// Game server CRUD (owner-scoped).
		servers := api.Group("/servers")
		servers.Use(middleware.Auth(jwtManager))
		{
			servers.POST("", serverHandler.Create)
			servers.GET("", serverHandler.List)
			servers.GET("/:id", serverHandler.Get)
			servers.GET("/:id/logs", serverHandler.Logs)
			servers.PATCH("/:id", serverHandler.Update)
			servers.POST("/:id/snapshot", serverHandler.RequestBackup)
			servers.DELETE("/:id", serverHandler.Delete)
		}

		// Whitelist roster (owner-scoped): players the caller may grant onto their
		// servers.
		players := api.Group("/players")
		players.Use(middleware.Auth(jwtManager))
		{
			players.POST("", playerHandler.Create)
			players.GET("", playerHandler.List)
			players.GET("/:id", playerHandler.Get)
			players.PATCH("/:id", playerHandler.Update)
			players.DELETE("/:id", playerHandler.Delete)
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
			admin.GET("/servers/:id/logs", adminHandler.ServerLogs)
			admin.GET("/hosts", adminHandler.ListHosts)
			// Per-user quota view/set (P9). Setting an empty body or an absent
			// override reverts the user to the system default via DELETE.
			admin.GET("/users/:id/quota", quotaHandler.GetForUser)
			admin.PUT("/users/:id/quota", quotaHandler.SetForUser)
			admin.DELETE("/users/:id/quota", quotaHandler.DeleteForUser)
			// Per-user pay-as-you-go bill (P9).
			admin.GET("/users/:id/billing", billingHandler.GetForUser)
		}

		// Hosts no longer register/heartbeat over HTTP: each agent holds a
		// persistent gRPC stream to the control plane (see internal/agentlink),
		// which both delivers commands and serves as the host's liveness signal.
	}

	return r
}
