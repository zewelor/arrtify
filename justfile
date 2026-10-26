set dotenv-load := false

version := env("ARRTIFY_VERSION", "dev")

default:
    @just --list

fmt:
    gofmt -w cmd internal tests

fmt-check:
    test -z "$(gofmt -l cmd internal tests)"

vet:
    go vet ./...

test:
    go test -race ./...

e2e:
    go test -race -count=1 ./tests/e2e

transmission-up:
    bash scripts/transmission-lab.sh up

transmission-down:
    bash scripts/transmission-lab.sh down

vuln:
    go tool govulncheck ./...

build:
    CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/arrtify ./cmd/arrtify

image-build:
    docker buildx build --check .
    docker buildx build --load --pull --build-arg VERSION={{version}} --tag arrtify:test .

container-test: image-build
    ARRTIFY_TEST_IMAGE=arrtify:test go test -count=1 ./tests/e2e -run '^TestContainer'

fuzz:
    go test ./internal/cleaner -run '^$' -fuzz '^FuzzConfig$' -fuzztime 5s -parallel 2
    go test ./internal/cleaner -run '^$' -fuzz '^FuzzCategory$' -fuzztime 5s -parallel 2
    go test ./internal/cleaner -run '^$' -fuzz '^FuzzHostname$' -fuzztime 5s -parallel 2
    go test ./internal/cleaner -run '^$' -fuzz '^FuzzRemoveTarget$' -fuzztime 5s -parallel 2

# Both source and container checks use only synthetic local API data.
ci: fmt-check vet test fuzz vuln build container-test
