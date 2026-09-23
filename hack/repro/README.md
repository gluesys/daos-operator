<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator#28 재현기

`txmode.c` 는 versitygw-daos 객체 쓰기가 DAOS 2.8 엔진을 죽이는 원인을 게이트웨이 코드 없이 재현한다.

**조건**: 조건부 KV 수정(`DAOS_COND_KEY_INSERT` / `DAOS_COND_KEY_REMOVE`)을 **명시적 트랜잭션**
(`daos_tx_open`) 안에서 실행하면 엔진이 `vos_fetch_begin` 에서 SIGSEGV(주소 0x18) 한다.
같은 연산을 `DAOS_TX_NONE` 으로 하거나, 트랜잭션 안에서 조건 플래그 없이 하면 정상이다.

| 트랜잭션 | 연산 | 결과 |
|---|---|---|
| `daos_tx_open` | `kv_put` flags=0 | 정상 |
| `daos_tx_open` | `kv_put` `DAOS_COND_KEY_INSERT` | **엔진 SIGSEGV** |
| `daos_tx_open` | `kv_remove` flags=0 | 정상 |
| `daos_tx_open` | `kv_remove` `DAOS_COND_KEY_REMOVE` | **엔진 SIGSEGV** |
| `DAOS_TX_NONE` | 위 네 가지 전부 | 정상 |

## 빌드와 실행

```bash
# daos-devel 이 있는 이미지 안에서
dnf -y install gcc libuuid-devel
gcc -O0 -g txmode.c -o txmode -ldaos -ldaos_common -lgurt -luuid

# DAOS 클라이언트(agent 소켓 있는 파드)에서
DAOS_AGENT_DRPC_DIR=/var/run/daos_agent ./txmode <system> <pool> <cont> <key> <mode>
```

모드 2 나 4 를 실행하면 **엔진이 죽는다.** 테스트 전용 시스템에서만 돌릴 것.
복구는 서버 파드 재시작이면 되고, 풀과 데이터는 남는다(실측).

## 이 경로가 트랜잭션에서만 도는 이유

`vos_fetch_begin()` 의 마무리에서 부르는 `vos_fetch_add_missing()` → `vos_ts_add_missing()` 은
첫 줄이 `if (!vos_ts_in_tx(ts_set) || dkey == NULL) return;` 이라 **트랜잭션 안에서만** 실행되고,
`ad->ad_iods[i].iod_name` 으로 iod 배열을 훑는다(DAOS 2.8 `src/vos/vos_common.c`, `src/vos/vos_io.c`).
조건부 연산의 존재 확인 fetch 가 이 경로를 타는 것으로 보이며, 정확한 라인은 debuginfo 로 확인이 필요하다.
