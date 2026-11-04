# ai

Generate images from your terminal, in Go.

This is a Go port of the image generation part of
[vercel-labs/ai-cli](https://github.com/vercel-labs/ai-cli). On top of the
original Vercel AI Gateway support it adds direct providers, subscription
sign-in, multiple accounts per provider, an interactive TUI, and an
agent-friendly interface (`--json` on every command, `ai schema`, `ai mcp`).

| Provider | Name | Sign-in |
|---|---|---|
| Vercel AI Gateway | `gateway` | API key (`AI_GATEWAY_API_KEY`) |
| OpenAI, or any OpenAI-compatible server | `openai` | API key (`OPENAI_API_KEY`), optional custom base URL |
| ChatGPT subscription (Codex) | `codex` | Browser or device sign-in, or import from Codex CLI |
| Grok subscription (SuperGrok / X Premium) | `grok` | Browser or device sign-in |
| xAI API | `xai` | API key (`XAI_API_KEY`) |
| Higgsfield | `higgsfield` | Key ID + secret (`HF_KEY=id:secret`) |
| fal | `fal` | API key (`FAL_KEY`) |
| Replicate | `replicate` | API token (`REPLICATE_API_TOKEN`) |
| Black Forest Labs | `bfl` | API key (`BFL_API_KEY`) |
| Google Gemini / Imagen | `google` | API key (`GEMINI_API_KEY`) |
| Stability AI | `stability` | API key (`STABILITY_API_KEY`) |
| OpenRouter | `openrouter` | API key (`OPENROUTER_API_KEY`) |

## Install

```bash
go install github.com/lassejlv/ai-cli/cmd/ai@latest
```

Or build from a checkout with `make build`. Requires Go 1.24+.

## Quick start

```bash
ai login openai          # or: codex, grok, higgsfield, gateway, fal, ...
ai image "a cute dog"
ai                       # open the interactive TUI
```

In terminals that support the [Kitty graphics protocol](https://sw.kovidgoyal.net/kitty/graphics-protocol/)
(Kitty, Ghostty, WezTerm, Warp, iTerm2, Konsole), generated images are shown
inline. Inside tmux, enable `set -g allow-passthrough on` for previews.

## Signing in

```bash
ai login                                   # pick a provider interactively
ai login openai                            # prompts for the key (hidden)
echo "$OPENAI_API_KEY" | ai login openai   # reads the key from stdin
ai login openai --base-url http://localhost:8080/v1 --account local   # OpenAI-compatible server
ai login codex                             # ChatGPT subscription, browser sign-in
ai login codex --device                    # headless: device code
ai login codex --import                    # reuse ~/.codex/auth.json from the Codex CLI
ai login grok                              # SuperGrok / X Premium, browser sign-in
ai login grok --device
ai login higgsfield                        # prompts for key ID and secret
```

Credentials are saved to `~/.config/ai-cli/auth.json` (mode 0600). For
providers with a key-check endpoint (AI Gateway, OpenAI, xAI, Replicate,
Stability AI, OpenRouter), the key is checked before it is saved; pass
`--no-verify` to skip the check. Environment variables take priority over saved keys unless an
account is named explicitly.

When no `codex` account is saved, `ai` uses the tokens of an existing
`codex login` from `~/.codex/auth.json` and writes refreshed tokens back so
both tools stay signed in.

Subscription sign-in uses the same OAuth clients as the official Codex and
Grok CLIs, and generation counts toward your plan's usage limits. Access to
image generation depends on your plan tier.

### Multiple accounts

Each provider can hold several named accounts:

```bash
ai login openai --account personal
ai login openai --account work --default
ai accounts                       # list accounts
ai accounts use openai personal   # change the default account
ai image -m openai@work:gpt-image-2 "a logo"
ai image --account work "a logo"  # account for unprefixed models
ai logout openai --account work
ai logout --all
```

## Generating images

```bash
ai image "a lighthouse at dusk"
ai image -m openai:gpt-image-2 --param quality=high --param background=transparent "a sticker"
ai image -m higgsfield:soul --aspect-ratio 16:9 "editorial portrait in soft daylight"
ai image -m codex:gpt-image-2 "a watercolor fox"          # ChatGPT subscription
ai image -m grok:grok-imagine-image-2.0 "a neon street sign"  # Grok subscription
ai image -m fal:flux -n 4 -o ./renders/ "isometric city"
ai image -m "openai:gpt-image-2,bfl:flux-2-pro,higgsfield:soul" "a red bicycle"   # compare models
```

### Models

Models are written `provider[@account]:model`. Comma-separate several to
compare them side by side. A model without a provider prefix uses the default
provider: `--provider`, then `AI_CLI_IMAGE_PROVIDER`, then
`ai providers default <name>`, then the first configured provider.

```bash
ai models                 # all providers; AI Gateway models are fetched live
ai models higgsfield      # one provider, including short aliases like soul, recraft
ai providers              # which providers are signed in
```

Any model ID the provider accepts works, not only the listed ones, for example
`fal:fal-ai/some/new-model` or `replicate:owner/model:version`.

### References and edits

```bash
ai image -i sketch.png "render this as a product photo"
ai image -i subject.png -i style.png "combine these"
ai image -i photo.png --mask mask.png "replace the background"
cat photo.png | ai image "make it look like a pencil drawing"
echo "a tiny robot" | ai image                   # text on stdin becomes the prompt
```

References accept local paths, `file://`, HTTP(S) and data URLs. Higgsfield
needs public URLs, so local files are uploaded through its file upload API
first.

### Options

```text
-m, --model <id>               provider[@account]:model, comma-separated for several
    --provider <name>          Provider for unprefixed model IDs
    --account <name>           Saved account for unprefixed model IDs
-n, --n <n>                    Images per model (default 1)
-i, --images <ref>             Reference image (repeatable)
    --mask <ref>               Mask image for edits
    --size <WxH>               Image size, e.g. 1024x1536
    --aspect-ratio <W:H>       Aspect ratio, e.g. 16:9
    --seed <int>               Seed, including 0
    --param key=value          Provider option (repeatable, see below)
    --provider-options <file>  JSON options keyed by provider name
    --headers <file>           JSON object of extra HTTP headers
    --base-url <url>           Override the provider API base URL
    --scope <team>             AI Gateway team ID or slug
    --max-images-per-call <n>  Batching limit per provider call
-p, --concurrency <n>          Parallel models and batches (default 4)
    --timeout <seconds>        Per-request timeout (default 300)
    --max-retries <n>          Retries for 429/5xx/network errors (default 2)
-o, --output <path>            Output file or directory (trailing / creates a directory)
    --raw                      Write raw image bytes to stdout (single image)
    --base64                   Print each image as base64, one per line
    --data-url                 Print each image as a data: URL, one per line
    --no-save                  Do not write files
    --no-preview               Disable Kitty graphics previews
    --json                     JSON manifest on stdout
    --dry-run                  Print the resolved request without calling a provider
-q, --quiet                    Suppress progress
-I, --interactive              Open the TUI prefilled with these flags
```

`--param` values are parsed as JSON when possible (`n=2`, `transparent=true`,
`colors=["red"]`), dotted keys nest (`imageConfig.imageSize=4K`), and a
`provider:` prefix scopes a value to one provider when comparing models
(`--param higgsfield:enhance_prompt=true`). Options pass through to the
provider's request body unchanged, so every provider-specific setting is
reachable. For the AI Gateway they are keyed under the model's creator
(`openai/gpt-image-2` → `providerOptions.openai`).

`--size` and `--aspect-ratio` are translated where a provider has its own
format. When a provider does not support a setting, the CLI warns instead of
failing; for example, GPT Image maps an aspect ratio to the nearest supported
size.

### Output

- By default images are saved as `<response-id>.<ext>` in the current
  directory, `-o`, or `AI_CLI_OUTPUT_DIR`, and previewed inline.
- With several images, filenames get a `-<model number>-<image number>` suffix.
- When stdout is piped, a single image is written to stdout as raw bytes.
- `--raw`, `--base64` and `--data-url` write to stdout and save files only when
  `-o` is given.
- `--json` writes a manifest with every file, its media type, size and
  dimensions, plus warnings, usage, batches and failures. Add `--base64` to
  embed the image data.

```bash
ai image --raw "an icon" > icon.png
ai image --base64 "an icon" | head -c 60
ai image --data-url "an icon"              # paste into HTML or Markdown
ai image --json --base64 "an icon" | jq -r '.results[0].images[0].base64'
```

Exit codes: `0` success, `1` invalid input or every model failed, `2` some
models failed. Fewer images than requested counts as a failure; the images that
were returned are still saved.

## Interactive TUI

Run `ai` (or `ai tui`, or `ai image` without a prompt) in a terminal. Pick a
provider (with an inline sign-in if needed), an account, a model and a prompt,
then adjust aspect ratio, size, count, seed, references and provider options.
After each run, generate again, edit the prompt, change settings, switch
models, or use the result as a reference for an edit. Previews render inline
between steps. Set `AI_CLI_NO_TUI=1` to never open it automatically.

## For agents and scripts

- Every listing command takes `--json`: `ai providers --json`,
  `ai accounts --json`, `ai models --json`, `ai login ... --json`.
- `ai image --json` writes only the manifest to stdout. Progress goes to
  stderr, and it is suppressed with `--json` or `-q`.
- `ai image --dry-run` shows the resolved models, accounts, credential sources
  and options without calling a provider.
- `ai schema` describes every command, flag, provider, environment variable
  and exit code as JSON.
- The TUI never opens when stdin or stdout is not a terminal.
- Non-interactive login: pipe the key on stdin, or pass `--api-key` (and
  `--api-secret` for Higgsfield).

### MCP server

`ai mcp` runs a Model Context Protocol server over stdio with the tools
`generate_image`, `list_providers`, `list_models` and `list_accounts`.

```json
{
  "mcpServers": {
    "ai": {
      "command": "ai",
      "args": ["mcp"],
      "env": { "AI_CLI_OUTPUT_DIR": "/absolute/output/dir" }
    }
  }
}
```

`generate_image` saves files and returns the manifest. Set
`return_images: true` to also get the images inline.

## Environment variables

| Variable | Description |
|---|---|
| `AI_GATEWAY_API_KEY`, `OPENAI_API_KEY`, `XAI_API_KEY`, `FAL_KEY`, `REPLICATE_API_TOKEN`, `BFL_API_KEY`, `GEMINI_API_KEY`, `STABILITY_API_KEY`, `OPENROUTER_API_KEY` | Provider keys; take priority over saved accounts |
| `HF_KEY` (`id:secret`), or `HF_API_KEY` + `HF_API_SECRET` | Higgsfield credentials |
| `OPENAI_BASE_URL`, `OPENAI_ORGANIZATION`, `OPENAI_PROJECT` | OpenAI connection settings |
| `AI_GATEWAY_BASE_URL`, `AI_GATEWAY_TEAM_ID_OR_SLUG` | AI Gateway connection settings |
| `CODEX_HOME` | Location of the Codex CLI's `auth.json` |
| `AI_CLI_IMAGE_PROVIDER` | Default provider for unprefixed models |
| `AI_CLI_IMAGE_MODEL` | Default model, optionally with a provider prefix |
| `AI_CLI_OUTPUT_DIR` | Default output directory |
| `AI_CLI_PREVIEW` | `1` forces Kitty previews on, `0` disables them |
| `AI_CLI_PREVIEW_WIDTH` | Preview width in terminal columns |
| `AI_CLI_NO_TUI` | Never open the TUI automatically |
| `AI_CLI_CONFIG_DIR` | Directory for `auth.json` (default `~/.config/ai-cli`) |
| `NO_COLOR`, `FORCE_COLOR` | Color output |

## Development

```bash
make test    # go test -race ./...
make build   # ./ai
```

Providers live in `internal/provider`, one file each. A provider registers an
`Info` (name, default model, auth type, env vars) and implements `Generate`
and `MaxImagesPerCall`. Tests run every provider against a mock HTTP server.

## License

[Apache-2.0](LICENSE). Derived from
[vercel-labs/ai-cli](https://github.com/vercel-labs/ai-cli); see [NOTICE](NOTICE).
