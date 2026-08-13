IMAGE ?= motion-levels-controller:local
SOURCE_REVISION ?= $(shell git rev-parse HEAD)
NATIVE_OUTPUT_DIR ?= dist
NATIVE_METADATA := $(NATIVE_OUTPUT_DIR)/motion-levels-controller-linux-amd64-$(SOURCE_REVISION).metadata.json

.PHONY: check test vet generate proto-check native-build native-verify docker-build

check:
	sh scripts/check.sh

test:
	go test ./...

vet:
	go vet ./...

generate:
	protoc --go_out=. --go_opt=paths=source_relative \
		contracts/floorpb/floor.proto \
		contracts/inputpb/input.proto \
		contracts/recordingpb/recording.proto

proto-check:
	@set -eu; \
		tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
		protoc --go_out="$$tmp" --go_opt=paths=source_relative \
			contracts/floorpb/floor.proto \
			contracts/inputpb/input.proto \
			contracts/recordingpb/recording.proto; \
		cmp -s "$$tmp/contracts/floorpb/floor.pb.go" contracts/floorpb/floor.pb.go; \
		cmp -s "$$tmp/contracts/inputpb/input.pb.go" contracts/inputpb/input.pb.go; \
		cmp -s "$$tmp/contracts/recordingpb/recording.pb.go" contracts/recordingpb/recording.pb.go

native-build:
	SOURCE_REVISION="$(SOURCE_REVISION)" OUTPUT_DIR="$(NATIVE_OUTPUT_DIR)" \
		sh scripts/build-native.sh

native-verify:
	python3 scripts/verify-native.py "$(NATIVE_METADATA)"

docker-build:
	docker build --build-arg BUILD_REVISION=local -t "$(IMAGE)" .
