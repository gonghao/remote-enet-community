# Session 生命周期约定

`Options.Initiator` 必须与 transport 层拨号方向一致，与 Role 无关；initiator 打开第一条 stream 作为 control。启动 `Run` 前不应打开其他 stream。Hello 中的 `ProtoVersion` 是主版本（当前为 1），不同即返回可由 `errors.Is(err, ErrVersion)` 识别的错误。能力取交集；既有 tcp/reverse 隧道不依赖 capability，专用扩展流按其 capability 规则处理。

Client 的非零 Token 在 Hello 后发送；零 Token 不发送。Operator 对已信任对端直接返回 PairResult；未信任对端等待 PairToken，失败立即关闭。不发送 Token 且未被信任时，双方受总计 10 秒的握手超时约束。`AcceptToken` 回调负责一次性消费令牌并持久化对端信任，Session 不持有信任数据库。

握手超时独立于会话内部的取消：调用方主动取消或握手真正到期时返回相应 context 错误；其余情况下，已经解码的配对拒绝和版本拒绝保留 `ErrPairing` / `ErrVersion`，不会因随后底层断开退化成 `context.Canceled`。普通握手 I/O 失败同时保留底层终止原因，供 `errors.Is` 识别。

`Run` 的 context 管理整个连接生命周期。底层断开、context 取消、control stream 错误均终止会话；无重连。`OnMessage` 必须快速返回；`OnTunnel` 必须响应 context 取消并负责关闭 stream（Session 也在回调结束时关闭）。数据 stream 头限时 10 秒，缺失或无效的头只关闭该 stream。目标白名单和真实 TCP 拨号交由上层处理。

RTT 使用本地单调时钟计算，Pong 必须匹配待处理 Ping 的序号和时间戳；最多保留一个待处理探测。默认每秒检查并发送 Ping，不设独立心跳断开阈值。

`Options.AllowLinkTest` 默认为 false，只有显式开启才自动声明 `linktest/1`；手写 Capabilities 不绕过该开关。双方协商成功后，`TestLink(ctx, linktest.Options) (linktest.Result, error)` 才打开专用流；不支持返回 `linktest.ErrUnsupported`。测试使用空 Target/Origin 的既有 OpenTunnel 头，在 session 内终止，永不进入 `OnTunnel`。每方向一个并发槽，单次取消/超时/协议错误只关闭该测试流；服务端有独立硬截止时间。限额、错误和结果含义见 [linktest 协议](../linktest/README.md)。`NegotiatedCapabilities` 仍是调用方只读的能力列表，测试权限保存在内部，外部修改这个列表不能启用测试。

Client 通过 `Options.DigitalCode` 发起一次数字配对，Operator 通过 `DigitalManager` 声明能力。两端注入本机 `LocalFP`，传输提供内层 TLS exporter。数字配对优先于已信任快捷路径，缺少能力时结束本次连接。Operator 验证客户端标签后，通过尝试对象的 Commit 调用 `Remember(ctx, clientFP)`；落盘成功才发送 Operator 标签。Client 验证该标签后，成功返回的 Session 才报告 `PairingOutcome() == DigitalPaired`。其余成功连接报告 Legacy。数字拒绝用 `errors.As` 提取 `cpace.Reason`；本地缺少对端能力为 `session.ReasonUnsupported`，未知远端原因映射为 `session.ReasonInterrupted`。
