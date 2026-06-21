//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/minecraft"
	"github.com/aarani/craftling-go/internal/runspec"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// healthServiceAddr is the in-VM Minecraft service the health probe pings. The
// workload always listens on the standard port inside its own network namespace,
// so loopback is reachable regardless of the host's NAT dataplane.
const healthServiceAddr = "127.0.0.1:25565"

// healthProbeTimeout bounds each loopback probe (ping or rcon) the guest runs on
// the host's behalf, so a wedged workload can't stall the control connection.
const healthProbeTimeout = 4 * time.Second

// In-VM snapshot control server (P5c). It listens on AF_VSOCK and, on the
// host's request, makes the world disk safe to snapshot while the server keeps
// running: flush the workload (RCON), then fsfreeze the world-disk filesystem;
// on RESUME, thaw and re-enable saves. The host drives this over the VM's vsock
// UDS (see internal/agent/firecracker) and snapshots the disk while it is
// frozen.

// FIFREEZE / FITHAW freeze and thaw a filesystem given an fd on it. x/sys/unix
// at our pinned version doesn't export them; the values are the arch-generic
// _IOWR('X', 119/120, int) encodings, correct for the amd64/arm64 guests we
// run.
const (
	fsFreeze = 0xC0045877
	fsThaw   = 0xC0045878
)

// startSnapshotControl launches the vsock control server in a background
// goroutine. It is best-effort: a failure to listen is logged but never fatal,
// because a VM that can't be snapshotted should still run.
func startSnapshotControl(logger *zap.Logger, q *runspec.QuiesceConfig) {
	go func() {
		if err := serveSnapshotControl(logger, q); err != nil {
			logger.Warn("init: snapshot control server stopped", zap.Error(err))
		}
	}()
}

// serveSnapshotControl binds the vsock control port and serves connections one
// at a time — the host issues a single PREPARE/RESUME exchange per snapshot, so
// there is no need for concurrency, and serializing avoids overlapping freezes.
func serveSnapshotControl(logger *zap.Logger, q *runspec.QuiesceConfig) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: runspec.VsockControlPort}); err != nil {
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	logger.Info("init: snapshot control listening", zap.Int("vsock_port", runspec.VsockControlPort))

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("vsock accept: %w", err)
		}
		handleSnapshotConn(logger, os.NewFile(uintptr(nfd), "vsock-conn"), q)
	}
}

// handleSnapshotConn runs the PREPARE/RESUME exchange on one connection. The
// freeze is held for the lifetime of the connection: if the host crashes
// mid-snapshot and drops the connection, the deferred thaw still runs, so a
// dropped host never leaves the guest's disk wedged frozen.
func handleSnapshotConn(logger *zap.Logger, conn *os.File, q *runspec.QuiesceConfig) {
	defer func() { _ = conn.Close() }()

	freezeFd := -1
	defer func() {
		if freezeFd >= 0 {
			_ = unix.IoctlSetInt(freezeFd, fsThaw, 0)
			_ = unix.Close(freezeFd)
			logger.Warn("init: snapshot connection closed while frozen; thawed")
		}
	}()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		cmd := strings.TrimSpace(scanner.Text())
		switch cmd {
		case runspec.SnapPrepare:
			if freezeFd >= 0 {
				reply(conn, runspec.SnapErrPrefix+"already prepared")
				continue
			}
			fd, err := prepareSnapshot(logger, q)
			if err != nil {
				reply(conn, runspec.SnapErrPrefix+err.Error())
				continue
			}
			freezeFd = fd
			reply(conn, runspec.SnapOK)

		case runspec.SnapResume:
			if freezeFd >= 0 {
				_ = unix.IoctlSetInt(freezeFd, fsThaw, 0)
				_ = unix.Close(freezeFd)
				freezeFd = -1
			}
			resumeSnapshot(logger, q)
			reply(conn, runspec.SnapOK)

		case runspec.HealthProbe:
			h := probeHealth(logger, q)
			raw, err := json.Marshal(h)
			if err != nil {
				reply(conn, runspec.SnapErrPrefix+"marshal health: "+err.Error())
				continue
			}
			reply(conn, runspec.SnapOK+" "+string(raw))

		default:
			reply(conn, runspec.SnapErrPrefix+"unknown command")
		}
	}
}

// prepareSnapshot flushes the workload (best-effort RCON) and freezes the
// world-disk filesystem, returning the held fd that RESUME thaws. A freeze
// failure is fatal to the snapshot (returned as an error); an RCON failure is
// not — we still freeze for a filesystem-consistent snapshot rather than abort.
func prepareSnapshot(logger *zap.Logger, q *runspec.QuiesceConfig) (int, error) {
	if q != nil && q.RCONAddress != "" {
		if _, err := minecraft.RCONExec(q.RCONAddress, 0, "save-off", "save-all flush"); err != nil {
			logger.Warn("init: rcon flush before freeze failed; freezing anyway", zap.Error(err))
		}
	}

	fd, err := unix.Open(persistScratch, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open freeze target: %w", err)
	}
	if err := unix.IoctlSetInt(fd, fsFreeze, 0); err != nil {
		_ = unix.Close(fd)
		// EBUSY here means the fs is already frozen — surface it; the host
		// should RESUME to recover.
		return -1, fmt.Errorf("fsfreeze: %w", err)
	}
	logger.Info("init: world disk frozen for snapshot")
	return fd, nil
}

// resumeSnapshot re-enables workload saves after a thaw (best-effort).
func resumeSnapshot(logger *zap.Logger, q *runspec.QuiesceConfig) {
	if q != nil && q.RCONAddress != "" {
		if _, err := minecraft.RCONExec(q.RCONAddress, 0, "save-on"); err != nil {
			logger.Warn("init: rcon save-on after thaw failed", zap.Error(err))
		}
	}
	logger.Info("init: world disk thawed")
}

// probeHealth runs the workload's deep-health probe on the host's behalf (P7),
// over the guest's own loopback. It prefers a Server List Ping (no auth, reports
// version + player counts), then — when RCON is configured — corroborates the
// counts with an authenticated "list", whose tally is authoritative. A failure
// of either probe is not fatal: an unreachable workload simply yields
// Reachable=false, which the control plane records as "process not up yet".
func probeHealth(logger *zap.Logger, q *runspec.QuiesceConfig) runspec.Health {
	var h runspec.Health
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()

	if st, err := minecraft.Ping(ctx, healthServiceAddr, healthProbeTimeout); err != nil {
		logger.Debug("init: server list ping failed", zap.Error(err))
	} else {
		h.Reachable = true
		h.Source = "ping"
		h.Version = st.VersionName
		h.PlayersOnline = st.PlayersOnline
		h.PlayersMax = st.PlayersMax
	}

	if q != nil && q.RCONAddress != "" {
		if bodies, err := minecraft.RCONExec(q.RCONAddress, healthProbeTimeout, "list"); err != nil {
			logger.Debug("init: rcon list failed", zap.Error(err))
		} else if len(bodies) > 0 {
			if online, max, ok := minecraft.ParsePlayerList(bodies[0]); ok {
				h.Reachable = true
				h.Source = "rcon"
				h.PlayersOnline = online
				h.PlayersMax = max
			}
		}
	}
	return h
}

// reply writes a single newline-terminated protocol line.
func reply(conn *os.File, line string) {
	_, _ = conn.WriteString(line + "\n")
}
