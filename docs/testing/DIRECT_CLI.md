# Testing Relays Directly via the CLI

This is the **direct** way to test the relayer: the built-in `relay` command
sends signed relay requests **straight to a relayer replica** (`:8180`),
bypassing the PATH gateway. Use it when you need honest per-relay results —
signature verification, error codes, and per-protocol behavior.

## Why direct instead of PATH?

PATH masks relayer errors: when the relayer returns a `503`, PATH answers the
client with `200 OK` and an empty body. So a PATH+`hey` run (see
[PATH_HEY.md](PATH_HEY.md)) is great for throughput and lifecycle testing, but
it **cannot exercise the relayer's error paths** — a rejected relay looks like
a success at the gateway.

The `relay` CLI talks to the relayer directly and validates the full response
(supplier signature + the backend's own error field), so a failure is a
failure. This is the tool for correctness testing per protocol.

## Prerequisites

- A running localnet (see [TILT.md](TILT.md)).
- The binary: `make build` produces `./bin/pocket-relay-miner`. Examples below
  use `pocket-relay-miner`; substitute `./bin/pocket-relay-miner` if it isn't
  on your `PATH`.

## The four transports

The relayer serves four transports; the CLI has one mode per transport:

| Mode | Transport | Localnet service |
|---|---|---|
| `jsonrpc` | HTTP / JSON-RPC | `develop-http` |
| `websocket` | WebSocket | `develop-websocket` |
| `grpc` | native gRPC (h2c) | `develop-grpc` |
| `stream` | REST/SSE streaming | `develop-stream` |

## `--localnet`: zero-config defaults

`--localnet` fills in everything for the local Tilt environment — relayer URL
(`http://localhost:8180`), chain gRPC/RPC endpoints, chain ID, the default
supplier, and it **auto-selects the app key** for the `--service` you name
(each localnet service is staked to its own app). So the minimal invocation is
just a mode + `--service`:

```bash
# One JSON-RPC relay, full diagnostic output (timings, signature, payload)
pocket-relay-miner relay jsonrpc --localnet --service develop-http
```

A successful diagnostic prints `Status: ✅ SUCCESS`, `Signature: ✅ VALID`,
`Error Check: ✅ NO ERRORS`, and the decoded backend response.

## Single relay per protocol (smoke test)

```bash
pocket-relay-miner relay jsonrpc   --localnet --service develop-http
pocket-relay-miner relay websocket --localnet --service develop-websocket
pocket-relay-miner relay grpc      --localnet --service develop-grpc
pocket-relay-miner relay stream    --localnet --service develop-stream -n 3
```

Notes per protocol:

- **grpc** — by default the CLI sends a *real* unary gRPC request
  (`demo.DemoService/GetBlockHeight`) so it exercises the relayer's native
  gRPC (h2c) forwarding end to end, and prints the decoded `Block Height`.
  Passing `--payload '<json>'` instead sends a JSON-RPC body, which
  deliberately drives the relayer's REST fallback for gRPC relays.
- **stream** — the SSE stream is long-lived, so `-n` means **how many batches
  to collect** before closing (not "number of relays"). It does not use
  `--load-test`. Each batch is signature-verified individually.

## Load testing (`--load-test`)

Add `--load-test` with `-n` (total requests) and `--concurrency` (workers).
Optionally `--rps N` to cap the rate. Not supported for `stream`.

```bash
# 1000 JSON-RPC relays, 50 workers
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --load-test -n 1000 --concurrency 50
```

The summary reports total/successful/errors, success rate, throughput, and
p50/p95/p99 latency. A relay counts as **successful only** if the supplier
signature verifies **and** the decoded response carries no error — a signed
backend error (e.g. HTTP 500/415) is correctly counted as a failure with the
reason shown in the error breakdown.

### `--all-suppliers`: round-robin across the session

A single supplier exhausts *its* per-session claimable budget quickly while the
other session suppliers sit idle. `--all-suppliers` spreads relays across every
supplier in the current session, matching how a gateway distributes traffic:

```bash
# 60 relays fanned out across all session suppliers, per protocol
pocket-relay-miner relay jsonrpc   --localnet --service develop-http       --load-test -n 60 --concurrency 5 --all-suppliers
pocket-relay-miner relay websocket --localnet --service develop-websocket  --load-test -n 60 --concurrency 5 --all-suppliers
pocket-relay-miner relay grpc      --localnet --service develop-grpc       --load-test -n 60 --concurrency 5 --all-suppliers
```

The run logs `round-robining across session suppliers` with the supplier
count. For WebSocket the supplier is pinned at the handshake, so the pool
opens one connection per supplier; for HTTP/gRPC the supplier rotates
per request over the shared connection.

## The `[::1]:8180` gotcha

After a relayer pod restart, Tilt sometimes re-binds the `:8180` port-forward
to IPv6-only. If a run fails with `connection refused` on `127.0.0.1:8180`,
point the CLI at the IPv6 loopback explicitly:

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --relayer-url "http://[::1]:8180"
```

## Verifying the relays landed (claims / proofs)

A successful relay at the CLI only proves the relayer signed and served it. To
confirm it flowed through the miner into a **claim** (and a **proof** where
required), inspect the submission tracking in Redis after the session's claim
window closes (see [CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) for
window timing):

```bash
# Claim/proof status per session for one supplier
pocket-relay-miner redis submissions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj

# Only the failures
pocket-relay-miner redis submissions --supplier <addr> --failed-only

# Session lifecycle state, SMST tree, and the supplier registry
pocket-relay-miner redis sessions  --supplier <addr>
pocket-relay-miner redis smst      --session <session_id>
pocket-relay-miner redis supplier  --list
```

The `submissions` output shows, per session-end height and service, the
`CLAIM_STATUS`, `PROOF_STATUS`, `RELAYS`, and compute units.

### Fewer on-chain relays than you sent?

`RELAYS` in a claim can be lower than the number you fired. Two relays that
hash to the same SMST leaf (identical signed request bytes) collapse into one
on-chain relay — this is the dedup / anti-replay design, not a lost relay. The
CLI generates a fresh ring signature per request specifically to avoid this;
see [CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md) for the full model.

## Worked example: full economic cycle for one protocol

```bash
# 1. Note the current height and send a round-robin batch
curl -s http://localhost:26657/status | jq -r '.result.sync_info.latest_block_height'
pocket-relay-miner relay grpc --localnet --service develop-grpc \
  --load-test -n 60 --concurrency 5 --all-suppliers

# 2. Wait for the session end + claim/proof windows to pass (see CLAIM_PROOF_LIFECYCLE.md),
#    then confirm every supplier claimed and proved:
for s in $(pocket-relay-miner redis supplier --list | awk '/^pokt/{print $1}'); do
  pocket-relay-miner redis submissions --supplier "$s" | grep develop-grpc
done
```

A healthy run shows `✓ SUCCESS` claim + proof for each supplier that received
relays, and the on-chain `EventClaimSettled` for the session marks the claim
`VALIDATED` with the correct mint.

## See also

- [TILT.md](TILT.md) — bringing up the localnet and the port map.
- [PATH_HEY.md](PATH_HEY.md) — load testing through the PATH gateway.
- [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) — claim/proof windows and the inclusion reconciler.
- [../REDIS.md](../REDIS.md) — the `redis` debug subcommands in depth.
