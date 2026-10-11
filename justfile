# kutils — task runner. Run `just` to list recipes.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

# So `go install`ed tools (govulncheck, golangci-lint) are on PATH
export PATH := `go env GOPATH` + "/bin:" + env("PATH")

version := `cat VERSION | tr -d '[:space:]'`
ldflags := "-s -w -X main.version=" + version
tools := "kgc kgpv ksd kgn"

default:
    @just --list

# Install dev tooling (golangci-lint, govulncheck)
setup:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
    go install golang.org/x/vuln/cmd/govulncheck@latest
    go mod download

# Build all binaries for the host platform into ./bin
build:
    mkdir -p bin
    for tool in {{tools}}; do \
        go build -ldflags "{{ldflags}}" -o "bin/$tool" "./cmd/$tool"; \
    done

# Install all binaries into GOBIN
install:
    for tool in {{tools}}; do \
        go install -ldflags "{{ldflags}}" "./cmd/$tool"; \
    done

# Run tests
test:
    go test ./...

# Run go vet
vet:
    go vet ./...

# Lint (installs golangci-lint if needed)
lint: _install-golangci-lint
    golangci-lint run ./...

# Auto-fix lint issues and format code
fix: _install-golangci-lint
    golangci-lint run --fix ./...
    golangci-lint fmt ./...

# All-in-one: auto-fix lint + format, then test
fixup: fix test

# Format all Go code
fmt:
    go fmt ./...
    gofmt -s -w .

# Tidy go.mod and go.sum
tidy:
    go mod tidy

# Update all module dependencies to latest compatible versions
update-deps:
    go get -u ./...
    go mod tidy
    @git --no-pager diff --stat -- go.mod go.sum

# Update module dependencies to latest patch versions only
update-deps-patch:
    go get -u=patch ./...
    go mod tidy
    @git --no-pager diff --stat -- go.mod go.sum

# Update the Go toolchain directive and all dependencies to latest, then tidy
update:
    go get go@latest toolchain@latest
    go get -u ./...
    go mod tidy

# Scan for known Go vulnerabilities (same check as the release workflow)
cve: _install-govulncheck
    govulncheck ./...

alias vuln := cve
alias audit := cve

# Bump vulnerable modules to patched versions, tidy, and re-scan
fix-cves: _install-govulncheck
    #!/usr/bin/env bash
    set -euo pipefail

    echo "==> Scanning for known vulnerabilities..."
    set +e
    json=$(govulncheck -json ./... 2>/dev/null)
    status=$?
    set -e

    if [[ "$status" -eq 0 ]]; then
        echo "No vulnerabilities found."
        exit 0
    fi
    if [[ "$status" -ne 3 ]]; then
        echo "govulncheck failed (exit ${status})"
        exit "$status"
    fi

    mapfile -t updates < <(printf '%s\n' "$json" | jq -r '
        select(.finding != null)
        | select(.finding.fixed_version != null and .finding.fixed_version != "")
        | select(.finding.trace != null and (.finding.trace | length) > 0)
        | .finding.trace[0] as $root
        | select($root.module != null and $root.module != "" and $root.module != "stdlib")
        | "\($root.module)@\(.finding.fixed_version)"
    ' | sort -u)

    if [[ ${#updates[@]} -eq 0 ]]; then
        echo "Vulnerabilities found, but none can be auto-fixed (stdlib/toolchain, or no patched version)."
        echo "Show details with: just cve"
        echo "Try a broader bump with: just update-deps"
        govulncheck ./...
        exit 1
    fi

    echo "==> Bumping vulnerable modules:"
    printf '  %s\n' "${updates[@]}"
    go get "${updates[@]}"
    go mod tidy
    git --no-pager diff --stat -- go.mod go.sum

    echo "==> Re-scanning..."
    govulncheck ./...

# Run all checks: fmt, vet, lint, test, cve
check: fmt vet lint test cve

# Remove build artifacts
clean:
    rm -rf bin

# Cross-compile release binaries for all platforms into ./dist
release-build:
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf dist && mkdir -p dist
    platforms="linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64"
    for platform in $platforms; do
        GOOS=${platform%/*}
        GOARCH=${platform#*/}
        for tool in {{tools}}; do
            ext=""
            [ "$GOOS" = "windows" ] && ext=".exe"
            out="dist/${tool}_{{version}}_${GOOS}_${GOARCH}${ext}"
            echo "building $out"
            CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
                go build -ldflags "{{ldflags}}" -o "$out" "./cmd/$tool"
        done
    done

# Install govulncheck if it is not already on PATH
[private]
_install-govulncheck:
    @command -v govulncheck >/dev/null || go install golang.org/x/vuln/cmd/govulncheck@latest

# Install golangci-lint if it is not already on PATH
[private]
_install-golangci-lint:
    @command -v golangci-lint >/dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
