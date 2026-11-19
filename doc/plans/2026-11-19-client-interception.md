<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# CSI 볼륨 파드에 인터셉션 라이브러리(ioil·pil4dfs) 주입 — 설계안

**목표:** CSI 로 DAOS 볼륨을 받은 파드가 앱을 고치지 않고 libioil / libpil4dfs 로 dfuse(FUSE) 를 우회하게 한다. 조건이 안 맞는 파드는 지금처럼 dfuse 로 둔다.

**상태:** 결정 D1~D4 = 권장안(2026-11-19). 구현 중: daos-images !11, operator 후속 MR.

## 1. 배경: S0 측정 (2026-11-19, 레인 A)

구성: rank 1개(3A, kdev `/dev/sdb`, 4 targets), `ofi+verbs;ofi_rxm` ib0, hostNetwork agent·CSI 노드 플러그인 4A, 측정 파드 4A hostNetwork. fio psync direct thread. 지운 공간 회수(1~2분)를 기다리며 역순으로 돈 2회차 기준, dfuse(CSI 기본값) 대비 배수.

| 부하 | dfuse | dfuse 튜닝(스레드 24·EQ 8) | ioil | pil4dfs |
|---|---|---|---|---|
| 순차 쓰기 1 MiB | 136 MiB/s | ×1.24 | ×0.95 | ×1.20 |
| 순차 읽기 1 MiB, 4 jobs | 574 MiB/s | ×0.99 | ×0.71 | ×0.96 |
| 랜덤 읽기 4 KiB, 4 jobs | 6,988 IOPS | ×1.13 | ×1.70 | ×1.76 |
| 랜덤 쓰기 4 KiB, 4 jobs | 9,118 IOPS | ×1.03 | ×1.16 | ×1.24 |
| 파일 생성 | 1,878 IOPS | ×0.97 | ×0.87 | **×4.57** |
| 파일 삭제 | 248 IOPS | ×0.99 | ×0.99 | **×4.00** |

- 확실한 것: pil4dfs 메타데이터(생성·삭제) 약 4배, 두 회차 모두. ioil 은 메타데이터를 가로채지 않는다.
- 판정 못 한 것: 대역폭. VM 디스크 rank 1개가 300~570 MiB/s 에서 막혀 dfuse 가 병목이 되기 전이다(회차 간 ±40%). NVMe 다중 rank 백엔드에서 다시 잰다(GPUFlow 2차 테스트베드 등).
- 폴백: pod network 파드(verbs 시스템)에서 ioil 은 `daos_init` DER_HG 로 0.6 s 안에 실패한 뒤 dfuse 로, pil4dfs 는 0건 가로챈 뒤 dfuse 로 썼다. `LD_PRELOAD` 파일이 없으면 glibc 경고 한 줄 후 정상 실행.
- **다른 배포판 이미지에서 동작(S0c):** Ubuntu 22.04(glibc 2.35) 앱 이미지에서 ioil·pil4dfs 모두 가로챘다. 조건 셋: RDMA 장치(`/dev/infiniband`) 접근, `NA_PLUGIN_PATH`(mercury NA 플러그인), `IBV_DRIVERS=<묶음>/libibverbs/libmlx5`(rdma-core 가 `-rdmav59.so` 를 붙임). 장치가 없으면 libfabric 에 verbs 가 안 보여 dfuse 로 폴백.
- **앱 이미지가 안 맞으면 앱이 죽는다(S0b):** EL9 묶음을 Ubuntu 20.04(glibc 2.31)에 preload 하면 `GLIBC_2.34 not found` 로 실행 실패(rc 1), Alpine 3.20(musl)은 재배치 오류로 rc 127. "ignored" 로 넘어가지 않는다. → 앱 이미지 검사(3절 2b)가 필수.

## 2. 언제 주입하나

앱 파드가 직접 DAOS 클라이언트가 되므로 모두 맞아야 한다.

