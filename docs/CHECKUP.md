# 连接自检与恢复验证

两端在设置和日志页提供“连接自检”。先保存网络设置，再点击“开始自检”；Client 使用主页面当前输入的邀请码，留空时读取已保存的配对端点。自检不会消费邀请码、保存配对、生成身份、向车辆发请求或请求系统授权。

## 如何理解结果

| 项目 | 检查范围 | 后续处理 |
|---|---|---|
| 网卡 | 当前可用接口、已保存名称/IPv4、自动选择是否唯一 | 缺失时重新连接或重新选择并保存；多个 link-local 地址时明确选择 |
| Client 的 Hub | 最多四个候选的 DNS 和 TCP 连接后关闭 | 检查主机名、远端监听、防火墙与端口转发 |
| Operator 端口 | 空闲时短暂绑定并释放 Hub、诊断、控制和发现端口 | 占用时停止冲突程序或更换端口；运行中全部显示跳过 |
| Operator 公开地址 | 地址格式与 DNS | 外网可达性需要另一网络上的对端验证，不使用本机公网回连作判断 |
| MGU 权限 | 当前进程权限和随包助手能力 | Mac 仅在明确启用直连时授权；Windows 直连需要管理员权限 |
| MGU 路由 | 已知目标的现存地址、路由与自动本地候选冲突 | 未知目标跳过，不发诊断查询；直连启用/配置期间跳过自身路由 |
| 对端会话 | 当前运行实例的连接状态 | 不代表车辆诊断或业务操作成功 |

“通过”仅是当次观测。“跳过”不表示条件满足。TCP 可达也不表示 TLS 身份、配对、WAN、车辆或 MGU 操作已验证。端口检查不预留端口，不停止其它程序；自检不自动修改任何配置。

运行总预算 10 秒，DNS/TCP 单项最多 2 秒，端口单项最多 1 秒，路由查询最多 3 秒，均受总预算约束。取消只终止自检，不结束健康会话。全局停止、关闭应用、保存设置、启动运行实例或切换会话会取消或撤销旧结果；取消等待超时不放弃后台汇合。

空闲探测与启动/保存共用控制器配置门，启动前等待本次探测 socket 全部关闭。运行中的检查不探测端口，也不持有该配置门等待网络请求。同步系统网卡/文件系统查询无法硬取消，代码在调用前后检查 context，不通过遗弃 goroutine 冒充硬超时。

## 复制结果

“复制自检结果”显式复制独立 `checkup/v1` JSON：版本、检查时间、完成/取消/超时标志，以及每项的固定 ID、候选序号、状态、错误码和毫秒耗时。未知字符串归一为 `unknown`，最多 32 项，不含配置、端点、地址、网卡名、VIN、指纹、邀请码、密钥、路径或原始错误。已有 Copy diagnostics 的 schema 和白名单不变。

主要错误码：`adapter_ambiguous`、`adapter_unavailable`、`port_in_use`、`address_unavailable`、`permission_denied`、`dns_failed`、`connection_refused`、`probe_timeout`、`target_conflict`、`local_conflict`。界面将其映射为固定建议，不直接显示任意 I/O 异常。

## 恢复压力验证

普通 CI 的真实 loopback TLS/会话测试覆盖重连、旧 TCP 结束、禁止重放、端口回收、流隔离与取消汇合。可选 soak 入口默认 Skip，显式运行后循环取消、断开传输、断开内存设备，每轮重建加密通路并校验 1 MiB 或 8 MiB 双向字节和 SHA256。

快速检查：

```sh
RENET_MANUAL_RECOVERY_SOAK=1 RENET_RECOVERY_SOAK_DURATION=1m CGO_ENABLED=0 \
  go test -v ./internal/integration -run '^TestManualRecoverySoak$' -count=1 -timeout 90s
```

持续验证可把 duration 改为 `8h`，同时将 Go 测试 timeout 改为 `8h1m`。允许范围为 1 分钟至 8 小时；达到目标时长后完成当前轮次再退出，单轮预算 15 秒。只有全部轮次及各自清理成功后才输出 `RECOVERY_SOAK_RESULT`，记录实际时长、轮数、故障类型、有效路径、字节/哈希和汇合结果。失败或中断不输出成功汇总。

这项 soak 使用 loopback 中继 TLS 和内存包设备，不创建原生 TUN 或使用内核 netem，也不模拟 OS 休眠、网卡驱动或真实 WAN。内核弱网继续使用 [LAB](LAB.md)，实车/WAN/休眠验收继续使用 [VALIDATION](VALIDATION.md)；不同证据不能互相替代。
