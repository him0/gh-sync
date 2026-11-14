# CLAUDE.md

gh-sync is a GitHub CLI extension written in Go. README.md, the code and the config files under `.github/` and `mise.toml` describe the behavior, CI and release; this file lists only the rules they don't.

- Build, test and lint with `mise run build`, `mise run test` and `mise run lint`. `lint` runs the same checks as CI.
- When a change affects user-visible behavior, update both README.md and README.ja.md.
- The golangci-lint version is pinned in both `mise.toml` and `.github/workflows/ci.yml`; change them together.
- After adding a workflow or changing a `uses:` line, run `gh actions-lock` at the repository root and commit `.github/workflows/actions.lock` (requires `gh extension install github/gh-actions-lock`).
  - Never edit `actions.lock` by hand, and keep the full tags it writes (`@v7.0.1`, not `@v7`).
  - To advance a ref that moved upstream, review the upstream history, then run `gh actions-lock --relock`.
- To release, push a `v*` tag (e.g. `git tag v1.0.0 && git push origin v1.0.0`).
