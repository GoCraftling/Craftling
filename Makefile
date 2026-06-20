.PHONY: run run-agent build build-agent test test-e2e test-kvm test-bpf tidy fmt bpf-generate proto-generate

run:
	go run ./cmd/server

# Run a host agent (P3). The agent dials the control plane's gRPC AgentLink and
# holds a stream open; override the target via env, e.g.
# MODE=agent CONTROL_PLANE_GRPC_ADDR=localhost:8090 AGENT_RUNTIME=fake.
run-agent:
	MODE=agent go run ./cmd/agent

build:
	go build -o bin/server ./cmd/server

build-agent:
	go build -o bin/agent ./cmd/agent

test:
	go test ./...

# Requires Docker — spins up Postgres via testcontainers.
test-e2e:
	go test -tags e2e -count=1 ./test/e2e/...

# Boots real Firecracker microVMs (P4). Requires /dev/kvm + host artifacts; run
# on a KVM host, not the default CI lane:
#   FC_KERNEL=... FC_IMAGE_DIR=... FC_DEFAULT_IMAGE=base.ext4 make test-kvm
test-kvm:
	go test -tags kvm -count=1 -v ./internal/agent/firecracker/...

# Loads the real eBPF NAT/tapfilter objects into the running kernel, attaches
# them to veth/tap devices, and drives packets through them. Needs root (CAP_BPF
# + CAP_NET_ADMIN), `ip`, and a kernel >= 6.6 (TCX + nf_conntrack kfuncs); the
# tests skip cleanly otherwise. No firecracker/KVM required — `sudo make test-bpf`.
test-bpf:
	sudo env "PATH=$$PATH" go test -tags bpf -count=1 -v ./internal/agent/firecracker/...

# Regenerate the eBPF bindings + compiled objects from the .c sources. This is a
# MAINTAINER step, run on a Linux >=6.6 host with clang, libbpf headers, and
# bpftool — NOT part of the normal build. The outputs (bpf/*_bpfel.go,
# bpf/*_bpfel.o, bpf/vmlinux.h) are committed so `go build`/CI need only the Go
# toolchain. CO-RE makes the single bpfel object portable across kernels at load
# time, so generate once (against the host's BTF) and commit. Run after editing
# any bpf/*.c, then `git add` the regenerated artifacts.
BPF_DIR := internal/agent/firecracker/bpf
bpf-generate:
	@command -v clang >/dev/null   || { echo "need clang (compile eBPF)"; exit 1; }
	@command -v bpftool >/dev/null || { echo "need bpftool (dump vmlinux.h)"; exit 1; }
	@test -r /sys/kernel/btf/vmlinux || { echo "need kernel BTF (CONFIG_DEBUG_INFO_BTF=y)"; exit 1; }
	bpftool btf dump file /sys/kernel/btf/vmlinux format c > $(BPF_DIR)/vmlinux.h
	go generate ./$(BPF_DIR)/...

# Regenerate the gRPC AgentLink stubs from proto/agentlink/agentlink.proto. This
# is a MAINTAINER step; the outputs (internal/agentlink/pb/*.pb.go) are committed
# so `go build`/CI need only the Go toolchain. Run after editing the .proto, then
# `git add` the regenerated files. Requires protoc plus the Go plugins:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
proto-generate:
	@command -v protoc >/dev/null || { echo "need protoc (protobuf-compiler)"; exit 1; }
	protoc \
		--go_out=. --go_opt=module=github.com/aarani/craftling-go \
		--go-grpc_out=. --go-grpc_opt=module=github.com/aarani/craftling-go \
		proto/agentlink/agentlink.proto

tidy:
	go mod tidy

fmt:
	go fmt ./...
