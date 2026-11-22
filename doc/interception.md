<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->
# 인터셉션 라이브러리 주입 사용 안내

CSI 로 받은 DAOS 볼륨을 쓰는 파드에 DAOS 인터셉션 라이브러리(libioil, libpil4dfs)를 `LD_PRELOAD` 로 넣어,
I/O 가 dfuse(FUSE 커널 경로)를 거치지 않고 파드 안에서 DAOS 로 바로 가게 하는 기능입니다. 설계와 측정은
`doc/plans/2026-11-19-client-interception.md`.

주입은 언제나 선택(opt-in)입니다. 주입하지 못하거나 라이브러리가 이미지에 맞지 않으면 파드는 지금처럼
dfuse 로 돕니다. webhook 은 파드를 거부하지 않습니다.

## 1. 켜기 (클러스터 관리자)

```yaml
# values.yaml
csi:
  enabled: true
interception:
  enabled: true
  # RDMA 장치를 extended resource 로 나눠 주는 경우(접두사는 / 로 끝맺음)
  rdmaResources: [rdma/]
  # 라벨 없는 네임스페이스의 Pod Security 기본 수준을 알면 적어 둔다
  defaultPodSecurity: ""
```

- DaosSystem 의 `spec.images.client` 가 `/opt/daos-il` 묶음이 든 daos-client 이미지(daos-images !11 이후)여야 합니다.
- 인증서는 operator 가 직접 만들고 갱신합니다(cert-manager 불필요).
- 클라이언트 agent(`spec.clientAgent.enabled`)가 켜져 있어야 합니다. agent 에 `nodeSelector` 를 주었다면
  인터셉션을 쓸 파드도 `nodeSelector` 에 같은 키·값을 가져야 합니다(아니면 `skipped/agent-placement`).

## 2. 쓰기 (앱 사용자)

파드 주석으로 고릅니다. 같은 키의 네임스페이스 라벨은 그 네임스페이스의 기본값이 되고, 파드 주석이 우선합니다.

```yaml
metadata:
  annotations:
    daos.gluesys.com/interception: ioil          # ioil | pil4dfs | pil4dfs-compat | off
    daos.gluesys.com/interception-report: "true" # 프로세스 종료 시 가로챈 횟수를 stderr 로(D_IL_REPORT=1)
```

| 값 | 가로채는 것 | 고를 때 |
|---|---|---|
| `ioil` | 데이터 읽기·쓰기 | 기본 선택. 셸 스크립트·대부분의 앱에서 안전. 작은 블록 랜덤 읽기·단일 스트림 I/O 가 많은 앱 |
| `pil4dfs` | 데이터 + 메타데이터(open·create·stat·unlink·mkdir …) | 작은 파일을 많이 만들고 지우는 앱 자체(생성·삭제 약 4배). **주입된 묶음에 `/opt/daos-il/PATCHES` 가 없으면(패치 전) 쓰지 말 것**, **tar 같은 범용 셸 도구를 DAOS 볼륨에 돌리는 파드에는 쓰지 말 것**(4절) |
| `pil4dfs-compat` | pil4dfs 와 같음, 호환 모드(`D_IL_COMPATIBLE=1`) | 패치 전 client 이미지에서 pil4dfs 대신(4절). open·create 가 기본 pil4dfs 보다 약 2배 느림 |
| `off` | 없음 | 네임스페이스 기본값에서 이 파드만 뺄 때 |

얼마나 빨라지나 (NVMe 4 rank 측정, dfuse 대비 — 표는 설계 문서 1.1절(rd_fac 3)·1.2절(rd_fac 0). 쓰기 배수는 복제 설정에 따라 달라집니다):

