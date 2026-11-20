# Hub 外层协议 v1

Hub 只根据本文件的路由头建立路径。路由完成后的字节属于端到端加密层，Hub 不解析、记录或识别其内容。`hub` 和 `transport/hub` 均不依赖 `secure`。

## 外层 TLS

- TCP 上运行 TLS 1.3，ALPN 必须为 `renet-hub/1`，不接受缺省 ALPN 或 TLS 1.2。
- 双方各出示一张 Ed25519 自签设备证书。检查有效期、Subject/Issuer 一致、自签名和未知关键扩展；不使用公网 CA、DNS 名或客户端自报 ID。
- 设备 ID 是证书 Ed25519 公钥的 SHA-256，由 `identity.FingerprintOfCert` 计算。普通 Connect 必须钉扎 `Endpoint.Hub`，拒绝零指纹。ConnectBootstrap 从通过上述校验的证书取得候选指纹 H，在同一条连接上发送 target=H、token 长度为零的 dial 请求。直连配对时内层 TLS 须钉扎 H；独立 Hub 的 H 只标识中转方，Operator 的身份确认由端点完成。
- Hub 从 TLS 客户端证书取得设备 ID。路由头不含发送方 ID，无法通过修改头部冒充其他 wait 方。
- TCP 建立、TLS 握手、头部和状态交换共用一个外层 setup 窗口（默认 10 秒）；调用方 context 可更早取消。成功后清除 setup deadline，context 的后续生命周期由调用方或 transport 管理。

所有 listener 在创建 TLS worker 前非阻塞检查并发和速率，超限直接关闭 TCP。内嵌 Hub 默认同时允许 16 个 setup，每秒接纳 8 个、突发 16 个；独立 Hub 分别为 64、32 和 64。可通过 SetupLimits 配置并发、速率、突发和外层 setup 时限；路由完成或 setup 失败即释放并发槽。

## 请求与响应

TLS 握手后，客户端只发一个请求，整数采用网络字节序：

| 偏移 | 长度 | 内容 |
| --- | --- | --- |
| 0 | 1 | role：`1=dial`，`2=wait` |
| 1 | 32 | target：原始 Fingerprint 字节 |
| 33 | 2 | token 长度 N，uint16，最大 1024 |
| 35 | N | 可选鉴权 token，不是端到端配对 token |

`dial.target` 必须非零。`wait.target` 为零表示允许任意已授权 dial 方；非零表示仅允许该设备指纹配对。未知 role、零 dial target、过长 token 或截断请求直接关闭，不发送成功响应。

响应固定一字节：

| 值 | 状态 | 行为 |
| --- | --- | --- |
| 0 | ok | dial 已占有配对或内嵌连接；wait 已完成注册 |
| 1 | busy | 目标已配对、重复 wait，或内嵌端已有连接 |
| 2 | unknown-peer | 目标没有已注册的 wait 方 |
| 3 | unauthorized | 身份/角色/目标/token 策略拒绝，或不符合 wait.target |

非 ok 响应后关闭连接。`errors.Is(err, hub.ErrPeerBusy)` 可识别忙碌；transport 同名错误是其别名。其他两类业务拒绝也有 sentinel error。

## 独立 Hub 配对

通过准入的 wait 以外层设备证书指纹为键登记，同一设备重复登记返回 `busy`。wait 收到 `ok` 后可以长期空闲，配对前发送应用字节会被关闭。普通 dial 按 target 查找 wait：没有登记返回 `unknown-peer`，已被占用返回 `busy`，不符合 wait 指定的对端指纹返回 `unauthorized`。拒绝均保留原登记与正在进行的会话。

配对成功时 Hub 在锁内占有 wait，并登记 dial 方的 Grant。解锁后才发送状态字节；任一侧写 `ok` 失败会清理登记并关闭相关连接。wait 的 `ok` 只表示注册成功，dial 的 `ok` 表示已配对。dial 收到配对成功状态后，须在一个新的 SetupLimits.Timeout 窗口（默认 10 秒）内发出至少一个字节，否则 Hub 关闭两端并释放 wait。Hub 一读到首字节即清除读期限，此后双方可以长期空闲。转发保留全部原始字节，内层握手负责验证实际对端。任一方向 EOF、出错或服务退出都会关闭两侧并回收转发协程；下次会话需重新注册 wait，自建模式可以再次使用同一张有效的设备票。

