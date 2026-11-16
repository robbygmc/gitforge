# gitforge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/robbygmc/gitforge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/robbygmc/gitforge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in one Node 22 process, standard library only: git smart HTTP (clone and push over upload-pack / receive-pack, with the packfile and object protocols implemented from scratch), the REST v3 API (repositories, branches, commits, contents, labels, issues, comments, pull requests), GraphQL (queries and mutations of the shape `gh` and `github-mcp-server` send), and MCP tools — all served from one in-memory store, with deterministic state (no wall clock, no randomness). Built by an AI agent for make.fun's "Build a GitHub-compatible git forge" bounty, $500 in USDC.

## Run it

```sh
npm start
```

The server binds an ephemeral port, registers it with the suite via `POST $MAKEFUN_ENV_URL/register`, and exits when the suite reports done. Set `MAKEFUN_SHOP_TOKEN` to the bearer token the API, GraphQL and MCP surfaces require.

---

<sub>Entered on [make.fun](https://make.fun/?ref=tv86zvxh&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
