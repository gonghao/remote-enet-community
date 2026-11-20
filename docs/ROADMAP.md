# 路线图

更新日期：2026-11-20。本文按依赖顺序列出社区仓库接下来的工作，不承诺具体日期。每一项完成后在对应的 Wave 文档或验收报告中记录证据，再更新本文状态。设计约束见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md)、[CENTER-PROTOCOL](CENTER-PROTOCOL.md) 与 [DESIGN](DESIGN.md)。

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
| 直连数字配对（地址 + 8 位码，CPace -21）与 LAN 台架验收 | [PAIRING](PAIRING.md)、PR #2、PR #4 |

## R1 · 公开仓库与发行基础

| 项目 | 说明 |
|---|---|
| 公开仓库守卫 | 已完成：`tools/repo-guard.py` 扫描私有标记，接入 CI 与 pre-push 钩子；私有规则经 CI secret `REPO_GUARD_RULES_INLINE` 或本地文件注入，不写进本仓库 |
| 贡献规则 | `CONTRIBUTING.md`、贡献授权方式、提交前检查清单 |
| Windows 安装包 | 核对 Wintun 预编译 DLL 与 AGPL 的组合发行条件；结论落地前不公开发布 Windows 包 |
| 版本与发布 | 版本号与 tag 规则、对应版本源码的获取入口、CI 构建制品 |
| 签名与更新 | macOS Developer ID 与公证、Windows Authenticode、签名更新清单；只属于官方发行层，功能代码不分叉 |

## R2 · 直连数字配对（已完成，设计见 [PAIRING](PAIRING.md)）

进度（2026-11-19）：经 PR #2 合并。1–6 已实现，离线测试与独立审查通过。7 已完成 LAN 台架：Windows GUI Operator + Linux CLI Client + 假网关，覆盖首次配对、无码重连、旧码重用、猜错计数与第 5 次成功、撤销、次数用尽、过期和复制格式。Mac GUI Client 与公网验证移入下方「并行验收线」；Mac 经 WG 访问台架时无法发现非同链路网关，属车端有意限制。独立密码学复核列为后续项，不阻塞发布。

对齐（2026-11-19）：经 PR #4 按 [CENTER-CONTRACT §10](CENTER-CONTRACT.md#10-中心模式的首次配对) 改写线格式（LEB128 CI、HMAC-SHA-512 确认、承诺与 SAS、消息 11–16、exporter 标签 `EXPORTER-renet-pake-sid-v1`），与 PR #2 的格式不兼容；应用向量由独立生成器产出。同时修复 Operator 拒绝后立即关流导致 Client 把拒绝原因误报为中断的竞态。下方第 7 项的 LAN 台架是对旧格式做的，打 tag 前需在台架重跑一次首次配对与拒绝场景。

目标：Client 输入「地址:端口 + 8 位数字码」即可完成首次配对，原始邀请码保留。

