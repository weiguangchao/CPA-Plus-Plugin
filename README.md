# CPA Plus

A CLIProxyAPI v8 scheduler plugin. It selects an account whose five-hour and
seven-day quotas both have remaining capacity, preferring the soonest five-hour
reset. If no eligible account has a five-hour reset, it prefers the soonest
seven-day reset. It caches the selected account per Provider and model and reuses
it while CPA still offers it as a candidate. If neither selection succeeds, it
leaves scheduling to CPA.

[中文说明与配置](README_CN.md)

Versions use the `0.1.x` series. The current version is `0.1.6`; the root `VERSION`
file supplies both plugin registration metadata and package filenames. Make
injects the version at build time, so the library does not read this file at
runtime. Direct Go builds without the version linker flag report `dev`.

Go sources and the C ABI header are under `src/`. Go tests are in
`test/`, and Python scripts are in `scripts/`. Run Make commands from the
repository root. The version file, Go module files,
configuration example and documentation remain at the root. Go code emits no
application logs; API failures still fall back to CPA as described below.

Use `make test` and `make check` to run the Go checks. The runner uses a temporary
Go overlay to compile the `test/` files in the source package so they can access
unexported functions. It leaves both directories unchanged and preserves the
existing test coverage. Plain `go test ./...` does not apply this mapping. To
run a specific test, use
`python3 scripts/go-check.py test -race ./... -run TestSchedulingOrder`.

Quota results live in memory only. No quota provider plugin is required. The
plugin lists credentials through CPA and uses
`POST /v8/management/requests/api-call` to have CPA query upstream usage APIs.
CPA sends the upstream requests; this plugin parses quota and reset fields.
All current adapters are enabled by default: `claude`, `anthropic` and `codex`.
The `providers` setting has been removed. Other providers delegate to CPA. These usage endpoints require compatible OAuth
credentials; API keys may fail and delegate to CPA.

The plugin form exposes five fields in this order: `cpa_url`, `management_key`,
`cache_ttl`, `request_timeout`, `retry_interval`. Configuration keys retain these
names and their descriptions are in Chinese. The old `providers` setting no
longer restricts account selection.

Build with Go 1.26.10 or later, CGO enabled, and a C compiler:

```sh
make check
make build
```

Copy `build/cpa-plus.so`, `.dylib`, or `.dll` to CPA's plugin directory. Merge
[config.example.yaml](config.example.yaml) into CPA's existing configuration.
Enter CPA's management key directly in the plugin form's `management_key` field
or the same YAML field. CPA stores it in the plugin configuration. An empty key
keeps the form available but delegates all scheduling without API calls.
Replace the old `management_key_env` setting with the actual `management_key`;
environment lookup is no longer used.
The library basename must be `cpa-plus` to match the configuration key.

Plugin metadata identifies `weiguangchao` as the author and
https://github.com/weiguangchao/CPA-Plus-Plugin as the source repository.

To build and package Linux amd64 on a Linux amd64 host with Go, a C compiler,
Make and Python 3:

```sh
make package-linux-amd64
python3 scripts/smoke.py build/linux-amd64/cpa-plus.so
```

The library is `build/linux-amd64/cpa-plus.so`. The package and SHA-256 checksum
are `dist/cpa-plus_0.1.6_linux_amd64.tar.gz` and the adjacent `.sha256` file. The
archive includes the library, version, example config, both READMEs and LICENSE.

For cross-compilation, put Zig in `PATH` and run the same Make target. It defaults
to `zig cc -target x86_64-linux-gnu.2.17` and uses `GOAMD64=v1`. To use another
cross-compiler, set `LINUX_AMD64_CC=x86_64-linux-gnu-gcc` on the Make command.
These builds target glibc Linux. Native builds depend on their C toolchain's
glibc baseline; Alpine/musl needs a separate build. Run the `.so` smoke test on
Linux amd64, since macOS cannot load a Linux shared library.

Run the native ABI smoke test with `make smoke`. To also check loading in a real
CPA binary, use:

```sh
python3 scripts/smoke.py build/cpa-plus.dylib --cpa /path/to/cli-proxy-api
```

The SDK is pinned to CPA v8.0.47. Build for the OS and CPU architecture of the CPA
process. Home mode bypasses local scheduler plugins. Only the highest-priority
active scheduler plugin runs. Direct plugin selections bypass CPA's built-in
session affinity. Default fallback is unrestricted by this plugin's quota filter.

Requests with a nonempty `Options.Metadata.pinned_auth_id` delegate directly to
CPA. They make no management API calls and do not read or update the ordinary
selection cache, so a pinned connection cannot overwrite another connection's
preferred account. A single candidate without a valid pin still uses quota
scheduling. The plugin does not unpin or migrate existing connections.

The plugin never disables credentials, clears cooldowns, resets upstream quotas,
or persists quota data. Unsupported windows, unsuccessful lookups and scheduling
wait timeouts delegate to CPA. Every refresh
sends a new usage request through CPA. Concurrent clients can consume quota
between observations.

There is no periodic polling or startup query. The first scheduling request with
supported candidates queues one background refresh: list CPA credentials, then
fetch quota and reset times only for matching candidates. A request without a
valid cached selection waits up to `request_timeout`, normally 10 seconds,
including queue time. If a suitable account is found within this limit, the
same request uses it. Otherwise the request delegates to CPA.

