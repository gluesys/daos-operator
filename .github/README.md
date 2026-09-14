<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

GitHub 는 GitLab(exastor/daos-operator) 의 푸시 미러다. CI 는 `.gitlab-ci.yml` 에서 돈다.
kubebuilder 가 생성한 GitHub Actions 워크플로는 제거했다 — 미러 토큰에 `workflow` 스코프가 없으면
워크플로 파일이 있는 브랜치의 미러 푸시가 거부되기 때문이다(lmcache-daos 미러 #18 실패 사례).
