# ImagiFlow (ModelRouter + Executor example)

A CLIProxyAPI plugin that exposes a vision image model (`gemini-3.1-flash-image`)
through the **standard OpenAI image API** (`/v1/images/generations`), by bridging
the request to the model's native **`/v1/chat/completions`** interface.

## Why

The bridged gateway only serves `gemini-3.1-flash-image` through
`/v1/chat/completions` (calling `/v1/images/generations` for it returns `400`),
and returns the generated picture inside the chat response rather than in the
standard images API shape:

```jsonc
{
  "choices": [{ "message": { "content": null,
    "images": [{ "type": "image_url", "image_url": { "url": "data:image/jpeg;base64,..." } }]
  }}]
}
```

This plugin translates between the two shapes so a normal OpenAI image client
keeps working unchanged.

## Configuration

Multiple image models can be exposed at once via the `models` list; pick the
model by name on the client side and each entry targets its own gateway:

```yaml
plugins:
  configs:
    imagiflow:
      enabled: true
      priority: 100
      timeout_seconds: 90
      models:
        - model: gemini-3.1-flash-image
          base_url: "http://8.137.16.163:8317/v1"
          api_key: "721683736"
          default_size: "1024x1024"
        # - model: another-image-model
        #   base_url: "https://example.com/v1"
        #   api_key: "..."
        #   default_size: "1024x1024"
        #   image_from: content_text
        #   response_format: url
```

The legacy single-model fields (`model`, `base_url`, `api_key`, `default_size`)
are still honored when `models` is empty.

### Customizing how each model's image response is read

Different upstream image models may return the generated image in different
places of the chat/completions response. Two per-model fields let you customize
that without touching code:

- `image_from` — where to extract the image from. Defaults to `auto`:
  - `auto` — `choices[0].message.images[].image_url.url`, then `content`
    array `image_url` parts whose URL is a `data:image` URL.
  - `message_images` — only `message.images[].image_url.url`.
  - `content_image` — only `content` array `image_url` parts (any URL).
  - `content_text` — each `content[].text` value is treated as an image URL.
  - `data` — a top-level `data[]` array with `url` / `b64_json` entries.
- `response_format` — the default `url` or `b64_json` for this model, used
  when the client omits `response_format` (empty means default behavior:
  `b64_json` for data URLs, otherwise `url`).

Both are also supported at the plugin's top level as **global defaults**
applied to every model; a per-model value overrides the global one.

### Web UI (CPA management center)

The plugin registers its config via `ConfigFields`, so everything is editable in
the plugin settings of the CPA web UI:

| What you customize | Field | UI control |
|---|---|---|
| Model name | `model` | text input |
| Endpoint URL | `base_url` | text input |
| Endpoint API key | `api_key` | text input |
| Existing credential | `auth_index` | text input (or pick from auth list) |
| Default size | `default_size` | text input |
| Response (image location) | `image_from` | dropdown |
| Response (return format) | `response_format` | dropdown |
| Timeout | `timeout_seconds` | number input |
| Advanced multi-model | `models` | JSON editor |

The web-UI fields (model/base_url/api_key/default_size/image_from/response_format)
configure a single exposed model when `models` is left empty. For multiple
models, fill `models` as a JSON array in the same editor; each entry supports
`model`, `base_url`, `api_key`, `default_size`, `image_from`, `response_format`
and overrides the top-level defaults.

### Model management page

The plugin also registers a management resource page. In the CPA web UI it
appears as a **图像模型管理** menu entry under this plugin. The page is a table
editor where you manage the bridge models (model name, `base_url`, `api_key`,
`default_size`, `image_from`, `response_format`) with add/remove rows.

Saving writes the whole config back through the host Management API
(`PUT /v0/management/plugins/imagiflow/config`), so changes are
persisted to the config file and applied immediately (the plugin reconfigures
in place). This is an alternative to editing the `models` JSON by hand.

### Routing to an existing CPA model's interface

The plugin acts as a **router**: a standard `/v1/images/generations` request
for a configured model is forwarded to that model's upstream interface
(`<base_url>/chat/completions`) with its credentials.

