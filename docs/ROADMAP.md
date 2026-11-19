# 路线图

更新日期：2026-11-19。本文按依赖顺序列出社区仓库接下来的工作，不承诺具体日期。每一项完成后在对应的 Wave 文档或验收报告中记录证据，再更新本文状态。设计约束见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md)、[CENTER-PROTOCOL](CENTER-PROTOCOL.md) 与 [DESIGN](DESIGN.md)。

## 范围

- 本仓库负责：Client、Operator、Hub、加密与会话协议、Hub 的票据验签，以及各端与中心交互的协议实现。
- 不在本仓库：官方中心服务端的实现与运营。
- 永不包含：车辆编码、刷写等业务逻辑。本项目只做发现、呈现与透明转发；涉及编码或刷写的验收，只验证真实诊断工具经由隧道时的转发行为。

## 已完成的基线

| 项目 | 证据 |
|---|---|
| 直连 + 原始邀请码 + 已保存配对，Windows / macOS 原生 GUI | [Wave 5](WAVE5.md)–[Wave 16](WAVE16.md) |
| 双端连接自检、恢复压力测试 | [Wave 15](WAVE15.md)、[CHECKUP](CHECKUP.md) |
| 八小时双通道长稳（loopback / 内存设备） | [Wave 17](WAVE17.md#2026-11-19完整长稳结果) |
| 公网入口、TLS 钉扎、配对复连、peer-only echo | [Wave 17](WAVE17.md) |
| F20 实车只读：E-Sys 建连及 FA / SVT / ECU 读取，ISTA 识别及 ECU 树 | [F20 只读验收](research/f20-readonly-2026-11-18.md) |
| AGPL-3.0-only 与第三方许可随包携带 | [OPEN-SOURCE-READINESS](OPEN-SOURCE-READINESS.md) |

## R1 · 公开仓库与发行基础

| 项目 | 说明 |
|---|---|
| 公开仓库守卫 | CI 或 pre-push 扫描私有标记，如内网地址段、VIN 形态、`renet1:` 邀请码正文、私钥块。私有域名等具体规则不能写进本仓库，应通过 CI secret 或本地私有规则文件注入 |
| 贡献规则 | `CONTRIBUTING.md`、贡献授权方式、提交前检查清单 |
| Windows 安装包 | 核对 Wintun 预编译 DLL 与 AGPL 的组合发行条件；结论落地前不公开发布 Windows 包 |
| 版本与发布 | 版本号与 tag 规则、对应版本源码的获取入口、CI 构建制品 |
| 签名与更新 | macOS Developer ID 与公证、Windows Authenticode、签名更新清单；只属于官方发行层，功能代码不分叉 |

## R2 · 直连数字配对（下一项开发）

目标：Client 输入「地址:端口 + 8 位数字码」即可完成首次配对，原始邀请码保留。

1. PAKE 选型（候选 CPace / SPAKE2）：纯 Go、无 cgo，评估维护状态与审计情况，在引入依赖的 commit 中写明理由。
2. 配对握手：密钥确认覆盖双方设备公钥指纹；配对成功后沿用现有信任存储，钉住对端指纹。
3. Operator 侧：数字码只保存在内存中，设置有效期、单码失败次数和总尝试预算，成功或超出预算后立即作废；可以撤销。
4. Client 侧：同一个输入框识别原始邀请码或「地址 + 数字码」，并给出明确的错误原因。
5. 界面：遵循 [Pixel Workbench](UI-DESIGN.md)，提供三语言文案；Operator 支持生成、复制、撤销数字码。
6. 测试：猜错与次数用尽、码过期、并发兑换、中间人中转、配对后重连无需码。只使用 loopback 和内存传输。
7. 验收：先用隔离假车，再在家庭以外网络做一次公网验证。

## R3 · 自建 Hub 与中心地址配置

1. 独立 Hub 命令：基于现有 `internal/hub` 提供可单独部署的入口，两端都通过出站连接接入。
2. 票据验签：Hub 配置验签公钥，强制校验票据；缺少配置时拒绝，不回退到无鉴权模式。
3. 自建场景的授权方式：提供不依赖中心的签发工具，例如由自建者持有私钥、离线签发长期或短期票据。
4. 中心地址配置：Client 和 Operator 支持配置中心地址，不同中心的信任根相互隔离，界面显示当前中心。

## R4 · 中心协议契约定稿

把 [CENTER-PROTOCOL](CENTER-PROTOCOL.md) 第 12 节的待定参数落成正式契约，包括 API 与版本协商、票据编码、续租与撤销、写请求幂等、入口回退。同时发布票据与 PAKE 的测试向量，供第三方中心验证兼容性。

## R5 · 各端接入中心

1. Operator：登录、选择节点、生成纯数字码、确认来访设备、结束接待。
2. Client：输入纯数字码兑换授权、连接指定 Hub、断线重连；分别显示网卡缺失、Operator 忙、授权到期、码无效、节点离线等原因。
3. Hub：上报心跳、负载与用量，执行租约与撤销。
4. 离线测试覆盖 [CENTER-PROTOCOL 第 11 节](CENTER-PROTOCOL.md#11-一致性测试要求)列出的场景。

## 并行验收线

这些项目依赖真实设备或真实网络，与上面的开发顺序并行推进：

| 项目 | 现状 |
|---|---|
| macOS / Windows 原生休眠唤醒、切换网络、USB 网卡断电 | 进程暂停与重连已在 Linux 模拟；真实系统休眠尚未验收 |
| 实车经公网的对照验收 | 按 [VALIDATION](VALIDATION.md) B 节，假车公网验收通过后再进行 |
| 更多车型与工具版本 | 目前只验证了 F20 的 E-Sys 与 ISTA 只读项目 |
| Tool32 / EDIABAS 的 LAN `Autodetect` | 初始化报 `NET-0012`，显式 IP 可读 VIN；需继续对照发现路径或隔离的 link-local 环境（见 [Wave 4](WAVE4.md)） |
| macOS 上的 Operator 功能 | HSFZ 抓取、全局快捷键 Cmd+Shift+M（可能需要“辅助功能”权限）、三语言切换、像素风界面只做过交叉构建，未在 Mac 实机验证 |
| MGU 直连与 L3 呈现的实车验证 | 隔离 fixture 已验证，实车尚未验证 |
| 编码与刷写经隧道的透明转发 | 未验收；只验证转发，由用户使用真实工具操作 |
| 实车或公网下的长时运行 | 现有八小时结果只限 loopback / 内存设备 |

## 小修与加固（来自代码审查遗留）

| 项目 | 说明 |
|---|---|
| Client 会话结束原因 | `internal/app` 的 `clientConnection` 在网卡变化断开时，两个 `select` 分支可能同时就绪；`session.Run` 结束时用的是 `ctx.Err()` 而非 `context.Cause`，结束原因有时记成 `context canceled`，影响 Log 与诊断导出的排障。重试与 LastError 不受影响 |
| 车端发现的部分发送失败 | `carside.Discover` 向多个目的地发送时，只要有一个成功，其他目的地的失败既不返回也不记日志。应补 warn 日志；2026-11-18 实车排查广播走错网卡时正需要这类信息 |
| `renet-tuncheck` 报告写入 | `writeReport` 直接 `os.WriteFile`，覆盖已有文件时保留原权限，README 的 0600 说法不准确；改为与 `renet-labprobe` 相同的原子写 |
| settings 可选加固（Wave 5 审查，当时有意未做） | 路径以分隔符结尾时留下空目录；rename 后未 fsync 父目录；监听地址接受 `[]:47000`；`NICName` 未拒绝 U+202E 等 Cf 类控制字符；缺字段报错不指明字段 |

## 研究项（未排期）

- 用 F20 实车 HSFZ 抓包核对 [协议调研](enet-protocol-research.md) 中标注“仍需实测 F20”的项目：原始发现响应、alive 使用 `0x0012` 还是 `0x0013`、抓到的 `0x0043` 响应含义等。原始抓包含 VIN，只在私有环境分析，结论脱敏后写回调研文档。

- HTTP 回连：需要先取得 URL 通告方式与本地源地址选择的证据，再设计反向通道（见 [DESIGN 9.4](DESIGN.md#94-mgu-http-刷写与-ssh为什么要-l3)）。
- DoIP 及其他接口的兼容性。
