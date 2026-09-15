<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# krew 배포 (#18)

## 지금 되는 것 (사내)
태그(`v*`)를 푸시하면 CI 의 `release-cli` 잡이 `kubectl-daos` 를 linux/amd64·arm64, darwin/amd64·arm64 로 빌드해
GitLab generic package `kubectl-daos/<tag>` 로 올리고, 같은 링크를 GitLab Release 에 붙인다. 설치는 내려받아 PATH 에 두면 끝이다.

```bash
V=v0.1.0
curl -fSLO --header "PRIVATE-TOKEN: $TOKEN" \
  "https://gitlab.gluesys.com/api/v4/projects/820/packages/generic/kubectl-daos/$V/kubectl-daos_${V}_linux_amd64.tar.gz"
tar xzf kubectl-daos_${V}_linux_amd64.tar.gz && sudo install kubectl-daos /usr/local/bin/
kubectl daos system status daos
```

## krew-index 제출에 필요한 것 (미결, 사람 결정)
krew-index 는 **공개 URL** 에서 내려받을 수 있는 자산만 받는다. 이 GitLab 은 사내망이므로 그대로는 제출할 수 없다. 남은 선택지:

1. 공개 미러 `github.com/gluesys/daos-operator` 에 GitHub Release 를 만들고 같은 tar.gz 를 자산으로 올린다.
   미러 토큰(`claude.github.token`)은 `public_repo` 스코프라 릴리스 생성은 가능하지만, **CI 에 GitHub 토큰을 넣는 것은 사람이 결정할 일**이라 자동화하지 않았다.
2. 릴리스 자산만 별도 공개 호스팅(예: 오브젝트 스토리지)에 둔다.

공개 URL 이 정해지면 매니페스트는 한 줄로 만들어진다:

```bash
VERSION=v0.1.0 hack/release-cli.sh
VERSION=v0.1.0 BASE_URL=https://github.com/gluesys/daos-operator/releases/download/v0.1.0 hack/krew-manifest.sh
kubectl krew install --manifest=krew/daos.yaml    # 로컬 검증
```
그 다음 `krew-index` 에 `plugins/daos.yaml` 로 PR 을 낸다(이름 `daos` 가 비어 있는지 먼저 확인).
