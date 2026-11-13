<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Connecting a serving stack to DAOS — the integration contract

daos-operator provides the storage side: the DAOS system, its pools and containers, and the
client agent on the GPU nodes. **It does not run inference.** The serving stack belongs to
whoever runs it — KServe, llm-d, NVIDIA Dynamo, Ray Serve, or a plain Deployment — and has its
own lifecycle: model rollouts, autoscaling, GPU queues, auth and ingress.

The chart's `vllm.services[]` is a **reference workload for PoCs, demos and our own tests**. It
is one implementation of the contract below, nothing more. In production, give the contract to
the team that owns serving. Copy what the reference template does, not the template.

Supported environments and the evidence behind them are in [compatibility.md](compatibility.md).

## 1. What the storage side provides

| Item | Where it comes from | The serving pod sees |
|---|---|---|
| DAOS system | `DaosSystem` (operator) | its `spec.systemName` (e.g. `daos_k8s`) |
| Pool | `DaosPool` | the pool label = the DaosPool name |
| KV container | `DaosContainer` | the container label = `spec.label`, else the DaosContainer name |
| Client agent | `DaosSystem.spec.clientAgent` — one daos_agent per selected node | a socket in a hostPath on the node |

Check the agent on a GPU node:

```bash
kubectl get daossystem <name> -o jsonpath='{.status.conditions[?(@.type=="ClientAgent")].message}{"\n"}'
```

## 2. What the serving pod must do

Every item comes from a failure that was measured. The **Why** column says what breaks when the item is missing.

| # | Requirement | Why |
|---|---|---|
| 1 | Mount the agent socket directory, `hostPath: /var/run/daos-operator/agent/<DaosSystem name>` (or `spec.clientAgent.hostSocketDir`), at `/var/run/daos_agent`, and set `DAOS_AGENT_DRPC_DIR=/var/run/daos_agent` | libdaos talks to the system only through the agent |
| 2 | `hostNetwork: true` (with `dnsPolicy: ClusterFirstWithHostNet`) | The agent hands the client host interface names, and the engines must reach the client at the address it announces. Behind a CNI masquerade every RPC ends in `DER_TIMEDOUT` (measured) |
| 3 | On an RDMA fabric (ofi+verbs, ucx): the verbs devices. Either `/dev/infiniband` with `privileged`, or an RDMA device plugin resource (e.g. `rdma/rdma_shared_device_a`) with `IPC_LOCK` | Without them the verbs provider does not initialise and the connector never comes up. The device plugin route drops `privileged` at the same speed (400G RoCE, 2026-10-01) |
| 4 | `PYTHONHASHSEED=0` in every process that computes cache keys (in-process mode) | The keys are hashed per process; a different seed after a restart finds none of the stored KV |
| 5 | An image with lmcache-daos and a DAOS **2.8** client (libdaos, libdfs, libfabric **1.22** from the DAOS repository — not the distribution's 2.x) | The client must match the servers; libfabric 2.2–2.5 crashes in `ibv_get_async_event` (ofiwg/libfabric#12181) |
| 6 | One serving port per node (hostNetwork) — and in MP mode one MP server port per node | Ports are the node's |

A pod that cannot use hostNetwork (some serving charts cannot ask for it) can use
`spec.clientAgent.hostNetwork: false`. That puts the agent on the pod network, and
`spec.client.includeFabricIfaces` must list the pod interface. **This path is not verified.**

## 3. How LMCache reaches DAOS

There are two modes. Choose by model.

### 3.1 In-process (`LMCacheConnectorV1`) — the default

The connector runs inside each vLLM worker. Configuration comes from a file named by
`LMCACHE_CONFIG_FILE`:

```yaml
chunk_size: 256
remote_serde: "naive"
remote_storage_plugins: ["daos"]
extra_config:
  remote_storage_plugin.daos.url: "plugin://daos/<pool>/<container>?sys=<systemName>"
  remote_storage_plugin.daos.module_path: lmcache_daos.connector
  remote_storage_plugin.daos.class_name: DaosConnector
# local_cpu / max_local_cpu_size: LMCache's CPU tier in front of DAOS (optional)
```

```
--kv-transfer-config={"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}
```

- Use the plugin route only. Setting `remote_url` as well makes LMCache 0.5.5 build two
  backends on one container, which stores every chunk twice.
- The container is POSIX with 4 MiB chunks (`DaosContainer` `type: POSIX`,
  `chunkSize: 4194304`). The 1 MiB default costs one RPC per chunk.
- The lmcache-daos rank guard (environment `DAOS_RANK_GUARD=1` and friends, deploy-gpu-k8s §3.1a) works in this mode only.

### 3.2 Multi-process (`LMCacheMPConnector`) — hybrid-attention models

Qwen3.5 / Qwen3.8 (linear GDN + full attention) start only this way (moon, 2026-11-13). An
LMCache MP cache server runs **in the same container** as vLLM, because vLLM passes its KV
buffers over CUDA IPC:

```bash
python3.12 -m lmcache_daos.mp.server --host 127.0.0.1 --port <mp-port> --chunk-size <n> \
  --l1-size-gb <L1> --eviction-policy LRU --disable-observability \
  --l2-adapter '{"type":"daos","pool":"<pool>","container":"<container>","sys":"<systemName>","root":"/mp","workers":16}'
# start vLLM only once <mp-port> is listening, and only if it is this process listening
```

```
--kv-transfer-config={"kv_connector":"LMCacheMPConnector","kv_role":"kv_both",
  "kv_connector_extra_config":{"lmcache.mp.host":"tcp://127.0.0.1","lmcache.mp.port":<mp-port>}}
```

- No `LMCACHE_CONFIG_FILE`: the server's arguments are the configuration.
- The adapter reads a POSIX container through DFS. The rank guard does not apply.
- Key names differ from in-process mode. Keep the two apart with `root` (default `/mp`).
- Model requirements (Qwen3.8 on A100): `--chunk-size` must be a multiple of vLLM's block (784), with
  `--enable-prefix-caching` and `--mamba-cache-mode=align`. The L1 must hold one whole
  request, and the attention backend and KV dtype must suit the image and GPU. See deploy-gpu-k8s §5.6.
- vLLM 0.18 picks a stale bundled `LMCacheMPConnector`. There, use `DaosMPConnector` with
  `kv_connector_module_path: lmcache_daos.mp.vllm_connector` (lmcache-daos README).

## 4. Checking the connection

The same three checks work for any serving stack (deploy-gpu-k8s §6):

1. The connector attached: look for `Creating connector for URL: plugin://daos/...` (in-process). In MP mode,
   check that the MP server is up before vLLM (the reference launcher logs `lmcache MP server listening on <port>`)
   and that vLLM starts its `LMCacheMPConnector` without a ZMQ error.
2. A cold request stores KV: `Stored N out of total N tokens`, or the DAOS container's used space grows.
3. After the GPU cache is dropped (a restart, or `/reset_prefix_cache` in dev mode), the same
   prompt is served from DAOS: LMCache hit tokens > 0, or vLLM's
   `vllm:external_prefix_cache_hits_total` rises.