1. PAKE 选型：已定为 CPace（固定草案 -21）+ `github.com/gtank/ristretto255`，理由见 [PAIRING §1](PAIRING.md#1-密码套件选择)；引入依赖的 commit 中写明理由。
2. 配对握手：密钥确认覆盖双方设备公钥指纹；配对成功后沿用现有信任存储，钉住对端指纹。
3. Operator 侧：数字码只保存在内存中，设置有效期、单码失败次数和总尝试预算，成功或超出预算后立即作废；可以撤销。
4. Client 侧：同一个输入框识别原始邀请码或「地址 + 数字码」，并给出明确的错误原因。
5. 界面：遵循 [Pixel Workbench](UI-DESIGN.md)，提供三语言文案；Operator 支持生成、复制、撤销数字码。
6. 测试：猜错与次数用尽、码过期、并发兑换、中间人中转、配对后重连无需码。只使用 loopback 和内存传输。
7. 验收：先用隔离假车，再在家庭以外网络做一次公网验证。

## R3 · 自建 Hub 与中心地址配置

已完成（离线测试与独立审查通过，人工台架验收待执行）。设计与实现记录见 [SELFHOST](SELFHOST.md)（第 14 节为两轮独立审查的逐条处理），冻结的接口见 [CONTRACTS · R3](CONTRACTS.md#r3-自建-hub-与中心地址配置2026-11-20-冻结)。另修复 Operator 内嵌 Hub 可被当作开放中继的问题（commit 2934906）；中心契约第八版已补入自建模式条文（commit 8e67f0c）。

交付物：

1. 独立 Hub 命令：`cmd/renet-hub`（`init` / `fingerprint` / `check` / `serve` / `ticket inspect`），基于 `internal/hub` 的 `Policy` / `Grant` / `SetPolicy`；两端都出站连接；Unix 上 SIGHUP 重载。
2. 票据验签：`internal/ticket`（`v = 2` 的 COSE 票据，向量由独立生成器产出）与 `internal/hubauth`（配置、签发者作用域、拒绝列表）；没有策略时 Hub 拒绝启动，不回退到无鉴权模式。
3. 自建场景的授权：每台 Operator 持有只能为自己签票的签发密钥，`renetd issuer` 输出 Hub 配置条目，`renetd invite --hub` 离线签发邀请，运行中的 Operator 自动签 wait 票并在会话握手末尾下发、续期设备票（消息 17 `HubTicket`）。
4. 首次配对：版本 2 邀请码，以及经 Hub 配对入口的「地址 + 8 位码」（`pair-hub/1`，Hub 指纹写入 PAKE）。
5. 中心地址配置：settings 版本 2、控制器显式切换、`GET /v1/meta` 核对 `center_id`（`internal/center`）；R5 之前两端隐藏中心配置界面；R5 将 Operator 的连接方式扩展为直连、自建 Hub、中心三选一，并按所选模式呈现首页会话控制。

证据：

| 范围 | 测试 |
|---|---|
| 票据编码与向量 | `internal/ticket`：`TestVectors`、`TestIssuer*` |
| Hub 准入、配对入口与撤销 | `internal/hub`：`TestPairingEntry`、`TestReloadRevokesRoutes`、`TestPairedDialFirstByteDeadline`；`internal/hubauth`：`TestReloadCompatible` |
| 端到端（真实 loopback Hub、Operator、Client） | `internal/integration/selfhost_test.go`：`TestSelfhostInviteExpiryAndRenewal`、`TestSelfhostWindow`、`TestSelfhostReloadRevokesClient`、`TestSelfhostBusyHandoff`、`TestSelfhostRestart` 等 |
| 伪造 Hub | `TestSelfhostHubFingerprintBinding`、`TestSelfhostImpostorHubFailsPairing`，对照 `TestSelfhostTransparentForwarding` |
| 中心 meta | `internal/center`：`TestFetchMetaAnonymous`、`TestFetchMetaRejectsRedirectAndHTTP` |

遗留：人工台架验收（Hub 跑在家庭网络以外的主机，Operator 与 Client 都在 NAT 之后），已列入下方「并行验收线」。

## R4 · 中心协议契约定稿

已完成（独立审查已处理，契约第九版）。把 [CENTER-PROTOCOL](CENTER-PROTOCOL.md) 第 12 节的待定参数落成正式契约 [CENTER-CONTRACT](CENTER-CONTRACT.md)（2026-11-19 定稿，第八版加入自建模式条文，第九版按一致性向量澄清边界，逐条记录见其第 18 节），并发布 V1–V15 一致性向量，供第三方中心验证兼容性。发布格式、各文件字段与 `reason` 枚举见 [CONFORMANCE](CONFORMANCE.md)；中心仓库按版本标签拉取 `testdata/conformance/`。

交付物：

1. 向量：`testdata/conformance/` 下 `v01-account-code.json`、`v02-numeric-code.json`、`v03-request-signature.json`、`v04-ticket.json`（含 V5）、`v06-pake-ci.json`、`v07-pairing-messages.json`、`v08-ticket-reject.json`（V8–V15），头部带 `contract_version`（`v1.9`）。生成器在 `testdata/conformance/gen/`，只用 Python 标准库、OpenSSL 命令行与（V6/V7）libsodium，不依赖 Go 代码。
2. 与向量交叉验证的 Go 端纯编解码：`internal/centercode`（账号码与数字码规范化，区分长度与字符错误）；`internal/center`（`CanonicalTarget`、`SignDevice` / `SignNode`、`VerifyDevice` / `VerifyNode`、`CheckTimestamp`）；`internal/ticket`（中心票据 `v = 1` 的 `SignCenter` 与 §8.4 第 1–9 步的 `VerifyCenter`，拒绝步骤以 `StepError.Step` 表示）；`internal/pairing/cpace`（`pair-center/1` 的 CI 绑定）。
3. CI：check 任务安装 `libsodium23`，对 CPace、自建票据与本目录全部生成器做逐字节复现检查。

证据：

| 范围 | 测试 |
|---|---|
| V1/V2 码规范化 | `internal/centercode`：`TestCodeConformance`、`TestNormalizationErrors` |
| V3 请求签名、目标规范化与时间窗 | `internal/center`：`TestRequestSignatureVectors` |
| V4/V5 签发、V8–V15 准入 | `internal/ticket`：`TestCenterVectorHeader`、`TestCenterIssuanceVectors`、`TestCenterAdmissionVectors`、`TestSignCenterBounds`、`TestSignCenterTTLBoundaries`、`TestInvalidCBORHeadSize` |
| V6/V7 CI 与配对消息 | `internal/pairing/cpace`：`TestConformanceCI`、`TestConformancePairingMessages`、`TestCenterCIVector` |
| 生成器复现 | `python3 -B testdata/conformance/gen/run_all.py --check` 及 `testdata/conformance/gen/test_common.py` |

留给 R5：Hub 中心模式的锁内提交（§8.4 第 10–12 步）、节点激活与心跳、中心 API 客户端，以及 S、P、C 组行为场景（需要 Hub 中心模式与中心假实现）。

## R5 · 各端接入中心

已完成（离线测试、端到端测试与独立审查通过；中心服务端与真实中心联调、人工验收待执行）。设计、任务与提交对应关系、与设计稿的差异和独立审查的逐条处理见 [CENTER-CLIENT](CENTER-CLIENT.md)（第 12 节），冻结的接口见 [CONTRACTS · R5](CONTRACTS.md#r5-各端接入中心2026-11-20-冻结)。中心契约第十版写入设计期的澄清，第十一版写入实现反馈（[CENTER-CONTRACT §18](CENTER-CONTRACT.md#18-修订记录)）。中心服务端不在本仓库；离线测试用的 `internal/testutil/fakecenter` 只供测试，不是参考中心。

交付物：

1. Hub 中心模式：`internal/hub` 的中心会话（锁内第 10–12 步、承载截止 D、封存、`jti` 单次消费、按会话用量、`CenterMode` 拒绝零会话授权）；`internal/hubcenter`（节点配置、中心准入策略、激活与心跳循环、`closed` 分批上报、保留期随租期更新）；`renet-hub check|serve` 的中心模式（`check --online`、SIGHUP 只重载 `ticket_keys`、`node_superseded` 退出码 3）。
2. 中心 API 客户端：`internal/center` 的 `Client`（签名、时间校正、nonce、幂等键、R/W/F/B 分类、入口回退与长轮询）与契约 §7 全部端点的类型化封装。
3. 两端接待流程：`internal/centerflow`（Operator 登录 / 选节点 / 发码 / 提交 `/established` / 结束；Client 兑换 / 配对 / 重连 / 找回）、`internal/centerstate`（登录缓存与钉住记录）、`internal/session` 与 `internal/pairing` 的中心模式配对（SAS 钩子必填、16 `rejected` / `confirm_timeout` / `not_paired`、隧道闸门）、`internal/transport/hub` 的单次持票 wait / dial。
4. 运行时与界面：`internal/app` 的中心运行路径（Operator 的「一致」须经 `VerifySAS` 核对客户读出的 6 位）、`renetd operator --center-node` 与 `renetd client --center-code-file`；`internal/desktop` 的中心控制器与原因映射；`internal/ui` 的连接方式三选一、登录、节点下拉选择与双端 SAS 对话；`internal/settings` 的 `operator.mode = "center"`，官方构建全新安装缺省为中心模式。

证据：

| 范围 | 测试 |
|---|---|
| Hub 中心会话 | `internal/hub`：`TestCenterRoutes`、`TestCenterDeadline`、`TestCenterCommitOrdering`、`TestCenterStatusWriteFailure`、`TestZeroSessionGrantByMode` |
| 节点配置、准入策略、激活与心跳 | `internal/hubcenter`：`TestLoad`、`TestReloadCompatible`、`TestAdmissionVectors`、`TestActivationRotationAndRouting`、`TestActivationRetries`、`TestActivationTerminal`、`TestSkewAndHeartbeatRecovery`、`TestSuperseded`、`TestClockUncertainty`、`TestClosedTailAndLoad`、`TestSendTimeLeaseAndDisconnectedDeadline`、`TestPartialHeartbeatResponse`、`TestClosedBatchesRetainFinalUsage`；`cmd/renet-hub`：`TestCenterCheck`、`TestCenterReload`、`TestCenterServe` |
| 中心 API 客户端 | `internal/center`：`TestClientFallbackReplay`、`TestClientClassActions`、`TestClientIDMismatch`、`TestClientDeadlines`、`TestHeartbeatFallsBackOnlyBeforeSending`、`TestEndpointLifecycle`、`TestHeartbeatDecisionLists` |
| 配对与隧道闸门 | `internal/session`：`TestCenterConfirmAndPin`、`TestCenterDecisionWhileConfirming`、`TestCenterFailedAttempts`、`TestCenterDataGate`、`TestCenterUnpinnedData`、`TestCenterRequiresHook`、`TestCenterSeparateAuthenticatedLinksShowDifferentSAS`；`internal/pairing`：`TestSessionResponderSaveOwnership`；`internal/pairing/cpace`：`TestCenterSASIndependentExchanges` |
| 接待流程与恢复 | `internal/centerflow`：`TestCommitGatesAndRecovery`、`TestLostDecision`、`TestPrePairRestartAndNIC`、`TestRedeemContentionAndLostReply`、`TestRecoveryOffersMultipleSessions`、`TestEndAlreadyTerminal`、`TestForbiddenPreservesPin`；`internal/centerstate`：`TestRecordsAndIsolation`、`TestCorruptRecords` |
| 运行时、控制器与界面 | `internal/app`：`TestCenterReception`、`TestUnverifiedOperatorTimesOut`；`internal/desktop`：`TestControllerCenterReception`；`internal/ui`：`TestCenterSASDialog`、`TestOperatorModesLayout`；`internal/settings`：`TestValidateCenterMode`、`TestLoadBuildDefaultCenter` |
| 端到端（真实 loopback Hub 由 `hubcenter.Node` 驱动、假中心、两端 app） | `internal/integration/center_test.go`：`TestCenterReceptionLifecycle`、`TestCenterPairingInterrupted`、`TestCenterRevocation`、`TestCenterReasonDisplay`、`TestCenterDeadlines`；`TestCenterSASTimeout`（三分钟，需 `RENET_LONG_CENTER_TEST=1`） |

遗留：中心服务端在私有仓库另行实现；与真实中心服务端的联调、官方构建验收与人工验收（Operator 与 Client 都在 NAT 之后，节点在家庭网络以外）已列入下方「并行验收线」。

**下一项开发：待定**，由项目负责人决定。候选：

- R1 剩余项：贡献规则、Windows 安装包、版本与发布、签名与更新。
- 中心服务端就绪后的联调（见「并行验收线」）。

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
| 中心模式与真实中心服务端联调、官方构建验收 | Go 端离线测试、端到端测试（假中心）与独立审查已通过（[CENTER-CLIENT](CENTER-CLIENT.md) 第 12 节）。待中心服务端可用后：官方或私有测试中心 + 家庭网络以外的节点，Operator 与 Client 都在 NAT 之后，完成登录、发码、首次配对与双端 SAS 核对、断线重连、两端重启恢复、撤销与到期；官方构建验证缺省中心与缺省中心模式；记录版本、拓扑与脱敏日志，不进入离线 CI |
| 自建 Hub 的台架与公网验收 | 离线测试与独立审查已通过（[SELFHOST](SELFHOST.md) 第 14 节）。待在台架上用隔离的假车，Hub 跑在家庭网络以外的主机，Operator 与 Client 都位于 NAT 之后：分别用邀请码和数字码完成首次配对，重启两端后无码重连，按设备撤销后重载；记录版本、拓扑、结果与脱敏日志，不进入离线 CI |
| 数字配对的 Mac GUI Client 与公网验收 | LAN 台架已通过；需在同网段或接车时验证 Mac GUI Client（含 rebase 后的紧凑窗口布局），并从家庭以外网络经 DDNS 走一遍首次配对与无码重连 |
| 实车或公网下的长时运行 | 现有八小时结果只限 loopback / 内存设备 |

## 小修与加固（来自代码审查遗留）

2026-11-19 已完成：Client 会话结束原因改用 `context.Cause`、车端发现的部分发送失败补 warn 日志、`renet-tuncheck` 报告改为原子 0600 写入、Peer ID 默认显示短 ID。

2026-11-19 另完成：Operator / Client 提示区固定高度，消息变化不再挤动下方控件；linktest 请求预算改为收齐请求后起算，并在 I/O 错误时核对本地截止时间，修正超时被判为中断；L3 出口队列按一个最大 TCP 窗口及其 ACK 定容（原误用最大连接数，默认仅 64 包，大流量时尾部丢包）；Operator 抓取测试等待录制完成再停止。三个偶发失败修复前后：8/400 → 0/400、2/15 → 0/40（-race）、3/200 → 0/200（-race）。

| 项目 | 说明 |
|---|---|
| 设备名 | Peer ID 已改为默认显示短 ID、详情页可复制完整 ID；设备名目前协议里没有，需在握手中新增字段，属于协议变更，单独设计 |
| govulncheck 遗留（2026-11-19） | Go 1.27.3 标准库 GO-2026-6859 在 net/http 路径上可达，需升级工具链修复；`golang.org/x/net` 的 GO-2026-6866、GO-2026-6860 与 `golang.org/x/crypto` 的 GO-2026-5932 未被调用，随依赖升级一并处理 |
| settings 可选加固（Wave 5 审查，当时有意未做） | 路径以分隔符结尾时留下空目录；rename 后未 fsync 父目录；监听地址接受 `[]:47000`；`NICName` 未拒绝 U+202E 等 Cf 类控制字符；缺字段报错不指明字段 |

## 研究项（未排期）

- 用 F20 实车 HSFZ 抓包核对 [协议调研](enet-protocol-research.md) 中标注“仍需实测 F20”的项目：原始发现响应、alive 使用 `0x0012` 还是 `0x0013`、抓到的 `0x0043` 响应含义等。原始抓包含 VIN，只在私有环境分析，结论脱敏后写回调研文档。

- HTTP 回连：需要先取得 URL 通告方式与本地源地址选择的证据，再设计反向通道（见 [DESIGN 9.4](DESIGN.md#94-mgu-http-刷写与-ssh为什么要-l3)）。
- DoIP 及其他接口的兼容性。
