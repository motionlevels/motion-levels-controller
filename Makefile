SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
NATIVE_OUTPUT_DIR ?= dist
NATIVE_METADATA := $(NATIVE_OUTPUT_DIR)/motion-levels-controller-linux-amd64-$(SOURCE_REVISION).metadata.json

.PHONY: check test test-stress vet build web-check web-e2e fuzz-short benchmark native-build native-verify

check:
	sh scripts/check.sh

test:
	go test -race ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/motion-levels-controller

web-check:
	node scripts/check-web.mjs

web-e2e:
	python3 scripts/check-web-e2e.py

fuzz-short:
	go test ./internal/adapter -timeout=30s -run='^$$' -fuzz=FuzzReadWireMessage -fuzztime=2s
	go test ./internal/adapter -timeout=30s -run='^$$' -fuzz=FuzzDecodeSensorPacket -fuzztime=2s
	go test ./internal/floor -timeout=30s -run='^$$' -fuzz=FuzzLogicalPhysicalRoundTrip -fuzztime=2s

benchmark:
	go test -run='^$$' -bench=. -benchmem ./internal/adapter ./internal/floor

test-stress:
	go test -race -shuffle=on -count=10 ./...

native-build:
	SOURCE_REVISION="$(SOURCE_REVISION)" OUTPUT_DIR="$(NATIVE_OUTPUT_DIR)" \
		sh scripts/build-native.sh

native-verify:
	python3 scripts/verify-native.py "$(NATIVE_METADATA)"
