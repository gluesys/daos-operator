# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# daos-hostprep: the daos-server image (for `daos_server nvme prepare`, lspci) plus the
# static hostprep binary. Build context = images/hostprep (make copies bin/hostprep there;
# the repo-root .dockerignore excludes bin/).
ARG BASE=registry.gitlab.gluesys.com/exastor/daos-images/daos-server:2.8.0-20260914
FROM ${BASE}
COPY hostprep /usr/local/bin/hostprep
RUN chmod 0755 /usr/local/bin/hostprep
ENTRYPOINT ["/usr/local/bin/hostprep"]
