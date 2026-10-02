# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# daos-hostprep: the host-preparation DaemonSet image = daos-server image (for
# `daos_server nvme prepare`) plus the static hostprep binary built here.
# Build context is the repository root:
#   docker build -f images/hostprep/Dockerfile --build-arg BASE=<daos-server image> -t daos-hostprep .
ARG BASE=registry.gitlab.gluesys.com/exastor/daos-images/daos-server:2.8.0-20260914
ARG BUILD_IMAGE=golang:1.26

FROM ${BUILD_IMAGE} AS builder
ENV GOTOOLCHAIN=auto GOFLAGS=-buildvcs=false
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -a -o hostprep ./cmd/hostprep

FROM ${BASE}
# SPDK setup.sh (daos_server nvme prepare) needs lsmod/modprobe; without them it exits under
# set -e before binding and nvme prepare still reports success (2026-10-02, exaci5-3a)
RUN dnf -y install kmod && dnf clean all
COPY --from=builder /workspace/hostprep /usr/local/bin/hostprep
RUN chmod 0755 /usr/local/bin/hostprep
ENTRYPOINT ["/usr/local/bin/hostprep"]
