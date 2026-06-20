package firecracker

import "testing"

// TestProvisionRef verifies the image chosen for a provision: an explicit
// per-server ImageRef (a marketplace template's authoritative image) always wins;
// otherwise the host's configured, version-templated default is used.
func TestProvisionRef(t *testing.T) {
	cfg := &Config{ImageRef: "registry.example/mc:{version}", DefaultImageRef: "registry.example/mc:latest"}

	// An explicit spec image ref overrides the configured default entirely.
	if ref, err := cfg.provisionRef("custom/modpack:v9", "java21"); err != nil || ref != "custom/modpack:v9" {
		t.Fatalf("provisionRef(custom) = %q, %v; want custom/modpack:v9", ref, err)
	}

	// No spec image ref falls back to the version-templated default.
	if ref, err := cfg.provisionRef("", "1.20.4"); err != nil || ref != "registry.example/mc:1.20.4" {
		t.Fatalf("provisionRef(templated) = %q, %v; want registry.example/mc:1.20.4", ref, err)
	}

	// No spec image ref and no version uses DefaultImageRef.
	if ref, err := cfg.provisionRef("", ""); err != nil || ref != "registry.example/mc:latest" {
		t.Fatalf("provisionRef(default) = %q, %v; want registry.example/mc:latest", ref, err)
	}
}