## Embedded 交接

启用 Embedded 时，target 为本 Hub 指纹的 dial 直接占有本地交接槽。无策略时只接受这种自拨，忽略其 token；其余 wait 和 dial 一律返回 `unauthorized`。显式配置策略时，自拨同样经过准入复查。

交接槽一次容纳一条连接，已占用返回 `busy`。`Embedded.Accept` 返回经过外层认证的设备指纹和已剥离路由头的连接。未领取的交接在 setup 时限后释放；过期连接的迟到发布无法覆盖新占位。领取方负责关闭连接，服务退出也会关闭所有交接连接。

## 配对入口

独立 Hub 收到 target 为本 Hub 指纹的 dial 时，token 必须为空，外层设备须通过策略检查。Hub 在路由锁内读取当前策略指定的 `pairing_operator`。未配置时返回 `unauthorized`；已配置但没有对应 wait 时返回 `unknown-peer`；wait 已配对时返回 `busy`。空闲 wait 的 Grant 必须带开放窗口，且此刻不能晚于 `pair_until + skew`，否则返回 `unauthorized`。通过后按普通配对转发。窗口只在配对入口拨入时检查。

## 准入策略

独立 Hub 启动必须配置 Policy。Hub 将外层设备指纹、路由请求和当前时间交给策略快照的 `Admit`，在路由锁外完成验票。`Grant.Ref` 由策略解释；`Expires` 是含时钟偏差的最后准入时刻，零值表示无票据期限；`PairingUntil` 是含偏差的窗口截止时刻，零值表示窗口关闭。两项截止时刻均包含边界。

Hub 在同一把路由锁内用**当前策略**的 `Valid` 复核撤销条件，重读时间检查本次请求的准入期限，再登记 wait 或配对。路由表同时保存双方的 Grant，作为撤销索引。`SetPolicy` 在锁内替换策略，关闭失效的空闲 wait，以及任一方失效的整条转发。自建模式重载只检查撤销条件，设备票到期不会顺带断开空闲 wait 或已建立的转发。

## 中心会话

非零 `Grant.Session` 将连接绑定到中心会话。中心策略在锁外完成契约 §8.4 第 1–9 步，提供 `Session`、单次使用的 `JTI` 和含偏差的 `Expires`。全零 SessionID 是自建模式的哨兵值，中心策略须在构造 Grant 前拒绝全零 sid（契约 §8.4）。`Options.CenterMode` 为真时（`renet-hub` 的中心模式），Hub 在路由锁内另行拒绝任何 `Session` 为零的 Grant，返回 `unauthorized` 并记错误日志，作为策略之外的第二道防线；自建模式不设此项，行为不变。中心模式使用独立 Hub，wait 的 target 必须是 Client 指纹；target 为本 Hub 指纹的 dial 没有配对入口，中心策略在锁外拒绝，带中心会话的自拨在锁内同样返回 `unauthorized`。激活前调用 `SetAdmitting(false)`，激活后按节点状态打开闸门。

在路由锁内，Hub 依次完成以下操作：

1. 用当前策略复查，重读时钟。票据已过期、准入关闭、会话已封存或 JTI 已消费时，返回 `unauthorized`。
2. 检查 wait 登记与 dial 配对条件。没有目标 wait 返回 `unknown-peer`，重复 wait 或已配对返回 `busy`，空闲 wait 与 dial 的会话 ID 不同返回 `unauthorized`。这些拒绝都保留票据的重试资格。
3. 消费 JTI 并保留至 `Expires`（含边界），把路由加入会话撤销索引，将承载截止 D 延长至原 D 与本票截止中的较晚者。解锁后发送状态；写失败会关闭路由，JTI 仍已消费。

