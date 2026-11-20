# R5 · 各端接入中心

状态：已实现（2026-11-20），人工验收待执行。设计于 2026-11-20 定稿，[第 11 节](#11-决定2026-11-20-负责人确认)的两项由负责人确认；第 9 节的澄清已写入契约第十版，实现反馈写入第十一版。目标见 [ROADMAP · R5](ROADMAP.md#r5--各端接入中心)。协议以 [CENTER-CONTRACT](CENTER-CONTRACT.md) 为唯一权威（当前第十一版），本文只规定 Go 端（Hub 中心模式、Operator、Client）怎样实现它，不重述契约条文。部署边界见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md)，R3 的代码与约定见 [SELFHOST](SELFHOST.md) 与 [CONTRACTS · R3](CONTRACTS.md#r3-自建-hub-与中心地址配置2026-11-20-冻结)，R5 冻结的接口见 [CONTRACTS · R5](CONTRACTS.md#r5-各端接入中心2026-11-20-冻结)。实现记录、与设计稿不同的细节和独立审查的处理见[第 12 节](#12-实现记录与审查处理)；第 1–11 节保留设计稿原文。

一句话概括：Hub 新增中心模式。它用 R4 的 `ticket.VerifyCenter` 在锁外完成第 1–9 步，`internal/hub` 在路由锁内完成第 10–12 步，并维护承载截止、封存集合、`jti` 集合与按会话的用量；激活与心跳循环放在新包 `internal/hubcenter`。`internal/center` 从只做 meta 扩展为完整的签名 API 客户端，负责时间校正、幂等键、R/B/W/F 分类与入口回退。两端的接待流程放在新包 `internal/centerflow`，复用现有配对状态机：中心模式下 SAS 钩子必填，拒绝和超时要发 16，配对连接与数据连接分开，`/established` 是开放隧道的提交点。钉住记录存放在 `state/<role>/centers/<center_id>/` 下。离线测试使用只在测试中使用的内存假中心 `internal/testutil/fakecenter`。

## 1. 范围与非目标

| 范围 | 内容 |
|---|---|
| Hub 中心模式 | `renet-hub` 配置 `mode: "center"`；激活与代次、心跳（负载、用量、`kids`、`seq`）、承载截止 D 与租约、撤销与封存、`jti` 单次消费、时钟偏差超限停止准入，以及 `node_disabled`、`node_superseded`、`unauthenticated` 的处理 |
| 中心 API 客户端 | 设备与节点两种签名主体；时间校正与 nonce；幂等键与 R/W/F/B 分类；按入口顺序有限回退；长轮询；错误码到界面原因的映射。契约 §7 的全部端点都提供类型化封装 |
| Operator | 连接方式扩展为直连 / 自建 Hub / 中心三选一。中心模式下：账号码登录、设备记录、选择节点、创建会话取码、SAS 确认、数据握手后提交 `/established`、结束接待、退出登录（吊销本机设备记录） |
| Client | 输入框识别纯 8 位码后走中心：兑换、领票、配对、看到 `active` 后才放行隧道、断线重连、找回会话、主动结束；分别显示网卡缺失、Operator 忙、授权到期、码无效、节点离线等原因 |
| 配对 | 复用 R2/R3 的消息 11–16 与状态机，补上契约 §10.2 中心模式专属的行为（SAS 钩子必填，16 `rejected` / `confirm_timeout` / `not_paired`，已核对指纹免确认，配对连接只承担配对） |
| 离线测试 | S1–S8、P1–P9 的 Go 端部分（P10 已由 R2 覆盖，保持不退化），以及 C1–C12 中由客户端可判定的部分；只用 loopback、`net.Pipe` 与内存传输 |

非目标：

- **中心服务端。** 官方中心在私有 cloud 仓库另行实现，不在本轮。本仓库不提供可运行的中心命令或二进制。
- **用假中心证明中心语义。** `internal/testutil/fakecenter` 只按契约实现足以驱动 S/P/C 场景的最小语义，供本仓库测试使用。契约第 15 节要求中心实现自己对真实数据库跑 C1、C2、C9、C10 的并发写入场景，验证唯一约束与条件写；假实现不替代这一项，也不用于宣称任何中心实现合规。
- 中心模式的 `renet-hub ticket inspect`。中心票据绑定运行时代次，离线排障价值低；用 `serve --debug` 的拒绝步骤日志排查。
- Hub 集群、跨节点迁移、自动选节点（Operator 手选，界面按实测延迟预选）。
- 管理后台、计费；Operator 内的设备列表管理只做「查看」和「吊销」。
- 设备名进入握手（ROADMAP 小修项，属于协议变更，单独设计）。
- Operator 同时在中心等待并直连监听；中心模式与另两种模式互斥。
- 车端 `carside` 不改；不涉及任何车辆编码或刷写逻辑。

## 2. 与 R3/R4 已有代码的衔接

| 已有 | R5 复用 | R5 改动 |
|---|---|---|
| `internal/hub` 的 `Policy` / `Grant` / `SetPolicy`、首字节期限、`SetupLimits` | 锁外 `Admit`、锁内 `register`、解锁后写状态的结构不变；首字节期限与 setup 限流照常适用 | `Grant` 增加 `Session`、`JTI`；`Server` 增加准入闸门、承载截止、封存、`jti` 集合、用量计数与撤销接口（第 3.6 节） |
| `internal/hubauth` | 作为中心策略的对照；配置的严格 JSON 解码器 | 导出严格解码器供中心模式配置复用（`DecodeStrict`），不改自建语义 |
| `internal/ticket.VerifyCenter`、`StepError` | 原样承担 §8.4 第 1–9 步，`StepError.Step` 写调试日志 | 无 |
| `internal/center` 的 `FetchMeta`、`CanonicalTarget`、`SignDevice` / `SignNode`、`CheckTimestamp` | 签名与目标规范化直接复用（已与 V3 向量交叉验证） | 新增 `Client`（第 4 节）；`FetchMeta` 允许 `hub` 角色 |
| `internal/centercode` | 账号码、数字码的本地规范化与字符/长度错误区分 | 无 |
| `internal/pairing/cpace` 的 `pair-center/1` 绑定 | CI 字段与零值规则（V6/V7） | `Reason` 增加 `rejected`、`confirm_timeout`、`not_paired` |
| `internal/pairing.Manager` | 不复用（直连专属：发码、码有效期、全局窗口） | 新增按会话计数的 `SessionResponder`，实现 `session.DigitalResponder` |
| `internal/session` 的消息 11–16、承诺、SAS、`ConfirmSAS` 钩子 | 消息、状态机、承诺顺序与预算计数位置不变 | 新增 `Options.Center`、`Pinned`、`HoldTunnels` 与 `ReleaseTunnels`；中心模式下钩子必填；拒绝与超时发 16；Client 在确认期间同时监听 16（第 5.1 节） |
| `internal/transport/hub` | `Wrap`、内层 Expect、`PathInfo.Hub` | 新增单次持票 `WaitTicket` / `DialTicket`，重试与领票由调用方负责 |
| `internal/settings` 版本 2 的 `Center` 字段、构建期缺省中心、`SetCenter` 只在空闲时生效 | 全部沿用 | `operator.mode` 增加 `"center"`，不增字段、不升版本（第 7.1 节） |
| `internal/app` 的 Runtime、Snapshot、presenter 与 forwarder | 数据会话交给现有的 serve 路径 | 新增中心运行路径与 SAS 应答接口（第 5、6 节） |
| `internal/desktop` 的 `SetCenter` / `CheckCenter`、`internal/ui` 的会话控制标题行与「邀请 Client」 | 标题行「模式 · 地址 · 状态」的结构 | 连接方式三选一；中心模式下标题行显示中心；邀请区只有数字码一栏；恢复中心设置卡片（commit 37f3b98 删除的部分按新设计重写） |
| `internal/locales` | 英文原文作键、三语同步 | 删除「纯 8 位码改写为直连提示」的 R5 前特例（`locales.Text` 与对应测试） |

## 3. Hub 中心模式

### 3.1 节点配置

与自建模式共用 `renet-hub` 与配置文件外形，`mode` 决定解码哪一种 schema。严格规则同 [SELFHOST 4.1](SELFHOST.md#41-配置文件)：未知字段、重复键、`null`、类型错误、尾随内容一律拒绝；**契约 §8.6 的必需项任一缺失，`serve` 拒绝启动，不回退到无鉴权，也不回退到自建模式。**

```json
{
  "version": 1,
  "mode": "center",
  "identity_dir": "/var/lib/renet-hub/identity",
  "listen": "0.0.0.0:443",
  "center": {"id": "example.net", "api": ["https://api.example.net", "https://alt.example.org/renet"]},
  "node_id": "base64url-16-bytes",
  "ticket_keys": ["base64url-32-bytes"],
  "setup_limits": {"concurrent": 64, "per_second": 32, "burst": 64, "timeout_s": 10}
}
```

| 字段 | 必需 | 约束 |
|---|---|---|
| `version` | 是 | 恰为 1 |
| `mode` | 是 | 恰为 `"center"` |
| `identity_dir` | 是 | 已有 Hub 身份（`renet-hub init`）。节点签名密钥就是 Hub 身份私钥：激活的 `node_pub` 与外层 TLS 证书是同一把 Ed25519 公钥，管理员登记的就是 `renet-hub fingerprint` 输出的指纹 |
| `listen` | 是 | 同自建模式 |
| `center.id` / `center.api` | 是 | 规则同 `settings.Center`（`[a-z0-9.-]{1,32}`；1–4 个 `https://` URL，可带路径前缀，不含用户信息、查询和片段） |
| `node_id` | 是 | base64url（无填充）的 16 字节 |
| `ticket_keys` | 是 | 1–16 个 base64url 的 Ed25519 原始公钥，`kid` 由公钥算出且不得重复。只经本文件下发（契约 §8.6），不接受任何 API 响应里的密钥 |
| `setup_limits` | 否 | 同自建模式 |

`clock_skew`（30 s）、`ticket_max_ttl`（300 s）、`heartbeat_timeout`（10 s）是契约 §14 的定值，写成常量，不进配置；`heartbeat_interval` 与 `lease_ttl` 取激活与心跳响应的最新值。

重载（Unix 上 SIGHUP）只允许改 `ticket_keys`，用于契约 §8.6 的轮换；其余字段变化视为不合法，需要重启。新配置不合法时记录错误、继续使用旧配置。移除公钥后，该密钥签发、尚未使用的票据随之失效，已建立的会话不受影响，与契约 §8.6 一致；下一次心跳的 `kids` 反映新列表。

### 3.2 启动、激活与代次

`serve` 按以下顺序进行。中心模式的首次激活需要中心能对登记的地址做外层 TLS 连通性校验，所以监听必须先于激活，但准入闸门在激活之前保持关闭：

1. 严格加载配置，只读加载身份，构造中心策略（第 3.6 节），`hub.NewServer`，并调用 `SetAdmitting(false)`。
2. 监听并开始 `Serve`。闸门关闭时，路由请求在锁内一律回 `unauthorized`，外层 TLS 握手照常完成，因此连通性校验可以通过。
3. 每个入口在本进程内首次使用前，先用 `GET /v1/meta` 核对 `center_id`；标识不一致的入口在本进程内永久排除并记错误日志（契约 §11）。
4. 生成本进程的激活幂等键，`POST /v1/node/activate`（`node_pub`、`version`、`kids`）。按契约 §7.8 的处理表处理结果：暂态错误用同一个键、同一请求体按退避（1 s 起，翻倍，上限 30 s）重试；`clock_skew` 记录偏差并报错，校时恢复前不进入中心模式，之后以心跳周期为间隔同键重试；终局错误记录后退出。同一个键重试满 23 小时仍未成功时换新键（契约第七版实现 35：激活记录在 `idempotency_ttl` 后可能被清除，旧键会被当作新键授予代次；本进程从未用过旧代次，换键无害）。
5. 成功后把 `gen` 写入策略，`seq` 从 1 起计。`node_state` 为 `active` 且时钟偏差在容忍内时打开闸门；为 `disabled` 时按 `node_disabled` 处理（闸门关闭，继续心跳）。打印 `hub=<指纹> center=<id> node=<短 ID> gen=<n> listen=<地址>`。

代次只在内存中。进程重启就是新代次，契约 §9.4 的 `old_gen_bound` 保证新代次的确认不会抵消旧进程可能承载的时间。

**`node_superseded` 的处理：** 关闭全部会话，关闭闸门，停止心跳，`serve` 以退出码 3 返回，日志写明「本节点代次已被取代，可能有另一个进程使用了同一节点身份」。不在进程内自动重新激活。理由：两个进程误用同一节点身份时，自动重新激活会互相取代、来回抖动。运维文档建议 systemd 配置 `RestartPreventExitStatus=3`。

### 3.3 心跳循环

每个 `heartbeat_interval` 发送一次，单次超时为 `heartbeat_timeout`，超时到达的响应丢弃（由 HTTP 客户端超时保证，契约 §9.1）。失败不重放：下个周期用 `seq + 1` 发新心跳。`seq` 在请求构造时递增，失败的请求同样占用一个序号。

| 请求字段 | 来源 |
|---|---|
| `gen`、`seq` | 本进程代次；本代次内从 1 起严格递增 |
| `version`、`kids` | 构建版本（非 SemVer 的构建发 `0.0.0-dev`）；当前配置全部公钥的 `kid`（base64url，11 字符） |
| `load` | 由 `Server.Usage()` 汇总：`conns` 当前连接数、`sessions` 有连接的会话数、`waiting` 只有 wait 的会话数；`rx_bps` / `tx_bps` 为两次心跳之间全节点字节增量除以间隔 |
| `sessions` | `Server.Usage()` 中当前有连接的会话：`state` 为 `waiting` 或 `paired`，`rx_bytes` / `tx_bytes` / `paired_s` 为本代次累计值 |
| `closed` | 已封存的会话及其最终累计值；同一会话不会同时出现在 `sessions` 中。会话在中心的 `revoke` 中不再出现后，从 `closed` 删除（契约 §9.3 末段） |

`sessions`、`closed` 各至多 1000 项（`heartbeat_max_entries`）。超过时按会话 ID 字节序截断并记错误日志：诚实部署不会超过（节点容量不超过 1000），截断只是防御。

响应处理在一次循环里按顺序完成：

1. 用发出时刻（`Now()`，含单调时钟读数）计算本次往返，用 `server_time` 判定时钟偏差（第 3.7 节）。
2. 每个 `renew` 项：`Server.Renew(sid, 发出时刻 + lease_s)`，直接赋值，允许缩短（契约 §9.1）。
3. 每个 `revoke` 项：`Server.Revoke(sid)`，在锁内封存并关闭全部连接，返回最终累计用量，写入 `closed` 待报集合。对本 Hub 从未见过的会话同样封存，并以全零用量报告，表示本节点不会再承载它，中心据此按 §9.4 条件 2 释放占位。
4. 采用新的 `heartbeat_interval_s`、`lease_ttl_s`；按 `node_state` 开关闸门。

### 3.4 承载截止 D 与租约

D 由 `internal/hub` 维护，用 `Options.Now` 的读数比较（生产中为 `time.Now`，含单调时钟），到期调度用新增的 `Options.AfterFunc`（签名与 `pairing.Config.AfterFunc` 一致，测试注入假时钟）。

- 准入（锁内第 12 步）：`D = max(D, Grant.Expires)`。中心策略给出的 `Grant.Expires` 就是 `exp + clock_skew`，与契约 §9.1 一致。
- 续租：`D = 发出时刻 + lease_s`，直接赋值。
- 到 D：在锁内关闭该会话的全部连接（wait 与已配对的 dial），会话不进入封存集合；再次准入需要新票据，并且同样受第 10 步检查。
- 中心失联时没有单独的宽限，`lease_ttl` 兜底（契约 §9.1）。

会话记录（D、连接集合、用量）在最后一个连接关闭后仍然保留，直到被 `Revoke`，或越过 `D + 保留期`（`Options.SessionRetention`，中心模式取 `lease_ttl + clock_skew`）。这样，正常结束的会话在下一次心跳收到 `revoke` 时，仍能在 `closed` 中报告连接关闭之前、上次心跳之后的那段尾部用量（见第 9 节第 2 项）。

### 3.5 撤销、封存与 `jti` 消费集合

| 集合 | 加入 | 保留到 | 用途 |
|---|---|---|---|
| 封存 | `Revoke` 在锁内加入 | `now + ticket_max_ttl + clock_skew`（330 s） | 第 10 步拒绝该会话的后续准入 |
| `jti` | 第 12 步消费 | `Grant.Expires`（即 `exp + clock_skew`） | 第 10 步拒绝重放 |
| 撤销索引 | 第 12 步把 wait 方、dial 方连接登记到会话 | 连接关闭 | `Revoke`、到 D 与 `CloseAll` 时找到全部连接 |

两个过期集合在插入时惰性清理（摊销 O(1)）。`jti` 集合的规模受中心的 `ticket_issue_budget` 与票据有效期约束。`unknown-peer` 与 `busy` 不消费 `jti`（第 11 步），Client 可以用同一张票据重试（S1）。

`CloseAll` 用于 `node_disabled`、`unauthenticated`、`upgrade_required`、`node_superseded`：关闭全部会话的全部连接，不封存；之后中心会在 `revoke` 中列出这些会话，Hub 届时再封存并报告 `closed`。

### 3.6 锁内第 10–12 步落在 `internal/hub` 上

结论：**需要扩展 hub 包接口，但不改 `Policy` 接口本身**。会话语义（截止、封存、`jti`、用量）都放在 `Server` 里，以 `Grant.Session` 是否为零来区分；自建模式的 Grant 不带会话，行为一个字节都不变。这与契约 §16「Hub 维护每会话 D、连接集合、封存集合、`jti` 集合与用量，Hub 包本身只依赖准入回调」一致。

签名草案：

```go
package hub

// SessionID is a center session_id. The zero value marks a self-hosted grant.
type SessionID [16]byte

type Grant struct {
	Ref                   any
	Expires, PairingUntil time.Time
	// Session binds the route to a center session and enables §8.4 steps 10–12.
	Session SessionID
	// JTI is consumed at commit and remembered until Expires. Ignored when Session is zero.
	JTI [16]byte
}

type Options struct {
	// ...existing fields...
	// AfterFunc schedules carriage deadlines on the clock of Now; nil uses time.AfterFunc.
	AfterFunc func(time.Duration, func()) (stop func() bool)
	// SealTTL is how long a revoked session stays sealed (ticket_max_ttl + clock_skew).
	SealTTL time.Duration
	// SessionRetention keeps an idle session's usage after its deadline passes.
	SessionRetention time.Duration
}

// SetAdmitting gates new session routes under the routing lock. Existing routes are unaffected.
func (s *Server) SetAdmitting(open bool)
// Renew assigns the carriage deadline. It returns false for unknown or sealed sessions.
func (s *Server) Renew(id SessionID, deadline time.Time) bool
// Revoke seals id, closes its connections under the routing lock and returns final usage.
// It is idempotent and seals sessions the server has never seen.
func (s *Server) Revoke(id SessionID) Usage
// CloseAll closes every session route without sealing and returns the number of sessions closed.
func (s *Server) CloseAll() int
// Usage reports sessions that currently have at least one connection.
func (s *Server) Usage() []Usage

type Usage struct {
	Session          SessionID
	Paired           bool          // false: wait only
	Conns            int
	RxBytes, TxBytes uint64        // cumulative for this process (generation)
	PairedFor        time.Duration // cumulative
}
```

`register`（已在路由锁内）对 `Grant.Session` 非零的请求按契约逐条执行：

| 步骤 | 现有代码 | 新增 |
|---|---|---|
| 10 | `p.Valid(g, dev)`；`now.After(g.Expires)` 时 `unauthorized` | 闸门关闭、会话在封存集合中、`jti` 已消费时 `unauthorized` |
| 11 wait | 设备已有 wait 时 `busy`，否则登记 | 等待项保存 Grant（含 `Session` 与 `Expires`）；中心 wait 的 target 为 `client_fp`，非零 |
| 11 dial | 无 wait 时 `unknown-peer`，已配对时 `busy`，`wait.target != dev` 时 `unauthorized`；都不改变状态 | `wait.grant.Session != g.Session` 时 `unauthorized`；以上拒绝都**不消费** `jti` |
| 12 | 登记 dial | 消费 `jti`；把连接登记到会话的撤销索引；`D = max(D, g.Expires)` 并重排到期调度；开始计用量 |

解锁后写 `ok`；写失败时清理连接，`jti` 保持已消费（契约 §8.4 末段）。

用量在 `connection.Read` / `Write` 中按会话原子累加，从状态字节写出之后开始计，只统计转发的内层字节（契约 §7.8 的口径）：Hub 从两端读到的计入 `rx`，写往两端的计入 `tx`。`PairedFor` 在配对与拆除时于锁内累加。

中心策略的 `Valid` 恒为真：中心模式的撤销来自心跳，而不是配置重载；准入闸门由 `SetAdmitting` 单独控制，因此 `ticket_keys` 重载不会误关已建立的会话。`PairingOperator` 恒为零：中心模式没有配对入口，target 为 Hub 自身指纹的 dial 在 `Admit` 处就会因为票据不符被拒。

中心策略（`internal/hubcenter`）：

```go
// Policy implements hub.Policy for center tickets. Keys and generation are swapped atomically.
type Policy struct{ /* center, node, hubFP; atomic keys; atomic gen */ }
func NewPolicy(cfg Config, hubFP identity.Fingerprint) (*Policy, error)
func (p *Policy) Admit(dev identity.Fingerprint, req hub.Request, now time.Time) (hub.Grant, error)
// Admit: gen == 0 → reject; ticket.VerifyCenter(token, keys, CenterExpect{
//   Center, Node, Hub, Generation: gen, TLSDevice: dev, Target: req.Target, Role: req.Role,
//   ClockSkew: 30, TicketMaxTTL: 300}, now) → Grant{Expires: exp+30s, Session: sid, JTI: jti}
func (p *Policy) Valid(hub.Grant, identity.Fingerprint) bool { return true }
func (p *Policy) PairingOperator() identity.Fingerprint      { return identity.Fingerprint{} }
func (p *Policy) SetGeneration(gen uint32)
func (p *Policy) SetKeys(keys []ed25519.PublicKey) error
func (p *Policy) Kids() []ticket.Kid
```

### 3.7 时钟偏差

Hub **不校正**自己签名用的时间戳。票据按 Hub 本地时钟校验，校正签名时间只会掩盖偏差（契约 §6.4「Hub 必须校时」）。

判定（第 9 节第 4 项拟写入契约 §6.4 的条文）：估计值 `offset = server_time − (发出时刻 + 往返 / 2)`，不确定度 `u = 1 秒 + 往返 / 2`（`server_time` 的整秒截断加半个往返）。`|offset| + u > clock_skew` 时判定超限。不确定度计入偏差一侧，判定只会更早停止准入，不会让真实偏差超过 `clock_skew` 仍在准入，从而保住 §9.2「最坏时钟偏差下也不越过硬截止」的推导。超限时记错误日志并 `SetAdmitting(false)`；之后某次心跳回到容忍内时重新打开（前提是 `node_state` 为 `active`，且没有处于 `unauthenticated` 等关闭状态）。已建立的会话照常转发直到各自的 D。激活响应与心跳响应都做这项判定；收到 `clock_skew` 错误时用 `details.server_time` 同样判定，并按暂态处理。

### 3.8 激活与心跳结果的处理

严格按契约 §7.8 的处理表实现，并按这张表做表驱动测试。下表只列 Hub 的状态变化：

| 结果 | 闸门 | 会话 | 心跳 | 进程 |
|---|---|---|---|---|
| 心跳 2xx | 按 `node_state` 与偏差判定 | 续租 / 撤销 | 继续 | — |
| 传输错误、超时、无信封 5xx、`unavailable`、`internal`、`bad_request` | 不变 | 照常到各自的 D | 下周期 `seq + 1` | `bad_request` 记错误 |
| `clock_skew` | 关闭，直到偏差恢复 | 照常 | 继续 | — |
| `node_disabled`、`unauthenticated`、`upgrade_required`、`node_state = disabled` | 关闭 | `CloseAll` | 继续，以便恢复 | — |
| `node_superseded` | 关闭 | `CloseAll` | 停止 | 退出码 3 |
| 激活的终局错误（`node_unknown`、`node_key_mismatch`、`node_check_failed`、`idempotency_key_reused`、`bad_request`、`upgrade_required`、`unauthenticated`） | 从未打开 | — | 不开始 | 记错误，退出码 2 |

### 3.9 `renet-hub` 命令

| 命令 | 中心模式下的行为 |
|---|---|
| `init`、`fingerprint` | 不变。输出的指纹交管理员登记为节点指纹 |
| `check --config FILE` | 先读 `mode` 再按对应 schema 严格校验。中心模式打印 `center`、`node`（短 ID）、各 `kid`、入口列表；离线，不联网 |
| `serve --config FILE [--debug]` | 第 3.2 节的启动顺序；`--debug` 输出 `StepError.Step` 等拒绝原因（不含完整票据）。退出码：0 收到信号正常退出；2 配置或激活的终局错误；3 `node_superseded` |
| `ticket inspect` | 只支持自建模式；对中心模式配置报「中心模式不支持」（第 1 节非目标） |

`renet-hub` 新增构建期版本变量（`-ldflags -X main.version=...`），缺省 `0.0.0-dev`，用于 `version` 字段与 `Renet-Client: hub/<version>`。

## 4. 中心 API 客户端

### 4.1 结构

`internal/center` 扩展为三层：已有的纯编解码（签名、目标规范化、meta），新增的传输层 `Client.Do`（签名、时间、幂等、分类、回退），以及类型化端点。传输层不理解业务，端点层不做重试。

```go
package center

type ClientOptions struct {
	Center     settings.Center
	Role       string // "operator" | "client" | "hub"; also selects the Renet-Client role
	AppVersion string // non-SemVer builds send 0.0.0-dev
	Device     ed25519.PrivateKey // device principal (Operator, Client)
	NodeKey    ed25519.PrivateKey // node principal (Hub), together with Node
	Node       [16]byte
	HTTP       *http.Client       // nil: TLS-verifying default; tests inject the fake center's client
	Now        func() time.Time
	Logger     *slog.Logger
}
func NewClient(o ClientOptions) (*Client, error)

type Call struct {
	Method, Path string        // Path relative to the entry, e.g. "/v1/sessions/{id}/tickets"
	Query        url.Values
	Body         any           // JSON; nil for GET
	Key          string        // Idempotency-Key; required for POST except heartbeat
	Nonce        bool          // fresh Renet-Nonce per attempt (§6.5)
	Wait         time.Duration // long-poll wait_s; extends the attempt timeout (§6.6)
}
type Response struct{ Status int; Replayed bool; Entry string }
func (c *Client) Do(ctx context.Context, call Call, out any) (Response, error)

type Class uint8 // ClassRetry (R) | ClassWait (W) | ClassFix (F) | ClassBusiness (B)
type Error struct {
	Status     int
	Code       string // stable machine value; never shown raw
	Class      Class
	RetryAfter time.Duration
	Details    json.RawMessage
}
func (e *Error) EndReason() string  // session_ended
func (e *Error) State() string      // state_conflict
func (e *Error) ServerTime() (time.Time, bool)
var ErrUnreachable = errors.New("center unreachable") // every attempt ended in an R-class failure
func ClassOf(code string) Class
func NewIdempotencyKey() string     // 128-bit CSPRNG, base64url
```

### 4.2 签名、时间校正与 nonce

- 每次尝试都重新签名（新时间戳），请求体字节不变。设备主体用 `SignDevice`，节点主体用 `SignNode`；请求目标为「入口路径前缀 + 端点路径 + 查询」，经 `CanonicalTarget` 规范化后发送并签名，规范化失败直接报错、不发送。
- **时间校正（只用于设备主体）：** `Client` 持有进程内的偏移量。首次核对入口时，用 meta 的 `server_time` 初始化；收到 `clock_skew` 时按 `details.server_time` 更新，同键、同入口重新签名重发一次。节点主体不校正（第 3.7 节）。
- **nonce：** `Call.Nonce` 为真时，每次尝试生成新的 16 字节 nonce；收到 `nonce_reused` 时换新 nonce 重发一次（F 类）。只有创建会话与 Operator 读取 `open` 会话需要它（契约 §6.5）。
- 请求头只有契约 §5.1 的几项加 `User-Agent: Remote-ENET`；拒绝重定向，不用 cookie jar，不带其他凭据。请求体上限 16 KiB（心跳 512 KiB），响应体上限 256 KiB，超限即停止读取。
- 日志只记录方法、端点模板、状态、`code` 与 `Renet-Request-Id`，不记录请求体、响应体、码、票据和完整指纹。

### 4.3 幂等键、重试分类与入口回退

`Do` 对一次调用按契约 §11、§12 执行：

| 结果 | 动作 |
|---|---|
| 2xx | 返回；`Idempotency-Replayed: true` 记入 `Response.Replayed` |
| R（传输错误、TLS 错误、无信封的非 2xx、`unavailable`、`internal`） | 换下一个入口（按配置顺序循环），沿用同一个幂等键 |
| W（`rate_limited`、`quota_releasing`、`session_not_ready`） | 不换入口；剩余预算够时在原入口等待 `retry_after_s` 后重试，否则把 `*Error` 返回给调用方 |
| F（`clock_skew`、`nonce_reused`） | 修正后在原入口重发一次；再次得到 F 则返回 |
| B | 立即返回，不重试，不换入口 |

- 一次调用至多 3 次尝试，合计不超过 30 秒，单次超时 10 秒；长轮询的单次超时为 `wait + 10` 秒，整次预算相应延长（契约 §6.6）。W 类等待计入整次预算。
- 每次调用都从第一个入口开始。契约第二版已去掉入口黏滞，限流在入口间共享。
- 入口在本进程首次使用前核对 meta：`IDMismatch` 永久排除，`Unreachable` 按 R 类处理。全部入口被排除时返回 `ErrUnreachable`，界面显示「中心标识不一致」或「无法连接中心」。
- **幂等键归调用方所有。** 一次逻辑操作（例如「为这个会话提交 `/established`」）生成一个键，跨重试、跨 `Do` 调用沿用，直到得到终局结果。键不持久化：进程重启后改用资源 GET 找回结果（契约 §6.3 的保留期规则）。
- 返回 `upgrade_required` 时停止该角色的全部中心操作，界面提示升级。

### 4.4 长轮询

`Session(ctx, id, since, wait, nonce)` 封装 `GET /v1/sessions/{id}?since_version=N&wait_s=W`，`W` 取 25 秒（`longpoll_max`）。`centerflow` 在会话未结束期间持续长轮询：返回后立即发起下一次，失败后按 1–5 秒退避。Operator 读取 `open` 会话需要码（启动后首次读取或重新显示）时才带 nonce；Client 的读取从不带。

### 4.5 端点封装

```go
// Operator
func (c *Client) Login(ctx context.Context, key, accountCode, label string) (Device, Account, error)
func (c *Client) Account(ctx context.Context) (AccountView, error)
func (c *Client) Devices(ctx context.Context) ([]DeviceEntry, error)
func (c *Client) RevokeDevice(ctx context.Context, key string, fp identity.Fingerprint) error
func (c *Client) Nodes(ctx context.Context) ([]Node, error)
func (c *Client) CreateSession(ctx context.Context, key string, node NodeID) (Session, error) // with nonce
func (c *Client) Established(ctx context.Context, key string, id SessionID, client identity.Fingerprint) (Session, error)
// Both roles
func (c *Client) Session(ctx context.Context, id SessionID, since int64, wait time.Duration, nonce bool) (Session, error)
func (c *Client) Ticket(ctx context.Context, key string, id SessionID) (Ticket, error)
func (c *Client) End(ctx context.Context, key string, id SessionID, reason string) (Session, error)
// Client
func (c *Client) Redeem(ctx context.Context, key, code string) (Session, error) // adds the 16-byte salt
func (c *Client) ClientSessions(ctx context.Context) ([]Session, error)
// Hub
func (c *Client) Activate(ctx context.Context, key string, req Activation) (Activated, error)
func (c *Client) Heartbeat(ctx context.Context, req Heartbeat) (HeartbeatResult, error)
```

- 线格式结构体在本包内定义，字段按契约 §7；未知字段忽略，契约声明「省略而不是 `null`」的字段用指针区分是否存在。指纹接受带与不带 `-` 两种形式；`code` 必须恰为 8 位 ASCII 数字，否则视为响应损坏（R 类以外的协议错误，不重试）。
- `Ticket.Raw` 是不透明字节：解码后检查长度在 1–1024 字节之间，并核对 `role`、`peer_fp`、`node` 与会话一致；不解释票据内容（契约 §8.5）。
- 账号码与数字码在调用前由 `centercode` 规范化。格式错误在本地直接提示，不发请求（契约 §4.1）。
- `Login` / `Redeem` 的 `salt` 每个幂等键生成一次，随请求体在重试中保持不变，否则同键会变成异体（`idempotency_key_reused`）。

### 4.6 错误码到界面原因

`centerflow` 把 `*center.Error`、`ErrUnreachable` 与 Hub 状态码转换为稳定的原因码（`centerflow.Reason`，字符串），desktop 通过 `locales` 映射为文案（第 6.5 节）。原则：

- 只按 `code` 分支，`message` 不显示。
- `session_ended` 与会话对象的 `end_reason` 走同一张结束原因表。
- Hub 的 `unauthorized` 不提供原因：两端先查询会话状态，按结束原因提示；未结束时领新票据重试一次（契约 §8.5），仍被拒则显示「Hub 拒绝了连接授权」并按退避继续查询会话。
- `unauthenticated`：Operator 清除本地登录缓存、回到登录；Client 丢弃本次会话、回到输入数字码。

## 5. 配对与连接

### 5.1 `internal/session` 的改动

```go
// CenterBinding selects pair-center/1 and supplies the CI fields both peers obtain independently.
type CenterBinding struct {
	CenterID string
	Session  [16]byte
	Hub      identity.Fingerprint // the outer pinned fingerprint
}

type Options struct {
	// ...existing fields...
	// Center enables center mode. ConfirmSAS is then mandatory and Run fails without it.
	// Client: a non-empty DigitalCode makes this a pairing connection; empty makes a data connection.
	// Operator: DigitalManager answers pairing connections; Pinned decides data connections.
	Center *CenterBinding
	// Pinned reports whether the Operator holds a pin for this client in the bound session.
	Pinned func(identity.Fingerprint) bool
	// HoldTunnels refuses inbound and outbound tunnels until ReleaseTunnels.
	HoldTunnels bool
	// ConfirmTimeout bounds a manual SAS check; zero uses 3 minutes (confirm_timeout).
	ConfirmTimeout time.Duration
}

// ReleaseTunnels opens the tunnel gate once; later calls are no-ops.
func (s *Session) ReleaseTunnels()
var ErrTunnelsHeld = errors.New("session: tunnels held until the center confirms the session")
```

`cpace.Reason` 新增 `ReasonRejected = "rejected"`、`ReasonConfirmTimeout = "confirm_timeout"`、`ReasonNotPaired = "not_paired"`。

| 行为 | 直连 / 自建（不变） | 中心模式 |
|---|---|---|
| 能力 | `pair-direct/1` / `pair-hub/1` | `pair-center/1`；`cpace.Binding` 填 `CenterID`、`SessionID`、`HubFP` |
| Client 声明 | 有码时声明 | 只在 `DigitalCode` 非空（本会话还没有钉住记录）时声明；声明了而 Operator 未声明时以 `unsupported` 关闭，不计尝试（P9） |
| Operator 声明 | 有 `DigitalManager` 时声明 | 总是声明（会话处于 `authorized` 或 `active` 时才会有连接到达） |
| `ConfirmSAS` | nil 自动通过 | 必填；钩子收到 SAS 与对端指纹，返回 `(true, nil)` 为一致，`(false, nil)` 为不一致；超过 `ConfirmTimeout` 视为超时 |
| Operator 拒绝 | 本地以 `confirmation` 结束 | 不一致时发 16 `rejected`，超时发 16 `confirm_timeout`，再以同名原因返回；由 `centerflow` 以同名原因调用 `/end` |
| Client 拒绝 | 同上 | 不发 16（16 只由 O 发往 C），以 `rejected` / `confirm_timeout` 返回，由 `centerflow` 调用 `/end` |
| Client 等待 16 | 本地确认之后再读 16 | 确认期间并行读取控制流：16 `accepted` 先到时保存，等本地确认后完成；16 拒绝先到时取消确认对话，以对端给出的原因返回；本地确认先完成时继续等 16（C3） |
| 已核对指纹 | — | 由钩子实现：`centerflow` 记住本会话已核对过 SAS 的对端指纹，同一指纹再次配对时钩子立即返回真（C3/O3、P4）；指纹不同时重新显示 SAS（P6） |
| 信任写入 | `Remember` 写 `trusted_peers` | `Remember` 写钉住记录（第 7.3 节）；Operator 写入失败时发 16 `store_failed`，由 `centerflow` 以 `pairing_lost` 结束 |
| 配对后的连接 | 直连：16 `accepted` 后沿用同一连接 | 返回 `DigitalPaired` 后由调用方关闭连接，`HoldTunnels` 保证这条连接上不开放任何隧道 |
| 数据连接 | — | Operator：Client 未声明配对能力时，`Pinned(client)` 为真则继续正常 session，否则发 16 `not_paired` 并关闭。Client：`Run` 在 Hello 之后返回；之后在控制流上收到 16 `not_paired` 时以 `ReasonNotPaired` 结束（第 9 节第 1 项） |
| 隧道闸门 | 无 | `HoldTunnels` 为真时，`OpenTunnel` 返回 `ErrTunnelsHeld`，入站的 OpenTunnel 流直接关闭（linktest 也一样）；`ReleaseTunnels` 之后恢复。控制消息（Ping、日志等）不受影响 |

预算由 `internal/pairing` 新增的按会话响应器承担（Operator 侧），Client 侧的计数在 `centerflow`：

```go
// SessionResponder implements session.DigitalResponder for one center session.
// Each received PakeCommit charges one attempt before Yb is sent; rejection by 16 instead of 12 does not.
func NewSessionResponder(code string, budget int) *SessionResponder
func (r *SessionResponder) Begin(ctx context.Context) (session.DigitalAttempt, error) // budget exhausted → cpace.ReasonBudgetExhausted
func (r *SessionResponder) AttemptsLeft() int
func (r *SessionResponder) Close()
```

`internal/pairing` 不能导入 `session`（session 依赖 pairing 的 cpace），因此 `Begin` 返回 `*SessionAttempt`，由 app 或 `centerflow` 包一层适配 `session.DigitalResponder`，与现有 `digitalResponder` 的做法一致。

### 5.2 Operator 接待状态机（`centerflow.Operator`）

```
idle ──Start(node)──▶ open(显示码) ──兑换──▶ authorized:pairing ──钉住──▶ authorized:data ──/established 成功──▶ active ──▶ ended
  ▲                                                                                                                │
  └──────────────────────────────────────── End / 结束原因（界面显示）────────────────────────────────────────────┘
```

| 阶段 | 动作 |
|---|---|
| Start | `CreateSession(key, node)`（带 nonce）。`quota_releasing` 由 `Do` 在原入口自动等待，超出预算后界面显示「额度正在释放」并继续自动重试；`device_busy` 时读取 `Account` 中本设备的未结束会话，按第 5.6 节恢复或结束；`node_unavailable` 回到节点列表 |
| open | 码只保存在内存，显示为 `XXXX XXXX`，倒计时到 `code_expires_at`；长轮询等待兑换 |
| authorized:pairing | 领 wait 票据，`WaitTicket(target = client_fp)`。连接到达后以中心模式运行 session：`DigitalManager` 为本会话的 `SessionResponder`（码来自创建响应），`ConfirmSAS` 为界面钩子。得到 `DigitalPaired` 即关闭连接，转入 data。配对连接中断、超时或格式错误时，本次尝试已计数，领新票据重新登记 wait（O 状态机「任意」行） |
| authorized:data | 领新票据重新登记 wait。数据连接 `Run` 成功（`Pinned` 检查通过）后，用该操作的固定幂等键提交 `/established`，暂态失败按退避重试，直到成功或会话结束 |
| active | 收到 `/established` 成功响应后调用 `ReleaseTunnels`，把会话交给 app 现有的 serve 路径（presenter、抓取、链路测试）。连接断开后领新票据重新登记 wait，等待 Client 的数据连接重连 |
| ended | 长轮询看到 `ended`、领票得到 `session_ended`、或用户结束（`End(operator_ended)`）：关闭连接，删除钉住记录，丢弃码，显示结束原因，回到 idle |

领票：`session_not_ready`（W）继续长轮询；`session_ended` 按结束原因收尾；Hub 回 `busy` 说明本设备已有 wait（例如另一个进程用了同一个状态目录），按退避重试并提示；回 `unauthorized` 按第 4.6 节处理。每条新连接都领一张新票据，票据不复用（契约 §8.5）。

### 5.3 Client 状态机（`centerflow.Client`）

| 阶段 | 动作 |
|---|---|
| 输入 | `invite.ParseInput` 得到 `Center` 后，用 `centercode` 规范化并把码交给 `centerflow`。没有配置中心时，本地提示「未配置中心」，不发请求 |
| 兑换 | `Redeem(key, code)` 得到 `authorized` 会话：`node`、`operator_fp`、`pair_deadline`。`code_invalid` 显示「数字码无效或已过期」，并清空输入框 |
| 配对 | 本地计一次尝试（`pake_budget` = 3，已用尽时以 `pake_failed` 调用 `/end`）。领 dial 票据，`DialTicket(peer = operator_fp)`，内层 Expect 为 `operator_fp`，`Center` 绑定填会话与外层指纹，带 `DigitalCode` 运行 session。`unknown-peer` 时用同一张票据按 0.5–5 秒退避重试，距 `ticket_expires_at` 不足 10 秒时换新票据；这段等待不计尝试（PakeCommit 尚未发出）。`busy` 显示「Operator 忙」并退避重试 |
| 配对失败 | PAKE 失败统一显示「数字码不正确或连接被篡改」，附剩余尝试次数，**不自动重试**：用户再次点「连接」时发起下一次（契约 §10.3） |
| 钉住 | 本地确认一致且收到 16 `accepted` 后写入钉住记录，关闭配对连接 |
| 数据连接 | 领新票据，不带码重新拨号（Hello 不声明配对能力）。`Run` 返回后长轮询会话，看到 `active` 才 `ReleaseTunnels`，把会话交给现有 forwarder；在此之前入站的 OpenTunnel 一律拒绝 |
| active | 断线后用新票据重连（数据连接，退避沿用 `RetryMin` / `RetryMax`）；领票得到 `session_ended`，或会话读取为 `ended` 时停止，删除钉住记录并显示原因 |
| 停止 | 用户点「停止」：`End(client_ended)`，删除钉住记录，丢弃码 |

码只在 Client 内存中，配对完成或会话结束后立即清零丢弃（契约 §4.2）。网卡缺失、未发现车辆等既有原因仍由 app 现有的 NIC 与发现流程给出，它们发生在拨号之前：Client 先确认网卡可用，再兑换码，避免码被兑换后才发现无法连接车辆（兑换会核销码）。

### 5.4 P1–P10 行为对照

| 场景 | 实现位置与要点 |
|---|---|
| P1 正常配对 | `centerflow` 编排：配对连接 → 两端钉住 → 关闭 → 数据连接 → `/established` → `ReleaseTunnels`；Client 看到 `active` 前拒绝 OpenTunnel |
| P2 猜错码 | Tb 校验失败；两端各计一次（Operator 在 `SessionResponder.Begin` 计，Client 在发出 11 前计）；用尽后 Operator 发 16 `budget_exhausted` 并以 `pake_failed` 结束，Client 收到后同样以 `pake_failed` 结束 |
| P3 承诺不符、乱序、重复、长度错误、Tb 前断开、16 代替 12 | 已有的 `readPair` 严格解码与计数位置；16 代替 12 时 `SessionResponder` 不计数 |
| P4 16 丢失 | Operator 已写钉住记录并记下已核对指纹；Client 未完成，用户再次发起；Operator 钩子对同一指纹立即返回真，直接回 `accepted` |
| P5 SAS 不一致或超时 | 第 5.1 节的 16 `rejected` / `confirm_timeout`；`centerflow` 以同名原因 `/end`，界面显示对应原因 |
| P6 对端指纹变化 | 已核对指纹不同，钩子重新弹出 SAS |
| P7 重启与钉住记录丢失 | 第 5.6 节 |
| P8 中间人 | 两端 SAS 不同：在 cpace 包内用固定标量（包内测试注入随机源）做确定性测试，并在 session 层用内存中继复现一次 |
| P9 能力协商 | Client 声明 `pair-center/1` 而 Operator 未声明时以 `unsupported` 关闭且不计数；篡改 Hello 的任一字节（含能力列表）使 PAKE 失败；数据连接不声明能力 |
| P10 直连 | 不改；R2 的测试保持通过 |

### 5.5 授权提交点与隧道闸门

两道闸门叠加，任何一道都足以阻止诊断流量：

1. **session 层：** 中心模式的 session 一律以 `HoldTunnels: true` 运行。配对连接永远不会释放；数据连接只在 Operator 收到 `/established` 成功响应后、Client 看到 `active` 后才释放。
2. **app 层：** presenter（Operator 的 HSFZ / L3 呈现与诊断端口）在释放之后才启动，Client 的 forwarder 在释放之后才接受隧道。Snapshot 的阶段在释放之前显示为「等待中心确认」，不显示「已连接」。

到硬截止仍未提交成功时，Hub 在截止时刻关闭连接（票据 `exp` 与租约都不越过硬截止），会话以 `pair_timeout` 结束，界面提示「无法连接中心，未能在时限内完成配对」（契约 §10.4 第 5 步，C7）。

### 5.6 重启与恢复

启动中心模式时，`centerflow` 先对账本地钉住记录与中心的会话列表（Operator 用 `Account` 中 `operator_fp` 等于本机的未结束会话，Client 用 `ClientSessions`）：

| 中心会话 | 本地钉住记录 | 处理 |
|---|---|---|
| `open` | — | 以 `pairing_lost` 结束（按契约 §10.4 字面执行，见第 9 节第 6 项） |
| `authorized` | 无，或损坏 | 以 `pairing_lost` 结束；界面提示需要新码 |
| `authorized` | 有 | Operator：进入 authorized:data；Client：进入数据连接 |
| `active` | 有 | 直接建立数据连接 |
| `active` | 无，或损坏 | 以 `pairing_lost` 结束 |
| 读取返回 `not_found` 或 `ended` | 有 | 删除钉住记录（Client 在结束 10 分钟后读取会得到 `not_found`，见第 9 节第 5 项） |

码与预算都不持久化，所以配对完成之前的重启一律需要新码（契约 §10.4 恢复规则）。

## 6. Operator 与 Client 的界面流程

遵循 [Pixel Workbench](UI-DESIGN.md)：直角、整数网格、钴蓝强调色、等宽数据，复用现有 card、value、metric 与主题，不引入圆角、渐变或模糊。所有新文案三语同步（第 6.5 节）。

### 6.1 设置

- **连接方式**：`Select` 由两项改为三项「直连 / 自建 Hub / 中心」，对应 `operator.mode` 的 `direct` / `hub` / `center`。选中「中心」时，表单显示当前中心（`center.id` 与入口数量）和「检查中心」按钮，不显示监听地址与 Hub 字段。
- **中心卡片**（两端都有，按新设计重写 commit 37f3b98 删除的部分）：中心 ID、入口列表（每行一个）、「检查中心」（调用现有 `CheckCenter`，逐入口显示已验证 / 不可达 / 标识不一致）、「切换中心」。切换仍只经 `Controller.SetCenter`，只能在空闲时执行，并用 mygo 的原生确认对话框提示「切换中心不会迁移或删除旧中心的登录与记录」。官方构建预填构建期缺省中心。
- 当前模式为中心时不能清空中心：`settings.Validate` 拒绝（第 7.1 节），界面提示先切换连接方式。

### 6.2 Operator 首页（中心模式）

会话控制卡片的结构不变，只是内容按模式填充：

- **标题行**：`会话控制 · 一次一个 Client · 中心 · <center_id> · <状态>`。状态取以下之一：未登录 / 已登录 · <账号显示名> / 等待兑换 / 等待来访设备 / 正在核对 / 等待中心确认 / 已连接 / 接待已结束：<原因>。
- **账号区**（未登录时显示）：账号码输入框（显示为 `XXXX-XXXX`，本地规范化，格式错误即时提示字符错误或长度错误）、可选的「设备名称」（缺省为空，不自动填主机名，避免未经同意把本机信息发给中心）、「登录」。已登录时显示账号名、有效期（到期前 7 天起用等待色提示）、额度 `占用/总数`，以及「设备列表」（查看与吊销）和「退出登录」。退出登录会吊销本机设备记录（`RevokeDevice(本机)`）并清除本地登录缓存：设备身份是长期密钥，只清本地缓存并不能让它失效。
- **节点选择**：节点列表为单选表格，列为 区域 / 负载 / 实测延迟 / 状态（`full` 不可选）。进入页面和点「刷新」时，对每个节点的 `addr:port` 测一次 TCP 连接耗时（并发不超过 4，超时 3 秒），按延迟预选最低者。只测 TCP 不做 TLS，以免占用 Hub 的 setup 配额。
- **开始与结束**：「生成数字码」（等同开始接待，Hub 模式下这里是「连接 Hub 并等待」）与「结束接待」。
- **邀请 Client**：中心模式下只有「数字码」一栏，不显示原始邀请码一栏（中心模式没有邀请码）。码以 Pixel Code 大号等宽显示为 `XXXX XXXX`，附倒计时与「复制数字码」（剪贴板只有 8 位码分组，不含地址）。提示文案为「Client 只需输入这 8 位数字」。兑换后该栏显示「已兑换 · 来访设备 <短 ID>」，码从内存与界面清除。
- **到期提示**：会话 `expires_at` 前 10 分钟起在标题行用等待色显示「授权将于 HH:MM 到期」。

### 6.3 SAS 确认对话

用 mygo 窗口内的 `ui.Modal`，按主题绘制（直角面板、1 DIP 边框、钴蓝主按钮），不用系统对话框：SAS 需要 24 DIP 的 Pixel Code 大号数字，系统对话框做不到，而且窗口内对话框能在会话结束时由程序关闭。

| 端 | 内容 | 操作 |
|---|---|---|
| Operator | 标题「核对短校验码」；说明「请客户读出其屏幕上的 6 位数字并填入下框，一致后才能确认」；SAS 以 `XXX XXX` 显示；6 位数字输入框（只接受 ASCII 与全角数字，忽略空格）；来访设备短 ID；剩余时间（3 分钟倒计时） | 「确认」只在输入恰好 6 位且与本机 SAS 一致时可用（第 11 节决定 1）；「不一致」始终可用。输入不一致时不自动判为拒绝，只在框下提示「与本机显示不一致」，由 Operator 选择重新输入或点「不一致」。输入内容不记日志、不进 Snapshot |
| Client | 标题「核对短校验码」；说明「请把下面 6 位数字读给 Operator」；SAS；Operator 短 ID；剩余时间 | 「一致」「不一致」 |

比对在界面层完成：`AnswerSAS` 只接收「一致 / 不一致」，Operator 侧的「一致」只能由输入匹配后的「确认」触发，desktop 不提供绕过输入的确认动作。对话框同时只有一个；会话结束、连接中断或收到对端 16 时由程序关闭并显示原因。超时由 session 层判定，对话框只显示倒计时。

### 6.4 Client 首页

输入框占位文字改为「邀请码、地址:端口 + 数字码，或 8 位数字码」，识别仍在 `desktop.runtimeConfigs`（`invite.ParseInput`）。纯 8 位码：已配置中心时走第 5.3 节；未配置时显示「未配置中心。请在设置中填写中心，或输入直连地址和数字码」。删除 `locales.Text` 中「中心数字码改写为直连提示」的特例与 `TestBareCodeDisplaysDirectAddressHint`，改为分别断言两种文案。

状态区按阶段显示：正在兑换 / 等待 Operator / 正在核对 / 等待中心确认 / 已连接 / 已结束：<原因>；配对失败时显示剩余尝试次数，「连接」按钮文案变为「重试配对」。连接详情的「连接方式」显示「经中心 <center_id> · 节点 <区域>」。

自检（`internal/checkup`）对纯 8 位码只做中心入口的 meta 检查，**不兑换**：兑换会核销码。

### 6.5 失败原因与文案

沿用 `internal/locales` 的英文原文作为键。下表为新增键（结束原因用会话对象的 `end_reason`，客户端原因由 `centerflow` 产生）。

| 原因码 | 端 | en（键） | zh-Hans | zh-Hant |
|---|---|---|---|---|
| 模式名 | 两端 | Center | 中心 | 中心 |
| `center_unconfigured` | Client | No center is configured. Set one in Settings, or enter a direct address and code. | 未配置中心。请在设置中填写中心，或输入直连地址和数字码 | 未設定中心。請在設定中填寫中心，或輸入直連位址和數字碼 |
| `center_unreachable` | 两端 | Cannot reach the center | 无法连接中心 | 無法連線至中心 |
| `center_id_mismatch` | 两端 | Center identity mismatch | 中心标识不一致 | 中心識別不符 |
| `upgrade_required` | 两端 | Update the app to continue | 请升级应用后继续 | 請更新應用程式後繼續 |
| `rate_limited` | 两端 | Too many requests. Try again later. | 请求过于频繁，请稍后再试 | 請求過於頻繁，請稍後再試 |
| `account_code_format` | Operator | Account code has an invalid character / Account code must be 8 characters | 账号码含无效字符 / 账号码应为 8 位 | 帳號碼含無效字元 / 帳號碼應為 8 位 |
| `account_code_invalid` | Operator | Account code not found | 账号码无效 | 帳號碼無效 |
| `account_disabled` | Operator | Account disabled | 账号已停用 | 帳號已停用 |
| `account_expired` | 两端 | Account authorization expired | 账号授权已到期 | 帳號授權已到期 |
| `device_limit` | Operator | Device limit reached. Revoke an old device first. | 账号设备数已满，请先吊销旧设备 | 帳號裝置數已滿，請先撤銷舊裝置 |
| `device_busy` | Operator | This device already has an open session | 本机已有未结束的接待 | 本機已有未結束的接待 |
| `unauthenticated` | Operator | Sign-in expired. Enter the account code again. | 登录已失效，请重新输入账号码 | 登入已失效，請重新輸入帳號碼 |
| `unauthenticated` | Client | Authorization lost. Enter a new numeric code. | 授权已失效，请输入新的数字码 | 授權已失效，請輸入新的數字碼 |
| `quota_exhausted` | Operator | Concurrent session limit reached | 并发额度已满 | 並行額度已滿 |
| `quota_releasing` | Operator | Releasing the previous session. Retrying automatically. | 上一个接待正在释放，自动重试中 | 上一個接待正在釋放，自動重試中 |
| `node_unavailable` | Operator | Node unavailable. Choose another node. | 节点不可用，请改选节点 | 節點無法使用，請改選節點 |
| `code_invalid` | Client | Numeric code invalid or expired | 数字码无效或已过期 | 數字碼無效或已過期 |
| `operator_waiting` | Client | Waiting for the operator | 等待 Operator | 等待 Operator |
| `busy` | Client | Operator busy（R2 已有） | Operator 忙 | Operator 忙碌中 |
| `hub_unauthorized` | 两端 | The hub refused the connection authorization | Hub 拒绝了连接授权 | Hub 拒絕了連線授權 |
| `awaiting_center` | 两端 | Waiting for center confirmation | 等待中心确认 | 等待中心確認 |
| `confirmation` | 两端 | Incorrect code or connection tampered（R2 已有） | 数字码不正确或连接被篡改 | 數字碼不正確或連線遭竄改 |
| `attempts_left` | Client | Attempts left: %d（R2 已有） | 剩余尝试次数：%d | 剩餘嘗試次數：%d |
| SAS 对话 | 两端 | Verify the short code / Ask the customer to read the 6 digits on their screen and enter them below. You can confirm only when they match. / Read these 6 digits to the operator. / Does not match this screen / Match / Mismatch / Confirm | 核对短校验码 / 请客户读出其屏幕上的 6 位数字并填入下框，一致后才能确认 / 请把这 6 位数字读给 Operator / 与本机显示不一致 / 一致 / 不一致 / 确认 | 核對短校驗碼 / 請客戶讀出其螢幕上的 6 位數字並填入下框，一致後才能確認 / 請將這 6 位數字讀給 Operator / 與本機顯示不一致 / 一致 / 不一致 / 確認 |
| 结束原因 `operator_ended` | 两端 | Operator ended the session | Operator 已结束接待 | Operator 已結束接待 |
| `client_ended` | 两端 | Client ended the session | Client 已结束连接 | Client 已結束連線 |
| `rejected` | 两端 | Short codes did not match. Session cancelled. | 短校验码不一致，接待已取消 | 短校驗碼不一致，接待已取消 |
| `confirm_timeout` | 两端 | Short code was not confirmed in time | 未在时限内核对短校验码 | 未在時限內核對短校驗碼 |
| `pake_failed` | 两端 | Too many failed attempts. Request a new code. | 尝试次数已用尽，请索取新码 | 嘗試次數已用盡，請索取新碼 |
| `pairing_lost` | 两端 | Pairing state lost. Request a new code. | 配对状态丢失，请索取新码 | 配對狀態遺失，請索取新碼 |
| `code_expired` | Operator | Numeric code expired | 数字码已过期 | 數字碼已過期 |
| `redeem_limit` | Operator | Code voided by the redemption limit | 数字码因兑换上限作废 | 數字碼因兌換上限作廢 |
| `pair_timeout` | 两端 | Pairing was not completed in time | 未在时限内完成配对 | 未在時限內完成配對 |
| `idle_timeout` | 两端 | Session ended after a long disconnection | 长时间未连接，接待已结束 | 長時間未連線，接待已結束 |
| `account_disabled` | 两端 | Account disabled | 账号已停用 | 帳號已停用 |
| `device_revoked` | 两端 | Device revoked | 设备已被吊销 | 裝置已遭撤銷 |
| `node_lost` | 两端 | Node offline | 节点离线 | 節點離線 |
| `node_disabled` | 两端 | Node disabled | 节点已停用 | 節點已停用 |
| `admin_revoked` | 两端 | Session ended by the administrator | 管理员已结束接待 | 管理員已結束接待 |
| 界面标签 | Operator | Sign in / Account code / Device name (optional) / Sign out / Devices / Revoke / Node / Region / Load / Latency / Refresh / Generate numeric code / End reception / Copy numeric code / Client only needs these 8 digits. / Redeemed · visitor %s / Authorization ends at %s / Check center / Switch center | 登录 / 账号码 / 设备名称（可选）/ 退出登录 / 设备列表 / 吊销 / 节点 / 区域 / 负载 / 延迟 / 刷新 / 生成数字码 / 结束接待 / 复制数字码 / Client 只需输入这 8 位数字 / 已兑换 · 来访设备 %s / 授权将于 %s 到期 / 检查中心 / 切换中心 | 登入 / 帳號碼 / 裝置名稱（選填）/ 登出 / 裝置清單 / 撤銷 / 節點 / 區域 / 負載 / 延遲 / 重新整理 / 產生數字碼 / 結束接待 / 複製數字碼 / Client 只需輸入這 8 位數字 / 已兌換 · 來訪裝置 %s / 授權將於 %s 到期 / 檢查中心 / 切換中心 |
| 路径 | 两端 | Via center %s · node %s | 经中心 %s · 节点 %s | 經中心 %s · 節點 %s |

日志、诊断导出、自检报告与订阅状态中不出现账号码、数字码、SAS、票据和完整的会话 ID（只显示前 6 个字符）。

## 7. 状态与存储

### 7.1 settings

- `operator.mode` 增加 `"center"`。不新增字段，不升 schema 版本：旧版程序读到 `"center"` 时校验失败、拒绝加载（与 R3「拒绝而不误读」的原则一致）。
- `mode == "center"` 时要求 `center` 已配置，`listen` 可以为空（与 Hub 模式相同，运行时不监听）；直连与 Hub 字段可以保留，运行时忽略。
- **缺省连接方式（第 11 节决定 2）：** `settings.Default()`（配置文件不存在时使用）在构建期缺省中心非空时把 `operator.mode` 设为 `"center"`，否则仍为 `"direct"`。社区构建的缺省中心为空，不受影响；`Load` 读入已有文件（含版本 1 升级）时不改模式，已有配置不迁移。
- 节点选择、登录状态等运行数据不进 settings，放在中心状态目录。

### 7.2 目录布局

中心相关的状态一律放在 `state/<role>/centers/<center_id>/` 下（SELFHOST §8 第 5 条）。切换中心不迁移、不删除任何目录；直连与自建 Hub 的 `trust.json` 不随中心切换变化，中心模式也不读写它。

```
state/operator/
  device.key / device.crt        设备身份（每个角色一份，同时是中心设备记录的主体）
  trust.json  issuer.key         直连 / 自建，中心模式不读写
  centers/<center_id>/
    login.json                   本地登录缓存
    pins/<session_id>.json       钉住记录
state/client/
  centers/<center_id>/
    pins/<session_id>.json
```

| 文件 | 内容 | 生命周期 |
|---|---|---|
| `login.json` | `{"version":1, "device_fp", "device_label", "account_id", "account_label", "expires_at", "logged_in_at"}` | 登录成功写入；`unauthenticated`、退出登录或吊销本机时删除。只是界面缓存，权威在中心：启动时用 `Account` 刷新，不一致以中心为准 |
| `pins/<sid>.json` | `{"version":1, "center_id", "session_id", "role", "peer_fp", "node": {"node_id", "addr", "port", "hub_fp"}, "paired_at"}`；文件名为 base64url 的 `session_id` | 两端各自在契约 §10.2 规定的时刻写入；会话结束、对账发现中心已无此会话时删除 |

- 写入使用临时文件 + `fsync` + rename + 父目录 `fsync`，权限 0600（目录 0700）。读取严格解码，字段与文件名不一致、`center_id` 与目录不一致都按损坏处理，损坏视为钉住记录丢失（第 5.6 节）。
- 设备身份不变：中心的设备指纹就是现有的角色身份，没有单独的中心设备密钥（SELFHOST §8 第 5 条「设备身份仍为每个角色一份」）。不同中心的信任隔离由「授权事实与设备记录在各自中心」和「本地状态按 `center_id` 分目录」保证。代价是同一台设备在多个中心登记时公钥相同，两个中心串通可以据此关联它；这与直连对端本来就能看到设备指纹的情形相当，本轮不另设按中心的设备密钥。
- 会话状态不持久化：除钉住记录外，会话的状态、码、预算、幂等键只在内存中，重启后从中心重新读取（第 5.6 节）。

新包签名草案：

```go
package centerstate

// Store owns state/<role>/centers/<center_id>/. It never touches other centers or trust.json.
func Open(roleDir, centerID string) (*Store, error)

type NodeRef struct { ID [16]byte; Addr string; Port uint16; HubFP identity.Fingerprint }
type Pin struct {
	CenterID string
	Session  [16]byte
	Role     string // settings.RoleOperator | settings.RoleClient
	Peer     identity.Fingerprint
	Node     NodeRef
	PairedAt time.Time
}
func (s *Store) SavePin(ctx context.Context, p Pin) error
func (s *Store) Pin(id [16]byte) (Pin, error)   // ErrNoPin; corrupt files return ErrCorrupt
func (s *Store) Pins() ([]Pin, error)
func (s *Store) DeletePin(ctx context.Context, id [16]byte) error

type Login struct { DeviceFP identity.Fingerprint; DeviceLabel, AccountID, AccountLabel string; ExpiresAt, LoggedInAt time.Time }
func (s *Store) SaveLogin(ctx context.Context, l Login) error
func (s *Store) Login() (Login, bool, error)
func (s *Store) ClearLogin(ctx context.Context) error
```

## 8. 测试计划

全部离线：中心由 `internal/testutil/fakecenter` 在 loopback TLS（`httptest.NewTLSServer`）上提供，Hub 用真实的 `hub.Server` 在 loopback 上运行，内层用真实 TLS 或 `net.Pipe`，时钟可注入。不访问公网，不依赖真车或外部服务。

### 8.1 假中心的范围

`fakecenter` 是测试辅助包，只被 `_test.go` 引用，不提供命令或可运行的服务端。它有自己的 JSON 线格式结构体（不复用 `internal/center` 的类型，避免两边同错），验签用已经过 V3 向量验证的 `center.VerifyDevice` / `VerifyNode`，签票用经过 V4 验证的 `ticket.SignCenter`。

| 实现（最小语义） | 不实现 |
|---|---|
| meta；登录与设备记录（含 `device_busy`、吊销）；节点列表；会话创建（码、nonce、`quota_exhausted` / `quota_releasing`）、读取与长轮询、领票（`exp` 夹取）、`/established`、`/end`；兑换（核销、`code_invalid`）、`client/sessions`；激活（代次、激活记录、`node_superseded`）、心跳（`seq`、续租 / 撤销、`closed`、用量取最大值合并、`carry_bound` / `old_gen_bound` 与三条释放条件的简化实现）；幂等记录（终局入表、暂态不入表、同键异体）；按数据库时钟的惰性迁移（码到期、配对期限、账号到期、节点离线） | 真实数据库、唯一约束与条件写的并发正确性；码与账号码的 HMAC / pepper 存储；精确的滚动限流（只做可配置的计数器）；管理后台 |
| 测试控制面：注入时钟、多个入口共享同一份状态（用于回退）、按端点注入一次性故障（返回指定错误码、提交后丢弃响应、延迟）、查询内部状态（会话状态、占位是否释放、用量合计） | — |

### 8.2 S 组（Hub 行为）

| 编号 | 包 | 方式 |
|---|---|---|
| S1 重放、`unknown-peer` 不消费 `jti` | `internal/hub` | 测试内的会话策略桩（直接给出带 `Session` / `JTI` 的 Grant）；真实 loopback Hub |
| S2 跨会话配对被拒 | `internal/hub` | 同上 |
| S3 撤销、封存、未用票据被拒、`closed` 带最终用量 | `internal/hub`（封存与关连接）；`internal/hubcenter`（假中心下发 `revoke`，下次心跳的 `closed`） | 真实 Hub + 假中心 |
| S4 撤销与准入并发；等锁期间票据过期 | `internal/hub`，`-race`，假时钟在锁内越过 `Expires` | 只出现「拒绝」或「登记后关闭」两种结果 |
| S5 承载截止、续租按发出时间、失联到 D、`node_disabled` / `node_superseded` 立即关闭 | `internal/hub`（D 与 `AfterFunc`）；`internal/hubcenter`（续租计算与处理表） | 假时钟；假中心注入失联与错误码 |
| S6 代次 | `internal/hubcenter` | 两个进程内节点用同一身份先后激活；旧代次票据被新策略拒绝；激活重试返回原代次、被取代后得到 `node_superseded` |
| S7 时钟偏差超限停止准入 | `internal/hubcenter` | 假中心的 `server_time` 偏移 31/32 秒；恢复后重新打开 |
| S8 正常结束、状态清空、无协程泄漏 | `internal/hub`、`internal/integration` | 结束后 `Usage()` 为空，`goleak` 式的协程计数检查（沿用现有测试做法） |

另：`v = 2` 自建票据被中心策略拒绝（已由 V10 覆盖，`hubcenter` 再加一条策略层用例）；处理表逐行的表驱动测试；配置严格解码与重载只允许改 `ticket_keys`；`renet-hub serve` 在 loopback 上对假中心完成激活，`node_superseded` 时退出码为 3。

### 8.3 P 组（配对）

| 编号 | 包 | 方式 |
|---|---|---|
| P1 | `internal/session`（闸门、`OpenTunnel` 被拒）；`internal/integration`（完整流程） | 内存传输；端到端用真实 Hub + 假中心 + 两端 app |
| P2、P3 | `internal/session`、`internal/pairing` | 内存传输，按消息注入篡改；`SessionResponder` 计数 |
| P4、P6 | `internal/centerflow` | 内存中继在 16 之前切断 / 换一台 Client 设备 |
| P5 | `internal/session`（16 的发送与原因）；`internal/centerflow`（`/end` 的原因写到假中心） | 钩子返回假、钩子超时（假时钟） |
| P7 | `internal/centerflow` | 进程内重启（重建 flow 与 store）、删除或损坏钉住记录、Operator 回 `not_paired` |
| P8 | `internal/pairing/cpace`（固定标量）；`internal/session`（内存中继） | 确定性 |
| P9 | `internal/session` | 能力矩阵与 Hello 篡改 |
| P10 | 不改，R2 现有测试 | 回归 |

### 8.4 C 组（中心语义）

C 组本质上是中心实现的测试。本仓库只验证 Go 端在这些场景下的行为，中心一侧的正确性由 cloud 仓库负责：

| 编号 | 本仓库验证的内容 | 包 | 不在本仓库 |
|---|---|---|---|
| C1 | Client 换键重兑得到 `code_invalid` 后，用 `ClientSessions` 找回并继续 | `centerflow` | 并发兑换只有一个成功（真实数据库） |
| C2 | — | — | 跨节点额度（中心） |
| C3 | Operator 收到 `device_busy` 时恢复或结束已有会话 | `centerflow` | 设备记录唯一性（中心） |
| C4 | 超时后同键重发；暂态结果后同键再执行；`Replayed` 透传 | `center` | 幂等表的原子性（中心） |
| C5 | R 类换入口并沿用同键、W 类与 B 类不换入口；假中心断言只创建了一个会话、只核销了一次码 | `center` | — |
| C6 | 只在需要码时带 nonce；`nonce_reused` 换新 nonce 重发 | `center` | nonce 去重存储（中心） |
| C7 | 硬截止时 Hub 关闭连接（假时钟）；`/established` 持续失败时不开放隧道，到期显示 `pair_timeout` | `integration` | 码到期迁移（中心） |
| C8 | 失联的 Hub 在最坏时钟偏差下按 `exp + skew` 与租约关闭，不越过硬截止 | `hubcenter` | 宽限期计算（中心） |
| C9 | Hub 侧：`revoke` 后报告 `closed`；新代次激活后旧会话不再被续租 | `hubcenter` | 三条释放条件与 `old_gen_bound`（中心） |
| C10 | `device_revoked` 结束原因的界面映射 | `centerflow`、`desktop` | 降额度、重置账号码、兑换上限（中心） |
| C11 | 两端对 `not_found` / `forbidden` 的处理 | `centerflow` | 主体校验（中心） |
| C12 | 心跳响应丢失后，下一次心跳重报累计值（不重置） | `hubcenter` | 取最大值合并（中心） |

### 8.5 其他

- `center.Client`：签名头、目标规范化、路径前缀；时间校正；重定向被拒；响应体超限；错误信封缺失按 R 类；`ErrUnreachable`；日志与错误文本不含码、票据。
- `centerstate`：原子写、权限、损坏判定、`center_id` 隔离（两个中心互不可见）。
- 秘密不外泄：注入含账号码、数字码、SAS、票据的流程，断言日志、Snapshot、State、诊断导出与自检报告中都不出现。
- 决定 1：Operator 的 SAS 对话在输入为空、不足 6 位、6 位但不一致时「确认」不可用，一致时可用；全角数字与空格规范化；「不一致」始终可用；输入内容不出现在日志与 State 中（`internal/ui` headless 测试）。desktop 没有绕过输入的确认动作（`internal/desktop`）。
- 决定 2：构建期缺省中心非空且没有配置文件时 `Default()` 的 `operator.mode` 为 `center`；缺省中心为空时为 `direct`；已有版本 1 / 版本 2 文件读入后模式不变（`internal/settings`，沿用现有注入 `defaultCenterJSON` 的测试方式）。
- 界面：两端主页、设置、SAS 对话在简繁中文与英文、100% / 150% 与最小尺寸下的 headless 渲染测试（沿用现有门禁）。
- 人工验收（不进入离线 CI，等中心服务端可用后执行）：官方或私有测试中心 + 家庭网络以外的节点，Operator 与 Client 都在 NAT 之后，完成首次配对、断线重连、两端重启恢复、撤销与到期；记录版本、拓扑与脱敏日志。

## 9. 契约差异（已写入契约第十版）

实现设计中发现以下需要澄清的地方，都不改变线格式与已有语义。经负责人同意，已于 2026-11-20 作为契约第十版写入 [CENTER-CONTRACT](CENTER-CONTRACT.md)（修订记录见其第 18 节）。下面保留原始条文；第 4、6 项按定稿调整后的措辞写入。

1. **§10.2「数据连接」：成功时 Operator 发什么。** 原文只写「继续正常 session，否则发 16 `not_paired` 并关闭」，没有说明 Client 怎样判断授权已通过。拟增：「数据连接上，Operator 通过检查时不发送任何配对消息，直接进入正常 session；Client 在 Hello 之后的控制流上任何时刻收到 16 `not_paired` 都按 `pairing_lost` 处理。数据连接上的 16 同样按 256 字节上限严格解码。Client 不以数据连接的建立作为授权依据，隧道的放行以看到 `active` 为准（§10.4 第 4 步）。」
2. **§7.8、§9.3：连接全部断开、尚未撤销的会话，其尾部用量如何上报。** `sessions` 只列有连接的会话（列入就会被续租，从而干扰 `idle_timeout`），所以从上一次心跳到连接断开之间的用量，只能在之后收到 `revoke` 时经 `closed` 报告。若中心在越过 `carry_bound` 后不经确认就释放占位、不再列出该会话，这段尾部用量会丢失。拟增：「Hub 应保留没有连接的会话的累计用量，至少到其承载截止之后 `lease_ttl`；中心不经 `closed` 释放占位时，未上报的尾部用量允许丢失。」
3. **§6.1、§11：Hub 是否调用 `GET /v1/meta`。** §11 要求每个入口核对 `center_id`，但 §6.1 的 `role` 一致性规则没有列出 `/v1/meta`，R3 的 `FetchMeta` 也只允许 `client` / `operator`。拟增：「`GET /v1/meta` 接受任意 `role`（`operator`、`client`、`hub`），不按角色返回不同内容；Hub 同样在使用入口前核对 `center_id`。」
4. **§6.4：Hub 的时间。** 拟增两句：「Hub 签名请求使用本地时钟，不按 `server_time` 校正时间戳（校正会掩盖偏差）。偏差按 `server_time − (发出时刻 + 往返 / 2)` 估计，考虑 `server_time` 的整秒截断，绝对值超过 `clock_skew + 1` 秒时判定超限。」原文的「偏差超过 `clock_skew`」没有给出估计方法。**定稿调整：** 把整秒截断与半个往返作为不确定度计入偏差一侧（估计值加不确定度超过 `clock_skew` 即超限），而不是放宽 1 秒；放宽会让 Hub 时钟最多慢 31 秒仍在准入，破坏 §9.2「最坏时钟偏差下也不越过硬截止」的推导。
5. **§5.2：Client 在会话结束 10 分钟后读取。** 得到 `not_found`，而 Client 可能仍持有钉住记录（例如关机期间会话结束）。拟增：「Client 持有钉住记录而读取会话得到 `not_found` 时，视为会话已结束（原因未知），删除钉住记录。」
6. **§10.4 恢复规则与 `open` 状态。** 字面上「完成配对之前重启」包括 `open`，但 `open` 时还没有任何 PAKE 尝试（领票要求 `authorized`），Operator 可以凭 nonce 重新取回码继续接待。本轮**按字面执行**（以 `pairing_lost` 结束）。**定稿调整：** 契约第十版只写明恢复规则同样适用于 `open` 状态，不加入放宽条文；以后若要允许 `open` 状态的 Operator 重启后经 §6.5 取回码继续等待，需另行修订。
7. **§8.5 Operator 的 `unauthorized`。** 原文「未结束则领新票据重试一次」，没有规定之后的行为。拟增：「重试仍被拒时，按退避继续查询会话状态，不连续领票；会话结束后按结束原因提示。」（避免消耗 `ticket_issue_budget`。）
8. **§7.8 激活键的保留期。** 第七版实现 35 只对中心实现提出要求。拟在 §7.8 对 Hub 补一句：「同一个激活幂等键的重试不得超过 `idempotency_ttl`；接近时换新键（本进程从未取得代次，换键无害）。」

## 10. 实现任务

每项都是一个可以独立提交的 commit，只改列出的包（及其测试、README 与 `go.mod`）。每个提交都必须单独通过 `gofmt -l .`（无输出）、`go build ./...`、`go vet ./...`，以及所改包的 `go test`。新增第三方依赖：无（HTTP 用标准库，JSON 沿用仓库已用的 `encoding/json/v2`）。

| # | 标题与包 | 对外接口草案 | 依赖 |
|---|---|---|---|
| T1 | `feat(hub): 中心会话的锁内提交、承载截止与撤销`，`internal/hub`（含 `PROTOCOL.md` 的「中心会话」一节） | 第 3.6 节：`SessionID`；`Grant.Session` / `JTI`；`Options.AfterFunc` / `SealTTL` / `SessionRetention`；`SetAdmitting`、`Renew`、`Revoke`、`CloseAll`、`Usage`、`Usage` 结构。自建 Grant 行为不变，现有测试不改 | 无 |
| T2 | `feat(center): 签名请求客户端、入口回退与错误分类`，`internal/center` | 第 4.1 节：`ClientOptions`、`NewClient`、`Call`、`Response`、`Do`、`Class`、`Error`、`ClassOf`、`ErrUnreachable`、`NewIdempotencyKey`；`FetchMeta` 接受 `hub` 角色 | 无 |
| T3 | `feat(session): 中心模式配对、SAS 拒绝与隧道闸门`，`internal/session`、`internal/pairing/cpace`、`internal/pairing` | 第 5.1 节：`CenterBinding`、`Options.Center` / `Pinned` / `HoldTunnels` / `ConfirmTimeout`、`ReleaseTunnels`、`ErrTunnelsHeld`；`cpace.ReasonRejected` / `ReasonConfirmTimeout` / `ReasonNotPaired`；`pairing.NewSessionResponder`、`AttemptsLeft`、`Close` | 无 |
| T4 | `feat(centerstate): 中心状态目录与钉住记录`，新包 `internal/centerstate` | 第 7.2 节：`Open`、`Pin`、`NodeRef`、`SavePin` / `Pin` / `Pins` / `DeletePin`、`Login`、`SaveLogin` / `Login` / `ClearLogin`、`ErrNoPin` / `ErrCorrupt` | 无 |
| T5 | `test(fakecenter): 只用于测试的内存假中心`，新包 `internal/testutil/fakecenter` | `New(t testing.TB, Options) *Center`；`(*Center).Entries() []string`（N 个共享状态的 TLS 入口）、`HTTPClient() *http.Client`、`TicketKey() ed25519.PublicKey`；开户、登记节点、注入故障、推进时钟、查询状态的控制方法。自测用 `center.SignDevice` 直接构造请求，不依赖 T2 | 无 |
| T6 | `feat(settings): Operator 中心模式`，`internal/settings` | `operator.mode` 接受 `"center"`；中心模式要求已配置中心，`listen` 可为空；构建期缺省中心非空时 `Default()` 缺省为中心模式；版本不变 | 无 |
| T7 | `feat(transport): 单次持票 wait 与 dial`，`internal/transport/hub` | `(*Connector).WaitTicket(ctx context.Context, ep transport.Endpoint, target identity.Fingerprint, token []byte) (transport.Session, error)`；`(*Connector).DialTicket(ctx context.Context, ep transport.Endpoint, peer identity.Fingerprint, token []byte) (transport.Session, error)`；单次尝试、不重试，返回 `router.ErrUnknownPeer` / `ErrBusy` / `ErrUnauthorized`；内层 Expect 为 target / peer | 无（测试用现有 `hub.Server` 与测试内的策略桩） |
| T8 | `feat(center): Operator、Client 与节点端点`，`internal/center` | 第 4.5 节的类型与方法 | T2；测试用 T5 |
| T9 | `feat(hubcenter): 节点配置与中心准入策略`，新包 `internal/hubcenter`，`internal/hubauth` 导出严格解码器 | `Config`、`Load(path) (Config, error)`、`(Config).ReloadCompatible(old Config) error`、`NewPolicy`、`SetGeneration`、`SetKeys`、`Kids`；`hubauth.DecodeStrict(r io.Reader, limit int, dst any) error` | T1 |
| T10 | `feat(hubcenter): 激活与心跳循环`，`internal/hubcenter` | `NewNode(NodeOptions{Config, Local, Server, Policy, API *center.Client, Version, Now, Logger}) *Node`；`(*Node).Run(ctx context.Context) error`（返回 `ErrSuperseded` 或终局错误）；`(*Node).Reload(cfg Config) error` | T8、T9；测试用 T5 |
| T11 | `feat(renet-hub): 中心模式`，`cmd/renet-hub` | 按 `mode` 分派；`check` / `serve` 的中心模式；退出码 2 / 3；`main.version`；SIGHUP 只重载 `ticket_keys` | T10 |
| T12 | `feat(centerflow): Operator 与 Client 的接待流程`，新包 `internal/centerflow` | `NewOperator(OperatorDeps) *Operator`：`Login`、`Logout`、`Account`、`Nodes`（含实测延迟）、`Start(ctx, node)`、`Run(ctx, base session.Options, serve func(context.Context, *session.Session) error) error`、`End`、`Snapshot() OperatorState`；`NewClient(ClientDeps) *Client`：`Redeem`、`Resume`、`Run(同上)`、`Retry`、`End`、`Snapshot() ClientState`；`Reason` 原因码；`SASPrompt{ID uint64; Code string; Peer identity.Fingerprint; Deadline time.Time}` 与 `Confirm func(context.Context, SASPrompt) (bool, error)` 依赖 | T3、T4、T7、T8；测试用 T1、T5 |
| T13 | `feat(app): 中心模式运行时`，`internal/app`、`cmd/renetd` | `OperatorConfig.Center *CenterConfig{Center settings.Center; Node string; AppVersion string}`；`ClientConfig.CenterCode string` 与 `ClientConfig.Center *settings.Center`（与 Invite / DirectPair / Peer 互斥）；`Runtime.AnswerSAS(ctx context.Context, id uint64, match bool) error`；`Snapshot.Center CenterSnapshot`（阶段、会话短 ID、码的显示、到期、待答 SAS、剩余尝试、结束原因）；presenter / forwarder 在 `ReleaseTunnels` 之后才启动；`renetd operator --center-node ID`（中心取自配置）与 `renetd client --center-code-file FILE` | T6、T12 |
| T14 | `feat(desktop): 中心模式控制器与原因映射`，`internal/desktop`、`internal/locales`、`internal/checkup` | `CenterLogin(ctx, accountCode, label string) error`、`CenterLogout(ctx) error`、`RefreshCenter(ctx) error`（账号与节点）、`RevokeCenterDevice(ctx, fp string) error`、`StartReception(ctx, nodeID string) error`、`AnswerSAS(ctx, id uint64, match bool) error`、`RetryPairing(ctx) error`；`State.Center CenterView`；第 6.5 节文案；删除 `locales.Text` 的 R5 前特例；自检对纯码只做 meta | T13 |
| T15 | `feat(ui): 连接方式三选一、登录、节点与 SAS 对话`，`internal/ui`、`internal/desktopui` | 第 6 节布局；`Actions` 增加对应动作；SAS 用 `ui.Modal`，Operator 侧输入 6 位一致后才能确认 | T14 |
| T16 | `test(integration): 中心模式端到端`，`internal/integration` | 真实 loopback Hub（`hubcenter.Node` 驱动）、假中心、两端 app：P1、P5、P7、C5、C7、S3/S8 端到端，以及「Operator 忙」「节点离线」「授权到期」「码无效」「网卡缺失」的原因显示 | T11、T13 |
| T17 | `docs: R5 契约同步`，`docs/CONTRACTS.md`（新增 R5 节）、`docs/DEPLOYMENT-MODES.md`、`docs/ROADMAP.md`、`docs/CONFORMANCE.md`、本文第 12 节 | 冻结签名；确认第 9 节已写入契约第十版，并核对条文与实现一致 | T1–T16 |

依赖顺序：

- **第一批（全部可并行）：** T1、T2、T3、T4、T5、T6、T7。其中 T1、T2、T3、T5 是工作量较大的四项，其余三项各自很小。
- **第二批：** T8（依赖 T2）、T9（依赖 T1）。
- **第三批：** T10（依赖 T8、T9）、T12（依赖 T3、T4、T7、T8），两者可并行。
- **第四批：** T11（依赖 T10）、T13（依赖 T6、T12），两者可并行。
- **之后依次：** T14、T15；T16 依赖 T11 与 T13，可与 T14、T15 并行；最后是 T17。

## 11. 决定（2026-11-20 负责人确认）

1. **Operator 确认 SAS 的方式：输入客户报出的 6 位，一致后才能确认。** Operator 的 SAS 对话提供 6 位数字输入框，「确认」只在输入与本机 SAS 一致时可用，「不一致」始终可用（第 6.3 节）。理由：契约第 13 节的风险目标（猜中数字码只造成一次接待重来）和发现中心或入口冒充，都以「Operator 只在客户本人报出 SAS、且与屏幕一致时才确认」为前提；只给两个按钮时，Operator 习惯性点「一致」会让这两个目标同时失效。Client 一侧仍为「一致 / 不一致」两个按钮：它的职责是把 SAS 读出去。
2. **官方构建全新安装的 Operator 缺省为中心模式。** 构建期缺省中心非空、且没有配置文件时，`operator.mode` 缺省为 `"center"`；社区构建（缺省中心为空）与已有配置不受影响，不做迁移（第 7.1 节）。理由：官方发行版的主要用法就是中心，缺省直连会让新用户先面对监听地址和端口转发。

设计时按推荐自行决定的事项：`node_superseded` 时退出（第 3.2 节）；`open` 状态重启按契约结束（第 9 节第 6 项）；退出登录即吊销本机设备记录（第 6.2 节）；设备名称缺省为空（第 6.2 节）；SAS 用窗口内对话框（第 6.3 节）。

## 12. 实现记录与审查处理

实现于 2026-11-20 完成，提交范围为 `e0c6cac..HEAD`。离线测试、端到端测试（假中心）与独立审查均已通过；中心服务端不在本仓库，与真实中心的联调和人工验收（第 8.5 节最后一项）尚未执行，因此本文状态为「已实现，人工验收待执行」。正文第 1–11 节保留设计稿原文，实现与之不同的地方列在 12.2；冻结的接口以 [CONTRACTS · R5](CONTRACTS.md#r5-各端接入中心2026-11-20-冻结) 为准。

### 12.1 任务与提交

| # | 提交 | 后续修正 |
|---|---|---|
| T1 hub 中心会话 | `cc4ac98` | `101ba8a`（保留期动态更新）、`81f2de5`（中心模式拒绝零会话授权）、`e669227`（`TestSetupSlots` 测试时序） |
| T2 center 签名客户端 | `168beb4` | `f2a6155`、`b4882f4` |
| T3 session 中心模式配对 | `68d645b` | `b730135`（`TestCenterFailedAttempts` 测试时序） |
| T4 centerstate | `166322b` | — |
| T5 fakecenter | `73ab184` | `60156ac` |
| T6 settings | `4dd3618` | — |
| T7 transport 持票 wait / dial | `41431ee` | — |
| T8 center 端点 | `4f58be3` | `b033599`、`71c3bb5` |
| T9 hubcenter 配置与策略 | `a805c56` | — |
| T10 hubcenter 激活与心跳 | `46c2879` | `bd9c590`（保留尾部用量）、`b033599`、`de4729b`、`60156ac` |
| T11 renet-hub 中心模式 | `506c02e` | `81f2de5` |
| T12 centerflow | `903fc37` | `8d559d8`、`ca2932f`、`f2a6155`、`71c3bb5`、`d5a7938`、`48a7303`、`b4882f4` |
| T13 app 与 renetd | `85e9378` | `c35cf9d`、`6ebf4ef`、`bfa5a4a` |
| T14 desktop | `0a29858` | `c35cf9d`、`48a7303`、`2d2fee3`、`bfa5a4a` |
| T15 ui | `1acc9d8` | `2dd6fd4` |
| T16 端到端测试 | `0cf813a` | — |
| T17 文档同步 | 本次文档改动 | — |

### 12.2 与设计稿不同的实现细节

| 位置 | 修订 |
|---|---|
| 3.4 / 3.6（T1、T10） | `Options` 新增 `CenterMode`：中心模式的 Hub 在锁内拒绝 `Session` 为零的 Grant，作为策略之外的第二道防线（`81f2de5`）。会话保留期为 `D + lease_ttl + clock_skew`（30 s），由 `hubcenter` 在每次激活与心跳成功后调用新增的 `Server.SetSessionRetention` 更新，已断连会话的清理时刻随之重排（`101ba8a`、`bd9c590`），不是启动时固定的 `Options.SessionRetention` |
| 3.3（T10） | `load` 的 `rx_bps` / `tx_bps` 改用新增的 `Server.TrafficTotals()` 两次采样之差：它包含已断连与已退役的会话，两次心跳之间开始并结束的会话也计入。`renew` 只作用于本次心跳上报过的会话，其余项忽略并记警告。`closed` 超过 1000 项时分批上报而不是截断：尚未上报的优先，其次是已上报但仍被列出的，最后是新封存的，每项保留到中心不再在 `revoke` 中列出（`de4729b`）。激活遇到 `clock_skew` 时固定 15 秒后同键重试 |
| 3.6（T9） | `Policy.Admit` 在 `ticket.VerifyCenter` 之后显式拒绝全零 `sid` 与 target 为 Hub 自身的 dial（即使票据 `peer` 与之相等），不依赖「票据不符」间接拒绝；已写入契约第十一版 §8.4 |
| 3.9（T11） | `check` 打印完整的 `node_id` 与 Hub 指纹；新增 `check --online`，用匿名 meta 核对各入口。`serve` 的其他错误退出码为 1（2 只用于中心模式的配置与激活终局错误） |
| 4.3（T2） | 长轮询的单次超时为 `10 s + wait`，整次预算为 `30 s + wait`；已向某个入口发出的心跳不重放、不换入口重发，只有发出前就失败的尝试才回退；没有错误信封（解析不出非空 `error.code`）的非 2xx 一律按 R 类处理。已写入契约第十一版 §6.6、§7.8、§11 |
| 4.1（T2、T8、T12） | 新增 `Client.Now()`（与设备签名同一校正时钟，`f2a6155`）、`ErrIDMismatch`（全部入口都报告别的 `center_id`，与网络不可达区分，`b4882f4`）、`ErrEndpointResponse` / `ErrProtocol`（成功响应格式无效）。心跳响应的 `renew` / `revoke` 可缺失或为 `null`（`b033599`） |
| 5.1（T3） | 新增 `Options.BeginClientAttempt`：Client 的会话预算由调用方在发 11 前扣减，返回 false 即已用尽。设置 `Options.Center` 时隧道闸门默认关闭，不必再同时设 `HoldTunnels`。数据连接的控制流用新增的 `proto.ReadCenterControl` 解码，配对帧仍受 256 字节上限 |
| 5.2 / 5.3（T12） | `Operator.Start` 的创建与首次领票限定在 1 分钟预算内，并可被取消或被退出登录打断（`ca2932f`）。票据临近到期按中心校正时钟判断（`f2a6155`）。兑换的响应丢失后换键重兑得到 `code_invalid` 时，只在 `ClientSessions` 恰有一个存活会话时自动找回，有多个时返回 `ChooseSessionError` 候选列表，由用户选择后 `Resume(id)`（`48a7303`，界面 `2dd6fd4`、`bfa5a4a`）。Operator 得到 `DigitalPaired` 后不立即关闭配对连接，等 Client 关闭（上限 `confirm_timeout + 10 s`），以免丢弃传输中的 16 |
| 4.6（T12） | 新增原因 `peer_version_incompatible` 与 `center_protocol_error`，区分对端版本不兼容与中心协议错误（`71c3bb5`）；`/end` 得到 `session_ended` 或 `not_found` 时视为已结束、正常收尾（`d5a7938`）；领票先观察到接待结束时同样正常收尾（`8d559d8`） |
| 6.3（T13、T14） | 比对不只在界面层：`Runtime.VerifySAS` 核对 Operator 输入（忽略空格、接受全角数字）并把结果绑定到当前提示，`AnswerSAS` 对未核对的「一致」返回 `ErrSASNotVerified` 并保持提示待答（`c35cf9d`）；desktop 的 `MatchSAS` 调用它。明文数字码与 SAS 只经 `Runtime.CenterDisplay` / `Controller.CenterDisplay` 交给本地控件，Snapshot、State、日志与诊断中都没有。Operator 对话框显示本机 SAS（`2d2fee3`、`2dd6fd4`），与 6.3 一致 |
| 6.2（T15） | 节点列表收在下拉选择框中（按实测延迟预选），不是单选表格 |
| 第 10 节 T13 | `renetd operator --center-node` 要求同一状态目录已经在 GUI 中登录；命令行核对 SAS 时 Operator 显示来访设备与本机校验码，输入客户读出的 6 位，不一致时提示后可重输（`6ebf4ef`）。新增 `app.NewWithCenterHTTP` 与 `ClientConfig.CenterResume` |

### 12.3 独立审查与处理

实现期间做了独立只读审查（DeepSeek）。每条意见的结论如下，「修复」均带回归测试。

| 意见 | 处理 |
|---|---|
| Hub 只靠中心策略拒绝零 Session，一旦策略出错，中心会话会落入自建路由语义 | 修复（`81f2de5`）：`Options.CenterMode`，锁内拒绝零 Session 并记错误日志；`renet-hub` 中心模式开启。测试 `TestZeroSessionGrantByMode` |
| 心跳响应的 `renew` / `revoke` 缺失或为 `null` 时整条响应被判无效；决策应逐项处理，响应格式错误应可观察 | 修复（`b033599`）：两个列表缺失或 `null` 按空处理，逐项续租与撤销，未知会话的续租忽略并记警告；无效响应计数并记错误日志，按暂态处理。测试 `TestHeartbeatDecisionLists`、`TestPartialHeartbeatResponse` |
| `closed` 超过 1000 项时截断会丢失最终用量 | 修复（`de4729b`）：分批轮转上报并保留最终用量，见 12.2。测试 `TestClosedBatchesRetainFinalUsage` |
| 假中心不检查节点请求必需字段是否存在（缺字段被当作 0），`load` 与 `sessions` 截断后不一致 | 修复（`60156ac`）：假中心按字段存在性核验激活与心跳请求；`sessions` 截断后再汇总 `load`。测试 `TestNodeRequestPresence` |
| Operator 的 SAS 核对只在界面层强制，绕过界面即可确认 | 修复（`c35cf9d`）：强制下沉到 app 层 `VerifySAS` / `AnswerSAS`，见 12.2；已写入契约第十一版 §10.2。测试 `TestCenterReception`、`TestUnverifiedOperatorTimesOut`（`2d2fee3`） |
| 命令行 Operator 看不到来访设备与本机校验码，输入不一致没有提示 | 修复（`6ebf4ef`）：显示来访设备短 ID 与本机码，不一致提示后可重输。测试 `TestCenterAnswer` |
| `Start` 在中心不可达时可能无限等待，且无法取消 | 修复（`ca2932f`）：创建与首次领票限定 1 分钟预算，可取消，退出登录可打断。测试 `TestStartRetryCanEndOrLogout`、`TestFirstTicketBudget` |
| 票据临近到期按本地时钟判断，与中心时间有偏差时会误判 | 修复（`f2a6155`）：按 `Client.Now()` 的校正时钟判断 |
| 原因映射不全；协议错误与对端版本不兼容混为一谈 | 修复（`71c3bb5`）：补齐原因与三语文案，新增 `peer_version_incompatible`、`center_protocol_error`。测试 `TestProtocolReasons`、`TestCenterMessages` |
| `/end` 遇到中心已结束或已移除的会话时报错 | 修复（`d5a7938`）：`session_ended` / `not_found` 视为成功并清理钉住记录。测试 `TestEndAlreadyTerminal` |
| 兑换找回在存在多个存活会话时任选其一 | 修复（`48a7303`，界面 `2dd6fd4`、`bfa5a4a`）：只在唯一候选时自动找回，否则返回候选列表由用户选择。测试 `TestRecoveryOffersMultipleSessions`、`TestClientSessionRecovery` |
| 全部入口 `center_id` 不符时报告为「无法连接中心」 | 修复（`b4882f4`）：新增 `ErrIDMismatch`，界面显示「中心标识不一致」。测试 `TestClientIDMismatch`、`TestCenterIdentityMismatch` |
| `TestLostDecision` 偶发失败 | 根因修复（`8d559d8`）：Operator 结束接待后，Client 的领票请求可能比长轮询先观察到会话已结束（HTTP 410 `session_ended`），原实现把它当作失败。修复后两条路径都正常清理并保留中心给出的结束原因；新增测试固定该顺序，修复前稳定失败，修复后 `-race -count=200` 通过 |
| `TestCenterFailedAttempts` 偶发失败 | 测试时序修正（`b730135`）：Client 在 Hello 后放弃时 Operator 的失败位置取决于时序，改为断言 Operator 失败且不消耗尝试次数；`-count=500`、`-race -count=100` 通过 |
| `TestSetupSlots` 偶发失败 | 测试时序修正（`e669227`）：wait 收到 `ok` 时唯一的 setup 槽可能尚未释放，dial 对传输层失败短暂重试；`-race -count=200` 通过 |

### 12.4 端到端测试证据

`internal/integration/center_test.go` 的每个用例都起真实的 loopback Hub（由 `hubcenter.Node` 激活与心跳驱动）、两个 TLS 入口的假中心，以及 Operator 与 Client 两个 `app.Runtime`：

- `TestCenterReceptionLifecycle`：P1 完整接待；`/v1/sessions` 与 `/v1/redeem` 首个入口的响应被丢弃时换入口沿用同键，假中心只创建一个会话、只核销一次码（C5）；两端钉住记录、`/established` 恰好提交一次；日志、Snapshot 与诊断导出中不出现账号码、数字码、SAS 与票据；两端重启后凭钉住记录恢复数据连接；Operator 结束后两端以 `operator_ended` 收尾、钉住记录删除、端口释放、Hub 无残留路由、无协程泄漏（S8）。
- `TestCenterPairingInterrupted`：`reject`（Operator 判 SAS 不一致，两端与中心均为 `rejected`，P5）与 `restart`（Client 在写钉住记录前重启，以 `pairing_lost` 结束，P7）。
- `TestCenterSASTimeout`：无人核对时以 `confirm_timeout` 结束。用例需三分钟，只在设置 `RENET_LONG_CENTER_TEST=1` 时运行。
- `TestCenterRevocation`：管理员撤销后 Hub 关闭路由，两端以 `admin_revoked` 结束，中心收到的最终用量与 `TrafficTotals` 一致且占位释放（S3）。
- `TestCenterReasonDisplay`：「Operator 忙」`busy`、「节点离线」`node_lost`、「网卡缺失」`nic_missing`（且未兑换码）、「码无效」`code_invalid`、「授权到期」`account_expired`、「节点不可用」`node_unavailable` 的原因显示。
- `TestCenterDeadlines`：`code_expired`，以及 `/established` 持续失败时不开放隧道、到期以 `pair_timeout` 结束（C7）。

### 12.5 未验证与遗留

- 中心服务端在私有仓库另行实现；与真实中心服务端的联调、官方构建验收和第 8.5 节最后一项的人工验收尚未执行，结果记录后在此补充。
- 假中心只实现驱动 S/P/C 场景的最小语义，不证明任何中心实现合规；C1、C2、C9、C10 的并发写入须由中心实现对真实数据库验证（第 1 节非目标）。
