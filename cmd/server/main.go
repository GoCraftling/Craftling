package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarani/craftling-go/internal/agentlink"
	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/db"
	"github.com/aarani/craftling-go/internal/handler"
	"github.com/aarani/craftling-go/internal/leader"
	applogger "github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/reaper"
	"github.com/aarani/craftling-go/internal/reconciler"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/aarani/craftling-go/internal/seed"
	"github.com/aarani/craftling-go/internal/worldstore"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	// reapInterval is how often expired refresh tokens are purged.
	reapInterval = time.Hour
	// reconcileInterval is how often game servers are reconciled.
	reconcileInterval = 2 * time.Second
	// hostReapInterval is how often the fleet is swept for stale hosts.
	hostReapInterval = 10 * time.Second
	// hostHeartbeatTTL is how long a host may go without heartbeating before it
	// is marked down.
	hostHeartbeatTTL = 30 * time.Second
	// hostDeadTTL is how long a running server's host may stay unreachable before
	// the reconciler presumes its VM dead and reschedules. Longer than
	// hostHeartbeatTTL so a brief agent restart (which marks the host down and
	// back up within seconds) isn't mistaken for a permanent loss.
	hostDeadTTL = 90 * time.Second
	// worldGCInterval is how often the durable world store is swept for
	// snapshots belonging to no live server (P5b).
	worldGCInterval = time.Hour
	// reconcileBackoffBase and reconcileBackoffMax bound the exponential retry
	// backoff applied to a server whose reconcile step fails (P8a): the first
	// failure waits the base, each subsequent failure doubles it, capped at the
	// max. This spaces out retries on a persistent fault instead of re-running the
	// failing step every reconcileInterval.
	reconcileBackoffBase = 5 * time.Second
	reconcileBackoffMax  = 10 * time.Minute
	// leaderLockKey is the Postgres advisory-lock key the control-plane replicas
	// contend on to elect the single leader that runs the reconciler and reapers
	// (P8d). A fixed, distinctive constant — the control plane uses no other
	// advisory locks, so collision is not a concern.
	leaderLockKey = 0x6372_6166_746c_30_38 // "craftl08"
	// leaderRetryInterval is how often a follower replica re-campaigns for
	// leadership, and so bounds how quickly one takes over after a leader steps down.
	leaderRetryInterval = 5 * time.Second
)