`Renew` 直接用心跳发出时刻加租期替换 D，可以缩短租约。到 D 关闭该会话的全部 wait 和 dial。`CloseAll` 关闭全部中心路由并保留尾部用量。两者都允许持新票重连。节点停用时先关闭准入，再调用 `CloseAll`。`Revoke` 在同一把锁内关闭整组连接并封存会话，返回最终累计用量；未知会话也封存，用量为零。封存期为 `SealTTL`（默认 330 秒），期内重复撤销返回相同用量并刷新封存期限。

`Usage` 仅列出有连接的会话。rx 为从两端读到的内层字节总数，tx 为写往两端的内层字节总数，均排除路由头和状态字节；配对时长累计会话至少有一对连接的时间。最后一个连接关闭后，累计值保留至撤销或 D 加 `SessionRetention`（默认 330 秒），供后续撤销报告尾部用量。`SetSessionRetention` 在运行中替换保留期，并按新值重排已断连会话的清理时刻；中心模式由 `hubcenter` 在每次激活与心跳成功后设为 `lease_ttl + clock_skew`，保留期随中心下发的租期动态更新。心跳调用方保存并上报 `closed`，通过 `TrafficTotals()` 的两次采样差计算节点吞吐率，即使会话在两次心跳之间开始并结束，也能计入。

## transport 生命周期与接线

`hubtransport.Register(wrap)` 在应用启动时注册 `"hub"`，只调用一次；显式注入解决基础层 `Config.Options` 只能传字符串、不能传函数的问题。注册表工厂接受 `listen`，较复杂配置直接调用 `New(Options)`。

- `Options.Endpoints` 是 Acceptor 的独立 Hub wait 候选，在 New 时拷贝。每次注册前调用 `Options.WaitToken(ctx, hub)` 现签 token（nil 时发送空 token），Connector 对每个候选使用该端点自己的 `Endpoint.Ticket` 作为 dial token。`Options.OnRegister(hub, token, err)` 在每次注册尝试结束后异步调用，不持有连接器的锁，调用之间不保证顺序，Close 之后也可能到达，因此必须快速返回。`Refresh` 作废尚未收到任何字节的空闲注册，让调用方换一张票据重新注册。`DialBootstrap` 返回外层候选指纹 H，内层 Expect 为零，由调用方按内层实际对端是否等于 H 决定配对能力。
- `Options.Listen` 创建并拥有内嵌 Hub listener；`Options.Embedded` 可从外部服务器接收连接。两者同时提供时使用同一个 Embedded。提供 Embedded 时 Acceptor 使用它，不同时注册 Endpoints。
- Connector 并发尝试 PeerInfo 的所有 hub 候选；完整 Wrap 首个成功者获胜。返回前取消并回收其他候选，关闭其连接和已生成的 Session。全部失败时用 `errors.Join` 保留可识别的底层原因。
- 内嵌路径以 `Endpoint.Hub == PeerInfo.ID` 识别，Path 为 direct；其他路径为 relay 且 Via 为 Hub 地址。外层钉扎与内层期望身份分别校验，不能用路径分类代替认证。
- Dial 与 Embedded.Accept 的内层 setup 最多 10 秒，使用 socket deadline 加计时取消。独立 wait 空闲时不启用内层计时；首个读到或写出的字节启动 10 秒窗口。成功后停止计时、清除 deadline，传给 Wrap 的长寿命 context 保留到 Session 关闭或调用方取消。
- Wrap 必须遵守 context、进行内层身份校验，并负责 Session 结束时关闭连接。非零 expect 必须严格校验，独立 wait 的 expect 为零，由内层返回实际认证身份。忽略 context 且不进行 socket I/O 的 Wrap 无法被本包强制终止。
- Acceptor 的 Accept 调用串行化；活动 Session 结束前不会开始下一次接收。Close 取消待处理 Accept、内层 setup、已接受 Session 和自有 listener。多个独立 Hub 之间没有全局账号锁，单会话边界是本 Acceptor/每个 Hub wait 注册。

## 本轮边界

无自动重连、退避、服务发现、全局 Hub 配额、流量限速、证书吊销、token 获取或公网互通测试。static 目录仅保存内存快照，本轮未增加可选文件持久化。Hub 只验证传输身份，首次配对授权由上层控制协议处理。
