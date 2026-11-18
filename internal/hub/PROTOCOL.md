# Hub 外层协议 v1

Hub 只根据本文件的路由头建立路径。路由完成后的字节属于端到端加密层，Hub 不解析、记录或识别其内容。`hub` 和 `transport/hub` 均不依赖 `secure`。

## 外层 TLS

- TCP 上运行 TLS 1.3，ALPN 必须为 `renet-hub/1`，不接受缺省 ALPN 或 TLS 1.2。
- 双方各出示一张 Ed25519 自签设备证书。检查有效期、Subject/Issuer 一致、自签名和未知关键扩展；不使用公网 CA、DNS 名或客户端自报 ID。
- 设备 ID 是证书 Ed25519 公钥的 SHA-256，由 `identity.FingerprintOfCert` 计算。客户端必须钉扎 `Endpoint.Hub`，零指纹不允许连接。
- Hub 从 TLS 客户端证书取得设备 ID。路由头不含发送方 ID，无法通过修改头部冒充其他 wait 方。
- TCP 建立、TLS 握手、头部和状态交换各连接共用最多 10 秒的外层 setup 窗口；调用方 context 可更早取消。成功后清除 setup deadline，context 的后续生命周期由调用方或 transport 管理。

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

wait 的注册键只能取自设备证书指纹，同一键最多一个注册。wait 收到 ok 后可长期空闲，但在 dial 配对前必须保持应用字节静默，否则被关闭。此约束只涉及发送时序，Hub 不检查字节内容。端到端 TLS 的 server 端首先等待读取，满足此约束。

dial 原子占有目标 wait，随后收到 ok。之后双方直接传递裸字节，没有二次消息或长度帧。wait 的 ok 只表示注册，不表示已有 dial，等待方从首批字节进入内层握手。wait 端不会从 Hub 得知 dial 的指纹，由注入的内层 Wrap 验证身份。

一个 wait 同时只接受一个 dial；第二个 dial 返回 busy，不排队、不抢占。配对中的任一方向 EOF/错误导致两侧关闭，取消服务也会关闭全部连接。Hub 不保留 TCP 半关闭语义，也不复用已用过的 wait 流；会话结束后等待方必须重新注册。

Hub 用同一把锁决定注册和配对，wait 的 socket 只有一个读取者，配对后经有界管道传递到 dial；反向直接拷贝。每条配对的拷贝协程在返回前被回收。不存在按流量增长的等待缓冲区。

## Embedded 交接

启用 Embedded 且 `dial.target == Hub.Local.Fingerprint()` 时，连接不查 wait 表，直接交给 `Embedded.Accept`。它返回剥离路由头的连接和来自证书的设备指纹。Hub 仍拥有服务关闭时的强制关闭权；调用方须在拒绝、握手失败、会话终止时关闭返回的连接。

Embedded 使用一个活动槽，reserve、publish 和领取均在同一把锁下完成。过期连接的晚到 publish 不能覆盖新占位。未领取的连接最多占用 10 秒；领取后由 transport 负责内层 setup 期限和会话生命周期。服务停止立即关闭包括未领取连接在内的所有 socket。

`Authorize` 应用于所有请求。`AuthorizeToken` 可为独立 Hub 校验 token；内嵌 self-dial 忽略 token，但仍执行 `Authorize`。两个回调为 nil 时不限制身份。回调可能并发执行，应快速返回，不能等待网络或服务关闭事件。

## transport 生命周期与接线

`hubtransport.Register(wrap)` 在应用启动时注册 `"hub"`，只调用一次；显式注入解决基础层 `Config.Options` 只能传字符串、不能传函数的问题。注册表工厂接受 `listen`，较复杂配置直接调用 `New(Options)`。

- `Options.Endpoints` 是 Acceptor 的独立 Hub wait 候选；`Options.Token` 是这些 Hub 的可选鉴权材料。候选和 token 在 New 时拷贝。
- `Options.Listen` 创建并拥有内嵌 Hub listener；`Options.Embedded` 可从外部服务器接收连接。两者同时提供时使用同一个 Embedded。提供 Embedded 时 Acceptor 使用它，不同时注册 Endpoints。
- Connector 并发尝试 PeerInfo 的所有 hub 候选；完整 Wrap 首个成功者获胜。返回前取消并回收其他候选，关闭其连接和已生成的 Session。全部失败时用 `errors.Join` 保留可识别的底层原因。
- 内嵌路径以 `Endpoint.Hub == PeerInfo.ID` 识别，Path 为 direct；其他路径为 relay 且 Via 为 Hub 地址。外层钉扎与内层期望身份分别校验，不能用路径分类代替认证。
- Dial 与 Embedded.Accept 的内层 setup 最多 10 秒，使用 socket deadline 加计时取消。独立 wait 空闲时不启用内层计时；首个读到或写出的字节启动 10 秒窗口。成功后停止计时、清除 deadline，传给 Wrap 的长寿命 context 保留到 Session 关闭或调用方取消。
- Wrap 必须遵守 context、进行内层身份校验，并负责 Session 结束时关闭连接。非零 expect 必须严格校验，独立 wait 的 expect 为零，由内层返回实际认证身份。忽略 context 且不进行 socket I/O 的 Wrap 无法被本包强制终止。
- Acceptor 的 Accept 调用串行化；活动 Session 结束前不会开始下一次接收。Close 取消待处理 Accept、内层 setup、已接受 Session 和自有 listener。多个独立 Hub 之间没有全局账号锁，单会话边界是本 Acceptor/每个 Hub wait 注册。

## 本轮边界

无自动重连、退避、服务发现、全局 Hub 配额、流量限速、证书吊销、token 获取或公网互通测试。static 目录仅保存内存快照，本轮未增加可选文件持久化。Hub 只验证传输身份，首次配对授权由上层控制协议处理。
