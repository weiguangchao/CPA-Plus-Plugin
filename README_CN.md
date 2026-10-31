# CPA Plus 调度插件

插件仅改变账号选择，通过 CPA v8 管理 API 转发额度查询，使用内存缓存。CPA 负责向 Provider 上游发起请求，插件解析返回的额度和重置时间。无需安装额度查询插件。

版本统一使用 `0.1.x`，当前为 `0.1.6`。根目录的 [VERSION](VERSION) 是插件注册版本和打包文件名的共同来源。Make 在构建时注入版本号，动态库运行时不读取此文件。直接用 Go 构建且未传版本链接参数时，版本显示为 `dev`。

Go 源码及 C ABI 头文件在 `src/`，Go 测试在 `test/`，Python 脚本在 `scripts/`。构建和测试命令从仓库根目录运行，版本文件、Go 模块文件、配置示例和文档保留在根目录。Go 代码不输出应用日志，API 查询失败时仍按下述规则回退给 CPA。

使用 `make test` 和 `make check` 运行 Go 检查。测试运行器通过临时 Go overlay 将 `test/` 文件映射到源码包，使现有测试仍能访问未导出函数，不向 `src/` 写入临时文件。直接运行 `go test ./...` 不会应用该映射。只运行某项测试时，可使用 `python3 scripts/go-check.py test -race ./... -run TestSchedulingOrder`。

## 调度规则

CPA 通过 `Options.Metadata.pinned_auth_id` 明确固定账号的请求，插件直接返回 `Handled: false`，交回 CPA，不调用管理 API，也不读写普通调度缓存。这些请求不会覆盖其他连接已选出的账号。只有一个候选但没有有效固定账号标记的请求，仍参与正常额度调度。

普通调度请求按 Provider、实际模型和允许的 Provider 集合缓存已选账号。每次请求先检查缓存：

- 已选账号仍在 CPA 本次候选中，且缓存未过期、未到已知重置时间时，直接返回同一账号，不调用 API。
- 没有缓存、已选账号退出候选、缓存过期或到达已知重置时间时，触发一轮后台查询。当前请求等待查询结果，没有固定等待上限。选出合适账号就用于当前请求；无法选出账号或插件停止时，返回 `Handled: false`，由 CPA 默认调度处理。

后台查询完成后，按以下规则从触发查询时的 CPA 候选中选定账号：

1. 五小时和七天额度均确认有剩余，数据未过期，才进入插件选择。
2. 优先选择五小时重置时间在未来且最早的账号。
3. 如果合格账号都没有有效的五小时重置时间，则选择七天重置时间在未来且最早的账号。
4. 无法选出账号时返回 `Handled: false`，交回 CPA 当前配置的默认策略。

选定后会持续复用，不在每次请求间轮换。相同重置时间的账号只在重新选择时参与轮换。新增或重新出现的候选不会抢占仍有效的已选账号。额度可用性与重置时间分开判断，两个窗口都有剩余但没有重置时间时也会交回 CPA。重置时间已过的观测需要重新查询，不能直接推断额度恢复。查询完成后，每个等待中的请求还会检查结果是否属于自己的候选；不匹配时，按自己的候选重新查询并继续等待。

双额度过滤只约束插件主动选择。回退后由 CPA 判断可用性，可能选择额度未知或被插件排除的账号，这是本插件的预期行为。

CPA 继续负责模型匹配、认证、请求执行、失败切换、重试和冷却。插件不会改写账号优先级、禁用状态或冷却状态，也不会调用额度重置接口。

## 前提

- CPA SDK 固定为 v8.0.47，实际部署需验证目标版本的 ABI 兼容性。
- 使用支持 usage 查询的 Claude 或 Codex OAuth 账号。普通 API Key 不保证支持这些 OAuth 额度接口，查询失败时交回 CPA。
- CPA 管理 API 已启用，插件能访问该 CPA 实例并使用其管理密钥。
- 使用普通本地运行模式。Home 模式绕过本地调度插件。

CPA 同时只使用优先级最高的有效调度插件，请检查是否已有其他调度插件。`plugins.configs.cpa-plus.priority` 是插件执行优先级，与账号本身的 `priority` 不同。

## 配置表单

表单只提供五项插件配置，依次为 `cpa_url`、`management_key`、`request_timeout`、`retry_interval`、`cache_ttl`。配置键保持原名，说明文字统一使用中文。所有现有额度适配器默认启用，不再显示 `providers`。旧配置中的 `providers` 不再限制账号选择。

## 构建与安装

需要 Go 1.26.10 或更新版本、CGO 和目标平台的 C 编译器。

```sh
make check
make build
```

