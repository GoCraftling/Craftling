// Command agent is the host-side worker (P3). It dials the control plane and
// holds a persistent gRPC stream open over which the control plane pushes VM
// lifecycle commands (provision/start/stop/deprovision); the agent runs them
// against its local Runtime and answers on the same stream. It has no inbound
// API — the open stream both delivers commands and proves the host's liveness.
//
// It ships with the in-memory FakeRuntime; a real Firecracker driver (P4) slots
// in behind the same Runtime interface without changing this wiring.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/agent/firecracker"
	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/image"
	applogger "github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/worldstore"
	"go.uber.org/zap"
)

func main() {
	cfg := config.Load()

	zlog, err := applogger.New(cfg.Env)
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer func() { _ = zlog.Sync() }()

	// The runtime that actually runs VMs, driven by commands off the link.
	rt, err := newRuntime(cfg, zlog)
	if err != nil {
		zlog.Fatal("init runtime", zap.Error(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Hold a persistent connection to the control plane for the agent's lifetime.
	// RunLink blocks until ctx is cancelled, reconnecting on its own if the stream
	// drops, so this is the agent's main loop.
	zlog.Info("connecting to control plane", zap.String("addr", cfg.Agent.ControlPlaneGRPCAddr))
	agent.RunLink(ctx, cfg.Agent.ControlPlaneGRPCAddr, rt, agent.LinkInfo{
		ID:            cfg.Agent.ID,
		Hostname:      cfg.Agent.Hostname,
		Zone:          cfg.Agent.Zone,
		AgentVersion:  cfg.Agent.Version,
		CPUsTotal:     cfg.Agent.CPUsTotal,
		MemoryMBTotal: cfg.Agent.MemoryMBTotal,
	}, zlog)

	zlog.Info("agent exited")
}

// newRuntime selects the VM backend by config: the in-memory FakeRuntime for
// local/dev runs, or the real Firecracker driver (P4) on KVM hosts.
func newRuntime(cfg *config.Config, log *zap.Logger) (agent.Runtime, error) {
	switch cfg.Agent.Runtime {
	case config.RuntimeFirecracker:
		log.Info("using firecracker runtime",
			zap.String("kernel", cfg.Agent.Firecracker.KernelPath),
			zap.String("image_ref", cfg.Agent.Firecracker.ImageRef),
			zap.String("image_cache_dir", imageCacheDir(cfg.Agent.Firecracker)))
		storeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		worldStore, err := worldstore.FromConfig(storeCtx, cfg.Agent.Firecracker, log)
		cancel()
		if err != nil {
			return nil, err
		}
		imageStore := &image.Store{
			CacheDir: imageCacheDir(cfg.Agent.Firecracker),
			Init: image.InitBinaries{
				LinuxAmd64: cfg.Agent.Firecracker.InitBinAmd64,
				LinuxArm64: cfg.Agent.Firecracker.InitBinArm64,
			},
		}
		return firecracker.New(firecracker.Config{
			BinaryPath:       cfg.Agent.Firecracker.BinaryPath,
			KernelPath:       cfg.Agent.Firecracker.KernelPath,
			ImageStore:       imageStore,
			ImageRef:         cfg.Agent.Firecracker.ImageRef,
			DefaultImageRef:  cfg.Agent.Firecracker.DefaultImageRef,
			WorkDir:          cfg.Agent.Firecracker.WorkDir,
			UplinkDevice:     cfg.Agent.Firecracker.UplinkDevice,
			VMSubnet:         cfg.Agent.Firecracker.VMSubnet,
			GatewayIP:        cfg.Agent.Firecracker.GatewayIP,
			HostPortMin:      uint16(cfg.Agent.Firecracker.HostPortMin),
			HostPortMax:      uint16(cfg.Agent.Firecracker.HostPortMax),
			AdvertiseHost:    cfg.Agent.AdvertiseHost,
			CPUsTotal:        cfg.Agent.CPUsTotal,
			MemoryMBTotal:    cfg.Agent.MemoryMBTotal,
			WorldPersistence: cfg.Agent.Firecracker.WorldPersistence,
			DataDir:          cfg.Agent.Firecracker.DataDir,
			WorldDiskMB:      cfg.Agent.Firecracker.WorldDiskMB,
			MkfsExt4Path:     cfg.Agent.Firecracker.MkfsExt4Path,
			WorldStore:       worldStore,
			SnapshotInterval: cfg.Agent.Firecracker.SnapshotInterval,
			RCONPort:         cfg.Agent.Firecracker.RCONPort,
			RCONPassword:     cfg.Agent.Firecracker.RCONPassword,
			Logger:           log,
		})
	case config.RuntimeFake, "":
		log.Info("using fake runtime")
		return agent.NewFakeRuntime(cfg.Agent.AdvertiseHost,
			agent.WithCapacity(cfg.Agent.CPUsTotal, cfg.Agent.MemoryMBTotal)), nil
	default:
		return nil, fmt.Errorf("unknown agent runtime %q", cfg.Agent.Runtime)
	}
}

// imageCacheDir resolves where converted squashfs rootfs files are cached: the
// configured CacheDir, else an "images" dir alongside the per-VM WorkDir (the
// same default the driver uses for WorkDir), so a single FC_WORK_DIR is enough
// to keep all driver state under one tree.
func imageCacheDir(fc config.FirecrackerConfig) string {
	if fc.CacheDir != "" {
		return fc.CacheDir
	}
	workDir := fc.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "craftling-fc")
	}
	return filepath.Join(workDir, "images")
}
