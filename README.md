# cpa-key-quota

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that limits, per API key, which models a key may call and how much it may use — by request count, tokens or cost — with manual reset and adjustment.

It works on CPA's own `access.api-keys`. It issues no keys of its own, so keys you already have are governed by it as they are; a key with no policy behaves exactly as before.

[中文说明](#中文说明)

## Features

- **Model allow / deny lists** per key. `*` matches any run of characters (including `/`), `?` matches one; case-sensitive; deny wins.
- **Limits** on requests, tokens or cost (USD), over a window of total / hour / day / week (Monday start) / month. A limit can apply to a subset of models; a key can have several.
- **Default policy** for keys without their own. Each key still gets its own counters.
- **Manual reset and adjustment** of any counter. Raising or lowering a limit keeps the usage so far.
- **Disable** a key without removing it from CPA.
- **Console** inside the CPA management center ("Key 配额" in the sidebar), at `/v0/resource/plugins/key-quota/console`. It reuses the management center's login.

## Behaviour

| Case | Response |
|---|---|
| Model not allowed | `403` `model_not_allowed` |
| Key disabled | `403` `key_disabled` |
| Any limit used up | `429` `quota_exceeded`, message names the reset time |
| Request without a CPA API key | not handled |

- Requests are counted before forwarding. A rejected request is not counted, and host retries of one request (same RequestID) count once.
- Tokens and cost are settled from the real usage after the response, so the last request let through may overshoot a token or cost limit; later ones are refused.
- Tokens = input + cache read + cache write + output (reasoning included). OpenAI-style counts (cache inside input) and Anthropic-style counts (cache separate) are normalized so nothing is counted twice.
- Cost uses the console's model prices in USD per million tokens. An exact model name beats a pattern; among patterns the longest wins. Models without a price cost 0.

Measured on the request path: admission takes about 0.3 µs with no allocation; the full RPC round trip about 2.3 µs.

## Requirements

- CLIProxyAPI with native plugin support (developed and tested against **v8.0.67**).
- Linux amd64 or arm64. Release builds need glibc 2.34 or newer; the official CPA image (Debian 12, glibc 2.36) qualifies.

## Install

1. Download the `.so` for your architecture from [Releases](https://github.com/R0smontis/cpa-key-quota/releases) and check it:

   ```bash
   sha256sum -c key-quota-v0.1.1_linux_amd64.so.sha256
   ```

2. Copy it into CPA's plugin directory under the name CPA expects:

   ```bash
   cp key-quota-v0.1.1_linux_amd64.so <plugins.dir>/linux/amd64/key-quota-v0.1.1.so
   ```

3. Enable it in CPA's `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     configs:
       key-quota:
         enabled: true
         data_dir: /data/plugins/key-quota   # policies, prices and counters live here
         timezone: Asia/Shanghai             # IANA zone for hour/day/week/month windows
   ```

4. Restart CPA. The log should show `plugin registered plugin_id=key-quota`.

If CPA sits behind a reverse proxy that allow-lists management paths, allow the three routes below and `/v0/resource/plugins/key-quota/console`.

## Management API

All routes require CPA's management key (`Authorization: Bearer <management key>`).

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v0/management/plugins/key-quota/state` | Body `{"scopes":[...]}`. Returns config, revision and live counters. |
| `PUT` | `/v0/management/plugins/key-quota/policy` | Replace policies, default policy and prices. Send `If-Match: "rev-N"` to avoid overwriting a concurrent edit (`412` on mismatch). |
| `POST` | `/v0/management/plugins/key-quota/usage` | Body `{"scope","limit_id","used"}`. `used: 0` resets; an empty `limit_id` targets every limit of the key. |

A key is identified by its CPA caller scope: `sha256("cli-proxy-api:caller-scope:v1\0" + key)` in hex. The console computes it in the browser; the raw key never reaches the plugin's storage.

Policy document:

```json
{
  "default": { "limits": [{ "id": "daily", "metric": "requests", "window": "day", "max": 500 }] },
  "policies": {
    "<caller scope>": {
      "label": "team-a",
      "allow_models": ["gpt-6*", "claude-*"],
      "deny_models": ["*-preview"],
      "limits": [
        { "id": "tokens", "metric": "tokens", "window": "month", "max": 50000000 },
        { "id": "claude-usd", "metric": "cost", "window": "month", "max": 20, "models": ["claude-*"] }
      ]
    }
  },
  "pricing": [
    { "pattern": "claude-opus-*", "input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75 }
  ]
}
```

Prices above are placeholders; enter your provider's current rates.

Do not add a route named `/config`: CPA itself owns `PUT /v0/management/plugins/:id/config`, which writes the body into the plugin's block in `config.yaml` and reloads (unloads) the plugin. `TestRoutesAvoidHostBuiltins` guards against that.

## Failure handling

- If the plugin call fails, CPA lets the request through (fail-open). That is host behaviour, not configurable here.
- A corrupt state file stops the plugin from starting rather than silently wiping limits.
- State is written to `data_dir/key-quota.json` (mode 0600, atomic replace). Counters are flushed every 2 s, so a crash loses at most about 2 s of counting.

## Build

The plugin is a cgo shared library and must be built on the target platform, with a glibc no newer than the host's:

```bash
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 -e GOAMD64=v1 golang:1.25-bookworm \
  go build -trimpath -buildmode=c-shared -ldflags "-s -w -X main.pluginVersion=0.1.1" -o key-quota-v0.1.1.so .
```

Add `--platform linux/amd64` when building on another architecture.

Tests: `CGO_ENABLED=1 go test -race ./...`. Benchmarks: `go test -run '^$' -bench .`.

Pushing a `v*` tag builds linux/amd64 and linux/arm64 in GitHub Actions and attaches them to a release.

## License

MIT

---

## 中文说明

按 Key 限制 CLIProxyAPI 的可用模型和用量的原生插件，可以按请求次数、Token 或金额（USD）限额，并支持手动重置和调整。它直接作用于 CPA 自带的 `access.api-keys`，不另外签发 Key。已有的 Key 不用改就能受策略约束；没有设置策略的 Key 和以前完全一样。

- 模型白名单 / 黑名单，支持通配符，黑名单优先。
- 限额：请求次数、Token、金额，周期可选累计、每小时、每天、每周（周一开始）、每月；每条限额可以只针对部分模型。
- 默认策略：对没有单独设置策略的 Key 生效，每个 Key 分开计数。
- 支持手动重置和调整已用量；修改上限不会清零已用量；可以停用 Key。
- 管理页在 CPA 管理中心左侧菜单的「Key 配额」，复用管理中心的登录状态。

超限返回 `429 quota_exceeded`，消息里包含重置时间；模型不允许或 Key 已停用返回 `403`。请求次数在转发前计入；Token 和金额在请求完成后按实际用量结算，所以放行的最后一次请求可能超出上限，之后的请求会被拒绝。插件调用出错时 CPA 会放行请求，这是 CPA 本身的行为。

安装：从 Releases 下载对应架构的 `.so`，改名为 `key-quota-v<版本>.so` 后放到 `<plugins.dir>/linux/<架构>/`，在 `config.yaml` 里启用（配置见上文），然后重启 CPA。
