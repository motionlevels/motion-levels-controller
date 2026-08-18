SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
NATIVE_OUTPUT_DIR ?= dist
NATIVE_METADATA := $(NATIVE_OUTPUT_DIR)/motion-levels-controller-linux-amd64-$(SOURCE_REVISION).metadata.json

.PHONY: check test vet build native-build native-verify

check:
	sh scripts/check.sh

test:
	go test -race ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/motion-levels-controller

native-build:
	SOURCE_REVISION="$(SOURCE_REVISION)" OUTPUT_DIR="$(NATIVE_OUTPUT_DIR)" \
		sh scripts/build-native.sh

native-verify:
	python3 scripts/verify-native.py "$(NATIVE_METADATA)"
