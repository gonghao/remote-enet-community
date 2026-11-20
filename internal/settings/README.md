# settings

GUI 配置独立于身份、信任和邀请码。公开类型与函数签名遵循 `docs/WAVE5.md` 的 C 工作包及 `docs/WAVE6.md` 的 B 工作包，不引入第三方依赖。

- `Default()`：Client 角色、允许连接测试。Operator 的默认监听地址为 `0.0.0.0:47000`，呈现端口为 `6801/6811/6811`，仅 Windows 默认启用独占。Endpoint、Bind 和 Client 选择项为空。`operator.mode` 缺省为 `direct`；构建期注入了有效且非空的缺省中心时为 `center`（见文末）。注入值无效时 `Default()` 退回无中心的 `direct`，错误由 `Load` 报告。
- `Config.Validate()`：同时校验两种角色的偏好。Endpoint、Bind 可暂留空，由控制器在启动前检查必填。只有 Operator 角色使用直连模式时必须填写 Listen。NICAddr、Bind 仅接受字面单播 IPv4，Peer 复用 identity 的完整非零指纹格式。端点检查不查询 DNS，允许合法 DNS 主机名及 IPv4/IPv6，只有监听地址允许通配主机。
- `Load(path)`：仅文件不存在时返回默认值，不创建文件。现有文件须为不超过 64 KiB 的普通 UTF-8 JSON 文件，所有字段必须出现且名称大小写精确匹配 snake_case；拒绝未知、缺失、重复、null、类型错误、尾随内容和无效配置。错误时返回零值配置，不降级到默认值。拒绝符号链接，包括悬空链接。
- `Save(path, cfg)`：先校验与编码，再创建目录和同目录临时文件，完成写入、Sync、Close 后 Rename 替换；替换前失败保留原文件并清理临时文件。新目录权限为 `0700`，配置文件为 `0600`（Windows 权限按系统语义）。不修改现有目录权限。
- `DefaultPaths()`：返回 `os.UserConfigDir()/remote-enet/settings.json` 和同目录下的 `state`，要求系统配置根目录为绝对路径，不依赖当前工作目录，也不创建目录。控制器在 `state/client`、`state/operator` 中分开管理身份与信任。
- `DefaultPathsForRole(role)`：仅接受 `client`、`operator`，配置路径为 `os.UserConfigDir()/remote-enet/{role}/settings.json`，stateDir 仍是共享的 `remote-enet/state` 根目录。只计算路径，不迁移、删除或创建文件；原 `DefaultPaths()` 行为不变。

测试覆盖默认值、两种角色的未完成配置、端点/IP/指纹/端口校验、固定 JSON 向量、严格解码、体积边界、保存往返与替换、失败保留原文件、符号链接和平台路径。测试不访问网络。

版本 1 只包含直连设置。读取时自动在内存中升级为版本 2，补入 `operator.mode = "direct"`、三个字段均为空字符串的 Hub 和构建期缺省中心。原文件在调用 `Save` 时才更新，保存统一写版本 2。

版本 2 必须包含 `operator.mode`、`operator.hub` 和顶层 `center`。直连模式使用 `operator.endpoint` 作为公开地址，可保留 Hub 配置，各非空字段仍需通过校验。Hub 模式要求 `hub.addr` 为实际连接的 host:port、`hub.fp` 为完整非零指纹。邀请码与数字配对使用 `hub.share` 指定的 host:port，空字符串表示使用 `hub.addr`。切换模式会保留另一种模式的地址。中心由 `id` 和 HTTPS 基址数组 `api` 组成，两者均为空表示未配置。版本 2 中显式保存的空中心优先于构建期缺省值。

中心模式（`operator.mode = "center"`）要求填写 `center.id` 和 `center.api`。`operator.listen` 可以留空。直连和 Hub 字段可以保留，非空时仍需通过校验。配置版本仍为 2，旧版程序会拒绝加载含中心模式的配置。

构建时可通过 Go 链接器注入缺省中心，例如 `go build -ldflags '-X remote-enet/internal/settings.defaultCenterJSON={"id":"official","api":["https://center.example"]}' ./cmd/renetd`。配置文件不存在时，注入了有效且非空的缺省中心则使用 `center`，社区构建使用 `direct`。已有版本 2 配置保留文件里的模式，版本 1 升级后仍使用 `direct`。
