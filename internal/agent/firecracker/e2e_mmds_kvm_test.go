//go:build kvm && linux

// This end-to-end test boots a real Firecracker microVM from a squashfs
// rootfs built by internal/image, with the Go init agent (cmd/init) as
// PID 1, and verifies the whole server-creation chain end to end:
//
//	Runtime.Provision
//	  → firecracker boots the squashfs rootfs (init=/.craftling/init)
//	  → host publishes the RunSpec into MMDS over a tap-backed NIC
//	  → in-VM init brings eth0 up, fetches the RunSpec from MMDS,
//	    and execs the workload with the env MMDS carried
//	  → the workload echoes a sentinel containing that env to the
//	    serial console, which firecracker captures to firecracker.log
//
// We assert the sentinel (with the per-run token we put in MMDS) shows
// up on the console — that single line can only appear if every link in
// the chain worked, including init reading the env from MMDS.
//
// It needs /dev/kvm, root (the MMDS tap needs CAP_NET_ADMIN), a
// firecracker binary, a vmlinux with squashfs + virtio-net, and network
// access to pull busybox. Gated behind the `kvm` build tag and skipped
// when prerequisites are missing:
//
//	FC_KERNEL=/path/vmlinux [FC_BINARY=/path/firecracker] \
//	  sudo -E go test -tags kvm -run TestKVMMMDSEnvRoundTrip -v \
//	  ./internal/agent/firecracker
package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/image"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// e2eImageRef is a tiny, public, multi-arch image with a /bin/sh — all
// the workload needs to echo its env. Pinned by tag; the test re-pins
// to the host-arch manifest digest before pulling.
const e2eImageRef = "docker.io/library/busybox:1.37"

func TestKVMMMDSEnvRoundTrip(t *testing.T) {
	binPath, kernel := requireFirecracker(t)

	// An image.Store + pinned busybox ref the runtime resolves and converts
	// through the normal Provision path.
	store, ref := e2eImage(t)

	rt, err := New(Config{
		BinaryPath: binPath,
		KernelPath: kernel,
		ImageStore: store,
		ImageRef:   ref,
		WorkDir:    t.TempDir(),
		// Read-only squashfs root, init at the path internal/image
		// injects. effectiveBootArgs appends the MMDS ip= directive
		// because the runtime always carries a run spec now.
		BootArgs:      "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath,
		AdvertiseHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A per-run token we hand the VM only through MMDS. Seeing it echoed
	// on the console proves init fetched the env from MMDS.
	token := randomToken(t)
	const marker = "CRAFTLING_E2E_MARKER"
	spec := &runspec.RunSpec{
		// busybox sh: print the marker with the MMDS-supplied env, then
		// idle so PID 1 stays alive until we deprovision (an exit would
		// make init power the VM off before we read the log).
		Cmd: []string{"/bin/sh", "-c",
			fmt.Sprintf("echo %s:$E2E_TOKEN; exec sleep 600", marker)},
		Env:        []string{"E2E_TOKEN=" + token},
		WorkingDir: "/",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	vm, err := rt.Provision(ctx, agent.VMSpec{
		ServerID: "e2e-mmds",
		Game:     "test",
		CPUs:     1,
		MemoryMB: 256,
		RunSpec:  spec,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer func() { _ = rt.Deprovision(context.Background(), vm.ID) }()

	if vm.State != agent.StateRunning {
		t.Fatalf("provisioned vm = %+v, want running", vm)
	}

	consoleLog := findConsoleLog(t, rt.cfg.WorkDir)
	want := marker + ":" + token
	if !waitForConsole(t, consoleLog, want, 90*time.Second) {
		t.Fatalf("did not observe %q on the guest console within timeout\n--- console ---\n%s",
			want, tailFile(consoleLog, 4096))
	}
	t.Logf("observed MMDS-delivered env on guest console: %s", want)
}

// requireFirecracker resolves the firecracker binary and kernel, or
// skips when the host can't run the e2e (non-root, no /dev/kvm, missing
// artifacts).
func requireFirecracker(t *testing.T) (binPath, kernel string) {
	t.Helper()
	if syscall.Geteuid() != 0 {
		t.Skip("firecracker mmds e2e requires root (MMDS tap needs CAP_NET_ADMIN)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	kernel = os.Getenv("FC_KERNEL")
	if kernel == "" {
		t.Skip("set FC_KERNEL to a vmlinux (with squashfs + virtio-net) to run")
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Fatalf("FC_KERNEL %q: %v", kernel, err)
	}
	binPath = os.Getenv("FC_BINARY")
	if binPath == "" {
		p, err := exec.LookPath("firecracker")
		if err != nil {
			t.Skip("firecracker not on PATH; set FC_BINARY to run")
		}
		binPath = p
	}
	return binPath, kernel
}

// e2eImage cross-compiles the init agent for the host arch and returns an
// image.Store (with the init binary wired in) plus the busybox reference
// pinned to the host-arch manifest digest. The runtime resolves and converts
// it on demand via Runtime.Provision → Store.Ensure, exercising the real
// rootfs-source path rather than a pre-staged image.
func e2eImage(t *testing.T) (*image.Store, string) {
	t.Helper()
	arch := runtime.GOARCH

	initBin := buildInitBinary(t, arch)
	store := &image.Store{CacheDir: t.TempDir()}
	switch arch {
	case "amd64":
		store.Init.LinuxAmd64 = initBin
	case "arm64":
		store.Init.LinuxArm64 = initBin
	default:
		t.Skipf("unsupported test arch %q", arch)
	}

	platform := &v1.Platform{OS: "linux", Architecture: arch}
	digest, err := crane.Digest(e2eImageRef, crane.WithPlatform(platform))
	if err != nil {
		t.Fatalf("resolve %s digest: %v", e2eImageRef, err)
	}
	return store, baseRef(e2eImageRef) + "@" + digest
}

// buildInitBinary statically cross-compiles cmd/init for arch.
func buildInitBinary(t *testing.T, arch string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "craftling-init")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/init")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build init agent: %v\n%s", err, b)
	}
	return out
}

// moduleRoot walks up from the test's working directory to the dir
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

// baseRef strips a tag or digest from an image reference, leaving the
// repository so a "@<digest>" pin can be appended.
func baseRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i]
	}
	return ref
}

// findConsoleLog locates the single per-VM firecracker.log under
// workDir (the runtime names each VM dir vm-<uuid>).
func findConsoleLog(t *testing.T, workDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(workDir, "vm-*", "firecracker.log"))
	if err != nil {
		t.Fatalf("glob console log: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one firecracker.log under %s, found %v", workDir, matches)
	}
	return matches[0]
}

// waitForConsole polls path until it contains want or the timeout fires.
func waitForConsole(t *testing.T, path, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// tailFile returns up to the last n bytes of path, for failure
// diagnostics.
func tailFile(path string, n int64) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read %s: %v)", path, err)
	}
	if int64(len(b)) > n {
		b = b[int64(len(b))-n:]
	}
	return string(b)
}

func randomToken(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}