Later requests reuse the selected account until it leaves CPA's candidates, the
five-minute `cache_ttl` expires, or a known relevant reset time arrives, whichever
happens first. Resets from other queried candidates also end the cache, since
those accounts can become preferable. The next request triggers a new refresh;
idle plugins issue no API calls. Cache hits do not extend the deadline, and new
candidates do not preempt a still-valid selection. Tied accounts rotate only on
reselection.

Concurrent misses for a route share one refresh, with a separate wait deadline
for each request. Waiting does not hold the cache lock or block valid cache hits.
After a wait timeout, the background refresh continues and can populate the
cache for later requests. All routes use one API worker,
which queries accounts sequentially. Failed or unsuccessful selections delegate
to CPA and wait at least `retry_interval`, normally 20 seconds, before a later
request can retry. A known earlier reset can shorten that wait. Individual API
requests use a 10-second timeout. Remove the old `refresh_interval` setting; it
is no longer used. Reloads clear all caches and wait for the next request.

CPA handles cooldown filtering. A reused selection confirms both quotas from the
last query, not their live upstream values. Same-ID credential replacement is
discovered on the next refresh. Large candidate pools
or many models can delay refreshes; results past their deadline are discarded.

## Usage API adaptation

Each refresh calls `GET /v8/management/credentials`, then forwards one usage
request per matching, enabled candidate with an `auth_index`. The forwarding
request contains the credential index, HTTP method, upstream URL and headers.
CPA replaces `Bearer $TOKEN$` with the ordinary account's current token and
applies credential or global proxy settings. The plugin checks the upstream
`status_code` inside the response, so outer HTTP 200 with upstream 401 or 429
is a failed observation. Token refresh remains CPA's responsibility.

Claude uses `GET https://api.anthropic.com/api/oauth/usage`. The plugin reads
`five_hour` and `seven_day`, converts `utilization` to remaining capacity and
reads `resets_at`. Model-specific weekly limits also apply to the corresponding
model. Fable supports active `weekly_scoped` entries in `limits[]`, with the
legacy percentage-based `iguana_necktie` window as a fallback.

Codex uses `GET https://chatgpt.com/backend-api/wham/usage`. The plugin identifies
five-hour and seven-day windows by `limit_window_seconds`, reads `used_percent`,
and uses `reset_at`, or `reset_after_seconds` when no valid absolute reset exists.
An explicit blocked rate limit prevents selection. Relevant additional model
limits apply, while code-review limits do not affect inference scheduling.
Ordinary Codex requests include `id_token.chatgpt_account_id` when CPA provides it.
Neither adapter requests subscription details or active-reset credits.

SIWC-enabled Codex accounts require one extra CPA request per refresh:
`GET /v8/management/credentials/download?name=...`. The plugin uses only the nested
`siwc.access_token`, omits the outer account header, and checks `expires_at`,
`expiresAt` or JWT `exp`. Missing, expired or undownloadable credentials are
skipped, including runtime-only SIWC accounts without a downloadable file. It
never falls back to the outer token. Downloaded credentials and tokens are not
persisted or retained in the selection cache.

Both shared windows must report available capacity; model-specific windows
cannot replace them. Missing or invalid usage percentages are unknown, and
monthly or other durations cannot replace five-hour or seven-day windows.
Remove the old `model_aliases` and `window_aliases` settings. The plugin uses
`Model` from CPA's scheduling request directly and does not map model aliases.
Existing quota plugins can be removed; this plugin no longer discovers or calls them.

## Priority, timeout and retry settings

Cross-priority scheduling is always enabled. The plugin receives available
candidates across all CPA credential priority tiers and can choose a lower-priority
account with an earlier reset. Cooldown, disabled-account and model filters still
apply. The plugin's own `priority` remains a CPA setting, and valid cached
selections remain sticky. Remove the old `schedule_across_priorities` setting;
it no longer controls this behavior, including when set to `false`.

`request_timeout` defaults to `10s`. It limits each background management API
request, including credential listing, usage forwarding and SIWC downloads.
It also limits how long a scheduling request without a valid cached selection
waits for a result, including queue time and any reselection needed to match
its candidates. This wait deadline does not restart after a refresh completes.
A wait timeout delegates the current request to CPA while the refresh continues.
Sequential account queries can take longer than this limit in total. This
setting does not limit inference execution time. Plugin shutdown cancels the
refresh and wakes waiting scheduling requests.

`retry_interval` defaults to `20s` after an unsuccessful refresh completes.
Once the delay expires, a new scheduling request can trigger another refresh.
A known earlier quota reset can shorten the wait. There are no timer-driven
retries, and this setting does not affect CPA inference retries or cooldowns.

## Releases from tags

GitHub Actions checks and builds Linux amd64 on pushes to `main` and pull
requests. Pushing a `v0.1.x` tag also publishes a GitHub release after the checks
pass. The tag must match the root `VERSION` exactly, prefixed with `v`.

For example, when `VERSION` contains `0.1.6`, run:

```sh
git tag v0.1.6
git push origin v0.1.6
```

For the next release, update `VERSION`, commit and push the changes before
creating the matching tag. A mismatched tag fails validation and publishes
nothing. Tags outside `v0.1.*` do not trigger this workflow.

The workflow runs race tests, vet and formatting checks, builds with Zig 0.15.2
for Linux amd64 with a glibc 2.17 target, and loads the resulting library for ABI
and scheduling checks on Linux. The release includes the `.tar.gz` package and
its `.sha256` file with generated release notes. It uses the repository's
`GITHUB_TOKEN`; no personal access token is required. Re-running a successful
tag workflow replaces the matching release assets.
