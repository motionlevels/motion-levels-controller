IMAGE ?= motion-levels-controller:local

.PHONY: check test vet generate proto-check docker-build

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

docker-build:
	docker build --build-arg BUILD_REVISION=local -t "$(IMAGE)" .