- 4 KiB 랜덤 읽기 약 4배(스레드 4개), 약 8배(16개). 단일 스트림 쓰기 약 1.7배, 읽기 1.2~1.4배. 동시 순차 읽기 1.2~1.5배.
- 여러 스레드가 동시에 큰 블록을 쓰는 부하는 dfuse 가 3~11 % 빠릅니다. 이런 부하만 있는 앱은 주입할 이유가 없습니다.
- 메타데이터는 `pil4dfs` 만 빨라집니다(생성·삭제 약 4배). `pil4dfs-compat` 은 삭제만 빨라지고 생성은 dfuse 와 같습니다. `stat` 은 pil4dfs 계열이 약 20 % 느립니다.
- 실제 앱(CSI 볼륨, 2026-11-21): 8 GiB 체크포인트 쓰기 약 3.8배·읽기 약 3배(세 모드 모두). 소스 트리(10만 항목) 풀기·검색·지우기는 `ioil` 로 빨라지지 않고 `find`·`grep -r` 은 오히려 9~13 % 느립니다. `pil4dfs-compat` 은 `tar xf` 가 상대 심볼릭 링크의 권한 설정에서 실패(rc 2, 4절)하므로 압축 해제에는 `ioil` 을 쓰세요.

주입되려면 파드가 스스로 DAOS 클라이언트가 될 수 있어야 합니다.

- agent 와 같은 네트워크 쪽(기본 agent 는 hostNetwork 이므로 파드도 `hostNetwork: true`).
- verbs(RDMA) 시스템이면 RDMA 장치: `privileged`, `/dev/infiniband` hostPath, 또는 `rdmaResources` 의 자원.
- DAOS 볼륨을 마운트한 컨테이너들이 모두 같은 이미지.

## 3. 결과 확인

```sh
# 승인 단계 판단: injected/<lib> 또는 skipped/<이유>
kubectl get pod P -o jsonpath='{.metadata.annotations.daos\.gluesys\.com/interception-result}'
# 앱 이미지에서 실제로 적재되는지(검사 init container 의 판정)
kubectl get pod P -o jsonpath='{.status.initContainerStatuses[?(@.name=="daos-il-check")].state.terminated.message}'
```

검사 판정이 `injected/<lib>` 이면 앱이 라이브러리를 씁니다. `skipped/image-incompatible ...` 이면 앱 이미지가
라이브러리를 올릴 수 없어(glibc 2.34 미만, musl/Alpine) 앱은 dfuse 로 돕니다. 이때 앱 로그의
`ERROR: ld.so: object '/opt/daos-il/active/...' ... cannot be preloaded ... ignored.` 는 무해합니다.

승인 단계 `skipped/<이유>`:

| 이유 | 뜻 / 대응 |
|---|---|
| `no-daos-volume` | CSI DAOS 볼륨을 마운트한 컨테이너가 없음 |
| `unknown-mode` | 주석 값 오타 |
| `pod-security` | 네임스페이스가 baseline·restricted(또는 모름)라 agent 소켓 hostPath 를 넣을 수 없음 |
| `namespace-unavailable` | 네임스페이스를 읽지 못함(일시적) — 파드를 다시 만들면 됨 |
| `agent-network-mismatch`, `pod-network-rdma` | 파드 네트워크 파드: `hostNetwork: true` 필요 |
| `no-rdma-device` | verbs 시스템인데 RDMA 장치가 없음 |
| `no-client-agent` | DaosSystem 의 클라이언트 agent 가 꺼져 있음 |
| `agent-placement` | agent 에 `nodeSelector` 가 있는데 파드의 `nodeSelector` 가 그 키·값을 모두 갖지 않음 — agent 가 없는 노드에 뜰 수 있음. 파드 `nodeSelector` 에 같은 키·값을 넣을 것(nodeAffinity 는 보지 않음) |
| `no-client-image`, `no-daos-system` | operator 설정(DaosSystem·`spec.images.client`) 문제 |
| `mixed-images` | DAOS 볼륨을 쓰는 컨테이너들의 이미지가 다름 |
| `ld-preload-from-source`, `env-from-source` | `LD_PRELOAD`·`NA_PLUGIN_PATH`·`IBV_DRIVERS`(그리고 요청한 경우 `D_IL_REPORT`·`D_IL_COMPATIBLE`)를 ConfigMap/Secret 에서 받음 — 값으로 바꾸거나 `off` |
| `already-injected` | 이미 주입된 파드 |