产物在 `build/`：macOS 为 `cpa-plus.dylib`，Linux 为 `cpa-plus.so`，Windows 为 `cpa-plus.dll`。必须匹配 CPA 进程的系统和 CPU 架构。跨平台构建还需要相应的 C 交叉编译工具链；Intel macOS 上的 Go 动态库运行时兼容性需要单独验证。

将动态库复制到 CPA 配置的插件目录，保持文件名 `cpa-plus`。把 [config.example.yaml](config.example.yaml) 合并到现有 CPA 配置。原有额度插件可移除，本插件不会调用它们。

插件注册元数据中的作者为 `weiguangchao`，源码地址为 https://github.com/weiguangchao/CPA-Plus-Plugin 。

在 CPA Plus 配置表单的 `management_key` 中直接填写 CPA 原有管理密钥，或在插件 YAML 的同名字段填写。保存后密钥由 CPA 存储在插件配置中。未填写或只填空白时，插件仍会展示配置表单，但不发起账号和额度查询，调度全部交回 CPA。升级时请将旧的 `management_key_env` 替换为 `management_key`，填写实际密钥，不再读取环境变量。

配置中的 `cpa_url` 是 CPA 服务地址，不包含 `/v8/management`。支持部署子路径，例如 `https://example.com/cpa`。在同一容器或主机内运行时通常可使用 `http://127.0.0.1:8317`。插件不使用环境代理连接管理 API，也不跟随 HTTP 重定向。

没有 Make 时可直接运行：

```sh
python3 scripts/go-check.py test -race ./...
python3 scripts/go-check.py vet ./...
go build -trimpath -buildmode=c-shared -ldflags "-X main.version=$(cat VERSION)" -o build/cpa-plus.dylib ./src
```

Linux、Windows 请替换产物扩展名，并保证父目录存在。

## Linux amd64 打包

在 Linux amd64 主机安装 Go、C 编译器、Make 和 Python 3 后运行：

```sh
make check
make package-linux-amd64
python3 scripts/smoke.py build/linux-amd64/cpa-plus.so
```

原始动态库为 `build/linux-amd64/cpa-plus.so`。打包生成：

- `dist/cpa-plus_0.1.6_linux_amd64.tar.gz`
- `dist/cpa-plus_0.1.6_linux_amd64.tar.gz.sha256`

压缩包包含 `cpa-plus.so`、`VERSION`、配置示例、中英文说明和 `LICENSE`。在 `dist` 目录使用 `sha256sum -c cpa-plus_0.1.6_linux_amd64.tar.gz.sha256` 校验，解压后将 `cpa-plus.so` 复制到 CPA 插件目录。

在 macOS 或其他架构交叉编译时，需要把 Zig 加入 `PATH`，然后运行相同的 `make package-linux-amd64`。默认使用 `zig cc -target x86_64-linux-gnu.2.17`，CPU 基线为 `GOAMD64=v1`。也可以指定其他 Linux amd64 C 工具链：

```sh
make package-linux-amd64 LINUX_AMD64_CC=x86_64-linux-gnu-gcc
```

产物面向 glibc Linux，例如 Debian 和 Ubuntu。Linux 原生构建的最低 glibc 版本取决于所用 C 工具链；Alpine 的 musl 环境需要单独构建。动态库加载测试必须在匹配的 Linux amd64 环境运行，macOS 无法直接加载 Linux `.so`。

## 额度 API 与缓存

插件只访问 CPA 管理接口：

- `GET /v8/management/credentials`，读取账号 ID、`auth_index`、Provider、Codex 账号 ID 和 SIWC 标记。
- `POST /v8/management/requests/api-call`，让 CPA 代账号发起 usage 请求。
- `GET /v8/management/credentials/download?name=...`，仅在查询启用 SIWC 的 Codex 账号时读取嵌套凭据。

默认启用全部现有适配器：`claude`、`anthropic` 和 `codex`，其中 `anthropic` 使用 Claude 适配器。无需配置 Provider 列表，旧的 `providers` 已不再使用。其他 Provider 的额度格式尚未适配，交回 CPA。每轮先查询一次完整账号列表，再逐个查询与本次候选的账号 ID、Provider 都匹配、未禁用且有 `auth_index` 的账号。普通账号每轮只发一次 usage 查询，同时获取额度和重置时间，不查询套餐或主动重置次数。

转发请求包含 `auth_index`、`method`、`url` 和 `header`。普通账号使用 `Authorization: Bearer $TOKEN$`，由 CPA 填入当前凭据，并应用账号或全局代理配置。普通 Codex 账号存在 `id_token.chatgpt_account_id` 时附加账号头。转发接口的外层 HTTP 200 只表示转发成功，插件还会检查响应中的上游 `status_code`；401、429 等失败不会作为有效额度。