| 조건 | 판단 근거 | 비고 |
|---|---|---|
| 파드가 RDMA 장치를 쓸 수 있다 | `privileged`, `/dev/infiniband` hostPath, 또는 설정 목록의 RDMA 장치 리소스 요청 | provider 가 tcp 면 불필요 |
| 파드가 fabric 에 닿는다 | `hostNetwork: true`, 또는 RDMA 2차 인터페이스(Multus 주석 + 설정 목록의 리소스 이름) | provider 가 tcp 면 pod network 도 가능 |
| agent 와 같은 쪽 | `spec.clientAgent.hostNetwork` 와 파드 네트워크가 같은 쪽 | agent 가 건네는 인터페이스 이름이 파드에 있어야 한다 |
| 사용자가 원한다 | 파드 주석 `daos.gluesys.com/interception: ioil\|pil4dfs\|off` | D2 참고 |

안 맞으면 주입하지 않고 파드 주석 `daos.gluesys.com/interception-result: skipped/<사유>` 를 남긴다.

## 3. 구성요소

1. **Mutating webhook (operator):** 파드 생성 시 볼륨이 DAOS CSI 인지 본다 — 바운드된 PVC 는 PV 의 `csi.driver`, 아직 바운드 안 된 PVC(지연 바인딩 포함)는 StorageClass 의 `provisioner`, generic ephemeral 볼륨은 템플릿의 StorageClass. `failurePolicy: Ignore`(operator 가 죽어도 파드 생성은 막지 않음), `namespaceSelector` 로 범위 제한. 주입 내용:
   - init container `daos-il-bundle`(daos-client 이미지) · `daos-il-check`(앱 이미지)
   - emptyDir `daos-il` 을 앱 컨테이너 `/opt/daos-il` 에 읽기 전용으로
   - `LD_PRELOAD=/opt/daos-il/active/<lib>.so`(il-check 가 통과할 때만 생기는 링크), `NA_PLUGIN_PATH=/opt/daos-il/lib/mercury`, `IBV_DRIVERS=/opt/daos-il/lib/libibverbs/libmlx5`(설정 가능), `D_IL_REPORT`(주석으로 켬)
   - agent 소켓 hostPath(없을 때만)
2. **묶음 복사 (첫 init container, daos-client 이미지):** `/opt/daos-il` 묶음을 emptyDir 로 복사만 한다. 연결 확인은 하지 않는다 — 실행 시점에 붙지 못하면 라이브러리가 스스로 dfuse 로 돌아가고(S0, S0c), init container 에 앱과 같은 RDMA 장치·리소스를 붙이기도 어렵다.
2b. **앱 이미지 검사 (두 번째 init container):** 앱 컨테이너와 **같은 이미지·같은 securityContext** 로 정적 바이너리 `il-check`(emptyDir 에서 실행, 셸 불필요)를 돌린다. 이 도구는 묶음에 든 작은 동적 프로그램을 `LD_PRELOAD` 를 걸고 실행해 보고, 되면 `active/<lib>.so` 심볼릭 링크를 만든다. 앱의 `LD_PRELOAD` 는 이 링크를 가리키므로, 실패하면(glibc 낮음, musl, 로더 없음) 파일이 없어 경고 한 줄 뒤 dfuse 로 돈다. `active/` 는 sticky 없는 0777(남의 심볼릭 링크를 커널이 막지 않게). 앱 이미지의 로더·glibc 로 시험하므로 S0b 의 실패를 앱 시작 전에 걸러낸다. 결과는 `interception-result: skipped/image-incompatible`.
3. **라이브러리 묶음 (daos-images):** `/opt/daos-il/lib` 에 libioil·libpil4dfs 와 의존 라이브러리(libdaos·libdfs·libgurt·cart·mercury·libfabric·libibverbs·librdmacm), `lib/mercury/libna_plugin_ofi.so`, `lib/libibverbs/lib<nic>-rdmav*.so` 를 RUNPATH 를 절대 경로 `/opt/daos-il/lib` 로 넣는다(`$ORIGIN` 이 아니므로 `active/` 의 심볼릭 링크로 preload 해도 의존 라이브러리는 `lib/` 에서 찾는다; 묶음은 항상 같은 경로에 마운트). 앱 전체에 `LD_LIBRARY_PATH` 를 걸지 않기 위해서다(NCCL·UCX 가 쓰는 libfabric 과 충돌 방지).
4. **인증서:** webhook 은 TLS 가 필요하다. D1 참고.

## 4. 위험

