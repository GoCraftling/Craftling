package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeKey(t *testing.T) {
	cases := map[string]string{
		"srv-12ab":         "srv-12ab",
		"a.b_c-D9":         "a.b_c-D9",
		"../../etc/passwd": ".._.._etc_passwd",
		"with spaces":      "with_spaces",
		"slash/and:colon":  "slash_and_colon",
		"":                 "_",
		"\x00\x01":         "__",
	}
	for in, want := range cases {
		if got := sanitizeKey(in); got != want {
			t.Errorf("sanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
	// A sanitized key is always a single path segment — no separators escape.
	for in := range cases {
		if got := sanitizeKey(in); strings.ContainsRune(got, os.PathSeparator) {
			t.Errorf("sanitizeKey(%q) = %q contains a path separator", in, got)
		}
	}
}

func TestWorldDiskPath(t *testing.T) {
	c := &Config{DataDir: "/data/worlds"}
	got := c.worldDiskPath("srv-1")
	want := filepath.Join("/data/worlds", "srv-1", "world.ext4")
	if got != want {
		t.Errorf("worldDiskPath = %q, want %q", got, want)
	}
	// An adversarial id can't escape DataDir.
	esc := c.worldDiskPath("../../escape")
	if !strings.HasPrefix(esc, "/data/worlds/") {
		t.Errorf("worldDiskPath escaped DataDir: %q", esc)
	}
}

func TestPersistTarget(t *testing.T) {
	ok := map[string]bool{
		"/data":        true,
		"/minecraft":   true,
		"/srv/world":   true,
		"/":            false,
		"":             false,
		"relative/dir": false,
	}
	for in, wantOK := range ok {
		got, gotOK := persistTarget(in)
		if gotOK != wantOK {
			t.Errorf("persistTarget(%q) ok = %v, want %v", in, gotOK, wantOK)
		}
		if wantOK && got != in {
			t.Errorf("persistTarget(%q) = %q, want %q", in, got, in)
		}
	}
}

// TestEnsureWorldDiskReusesExisting verifies a second call leaves an existing
// disk untouched and never shells out to mkfs — the property that makes a world
// survive stop/start. The mkfs path is deliberately bogus: if the reuse branch
// failed to short-circuit, the format attempt would error and fail the test.
func TestEnsureWorldDiskReusesExisting(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "srv", "world.ext4")
	if err := os.MkdirAll(filepath.Dir(disk), 0o750); err != nil {
		t.Fatal(err)
	}
	const sentinel = "existing-world-bytes"
	if err := os.WriteFile(disk, []byte(sentinel), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ensureWorldDisk(disk, 64, "/nonexistent/mkfs.ext4"); err != nil {
		t.Fatalf("ensureWorldDisk on existing disk: %v", err)
	}
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Errorf("existing world disk was modified: got %q, want %q", got, sentinel)
	}
}

// TestEnsureWorldDiskMkfsFailureLeavesNoFile ensures a failed format doesn't
// publish a half-baked disk that the next provision would mistake for a real
// world.
func TestEnsureWorldDiskMkfsFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "srv", "world.ext4")

	if err := ensureWorldDisk(disk, 64, "/nonexistent/mkfs.ext4"); err == nil {
		t.Fatal("expected error from missing mkfs, got nil")
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Errorf("failed format left a file at %q (err=%v)", disk, err)
	}
	if _, err := os.Stat(disk + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("failed format left a .tmp file behind")
	}
}

// TestPersistenceConfigDefaults checks that enabling WorldPersistence fills the
// data-dir/size/mkfs defaults at New time. mkfs is pointed at an existing file
// so the check passes on a host (e.g. macOS CI) without e2fsprogs.
func TestPersistenceConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")

	rt, err := New(Config{
		KernelPath:       kernel,
		ImageStore:       testImageStore(t),
		ImageRef:         "example.invalid/mc:{version}",
		WorkDir:          work,
		WorldPersistence: true,
		MkfsExt4Path:     kernel, // any existing file satisfies resolveExecutable
	})
	if err != nil {
		t.Fatalf("New with persistence: %v", err)
	}
	if !rt.cfg.persistEnabled() {
		t.Error("persistEnabled() = false after enabling WorldPersistence")
	}
	if rt.cfg.WorldDiskMB != DefaultWorldDiskMB {
		t.Errorf("WorldDiskMB = %d, want default %d", rt.cfg.WorldDiskMB, DefaultWorldDiskMB)
	}
	wantData := filepath.Join(work, "worlds")
	if rt.cfg.DataDir != wantData {
		t.Errorf("DataDir = %q, want %q", rt.cfg.DataDir, wantData)
	}
	if fi, err := os.Stat(wantData); err != nil || !fi.IsDir() {
		t.Errorf("DataDir was not created: %v", err)
	}
}

// TestPersistenceConfigRejectsMissingMkfs checks New fails fast when world
// persistence is on but mkfs.ext4 can't be resolved.
func TestPersistenceConfigRejectsMissingMkfs(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{
		KernelPath:       kernel,
		ImageStore:       testImageStore(t),
		ImageRef:         "example.invalid/mc:{version}",
		WorkDir:          filepath.Join(dir, "work"),
		WorldPersistence: true,
		MkfsExt4Path:     "/nonexistent/mkfs.ext4",
	})
	if err == nil {
		t.Fatal("expected New to fail when mkfs.ext4 is missing")
	}
}
