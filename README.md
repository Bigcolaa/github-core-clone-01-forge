# github-core-clone-01 — static Go git forge

A from-scratch, statically linked Git forge implemented in pure Go. It serves Git
smart HTTP (upload-pack v2, receive-pack v0), GitHub-compatible REST v3, GraphQL
v4, and MCP `2026-07-28`, with a port-0 / register / done bot lifecycle and a
`/healthz` endpoint. No external services, no runtime dependencies: a thin Node
22 launcher execs the committed static binary.

[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Bigcolaa/github-core-clone-01-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Bigcolaa/github-core-clone-01-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

See the [bounty on make.fun](https://make.fun/bounty/github-core-clone-01).

## Run

```sh
node main.ts
```

The launcher selects `forge-windows.exe` on Windows and `forge-linux` elsewhere,
copies it to a temporary directory, marks it executable, and starts it with the
environment supplied by the harness (`MAKEFUN_ENV_URL`, `MAKEFUN_SHOP_TOKEN`).

## Design

- Pure Go core, built with `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w"`.
- Pre-serialized issue GET response cache: after the first request, a read is a
  single `conn.Write` of pre-baked bytes.
- Sharded, lock-striped object store with no global mutex on hot paths.
- Pack generation uses full tree closures and explicit shallow-boundary handling,
  so clones, incremental fetches, and shallow fetches all deliver complete,
  minimal object sets.

## License

MIT — see [LICENSE](LICENSE).
