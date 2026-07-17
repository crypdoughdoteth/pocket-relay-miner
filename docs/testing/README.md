# Testing the RelayMiner

How to exercise the relayer and miner on the local Tilt environment. There are
three ways to test, each with its own guide:

| Guide | What it's for |
|---|---|
| **[TILT.md](TILT.md)** | Bring the localnet up in kind, confirm it's healthy, the port map, and the HA/chaos suite. **Start here.** |
| **[PATH_HEY.md](PATH_HEY.md)** | Load and full-lifecycle testing **through the PATH gateway** with `hey`, the way production traffic flows. |
| **[DIRECT_CLI.md](DIRECT_CLI.md)** | Signed relays sent **straight to the relayer** (`:8180`) with the `relay` CLI — per-transport correctness and error paths. |

## Which one do I want?

- **First time / nothing running** → [TILT.md](TILT.md) to bring it up.
- **"How many RPS does the whole path carry?" / lifecycle → claims → proofs** →
  [PATH_HEY.md](PATH_HEY.md).
- **"Does protocol X actually work? Does the relayer reject bad input?"** →
  [DIRECT_CLI.md](DIRECT_CLI.md). Use this for the five transports
  (JSON-RPC, WebSocket, gRPC, streaming, CometBFT) and for anything
  error-related.

> **Key gotcha:** PATH masks relayer `503`s as `200` + empty body. So the PATH +
> `hey` path is throughput/lifecycle only — for error-path and negative testing,
> go direct with the CLI ([DIRECT_CLI.md](DIRECT_CLI.md)).

## Reference material

These are not testing walkthroughs but you'll want them while verifying results:

- [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) — claim/proof window
  timing and the block-driven inclusion reconciler.
- [../CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md) — how relays map to SMST
  leaves and on-chain relay counts (why a claim can show fewer relays than you
  sent).
- [../REDIS.md](../REDIS.md) — Redis key patterns and the `redis` debug
  subcommand in depth.
- [../../scripts/loadtest/README.md](../../scripts/loadtest/README.md) —
  backend RPS-ceiling measurement and per-service connection-pool tuning.
