# AGENTS.md

## Using `ai` from an agent

- Generate: `ai image --json -o ./out/ -m provider:model "prompt"`. Read file
  paths from `.results[].images[].file`. Exit code 0 means every model
  succeeded, 2 means a partial failure, and 1 means failure or invalid input.
- Discover: `ai providers --json` (signed-in providers), `ai models --json`
  (model IDs), `ai accounts --json`, `ai schema` (all commands and flags).
- Check before spending: `ai image --dry-run ...` resolves models, accounts
  and options without calling a provider.
- Pass `--no-stdin` (or set `AI_CLI_NO_STDIN=1`) when your stdin stays open;
  otherwise `ai image` waits for EOF on a piped stdin.
- Never rely on the TUI. It only opens when stdin and stdout are terminals; set
  `AI_CLI_NO_TUI=1` to be sure.
- Images as data: `--base64` or `--data-url` (one line per image), or
  `--json --base64` to embed them in the manifest.
- MCP: `ai mcp` exposes `generate_image`, `list_providers`, `list_models` and
  `list_accounts` over stdio.

## Developing

- Go 1.24+. Run `gofmt -w .`, `go vet ./...` and `go test -race ./...` before
  committing.
- Layout: `cmd/ai` (entry point), `internal/cli` (commands, TUI, MCP, run
  engine), `internal/provider` (one file per provider), `internal/auth`
  (multi-account store, Codex and Grok OAuth), `internal/media` (reference
  loading, media type sniffing), `internal/output` (files, Kitty previews,
  progress).
- New provider: add `internal/provider/<name>.go` that calls `Register` with an
  `Info` and implements `Provider`. Add a mock-server test to
  `provider_test.go`, add the provider to `providerPriority` in
  `internal/cli/creds.go`, and document it in `README.md`.
- New flags appear in `ai schema` automatically. Update the README options
  list and, if agents need the flag, the `generate_image` schema in
  `internal/cli/mcp.go`.
- Keep stdout machine-readable: progress, warnings and previews go to stderr.
