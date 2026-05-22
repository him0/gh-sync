default:
    @just --list

build:
    mkdir -p .cache/go-build .cache/go-mod
    env -u GOROOT GOCACHE="$PWD/.cache/go-build" GOMODCACHE="$PWD/.cache/go-mod" go build -o gh-sync .

test:
    mkdir -p .cache/go-build .cache/go-mod
    env -u GOROOT GOCACHE="$PWD/.cache/go-build" GOMODCACHE="$PWD/.cache/go-mod" go test ./...