- **glibc 버전·musl:** 묶음은 EL9(glibc 2.34) 빌드다. 낮은 glibc·musl 이미지에서는 앱이 시작하지 못한다(S0b 확인). 2b 검사로 막지만, 검사가 빠지거나 틀리면 파드가 죽는 종류의 실패라 기본은 opt-in 으로 둔다. 정적 바이너리(Go)는 효과 없이 지나간다.
- **pil4dfs 호환:** mmap·fork 후 동작·io_uring 등 맞지 않는 앱이 있다. 기본은 ioil, pil4dfs 는 명시할 때만.
- **pil4dfs × dash(실기 확인):** 기본 pil4dfs(fake fd)는 dash(`/bin/sh`)의 DAOS 파일 리다이렉션을 깨뜨린다(echo·printf EIO, 외부 명령 EBADF, 이후 sh 멈춤). `D_IL_COMPATIBLE=1` 과 ioil 은 정상이지만 호환 모드는 생성이 약 2배 느리다. 결정(2026-11-19): pil4dfs 는 그대로 두고 주석 값 `pil4dfs-compat` 으로 호환 모드를 고르게 하며, 사용 안내(`doc/interception.md`)에 적는다.
- **연결 수:** 파드마다 pool/container 를 연다.
- **Pod Security:** webhook 은 hostNetwork 를 켜지 않는다. agent 소켓 hostPath 를 더해야 하는데 네임스페이스 enforce 가 baseline·restricted 면 건너뛴다(`skipped/pod-security`). agent DaemonSet 이 꺼져 있으면 소켓 디렉터리가 없어 건너뛴다(`skipped/no-client-agent`).
- **동적 로더 차이(남는 위험):** il-test 는 이미지의 기본 로더(`/lib64/ld-linux-x86-64.so.2`)로 시험한다. webhook·init container 는 앱이 실제로 실행할 바이너리를 알 수 없어(엔트리포인트·셸 스크립트·자식 프로세스) 로더를 미리 맞출 수 없다. 앱 바이너리가 다른 `PT_INTERP`(conda·nix 류 자체 로더)를 쓰면 검사 통과 후에도 시작이 실패할 수 있다. 그래서 주입은 기본 opt-in(D2)이고, 그런 앱은 주석 `off`. e2e 와 문서에 증상(앱 시작 시 `version GLIBC_* not found` 등)을 적는다.

## 5. 단계

- [x] S0b: glibc 낮은 이미지·musl 이미지 → 앱 실행 실패 확인(2026-11-19)
- [x] S0c: Ubuntu 22.04 앱 이미지에서 가로채기 확인, 필요한 환경 변수·장치 확인(2026-11-19)
- [ ] S0c: 빠른 백엔드에서 대역폭 재측정(장비 결정 필요)
- [x] S2-1 daos-images: `/opt/daos-il` 묶음 + il-check (daos-images !11, 레인 A 수동 흐름: ubuntu22 injected·가로채기, ubuntu20·alpine skipped·rc 0)
- [x] S2-2 operator: webhook(주입·판단 표), 단위 시험 (!112)
- [x] S2-4 차트: webhook 설정·인증서·RBAC(PVC/PV/StorageClass get, MutatingWebhookConfiguration get/update), 기본 꺼짐
- [x] S2-5 e2e(레인 A 수동, 2026-11-19): ubuntu22 pil4dfs 주입·가로채기, ubuntu20·alpine image-incompatible 로 rc 0, pod network 파드 승인 단계 skipped
- [ ] S2-6 CI e2e 케이스(레인 B tier1 은 ofi+tcp 1 rank — TCP 경로로 주입·검사·skip 확인)

## 6. 결정 필요

- **D1 인증서:** (권장) operator 가 자체 서명 인증서를 만들고 webhook caBundle 을 갱신(cert-controller 류) / cert-manager 의존.
- **D2 기본값:** (권장) 파드 주석으로만 켬, 네임스페이스 라벨로 기본값 지정 가능 / StorageClass 파라미터로 기본 켬.
- **D3 묶음 전달:** (권장) init container 복사(K8s 1.31 레인에서 동작) / image volume(K8s 1.33+ 에서만, 복사 없음).
- **D4 pil4dfs 노출:** (권장) 주석 값으로 허용하되 문서에 호환 위험 명시 / ioil 만.
