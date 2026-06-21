//go:build e2e

// Package e2e contains end-to-end tests that exercise the real HTTP server and
// the real agent control plane against a live PostgreSQL instance started via
// testcontainers.
//
// The control plane and a host agent are wired exactly as in production (P3):
// the agent dials the hub's gRPC listener and holds a persistent stream open,
// over which the hub pushes VM lifecycle commands. There is no inbound agent
// API — the open stream both delivers commands and proves the host's liveness.
//
// Run with: go test -tags e2e ./test/e2e/...  (requires Docker).
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/agentlink"
	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/db"
	"github.com/aarani/craftling-go/internal/handler"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/reconciler"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Shared test fixtures, set up in TestMain.
var (
	baseURL  string                     // address of the test HTTP server
	pool     *pgxpool.Pool              // direct DB access for integration assertions
	grpcAddr string                     // address of the agent-link gRPC hub
	hostRepo *repository.HostRepository // shared fleet inventory
)

// Capacity of the always-on placement host the agent registers in TestMain. It
// is sized large in cpu so every test's server can be placed (the scheduler
// needs a ready host), but its memory total is deliberately below the maximum
// allowed server spec so a create request can still exceed it and exercise the
// oversize path.
const (
	placementHostCPUs     = 64
	placementHostMemoryMB = 32768
)

// placementHostID is the stable id the always-on placement agent registers
// under; fakeRT is that agent's in-process runtime, queried directly by the
// agent-seam test to confirm a VM really landed on the agent. Both are set in
// TestMain.
const placementHostID = "22222222-2222-2222-2222-222222222222"

var fakeRT *agent.FakeRuntime

func TestMain(m *testing.M) {
	ctx := context.Background()

	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("craftling"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		os.Exit(1)
	}

	connStr, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	pool, err = db.Connect(ctx, connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect db: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}

	cfg := &config.Config{
		Env:        "test",
		JWTSecret:  "test-secret",
		AccessTTL:  time.Hour,
		RefreshTTL: time.Hour,
	}
	hostRepo = repository.NewHostRepository()
	gameServerRepo := repository.NewGameServerRepository(pool)

	// The hub is the control plane's end of the persistent agent connection:
	// agents dial this gRPC listener and hold a stream open, and the hub registers
	// hosts and pushes VM commands down the stream. This is the real P3 seam,
	// replacing the old outbound HTTP client the control plane used to dial. The
	// remote provisioner over it backs both the reconciler and the log endpoint.
	hub := agentlink.NewHub(hostRepo, gameServerRepo, zap.NewNop())
	prov := provisioner.NewRemote(hub)

	srv := httptest.NewServer(handler.NewRouter(cfg, zap.NewNop(), pool, hostRepo, prov))
	baseURL = srv.URL

	grpcSrv := grpc.NewServer()
	pb.RegisterAgentLinkServer(grpcSrv, hub)
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen for agent gRPC: %v\n", err)
		os.Exit(1)
	}
	grpcAddr = grpcLis.Addr().String()
	go func() { _ = grpcSrv.Serve(grpcLis) }()

	// Run the reconciler with a fast tick so lifecycle tests converge quickly. The
	// remote provisioner drives VMs by pushing commands down the host's stream via
	// the hub (the control plane never touches a runtime directly).
	recCtx, recCancel := context.WithCancel(ctx)
	sched := scheduler.New(hostRepo)
	billingRepo := repository.NewBillingRepository(pool)
	rec := reconciler.New(gameServerRepo, prov, sched, billingRepo, 30*time.Second, zap.NewNop())
	go rec.Run(recCtx, 100*time.Millisecond)

	// Bring up the always-on placement host as an in-process agent dialing the hub
	// over the real gRPC stream: a FakeRuntime that registers the host (so the
	// scheduler can place onto it) and answers VM commands. Its FakeRuntime is the
	// same instance the agent-seam test inspects directly. RunLink holds the stream
	// open for the suite's lifetime and reconnects on its own; recCancel stops it.
	fakeRT = agent.NewFakeRuntime("127.0.0.1")
	go agent.RunLink(recCtx, grpcAddr, fakeRT, agent.LinkInfo{
		ID:            placementHostID,
		Hostname:      "placement-host",
		Zone:          "zone-a",
		AgentVersion:  "test",
		CPUsTotal:     placementHostCPUs,
		MemoryMBTotal: placementHostMemoryMB,
	}, zap.NewNop())

	// Wait for the placement host's stream to register before running tests, so the
	// scheduler always has a ready host to place onto.
	if err := waitHostReady(ctx, placementHostID, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "placement host did not register: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	recCancel()
	grpcSrv.Stop()
	srv.Close()
	pool.Close()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

// startAgent dials the hub as an in-process agent with the given identity and a
// throwaway FakeRuntime, returning a stop func that disconnects it (closing the
// stream, which the hub observes as the host going down). RunLink reconnects on
// its own until stop() cancels its context.
func startAgent(t *testing.T, info agent.LinkInfo) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.RunLink(ctx, grpcAddr, agent.NewFakeRuntime("127.0.0.1"), info, zap.NewNop())
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitHostReady polls the fleet inventory until host id is registered and ready,
// or the timeout elapses.
func waitHostReady(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h, err := hostRepo.GetByID(ctx, id); err == nil && h.Status == model.HostReady {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("host %s not ready within %s", id, timeout)
}
