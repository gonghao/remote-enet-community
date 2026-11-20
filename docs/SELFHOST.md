# R3 · 自建 Hub 与中心地址配置

状态：已实现（2026-11-20），人工台架验收待执行。设计定稿于 2026-11-20（提案 2026-11-19，第 13 节的两项已由负责人确认）；实现中发现并修正的细节已就地写入各节，实现记录与审查处理见[第 14 节](#14-实现记录与审查处理)。目标见 [ROADMAP · R3](ROADMAP.md#r3--自建-hub-与中心地址配置)，部署边界见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md)，票据格式与 Hub 校验沿用 [CENTER-CONTRACT §8](CENTER-CONTRACT.md#8-票据)，配对消息与状态机沿用 [CENTER-CONTRACT §10](CENTER-CONTRACT.md#10-中心模式的首次配对) 与 [PAIRING](PAIRING.md)。本文定下契约 §17 留给 R3 的部分：自建模式下票据的字段、期限与撤销语义，独立 Hub 的命令与配置，签发方式，两端的连接流程，以及中心地址配置在 R5 之前做到哪一步。

一句话概括：自建票据使用 `v = 2`，字段集和准入规则都与中心票据分开，中心模式的检查一条不动。每台 Operator 持有一把只能为自己签票的签发密钥，Hub 配置列出这些公钥。Client 首次配对时，用邀请码里的短期不记名票据，或在 Operator 发码期间经 Hub 的配对入口，连到 Operator；配对成功后，Operator 在端到端会话里给 Client 签发绑定其设备指纹的长期票据，之后重连都用这张票据。票据只约束准入。撤销靠编辑 Hub 配置并重载，重载时立即关闭不再有效的连接。

## 1. 范围与非目标

| 范围 | 内容 |
|---|---|
| 独立 Hub | 新命令 `renet-hub`，基于 `internal/hub`；两端都出站连接；配置缺少必需项或没有任何签发者时拒绝启动 |
| 自建票据 | `v = 2` 的 COSE_Sign1 票据，编码与签名结构同契约 §8.1；三种用途：Operator 的 wait 票、Client 的设备 dial 票、邀请码携带的不记名 dial 票 |
| 签发 | Operator 状态目录里的签发密钥；`renetd issuer` 输出 Hub 配置条目；`renetd invite --hub` 离线生成带票据的邀请码；运行中的 Operator 自动签 wait 票，并给 Client 签发和续期设备票 |
| 首次配对 | 原始邀请码（新版本 2，携带不记名票据）与「Hub 地址 + 8 位码」（经 Hub 配对入口，新配对能力 `pair-hub/1`）两种入口 |
| 中心地址 | 配置字段、校验、显式切换、界面显示，以及调用无需签名的 `GET /v1/meta` 核对 `center_id`；规定中心状态的存放位置 |
| 加固 | 内嵌 Hub 不再替陌生设备做中继（见 2.3） |

非目标：

- 一台 Operator 同时直连监听并在 Hub 上等待，或同时挂在多个 Hub 上。Operator 二选一。
- Hub 集群、计量上报、配额、限速、租约与心跳。这些只属于中心模式（R5）。
- 签发私钥完全不放在 Operator 机器上的手工模式，原因见第 5 节和第 13 节第 2 项。
- 不重载就生效的撤销、撤销列表的分发、Windows 上的热重载（Windows 上重启 Hub）。
- 中心登录、领票、中心模式的数字码与钉住记录（R5）。
- Operator 从直连切换到 Hub 后自动迁移已配对的 Client。已配对的 Client 需要用新邀请码或数字码重新配对一次；Operator 的 `trusted_peers` 保留，原始邀请走已信任快捷路径，不消耗令牌。
- 车端 `carside` 不改；不涉及任何车辆编码或刷写逻辑。

## 2. 拓扑与威胁模型

### 2.1 拓扑

```
Client ──出站 TLS（dial，target=Operator 指纹，token=票据）──▶ Hub ◀──出站 TLS（wait，target=零，token=wait 票）── Operator
   └──────────────── 内层 TLS 1.3（renet/1，设备身份互钉）+ session，Hub 只搬密文 ────────────────┘
```

外层仍是 [Hub 外层协议 v1](../internal/hub/PROTOCOL.md)，路由头格式不变。Hub 从外层 TLS 客户端证书得到设备指纹，从 token 得到票据，两者都只是外层元数据；内层字节 Hub 不解析、不记录。

### 2.2 相对直连模式的变化

| 方面 | 直连（R2） | 自建 Hub（R3） |
|---|---|---|
| Operator 暴露面 | 公网可达的监听端口；任何人都能发起外层 TLS 并进入内层握手 | 不需要入站端口。陌生设备只在 Operator 开放配对窗口期间、经指定入口才能到达它（第 6.3 节），其余时间必须持有指向它的有效票据 |
| 地址隐私 | Client 知道 Operator 的网络地址 | Client 只知道 Hub 地址 |
| 新增的受信方 | 无 | Hub 运营者（通常就是自建者本人）。它看得到两端 IP、设备指纹、票据内容、时间与流量；可以阻断、拖慢、拒绝服务；**读不到内容，也无法冒充任何一端**（内层 TLS 钉住设备身份，首次配对由 PAKE 或一次性令牌认证） |
| 新增的密钥 | 无 | 每台 Operator 一把签发密钥。泄露后，攻击者只能为这台 Operator 的会话签票（作用域见 3.4），也就只能占用它在 Hub 上唯一的配对槽，或拿它的配对槽当一条中继。而能拿到这把密钥的人，通常也拿到了更要紧的 Operator 设备私钥 |
| 新增的攻击 | — | ① 伪造 Hub：外层指纹钉扎（已保存或邀请码里写明），首次数字配对把 Hub 指纹写进 PAKE 的 CI（7.2）；② 把 Hub 当免费中继：只有配置里列出的 Operator 能登记 wait，任何一条中继都必须有一端是这些 Operator；③ 占住 Operator 的 wait 造成忙：需要持有指向它的票据，或者在配对窗口内；④ 时钟错乱：票据时间按 Hub 时钟校验，容差可配 |
| 少掉的风险 | — | 互联网扫描无法直接触达 Operator；Operator 不暴露内嵌 Hub 的未认证握手面 |
| 不变 | 内层 TLS、设备指纹钉扎、trusted_peers、原始邀请令牌一次性消费、数字码预算（5 次/码、15 分钟 20 次）、一个 Operator 同时只服务一个 Client | 同左 |

### 2.3 现状缺口

审读代码发现以下问题，R3 一并处理：

1. **内嵌 Hub 是开放中继。** `internal/hub/server.go` 的 `handle` 只把 `dial.target == 本机指纹` 交给 Embedded；其他 wait 和 dial 照常进入中继表，并且 `Authorize` / `AuthorizeToken` 在生产代码里都是 nil。任何人都可以拿 Operator 的直连入口，在两台自己的设备之间转发流量，占用的是 Operator 所在网络的带宽。commit 2934906 已在 `transport/hub` 先行修复（内嵌 listener 只准入指向本机的 dial）；T2 把这条规则下沉到 `internal/hub`。R3 规定：没有配置准入策略时，Hub 只接受内嵌自拨，其余请求一律回 `unauthorized`；独立部署（没有 Embedded）的 Hub 没有策略就拒绝启动（4.4）。
2. **`AuthorizeToken` 只返回布尔值，并且在路由锁外执行**，无法支持撤销索引，也无法满足契约 §8.4 的锁内提交。R3 把它换成 `Policy` / `Grant`（4.4），这也是契约 §16 为中心模式预留的拆分。
3. **`transport/hub.Options.Token` 是所有端点共用的一份静态凭据。** 自建模式需要每个端点一张票据，wait 每次注册都要一张新票。
4. **`DialDirectPair` 用外层指纹 H 作为内层的 Expect**，因此只能连到内嵌 Hub。经独立 Hub 时，内层对端是 Operator，指纹与 H 不同。
5. **`trust.json` 与 `settings.json` 都是严格 schema**，拒绝未知字段，并要求字段齐全。新增的字段要升版本，旧文件按旧版本读入（7.4、8）。

## 3. 自建票据（v = 2）

### 3.1 为什么升 v

契约 §8.2 规定：遇到不认识的约束键必须拒绝，改变准入语义只能升 `v`。自建模式改变了准入语义：票据可以重复使用、wait 的 target 为零、存在不记名票据、增加了签发者作用域。如果仍用 `v = 1`，再靠 Hub 配置区分模式，同一张票据在不同 Hub 上的含义就会不同，签发工具的一个缺陷（例如漏写某个字段）也可能被另一种模式误读。因此：

- 自建票据固定 `v = 2`，字段集按 3.2 表另行规定。
- 中心模式的 Hub 继续按契约 §8.4 第 5 步要求 `v == 1`，自建票据不需要任何新规则就会被拒；自建 Hub 只接受 `v == 2`。结构上不存在放宽中心检查的路径。
- Hub 配置文件带必需字段 `mode`，R3 只接受 `"selfhost"`。一个 Hub 进程只运行一种模式，签发者公钥也只在本模式的配置里出现。

### 3.2 字段

编码同契约 §8.1：带 tag 18 的 COSE_Sign1，EdDSA，protected 恰为 `{1: -8, 4: kid}`，unprotected 为空 map，payload 是确定性编码的 CBOR map（整数键）。`kid` = SHA-256(签发公钥) 的前 8 字节。键 1–63 是约束字段，验证方遇到不认识的必须拒绝；键 ≥ 64 是信息字段，验证方忽略。

| 键 | 字段 | 类型 | 设备 dial 票 | 不记名 dial 票 | wait 票 |
|---|---|---|---|---|---|
| 1 | `v` | uint | 2 | 2 | 2 |
| 2 | `center` | — | 禁止 | 禁止 | 禁止 |
| 3 | `acct` | — | 禁止 | 禁止 | 禁止 |
| 4 | `dev` | bstr 32 | 必需，Client 设备指纹 | **禁止** | 必需，Operator 设备指纹 |
| 5 | `role` | uint | 1 | 1 | 2 |
| 6 | `peer` | bstr 32 | 必需，Operator 指纹 | 必需，Operator 指纹 | **禁止**（wait 的 target 必须为零） |
| 7 | `sid` | — | 禁止 | 禁止 | 禁止 |
| 8 | `node` | — | 禁止 | 禁止 | 禁止 |
| 9 | `hub` | bstr 32 | 必需，Hub 身份指纹 | 必需 | 必需 |
| 10–12 | `iat` `nbf` `exp` | uint ≤ 2³²−1 | 必需 | 必需 | 必需 |
| 13 | `jti` | bstr 16 | 必需 | 必需 | 必需 |
| 14 | `gen` | — | 禁止 | 禁止 | 禁止 |
| 15 | `pair_until` | uint ≤ 2³²−1 | 禁止 | 禁止 | 可选：开放配对入口的截止时间（6.3） |
| 16 | `bearer` | 简单值 `true` | 禁止 | **必需且为 `true`** | 禁止 |

- 键 2、3、7、8、14 沿用契约的编号，自建票据里必须缺席，不用零值或空串表示「不适用」。
- 不记名票据要求 `dev` 缺席且 `bearer` 为 `true`，两个条件同时成立才算不记名。这样，签发端漏写 `dev` 的缺陷只会产生被拒的票据，不会产生不记名票据。`bearer: false` 也拒绝。
- `jti` 是 16 字节 CSPRNG 随机数，**不做单次消费**，只用作撤销句柄和日志关联（3.6）。
- 时间取 Unix 秒。签发时 `iat = nbf = 签发时刻`。

尺寸：设备 dial 票 payload 146 字节，整张票据 231 字节；不记名票 198 字节；带 `pair_until` 的 wait 票 202 字节，都远低于路由头 1024 字节的上限。实现须有测试断言签发的票据不超过 256 字节。

### 3.3 三种票据与签发方

| 票据 | 签发方与时机 | 有效期 | 使用方式 |
|---|---|---|---|
| wait 票 | 运行中的 Operator，每次向 Hub 注册 wait 前现签 | 10 分钟；有数字码时 `pair_until` = 码的截止时间 | 只用一次即丢弃 |
| 设备 dial 票 | 运行中的 Operator，在每次成功的会话握手末尾签给对端 Client（6.4） | 30 天 | Client 保存，作为该 Hub 端点的 token 反复使用；每次连上都换一张新票，所以 30 天内连过一次就会续期 |
| 不记名 dial 票 | `renetd invite --hub` 或 GUI 生成邀请码时，由 Operator 状态目录里的签发密钥离线签出，写进邀请码 | 等于邀请码有效期，Hub 限制为至多 `bootstrap_max_ttl_s` | 首次配对用；可以重复使用到过期，真正的门槛是同一邀请码里的一次性配对令牌 |

### 3.4 Hub 校验步骤

先在路由锁外完成 A 组检查，任一失败都回 `unauthorized`（状态 3）并关闭连接；原因只写调试日志，日志里只出现 `kid`、`jti` 和短指纹，不写完整票据。

**A0 分流。** 如果 role = dial、target 等于本 Hub 指纹，并且没有启用 Embedded（独立 Hub 永远不启用），这条请求**一律**走配对入口，不进入 A1–A10：token 必须为空，带 token 直接拒绝（没有合法的票据会指向 Hub 自己的指纹，单一路径更简单，也不给「票据加配对入口」的组合留解释空间）；配置里必须设了 `pairing_operator`；外层 TLS 设备不在 `deny.devices` 中。满足时产出「配对入口」Grant，直接进入锁内的 B2。其余请求走 A1–A11。

| 步骤 | 检查 |
|---|---|
| A1 | token 非空（长度上限 1024 已由路由头解析保证） |
| A2 | 严格解码：tag 18，4 元素定长数组，无不定长编码，无重复键，嵌套深度有界；protected 恰为 `{1: -8, 4: kid}`；unprotected 为空；签名 64 字节 |
| A3 | `kid` 是配置中某个签发者的公钥算出的 |
| A4 | Ed25519 验签；通过之前不解释 payload |
| A5 | payload 严格解码；`v == 2`；按 3.2 表核对必需与禁止的字段及其类型、长度；1–63 范围内没有未知键 |
| A6 | `hub` 等于本 Hub 身份指纹 |
| A7 | 设备绑定：非不记名票据要求 `dev` 等于外层 TLS 客户端证书的指纹；不记名票据要求 role = dial |
| A8 | 路由一致：`role` 等于路由头 role；dial 的 `peer` 等于 target（非零）；wait 的 target 必须为零 |
| A9 | 签发者作用域（配置中该 `kid` 绑定的 `operator`）：wait 要求 `dev == operator`；dial 要求 `peer == operator`；带 `pair_until` 的 wait 还要求 `operator == pairing_operator` |
| A10 | 时间，全部按有符号 64 位计算：`iat ≤ nbf < exp`；`iat ≤ now + skew`；`nbf − skew ≤ now ≤ exp + skew`；`exp − iat ≤ ticket_max_ttl_s`；不记名票据另须 `exp − iat ≤ bootstrap_max_ttl_s`；`pair_until` 须满足 `nbf ≤ pair_until ≤ exp` 且 `pair_until − iat ≤ pairing_window_max_s` |
| A11 | 拒绝列表：`jti` 不在 `deny.jtis` 中；外层 TLS 设备指纹不在 `deny.devices` 中。对所有票据类型都检查，包括不记名票据 |

然后在路由锁内一次完成 B 组：

| 步骤 | 检查与动作 |
|---|---|
| B1 | 用**当前**策略重新判定这个 Grant 仍然有效（锁外检查期间可能发生了重载，见 4.2），配对入口的 Grant 同样在锁内复查撤销；重新读时间，核对 `now ≤ exp + skew` |
| B2 | wait：该设备已有 wait 时回 `busy`；否则登记 wait，附上 Grant（含 `pair_until + skew`）。普通 dial：目标没有 wait 回 `unknown-peer`，已配对回 `busy`。配对入口的 dial：`pairing_operator` 没有 wait 回 `unknown-peer`，已配对回 `busy`；wait 的 Grant 没有开放窗口或窗口已过回 `unauthorized`。以上拒绝都不改变任何状态 |
| B3 | 把连接登记到撤销索引：wait 方在登记时登记，dial 方在配对时登记 |

解锁后再写 `ok`，写失败则清理连接。

**配对后的首字节期限。** 独立 Hub 把 dial 配对给 wait 之后，dial 方须在一个新的 `setup_limits.timeout`（缺省 10 秒）内发出至少一个字节，否则 Hub 关闭两端并释放 wait；Hub 读到首字节即清除期限，此后双方可以长期空闲。这是 6.3「每次占用最长 10 秒」的实际保证：没有它，持有窗口入口或有效票据的对端可以配对后保持静默，无限期占住 Operator 唯一的 wait。

### 3.5 与契约 §8.4 逐条对照

| §8.4 | 自建模式 | 说明 |
|---|---|---|
| 1 token 非空 | 适用，有改动 | 唯一例外是配对入口（A0）：target 为 Hub 自身指纹，token 必须为空，由 Operator 签名的窗口授权。独立 Hub 没有内嵌交接，与中心模式相同 |
| 2 严格解码 | 适用 | 不变 |
| 3 kid 已配置 | 适用，有改动 | 公钥来自 Hub 配置的 `issuers`，每个公钥绑定一个 Operator |
| 4 验签后才解释 payload | 适用 | 不变 |
| 5 payload 严格、`v == 1` | 改动 | `v == 2`，字段集按 3.2 表；仍然拒绝未知约束键 |
| 6 `center` / `node` / `hub` / `gen` | 部分适用 | `hub` 适用；`center`、`node`、`gen` 必须缺席 |
| 7 `dev` = TLS 指纹 | 适用，有例外 | 不记名票据不含 `dev`，用途只限 dial，并受 `bootstrap_max_ttl_s` 约束 |
| 8 role / peer / target | dial 适用；wait 改动 | wait 不带 `peer`，target 必须为零（Operator 要服务多个 Client，而 Hub 上一台设备只能有一个 wait）。另增 A9 签发者作用域 |
| 9 时间 | 适用，有补充 | `clock_skew`、`ticket_max_ttl` 取自 Hub 配置；另加不记名票据与 `pair_until` 的上限 |
| — | 新增 A11 | 拒绝列表 |
| 10 锁内复查时间、可准入状态、封存、`jti` | 部分适用 | 复查时间适用；「可准入状态」改为「当前策略仍认可该 Grant」；没有会话封存；`jti` 不消费 |
| 11 登记 wait、dial 的 busy / unknown-peer 且不消费 | 适用 | 没有 `sid`，不核对两端 `sid`；配对入口见 B2 |
| 12 消费 `jti`、撤销索引、承载截止 D | 部分适用 | 撤销索引适用（按 Grant）；不消费 `jti`；没有 D，见 3.6 |
| 解锁后写 `ok` | 适用 | 不变 |
| §8.5 Operator wait 的 target = `client_fp` | 改动 | 自建模式下 target 为零；能连进来的 dial 已由 Hub 按票据限定为指向该 Operator |
| §8.5 每条连接一张新票据 | 改动 | 设备 dial 票重复使用；wait 票每次注册现签 |

### 3.6 期限与撤销语义

**票据只约束准入。** Hub 只在登记 wait、配对 dial 的那一刻检查票据。已建立的转发不会因为票据到期而被切断；空闲的 wait 在 wait 票到期后仍然保持登记（Operator 每次重新注册都会换一张新票）；配对入口的窗口在每次配对入口 dial 时按当时的时间判定。这样定的理由：自建模式没有中心记账与租约，到期切断换不来任何可执行的保证，反而会在诊断进行中硬切断连接，违背 [CENTER-PROTOCOL §6](CENTER-PROTOCOL.md#6-授权期限中心故障与会话结束)。需要立即切断时，用撤销。

**撤销等于编辑 Hub 配置后重载。** 可用的手段：

| 手段 | 效果 |
|---|---|
| 从 `issuers` 删除某个公钥 | 该密钥签的所有票据失效。用于签发密钥泄露，或停用一台 Operator |
| `deny.devices` 加入设备指纹 | 该设备的所有连接都被拒绝，不论持有什么票据，也包括配对入口。撤销某个 Client 时首选这一项 |
| `deny.jtis` 加入票据 ID | 只作废这一张票据，主要用于泄露的邀请码（Operator 签发邀请时会在日志里记录不记名票据的 `jti`） |
| 去掉 `pairing_operator` | 关闭配对入口 |

重载在路由锁内原子地替换策略，并关闭新策略不再认可的已登记 wait 和已建立的转发（判定只看撤销相关的条件，不看时间，所以重载不会顺带切断票据已到期的正常连接）。传播上限就是重载的那一刻；不重载就不生效。界面与文档不得宣称即时吊销。

设备指纹在 `deny.devices` 中时，Client 再连会得到 `unauthorized`。移除指纹并重载后即解除设备撤销，Client 可立即用仍有效的设备票重连。若要求重新配对，应在同一次重载中把该设备仍有效的设备票 `jti` 加入 `deny.jtis`，或轮换签发密钥并移除旧公钥。移除旧公钥会同时作废该密钥签发的其他票据。Operator 一侧对 Client 的信任（`trusted_peers`）是内层的事，R3 不改变它。

**签发密钥轮换。** Operator 生成新密钥，自建者把新公钥加进配置并重载；新密钥签发的设备票随每次会话下发，30 天后旧公钥签的票据都已过期，再删除旧条目。如果提前删除旧公钥，期间没有连过的 Client 需要重新配对。

## 4. Hub 命令与配置

### 4.1 配置文件

JSON 格式，UTF-8，至多 64 KiB，按 `settings` 的严格规则解码：未知字段、重复键、`null`、类型错误、尾随内容一律拒绝。**必需字段缺失、`issuers` 为空或任何字段不合法时，`serve` 拒绝启动**；可选字段缺失时使用缺省值。

```json
{
  "version": 1,
  "mode": "selfhost",
  "identity_dir": "/var/lib/renet-hub/identity",
  "listen": "0.0.0.0:443",
  "issuers": [
    {"name": "shop-pc", "operator": "ABCD-EFGH-…", "public_key": "base64url-32-bytes"}
  ],
  "pairing_operator": "ABCD-EFGH-…",
  "ticket_max_ttl_s": 2678400,
  "bootstrap_max_ttl_s": 86400,
  "pairing_window_max_s": 900,
  "clock_skew_s": 30,
  "setup_limits": {"concurrent": 64, "per_second": 32, "burst": 64, "timeout_s": 10},
  "deny": {"devices": [], "jtis": []}
}
```

| 字段 | 必需 | 缺省 | 约束 |
|---|---|---|---|
| `version` | 是 | — | 恰为 1（配置文件 schema 版本，不是票据的 v） |
| `mode` | 是 | — | 恰为 `"selfhost"`；R5 另行定义中心模式 |
| `identity_dir` | 是 | — | 必须已有 Hub 身份（由 `renet-hub init` 创建），`serve` 不会自动生成，避免运维不知道新指纹 |
| `listen` | 是 | — | `host:port`，允许通配主机 |
| `issuers` | 是 | — | 1–64 项；`kid` 不得重复；同一个 Operator 可以有多项（轮换期间） |
| `issuers[].operator` | 是 | — | 完整的非零设备指纹 |
| `issuers[].public_key` | 是 | — | base64url（无填充）编码的 Ed25519 原始 32 字节公钥 |
| `issuers[].name` | 否 | `""` | ≤ 64 字节可打印 ASCII，只用于日志 |
| `pairing_operator` | 否 | `""`（配对入口关闭） | 非空时必须是某个 `issuers[].operator` |
| `ticket_max_ttl_s` | 否 | 2678400（31 天） | 2592000–31536000。下限保证 Operator 签发的 30 天设备票能通过 |
| `bootstrap_max_ttl_s` | 否 | 86400 | 300–86400 |
| `pairing_window_max_s` | 否 | 900 | 300–3600。下限覆盖 R2 的 5 分钟码有效期 |
| `clock_skew_s` | 否 | 30 | 5–300 |
| `setup_limits` | 否 | 如上 | 各项为正整数，`timeout_s` 1–30；作用于每个入站连接在路由完成之前的阶段 |
| `deny.devices` / `deny.jtis` | 否 | 空 | 各至多 10000 项；指纹格式同上，`jti` 为 base64url 编码的 16 字节 |

### 4.2 启动与重载

- `serve` 启动时依次：严格加载配置，加载身份（用 `identity.LoadExisting`，只读：私钥缺失即失败，证书缺失或与私钥不符时在内存中重建、不落盘），构造策略，监听，然后打印 Hub 指纹、签发者数量、配对入口状态。
- Unix 上收到 SIGHUP 时重新读取并完整校验配置。新配置不合法时记录错误，**继续使用旧配置**，不会退化到无鉴权。`identity_dir`、`listen`、`mode`、`setup_limits` 任一与旧值不同的配置视为不合法，这几项需要重启才能更改。合法时调用 `Server.SetPolicy` 原子替换（4.4），并记录被关闭的 wait 与转发的数量。
- 不监视文件变化，也不支持 Windows 热重载；在 Windows 上重启 Hub 进程即可，已建立的连接随之断开，两端自动重连。

### 4.3 `renet-hub` 命令行

| 命令 | 作用 |
|---|---|
| `renet-hub init --identity-dir DIR` | 创建 Hub 身份；目录里已有身份时失败。打印 Hub 指纹 |
| `renet-hub fingerprint --identity-dir DIR` | 打印已有身份的指纹 |
| `renet-hub check --config FILE` | 按 4.1 严格校验并检查身份可加载；打印每个签发者的 name、operator 短 ID、kid，以及配对入口状态。不合法时以非零状态退出，错误信息指明字段 |
| `renet-hub serve --config FILE [--debug]` | 运行 Hub；`--debug` 打开准入拒绝原因的调试日志 |
| `renet-hub ticket inspect --config FILE --ticket-file FILE [--device FP] [--hub-fp FP]` | 运维排障：解码票据并按当前配置和时间离线执行 A2–A7、A9–A11，输出第一个失败的步骤。只读，不联网。票据从文件读取（无填充 base64url 文本，避免写进 shell 历史）。先验签，验签失败时只输出失败步骤与标注 unverified 的 `kid`，不展示票据字段。提供 `--hub-fp` 时不读取 Hub 私钥，可在没有 Hub 身份目录的机器上验票；省略 `--device` 时跳过 A7（外层 TLS 设备比对），对不记名票据还跳过 A11 的设备拒绝检查；A8（路由比对）因没有外层请求总是跳过，输出会逐项注明 |

### 4.4 `internal/hub` 的行为变化

- 删除 `Authorize` / `AuthorizeToken`，新增 `Options.Policy`。`Policy.Admit` 在锁外执行 A 组检查并返回 `Grant`。`Grant` 对 Hub 不透明，只暴露锁内要用的到期时间与配对窗口。`Policy.Valid` 在锁内执行，判定 B1 和重载。
- **不配置 Policy 时：** 有 Embedded 的 Hub 只接受内嵌自拨，其余一律 `unauthorized`（修复 2.3 的问题 1）；没有 Embedded 的 Hub 调用 `Serve` 直接返回错误。现有用独立 Hub 中转的测试改为在测试文件里构造显式放行的策略。
- 配对入口：独立 Hub 上 target 为本机指纹的 dial，按 A0、B2 处理。
- `Server.SetPolicy(p)`：在路由锁内替换策略，遍历撤销索引，关闭 `p.Valid` 判定为无效的 wait 和转发。与并发准入相比，结果只有两种：连接被拒，或者先登记、随后被关闭。
- `SetupLimits` 不再只对 Embedded 生效，独立 Hub 也套用（缺省值见 4.1）。
- 新增 `Options.Now`，供测试注入时钟。
- 外层协议格式、状态码与 sentinel error 都不变。`PROTOCOL.md` 改写「独立 Hub 配对」与「Embedded 交接」两节，并新增「配对入口」与「准入策略」两节。

## 5. 签发：密钥归属与工具

**结论：每台 Operator 持有一把只能为自己签票的签发密钥，Hub 配置是唯一的信任根。** 不设可以给全 Hub 签票的总密钥。

- 生成：Operator 进入 Hub 模式或执行 `renetd issuer` 时，如果 `<Operator 状态目录>/issuer.key`（即 `state/operator/issuer.key`）不存在，就生成 Ed25519 密钥，以 PKCS#8 PEM 格式、0600 权限创建。发布方式为：先写同目录的临时文件（`O_EXCL`、0600），写完关闭后用硬链接独占发布到最终路径，不会暴露写了一半的文件；并发创建时，后到者（硬链接得到 `EEXIST`）改为读取已有文件。文件系统不支持硬链接（部分网络共享）时回退为 `O_EXCL` 直接创建，读取方对短暂可见的残缺 PEM 做有限次重试。非 Windows 上，读取与写入都要求文件权限不含组和其他位，不满足时报错并给出 `chmod 600` 提示；FAT / exFAT 等不支持 POSIX 权限的文件系统不受支持，错误提示建议把状态目录迁到支持的文件系统。签发密钥与设备身份密钥分开，只用于签票据（契约 §8.1）。
- 授权：`renetd issuer --state-dir DIR [--name NAME]` 打印一行可以直接粘贴进 `issuers` 的 JSON 条目（`name`、`operator` = 本机设备指纹、`public_key`）。GUI 在 Operator 的 Hub 设置区显示同一条目，并提供复制按钮。
- 作用域：Hub 按条目里的 `operator` 限制该密钥能签的票据（A9），所以一台 Operator 的密钥泄露不影响 Hub 上的其他 Operator。
- 离线签发：`renetd invite --state-dir DIR --hub HOST:PORT --hub-fp FP [--ttl D]` 不需要 Operator 进程在运行，也不需要任何中心，直接用状态目录里的密钥签出不记名票据并生成邀请码。它与现有 `renetd invite` 一样，在信任文件锁下写入一次性令牌的哈希。邀请码有效期在 Hub 模式下限制为至多 24 小时。
- 自动签发：运行中的 Operator 自己签 wait 票，并在每次会话握手末尾签发设备票（6.4）。

**为什么不支持签发密钥完全离线保存。** 设备票要绑定 Client 的指纹，而这个指纹要到首次配对时才知道。如果签发密钥不在 Operator 机器上，每次首次配对都要在两端之间人工往返一次（Client 报出指纹，签发者签票，再把票交给 Client），每 30 天还要人工续期一次。而且 Operator 机器上已经有它的设备私钥：攻破这台机器的人本来就能以 Operator 身份接待任何 Client，作用域限定后的签发密钥并不扩大这个权限。负责人已于 2026-11-20 确认（第 13 节）。

## 6. Operator：配置与连接流程

### 6.1 配置

`settings.Operator` 新增 `mode`（`"direct"` / `"hub"`）和 `hub`（`addr`、`fp`、`share`），schema 升为版本 2，见第 8 节。

| 字段 | direct | hub |
|---|---|---|
| `listen` | 必需（现状） | 不监听任何端口；CLI 与 `app.OperatorConfig` 要求 `Listen` 为空 |
| `hub.addr` | 可保留；非空时校验，运行时忽略 | 必需；Operator 实际连接的 Hub 地址 |
| `hub.fp` | 可保留；非空时校验，运行时忽略 | 必需；外层钉扎用，来自 `renet-hub init` 的输出。不做首次连接时信任（TOFU） |
| `hub.share` | 可保留；非空时校验，运行时忽略 | 分享给 Client 的 Hub 地址（host:port），空字符串表示使用 `hub.addr`。例如 Operator 通过局域网连接 Hub，而 Client 使用 DDNS 名 |
| `endpoint` | Operator 的直连公开地址 | 保留，运行时忽略 |
| `bind`、各端口、`exclusive` | 不变 | 不变 |

CLI：`renetd operator --hub HOST:PORT --hub-fp FP [--share-address HOST:PORT]`，与显式给出的 `--listen` 互斥。`app.OperatorConfig` 新增 `Hub *HubConfig`：为 nil 时是直连；非 nil 时 `Listen` 必须为空。

### 6.2 在 Hub 上等待

- Operator 用 `transport/hub` 的 Endpoints 注册 wait：target 为零，token 由回调 `WaitToken` 在每次注册前现签（3.3）。
- **一个 wait 只接一个 dial。** 配对之后，这个 wait 连接就成为当前会话；会话结束后，Operator 重新注册。
- **对多个已配对 Client 意味着：** 它们共用 Operator 唯一的 wait，谁先 dial 谁得到会话，与直连模式「一个 Operator 同时只服务一个 Client」的约束一致。会话进行期间，Hub 里该 Operator 的 wait 表项仍在、并标记为已配对，所以其他 Client 得到 `busy`，界面显示「Operator 忙」，并按现有退避重试。会话结束到重新注册之间的短暂空隙里，Client 得到 `unknown-peer`，显示「Operator 不在线」，同样重试。
- wait 的 target 取零，而不是某个 Client 的指纹：Hub 上一台设备只能有一个 wait，取具体指纹就只能服务一个 Client。能 dial 进来的设备已经由 Hub 按票据限定为「持有指向本 Operator 的有效票据」或「在配对窗口内经配对入口」。进来之后，内层仍然按 `trusted_peers`、一次性令牌或 PAKE 决定是否接待，这与直连完全相同。
- 注册失败的处理：`unauthorized` 表示 Hub 未授权本机、时钟偏差或密钥已被撤销，以 30 秒间隔重试，界面显示授权条目；Hub 身份不符直接停止；`busy` 表示同一身份已在该 Hub 上等待（可能另一个进程用了同一个状态目录），按退避重试；网络错误按现有 100 ms–5 s 退避。

### 6.3 发码与配对窗口

Hub 模式下生成数字码（R2 的 `pairing.Manager` 不变）之后，Operator 调用 `Connector.Refresh()`，丢弃尚未收到任何字节的空闲 wait（含正在签票或等待重试的注册），用带 `pair_until` = 码截止时间的新 wait 票重新注册。码被兑换、撤销或作废时，再 `Refresh()` 一次，注册不带窗口的 wait；码自然到期时，窗口由 Hub 按时间关闭，不需要额外动作。

- 注册结果通过 `transport/hub` 的 `Options.OnRegister(hub, token, err)` 异步通知 Operator：每次注册尝试结束时调用一次，`token` 是该次所用 wait 票的副本（签发失败时为 nil），`err` 为 nil 表示 Hub 已确认；回调在独立协程里执行、不持有连接器的锁，多次调用之间不保证顺序，Close 之后也可能到达，因此必须快速返回。Operator 据此判断窗口是否被接受，并更新「已在 Hub 等待」状态（`Snapshot.HubWaiting`）。
- 如果带窗口的注册得到 `unauthorized`（Hub 未设置 `pairing_operator`，或者指向的是别的 Operator），Operator 撤销该码，原因记为「Hub 未为本机开放数字配对」，并立即注册不带窗口的 wait。这样 Operator 保持可达，界面给出明确原因。
- 正在会话中时没有空闲 wait，`Refresh()` 什么也不做；码要等当前会话结束、重新注册后才能使用。这与直连模式下内嵌 Hub 的单活动槽语义一致。
- 窗口期内，任何知道 Hub 地址的人都能经配对入口占用这个 wait。占用者在内层面对的是 R2 的猜码预算，每次占用最长 10 秒：配对后 10 秒内必须发出首字节（3.4 的首字节期限，否则 Hub 关闭两端），之后受内层 setup 时限约束；Hub 另有全局 setup 限流。这与直连模式下公网入口的暴露相当，但只在发码期间存在。
- GUI 生成的 Hub 邀请码有效期固定为 15 分钟（`renetd invite --ttl` 缺省同为 15 分钟，Hub 模式至多 24 小时，见第 5 节）。
- 关闭 wait 与 Hub 刚把 dial 配对给它之间存在竞态：这时 Client 收到 EOF，按退避重试即可。`Refresh` 不会关闭已经开始内层握手的连接。

### 6.4 设备票下发：消息 17 `HubTicket`

新增控制消息 17 `HubTicket`（Operator → Client，正文键 1 为字节串票据，≤ 1024 字节），以及 Hello 能力 `hub-ticket/1`。

- Client 总是声明 `hub-ticket/1`（因此直连 Hello 的原始字节与此前版本不同；配对 CI 按各自实际收到的原始 Hello 帧计算哈希，兼容旧端）。Operator 只在 Hub 模式下、且持有签发密钥（`session.Options.GrantTicket` 非 nil）时声明。
- 双方协商出该能力时，Operator **在提交配对之前**先预签设备票：消耗一次性令牌、写入信任或兑换数字码都发生在签发成功之后。签发失败时 Operator 以 `session.ReasonHubTicket` 拒绝（PairResult 或 16 的原因为 `hub_ticket`），令牌不被消耗，Client 可以用同一邀请码重试。预签成功后，消息 17 仍在握手末尾发出：在已信任 Client 的 PairResult、原始邀请令牌的 PairResult(true)、或数字配对的 16 accepted 之后。Client 在 `session.Run` 返回之前读取并校验这条消息。Client 侧 session 选项为 `EndpointHub`（本次所用端点的 Hub 指纹，用于核对票据的 `hub`）与 `RequireHubTicket`（使用不记名邀请时置位：若未协商出 `hub-ticket/1`，以 `ReasonHubTicket` 失败，避免带着很快过期的不记名票保存配对）。校验不签名（Client 不知道签发公钥，验签由 Hub 完成），只用于防止保存无用或错位的票据：`v == 2`、role = dial、非不记名、`dev` = 本机指纹、`peer` = 内层对端指纹、`hub` = 本次连接所用端点的 Hub 指纹、`exp` 晚于当前时间。校验失败时，按「无法取得 Hub 授权」结束本次连接。
- 旧版 Client 不声明该能力，Operator 也就不发送。即使收到未知编号，旧版 session 也只会跳过。
- Operator 签发失败（例如签发密钥读取失败）时按上述方式拒绝并结束会话，日志只记录 Client 短 ID，不含密钥路径或票据内容；应用层把本机签发密钥不可用单独分类为 `hub_issuer`（第 9 节）。
- 日志只记录 Client 短 ID、`jti` 和 `exp`，便于自建者按 `jti` 或设备撤销。

## 7. Client 流程

### 7.1 原始邀请码（版本 2）

`renet1:` 前缀不变，记录的版本字节为 2。版本 2 在每个端点的 `hub` 之后增加 `ticket length:u16 + ticket bytes`（长度 0 表示无票据，至多 1024）。只要有端点带票据，就必须编码为版本 2；不带票据时仍编码为版本 1，直连邀请码的字节保持不变。旧版 Client 读到版本 2 时报「邀请码无效」。

Hub 模式下的邀请码：Peer.ID = Operator 指纹；端点为 `{hub, 分享地址, Hub 指纹, 不记名票据}`；附一次性令牌和有效期。

Client 的流程：外层钉住邀请码里的 Hub 指纹，dial 的 target = Operator 指纹，token = 不记名票据；内层 Expect = Operator 指纹；走现有的 PairToken 流程；收到 17 后保存对端，端点的 token 换成设备票，界面报告已连接。**如果所用端点的票据是不记名的，而握手没有带回有效的 17**，Client 不保存对端，以「无法取得 Hub 授权」结束：带着很快过期的不记名票保存配对，只会让下一次重连失败。多端点邀请中，只把成功换得的设备票持久化到与本次路径（Hub 指纹加地址）匹配的端点，保留没有票据的端点（例如直连端点），丢弃其他未兑换的不记名票据；换得的设备票找不到匹配端点时同样按「无法取得 Hub 授权」处理。

### 7.2 「Hub 地址 + 8 位码」

输入格式与 R2 相同，`invite.ParseInput` 不变，Client 事先不需要知道这个地址是 Operator 的内嵌 Hub 还是独立 Hub。

1. 外层：`DialBootstrap(addr)` 完成证书格式、自签名与有效期检查，取得外层指纹 H 作为候选，在同一条连接上发 target = H、token 为空的 dial。对内嵌 Hub 来说这是现有的自拨交接；对独立 Hub 来说这就是配对入口（A0）。
2. 内层：以零 Expect 完成内层 TLS，取得实际的对端指纹 O，同样只是候选。
3. 按 O 与 H 是否相同选择配对能力：
   - O == H：Operator 自己就是 Hub，即直连。只声明 `pair-direct/1`，CI 与 R2 相同。
   - O ≠ H：经独立 Hub。只声明 `pair-hub/1`，CI 在直连 CI 的末尾追加 32 字节的 `hub_fp` = H：`lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", "pair-hub/1", "renet/1", FP_client, FP_operator, SHA-256(HC), SHA-256(HO), hub_fp)`。Operator 取自己配置的 `hub.fp`。
4. 其余消息、预算、承诺、SAS（默认不显示）、16 的原因枚举以及「收到 accepted 后沿用本连接」，都与 `pair-direct/1` 相同；之后 Operator 发出 17。Client 保存 `{ID: O, Endpoints: [{hub, 输入的地址, H, 设备票}]}`。

**为什么把 Hub 指纹写进 CI。** H 来自未经认证的外层。如果路径上有一个伪造的 Hub H'，它可以终止 Client 的外层连接，再以自己的设备经真 Hub 的配对入口转发内层字节。这样 PAKE 本身仍会成功，Client 却会把 H' 钉成以后要连的 Hub。把 `hub_fp` 写进 CI 后，两端看到的 Hub 不同时 PAKE 即失败，界面统一报「数字码不正确或连接被篡改」，Operator 也不会写入信任。此外，O ≠ H 时只允许 `pair-hub/1`，而直连 Operator 只声明 `pair-direct/1`：两种模式的 CI 永远不会混用，也就不能把「经中继」伪装成「直连」，或者反过来。

`ConnectBootstrap` 现在要求内层身份等于 H；改为零 Expect 之后，「两层身份相同」由 Client 在第 3 步通过选择能力来保证，并不放松直连模式：O == H 时行为与 R2 完全相同。

### 7.3 已保存配对的重连与续期

- `transport.Endpoint` 新增 `Ticket string`，存放不透明的票据字节；用 string 是为了保持 Endpoint 可比较。`Connector.Dial` 对每个端点把它的 Ticket 作为 token 发出；内嵌 Hub 端点的 Ticket 为空。
- 拨号前，Client 用不验签的解析读取所存票据的 `exp`，已过期的端点不再联网；所有候选端点都过期（且没有无票端点）时直接显示「Hub 授权已到期或被撤销」。只要还有别的可用端点，就继续用它们。这只用于界面提示，准入仍以 Hub 的判定为准。
- 连接成功并收到 17 之后，用新票替换本次所用端点的票据：按 `PathInfo.Hub` 加 `PathInfo.Via` 定位端点，调用 `trust.Store.UpdateEndpointTicket(ctx, peer, hub, addr, ticket)` 在信任文件锁下原子写入；定位不到唯一端点则报错。
- 判定授权失效：Hub 对持票 dial 回 `unauthorized` 只在**全部候选端点**都被拒绝授权时才停止重试并显示授权失效；只要有一个端点仍可重试（网络错误、`busy`、`unknown-peer` 等），就继续按退避重试，避免一条被撤销的路径让其他可用路径一并停摆。
- `transport.PathInfo` 新增 `Hub`（外层钉住的指纹），用来确定本次所用的端点，并在界面上显示「经 Hub」。

### 7.4 存储

`trust.json` 升为版本 2：端点记录增加可选字段 `ticket`（base64url）。读取同时接受版本 1 和版本 2；版本 1 的文件里出现 `ticket` 时按损坏处理。写入时只要有任一端点带票据就写版本 2，否则仍写版本 1，这样只使用直连的用户降级到旧版本时不受影响。`trust.ReadPeer` 同步支持。设备票只能由持有对应设备私钥的 Client 使用，但它带有设备指纹和 `jti`，所以诊断导出、日志与自检报告都不包含票据。

## 8. 中心地址配置：R3 做到哪一步

**R3 交付：**

1. **配置。** `settings` 升为版本 2。顶层新增 `center: {"id": "", "api": []}`，两种角色共用。`id` 符合契约 §1 的 `[a-z0-9.-]{1,32}`；`api` 为 1–4 个绝对 `https://` URL，不含用户信息、查询和片段，每个至多 256 字节；`id` 为空当且仅当 `api` 为空。同一次升级加入 6.1 的 `operator.mode` 与 `operator.hub`。`Load` 读入版本 1 时在内存中补齐缺省值（`mode = "direct"`、`hub.addr`、`hub.fp`、`hub.share` 均为空字符串、`center` 取构建期缺省中心，社区构建为空）；读入版本 2 时按严格规则解码，其中显式保存的空中心优先于构建期缺省值。`Save` 总是写版本 2；旧版程序读版本 2 会报未知字段而拒绝加载（结果同样是拒绝，不会误读）。社区构建的缺省中心为空；官方发行版通过构建参数注入缺省值（`-ldflags -X`，只在发行层配置，代码不分叉）。
2. **显式切换。** 中心与 `Client.Peer` 一样由控制器管理：`Save` 始终合入当前已保存的中心，表单不能顺带修改它；切换只能通过 `Controller.SetCenter(ctx, center)`，界面上经过确认对话框，并且只能在空闲时执行。
3. **显示。** Client 与 Operator 的状态区显示「中心：未配置」或中心 ID，连接详情另外显示「连接方式：直连 / 经 Hub（地址、短指纹）」。自建 Hub 不是中心，两者分开显示。
4. **核对。** 新包 `internal/center` 只实现无需签名的 `GET /v1/meta`。界面提供「检查中心」：逐个入口请求，单次超时 10 秒（契约 §14 的 `fallback_attempt_timeout`，逐入口各自计时），核对返回的 `center_id` 与配置一致，结果按入口显示为已验证、不可达或标识不一致。请求携带 `Renet-Client: <role>/<app_version>` 头（非 SemVer 的构建发送 `0.0.0-dev`），拒绝重定向，要求校验证书的 TLS，不带任何凭据。只读，不发送任何身份信息，不保存结果。
5. **隔离规则（写入文档，R5 执行）。** 中心相关的状态一律存放在 `state/<role>/centers/<center_id>/` 下。直连与自建 Hub 的信任（`state/<role>/trust.json`）不随中心切换而变化，中心模式也不读取它。切换中心不迁移、不删除任何状态。设备身份仍为每个角色一份。
6. **只有 8 位码的输入。** 配置了中心时，提示「暂不支持中心数字码」；未配置时，沿用 R2 的提示「请输入直连地址和端口」。不联网。

**推迟到 R5：** 登录与设备记录、请求签名与 nonce、入口回退、兑换与领票、中心模式的钉住记录及其目录的实际读写、SAS 确认界面、中心模式的 Hub。R3 不为这些功能预先建立没有调用方的接口。

## 9. 失败原因与界面

沿用 `internal/locales` 的英文原文作为键，三种语言同步，占位符一致。远端原因只接受固定枚举。

| 场景 | 端 | 处理 | en（键） | zh-Hans | zh-Hant |
|---|---|---|---|---|---|
| 外层 Hub 指纹不符 | 两端 | 停止 | Hub identity mismatch | Hub 身份不匹配 | Hub 身分不符 |
| 无法连接 Hub | 两端 | 退避重试 | Cannot reach hub | 无法连接 Hub | 無法連線 Hub |
| wait 被拒 | Operator | 30 秒重试，显示授权条目 | Hub has not authorized this device. Check the entry and the system time. | Hub 未授权本机，请检查授权条目与系统时间 | Hub 未授權本機，請檢查授權條目與系統時間 |
| 带窗口的注册被拒 | Operator | 撤销码，注册无窗口的 wait | Hub numeric pairing is not enabled for this device | Hub 未为本机开放数字配对 | Hub 未為本機開放數字配對 |
| 正在注册 wait | Operator | 状态标题 | Connecting to hub | 正在连接 Hub | 正在連線至 Hub |
| wait 被拒 | Operator | 状态标题 | Hub has not authorized this device | Hub 未授权本机 | Hub 未授權本機 |
| 已在 Hub 等待 | Operator | 状态 | Waiting on hub | 已在 Hub 等待 | 已在 Hub 等待 |
| 设备票被拒，或本地判断已过期 | Client | 停止，提示索取新码 | Hub authorization expired or revoked | Hub 授权已到期或被撤销 | Hub 授權已到期或遭撤銷 |
| 配对入口被拒（窗口未开或已过） | Client | 停止 | Code unavailable（R2 已有） | 数字码不可用 | 數字碼無法使用 |
| `unknown-peer` | Client | 退避重试 | Operator offline | Operator 不在线 | Operator 不在線 |
| `busy` | Client | 退避重试 | Operator busy（R2 已有） | Operator 忙 | Operator 忙碌中 |
| 本机签发密钥不可用（读取或创建失败、权限不符） | Operator | 启动时读取失败则启动报错；运行中签 wait 票失败则在状态里显示该原因 | Issuer key on this device is unavailable. Check the state directory and file permissions. | 本机 Hub 签发密钥不可用，请检查状态目录与文件权限 | 本機 Hub 簽發金鑰無法使用，請檢查狀態目錄與檔案權限 |
| 握手缺少 17，或 17 不合格 | Client | 停止，不保存 | Could not obtain hub authorization | 无法取得 Hub 授权 | 無法取得 Hub 授權 |
| PAKE 失败（含 Hub 被替换） | Client | 同 R2 | Incorrect code or connection tampered（R2 已有） | 数字码不正确或连接被篡改 | 數字碼不正確或連線遭竄改 |
| 设置界面 | Operator | — | Connection mode / Direct / Self-hosted hub / Hub address / Hub fingerprint / Hub authorization entry / Copy entry | 连接方式 / 直连 / 自建 Hub / Hub 地址 / Hub 指纹 / Hub 授权条目 / 复制条目 | 連線方式 / 直連 / 自建 Hub / Hub 位址 / Hub 指紋 / Hub 授權條目 / 複製條目 |
| 中心 | 两端 | — | Center / Not configured / Change center… / Check center / Center verified / Center unreachable / Center ID mismatch / Numeric codes from a center are not supported yet | 中心 / 未配置 / 更换中心… / 检查中心 / 中心已验证 / 中心不可达 / 中心标识不一致 / 暂不支持中心数字码 | 中心 / 未設定 / 更換中心… / 檢查中心 / 中心已驗證 / 中心無法連線 / 中心識別碼不符 / 暫不支援中心數字碼 |
| 路径 | 两端 | — | Via hub %s | 经 Hub %s | 經 Hub %s |

`unauthorized` 在 Client 一侧按所用的入口区分：持票 dial 被拒是授权问题，配对入口被拒是码不可用。Hub 不提供更细的原因，这与契约 §8.4 末段一致；自建者可以用 `renet-hub ticket inspect` 或 `--debug` 日志排查。自检（`internal/checkup`）对 Hub 端点只做外层 TLS 与指纹钉扎检查，不发送路由头，也不发送票据。

## 10. 测试计划

全部离线：外层使用 loopback 监听，内层使用真实 TLS 或 `net.Pipe`、内存传输，时钟可注入；不访问公网，不依赖真车或外部服务。

| 层次 | 可判定的结果 |
|---|---|
| 票据编码 | 固定密钥与时间的向量：三种票据的 payload、protected、Sig_structure、完整字节、`kid`；最大尺寸断言。拒绝向量：篡改签名或 payload、未知 `kid`、protected 多键、unprotected 非空、缺 tag 18、不定长编码、重复键、`v` 为 1 或 3、出现 2/3/7/8/14 号键、未知约束键（< 64）；接受带未知信息键（≥ 64）的票据；`bearer: false`；不记名票据带 `dev`；dial 缺 `peer`；wait 带 `peer`；超过 1024 字节 |
| 准入策略 | 按 A0–A11 表驱动：`hub` 不符；`dev` 与 TLS 指纹不符；role 不符；`peer` 与 target 不符；wait 的 target 非零；作用域不符（wait 的 `dev` 或 dial 的 `peer` 不是签发者绑定的 Operator）；非指定 Operator 带 `pair_until`；时间边界（`exp + skew` 前后 1 秒、`nbf` 在未来、`iat` 超前、超出 `ticket_max_ttl` / `bootstrap_max_ttl` / `pairing_window_max`、`pair_until > exp`、接近 uint32 上界）；`deny.devices` 与 `deny.jtis` |
| Hub 服务 | 独立 Hub 无策略时 `Serve` 报错；内嵌 Hub 无策略时陌生 wait 与非自拨 dial 得到 `unauthorized`（开放中继回归）；同一张设备票重复使用；会话中第二个 dial 得到 `busy`、无 wait 时得到 `unknown-peer`，且都不改变状态；配对入口在窗口内配对成功、窗口过后（假时钟）被拒、未配置时被拒、带 token 时被拒；`SetPolicy` 关闭被撤销的空闲 wait 和已配对转发，保留其他连接；`SetPolicy` 与准入并发（`-race`）时只出现「拒绝」或「登记后关闭」两种结果；独立 Hub 的 setup 限流生效；退出时没有协程泄漏 |
| 配置与命令 | 严格解码：缺必需字段、未知字段、重复键、`null`、`issuers` 为空、重复 `kid`、`pairing_operator` 不在签发者中、各项越界都被拒绝；重载遇到非法文件时保留旧策略；重载修改 `listen` 或身份时被拒；`renet-hub init / fingerprint / check / ticket inspect` 用临时目录完成往返测试；`serve` 在 loopback 上启动，用测试 context 停止 |
| transport/hub | 每个端点使用自己的 Ticket；`WaitToken` 在每次注册前被调用；`Refresh` 只重新注册空闲的 wait，不影响已开始内层握手的连接和已建立的会话；`DialBootstrap` 在经独立 Hub 时返回 O ≠ H，`PathInfo.Hub` 正确 |
| cpace / proto / session | `pair-hub/1` 的 CI 向量（独立生成器产出，与 R2 的应用向量放在一起）；能力矩阵：直连 Operator 对 O ≠ H 的 Client 返回不支持，Hub Operator 对 O == H 的 Client 返回不支持，旧端缺能力时不回退；`hub_fp` 不同则 PAKE 失败且不写入信任；17 缺失、超长、`dev` / `peer` / `hub` 错位、不记名、已过期时都结束连接；未协商能力时不发送 17；旧版 session 跳过未知编号 |
| 邀请码与存储 | 版本 1 的编码与现有 golden 字节一致；版本 2 往返；票据长度边界；旧解码器拒绝版本 2；`trust.json` 读版本 1、无票据时写版本 1、有票据时写版本 2、版本 1 带 `ticket` 判为损坏；`ReadPeer` 读版本 2；settings 版本 1 自动升级、版本 2 严格解码、中心与 Hub 字段的校验 |
| 端到端 | 一个进程内起真实的 Hub（loopback）、Operator 与 Client 运行时，假网关使用现有 hooks：① 原始邀请码经 Hub 首次配对，收到设备票，邀请码过期后用设备票重连；② 「Hub 地址 + 码」在窗口内配对成功、码错扣次数、窗口关闭后显示「数字码不可用」；③ 伪造 Hub 终止外层后经真 Hub 的配对入口转发，PAKE 失败，两端都不写入信任；④ 把 Client 加入 `deny.devices` 并重载，会话被切断，重连得到授权失效且 Client 停止重试；⑤ Operator 未被授权时界面状态与重试间隔正确；⑥ 本地票据已过期时不联网；⑦ Client A 会话中，Client B 得到「Operator 忙」，A 结束后 B 连上；⑧ 第二次会话把所存票据替换为更晚到期的新票；⑨ Hub 重启后 Operator 重新注册，Client 自动重连；⑩ 非指定 Operator 发码时撤销码并保持可达 |
| 秘密与日志 | 注入含票据与码的输入，断言日志、设置、诊断导出、自检报告、订阅状态中都不出现完整票据或码；Hub 日志只有 `kid`、`jti` 与短 ID |
| 中心 | 用 `httptest` 的 loopback TLS 服务模拟 `/v1/meta`：一致、标识不一致、超时、非 2xx、超大响应体；`SetCenter` 只在空闲时生效，`Save` 不能修改中心 |
| 人工验收（待执行） | 在台架上用隔离的假车，Hub 跑在家庭网络以外的主机上，Operator 与 Client 都位于 NAT 之后：分别用邀请码和数字码完成首次配对，重启两端后无码重连，按设备撤销后重载。记录版本、拓扑、结果与脱敏日志，单列，不进入离线 CI |

## 11. 与 CENTER-CONTRACT 的关系

R3 不改变中心模式的任何字段、步骤或参数。以下修订已于 2026-11-20 经负责人批准，写入 [CENTER-CONTRACT](CENTER-CONTRACT.md) 第八版（见其第 18 节），条文如下：

1. **§8.2 `v` 一行**改为：「`1` = 中心票据。`2` 是自建票据（[SELFHOST §3](SELFHOST.md#3-自建票据v--2)），字段集与准入规则不同；中心模式的 Hub 按 §8.4 第 5 步拒绝 `v ≠ 1`。」另在表后注明：键 15 `pair_until`、16 `bearer` 只在 `v = 2` 中定义，中心票据中出现即按未知约束键拒绝。
2. **§10 新增 10.7「自建 Hub 模式」**：配对能力 `pair-hub/1`；CI 为 `lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", "pair-hub/1", "renet/1", fp_client, fp_operator, SHA-256(HC), SHA-256(HO), hub_fp)`，`hub_fp` 由 Client 取外层实际看到的指纹，由 Operator 取本地配置的 Hub 指纹；其余规则同 10.6。Client 只在内层对端指纹不等于外层指纹时声明此能力。
3. **开头「session 控制消息只新增第 10.2 节的消息编号」**改为：「……只新增第 10.2 节的消息编号，以及自建模式的 17 `HubTicket` 与能力 `hub-ticket/1`（SELFHOST §6.4），中心模式不使用它们。」
4. **§15** 增加一行指向 SELFHOST 第 10 节的自建票据向量，并注明 R5 实现中心验证器时，需要补充「`v = 2` 票据被中心 Hub 拒绝」的向量。
5. **§16 第一条**：把示意名 `Admission` 改为 R3 实际落地的 `hub.Policy` / `hub.Grant` / `Server.SetPolicy`，并注明中心模式的策略在 `Grant` 中携带 `sid`、`jti` 与 `exp`，在 `Valid` 与锁内提交中完成第 10–12 步。
6. **§17** 改写为：「自建模式使用 `v = 2` 的票据，编码与签名结构同 §8.1，字段、Hub 校验步骤、期限与撤销语义见 SELFHOST 第 3 节。没有中心，因此没有代次、心跳、租约与 `jti` 消费；票据只约束准入，撤销通过修改 Hub 配置并重载生效。`ticket_max_ttl` 等参数由自建者在 Hub 配置中设定（SELFHOST 4.1）。」

## 12. 实现任务

每项都是一个可以独立提交的 commit，只改列出的包（及其测试和 `go.mod`）。每个提交都必须单独通过 `gofmt -l .`（无输出）、`go build ./...`、`go vet ./...`，并通过所改包的 `go test`。「对外接口草案」一列保留设计时的草案；冻结后的签名以 [CONTRACTS · R3](CONTRACTS.md#r3-自建-hub-与中心地址配置2026-11-20-冻结) 与代码为准（实现与草案的差异：T9 另有 `EndpointHub`、`RequireHubTicket`、`Now`；T13 的 `FetchMeta` / `Check` 增加 `role`、`appVersion` 参数，`Meta` 增加 `MinAppVersion`；T5 增加 `Options.OnRegister`）。

| # | 标题与包 | 对外接口草案 | 依赖 | 状态与提交 |
|---|---|---|---|---|
| T1 | `feat(ticket): 自建票据编解码、签名与签发密钥`，新包 `internal/ticket` | `type Kid [8]byte`；`KidOf(ed25519.PublicKey) Kid`；`type Claims struct{ Device, Peer, Hub identity.Fingerprint; Role uint8; IssuedAt, NotBefore, Expires, PairUntil time.Time; JTI [16]byte; Bearer bool }`；`Sign(ed25519.PrivateKey, Claims) ([]byte, error)`（签名前先按 3.2 校验）；`Verify(raw []byte, key func(Kid) (ed25519.PublicKey, bool)) (Kid, Claims, error)`（A2–A5，不含时间与路由）；`Inspect(raw []byte) (Kid, Claims, error)`（不验签，供 Client 提示与运维工具使用）；`LoadOrCreateIssuer(path string) (*Issuer, error)`；`(*Issuer).PublicKey()`、`Kid()`、`Wait(now time.Time, dev, hub identity.Fingerprint, pairUntil time.Time) ([]byte, error)`、`Dial(now time.Time, client, operator, hub identity.Fingerprint) ([]byte, error)`、`Bootstrap(now, expires time.Time, operator, hub identity.Fingerprint) ([]byte, [16]byte, error)`；常量 `WaitTTL = 10 * time.Minute`、`DialTTL = 30 * 24 * time.Hour` | 无 | 已完成：`e24994b`；后续 `6fa51fa`、`3a4429d`、`4060ba7` |
| T2 | `feat(hub): 准入策略、配对入口与重载撤销`，`internal/hub`，同时修改依赖开放中继的测试 | `type Grant struct{ Ref any; Expires, PairingUntil time.Time }`；`type Policy interface{ Admit(dev identity.Fingerprint, req Request, now time.Time) (Grant, error); Valid(g Grant, dev identity.Fingerprint) bool; PairingOperator() identity.Fingerprint }`；`Options.Policy`、`Options.Now`；删除 `Authorize` / `AuthorizeToken`；`(*Server).SetPolicy(Policy)`；独立 Hub 无策略时 `Serve` 报错；同步改写 `PROTOCOL.md` | 无 | 已完成：`6936e5a`；后续 `4d353b6`（首字节期限） |
| T3 | `feat(hubauth): 自建 Hub 配置与准入策略`，新包 `internal/hubauth` | `type Config struct{…}`（字段见 4.1）；`Load(path string) (Config, error)`；`(Config).Validate() error`；`NewPolicy(cfg Config, hub identity.Fingerprint) (*Policy, error)`，实现 `hub.Policy`；`(Config).ReloadCompatible(old Config) error`；`Explain(cfg Config, hub identity.Fingerprint, raw []byte, dev identity.Fingerprint, now time.Time) error`，供 `inspect` 返回第一个失败的步骤 | T1、T2 | 已完成：`902a7d1` |
| T4 | `feat(renet-hub): 独立 Hub 命令`，新目录 `cmd/renet-hub` | 子命令 `init`、`fingerprint`、`check`、`serve`、`ticket inspect`（4.3）；Unix 上 SIGHUP 时调用 `Load`、`ReloadCompatible`、`NewPolicy`、`SetPolicy`；信号接线放在带构建约束的文件中 | T3 | 已完成：`4c7b86d`；配套 `59124ec`（只读身份加载）、`6465a63`、`aa4900a`（inspect） |
| T5 | `feat(transport): 端点票据、wait 现签与引导拨号`，`internal/transport`、`internal/transport/hub` | `Endpoint.Ticket string`；`PathInfo.Hub identity.Fingerprint`；`hubtransport.Options.WaitToken func(ctx context.Context, hub identity.Fingerprint) ([]byte, error)`，替换静态的 `Options.Token`；`(*Connector).Refresh()`；`(*Connector).DialBootstrap(ctx context.Context, addr string) (transport.Session, identity.Fingerprint, error)`（返回会话与外层 H，内层 Expect 为零，按 O 是否等于 H 设定 Path）。保留 `DialDirectPair`，T11 迁移完调用方后删除 | T2（测试使用策略） | 已完成：`fc26b17`；后续 `b7b6876`（`OnRegister`） |
| T6 | `feat(trust): 信任文件版本 2 保存端点票据`，`internal/trust` | `SavePeer` / `Peer` / `ReadPeer` 透传 `Endpoint.Ticket`；按 7.4 选择写入的版本 | T5 | 已完成：`c8f7bc9` |
| T7 | `feat(invite): 邀请码版本 2 携带端点票据`，`internal/invite` | `Invite.Version` 可为 2；`Encode` 在有票据时要求版本 2、无票据时要求版本 1；`Decode` 接受 1 和 2 | T5 | 已完成：`42b242c` |
| T8 | `feat(cpace): pair-hub/1 的 CI 绑定 Hub 指纹`，`internal/pairing/cpace`、`internal/proto` | `cpace.Binding` 增加 `Capability string` 与 `HubFP [32]byte`（`pair-direct/1` 时必须为零，`pair-hub/1` 时必须非零）；`proto.PairHubCapability = "pair-hub/1"`、`proto.HubTicketCapability = "hub-ticket/1"`、`TypeHubTicket = 17`、`HubTicket{Ticket []byte}`，严格读取，上限 1100 字节；补充应用向量 | 无 | 已完成：`e3ee91d` |
| T9 | `feat(session): 经 Hub 的数字配对与设备票下发`，`internal/session` | `Options.HubFP identity.Fingerprint`（非零时数字配对只用 `pair-hub/1`）；`Options.GrantTicket func(client identity.Fingerprint) ([]byte, error)`（Operator；非 nil 时声明 `hub-ticket/1`）；Client 总是声明 `hub-ticket/1`；`(*Session).HubTicket() []byte`；17 的校验按 6.4 进行，失败归类为新的 `ReasonHubTicket` | T1、T8 | 已完成：`b4b07e6`；后续 `2b56ef2`（先签后提交） |
| T10 | `feat(settings): 配置版本 2，加入 Hub 模式与中心`，`internal/settings` | `Operator.Mode`、`Operator.Hub{Addr, FP, Share}`；顶层 `Center{ID string; API []string}`；`Load` 升级版本 1；`Save` 写版本 2；缺省中心来自构建期变量 | 无 | 已完成：`d50c84f` |
| T11 | `feat(app): Operator Hub 模式与 Client 经 Hub 配对`，`internal/app`、`cmd/renetd` | `type HubConfig struct{ Addr string; FP identity.Fingerprint }`；`OperatorConfig.Hub *HubConfig`；`InviteConfig.Hub *HubConfig`；`IssuerEntry(ctx context.Context, stateDir, name string) (string, error)`；Operator 发码、撤销与兑换后调用 `Refresh`，并处理窗口被拒；Client 改用 `DialBootstrap` 并按 7.2 选择能力，保存、续期票据，本地检查到期，并分类错误；删除 `DialDirectPair`；`renetd operator --hub/--hub-fp/--share-address`、`renetd issuer`、`renetd invite --hub/--hub-fp` | T5、T6、T7、T9 | 已完成：`1421e7f`；后续 `3fb3fb8`、`abd565c`。`DialDirectPair` 在本项删除 |
| T12 | `feat(desktop): Hub 模式设置、授权条目与中心显示`，`internal/desktop`、`internal/desktopui`、`internal/locales`、`internal/checkup` | `Controller.SetCenter(ctx context.Context, c settings.Center) error`；`Controller.IssuerEntry(ctx context.Context) (string, error)`；State 增加 Hub 等待状态与路径；补全第 9 节文案和错误映射；数字配对持久化的核对改为匹配 Hub 端点（`Hub != ID`，带 Ticket）；自检只做 Hub 外层钉扎检查 | T10、T11 | 已完成：`25d44e6`；文案 `e7a7955` |
| T13 | `feat(center): 中心 meta 核对`，新包 `internal/center`，加上 desktop 的「检查中心」 | `type Meta struct{ CenterID string; APIVersions []string; ServerTime time.Time }`；`FetchMeta(ctx context.Context, client *http.Client, entry string) (Meta, error)`；`Check(ctx context.Context, c settings.Center) []EntryResult`；响应体上限 16 KiB。可以独立推迟，不阻塞 T1–T12 | T10、T12 | 已完成：`bd6f7c0`；后续 `37ae4db`（`Renet-Client` 头） |
| T14 | `test(integration): 自建 Hub 端到端矩阵`，`internal/integration` | 第 10 节「端到端」的 ① 至 ⑩ | T3、T11 | 已完成：`979b1f9`、`6eb35a0`、`2c0d7a4`、`2a55a8b` |
| T15 | `docs: 自建 Hub 契约同步`，`docs/CONTRACTS.md`、`docs/DEPLOYMENT-MODES.md`、`docs/ROADMAP.md`、`internal/proto/README.md` | 冻结 T1–T13 的签名；更新部署模式的状态表与第 3 节的入口表；第 11 节的契约修订交给负责人决定 | T1–T14 | 已完成：本次文档同步（见各文档的 git 提交） |

依赖顺序：T1、T2、T8、T10 可以并行；T3 依赖 T1 和 T2；T4 依赖 T3；T5 依赖 T2；T6、T7 依赖 T5；T9 依赖 T1 和 T8；T11 依赖 T5、T6、T7、T9；T12 依赖 T10 和 T11；T13 依赖 T12；T14 依赖 T3 和 T11；最后是 T15。T2 改变了无策略时内嵌 Hub 的行为，必须在同一个提交里修改所有依赖开放中继的测试（`internal/hub`、`internal/transport/hub`、`internal/integration`，以及使用独立 Hub 的命令测试）。

## 13. 决定（2026-11-20 负责人确认）

以下两项按推荐方案执行；第 11 节的契约修订随本稿一起写入 CENTER-CONTRACT。

1. **R3 是否提供经 Hub 的「地址 + 8 位码」。** 推荐**提供**，采用第 6.3 节的方案：配置中指定一台 Operator，由它签名的窗口只在发码期间开放配对入口。理由：没有这个入口，自建用户只能使用长邀请码，失去 R2 刚做好的口述体验；配对入口是唯一不需要票据就能到达 Operator 的路由，但它只对一台指定的 Operator、只在码有效期内开放，暴露程度不超过直连模式，同时 Hub 指纹写入 CI，可以发现伪造的 Hub。不提供的话，可以删去 A0、键 15、`pairing_operator`、`Refresh` 和 `pair-hub/1`，工作量减少约四分之一。
2. **签发私钥放在 Operator 机器上，R3 不支持私钥完全离线的手工模式。** 推荐**接受**。理由见第 5 节：设备票必须在首次配对后才能签出，手工模式意味着每次首次配对和每 30 天的续期都要人工往返；按 Operator 限定作用域之后，签发密钥泄露的影响不超过同一台机器上设备私钥泄露的影响。若需要手工模式，可以作为后续项增加 Client 导入票据文件和 `renet-hub ticket sign`，不影响本设计的票据格式。

## 14. 实现记录与审查处理

实现于 2026-11-20 完成，提交范围为 `ebc98d6..HEAD`，任务与提交对应关系见第 12 节。离线测试全部通过；人工台架验收（第 10 节最后一行）尚未执行，因此本文状态为「已实现，台架验收待执行」。

### 14.1 与设计稿不同的实现细节

设计稿在实现与审查中修订的地方都已写入各节正文，这里只列清单，便于对照：

| 位置 | 修订 |
|---|---|
| 3.4 A0 | target 等于 Hub 指纹的请求一律走配对入口，带 token 直接拒绝；配对入口在锁内也复查撤销；A11 对所有票据类型检查 |
| 3.4 末尾 | 新增配对后首字节期限（`setup_limits.timeout`，缺省 10 秒） |
| 4.2 / 4.3 | 重载时 `setup_limits` 也需重启；`ticket inspect` 新增 `--hub-fp`、先验签、逐项注明跳过的步骤；身份用 `identity.LoadExisting` 只读加载 |
| 5 | 签发密钥的发布方式（硬链接独占发布，不支持时回退 `O_EXCL`）与权限检查 |
| 6.1 | 两种模式分别保存地址；直连模式可保留已校验的 Hub 字段 |
| 6.3 / 6.4 | `OnRegister` 异步通知；先预签设备票再提交配对；`EndpointHub`、`RequireHubTicket`；Client 总是声明 `hub-ticket/1` |
| 7.1 / 7.3 | 多端点邀请只持久化换得的设备票；续期按 `PathInfo.Hub` + `PathInfo.Via` 定位；全部候选端点都被拒才判定授权失效 |
| 8 | 版本 1 升级使用构建期缺省中心；`Renet-Client` 头、拒绝重定向、逐入口 10 秒 |
| 9 | 新增 `hub_issuer`（本机签发密钥不可用） |

### 14.2 独立审查与处理

实现期间做了两轮独立只读审查（DeepSeek）。每条意见的结论如下，「修复」均带回归测试。

第一批：

| 编号 | 意见 | 处理 |
|---|---|---|
| M1 | 中心没有纳入 `Save` 的合并，表单保存可能顺带改写中心 | 修复（T12，`25d44e6`）：`Save` 始终合入当前已保存的中心，切换只走 `Controller.SetCenter` |
| M2 | 文件系统不支持硬链接时签发密钥无法创建 | 修复（`6fa51fa`）：硬链接返回不支持或无权限时回退为 `O_EXCL` 直接创建，读取方对短暂的残缺 PEM 做有限次重试 |
| L1 | 票据解码接受非最短编码的 CBOR | 修复（`6fa51fa`）：严格校验确定性编码，非最短编码一律拒绝 |
| L2 | 签发密钥读取时不检查权限 | 修复（`6fa51fa` 读取时检查，`3a4429d` 扩展到创建并改进提示）：读写都检查，见第 5 节 |
| L3 | 消息 17 是否会被投递给应用层的 `OnMessage` | 无需改：T9 已覆盖 |
| L4 | `internal/settings/README.md` 未反映版本 2 | 修复（`6fa51fa`）：README 已写明版本升级、Hub 模式校验与构建期缺省中心 |
| — | 票据向量的生成器没有入库，向量无法复现 | 修复（`6fa51fa`）：`internal/ticket/testdata/gen/generate.py` 与 `testdata/README.md` 入库，向量由它独立产出 |
| — | 若干测试缺口 | 补齐 |
| — | 公开仓库守卫的豁免按整个文件放行 | 修复（`6fa51fa`，取代 `0e71259` 的文件级条目）：改为按行豁免（只放行 PEM 头字面量） |

第二批：

| 编号 | 意见 | 处理 |
|---|---|---|
| S1 | 静默的 dial 配对后可以无限期占住 Operator 的 wait | 修复（`4d353b6`）：配对后首字节期限，见 3.4 |
| C1 | target 等于 Hub 指纹且带 token 的请求应如何处理 | **不改实现，以本文 A0 规则为准**：没有合法票据的 peer 会是 Hub 指纹，直接拒绝；单一路径更简单，不留组合歧义 |
| C2 / C3 | `ticket inspect` 在验签前展示字段；需要 Hub 私钥才能验票；跳过的检查没有说明 | 修复（`6465a63`、`aa4900a`）：`--hub-fp` 离线验票；先验签，失败只输出步骤与 unverified 的 `kid`；逐项注明跳过的检查 |
| C4 | FAT / exFAT 上权限检查无法通过 | 明确不支持（`3a4429d`）：改进错误提示，建议迁移状态目录 |
| C5 | 嵌套映射的键序是否符合核心确定性编码 | 核对后无缺陷：RFC 8949 §4.2.1 的键序是字节字典序，与 CoreDet 一致；补充固定该顺序的测试（`4060ba7`） |
| C6 | 指纹格式过于宽松 | 不改：契约 §1 要求接受不带 `-` 的形式 |
| C7 | 令牌在设备票签发失败时已被消耗（令牌被烧） | 修复（`2b56ef2`）：先签后提交，见 6.4 |
| — | 多端点场景中一个端点被拒就判定授权失效；本机签发密钥错误被归入网络错误 | 修复（`3fb3fb8`、`abd565c`）：全部候选端点都被拒才判定失效；新增 `hub_issuer` 分类（文案 `e7a7955`） |
| — | 测试缺口 | 补齐（`2a55a8b`：强制领票、持票准入、配对入口改派） |

### 14.3 端到端测试证据

第 10 节「端到端」矩阵在 `internal/integration/selfhost_test.go` 中实现，每个用例都起真实的 loopback Hub、Operator 与 Client 运行时：`TestSelfhostInviteExpiryAndRenewal`（邀请码过期后用设备票重连、续期换票）、`TestSelfhostWindow`（数字码窗口）、`TestSelfhostReloadRevokesClient`（`deny.devices` 重载后切断并停止重试）、`TestSelfhostUnauthorizedWait`（Operator 未被授权）、`TestSelfhostExpiredBeforeDial`（本地票据过期不联网）、`TestSelfhostBusyHandoff`（会话中的 `busy` 与交接）、`TestSelfhostRestart`（Hub 重启后重新注册与重连）、`TestSelfhostNonDesignatedOperator`（非指定 Operator 发码时撤销码并保持可达）、`TestSelfhostHubFingerprintBinding`（两端看到的 Hub 指纹不同则 PAKE 失败）、`TestSelfhostSecretSurfaces`（票据与码不出现在日志与状态里）。

另记 T14 的两项拓扑测试：`TestSelfhostImpostorHubFailsPairing` 用完整拓扑验证伪造 Hub 终止外层、再以自己的设备经真 Hub 的配对入口转发内层字节时，PAKE 失败，两端都不写入信任（第 10 节「端到端」③）；`TestSelfhostTransparentForwarding` 是它的对照：只在 TCP 层原样转发、外层 TLS 仍直达真 Hub 时，Client 在新地址上认证到真 Hub，配对成功并保存设备票与新地址，说明上一条的失败来自 `hub_fp` 绑定，而不是转发本身。

### 14.4 未验证与遗留

- 人工台架验收尚未执行，范围见第 10 节最后一行；结果记录后在此补充。
- Windows 上没有热重载（4.2）；签发密钥的 Windows ACL 不检查（只在非 Windows 上检查权限位）。