## 4. 알려진 문제와 증상

**pil4dfs 결함과 client 이미지.** DAOS 2.8.0 RPM 의 libpil4dfs 기본(fake fd) 모드에는 결함이 셋 있습니다
(2026-11-19~20 레인 A 확인, exastor/daos#14). daos-images !12 이후의 client 이미지(`daos-client:2.8.0-20261120-g27347e0e` 부터)는 패치한
libpil4dfs 를 넣어 셋 다 고쳤습니다. 고친 묶음에는 `/opt/daos-il/PATCHES`(적용한 패치 목록)가 있습니다.

판정은 **실제로 주입된 파드**에서 합니다. 같은 태그라도 노드에 받아 둔 이미지는 다를 수 있습니다(태그 이미지는
`IfNotPresent` 로 받음).

```sh
# 주입된 파드의 앱 컨테이너에 복사된 묶음: 0001-…, 0002-… 가 나오면 고친 묶음, 파일이 없으면 패치 전
kubectl exec P -c <앱 컨테이너> -- cat /opt/daos-il/PATCHES
# 묶음을 복사한 이미지(다이제스트)
kubectl get pod P -o jsonpath='{.status.initContainerStatuses[?(@.name=="daos-il-bundle")].imageID}'
```

이렇게 확인할 수 없으면 `pil4dfs` 를 쓰지 말고 `pil4dfs-compat` 이나 `ioil` 을 쓰세요. 노드마다 다른 이미지가 쓰이지
않게 하려면 `spec.images.client` 를 확인한 이미지의 다이제스트(`…/daos-client@sha256:…`)로 고정합니다.

| 결함 | 패치 전 이미지에서의 증상 |
|---|---|
| dash 리다이렉션 | Debian·Ubuntu 의 `/bin/sh`(dash)에서 `echo a > f`·`printf` 가 `I/O error`, 파일이 비어 있음. 그 뒤 실행한 `sh` 가 `daos_event_priv_get() private event is inuse` 와 함께 멈춤 |
| 외부 명령 리다이렉션 | `/bin/echo a > f` 처럼 리다이렉션한 외부 명령이 **bash·dash 모두** 멈춤(dash 는 `lseek failed ... Bad file descriptor`, rc 126) |
| fork 뒤 파일 생성 | 앱이 DAOS 파일을 한 번 연 뒤 fork 한 자식(셸 subshell·외부 명령, Python `multiprocessing`·`os.fork`)이 만든 파일이 부모·다른 자식의 파일과 **같은 객체**가 됨 — 이름은 여럿이고 내용은 마지막에 쓴 것 하나(**데이터 손상**, 오류 없음) |

패치 전 이미지에서는 `pil4dfs` 대신 `pil4dfs-compat` 이나 `ioil` 을 쓰세요. 둘 다 세 결함이 없습니다(레인 확인).

**GNU tar 와 소유권·권한 호출(2026-11-21, CSI 볼륨 실측, exastor/daos#14 후속).** daos-images !13 이후의 client
이미지(`daos-client:2.8.0-20261122-g738daa31` 부터)는 아래 B·C 를 고친 libpil4dfs 를 넣습니다. 고친 묶음의
`/opt/daos-il/PATCHES` 에는 `0003-…`·`0004-…` 가 더 있습니다.

| | 모드 | 증상 | 원인 | 상태 |
|---|---|---|---|---|
| A | `pil4dfs` | `tar xf` 가 모든 항목에서 `Cannot mkdir/open: Bad file descriptor` 로 실패, 파일이 하나도 풀리지 않음(rc 2) | RHEL 9.8 의 GNU tar(CVE-2025-45582 수정판)는 `syscall(SYS_openat2)` 로 파일을 여는데, pil4dfs 가 이를 가로채지 않아 가짜 디렉터리 fd 가 커널로 감 | daos-images !14 의 0005 로 해결(머지 대기) |
| B | `pil4dfs` | `fchownat(dirfd, …, AT_SYMLINK_NOFOLLOW)`(`chown -h`, Python `os.chown(…, dir_fd=…)`)이 `EBADF`. `linkat`·`mknodat` 도 같음 | pil4dfs 가 `fchownat`·`linkat`·`mknodat` 을 가로채지 않아 가짜 디렉터리 fd 가 커널로 감 | **0003 으로 해결**: dfuse 와 같은 결과 |
| C | `pil4dfs-compat`(기본도 같음) | `tar xf` 가 상대 경로 심볼릭 링크의 권한 설정에서 `No such file or directory` 로 rc 2. 링크 대상이 있으면 **대상의 권한이 바뀜** | `fchmodat(…, AT_SYMLINK_NOFOLLOW)` 의 NOFOLLOW 를 무시하고 링크를 따라감 | **0004 로 해결**: Linux 처럼 `EOPNOTSUPP`, 대상 그대로 |
| D | `pil4dfs-compat` | 상위 디렉터리 항목이 없는 tar(`tar cf x.tar d/f` 처럼 파일만 담은 것)를 풀면 `Cannot mkdir/open: No such file or directory` | 첫 open 실패를 dfuse 가 "없음"으로 캐시했는데, tar 가 만든 상위 디렉터리는 pil4dfs 가 DFS 로 직접 만들어 dfuse 가 모름 | daos-images !14 의 0005·0006 으로 해결(머지 대기) |

B·C 를 고친 이미지에서 compat 모드로 리눅스 커널 소스 tar 를 풀면 rc 0, dfuse 로 푼 트리와 모드·소유자·링크까지 같습니다.
A·D 까지 고친 묶음(0005·0006)에서는 기본 모드도 rc 0·차이 0 이고 dfuse 보다 약 1.5배 빠릅니다(레인 측정). dfuse·`ioil` 은
넷 다 정상입니다. 0005·0006 이 든 이미지가 나오기 전까지는 셸 스크립트·빌드·압축 해제처럼 범용 도구가 DAOS 볼륨을 다루는
파드에 `ioil` 을 쓰고, `pil4dfs` 는 직접 짠 앱(학습 데이터 로더, 체크포인트 저장 등)에 씁니다.

고친 범위의 한계: 디렉터리 fd 를 연 뒤 그 디렉터리의 이름이 바뀌면, 여러 단계 상대 경로·`linkat`·`mknodat`·링크를
따라가는 `fchownat` 은 연 디렉터리가 아니라 원래 경로를 기준으로 처리됩니다(pil4dfs 의 기존 경로 모델과 같음).

**쓰기 직후 다른 프로세스의 읽기.** pil4dfs 는 dfuse 를 거치지 않고 씁니다. 가로채지 않는 프로세스(주입되지 않은
컨테이너, 정적 바이너리, 다른 노드, pil4dfs 가 스스로 건너뛰는 `cat` 같은 명령)가 dfuse 로 같은 파일을 읽으면 dfuse 캐시 때문에 몇 초 동안 이전 내용이 보일 수
있습니다(레인에서 append 직후 약 3 s). 패치와 무관하며 RPM 그대로의 동작입니다.

**자체 로더를 쓰는 바이너리.** 검사는 이미지 기본 로더로 합니다. 앱이 다른 동적 로더(`PT_INTERP`, conda·nix 류)를
쓰는 바이너리를 실행하면 검사를 통과해도 시작 때 `version 'GLIBC_2.3x' not found` 같은 오류로 실패할 수 있습니다.
주석을 `off` 로 바꾸면 됩니다.

**기타.** 정적 바이너리(Go 등)는 가로채지 않고 그대로 dfuse 를 씁니다. pil4dfs 는 mmap·fork 이후·io_uring 에서
맞지 않는 앱이 있습니다(DAOS 문서 "Bypassing function interception", `D_IL_BYPASS_LIST`).
