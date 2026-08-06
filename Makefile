.PHONY: docs-config test test-race test-short test-docker-runtime-state test-detector-linux test-detector-soak test-detector-chaos test-detector-btrfs-real test-detector-btrfs-real-docker test-versioning-btrfs-real-docker test-versioning-soak fuzz-smoke bench-go bench-http bench-compose bench-tree check

docs-config:
	go run ./cmd/filegate config schema --format markdown > docs-site/docs/en/reference/config.md

test:
	go test ./...

test-short:
	go test -short ./...

test-race:
	go test -race ./adapter/http ./domain ./infra/pebble

test-docker-runtime-state:
	./bench/scripts/run-docker-runtime-state-smoke.sh

test-detector-linux:
	./bench/scripts/run-detector-sync-tests.sh

test-detector-soak:
	./bench/scripts/run-detector-soak.sh

test-detector-chaos:
	./bench/scripts/run-detector-chaos.sh

test-detector-btrfs-real:
	./bench/scripts/run-detector-btrfs-real.sh

test-detector-btrfs-real-docker:
	./bench/scripts/run-detector-btrfs-real-docker.sh

test-versioning-btrfs-real-docker:
	./bench/scripts/run-versioning-btrfs-real-docker.sh

test-versioning-soak:
	./bench/scripts/run-versioning-soak.sh

fuzz-smoke:
	go test ./infra/fgbin -run '^$$' -fuzz '^FuzzDecodeEntity$$' -fuzztime=10s
	go test ./infra/fgbin -run '^$$' -fuzz '^FuzzDecodeChild$$' -fuzztime=10s
	go test ./adapter/http -run '^$$' -fuzz '^FuzzHashChunkFromReader$$' -fuzztime=10s
	go test ./adapter/http -run '^$$' -fuzz '^FuzzWriteChunkAtPath$$' -fuzztime=10s

bench-go:
	./bench/scripts/run-go-benches.sh

bench-http:
	./bench/scripts/run-http-bench.sh

bench-compose:
	./bench/scripts/run-http-bench-compose.sh

bench-tree:
	./bench/scripts/run-tree-bench.sh

check: test test-race test-detector-linux bench-go
