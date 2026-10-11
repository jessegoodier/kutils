# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

kutils is a small collection of Kubernetes CLI utilities written in Go, one binary per tool:

- `kgc` (`cmd/kgc`) — `kubectl get pods` style view per container, color-coded status and last restart reason; supports watch mode.
- `kgpv` (`cmd/kgpv`) — lists PersistentVolumes with bound PVC, pod, zone, storageClass and size; supports YAML/CSV output.
- `ksd` (`cmd/ksd`) — prints a Secret like `kubectl get secret -oyaml` with values base64-decoded (certificate details, `-k`, `-o`, fzf picker); the shell completion scripts call the hidden `ksd __complete WORDS...` subcommand for dynamic secret/namespace/key names.
- `kgn` (`cmd/kgn`) — node CPU/memory/GPU allocatable, requests (computed from pods like `kubectl describe node`) and metrics-server usage, color-coded health (conditions, taints, cordon, kubelet skew, unhealthy GPUs); `-d` explains why a node is unhealthy, `-o wide` adds split columns and cloud metadata. Uses the same `__complete` pattern as `ksd` for node names.

Each tool is a single self-contained `main.go` (plus `main_test.go`) in its own `cmd/<tool>/` directory. There is no shared internal package; helpers are intentionally duplicated per tool, so a change to common behavior (kubeconfig loading, shell completions, version handling) usually needs to be made in both. `kgpv` works mostly on unstructured maps (`getMapValue`, `getStringValue`, …) while `kgc` uses typed `client-go` objects.

## Commands

Tasks are run with [`just`](https://github.com/casey/just) (see `justfile`; `just` lists recipes):

- `just setup` — install golangci-lint and govulncheck
- `just build` — build all binaries into `./bin` (injects version from `VERSION` via ldflags)
- `just test` — `go test ./...`; single test: `go test ./cmd/kgc -run TestName`
- `just lint` / `just fix` / `just fmt` — golangci-lint (v2), auto-fix, gofmt
- `just check` — fmt, vet, lint, test, govulncheck
- `just cve` / `just fix-cves` — scan for / auto-bump vulnerable modules
- `just update-deps` — update dependencies and tidy
- `just release-build` — cross-compile all platforms into `./dist`

## Versioning and release

- The version is a `var version = "dev"` in each `main.go`, overridden with `-X main.version=...`; when still `dev`, `init()` falls back to the module version from `debug.ReadBuildInfo` (so `go install ...@vX` reports correctly).
- Releases are driven by `.github/workflows/release.yml`: it bumps `VERSION`, commits `chore: release vX.Y.Z`, tags, builds binaries, and publishes (Homebrew tap: `jessegoodier/kutils`). Don't bump `VERSION` or tag manually.
- The Go version in `go.mod` is bumped deliberately (CI uses `go-version-file: go.mod`).
