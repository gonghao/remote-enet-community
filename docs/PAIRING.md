# R2 · 直连数字配对

设计提案，2026-11-19。Client 在一个框中输入 `host:port 4827 1936`。一次显式连接只执行一次口令交换，双端确认设备身份后，下一次连接直接使用已保存配对。本轮只覆盖 Operator 内嵌 Hub。目标与验收顺序见 [ROADMAP · R2](ROADMAP.md)，部署边界见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md)。

## 1. 密码套件选择

建议固定 **CPace，ristretto255，SHA-512，发起者在前**。使用纯 Go 的 `github.com/gtank/ristretto255 v0.2.0` 做群运算，在本仓库按固定版本草案实现小范围适配。套件标识固定为 `cpace21-r255-sha512-ir`。规范依据是 2026 年 4 月的 **Internet-Draft -21**（文本有效期到 10 月 25 日），RFC 9496 定义的是群；2026-11-19 核对 [Datatracker 记录](https://datatracker.ietf.org/api/v1/doc/document/draft-irtf-cfrg-cpace/)，最新修订仍为 21，未列后继 RFC，IETF 归档无 -22。过期草案没有最终标准的稳定性；后续规范变化需独立比较，并使用新的套件标识。实现必须逐字节通过 -21 附录 B.3 的向量比对；独立密码学复核（重点是生成元派生、点验证、身份绑定和确认顺序）列为后续项，不阻塞发布（负责人 2026-11-19 决定）。[CPace -21 §§6–10、附录 A/B.3](https://www.ietf.org/archive/id/draft-irtf-cfrg-cpace-21.txt) · [RFC 9496](https://www.rfc-editor.org/rfc/rfc9496)

| 候选及维护方 | 可核实的活动与许可 | 接口、向量和审计情况 | 取舍 |
|---|---|---|---|
| [filippo.io/cpace](https://github.com/FiloSottile/cpace)，Filippo Valsorda | Go proxy 最近版本 2021-01-01，仓库最近推送 2021-02-11。BSD-3-Clause，可随 AGPL 分发并保留声明 | 纯 Go，Start / Exchange / Finish 可传两端 ID 与额外上下文。自带 TestTranscript、坏点与单位元测试；源码明确为宽松参照 draft-haase-cpace-01，未核实覆盖该版本的独立审计 | 接口合适，但生成元、盐和派生格式不同，无法直接运行 -21 互操作向量 |
| [github.com/niomon/spake2-go](https://github.com/niomon/spake2-go)，niomon 项目 | 最近模块版本及仓库推送 2022-07-27；未核实项目整体的授权许可，不能据此确认 AGPL 兼容 | Go 实现，含角色状态、确认和 TestSPAKE2Vectors；参照 draft-irtf-cfrg-spake2-08。作者明确说明未经正式审计且运算可能非常量时间 | 不采用。正式 SPAKE2 另有 [RFC 9382](https://www.rfc-editor.org/rfc/rfc9382)，草案测试通过不足以证明实现符合 RFC |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto)，Go 项目 | 本仓已有 v0.57.0，proxy 最近发布 v0.59.0，2026-11-11。BSD-3-Clause，可兼容 | 无可直接调用的平衡 CPace / SPAKE2 握手、对应套件向量或可继承的协议审计 | 不能只组合普通 ECDH、口令摘要和 HMAC 来替代 PAKE |
| [github.com/gtank/ristretto255](https://github.com/gtank/ristretto255)，George Tankersley 等 | v0.2.0 于 2025-08-06 发布，仓库最近推送 2026-02-19。BSD-3-Clause；传递依赖 [filippo.io/edwards25519 v1.1.0](https://pkg.go.dev/filippo.io/edwards25519@v1.1.0) 同为 BSD-3-Clause，保留两份通知可与 AGPL-3.0-only 分发 | Go 1.23 模块、无 cgo；提供均匀字节映射、规范解码、标量乘和单位元。自带 RFC 群向量；完整 PAKE 另有附录 B.3 的独立固定输入/输出。未核实覆盖所选版本和上层组合的外部审计 | 推荐。直接用 Edwards 点还需自己处理余因子与编码，引入更多验证责任 |

活动日期区分模块发布时间和仓库推送，均不是维护承诺。引依赖的提交应说明“复用已有 RFC 9496 群运算和坏编码向量，以固定 CFRG 套件实现八位口令配对，避免维护曲线算术”，列出间接模块和随包许可。Go 1.27.3、CGO_ENABLED=0 下的编译及平台验证属于实现验收，本稿未编译候选库。[模块元数据](https://proxy.golang.org/github.com/gtank/ristretto255/@v/v0.2.0.info)

## 2. 首次拨号与协商

现有路径分两层。外层 TLS 1.3 使用 ALPN `renet-hub/1`，通常先验证 Endpoint.Hub，再发非零 dial.target。内嵌 Hub 把拨向自己的连接交给 Operator。内层 `secure.WrapStream` 使用 `renet/1`、双方 Ed25519 自签证书、TLS 接受字节和 yamux；第一条流交换 session v1 Hello。[Hub 协议](../internal/hub/PROTOCOL.md) · [Session 生命周期](../internal/session/README.md) · [包契约](CONTRACTS.md)

1. 仅当用户显式输入直连地址和数字码，使用专用一次性拨号函数。外层保持证书格式、自签、有效期、私钥持有和 ALPN 检查，提取实际 Hub 公钥 SHA-256 为候选 H，在取得 H 的同一条 TLS 连接上用 H 发普通 self-dial（一次解析拨号流程，不另起探测连接），token 长度为零。H 只在本次连接内有效。保留普通 Dial 的非零预期指纹约束，不允许用空指纹切换模式。
2. 内层 Client 临时以 H 为 Expect，Operator 继续以内嵌交接提供的外层 Client 设备指纹为 Expect。双方从内层连接读取实际指纹；直接模式要求两层 Operator 身份相同。此时 Client 知道对端持有候选私钥，仍须证明口令。若用户原先选择的是已保存对端，指纹变化直接失败，不自动开启数字配对或覆盖旧记录。
3. Client **只在此次兑换**的 Hello 中声明 `pair-direct/1`。新版内嵌 Operator 在 Hello 声明支持，未发码也可回复“无可用码”。双方 Role / ProtoVersion 通过现有检查且能力取交集后进入下节，优先于 Trusted 快捷路径。数字 Client 缺此能力立即关流，旧端收到 Hello 后断开即可；即使收到 Accepted 也不能越过 PAKE。原始邀请和指纹复连客户端不声明该能力，继续现行 PairToken / PairResult，原始邀请中的两层 pin 从首次拨号起生效。
4. 只有结束配对握手后才启动 session 的控制消息及数据流处理，之前不报告 Connected、不暴露车辆状态。Hub 不识别 Hello、码或配对帧。纯字节中继可以连通真正两端，无法读数据或替换身份；终止并重建内层 TLS 的中间人将得到两个不同的 exporter。

## 3. 字节契约与确认顺序

下表编号拟从当前 TypeOpenTunnel=10 后分配。使用既有“大端 uint32 长度 + CBOR envelope {1:type,2:body}”。新增 proto.ReadPair：只对数字握手中的配对帧严格解码（可重编码比对），既有 proto.Read 的宽容行为保持原样。配对消息只在首条 control stream 的指定状态接受，body 均为单字段 map，键 1 为固定 32 字节 bstr。按协议阶段读取，检查长度再分配（配对帧含 envelope 最多 256 字节），拒绝错序、重复、未知字段、重复键、非规范编码及无效点。Operator 读取 Hello 时尚未知客户端模式，两端 Hello 沿用 proto.MaxFrameLen；握手沿用 10 秒总时限，ctx 取消关闭流。失败可在任一等候点接收现有 PairResult(false, reason)，之后立即结束；成功顺序唯一。

| 顺序 | 发送者 | 消息 | 值 |
|---|---|---|---|
| 1 | Client | PairShareA (11) | Ya |
| 2 | Operator | PairShareB (12) | Yb |
| 3 | Client | PairConfirmA (13) | Ta |
| 4 | Operator | PairConfirmB (14) | Tb；已验证 Ta 且本地信任已提交 |
| 5 | Client | 本地保存并转入已连接态 | 验证 Tb 后调用 SavePeer；任何状态收到 PairResult(true) 都是协议错误；只有 Verify(Tb) 成功才可产生 DigitalPaired |

双方严格使用同一组八位 ASCII 字节作为 PRS，不先转整数。A 是 Client，B 是 Operator。以下字符串均为 ASCII，无终止符。以下 L 表示对每个参数加四字节大端字节长度后依次拼接，**只用于应用层**。草案内部的 lv_cat、generator_string、transcript_ir 一律使用草案原始编码。FP 是内层证书 Ed25519 原始公钥的 SHA-256，共 32 字节；禁止使用显示短 ID。

- HC、HO 是双方按 Client、Operator 顺序发送/收到的精确 Hello 帧字节（包括长度前缀）。捕获本流原字节，不能解码后重排。E 是内层 TLS ConnectionState.ExportKeyingMaterial("EXPORTER-renet-pair-direct-v1", nil, 32)，在握手完成后计算；外层 exporter 不参与。内层 TLS 沿用 SessionTicketsDisabled；外层无需据此改变票据设置。
- sid = E；CI = L("renet/1","pair-direct/1","cpace21-r255-sha512-ir","client",FP_A,"operator",FP_B,SHA256(HC),SHA256(HO))；ADa、ADb 均为空。根据 -21 §§6–8.3 计算生成元和临时标量，用系统密码学随机源，每边每次全新取样，拒绝零标量、单位元和不规范点；严格按 transcript_ir 导出 ISK。Ya、Yb 是各自规范编码的点。
- T = SHA256(L("renet-pair-confirm/1",sid,CI,Ya,Yb))；PRK = HKDF-Extract-SHA256(salt=T, IKM=ISK)。对 r 分别取 "client" 和 "operator"，Kr = HKDF-Expand-SHA256(PRK,L("confirm",r),32)，对应标签为 HMAC-SHA256(Kr,T)。以常量时间比较完整标签，失败即销毁握手状态。ISK 和确认密钥仅限本次配对，隧道仍由内层 TLS 加密。
- 每次接纳只给一个 Ya 返回一个 Yb，每个状态只检验一次 Ta。给定猜测，主动对端可形成一个候选共享密钥并观察确认结果。均匀八位码的第一次成功概率为 1/10⁸，q 次不同猜测的总概率最多 q/10⁸。被动记录（哪怕有份内层明文 transcript）中的两份临时公钥和 MAC 不提供口令校验器，在 CPace 的群困难性假设下，换一个猜测也不能从已知消息重算 ISK。双方设备指纹、角色、协商和 E 都参与确认，移植标签或反射无效。恶意服务器也能对一次用户发起的 Client 握手猜一次，故错码、断线或确认不明都不自动重试数字握手。

中心模式将来另设能力、套件上下文及授权域，绑定中心标识、会话和票据所指的两端身份，Hub pin 独立验证；不能复用直连的 H 等于 Operator 或已信任即获准的假设。中心若生成或看到完整口令，单凭 PAKE 不能抵抗该中心冒充未知设备；R4 要求该性质时须另行设计端侧秘密或独立验证身份的机制。[中心协议 §§4–6](CENTER-PROTOCOL.md)

## 4. 发码、计数与信任提交

| 范围 | 建议默认值 | 原因与实施点 |
|---|---|---|
| 发码 | 每个运行中的内嵌 Operator 至多一码，5 分钟 | crypto/rand.Int 在 [0,100000000) 均匀取样，按八位补零后一直作为字符串。使用单调截止时间。重发先废旧码，停止监听、重启、撤销立即作废 |
| 单码 | 最多 5 次，最多 1 次在途 | 收到格式正确的 ShareA 且准备进行点运算前，在同一锁内核对代次、截止、占位及两级余额，再同时扣一次。无队列，忙返回 busy。扣费后畸形点、断线、取消、超时一律不退；第 5 次可成功，该次失败即失效 |
| 全局 | 每个监听 Manager 滚动 15 分钟共 20 次，跨码、跨 IP | 在同一临界区记录获准开始的时刻。单码风险上界 5×10⁻⁸，该窗口最多 2×10⁻⁷。第 20 次保留本次完成权，同时禁止其他尝试及生成；该次终止即销毁码。等最早计数移出窗口后显式重新生成 |
| 未认证连接 | 建议内嵌 listener 在建 TLS worker 前非阻塞限并发 16、每秒接纳 8、突发 16；外层 setup 最长 10 秒 | 资源限额与猜测预算独立。裸 TCP、TLS 失败、半帧、busy 不消耗数字次数。仅在 setup 期间持有握手槽，进入 Embedded 后沿用单活动槽与内层截止时间。持续正确发起 PAKE 的攻击者仍能烧完码，单入口仍可能遭遇拒绝服务 |

管理器用代次和不可复用的占位标识核对所有异步完成操作。替换、过期或撤销使旧占位无效并取消握手。验证 Ta 后在锁内复核代次及期限，将唯一占位移入不可撤回的“提交中”并销毁可兑换口令；这是成功授权的线性化点。落盘期间不接受新兑换；在信任文件版本 1 的原有锁/原子替换操作中设置 Trusted[FP_A]=true，成功后才发 Tb。状态为 attempting → committing → redeemed（落盘成功）或 revoked（落盘失败）。Abort 只在 attempting 生效；延迟调用的 Finish 须幂等，在 committing 及终态均为空操作。只有 Commit 完成者可释放提交占位。不得持管理锁执行文件 I/O，Revoke(ctx,generation) 在提交中立即返回 already_redeeming，成功后返回 redeemed。保存使用独立于连接及 10 秒握手的有界 context（建议 3 秒），Stop 先禁止 Begin 并取消 attempting，退出前等待 committing 收尾。两个独立设备无法跨网络原子提交：丢失 Tb 或 Client 保存失败时，Operator 可能已经记住 Client；提示重新生成并显式配对，不能从半次握手中信任候选地址或指纹。

Client 验证 Tb 后以 FP_B、用户输入的规范地址以及 Hub=FP_B、Kind="hub" 调用现有 Store.SavePeer。将当前连接循环的目标切成该 PeerInfo，清除口令，之后走原有双层 pin 拨号。Operator 不创建虚构的 16 字节令牌，也不改 Invites；旧邀请继续由 Consume 原子消费。监听停止即关闭 Manager；下一次启动重新计数，所有数字码随之消失，必须手工发新码。时限过后不切断已建立的诊断会话。所有日志、设置、历史输入、通用状态快照、自检、诊断导出不含口令、其摘要、ISK 或标签。释放秘密引用，Go 字符串与系统剪贴板均无法保证物理擦除。

## 5. 输入和界面

去掉首尾 Unicode 空白。先用 strings.EqualFold 识别不区分大小写的 `renet1:` 调现有 invite.Decode，不折叠内部字符或全角符号，解码失败原路报错。其余以空白分字段，允许一项地址加连续八位，或地址加两组各四位；空白可为换行、制表、全角空格。只在口令字段把 U+FF10…FF19 映射为 ASCII，拒绝其他数字、标点、零宽字符及多余字段。单独八位（也可 4+4）是预留的中心输入，R2 提示补充直连地址，不联网。例：`[2001:db8::2]:47000 ００００ １２３４` 得到口令 `00001234`。

地址以 net.SplitHostPort 拆分，端口为 ASCII 十进制 1…65535；接受 IPv4、括号 IPv6、ASCII DNS/DDNS 名（单标签或点分标签，每标签 1…63 字节，字母数字及内部连字符，可有末尾根点，总长至多 253 字节不含根点）。IP 用 netip 严格解析，纯数字点分而无效的 IPv4 直接报错。拒绝空主机、未括起的 IPv6、zone、URL、路径、用户信息和通配监听地址。用 net.JoinHostPort 重建，DNS 存小写名、保留末尾根点，重连时重新解析。DNS 失败、连接超时与口令格式错误分开显示。解析错误只含枚举和出错字段，不携带输入或嵌套含原文的解析器错误。桌面现有脱敏正则只覆盖原始邀请。数字自检只向 internal/checkup 传固定 PeerCode=direct_input_valid 或对应错误枚举，Endpoints 留空；新增本地 finding 分支，不做 DNS、TCP 或口令探测。

Operator 在已有邀请区增加「生成数字码」「复制地址和数字码」「撤销数字码」。生成只在监听且可接待时可用；复制和撤销只在当前代次有效时可用。复制格式为地址、一个空格、4+4 分组。码用等宽数字，旁列倒计时和剩余可发起次数，提交中显示处理中。桌面 State.NumericCode 只携带无秘密 Status；独立的复制操作按当前代次取 Display，显式展示用的 UI 局部秘密在失效、换代或窗口关闭时清除。State.Invitation 继续服务原始邀请。Client 沿用单输入框，提交后清空敏感输入；终态提示是否需要新码。遵循浅色、直角、整数网格，状态同时用文字表达，键盘可达，简繁字体回退与窄窗布局沿用主题。[UI-DESIGN](UI-DESIGN.md)

沿用 internal/locales 的英文原文作为键，三个 JSON 同步。下表同一行的斜线分隔独立键（占位符类型、数量、顺序一致）。远端 reason 仅接受固定枚举并本地化，确认前的远端提示仅供排障，不作为已验证身份的事实。

| en（键与默认文案） | zh-Hans | zh-Hant |
|---|---|---|
| Generate numeric code / Copy address and code / Revoke code | 生成数字码 / 复制地址和数字码 / 撤销数字码 | 產生數字碼 / 複製位址和數字碼 / 撤銷數字碼 |
| Expires in %s / Attempts left: %d / Completing pairing | 剩余 %s / 剩余次数：%d / 正在完成配对 | 剩餘 %s / 剩餘次數：%d / 正在完成配對 |
| Invite or address:port and code / Enter the direct address and port | 邀请码或地址:端口和数字码 / 请输入直连地址和端口 | 邀請碼或位址:連接埠和數字碼 / 請輸入直連位址和連接埠 |
| Invalid invite / Invalid host / Invalid port / Enter eight digits | 邀请码无效 / 地址无效 / 端口无效 / 请输入八位数字 | 邀請碼無效 / 位址無效 / 連接埠無效 / 請輸入八位數字 |
| Address lookup failed / Connection timed out / Operator busy | 地址解析失败 / 连接超时 / Operator 忙 | 位址解析失敗 / 連線逾時 / Operator 忙碌中 |
| Numeric pairing unsupported / Device identity mismatch | 对端不支持数字配对 / 设备身份不匹配 | 對端不支援數字配對 / 裝置身分不符 |
| Code unavailable / Code expired / Attempts exhausted / Try again later | 数字码不可用 / 数字码已过期 / 次数已用尽 / 请稍后重试 | 數字碼無法使用 / 數字碼已過期 / 次數已用盡 / 請稍後重試 |
| Code or peer confirmation failed / Pairing interrupted / Could not save pairing | 数字码或对端确认失败 / 配对已中断 / 无法保存配对 | 數字碼或對端確認失敗 / 配對已中斷 / 無法儲存配對 |

拒绝原因依次使用 unavailable（未生成、已撤销或已兑换）、expired、exhausted、rate_limited、busy、confirmation、store_failed。只保留当前管理器末次终态给出原因，重启后统一 unavailable。无法从 MAC 失败区分输错与中间人；旧端明确缺 capability 才显示“不支持”，单独 EOF 显示“中断”。原始邀请过期沿用其期限检查。

## 6. 实施拆分和接口

以下为建议的新签名，均位于 internal 下。A 先冻结 Binding、角色、字节契约和类型化 Reason 枚举，再按下表目录并行；F 负责依赖接线和兼容验证。接口中的 Binding 持有固定长度 Exporter、ClientFP、OperatorFP 及两个原始 Hello 的 SHA-256。

| 工作项与目录所有者 | 交付接口与约束 |
|---|---|
| A · pairing/cpace（含 Binding、Reason） | Start(code string, b Binding) (*Initiator, [32]byte, error)，Respond(code string, b Binding, a [32]byte) (*Responder, [32]byte, error)；Initiator.Confirm(y [32]byte) ([32]byte,error)、Initiator.Verify(tag [32]byte) error、Responder.VerifyAndConfirm(tag [32]byte) ([32]byte,error)。状态单次使用，Responder 的标签由 session 暂持到落盘成功 |
| B · pairing、trust | Manager.Issue() (Display,error)、Status() Status、Revoke(ctx context.Context,generation uint64) error、Begin(ctx context.Context) (*Attempt,error)、Close()。Attempt.Respond(b cpace.Binding,a [32]byte) (*cpace.Responder,[32]byte,error) 在包内使用码；Operator session 仅获响应状态及占位 context。Finish、Abort 和 Commit(save func(context.Context) error) error 遵守第 4 节。trust.Store.Remember(ctx context.Context, peer identity.Fingerprint) error 更新 Trusted。Status 含 Generation、Address、ExpiresAt、AttemptsLeft、Committing、Reason；Display 内嵌 Status，另含 Code |
| C · hub、transport/hub、secure | hub.ConnectBootstrap(ctx context.Context,addr string,local *identity.Local) (net.Conn,identity.Fingerprint,error) 复用 verifyPeer 的证书检查及同一 socket 的 self-dial；普通 Connect 仍拒绝零 hubID。hubtransport.New 返回具体 *Connector 及原 Acceptor；Connector.DialDirectPair(ctx context.Context,addr string) (transport.Session,error) 使用实例已有 Local 和 Wrap，普通注册工厂仍按 transport.Connector 暴露。secure.WrapStream 在内层 HandshakeContext 成功后立即计算 E，失败关闭，成功写入 session 固定 [32]byte 字段。ownSession 若底层实现 PairingExporter() [32]byte，则返回显式转发该方法、嵌入 *ownedSession 的包装；否则保留原包装。双方本机 FP 取 app 注入的 identity.Local.Fingerprint()，远端取 Peer()。Hub 同时负责外层限流 |
| D · proto、session | 四种固定帧及有界读写；Options 增加 DigitalCode string（Client）、DigitalManager *pairing.Manager、Remember func(context.Context,identity.Fingerprint) error（Operator）。Options 另传 LocalFP。保持 Run 签名，在成功返回的 *Session 上提供只读 PairingOutcome()，区分 Legacy 与 DigitalPaired；数字 Client 仅在 Verify(Tb) 通过时标记。负责握手分支、原始 Hello 采集和类型化失败；在握手期结束前不启动隧道。旧 Options 零值行为照旧 |
| E · invite、desktop、desktopui、ui、locales、checkup | invite.ParseInput(s string) (Input,error)，Input 区分 Raw、Direct、Center，分别携带解码后邀请、地址与码、或预留类型。Controller.IssueNumericCode(ctx)、CopyNumericCode(ctx,generation)、RevokeNumericCode(ctx,generation) 按现有操作校验角色和 Stop epoch，通过当前 running.runtime 的 appRuntime 接口调用同一运行实例，回填前再核对运行代次。生成/复制用 settings.Operator.Endpoint，按 §5 校验并拒绝未指定地址；cfg.Listen 仅监听用。变更分享地址使旧码失效。补全三语言、错误映射及自检分支 |
| F · app、integration | Runtime.IssueNumericCode(ctx context.Context) (pairing.Display,error)、NumericCodeStatus() pairing.Status、NumericCodeDisplay(ctx context.Context,generation uint64) (pairing.Display,error)（仅当前有效代次）、RevokeNumericCode(ctx context.Context,generation uint64) error。RunOperator 创建 Manager，配置注入已校验分享地址；与 acceptor 共用关闭路径，ctx 取消、Stop、acceptor 关闭或 RunOperator 退出都幂等关闭 Manager、作废代次。数字模式只在网卡与唯一网关已准备好时拨一次。首次数字拨号一旦开始，任何错误（含 EOF、超时、网卡变化）直接终止，不进 clientWait。仅在 DigitalPaired 后 SavePeer 成功才发布 Connected。clientConnection 将已落盘的完整 PeerInfo 作为独立结果返回（即使随后断线），clientLoop 优先采纳、清码，再按已保存路径重连。落盘后同步发出无秘密 OnPaired(PeerInfo) 回调，desktop 在运行代次锁下设置 g.pairing，使正常订阅与退出时 recoverPairing 都能保存 settings.Client.Peer；不能依赖可合并的状态通知 |

整合者在 docs/CONTRACTS.md 补录两个新包及冻结接口。上述为分工契约，identity 指纹算法、trust 文件 schema、carside 和车辆侧转发逻辑均沿用现状。独立 Hub、中心票据与登录流程留给 R3–R5。[DESIGN §§4、9](DESIGN.md) · [CONTRACTS](CONTRACTS.md)

## 7. 测试与验收

| 层次 | 可判定结果 |
|---|---|
| 密码与编码 | 固定输入逐字节对照 -21 A/B.3（区分 ir 与 oc），另留独立生成的应用向量含两个 Hello、E、CI、Ya/Yb、ISK、T、Ta/Tb，仅用测试身份与公开示例码。换角色、指纹、Hello、exporter 任一字节均拒绝；坏点、单位元、短帧、超长、乱序、重复确认均不能写 trust |
| 管理器 | 假单调时钟测截止前后、5 次/20 次边界、滚动恢复、重发/撤销/Stop、超时弃权。并发正确兑换只允许一个提交；在提交线性化点两侧插入撤销、磁盘错误、换码，晚到回调不得复活旧码或扣新码。TLS 半连接、重复 busy 不扣猜测预算；限额释放后合法连接可达 |
| 端到端 | loopback 外层、net.Pipe 或内存传输内层，分别验证透明转发成功与终止两条 TLS 后转发 PAKE 消息失败。断言失败时两端无新 pin、无车辆消息。测试错码、最后一次猜对、过期、丢 Tb、SavePeer 失败、重启后旧码失败；成功后清空码并重启两端，已保存配对可连接，换任一设备私钥即拒绝 |
| 兼容和界面 | 旧/新 Client 与 Operator 的原邀请及 saved 路径矩阵；数字输入连旧端明确失败，绝不回退。表驱动覆盖前导零、全角码、DNS、IPv6、坏端口及仅有码；三语言键、占位符、剪贴板内容、倒计时和所有终态。检查日志、设置和诊断导出无秘密。运行 gofmt、go vet、go test，支持的平台另跑并发 race 检查 |
| 审查回归 | 从真实 Connector 取得的包装 session 导出值与测试内层 TLS 对端直接导出的 E 相等；两层建连各计数并确认只有一次外层拨号。假 Operator 发 PairResult(true) 时 trust 和 Settings 都不变；读 Ta 后断开时只观测到一次 Ya/Ta。阻塞 save，同时 Finish、Begin、Revoke，断言占位未释放、撤销立即返回且只提交一次。扩展 desktop/operations_test 的正常订阅、快速结束、端点不符及保存失败矩阵覆盖数字配对。注入含八位码的无效输入，断言 LastError、日志、自检报告、订阅状态均不含码，数字自检网络调用为零 |
| 人工验收（待执行） | 先在隔离假车上从空信任目录生成码、复制、连接、确认双端完整指纹、观察假车发现/只读通路，再重启双端无输入复连。通过后，从家庭网络以外的 Client 连公网地址或 DDNS，按同一顺序验证首次与无代码复连。记录版本、平台、拓扑、结果、失败阶段和脱敏日志，不保留真实码、地址、VIN；公网验证单列，不进离线 CI |

## 8. 负责人决定

2026-11-19 已决定：接受固定草案 -21，套件标识带修订号；-21 向量比对是实现验收的硬性条件，独立密码学复核列为后续项，不阻塞发布。其余按上述默认值实施；中心能否知道完整口令由 R4 的威胁模型决策，本轮无需等待。


## 审查处理记录

| 项 | 处理 |
|---|---|
| 1 | 已修改。内层握手后导出，跨 ownedSession 显式转发，并与 TLS 原值对照。 |
| 2 | 已修改。新增外层 bootstrap 和具体 Connector 方法；app 实际直接调用 New 并注入 Wrap，Register 并非唯一路径。 |
| 3 | 已修改。Verify(Tb) 唯一产生 DigitalPaired，落盘检查结果。 |
| 4 | 已修改。首次数字拨号失败即退出，覆盖发 Ta 后断线。 |
| 5 | 已修改。提交占位归 Commit，落盘有独立期限。 |
| 6 | 已修改。传递已保存的 PeerInfo 并覆盖快速结束；现有普通订阅可凭 Snapshot.Peer 保存，零 g.pairing.ID 阻断的是退出恢复。 |
| 7 | 已修改。管理器随监听关闭，取消在途兑换。 |
| 8 | 已修改。撤销带 ctx，提交期间立即返回明确状态。 |
| 9 | 已修改。固定字段、分享地址来源及当前 Runtime 的控制器入口。 |
| 10 | 已修改。E 负责 checkup 本地分支。 |
| 11 | 已修改。类型化错误、Operator 端秘密封装及泄漏回归。 |
| 12 | 已修改。邀请前缀沿用 EqualFold。 |
| 13 | 已修改。严格解码限配对帧。 |
| 14 | 已修改。Hello 沿用现有限长。 |
| 15 | 已修改。取 H 与 self-dial 使用同一连接。 |
| 16 | 已修改。预算归监听 Manager。 |
| 17 | 已修改。禁用票据仅指内层 TLS。 |
| 18 | 已修改。A 拥有 cpace 子包和公共契约，整合者补包文档。 |
| 19 | 不修改套件版本。已核实最新仍为过期的 -21，并记录复核来源及风险。 |
| 20 | 已修改。桌面另设 NumericCode 状态与复制操作。 |
