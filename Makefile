# Where this repo is downloaded
ROOT_DIR?=$(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

# Artifacts outside of the source tree
OUT?=${ROOT_DIR}/../out/hbone

GOSTATIC=CGO_ENABLED=0  GOOS=linux GOARCH=amd64 time  go build -ldflags '-s -w -extldflags "-static"' -o ${OUT}

.go-build:
	(cd cmd/${NAME} && go build -o ${OUT}/${NAME} .)

size-test:
	NAME=hbone $(make) .go-build
	NAME=hbone-min $(make) .go-build
	NAME=hbone-oc $(make) .go-build
	NAME=hbone-otel $(make) .go-build

proto-gen: PATH:=${HOME}/go/bin:${PATH}
proto-gen:
	cd ext/uxds/proto && buf generate


perf-test-setup:
    # Using goben instead of iperf3
	goben -defaultPort :5201 &

perf-test:
	# -passiveClient -passiveServer
	goben -hosts localhost:15201  -tls=false -totalDuration 3s

perf-test-setup-iperf:
    # Using goben instead of iperf3
	iperf3 -s -d &
