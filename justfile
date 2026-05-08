# Postbox build commands

default:
    @just --list

build:
    go build ./...

test:
    go test ./...

test-v:
    go test -v ./...

test-race:
    go test -race ./...

test-cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out -o coverage.html

tidy:
    go mod tidy

fmt:
    go fmt ./...

lint:
    golangci-lint run ./...

vulncheck:
    govulncheck ./...

proto:
    buf generate

tools:
    mise install

setup: tools proto tidy build

clean:
    rm -f coverage.out coverage.html bin/postbox

install:
    go install .

serve *ARGS:
    go run . serve {{ARGS}}

release:
    #!/usr/bin/env bash
    set -euo pipefail
    version=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    parts=(${version//./ })
    patch=$((${parts[2]#v*} + 1))
    new_tag="${parts[0]}.${parts[1]}.${patch}"
    echo "Tagging $new_tag"
    git tag "$new_tag"
    git push origin "$new_tag"
