.PHONY: build test test-linux test-race docs
build:
	CGO_ENABLED=0 go build -o bin/filegate ./cmd/filegate

test:
	CGO_ENABLED=0 go test ./...
	cd sdk/ts && bun run build && bun test test

test-linux:
	docker run --rm -v "$(CURDIR):/src" -v filegate-hardcut-gomod:/go/pkg/mod -v filegate-hardcut-gocache:/root/.cache/go-build -w /src -e CGO_ENABLED=0 golang:1.25 go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

docs:
	cd docs-site && bun run typecheck && bun run build
	diff -r skills/filegate docs-site/agent-skills/filegate
