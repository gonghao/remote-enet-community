# 脱敏诊断构建器

`Build(Input) ([]byte, error)` 只构建独立的 JSON 诊断格式，`schemaVersion` 固定为 `1`，与 settings 格式无关。调用者仅在用户点击 Copy diagnostics 后调用，并自行负责复制、显示失败和权限观测；本包不访问磁盘、环境、网络、剪贴板或真实系统权限，不记录日志。

## 公开 API

- `Permission`：`Platform string` 和 `Elevated`、`HelperAvailable`、`AuthorizationRequired bool`，必须由真实权限观测结果提供。`Platform` 必须严格等于当前编译目标的 `runtime.GOOS`；空值、未知值或其他平台均返回固定错误，避免把别的平台权限描述为当前权限。
- `Input`：`Version, Role string`、`Runtime app.Snapshot`、`Running, Stopping, Testing, DirectWorking bool`、`Permission Permission`。调用期间调用者不得修改输入；构建器不修改输入，不遍历或持有快照的可变集合。
- `Build`：成功返回调用者独占的 JSON 字节切片，最多 64 KiB。任何失败均返回 `nil` 字节切片与固定错误，不回显输入，不返回部分报告。相同输入可并发构建。

版本最多 64 个 ASCII 字节，只接受 `dev`、可选 `v` 前缀的 `major.minor.patch`，以及可选的 `-alpha.N`、`-beta.N`、`-rc.N` 或固定预览后缀。预览后缀严格为 `-preview\.(wave[0-9]{1,3}(\.[0-9]{1,3})?|[0-9a-f]{7,40})(-worktree)?`（点均为字面点），例如 `0.1.0-preview.wave9.2-worktree`、`0.1.0-preview.wave10.0`、`0.1.0-preview.abcdef0-worktree`。主版本三段及 alpha/beta/rc 序号各为 1–9 位，除单独的 `0` 外不得有前导零；wave 数字段遵循上述 1–3 位规则。例如 `v1.2.3-rc.1` 合法；空版本、任意标签、固定预览格式之外的 revision、构建元数据、路径及换行不合法。整合者应传真实产品版本；预览格式允许的 7–40 位小写 commit hex 与 `-worktree` 是明确授权的版本标记，不能拼入额外 dirty、配置或自由文本。任意其他版本标签需要先扩展合同，不能绕过验证。

## schema v1 字段白名单

所有字段始终输出，包括零值。所有时长单位均为纳秒。

| 对象 | 字段 |
| --- | --- |
| 顶层 | `schemaVersion`, `version`, `system`, `role`, `runtime`, `lifecycle`, `permission` |
| `system` | `goos`, `goarch`，仅来自 `runtime.GOOS/GOARCH` 编译常量 |
| `permission` | `platform`, `elevated`, `helperAvailable`, `authorizationRequired` |
| `lifecycle` | `running`, `stopping`, `testing`, `directWorking` |
| `runtime` | `role`, `phase`, `generation`, `path`, `hasError`, `rttNanoseconds`, `rttValid`, `metrics`, `exclusive`, `direct` |
| `runtime.direct` | `phase`, `hasError` |
| `runtime.exclusive` | `enabled`, `active`；不输出进程标识 |
| `runtime.metrics` | `incomplete`, `activeConnections`, `hasError`, `totalIn`, `totalOut`, `sessionGeneration`, `sessionActive`, `sessionIn`, `sessionOut`, `inBytesPerSecond`, `outBytesPerSecond`, `rateWindowNanoseconds`, `rttSamples`, `rttTotalSamples`, `rttLatestNanoseconds`, `rttP50Nanoseconds`, `rttP95Nanoseconds` |

顶层 `role` 表示调用者角色，`runtime.role` 保留快照角色，二者分别验证，不互相推断。角色仅接受 `client/operator`；运行阶段仅接受 app 的既有阶段；连接路径仅接受 `direct/relay`；Direct 阶段仅接受 `disabled/discovering/starting/ready/stopping/failed`。未知和空枚举一律输出 `unknown`。所有枚举通过后返回白名单常量，不直接传出调用者字符串。

`rttValid=false` 时单次 RTT 输出零；会话 `rttSamples=0` 时 latest/p50/p95 输出零，零值不能被当成实测。`incomplete` 保留部分观测状态，不从列表长度反推连接数或字节数。速率 NaN/Inf 一律失败。错误只投影存在布尔值：运行及 Direct 使用错误文本是否非空，metrics 使用 `LastError != nil`，不解引用事件或检查其内容。

不序列化 `app.Snapshot` 整体，也不输出任何地址、端点、VIN、设备或对端指纹、MAC、网卡或适配器名、文件路径、邀请码、配置、原始错误、Events、Tunnel Target、进程身份或绝对时间。

## 测试与整合边界

`diagnostics_test.go` 递归向快照所有导出的可变字符串注入秘密，并另外覆盖指纹、IP、端点和进程身份。版本及权限平台作为验证入口独立测试，验证失败字节为 nil、错误固定。其余测试覆盖观测布尔值、枚举、非有限速率、部分指标、未测 RTT、集合不变、输出独立与并发构建。字段树测试固定完整 JSON 白名单，新增字段必须显式审查。

```sh
CGO_ENABLED=0 go vet ./internal/diagnostics
CGO_ENABLED=0 go test ./internal/diagnostics
CGO_ENABLED=1 go test -race ./internal/diagnostics
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/diagnostics-windows.test.exe ./internal/diagnostics
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go test -c -o /tmp/diagnostics-darwin.test ./internal/diagnostics
```

跨编译只验证构建能力，不代表已执行原生 Windows/macOS 测试。本包不接线 UI、权限检测或复制操作，不改变其他包和依赖。