SIWC 账号每轮多一次凭据下载。插件只使用嵌套 `siwc.access_token`，不携带外层账号头，并检查 `expires_at`、`expiresAt` 或 token 中的 JWT `exp`。缺少 token、无法确认有效期、已过期或凭据无法下载时跳过该账号，不改用外层 token。仅存在于运行时且无法下载的 SIWC 账号也会跳过。下载内容和 token 只用于本轮查询，不写入文件、不加入调度缓存。插件不主动刷新 token，依赖 CPA 正常维护凭据。

| 时机 | 是否调用 API |
| --- | --- |
| 插件加载、重新配置或重载，但尚无请求 | 否，只创建空缓存 |
| 请求包含有效的 `pinned_auth_id` 固定账号标记 | 否，直接交回 CPA，不读写普通缓存 |
| 某个 Provider 和模型首次收到可管理的候选 | 是，后台查询账号列表及对应候选的额度 |
| 已选账号仍在候选中且缓存有效 | 否，直接复用 |
| 已选账号退出 CPA 本次候选 | 是，按当前候选重新查询和选择 |
| 固定缓存时长到期后的首次请求，仅 `cache_ttl` 大于 0 时 | 是，重新查询和选择 |
| 到达已知重置时间后的首次请求 | 是，重新查询和选择 |
| 查询失败或未选出账号后的请求 | 默认等待至少 20 秒，再由新请求触发重试 |
| 没有请求、没有候选或候选均无可用额度适配器 | 否 |

没有定时轮询。`cache_ttl` 默认值为 `10m`，即 10 分钟。填写 `0` 表示不按固定时长过期；YAML 可直接填写 `cache_ttl: 0`，也支持字符串 `"0"` 或 `"0s"`。其他固定有效期必须至少为 1 秒，不接受负数或不足 1 秒的非零值。固定有效期从查询轮次开始计算，不会因缓存命中延长。无论 `cache_ttl` 是否为 0，查询到的任一当前模型相关窗口的未来重置时间都会限制这轮缓存，包括暂时耗尽的其他候选账号，因为它重置后可能更合适。到期只让缓存失效，不会在空闲时主动调用 API。已选账号退出候选时仍会重新查询和选择，插件重载或进程重启后缓存仍会清空。

查询失败或没有账号满足双额度及重置时间要求时，交回 CPA，并通过 `retry_interval` 限制同一路由的重试频率，默认 20 秒。若已知下一次重置时间更早，则允许该时间到达后由新请求重试。等待期间没有自动重试。每个 API 请求最多等待 10 秒，通过 `request_timeout` 修改。旧配置中的 `refresh_interval` 已不再使用，请删除该项。

同一 Provider 和模型的并发刷新合并为一轮，等待中的请求不设固定超时。等待时释放缓存锁，不阻塞有效缓存的命中。所有路由共用一个后台查询线程，一次只发一个 API 请求。大账号池或多个模型同时失效时，排队和查询会增加当前请求的等待时间；超过本轮缓存有效期的结果会丢弃，不再延长缓存。

缓存随进程或插件重载消失；重载后首次请求重新触发查询。CPA 负责过滤冷却、禁用及模型不匹配的账号。只要已选账号退出本次候选，插件便不再返回它。若凭证在保持相同账号 ID 的情况下被替换，则在下一轮按需查询时识别，插件不再定时扫描凭证文件。插件停止或重载会取消后台请求，不写入额度文件。

每轮刷新都会通过 CPA 发起新的 usage 请求。并发请求及官方客户端也可能同时消耗额度。CPA 候选过滤只能反映 CPA 已知的失败及冷却状态，不能保证缓存期内双额度始终有剩余，最终以 Provider 的真实响应为准。

## 字段适配

Claude 查询 `GET https://api.anthropic.com/api/oauth/usage`，读取 `five_hour` 和 `seven_day`。`utilization` 是已用百分比，插件计算剩余比例；`resets_at` 是重置时间。当前模型对应的 `seven_day_sonnet`、`seven_day_opus` 等专属额度也必须可用。Fable 兼容 `limits[]` 中的 `weekly_scoped`，优先使用 active 项；旧版 `iguana_necktie` 仅在它是百分比窗口时使用，美元余额不代替周额度。

Codex 查询 `GET https://chatgpt.com/backend-api/wham/usage`，读取 `rate_limit` 中的 `primary_window` 和 `secondary_window`。根据 `limit_window_seconds` 的 18000 或 604800 识别五小时、七天，不固定主次窗口的顺序。`used_percent` 用于计算剩余比例；优先读取 `reset_at`，没有有效绝对时间时使用查询时间加 `reset_after_seconds`。`allowed: false` 或 `limit_reached: true` 会阻止插件选择该账号。