func main() {
	cfg := config.Load()

	zlog, err := applogger.New(cfg.Env)
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer func() { _ = zlog.Sync() }()

	// Connect to Postgres and apply the schema.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Connect(dbCtx, cfg.DatabaseURL)
	if err != nil {
		dbCancel()
		zlog.Fatal("connect to database", zap.Error(err))
	}
	defer pool.Close()
	if err := db.Migrate(dbCtx, pool); err != nil {
		dbCancel()
		zlog.Fatal("run migrations", zap.Error(err))
	}

	// Optionally bootstrap the admin account.
	if created, err := seed.Admin(dbCtx, repository.NewUserRepository(pool), cfg.AdminEmail, cfg.AdminPassword); err != nil {
		dbCancel()
		zlog.Fatal("seed admin", zap.Error(err))
	} else if created {
		zlog.Info("seeded admin user", zap.String("email", cfg.AdminEmail))
	}
	dbCancel()

	// The fleet inventory lives in process memory (P1). It is shared between the
	// agent link hub (which registers/heartbeats hosts as their streams come and
	// go) and the host reaper.
	hostRepo := repository.NewHostRepository()
	gameServerRepo := repository.NewGameServerRepository(pool)

	// The hub is the control plane's end of the persistent agent connection:
	// agents dial the gRPC listener and hold a stream open, and the hub pushes VM
	// commands down it. It registers hosts (reconstructing committed capacity from
	// the durable server records) and tracks liveness off the stream. Built here
	// (before the router and reconciler) so both the on-demand log endpoint and
	// the reconciler drive agents through the same remote provisioner.
	hub := agentlink.NewHub(hostRepo, gameServerRepo, zlog)
	prov := provisioner.NewRemote(hub)

	router := handler.NewRouter(cfg, zlog, pool, hostRepo, prov)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	grpcSrv := grpc.NewServer()
	pb.RegisterAgentLinkServer(grpcSrv, hub)
	grpcLis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		zlog.Fatal("listen for agent gRPC", zap.Error(err))
	}

	// ctx is cancelled on the first interrupt/terminate signal, which both
	// stops the reaper and triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The scheduler places unassigned servers onto ready hosts from the same fleet
	// inventory the agent endpoints and host reaper share; the remote provisioner
	// then drives the VM by calling the assigned host's agent (the control plane
	// never touches KVM itself).
	sched := scheduler.New(hostRepo)
	// The billing meter records each server's running intervals for pay-as-you-go
	// billing (P9); the reconciler opens/closes them on state transitions. The
	// player repo feeds each running server's whitelist over RCON; the fence repo
	// records VMs orphaned on unreachable hosts for the reconciler to reclaim (P8b).
	billingRepo := repository.NewBillingRepository(pool)
	playerRepo := repository.NewPlayerRepository(pool)
	fenceRepo := repository.NewFenceRepository(pool)
	refreshRepo := repository.NewRefreshTokenRepository(pool)
	rec := reconciler.New(gameServerRepo, prov, sched, billingRepo, playerRepo, fenceRepo, hostDeadTTL, reconcileBackoffBase, reconcileBackoffMax, zlog)

	// A durable world store, if configured, lets the reconciler GC snapshots no
	// live server claims (orphans from a host that died before its server was
	// deprovisioned). The control plane sees the same store the agents do.
	storeCtx, storeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	worldStore, err := worldstore.FromConfig(storeCtx, cfg.Agent.Firecracker, zlog)
	storeCancel()
	if err != nil {
		zlog.Warn("world store unavailable; world GC disabled", zap.Error(err))
	}

	// runLeaderWork starts the single-writer background goroutines — the reconciler
	// and the reapers — bound to leaderCtx. These must run on exactly one replica:
	// two reconcilers would race to drive the same VMs. Leadership (P8d) gates them
	// behind a Postgres advisory lock; every replica still serves the API and the
	// agent gRPC hub, so agents connect anywhere while only the leader mutates
	// compute. leaderCtx is cancelled when this replica steps down, stopping them.
	runLeaderWork := func(leaderCtx context.Context) {
		go reaper.RefreshTokens(leaderCtx, zlog, refreshRepo, reapInterval)
		go reaper.Hosts(leaderCtx, zlog, hostRepo, hostReapInterval, hostHeartbeatTTL)
		go rec.Run(leaderCtx, reconcileInterval)
		if worldStore != nil {
			go reaper.Worlds(leaderCtx, zlog, worldStore, gameServerRepo, worldGCInterval)
		}
	}
	if cfg.LeaderElection {
		go leader.Campaign(ctx, pool, leaderLockKey, leaderRetryInterval, zlog, runLeaderWork)
	} else {
		// Single-replica deployment opted out of election: run the work directly.
		runLeaderWork(ctx)
	}

	// Serve the agent gRPC link alongside the HTTP API.
	go func() {
		zlog.Info("agent gRPC listening", zap.String("port", cfg.GRPCPort))
		if err := grpcSrv.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			zlog.Fatal("agent gRPC serve failed", zap.Error(err))
		}
	}()

	// Start the server in a goroutine so it doesn't block graceful shutdown handling.
	go func() {
		zlog.Info("server listening", zap.String("port", cfg.Port), zap.String("env", cfg.Env))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			zlog.Fatal("listen failed", zap.Error(err))
		}
	}()

	<-ctx.Done()
	stop() // restore default signal handling so a second signal force-quits
	zlog.Info("shutting down server...")

	grpcSrv.GracefulStop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		zlog.Fatal("forced shutdown", zap.Error(err))
	}

	zlog.Info("server exited")
}
