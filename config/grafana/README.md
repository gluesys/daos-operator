<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Grafana 대시보드

`DAOS-Grafana-Dashboard.json` 은 DAOS 저장소 `utils/grafana/DAOS-Grafana-Dashboard.json` 을 그대로 가져온 것이다
(BSD-2-Clause Plus Patent License, Copyright Intel Corporation). 수정하지 않고 동봉하며, 갱신은 upstream 파일을 다시 복사한다.

- 데이터소스 변수 `${DS_PROMETHEUS}` 를 쓰므로 Grafana 임포트 시 Prometheus 데이터소스를 고르면 된다.
- 쿼리는 `engine_*` 메트릭(`engine_rank`, `engine_pool_xferred_*`, `engine_events_dead_ranks`)을 `rate(...[15s])` 로 읽는다.
  ServiceMonitor 기본 scrape 주기를 15초로 맞춘 이유다.
- `kustomize build config/grafana` 는 grafana 사이드카 규약(`grafana_dashboard: "1"`)의 ConfigMap 을 만든다. Helm 차트에서는
  `grafana.dashboard.enabled` 로 같은 ConfigMap 을 설치한다.
