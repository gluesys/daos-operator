#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 3J(exaci5-3j) 안에서 root 로 실행. 사용: RUNNER_TOKEN=glrt-… runner-setup.sh
# shell executor 는 gitlab-runner 사용자로 잡을 돌린다. 그 사용자에게 kubectl·helm 과 키 디렉터리를 준다.
set -euo pipefail
: "${RUNNER_TOKEN:?runner authentication token (glrt-…)}"
curl -fsSL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.rpm.sh | bash
dnf install -y -q gitlab-runner git jq
curl -fsSL https://dl.k8s.io/release/v1.31.14/bin/linux/amd64/kubectl -o /usr/local/bin/kubectl && chmod +x /usr/local/bin/kubectl
curl -fsSL https://get.helm.sh/helm-v3.19.0-linux-amd64.tar.gz | tar xz -C /tmp && install /tmp/linux-amd64/helm /usr/local/bin/helm
gitlab-runner register --non-interactive --url https://gitlab.gluesys.com/ --token "$RUNNER_TOKEN" \
    --executor shell --description exaci5-3j-daos-k8s
sed -i 's/^concurrent = .*/concurrent = 1/' /etc/gitlab-runner/config.toml   # 클러스터는 하나다
systemctl enable --now gitlab-runner
# 잡은 gitlab-runner 사용자로 돈다. ssh 키는 CI 변수(file)로 들어오므로 여기서는 디렉터리만.
install -d -o gitlab-runner -g gitlab-runner -m 700 /home/gitlab-runner/.ssh /home/gitlab-runner/.kube
gitlab-runner verify
echo "runner-setup: done"
