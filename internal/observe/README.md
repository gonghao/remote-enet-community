# 有界本地指标

`observe` 只依赖标准库，不持有传输、UI 或车辆逻辑。一个 `Tracker` 跟踪一个逻辑对端的连续会话代次；所有方法、隧道句柄和 `Snapshot` 都可并发调用。

```go
tracker := observe.New(observe.Options{})
generation := tracker.BeginSession()
lease, err := tracker.BeginTunnel(generation, target, observe.Outbound)
if err != nil {
    return err
}
lease.AddIn(bytesFromRemote)
lease.AddOut(bytesToRemote)
lease.End(pipeErr)
tracker.AddRTT(generation, measuredRTT)
snapshot := tracker.Snapshot()
tracker.EndSession(generation, sessionErr)
```

`Inbound/Outbound` 表示谁发起隧道；`Bytes.In/Out` 始终按本机视角表示远端→本机 / 本机→远端，二者互不混淆。`End` 与 `EndSession` 幂等，结束后追加字节被忽略。`BeginSession` 自动结束旧代次的活动隧道、清空当前代次字节/RTT/速率，保留终身总量与有界历史。旧代次的结束回调、RTT 和事件不能污染新代次。

整合者在 Pipe 周围接线；该包不包装连接，不改变半关闭。若仅从 `tunnel.Stats` 读取结束计数，应在 `End` 前各提交一次总量；如需当前速率，应在转发期间提交增量，不能反复提交累计值。重连结束旧代次前，应先停止旧转发并提交最终计数。

| `Options` | 默认 | 硬上限 |
|---|---|---|
| `ActiveTunnels` | 256 | 4096，满时 `BeginTunnel` 返回 `ErrLimit` |
| `TunnelHistory` | 128 | 4096，覆盖最旧记录 |
| `RTTSamples` | 128 | 1024，滚动百分位 |
| `EventHistory` | 128 | 1024，覆盖最旧事件 |
| `RateWindow` | 1s | 100ms–1min，20 个固定桶 |

非正容量取默认，超过上限截断；目标、事件种类、消息与错误每字段最多保留 512 字节，并复制字符串，避免短切片保留大底层数据。活动连接、历史、RTT、事件、最近错误与速率桶都有界。调用方保存的句柄/快照内存由调用方管理。

`Snapshot` 在一个锁内取一致视图，按 ID 排序活动隧道，历史/事件从旧到新。切片、最近错误指针均独立复制；修改或保存旧快照不会影响 Tracker 或其他快照。`Session.Bytes` 仅含当前代次，`Totals` 包含所有代次。

RTT 方向固定为本机→对端→本机，`Samples` 是滚动窗口有效样本数，`TotalSamples` 是当前代次累计有效数。零样本时百分位未测量；不能显示为真实 0ms。非负样本均有效，允许时钟分辨率下实际测得的零；负值或旧代次样本被忽略。p50/p95 使用 nearest-rank。

速率按最近 20 桶内的字节增量除以 `Rate.Window` 计算；首个窗口使用实际经过时间，后续使用配置窗口。它是有桶粒度误差的当前速率，无流量超过一个窗口后归零，不是会话平均吞吐。`LastError` 独立于事件环保留最近错误，可跨代次展示；`RecordEvent` 只保留错误文本，不持有调用方错误对象。

`RecordEvent` 接受当前最新代次的事件，即使该代次尚未开始或已经结束，用于记录等待车辆、退避和结束后的运行时错误。字节与 RTT 在结束时冻结；事件环与 `LastError` 仍可追加。开始下一代次后，旧代次事件被拒绝，不会污染新的会话。
