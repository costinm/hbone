# Base dir where this repo is downloaded
ROOT_DIR?=$(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
BASE_DIR?=$(shell basename $(ROOT_DIR))

# Artifacts outside of the source tree by default
OUT?=${ROOT_DIR}/../out/${BASE_DIR}

GOSTATIC=CGO_ENABLED=0  GOOS=linux GOARCH=amd64 time  go build -ldflags '-s -w -extldflags "-static"' -o ${OUT}

DOCKER_REPO?=gcr.io/dmeshgate/${BASE_DIR}

# Same as Istio
OCI_BASE?=ubuntu:jammy

# For smallest image
BASE_DISTROLESS?=gcr.io/distroless/static

BIN?=${BASE_DIR}


all: build push

_oci_base:
	gcrane mutate ${OCI_BASE} -t ${DOCKER_REPO}/${BIN}:base --entrypoint /${BIN}

_oci_image:
	(cd ${OUT} && tar -cf - ${PUSH_FILES} ${BIN} | \
    	gcrane append -f - \
    				  -b  ${DOCKER_REPO}/${BIN}:base \
    				  -t ${DOCKER_REPO}/${BIN}:latest )

_oci_local: build
	docker build -t costinm/hbone:latest -f tools/Dockerfile ${OUT}/

_push:
		(\
		export IMG=$(shell cd ${OUT} && tar -cf - ${PUSH_FILES} ${BIN} | \
    					  gcrane append -f - -b ${BASE_DISTROLESS} \
    						-t ${DOCKER_REPO}/${BIN}:latest \
    					   ) && \
    	gcrane mutate $${IMG} -t ${DOCKER_REPO}/${BIN}:latest --entrypoint /${BIN} \
    	)

.go-build:
	(cd cmd/${NAME} && go build -o ${OUT}/${NAME} .)

build:
	#NAME=hbone $(MAKE) .go-build
	(cd cmd/hbone && ${GOSTATIC} .)

push: _oci_image

proto-gen: PATH:=${HOME}/go/bin:${PATH}
proto-gen:
	cd urpc/proto && buf generate


perf-test-setup:
    # Using goben instead of iperf3
	goben -defaultPort :5201 &

perf-test:
	# -passiveClient -passiveServer
	goben -hosts localhost:15201  -tls=false -totalDuration 3s

perf-test-setup-iperf:
    # Using goben instead of iperf3
	iperf3 -s -d &

# Starting with certs. Identity extracted from the cert.
docker/run/certs:
	docker run -it --rm \
	   -v /etc/certs:/etc/certs -p 13009:15009\
	    costinm/hbone:latest

# Starting with k8s credentials
docker/run/k8s: _oci_local
	docker run -it --rm \
	   -v ${HOME}/.kube/config:/config -e KUBECONFIG=/config \
	    costinm/hbone:latest

# Starting with GCP credentials
docker/run/gcp: _oci_local
	docker run -it --rm \
	   -e GOOGLE_APPLICATION_CREDENTIALS=/gcp.json -v ${HOME}/.config/gcloud/application_default_credentials.json:/gcp.json \
	    costinm/hbone:latest
