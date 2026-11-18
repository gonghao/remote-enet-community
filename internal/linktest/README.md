# 专用连接测试流

`linktest/1` 仅在两个已经配对的 renet 对端之间做有界 echo，不拨号车辆网关。双方 `session.Options.AllowLinkTest=true` 时自动声明 Hello capability，取交集后才允许 `Session.TestLink`。默认关闭；手写 `Capabilities` 不能绕过开关。

使用既有 `OpenTunnel` CBOR 头：`Kind="linktest/1"`，`Target=""`，`Origin=""`。session 在调用车辆/反向 `OnTunnel` 之前截获该 Kind；没有协商、携带地址或入站测试已占用时只关闭新流。普通 `OpenTunnel` 入口不接受这个 Kind，调用方必须使用 `TestLink`。CBOR schema、消息编号与版本不变。

```go
result, err := sess.TestLink(ctx, linktest.Options{})
if errors.Is(err, linktest.ErrUnsupported) {
    // The peer did not negotiate linktest/1.
}
```

## API 和资源预算

`Options.Normalize() (Options, error)` 在开流前验证参数；零值取默认值，负数或超限返回 `ErrOptions`。`Run(ctx, io.ReadWriteCloser, Options) (Result, error)` 与 `Serve(ctx, io.ReadWriteCloser) error` 拥有流，所有退出路径均关闭它。流必须支持并发 Close 解除阻塞 Read/Write，不要求 `net.Conn` deadline。取消回调在清理时收敛，不为每次阻塞读写另建 goroutine。

| 选项 | 默认 | 硬限额/语义 |
|---|---|---|
| `Samples` | 10 | 1–1024；每个 RTT probe 固定 32 字节 |
| `PayloadBytes` | 1 MiB | 每方向最多 8 MiB，包含 probe；至少 `Samples*32` |
| `FrameBytes` | 32 KiB | bulk 数据每帧 32–65527 字节；包含长度前缀、type/seq 的整帧不超过 64 KiB |
| `Timeout` | 10s | 1ms–15s；请求编码向上取整到毫秒 |

载荷预算是每方向的应用字节，echo 同一载荷还会在反方向传输一次；协议头、TLS/yamux 开销不计入载荷。每个 session 最多同时一个入站、一个出站测试，出站忙返回 `ErrBusy`，入站忙直接关闭额外流。服务端只存一帧，客户端只保留有界 probe 时长和两帧缓冲，不存整段载荷。

客户端时间预算包括开流和 OpenTunnel 头写入。服务端从 `Serve` 进入起最多 15s，包括等待请求、读帧和 echo 写入；收到请求后，以进入时刻加请求 Timeout 收紧截止时间，不能重置或延长预算。对端未发请求、只发部分帧、停发 probe、不读 echo，均能自行退出。会话关闭解除所有测试 I/O；取消单个测试仅关闭该流。

## 流内协议

所有整数使用大端。每帧为 `uint32 length || uint8 type || uint32 seq || payload`；length 不包含四字节前缀，合法范围 5–65532，超限在读正文前拒绝，不按不可信长度分配。

| type | 名称 | seq / payload |
|---|---|---|
| 1 | Request | seq=0；20 字节：`uint32 samples, uint64 payloadBytes, uint32 frameBytes, uint32 timeoutMillis` |
| 2 | Ready | seq=0；无 payload |
| 3 / 4 | Probe / ProbeEcho | seq 从 1 开始递增；每帧 32 字节，服务端原样 echo |
| 5 / 6 | Data / DataEcho | 延续 seq；每帧 `min(frameBytes, remaining)` 字节，原样 echo |
| 7 / 8 | Finish / Finished | 下一 seq；无 payload，确认所有预定载荷已处理 |

顺序固定为 Request/Ready → 指定次数的 Probe/ProbeEcho → 剩余预算的 Data/DataEcho → Finish/Finished。逐帧等待 echo，不流水线发送。请求各字段有独立边界检查；零样本、过小载荷、错误 type/seq/长度、echo 内容不符均为 `ErrProtocol`。服务端不解释数据内容，客户端用确定性数据验证完整 echo。

`ErrTransport` 包装底层 I/O 错误；完整解码后的协议违规返回 `ErrProtocol`；本地超时/取消保留 `context.DeadlineExceeded` / `context.Canceled`。远端直接关闭、未返回完整帧属于传输失败，不能由 EOF 推断其内部原因。部分结果可与错误同时返回。

## 结果解释

`Samples` 只计算完整、校验通过的 probe；为 0 时 RTT 未测量，不能把零值百分位显示成 0ms。`RequestedSamples` 保存本次目标；p50/p95 使用本地单调时钟和 nearest-rank，RTT 包含两端应用调度、分帧与传输时间。

`BytesSent/BytesReceived` 是已写入/读到的 probe 与 bulk 应用数据，包括失败帧中实际读写的部分；不含请求、确认与帧头。`EchoedBytes` 仅计通过完整性校验的 bulk 数据。`PayloadDuration` 从首个 bulk 发送开始，到最后一个 bulk echo 或该阶段失败为止，不含 probe 与最终确认。`EchoBytesPerSecond = EchoedBytes / PayloadDuration` 是串行往返 echo 的单份有效载荷速率；不把收发相加，也不是上行/下行链路容量。没有 bulk 数据时这两个字段为零。

`Duration` 包括整次调用清理时间；`TimedOut` 表示本地 deadline 到期，其他失败标记 `Interrupted`。测试在可靠的 TCP/复用流上工作，不能测得 IP 丢包率；它不检查车辆诊断条件，也不提供编码/刷写安全结论。
