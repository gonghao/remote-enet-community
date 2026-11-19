# settings

GUI 配置独立于身份、信任和邀请码。公开类型与函数签名遵循 `docs/WAVE5.md` 的 C 工作包及 `docs/WAVE6.md` 的 B 工作包，不引入第三方依赖。

- `Default()`：Client 角色、允许连接测试；Operator 监听 `0.0.0.0:47000`，呈现端口为 `6801/6811/6811`，仅 Windows 默认启用独占；Endpoint、Bind 和 Client 选择项为空。
- `Config.Validate()`：同时检查两种角色中已填写的内容。Endpoint、Bind 可暂留空，由控制器在启动前检查必填；非当前 Operator 角色的 Listen 也可留空。NICAddr、Bind 仅接受字面单播 IPv4；Peer 复用 identity 的完整非零指纹格式。端点检查不查询 DNS，允许合法 DNS 主机名及 IPv4/IPv6，只有监听地址允许通配主机。
- `Load(path)`：仅文件不存在时返回默认值，不创建文件。现有文件须为不超过 64 KiB 的普通 UTF-8 JSON 文件，所有字段必须出现且名称大小写精确匹配 snake_case；拒绝未知、缺失、重复、null、类型错误、尾随内容和无效配置。错误时返回零值配置，不降级到默认值。拒绝符号链接，包括悬空链接。
- `Save(path, cfg)`：先校验与编码，再创建目录和同目录临时文件，完成写入、Sync、Close 后 Rename 替换；替换前失败保留原文件并清理临时文件。新目录权限为 `0700`，配置文件为 `0600`（Windows 权限按系统语义）。不修改现有目录权限。
- `DefaultPaths()`：返回 `os.UserConfigDir()/remote-enet/settings.json` 和同目录下的 `state`，要求系统配置根目录为绝对路径，不依赖当前工作目录，也不创建目录。控制器在 `state/client`、`state/operator` 中分开管理身份与信任。
- `DefaultPathsForRole(role)`：仅接受 `client`、`operator`，配置路径为 `os.UserConfigDir()/remote-enet/{role}/settings.json`，stateDir 仍是共享的 `remote-enet/state` 根目录。只计算路径，不迁移、删除或创建文件；原 `DefaultPaths()` 行为不变。

测试覆盖默认值、两种角色的未完成配置、端点/IP/指纹/端口校验、固定 JSON 向量、严格解码、体积边界、保存往返与替换、失败保留原文件、符号链接和平台路径。测试不访问网络。

版本 1 只包含直连设置。读取时自动在内存中升级为版本 2，补入 `operator.mode = "direct"`、空 Hub 和构建期缺省中心。原文件在调用 `Save` 时才更新，保存统一写版本 2。

版本 2 必须包含 `operator.mode`、`operator.hub` 和顶层 `center`。直连模式要求 Hub 的 `addr`、`fp` 都为空；Hub 模式要求填写 Hub 的主机与端口和完整非零身份指纹。中心由 `id` 和 HTTPS 基址数组 `api` 组成，两者均为空表示未配置。版本 2 中显式保存的空中心优先于构建期缺省值。

构建时可通过 Go 链接器注入缺省中心，例如 `go build -ldflags '-X remote-enet/internal/settings.defaultCenterJSON={"id":"official","api":["https://center.example"]}' ./cmd/renetd`。缺省值用于新配置和版本 1 的升级。
