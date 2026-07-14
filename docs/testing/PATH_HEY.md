# Testing via PATH + `hey` (HTTP load)

Drive relays through the **PATH gateway** with [`hey`](https://github.com/rakyll/hey),
exactly the way production traffic flows: client → PATH → relayer → backend.
This is the go-to path for **throughput and full-lifecycle** testing on the
Tilt localnet.

This is one of three testing guides. See the index at
[`./README.md`](./README.md) for how they fit together:

- [`./TILT.md`](./TILT.md) — bring the localnet up and confirm it's healthy.
- **`./PATH_HEY.md`** (this doc) — load/lifecycle testing through the gateway.
- [`./DIRECT_CLI.md`](./DIRECT_CLI.md) — send signed relays straight to the
  relayer with the `relay` CLI (needed for error-path testing — see below).

## When to use PATH + `hey` (and when NOT to)

> **CAVEAT — PATH masks relayer 503s.** When the relayer returns a `503`
> (e.g. fail-closed on a Redis outage, session not found, over-budget),
> PATH forwards it to the client as **HTTP 200 with an empty body**. `hey`
> only inspects status codes, so it counts those as success. That makes
> PATH + `hey` **wrong for exercising relayer error paths** — you'd get
> false green. For error-path and negative testing, send signed relays
> directly to the relayer (`:8180`) with the CLI: see
> [`./DIRECT_CLI.md`](./DIRECT_CLI.md). This masking is real and is why
> `scripts/test-cache-cleanup-live.sh` drives validation through the CLI,
> not PATH.

Use PATH + `hey` for:

- **Throughput / sustained load** — how many RPS the whole path carries.
- **Lifecycle** — relays → sessions → claims → proofs → settlement.
- **Distribution** — round-robin across suppliers and backends.

Do **not** use it for:

- **Relayer error paths / negative cases** — masked as 200 (use the CLI).
- **Body-level correctness** — `hey` never reads the body. Use
  `scripts/loadtest/http-verify.go` (below), which validates the JSON-RPC
  `result` field and catches the empty-body / RPC-error cases `hey` misses.

## Prerequisites

- Tilt localnet up and healthy — see [`./TILT.md`](./TILT.md).
- `hey` installed:
  ```bash
  go install github.com/rakyll/hey@latest
  ```
- The built binary for Redis inspection (the `redis` subcommand ships in the
  same binary):
  ```bash
  make build            # produces ./bin/pocket-relay-miner
  ```
  Commands below use `pocket-relay-miner` for brevity; substitute
  `./bin/pocket-relay-miner` if it isn't on your `PATH`.

### Localnet endpoints (verified against `tilt/k8s`)

| What | Address | Source |
|------|---------|--------|
| PATH gateway (HTTP) | `http://localhost:3069/v1` | `tilt/k8s/defaults.Tiltfile` (`path.port: 3069`), `path.Tiltfile` |
| PATH gateway (WebSocket) | `ws://localhost:3069/v1` | same port, WS upgrade |
| Relayer HTTP (direct) | `http://localhost:8180` | `relayer.Tiltfile` port-forward `8180→8080`; used by `DIRECT_CLI.md` |
| Validator RPC (CometBFT) | `http://localhost:26657` | `ports.Tiltfile` (`validator_rpc`) |
| Validator gRPC | `localhost:9090` | `ports.Tiltfile` (`validator_grpc`) |
| Redis | `redis://localhost:6379` | `ports.Tiltfile` (`redis`); also the `redis` subcommand default |
| Prometheus | `http://localhost:9091` | `ports.Tiltfile` (`prometheus`) |
| Grafana | `http://localhost:3000` | `ports.Tiltfile` (`grafana`) |
| Loki | `http://localhost:3100` | `observability.Tiltfile` (`loki.port: 3100`) |
| Relayer pprof | `http://localhost:6060` | `defaults.Tiltfile` (`relayer.pprof_port`) |
| Miner pprof | `http://localhost:6065` | `defaults.Tiltfile` (`miner.pprof_port`) |

All ports are forwarded automatically by Tilt — no manual `kubectl port-forward`.

The genesis stakes four apps, one per service, all delegating to `gateway1`
(see `cmd/cmd_relay.go`):

| Service ID | Transport |
|------------|-----------|
| `develop-http` | JSON-RPC over HTTP |
| `develop-websocket` | WebSocket |
| `develop-stream` | REST / SSE streaming |
| `develop-grpc` | gRPC |

## Pre-flight: confirm the gateway serves relays

One relay, checking only the HTTP status (should print `200`):

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1
```

Full body (to eyeball the actual response, including `result.backend_id`):

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1 | jq .
```

## Sending relays with `hey`

`hey` selects the target service via the `Target-Service-Id` header and POSTs
the JSON-RPC body to the gateway's `/v1` endpoint.

**Single request** (sanity check via the load tool itself):

```bash
hey -n 1 -c 1 -m POST \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1
```

**Load** — 3000 relays, 50 concurrent workers (`-n` total, `-c` concurrency):

```bash
hey -n 3000 -c 50 -m POST \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1
```

Reading the output:

- **Status code distribution** should be all `200`. Remember the caveat:
  `200` here means "PATH accepted it", not "the relay was valid" — a masked
  relayer `503` also shows as `200`. Use `http-verify.go` (next section) when
  you need body-level truth.
- **Requests/sec** is hardware-dependent; on a kind localnet expect low
  hundreds of RPS end-to-end (PATH + validation + signing add overhead versus
  the relayer's raw ceiling). Treat it as a relative number across runs, not
  an absolute benchmark.

## Body-level correctness (where `hey` is blind)

`hey` never reads the response body, so it cannot tell a valid relay from a
masked `503` or a JSON-RPC error. `scripts/loadtest/http-verify.go` is a load
generator that **does** parse each body and classifies it: valid `result`,
empty body (masked `503`), malformed body, or JSON-RPC `error`.

```bash
# 1000 RPS for 5 minutes against the gateway, reporting body-level buckets
go run scripts/loadtest/http-verify.go -rps 1000 -duration 300s
```

Useful flags (see the file header for the full list): `-url`
(default `http://localhost:3069/v1`), `-service` (default `develop-http`),
`-rps`, `-workers`, `-duration`, `-report`.

For **backend-side** capacity — how much each upstream RPC node can sustain,
and the `max_conns_per_host` to configure per service — use the separate
ceiling tool documented in [`scripts/loadtest/README.md`](../../scripts/loadtest/README.md).
That tool measures backends **directly** (not through the relayer), so it
answers a different question than PATH + `hey`.

## Verifying the lifecycle

After sending relays, walk the state machine:
**relays consumed → sessions → claims → proofs → settlement.**

For what the claim and proof windows mean, and the in-window inclusion
reconciler that guards against a tx accepted to the mempool but never landing
in a block, read [`../CLAIM_PROOF_LIFECYCLE.md`](../CLAIM_PROOF_LIFECYCLE.md).
For how a claim's leaves/relays are counted, see
[`../CLAIM_LEAF_MODEL.md`](../CLAIM_LEAF_MODEL.md).

**Window timing** is set by `x/shared` params. The **current localnet
genesis** (source: header of `scripts/test-chaos-leader-flush-gap.sh`):

- `num_blocks_per_session`: **10**
- `claim_window_open_offset_blocks`: **1**
- `claim_window_close_offset`: **8**
- `proof_window_open_offset`: **0**
- `proof_window_close_offset`: **8**

So for a session ending at height `E`: the claim window opens at `E+1` and
closes `~8` blocks later, the proof window opens when the claim window closes
and closes `~8` blocks after that, and settlement follows the proof-window
close. Keep timing statements relative to these offsets — do **not** assume a
4-block claim window (that was the old, stale value).

### Inspect state with the `redis` subcommand

All session state lives in Redis (key patterns in
[`../REDIS.md`](../REDIS.md)). The `--redis` flag defaults to
`redis://localhost:6379`, so it can be omitted on localnet.

Current block height (to know where you are in the window cycle):

```bash
curl -s http://localhost:26657/status | jq -r '.result.sync_info.latest_block_height'
```

Which suppliers exist and how claims are distributed across miners:

```bash
pocket-relay-miner redis supplier --list
pocket-relay-miner redis supplier --claims
```

Stream (WAL) depth for a supplier — confirm relays were consumed
(`PENDING` should fall to 0):

```bash
pocket-relay-miner redis streams --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
```

Session lifecycle for a supplier, optionally filtered by state
(`active | claiming | claimed | proving | settled | expired`):

```bash
pocket-relay-miner redis sessions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
pocket-relay-miner redis sessions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj --state settled
```

Claim/proof submission tracking — tx hashes, success/failure, timing, error
reasons (7-day TTL, the authoritative record for "did the claim/proof land?"):

```bash
pocket-relay-miner redis submissions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
pocket-relay-miner redis submissions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj --failed-only
# Drill into one session (session-end height is required alongside --session):
pocket-relay-miner redis submissions \
  --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj \
  --session <session_id> --session-end <height>
```

(`pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj` is the first localnet supplier —
a public genesis fixture. Use `redis supplier --list` to see the rest.)

### Metrics (Prometheus) and logs (Loki)

Prometheus at `http://localhost:9091` — the miner exports `ha_miner_*` and the
relayer `ha_relayer_*` series. Useful counters for the lifecycle:

- `ha_miner_relays_consumed_from_stream_total` — relays pulled off the WAL.
- `ha_miner_sessions_created_total` — sessions opened.
- `ha_miner_claim_num_leaves` / `ha_miner_relay_processing_latency_seconds`.

Logs go to Loki at `http://localhost:3100` (containers rotate under load, so
prefer Loki over `kubectl logs`). Example — claim/proof submissions in the last
10 minutes:

```bash
now_ns=$(date -d "now" +%s)000000000
start_ns=$(date -d "10 minutes ago" +%s)000000000
curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="miner"} |~ "claim submitted|proof submitted"' \
  --data-urlencode "limit=50" \
  --data-urlencode "start=$start_ns" \
  --data-urlencode "end=$now_ns" | jq -r '.data.result[].values[][1]'
```

## The PATH + `hey` test scripts

All are tracked under `scripts/`. Each drives PATH with `hey` (and, where
noted, WebSocket load), then inspects the outcome via Redis/Loki. Env-var
overrides shown are read from each script's header.

**Sustained load / leak detection** — `scripts/test-continuous-load.sh`.
Streams ~200 RPS until Ctrl-C, printing success/failure every 30s; use it to
watch relayer/miner memory and CPU over time (pprof endpoints at `:6060` /
`:6065`).

```bash
./scripts/test-continuous-load.sh              # ~200 RPS
./scripts/test-continuous-load.sh --rps 300    # bump target RPS
```

**Multi-supplier fan-out** — `scripts/test-multi-supplier-10k.sh`. Sends 10k
relays with **no** supplier pinning (parallel `hey` batches), so the miner has
to claim/prove across every supplier's session concurrently.

```bash
./scripts/test-multi-supplier-10k.sh
TOTAL_RELAYS=20000 ./scripts/test-multi-supplier-10k.sh
```

> Note: the `relay` CLI now does supplier fan-out natively with
> `relay --all-suppliers` (round-robins across every supplier in the current
> session, avoiding one supplier's per-session claimable budget). For CLI-driven
> multi-supplier load see [`./DIRECT_CLI.md`](./DIRECT_CLI.md).

**Max stress (HTTP + WebSocket)** — `scripts/test-stress-max.sh`. Runs ~1000
RPS HTTP against `develop-http` plus 200 concurrent WebSocket connections
against `develop-websocket` for ~5 minutes, then asserts claims/proofs
succeeded via Loki. **Prerequisite:** a fresh cluster whose genesis sets
`compute_units_to_tokens_multiplier=100` and `granularity=1` (so claims clear
the proof fee) — `tilt down && tilt up` after adjusting genesis.

```bash
./scripts/test-stress-max.sh
```

**SMST-claim + WebSocket regression** — `scripts/test-smst-claim-and-ws.sh`.
Combines HTTP claim/proof load with concurrent WebSocket writes to verify two
fixes together: claims keyed on the SMST root hash, and the WebSocket mutex
that prevents concurrent-write panics.

```bash
./scripts/test-smst-claim-and-ws.sh
HTTP_RPS=300 WS_CONNS=100 ./scripts/test-smst-claim-and-ws.sh
```

**Backend round-robin distribution** — `scripts/test-round-robin.sh`. Sends N
relays (default 1000) and tallies `result.backend_id` per response to confirm
even distribution across the multi-backend HTTP service.

```bash
./scripts/test-round-robin.sh          # 1000 relays
./scripts/test-round-robin.sh 5000     # 5000 relays
```

## Related docs

- [`./README.md`](./README.md) — testing index.
- [`./TILT.md`](./TILT.md) — bring up / verify the localnet.
- [`./DIRECT_CLI.md`](./DIRECT_CLI.md) — signed relays direct to the relayer
  (error paths, per-transport, `--all-suppliers`).
- [`../CLAIM_PROOF_LIFECYCLE.md`](../CLAIM_PROOF_LIFECYCLE.md) — windows,
  batching, inclusion reconciler.
- [`../CLAIM_LEAF_MODEL.md`](../CLAIM_LEAF_MODEL.md) — how relays map to claim
  leaves.
- [`../REDIS.md`](../REDIS.md) — Redis key patterns behind the `redis`
  subcommand.
- [`../../scripts/loadtest/README.md`](../../scripts/loadtest/README.md) —
  backend RPS-ceiling and per-service pool tuning.
