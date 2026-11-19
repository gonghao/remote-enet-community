# PAKE 选型调研：8 位数字码配对

2026-11-19。服务于 [ROADMAP](../ROADMAP.md) R2 第 1、2 项，以及 [DEPLOYMENT-MODES 第 4 节](../DEPLOYMENT-MODES.md#4-8-位数字码必须作为-pake-口令)、[CENTER-PROTOCOL 第 5 节](../CENTER-PROTOCOL.md#5-数字码票据与信任)。中心模式的配对顺序、钉住记录和预算以 [CENTER-CONTRACT 第 10 节](../CENTER-CONTRACT.md#10-中心模式的首次配对)为准，本文第 3、4 节与其对齐。本文只记录调研结论；R2 的直连实现按本文推荐的 ristretto255 套件进行，设计见 [PAIRING](../PAIRING.md)。文中的版本、提交日期和测试结果都在当天从一手来源核对，见第 6 节。

## 1. 结论

**推荐：自己实现 CPace（draft-irtf-cfrg-cpace-21）的 `CPACE-RISTR255-SHA512` 套件。** 群运算、规范编码与单位元检查用 `github.com/gtank/ristretto255` v0.2.0（BSD-3-Clause，传递依赖 `filippo.io/edwards25519`）。采用 initiator-responder 模式，Client 作为发起方，在现有 TLS 1.3 连接内运行。sid 取 TLS exporter，双方设备指纹和会话上下文放进 CI，并按 draft §10.4 做显式密钥确认。

理由：CPace 是 CFRG 选定的对称 PAKE，规范已送 RFC Editor。按规范本身实现约 170 行，草案附录的全部相关测试向量已在原型中通过；现有纯 Go 的 CPace 库都没有审计，维护者也都只有一人。选 ristretto255 而不是 X25519：素数阶群不需要自己处理余因子，也不需要自写常量时间的 Elligator2 映射，安全关键的群运算全部落在一个跟踪标准库实现的库里；R2 分支已按此实现并逐字节通过草案 -21 附录 B.3 向量。代价是比 X25519 方案多一个模块。

补充判断：

- 备选套件是 `CPACE-X25519-SHA512`：标量乘法用标准库 `crypto/ecdh`，只为 Elligator2 引入 `filippo.io/edwards25519` 的 `field` 包。原型同样通过草案 B.1 向量，依赖少一个模块，但要自己维护约 25 行常量时间 Elligator2。协议层设计与推荐套件相同。
- 暂不引入现成 CPace 库。符合 draft-21 的三个库（第 2.2 节）质量不差，但都是近几个月新出现的单人项目，未经独立审查。审阅它们的成本不低于审阅我们自己的约 170 行，后续还要跟踪上游。日后若有库通过审计或被广泛采用，再重新评估。
- 不选 SPAKE2（RFC 9382）。在 Go 里需要点加法，标准库不提供；对称变体（M=N）没有给出固定常量和测试向量；可用的 Go 实现也比 CPace 更弱（第 2.2 节）。
- 中心模式下数字码由中心分配，中心和 API 入口都知道 PAKE 口令。PAKE 防的是 Hub 和网络路径冒充对端；防被攻破的中心，要靠两端口头核对由 ISK 派生的 6 位短校验码（SAS）。为了让 6 位 SAS 不能被中间人“磨”出来，在 CPace 前加一条承诺消息，共 5 条消息（第 3.3 节；消息编号、编码与状态机以 CENTER-CONTRACT §10.2 为准）。DEPLOYMENT-MODES §4 与 CENTER-PROTOCOL §5 已按此修正表述。
- 直连模式统一使用同一个 5 条消息的流程，默认不显示 SAS（2026-11-19 已定）。

## 2. 候选对比

### 2.1 协议

| 协议 | 规范与状态 | 类型 | 消息轮次 | 适配本项目的要点 |
|---|---|---|---|---|
| **CPace** | draft-irtf-cfrg-cpace-21（2026-04-22）。IRTF 状态“Sent to the RFC Editor”（2026-07-15），RFC Editor 状态 In Progress，尚未分配 RFC 号 | 对称（balanced） | 1 轮（2 条消息）；加密钥确认后共 3 条 | 2020 年 CFRG PAKE 评选选定的对称 PAKE。CI/sid/ADa/ADb 能直接承载上下文绑定；身份放进 CI 后，攻击者每猜一个口令和身份组合都要解一次 CDH（“quantum-annoying”）；不需要可信设置的 M/N 常量；草案附带全部套件的测试向量 |
| SPAKE2 | RFC 9382（2023-09，Informational，IRTF） | 对称 | 2 轮（含必选的密钥确认） | 必须预先固定 M、N；对称变体 M=N 只给出 `hash_to_curve("M AND N SPAKE2")` 的定义，没有常量和向量；RFC 只有 P-256 的测试向量；w 的派生方式要由应用自行规定。这些点的安全性都依赖 M、N 的离散对数无人知晓 |
| SPAKE2+ | RFC 9383（2023-09，Independent Submission） | 增强（augmented） | — | 面向服务端存验证值的场景。本项目两端都持有明文码，不适用 |
| OPAQUE | RFC 9807 | 增强 | — | 同上；CPace 草案 §4 也建议客户端/服务器长期存口令的场景才用增强 PAKE |
| J-PAKE / SPEKE | RFC 8236 / 论文 | 对称 | J-PAKE 2 轮且带零知识证明 | 2019 年 CFRG 评选中进入了对称组，但没有进入第二轮。消息大、实现复杂，Go 生态里没有可用实现，不再展开 |

### 2.2 纯 Go 实现

“向量”一列是本次在 `/tmp` 中 clone 源码、用 Go 1.27.3（`CGO_ENABLED=0`）运行其测试的结果，并抽查了库内嵌的 draft-21 ISK 值是否与草案 JSON 一致。“审计”一列只依据 README、SECURITY 文件和公开信息，未发现任何一家有独立审计。

| 实现 | 版本与时间 | 对应规范 | 测试向量 | 审计 | 许可 | 依赖 | 上下文 API | 结论 |
|---|---|---|---|---|---|---|---|---|
| **自实现原型**（`/tmp`，未进仓库） | — | draft-21，X25519 与 ristretto255 两个套件 | 草案 A.1.2、A.1.4、A.3.3、A.3.5、A.3.7、B.1.1、B.1.9（含 ISK_IR/ISK_SY）、B.1.10（低阶点与 bit 255）、B.3.1、B.3.9、B.3.11 **全部通过**；可交叉编译到 windows/amd64、darwin/arm64、linux/arm64 | 无；`crypto/ecdh` 属于 2025 年 Trail of Bits 审计 Go 密码库的范围 | 随本仓库 AGPL-3.0-only | X25519：仅 `filippo.io/edwards25519` v1.2.0；ristretto：再加 `gtank/ristretto255` v0.2.0 | 完全自定 | **推荐（ristretto255 套件）** |
| `github.com/mendsec/cpace` | v0.1.0（2026-10-13）；仓库只有 1 个提交，0 star | draft-21，仅 ristretto255 | 自带测试通过；ISK 与 B.3.9 一致 | 无（README 自述） | Apache-2.0 | `gtank/ristretto255` v0.2.0、`filippo.io/edwards25519` v1.2.0 | `Config{PRS, CI, SID, AD}` 直接暴露，内置 §10.4 确认，3 条消息 | 设计贴合本项目，但太新、单人维护；只作对照实现 |
| `github.com/the-sarge/cpace` | v0.1.3（tag 2026-08-01）；最近提交 2026-11-11，共 567 个提交，1 star | draft-21，仅 ristretto255 | 自带测试通过；testdata 中的 ISK 与 B.3.9 一致；带 fuzz 与 OSS-Fuzz 目标 | 无，README 自述 “not production-ready” | BSD-3-Clause | 同上 | 库内部按自有 profile 拼 CI（SelfID/PeerID/Context）；线格式归库所有（前缀 `0xc1`）；强制非空 SessionID 和密钥确认 | 工程纪律强，但 CI 拼法和线格式由库定义，第三方客户端要按该库的 profile 才能互通；不采用 |
| `github.com/the-sarge/cpace-x25519` | 无 tag，伪版本 `v0.0.0-20261031051458-14d4776eecc3` | draft-21，X25519 | 自带测试通过；testdata 中的 ISK 与 B.1.9 一致；另有 Sage 扩展向量 | 无 | BSD-3-Clause | `filippo.io/edwards25519` v1.2.0 | 同上 | 尚未发版；不采用 |
| `github.com/bytemare/cpace` | 伪版本 `v0.0.0-20260505155446-1112dd0d30ae`；核心代码基本停在 2021 年，2026-05 只更新了 CI 和依赖 | 早期草案：DSI 含 hash-to-curve 套件名，直接拼接而没有 `lv_cat` 长度前缀 | 自带向量通过，但**不是 draft-21 向量** | 无（README：“proof of concept”） | MIT | `bytemare/ecc`、`hash`、`hash2curve`、`secp256k1`、`nistec`、`x/crypto` 等 | — | 规范过时，排除 |
| `filippo.io/cpace` | `v0.0.0-20210101143347-24d601e2e469`；仓库最后一次推送在 2021-02 | “loosely based on draft-haase-cpace-01”，并加入 HKDF | 无标准向量 | 无（README 标明 experimental） | BSD-3-Clause | `gtank/ristretto255` v0.1.2、`x/crypto` | `NewContextInfo(idA, idB, ad)` | 早于标准化，排除 |
| `github.com/backkem/spake2-go` | v0.0.1（2025-05-29）；共 5 个提交，首个提交名为 “Initial vibe commit” | RFC 9382，P-256 | RFC 的 4 组 P-256 向量通过（已对照 RFC 正文中的 cA/cB 值） | 无（README 警示） | MIT | `go.dedis.ch/kyber/v4` v4.0.0-pre2（MPL-2.0，预发布版）、2019 年的 `x/crypto` | identities + AAD | 依赖陈旧，排除 |
| `github.com/CzarJoti/gospake2` | v0.1.6（2026-02-10） | 自称 RFC 9382，edwards25519 | 只有自测（RFC 没有 edwards25519 向量） | 无 | MIT | `filippo.io/edwards25519` v1.1.0 | — | 无法对照标准向量，排除 |
| `github.com/schollz/pake/v3` | v3.2.1（2026-11-07） | 非标准，取自 Boneh–Shoup 教材的 PAKE2 变体 | 无标准向量 | 无 | MIT | `tscholl2/siec`、`filippo.io/edwards25519` | v3.2 起可传 idA/idB | 基于 `math/big` 和已弃用的 `crypto/elliptic` 通用运算，不是常量时间实现，排除 |
| `salsa.debian.org/vasudev/gospake2` | `v0.0.0-20210510093858-d91629950ad1` | 与 python-spake2 互通，不是 RFC 9382 | — | 无 | — | — | — | 排除 |

许可：上表中的 MIT、BSD-3-Clause、Apache-2.0 都能与 AGPL-3.0-only 组合（Apache-2.0 与 GPLv3 系兼容）。引入 `github.com/gtank/ristretto255` 与传递依赖 `filippo.io/edwards25519`（均为 BSD-3-Clause）时，要按 [OPEN-SOURCE-READINESS](../OPEN-SOURCE-READINESS.md) 的流程补许可快照和 `THIRD_PARTY_NOTICES.md`。

### 2.3 自实现与引入现成库的取舍

| 维度 | 自实现（ristretto255 套件） | 引入现成库 |
|---|---|---|
| 依赖 | 2 个模块：`github.com/gtank/ristretto255` v0.2.0 与传递依赖 `filippo.io/edwards25519`（后者没有任何依赖，README 说明它跟踪标准库 `crypto/internal/fips140/edwards25519`） | 同样 2 个模块，外加库本身 |
| 安全关键代码在哪 | 群运算、one-way map、规范解码和单位元判断在 ristretto255 库。我们自己写的只有 `lv_cat`、生成元串、ISK、MAC、承诺和状态机 | 全部在第三方仓库，由单人维护、未审计 |
| 规范符合性 | 原型已通过草案全部相关向量 | 三个 draft-21 库各自声称并在测试中通过 |
| 上下文绑定 | CI/sid/AD 完全由我们定义，可以发布自己的测试向量（R4 要求） | mendsec 直接暴露 CI；the-sarge 由库拼 CI，线格式也归库所有 |
| 后续维护 | RFC 发布后对照终稿重跑向量即可；协议层版本号写进 CI | 跟踪上游发版，评估 API 变动 |
| 风险 | 自写的绑定与状态机需要独立评审 | 库作者可能停更；审阅工作量不比自写少 |

已知坑点，实现时要写成测试：

- B.1.10 中 u6、u8、u9、ua、ub 的第 255 位被置位，**不应**中止，而应得到草案给出的非零结果；只有 u0–u5、u7 必须中止。草案 JSON 把这 12 个值都标成 “Invalid”，容易误读（原型第一次测试就在这里写错了预期）。
- ristretto255 用完整 64 字节的生成元哈希做 one-way map；收到的点必须规范解码，结果为单位元时中止。若改用 X25519，生成元哈希要截取前 32 字节并清除第 255 位，上一条 B.1.10 的坑也只针对 X25519。
- 不得对外暴露 K（§10.3）；比较 MAC 用 `hmac.Equal`。

## 3. 上下文绑定与密钥确认设计

### 3.1 位置：在 TLS 内运行，exporter 作 sid

CPace 在两端之间的 TLS 1.3 连接内运行，并且在开放任何隧道之前完成。两种模式的区别在于进入 PAKE 前对端证书是否已被钉住：

- **直连模式。** Client 没有预置 Operator 指纹，按 R2 设计以本次连接取得的候选指纹作内层 `Expect`。握手完成后，两端都拿到了对端证书指纹，但此时还没有任何认证。
- **中心模式。** 两端以中心给出的对端指纹作为内层 `Expect`。指纹不一致时 TLS 直接失败，不进入 PAKE，也不扣预算（[CENTER-CONTRACT §10.1](../CENTER-CONTRACT.md#101-pake-输入)）。

两种模式都先在控制流上交换 Hello，用能力（直连 `pair-direct/1`，中心 `pair-center/1`）协商出配对用途，再运行 PAKE；双方 Hello 原始帧的 SHA-256 写进 CI。消息流程见第 3.3 节。配对成功后：直连模式沿用同一条连接作为数据连接；中心模式关闭这条连接、按钉住的指纹重连，因为诊断流量要等中心提交 `/established`。每条 TLS 连接只运行一次 PAKE，符合 RFC 9266 §4.1 的约束（同一条 TLS 连接上的 channel binding 只给一个认证实例使用）。

### 3.2 各输入的取值

| CPace 输入 | 取值 | 说明 |
|---|---|---|
| PRS | 规范化后的 8 位 ASCII 数字：去掉空格和分隔符，保留前导零 | 不额外做 KDF 或 MHF：PAKE 不产生可离线验证的材料，在线猜测只靠计数防护，MHF 对此无帮助 |
| sid | `ExportKeyingMaterial("EXPORTER-renet-pake-sid-v1", nil, 32)`，中心模式取**内层** TLS | 每条 TLS 连接唯一，双方无需额外往返就能得到。中间人终结 TLS 时，两侧的 exporter 不同，CPace 必然失败。满足草案 §10.9 “由上层协议建立唯一 sid” 的建议 |
| CI（直连） | `lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", "pair-direct/1", "renet/1", fp_client, fp_operator, SHA-256(HC), SHA-256(HO))` | 先放 initiator（Client），再放 responder（Operator），符合 §10.1.1。指纹取自本端在 TLS 握手中**实际看到的证书**，不取对方自报的值。HC、HO 是双方 Hello 帧的原始字节（含长度前缀），使能力协商受 PAKE 保护 |
| CI（中心） | `lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", "pair-center/1", "renet/1", fp_client, fp_operator, SHA-256(HC), SHA-256(HO), hub_fp, center_id, session_id)` | 与 CENTER-CONTRACT §10.1 一致。`hub_fp` 取外层钉住的 Hub 指纹，`center_id` 取本地配置，`session_id` 为 16 字节原始值，两端各自独立取得。两个能力名防止跨模式混用 |
| ADa / ADb | 空 | 能放进 CI 的都放进 CI：CI 不上线，也省掉一处容易漏写的接收方校验（§10.1.2）。以后若要发送能力协商信息，再放进 AD 并在 CI 中记录版本 |

**为什么 TLS exporter 已经绑定了通道，指纹仍要显式放进 CI：** TLS 1.3 的 `exporter_master_secret` 由 `ClientHello...server Finished` 派生（RFC 8446 §7.1），不覆盖之后才发送的客户端 Certificate。把双方指纹写进 CI，“确认覆盖双方设备公钥指纹”这一要求就不再依赖 TLS 握手细节；草案 §10.1 也优先推荐把身份放进 CI。CI 参与生成元计算，因而进入 K、ISK 和 mac_key，确认标签和 SAS 都会隐式覆盖全部上下文。

### 3.3 消息流程、承诺与密钥确认

**权威定义在契约。** 两种模式使用同一个消息序列：承诺 → Yb → Ya → Tb → Ta，共 5 条消息。消息编号、编码、SAS 的派生公式和配对状态机都以 [CENTER-CONTRACT §10.2](../CENTER-CONTRACT.md#102-sas-与承诺) 为唯一权威，本节不再重复，只保留设计理由、安全分析和预扣位置的论证。直连模式复用同一序列和编码，区别只有：能力与 CI 使用 `pair-direct/1`（第 3.2 节）；默认不显示 SAS；配对成功后写入长期的 `trusted_peers` 并沿用同一条连接；可以用 `unavailable` 等直连原因在发出 Yb 之前拒绝；预算参数不同（第 4.2 节）。

**CPace 层的计算**（不受消息顺序影响）：

- ISK、Ta、Tb 都按 initiator/responder 的固定顺序计算（`transcript_ir(Ya, ADa, Yb, ADb)`），与实际发送顺序无关。CPace 本身允许双方并行发送，Operator 先发 Yb 不影响安全性：Yb 是均匀分布的群元素，不泄露 PRS。
- 密钥确认按草案 §10.4：`mac_key = SHA-512("CPaceMac" || sid || ISK)`，`Tb = HMAC-SHA-512(mac_key, lv_cat(Yb, ADb))`，`Ta = HMAC-SHA-512(mac_key, lv_cat(Ya, ADa))`，比较时用 `hmac.Equal`。
- ISK 不用作流量密钥（流量已经由 TLS 保护），只用于派生 mac_key 和 SAS。

**为什么要承诺。** 在 CPace 原本的 3 条消息顺序里，responder 看到 Ya 之后才选 Yb。知道码的中间人（被攻破的中心或 API 入口）可以先与真 Operator 完成一次 PAKE，得到那一侧的 SAS；再对 Client 一侧离线换 yb、逐个计算 ISK，约 10⁶ 次标量乘法就能磨出相同的 6 位 SAS。Client 先承诺 Ya 之后，中间人在 Client 一侧必须先发出 Yb 才能看到 Ya，在 Operator 一侧必须先承诺自己的 Ya′ 才能看到 Yb，两侧都无法磨，单次碰上的概率为 10⁻⁶。

**直连模式也采用同一流程（2026-11-19 已定）。** 直连下只有两端知道码，不知道码的中间人完成不了任何一侧的 PAKE，所以 SAS 和承诺在直连下都没有安全收益。统一流程是为了：只维护一套状态机和一套测试向量，预扣位置相同，R4 发布的 PAKE 向量也只需一份。代价是多一个往返，对一次性配对可以忽略。直连默认不显示 SAS。

**安全分析：**

1. **承诺的哈希输入与域分隔足够。**
   - 承诺用独立的标签，并对 Ya、ADa 做 `lv_cat` 后取 SHA-512。这个标签与 CPace 内部的哈希输入（DSI 为 `CPaceRistretto255`、`CPaceRistretto255_ISK`、`CPaceMac`，均带长度前缀）都不同，不会与其他哈希混淆。
   - 隐藏性来自 Ya 本身：每次尝试的 ya 都是新的 255 位随机数，Ya 不可预测，所以承诺不需要额外的随机盐。前提是标量绝不复用。
   - 绑定性来自 SHA-512 的抗碰撞性。
   - 可选加固：把 sid 也放进承诺，使承诺绑定到这一条内层 TLS 连接。目前没有找到不加 sid 就能利用的攻击：中间人转发别人的承诺后无法打开它，最多消耗预算，而这本来就做得到。
2. **SAS 派生合理。**
   - 契约用 HKDF-SHA-512，以 ISK 为输入，带独立的 info 标签，取 8 字节按大端解成整数后模 10⁶。
   - 它与 mac_key 的构造不同，两者互相独立；64 位取模 10⁶ 的偏差约为 10⁶/2⁶⁴，可以忽略。
   - ISK 已经覆盖 CI、sid 和完整转录，SAS 无需再绑定其他输入。
   - 实现时用 `crypto/hkdf.Key(sha512.New, isk, nil, info, 8)`。
3. **预扣位置。**
   - 必要条件是：Operator 必须在发出 Tb 之前扣减。猜码攻击者最早要在收到 Tb 时才能验证自己的猜测；如果等到 Ta 校验失败才计数，攻击者断线就等于免费猜一次。
   - 统一流程把扣减放在收到承诺、发出 Yb 之前，满足这一条件，而且 Operator 在发出任何与本次尝试相关的输出前就已完成计数，更严格。
   - 代价是只发一条承诺就会消耗一次预算，但路径上的一方本来就能靠断线制造拒绝服务，安全性没有因此变差。
4. **Client 侧也要计数，不能自动重试（已采纳）。**
   - 承诺只保证单次碰上的概率是 10⁻⁶。如果 Client 在连接中断后自动重新发起配对，中间人每次看到 Ya 就能算出 SAS，不匹配就在 Tb 之前断线，等于免费再来一次。
   - 再配合 Operator 侧的预算，命中概率约为 Operator 尝试数 × Client 尝试数 × 10⁻⁶。
   - 因此 Client 每发出一次 Ya 计一次本地尝试，在 Tb 之前中断也算；中心模式每会话上限 3 次，不自动重试。
5. **SAS 必须两端都确认（已采纳）。**
   - 只由 Operator 确认，只能保护 Operator 一侧。Client 的对端可能正是中间人，它可以伪造“接受”结果，让 Client 写入中间人的指纹。Client 是车端，后果是中间人获得车辆诊断通道。
   - 因此 Client 写入钉住记录的条件是：Tb 校验通过，并且 Client 用户在本地确认 SAS 一致，不能只凭收到 `PairResult`。
   - 中心模式取消 Operator 的自动接受。
6. **同一会话内重新配对（已采纳）。** 重新配对会产生新的 ISK 和 SAS。只有当对端指纹与本会话中已经核对过 SAS 的指纹相同时，才可以免去再次核对；否则两端都要重新核对。

**消息承载。** 在控制流上、Hello 之后承载配对消息 11–16，结果由 16 `PairDecision` 表达，不复用 `PairResult`。协商出配对能力的连接只接受配对消息；每条连接最多运行一次 CPace，状态单次使用，每次尝试都重新生成标量。

**exporter 接口。** R2 分支已在 `secure` 中于内层握手后导出 exporter，并通过 `PairingExporter() [32]byte` 透传；标签须使用契约规定的 `EXPORTER-renet-pake-sid-v1`。

### 3.4 中心模式的信任边界

- 外层：两端分别与 Hub 建立 TLS，并钉住票据给出的 Hub 指纹。内层：两端之间仍然走 `secure.WrapStream`，Hub 只搬运密文。数字码只经过 Client 到中心的 API，不经过 Hub。
- 信任假设见 CENTER-CONTRACT §2：Hub 和网络路径不知道码，PAKE 使它们无法冒充对端；中心和 API 入口知道码，首次配对时属于受信方，被攻破时要靠 SAS 口头核对才能发现。只要 Client 输入的仅仅是这 8 位码、中心又要据此路由，中心总能得到码的一个确定性函数，10⁸ 的空间可以离线穷举，这一点无法靠协议消除。
- 不能用设备指纹的短 ID 代替 SAS：`Fingerprint.Short()` 只有 8 个 base32 字符，约 40 位，攻击者可以预先穷举出短 ID 相同的密钥。ISK 每次会话都是新的，中间人无法预先选定。
- **不保存长期信任。** 钉住记录按 `(center_id, session_id)` 保存，会话结束即删除，不写入直连模式的 `trusted_peers`。同一对设备下一次接待也要重新配对。不同中心、不同会话的信任因此天然隔离。
- 确认放在 PAKE 成功之后，两端都在本地进行：Operator 显示 SAS 和来访设备，接受后才写入钉住记录并发送 `PairResult`；Client 显示 SAS，用户确认一致后才写入钉住记录。中心模式不提供 Operator 自动接受。理由见第 3.3 节安全分析第 5 条。

## 4. 在线猜测防护

### 4.1 为什么一次失败只对应一次猜测

- 被动窃听者从 Ya、Yb 学不到任何可以离线验证口令的东西。Ya、Yb 是均匀分布的群元素；承诺是 Ya 的哈希；标签和 SAS 由 ISK 派生，也不提供离线验证材料。
- 主动攻击者每发出一条消息，最多绑定一个候选口令。CPace 的安全证明（草案 §10 引用的 [AHH21]）保证：不解 CDH 就无法让同一次会话同时对两个口令成立。
- 对比反例：如果把码做成 `H(code || nonce)` 之类的认证值发出，10⁸ 的空间在普通电脑上不到一秒就能离线穷举完。这也是现有 128 位令牌不能直接截短成 8 位的原因。

这个性质还依赖实现做到以下几点：

1. **Operator 在发出任何依赖 PRS 的验证材料之前扣减预算。** 必要条件是在发出 Tb 之前扣；统一流程把扣减放在收到承诺、发出 Yb 之前（第 3.3 节安全分析第 3 条）。攻击者收到 Tb 后就能验证本次猜测然后断开，所以如果等到 Ta 校验失败才计数，断线就等于免费猜一次。成功不退还预算。
2. 每条 TLS 连接只运行一次 CPace，状态单次使用，不复用标量。第二条配对消息直接断开连接。
3. Operator 永远只当 responder，并且不以 initiator 身份使用同一个码，杜绝反射和自环攻击（§10.1.2）。
4. 同一个码同时最多只有一个进行中的 PAKE，或者预算的扣减是原子的（R2 测试项“并发兑换”）。
5. 失败时对外统一报“数字码不正确或连接被篡改”，不区分原因，也不让耗时随原因变化。

### 4.2 计数放在哪一侧

| 场景 | 谁计数 | 原因与参数建议 |
|---|---|---|
| 直连 | **Operator** | 只有 Operator 持有码，所有尝试都直接打到它。按码设有效期（例如 10 分钟）、单码失败上限（例如 5 次，猜中概率 ≤ 5×10⁻⁸）、总尝试预算；成功或预算耗尽后立即作废。代价是攻击者可以故意耗尽预算让码作废，这是可用性问题，不影响安全；用户重新生成码即可 |
| 中心模式：兑换数字码 | **中心** | 中心按完整的码路由，兑换成功就等于猜中了整个码。码在全局命名空间里，每次兑换的命中概率为“当前有效码数 ÷ 10⁸”，所以除了按账号、设备、来源限速，还需要全局预算 |
| 中心模式：PAKE | **Operator 本地，每会话 3 次** | 以 CENTER-CONTRACT §10.3 为准：计数归属于会话，在收到承诺、发出 Yb 之前扣减，成功不退还，扣完以 `pake_failed` 结束会话。预算不持久化，任一端在完成配对前重启，会话即结束。不知道码的攻击者在兑换这一步就会失败，所以 PAKE 失败通常意味着路径上有人替换了对端，或连接在握手中途断开 |
| Client | **Client 本地** | 直连模式下，冒充的 Operator 每次也只能猜一次。中心模式下，Client 每发出一次 Ya 计一次，Tb 之前中断也算，每会话上限 3 次，用来限制中间人磨 SAS 的次数（第 3.3 节安全分析第 4 条）。两种模式都不自动重试，由用户决定 |

## 5. 风险与待验证项

| 项 | 状态与处理 |
|---|---|
| CPace 尚未正式发布为 RFC | draft-21 已在 RFC Editor 队列中（2026-07-15 送出，2026-10-07 状态为 Awaiting First editor），AUTH48 阶段一般不改技术内容。RFC 发布后对照终稿重跑向量；协议标签 `renet-pake-v1` 和套件名写进 CI，将来可以平滑升级 |
| 自实现未经审计 | 范围很小（约 170 行）。缓解措施：内嵌草案全部相关向量；补充 RFC 9380 curve25519 Elligator2 映射向量（本次**未跑**）；对消息解码做 fuzz；实现完成后请独立评审 |
| 映射常量时间 | ristretto255 的 one-way map 由库实现，满足草案 §10.10 的常量时间要求；若改用 X25519 套件，自写的 Elligator2 只能用 `field.Element` 的常量时间运算，需在代码评审中专门检查 |
| `filippo.io/edwards25519` 的安全公告 | GO-2026-4503（GHSA-fw7p-63qq-7hpr，影响 `MultiScalarMult`）已在 v1.1.1 修复。`gtank/ristretto255` v0.2.0 声明的传递依赖是 v1.1.0，应在 `go.mod` 中把它提升到 v1.1.1 以上（建议 v1.2.0），并用 govulncheck 确认 |
| exporter 接口 | R2 已实现导出，标签需改为 `EXPORTER-renet-pake-sid-v1`（第 3.3 节） |
| 中心知道码 | 已按 CENTER-CONTRACT §2 处理：首次配对时中心是受信方，SAS 口头核对用来发现中心被攻破的情况（第 3.4 节）。Client 侧确认 SAS、Client 侧计数和同会话重新配对三项已采纳，由契约修订落实（第 3.3 节安全分析第 4–6 条） |
| 直连模式的流程（2026-11-19 已定） | 统一使用与中心模式相同的 5 条消息流程，默认不显示 SAS，预扣位置相同。理由见第 3.3 节。是否在直连界面上提供可选的 SAS 显示（例如用户怀疑地址被劫持时核对），由界面设计决定 |
| 预算被恶意耗尽 | 只影响可用性。来源 IP 限速可以减轻，但 IP 不能作为身份依据 |
| 量子计算 | CPace 是 quantum-annoying 的；长期身份仍是 Ed25519，抗量子不在本次范围 |
| 未核实 | 没有找到任何 Go CPace/SPAKE2 库的审计报告（依据是 README、SECURITY 文件和检索结果，不排除有未公开的审计）；没有测试 `GODEBUG=fips140=only` 下的行为；原型只在 linux/amd64 上实际运行，其余平台仅交叉编译 |

## 6. 参考来源

核对日期均为 2026-11-19。

- CPace 草案：[draft-irtf-cfrg-cpace-21](https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/)（2026-04-22 发布）；状态与历史见 datatracker 的 [history](https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/history/)；正文与附录向量取自 [ietf.org 归档文本](https://www.ietf.org/archive/id/draft-irtf-cfrg-cpace-21.txt)。
- SPAKE2：[RFC 9382](https://www.rfc-editor.org/rfc/rfc9382)（2023-09）。SPAKE2+：[RFC 9383](https://www.rfc-editor.org/rfc/rfc9383)。OPAQUE：[RFC 9807](https://www.rfc-editor.org/rfc/rfc9807)。
- TLS 1.3 exporter：[RFC 8446 §7.1、§7.5](https://www.rfc-editor.org/rfc/rfc8446)。TLS exporter channel binding：[RFC 9266](https://www.rfc-editor.org/rfc/rfc9266)。
- CFRG PAKE 评选：[cfrg/pake-selection](https://github.com/cfrg/pake-selection)；[2020 年 interim 会议结果幻灯片](https://datatracker.ietf.org/meeting/interim-2020-cfrg-01/materials/slides-interim-2020-cfrg-01-sessa-results-of-the-pake-selection-process-00.pdf)。
- Go 密码库审计：[Go 博客 “Go Cryptography Security Audit”](https://go.dev/blog/tob-crypto-audit)（2025-05-19）。
- Go 标准库文档：`crypto/ecdh` 的 `PrivateKey.ECDH`（X25519 结果全零时返回错误）和 `crypto/tls` 的 `ConnectionState.ExportKeyingMaterial`，均以本地 Go 1.27.3 的 `go doc` 为准。
- 模块版本与时间：[Go module proxy](https://proxy.golang.org)（`/@v/list`、`/@latest`）；漏洞索引：[vuln.go.dev](https://vuln.go.dev)（GO-2026-4503）。
- 实现仓库（提交历史、许可证、依赖以 clone 下来的源码为准）：
  [filippo.io/edwards25519](https://github.com/FiloSottile/edwards25519)、
  [gtank/ristretto255](https://github.com/gtank/ristretto255)、
  [mendsec/cpace](https://github.com/mendsec/cpace)、
  [the-sarge/cpace](https://github.com/the-sarge/cpace)、
  [the-sarge/cpace-x25519](https://github.com/the-sarge/cpace-x25519)、
  [bytemare/cpace](https://github.com/bytemare/cpace)、
  [FiloSottile/cpace](https://github.com/FiloSottile/cpace)、
  [backkem/spake2-go](https://github.com/backkem/spake2-go)、
  [CzarJoti/gospake2](https://github.com/CzarJoti/gospake2)、
  [schollz/pake](https://github.com/schollz/pake)、
  [pkg.go.dev 上 spake2/cpace 的检索结果](https://pkg.go.dev/search?q=cpace)。