Each `models` entry can select an existing CPA model in two ways:

- **From an existing credential** (recommended): set `auth_index` to the
  credential's `auth_index` and leave `base_url`/`api_key` empty. At runtime the
  plugin calls `host.auth.get` and reads `base_url`/`api_key` from that
  credential, so no upstream secrets are stored in the plugin config. In the
  management page, pick an auth file and click one of its models to add such an
  entry automatically.
- **Explicit interface**: set `base_url` and `api_key` directly (takes
  precedence over `auth_index`).

At the plugin top level, `auth_index` (with `model`) behaves the same way in
single-model mode.

## How it works

1. ModelRouter claims the request when the model matches the configured `model`
   and the body looks like an image generation request (top-level `prompt`).
2. The executor builds a chat/completions request toward the configured gateway:

   ```jsonc
   {
     "model": "gemini-3.1-flash-image",
     "messages": [{ "role": "user", "content": [
       { "type": "text",  "text": "<prompt>" },
       // optional: { "type": "image_url", "image_url": { "url": "data:...;base64,..." } } for reference/edit
     ]}],
     "size": "1024x1024"
   }
   ```

3. It posts to `{base_url}/chat/completions` with `Authorization: Bearer <api_key>`
   using its own config (no CPA upstream auth involved).
4. It reshapes `choices[0].message.images[].image_url.url` back into the standard
   images API response:
   - default: `{ "data": [{ "b64_json": "<base64>" }] }`
   - when the client requests `"response_format": "url"`: `{ "data": [{ "url": "<data URL>" }] }`

Single candidate per call (`n` is clamped to `1`), matching the gateway limit.

## Build

Requires Go 1.26 + a Linux C toolchain (`gcc`). Build on Linux (or a Linux build
container/WSL with a `gcc` cross toolchain):

```bash
cd imagiflow/go
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false \
  -buildmode=c-shared -ldflags "-X main.version=0.1.0" -o imagiflow.so .
rm -f imagiflow.h
```

## Install

```bash
# 1. Put the shared library into the CPA plugin directory
mkdir -p plugins/linux/amd64
cp imagiflow.so plugins/linux/amd64/glm-vision-bridge.so   # optional naming
#    (the plugin file basename should be stable and referenced by plugins.path)

# 2. Merge examples/plugin/imagiflow/config.example.yaml into config.yaml

# 3. Restart CLIProxyAPI
```

## Usage

Once installed and enabled, a normal OpenAI image client can call:

```bash
curl http://<cpa>:8317/v1/images/generations \
  -H "Authorization: Bearer $CPA_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-image",
    "prompt": "一只红苹果放在白色背景上，产品摄影",
    "size": "1024x1024"
  }'
```

The response is a standard images API payload (`data[].b64_json` by default), so
tools that already consume `/v1/images/generations` work unchanged.

### Reference / edit images

You can pass a source picture along with the prompt for conditioning or editing.
The plugin forwards these into the upstream chat request as `image_url` content
parts:

- `image` — an array of data URLs, bare base64 payloads, or remote URLs
  (bare base64 is wrapped into a `data:image/png;base64,...` URL automatically).
- `image_url` — an array of full image URLs.

```bash
curl http://<cpa>:8317/v1/images/generations \
  -H "Authorization: Bearer $CPA_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-image",
    "prompt": "把照片背景换成夜晚的城市",
    "size": "1024x1024",
    "image_url": ["https://example.com/input.jpg"]
  }'
```

## Notes / caveats

- **Routing integration must be validated on your running gateway.** CPA's
  `/v1/images/generations` handler classifies models and may transform unknown
  models into an internal Responses flow *before* plugin routers run. If your
  build routes the raw images payload to this executor, everything above applies;
  otherwise the request may reach the executor already reshaped. Verify with one
  call and adjust `routeModel` detection if needed.
- `api_key` is sent as-is to the configured gateway; keep it out of public
  configs (use CPA secret resolution if available).
- Images are returned inline as `b64_json`/data URL; set `response_format: url`
  if your client prefers a URL.