Codex 的 `additional_rate_limits` 按 `limit_name` 或 `metered_feature` 匹配当前模型，覆盖 Spark 等专属额度。其他模型的专属额度不阻塞当前模型，Code review 额度不影响普通推理调度。

已用百分比必须存在且在 0 到 100 之间。缺失或异常值视为未知，100 表示用尽。账号必须有可用的公共五小时和七天窗口，模型专属窗口不能替代公共窗口。重置时间支持 RFC 3339、Unix 秒和 Unix 毫秒。月额度或其他时长不会代替这两个窗口。未提供有效重置时间时，仍按调度规则尝试七天排序或回退 CPA。

模型名称直接使用 CPA 调度请求中的 `Model`，按忽略大小写和分隔符后的名称匹配。插件不提供模型别名映射。旧的 `model_aliases` 和 `window_aliases` 不再使用，请删除这些项。

## 查询超时与重试

`request_timeout` 默认 `10s`，只限制每一次后台 CPA 管理 API 请求，包括账号列表、额度转发和 SIWC 凭据下载。API 请求超时视为该次查询失败。多个账号逐个查询时，整轮总耗时可以超过 10 秒。

无有效缓存时，调度请求会等待排队、查询以及候选不匹配后的重新选择，没有固定等待上限。查询完成且没有选出账号时交回 CPA。插件停止或重载会取消查询并唤醒等待中的调度请求。`request_timeout` 不限制用户推理执行时间。

`retry_interval` 默认 `20s`。一轮查询完成后，如果没有选出账号或查询失败，同一路由至少等到这个间隔结束，才允许后续请求触发新一轮查询。没有新请求就不重试。已知的相关重置时间如果更早，可以提前结束等待。它不控制 CPA 的推理失败重试和账号冷却。

## 账号优先级与会话

跨账号优先级调度固定启用。CPA 向插件提供所有可用优先级的候选，插件可以选中优先级较低但重置时间更近的账号。CPA 仍负责模型匹配、禁用和冷却过滤。插件自身的 `priority` 仍由 CPA 管理，已有有效缓存时仍按缓存规则复用账号。

例如账号 A 的优先级是 10、五小时重置还剩 2 小时，账号 B 的优先级是 0、重置还剩 10 分钟，且两者双额度均可用。重新选择时，插件会优先选择 B。

旧的 `schedule_across_priorities` 配置项已移除，请删除该项。旧配置即使保留 `false`，也不会关闭跨优先级调度。

普通调度请求按 Provider 和模型保持账号选择，同一模型的不同会话会复用同一缓存。直接选择时不自动执行 CPA 内置会话粘性。回退默认策略时，CPA 原有选择器及其会话规则恢复执行。带有 `pinned_auth_id` 的请求始终交回 CPA，不影响普通缓存；插件不会解除固定账号或将已有连接迁移到其他账号。

## 验证

```sh
make check
make smoke
```

`make smoke` 通过 C ABI 加载实际动态库，使用本地模拟 CPA 转发 API 和上游 usage 响应验证按需查询、账号复用、候选变更、双窗口过滤、五小时与七天选择、默认回退和重新配置，不需要真实账号。

已有 CPA 二进制时，可以额外验证真实宿主加载：

```sh
python3 scripts/smoke.py build/cpa-plus.dylib --cpa /path/to/cli-proxy-api
```

该检查使用临时配置和测试密钥启动独立 CPA 进程，完成后退出，不发送真实推理或额度请求。

## 通过 tag 发版

GitHub Actions 在推送 `main` 和提交 Pull Request 时执行检查并构建 Linux amd64。推送 `v0.1.x` tag 后，检查通过才创建 GitHub Release。tag 必须等于根目录 `VERSION` 的内容加上 `v` 前缀。

例如 `VERSION` 为 `0.1.6` 时执行：

```sh
git tag v0.1.6
git push origin v0.1.6
```

下次发版先更新 `VERSION`，提交并推送代码，再创建对应 tag。tag 与版本不一致时检查失败，不发布；不符合 `v0.1.*` 的 tag 不触发该工作流。

工作流执行竞态测试、vet 和格式检查，使用 Zig 0.15.2 构建面向 glibc 2.17 的 Linux amd64 动态库，并在 Linux 上加载动态库验证 ABI 和调度。Release 附带 `.tar.gz` 安装包、`.sha256` 校验文件和自动生成的发布说明。使用 GitHub 内置的 `GITHUB_TOKEN`，无需配置个人访问令牌。重新运行同一 tag 的成功工作流时，会替换对应 Release 附件。
