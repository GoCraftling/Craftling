//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
)

// TestAgentSeam verifies the control plane drives the VM on the host agent
// across the gRPC stream seam (P3): a created server's VM actually exists and
// runs on the in-process agent, and deleting the server tears that VM down.
func TestAgentSeam(t *testing.T) {
	user := registerUser(t, "seam-user@example.com", "hunter2pass")
	tok := user.AccessToken

	id := createServerID(t, tok, "seam-world")
	running := waitForStatus(t, tok, id, "running")

	vmID, _ := running["vm_id"].(string)
	if vmID == "" {
		t.Fatalf("running server has no vm_id: %v", running)
	}

	// The agent must report this VM as running, and tagged with the server id.
	vm := agentVM(t, vmID)
	if vm.State != agent.StateRunning {
		t.Errorf("agent vm state = %v, want running", vm.State)
	}
	if vm.ServerID != id {
		t.Errorf("agent vm server_id = %v, want %s", vm.ServerID, id)
	}

	// Deleting the server deprovisions the VM on the agent.
	resp, _ := doJSON(t, http.MethodDelete, "/api/v1/servers/"+id, tok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", resp.StatusCode)
	}
	waitForGone(t, tok, id)

	if vm := agentVM(t, vmID); vm.State != agent.StateMissing {
		t.Errorf("after delete, agent vm state = %v, want missing", vm.State)
	}
}

// agentVM fetches a VM's record directly from the placement host's in-process
// runtime — the same FakeRuntime the control plane drives over the stream — so
// the test can confirm the command actually landed on the agent.
func agentVM(t *testing.T, vmID string) *agent.VM {
	t.Helper()
	vm, err := fakeRT.Status(context.Background(), vmID)
	if err != nil {
		t.Fatalf("agent vm status: %v", err)
	}
	return vm
}
