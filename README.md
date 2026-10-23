# cpa-plugin-limit

[简体中文](README_CN.md)

CLIProxyAPI native plugin for **upstream-account concurrency limits and rolling
60-second request admission**. Defaults: **5 attempts per minute, 1 concurrent
request per credential**. All downstream API keys, sessions, and models selecting
the same credential share its limits.

Uses the same C ABI / JSON RPC / configuration-field architecture as
[cpa-plugin-xty](https://github.com/yinqf/cpa-plugin-xty). State lives in memory;
there are no background goroutines, storage writes, or upstream network calls.

## Admission contract

1. `request.intercept_before` starts request lifecycle tracking using CPA's `RequestID`.
2. `request.intercept_after` reads `selected_auth_index`, falling back to
   `selected_auth_id`. It atomically checks account concurrency and the rolling
   window before recording an admitted attempt.
3. The default `capacity-aware` scheduler checks capacity before selection and
   reserves attempts and slots atomically. Sessions keep their account while it
   has room, then move to another eligible candidate. New sessions favor lower
   load within the highest available priority tier; positive weights scale the
   load comparison and zero-weight candidates are excluded.
4. When every eligible candidate is full, the after-auth guard returns HTTP **429**,
   an OpenAI/Claude-compatible JSON error, and `Retry-After`. There is no queue.
5. `request.complete` releases the request's account reservations on success,
   failure, rejection, or cancellation. Repeated completion is harmless.

The plugin implements `scheduler.pick` and keeps bounded session bindings with a
default idle TTL of one hour. It accepts only the host's eligible candidate list,
preserving provider/model compatibility, cooldown exclusions, and pinned accounts.
Session identities come from host canonical metadata, explicit session headers,
body session/cache/thread fields, or derived execution metadata; requests without
a session are balanced without affinity. Caller/session namespaces are hashed in
memory. This does not reproduce CPA's complete LCP or hierarchical-session matcher.

An internal `X-Cpa-Limit-Request` header correlates selection with lifecycle;
it is overwritten before selection and removed after selection before execution.
Payloads, client session headers, and prompt cache keys are unchanged. Unexecuted
reservations are released on reselection or completion without consuming RPM.
Concurrency monitoring includes preselected pending slots.

CPA uses **one scheduler plugin at a time**. Choose `routing_mode: guard-only` and
reload the plugin if another scheduler must remain in control. This restores the
old behavior of rejecting a full selected account without rerouting. Mode changes
require reload because the host caches capability registration. The monitor shows
whether scheduler callbacks were observed. Home dispatch may bypass local routing;
the after-auth guard still protects observed admissions. A local refusal does not
report an upstream failure or start account cooldown.

Each admitted after-auth invocation consumes an attempt, including failed attempts
and retries that invoke the hook again. Completion never refunds RPM. Retries
within the same logical request reuse its account concurrency reservation. If a
request fails over across accounts, reservations on **all** admitted accounts are
conservatively held until that request completes because the host provides no
per-attempt terminal callback. A long-running stream never loses its reservation
just because 60 seconds elapsed.

## Configuration

Configure all eight fields in **CPA → Plugins → cpa-plugin-limit → Configuration**.
The plugin declares numeric, JSON-array, and JSON-object editors with defaults and
Chinese field descriptions. Saving uses CPA's existing plugin reconfiguration;
manual YAML editing is optional. Clearing a field restores its default.

Copyable JSON examples are available under **配置填写示例** on the plugin's
monitoring page. Configuration field descriptions use plain-text instructions
so the host's HTML sanitization does not expose encoded quotation marks.

Requires a plugin-enabled CPA build with RPC schema **2+**, request termination,
and lifecycle completion. The SDK dependency matches the reference plugin
(`v7.3.4`); native ABI 1 and the schema-2 contract also work with the current v8 host.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-plugin-limit:
      enabled: true
      requests_per_minute: 5
      max_concurrency: 1
      # Empty means all upstream credentials, across all providers.
      auth_indexes: []
      account_groups: {}
      account_limits: {}
      account_display: email
      routing_mode: capacity-aware
      session_affinity_ttl_seconds: 3600

routing:
  session-affinity: true
  session-affinity-ttl: "1h"
```

The eight plugin fields are declared for CPA's visual plugin configuration editor.
Numeric limits must be integers in `0..10000`; **0 disables that dimension**.
Omitted fields use defaults; omitted per-account fields inherit global values.
Standard host fields such as `enabled`, `priority`, and `store` are accepted.

`account_display` selects `email` (default), `masked_email`, or `fingerprint` in
the configuration editor. Email modes show only the email without a fingerprint;
missing addresses and failed lookups have explicit placeholders. Long addresses
wrap so the full email remains visible. Display settings do not change account
identity or admission limits.

`routing_mode` defaults to `capacity-aware`; `guard-only` disables plugin routing.
`session_affinity_ttl_seconds` accepts integers in `0..86400`; zero disables plugin
affinity. Full accounts cause the session to rebind to the selected replacement.

To restrict only Codex credentials, list their indexes in `auth_indexes`. The
plugin does not infer providers from model names or protocol formats, since
different providers can use the same format. **By default every selected upstream
credential is limited.**

### Multiple credentials for one real account

Credential indexes are not necessarily real provider account IDs. Explicitly group
duplicate imports of the same account; these groups share one bucket:

```yaml
auth_indexes: [codex-index-a, codex-index-b]
account_groups:
  codex-index-a: codex-shared
  codex-index-b: codex-shared
account_limits:
  codex-shared:
    requests_per_minute: 5
    max_concurrency: 2
```

Without a group, an override key is the credential index (or fallback credential
ID). Once an index and ID appear together, their aliases share a bucket, including
later ID-only callbacks. A configured group selected by `auth_indexes` includes
its other mapped members too.

Threshold reconfiguration preserves active reservations and recent history.
Changing `account_groups` is rejected while any account has an active reservation
or a recent admitted attempt. Drain traffic and wait for the 60-second window to
expire first. Restarting or replacing the plugin binary clears all memory and can
also overlap old requests; drain before reload when strict accounting matters.

## Monitoring

CPA's **账户限流监控** plugin menu opens the Chinese monitoring page:

```text
/v0/resource/plugins/cpa-plugin-limit/ui
/v0/resource/plugins/cpa-plugin-limit/status
```

The page follows CPA's typography, cards, controls, and tables. Same-origin
embedding synchronizes host color tokens and the light, white, and dark themes;
host-injected theme tokens are also supported. When opened independently or
embedded across origins, an appearance selector provides explicit themes. Auto
mode uses the same-origin CPA theme preference when available, then the system
preference. No CPA or management frontend changes are required.

Four summary cards show occupied account slots, attempts admitted in the last
60 seconds, lifetime admitted attempts, and lifetime rejections. Account rows show
utilization bars, rate/concurrency status, retry timing, and rejection breakdowns.
The page also shows effective global limits and scope/group/override counts.
It refreshes every five seconds by default; select manual, 10-second, or 30-second
refresh instead. Polling pauses while hidden; failures keep the last snapshot and
show a stale-data warning.

Email labels come from the host's `host.auth.list` callback and only its explicit
email field. File names, account strings, and labels are never treated as email
addresses. Lookups are cached for 30 seconds and shared with scheduler ID-to-index
resolution. Host callbacks never hold the admission mutex. Aliases and observed group members deduplicate
email labels. Credentials with matching emails retain separate limit buckets.
Unsupported or failed callbacks display an email lookup failure placeholder.
Without directory resolution the scheduler uses observed aliases; unknown grouped
credentials defer bucket creation to the after-auth guard. This can degrade the
first unknown credential selection to guard-only behavior while preserving limits.

These modern resource routes are **not management-authenticated by CPA**. They
expose counters, thresholds, timestamps, 16-character SHA-256 fingerprints, and
full email labels by default. **Restrict resource access at your gateway because
full email addresses are returned through the public status endpoint.** The
fingerprint is retained in the API for correlation and is not shown in email modes.
Credential indexes, IDs, group names,
client keys, session IDs, request IDs, headers, and bodies are never returned.
No deprecated `/v0/management` routes are added.

`status` returns `version`, `observed_at`, `quiescing`, `active_requests`,
`active_slots`, `window_requests`, lifetime `admitted`/`rejected` attempt counters,
`rate_rejected`, `concurrency_rejected`, effective `defaults`, redacted `scope`
counts, `account_display`, `email_lookup_available`, `routing_mode`,
`session_affinity_ttl_seconds`, `affinity_sessions`, optional `scheduler_last_seen`,
`pending_attempts`, and `items`. Each item contains
`account` (always a fingerprint), an optional `label` (email display),
`active`, `window_requests`, configured limits, `rate_retry_after_seconds`, and
admitted/rate-rejected/concurrency-rejected counters. Idle buckets disappear once
their history expires, so per-account counters then reset. `window_requests` is
capped at 10000 while RPM is disabled. The concurrency retry hint is one second;
the actual release time depends on the running request.

State is bounded to 4096 accounts and 16384 active request trackers and session bindings, plus 16384
recent completion IDs. Exhaustion or missing required host identity returns **503**
instead of silently bypassing protection. Completion is asynchronous in CPA, so a
recently finished request may briefly retain its slot until notification arrives.

## Host boundaries

This plugin protects **admissions observed by the host hooks**, not every possible
upstream wire send:

- CPA executor-internal resends, including some unauthorized-refresh retries, can
  bypass `request.intercept_after` and are not additional RPM admissions.
- Follow-up `response.create`/`response.append` frames and steering on an existing
  Codex duplex WebSocket can bypass both request hooks. A socket may hold one
  logical-request slot for its entire lifetime. This is not a per-turn or an idle
  WebSocket connection-pool limiter.
- Existing upstream connections are not interrupted. No request, stream, or
  network timeout is introduced.
- Counts are process-local. Multiple CPA instances, direct provider usage, plugin
  unload, and host plugin failures require separate coordination or host support.
  CPA can skip disabled/fused plugins; an unloaded limiter cannot enforce limits.

Strict accounting of every upstream attempt or duplex turn requires corresponding
host admission/release hooks. This repository does not change CPA's source.

## Build and verify

Go 1.26+ and a C compiler are required for the shared library. On macOS:

```sh
gofmt -w .
go vet ./...
go test -race ./...
mkdir -p build
go build -buildmode=c-shared -o build/cpa-plugin-limit.dylib .
python3 scripts/smoke_native.py build/cpa-plugin-limit.dylib
```

Use `.so` on Linux and `.dll` on Windows. Copy **only the library** into CPA's
plugin directory, enable it, and drain/restart CPA as appropriate. No Go SDK is
needed on the deployment host. The output filename must retain the plugin ID
`cpa-plugin-limit` to match the configuration and resource paths.

Tests use a controlled clock for rolling-window boundaries and real concurrent
goroutines for atomic admission and completion races. The native smoke script
loads the actual library through its C ABI using dummy accounts; it makes no
billable requests and does not prove upstream or duplex-turn coverage.

CI runs formatting, vet, race tests, shared-library compilation, and the native
smoke test. Pushing a `vX.Y.Z` tag triggers a release build for Linux amd64 only.
Other platforms can be built manually. Archives follow
`cpa-plugin-limit_<version>_<goos>_<goarch>.zip` and include `checksums.txt` in the
release. `registry.json` uses the same store format as the reference plugin; no
release, registry upload, or deployment is performed by local builds.
