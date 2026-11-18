# gitforge

[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/gatotz/gitforge/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/gatotz/gitforge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A single-process GitHub-compatible git forge: git smart HTTP, the REST v3
API, GraphQL v4 and MCP from one in-memory store. No UI, no database, no
git binary — one statically linked binary an agent starts inside a sandbox.

## Run

```sh
# Trial mode (the make.fun env drives it):
MAKEFUN_ENV_URL=http://store:8080 MAKEFUN_SHOP_TOKEN=<token> node main.ts

# Standalone mode (local practice): serves until killed.
MAKEFUN_SHOP_TOKEN=test-token FORGE_PORT=8080 node main.ts
```

`main.ts` (Node 22, standard library only) starts the committed Go binary
`./gitforge` and waits for it. The binary binds an ephemeral port,
registers it with the env, polls `/done`, and exits 0.

## Build

```sh
export GOROOT=$HOME/go GOPATH=$HOME/gopath PATH=$HOME/go/bin:$PATH
cd go && CGO_ENABLED=0 go build -ldflags="-s -w" -o ../gitforge .
```

## Layout

- `main.ts` — the entrypoint: starts the forge binary, forwards its exit code.
- `go/` — the forge source (Go, standard library only).
- `gitforge` — the statically linked Linux amd64 binary, built from `go/`.

## Behaviour

- Deterministic: forge-created commits carry a fixed timestamp, issue and
  pull request numbers are per-repository counters, and every list is
  sorted, so a live run and its replay behave byte for byte the same.
- The bearer token names the viewer `adaiversen`; every repository belongs
  to that login, and the same name under any other login is a 404.

## License

MIT — see [LICENSE](LICENSE).
