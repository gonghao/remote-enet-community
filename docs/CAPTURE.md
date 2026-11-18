# Operator HSFZ 抓取

Operator 的 **Log → HSFZ capture** 提供实时帧索引及本地 JSONL 记录，默认关闭，不需要管理员权限。它记录应用层字节，不是 PCAP，不包含 IP/TCP 报文头、重传或抓包时间线。

## 使用

1. 启动 Operator 监听；可以在 Client 连接前开始录制。
2. 打开 Log → HSFZ capture，点击 Start capture，选择**不存在的新文件**。取消文件选择不会开始录制；旧文件、末级符号链接和目录均拒绝，不覆盖历史证据。
3. 重新连接诊断工具。仅新准入的 TCP 流接入录制，不在已有流中途猜测帧边界。列表显示最近 200 个索引，可按方向或连接 ID 筛选，选中查看 control、正文长度、源/目的诊断地址和原始前缀。
4. Stop capture 停止接收新记录，写完已接收队列并关闭文件。Operator 会话继续；再次录制要选择另一个新文件。结束 Runtime 或关闭应用也会等待记录器关闭。

Raw prefix 最多 256 字节，包含六字节 HSFZ 头；长帧明确显示前缀截断。Copy shown hex 只复制显示的前缀，完整已记录字节在 JSONL 的 `data` 事件中。最近列表不是完整历史，也不从旧 JSONL 导入。

一次录制可跨 Client 自动重连，各连接记录实际会话 `Generation`；重连不会重放请求。Runtime 结束即关闭录制，下次启动默认关闭。开关和路径不写入 Settings。

## 快捷标记

录制期间，Operator 每个页面顶部均有 **Insert marker**。全局快捷键为 macOS **Cmd+Shift+M**、Windows **Ctrl+Shift+M**，诊断软件处于前台时也可使用，不会切换前台窗口。快捷键仅在录制期间注册，停止、录制故障和退出时释放；被其它应用占用时会提示不可用，按钮仍可使用。

在 Capture 页的 Marker label 中可预填短注释，例如 `Before reading VIN`。下一次按钮或快捷键使用该注释，成功准入后清空；不填则使用 `Marker 1`、`Marker 2` 等自动编号，每份新录制重新计数。注释限单行、最多 256 个 UTF-8 字节，保存在本地 JSONL，不进入普通 Log 或匿名诊断报告。

先插入标记，再操作诊断软件。标记时间在处理按键／按钮回调时立即取得，与双向字节使用同一录制队列，不等待普通后台动作或磁盘写入。`Marker N queued` 表示已准入；Markers 计数及列表仅包含成功写入的标记。队列满会明确报错，不声称标记已保存；后续磁盘故障仍按抓取错误显示。

点击 **Frames after marker N** 可以与连接、方向筛选同时使用，显示首字节时间不早于该标记的最近帧；**Show all frame times** 清除此时间筛选。跨越标记的帧，其首字节可能早于标记，完整字节仍在 JSONL 中。标记不是协议边界，也不保证两个应用之间的执行因果关系。

CLI 在现有 Operator 参数上追加：

```sh
./bin/renetd operator --state-dir /tmp/remote-enet-demo/operator \
  --listen 127.0.0.1:47000 --bind 127.0.0.1 \
  --diag-port 26801 --ctrl-port 26811 --disc-port 26811 \
  --remote-diag-port 16801 --remote-ctrl-port 16811 \
  --capture-hsfz /tmp/remote-enet-demo/hsfz-new.jsonl
```

创建失败会在开始监听前返回错误。Ctrl+C 先等待 Runtime 的转发结束，再关闭抓取文件。此示例对应 README 的假网关台架，不连接真车。

## 覆盖与方向

| Network / Channel | 方向 | 字节含义 |
|---|---|---|
| `tcp / diagnostic`、`tcp / control` | `to_vehicle` | 工具到车辆：目标 stream.Write 成功返回的字节 |
| 同上 | `from_vehicle` | 车辆到工具：本地 socket.Write 成功返回的字节 |
| `tcp / mgu-query` | `to_vehicle`、`from_vehicle` | Operator 内部只读 MGU 地址查询实际写入/读取的字节 |
| `udp / discovery` | `to_operator` | 选定网卡的发现 socket 收到的数据报 |
| 同上 | `from_operator` | 本地缓存发现应答实际发送的字节，不代表本次又从车辆读取 |

UDP 每个数据报单独编号，收发两条不复用一个连接 ID。TCP 两个方向共享连接 ID，各自从 offset 0 计数。记录成功 Write 不等于远端车辆已接收或执行。单个帧可能横跨多个 `data` 事件，一块 `data` 也可能含多个帧。

不记录 Client/Hub 内部载荷解释、TLS/会话握手、邀请码、密钥、peer-only 连接测试或 MGU L3/SSH 数据。抓取可能包含 VIN、地址及原始诊断载荷，只保存在选定的本地文件；不会进入 Copy diagnostics 白名单报告、普通操作历史或控制台日志。

## JSONL schema v1

每行独立 JSON，顶层固定 `version:1`、`type`、`capture`（记录器内的录制代次）、`at`（RFC3339Nano）。同一文件内按写入顺序排列；连接开始/结束及 data 的时间在观察时取得，帧时间取首字节时间。录制终态使用排空处理时间。两个方向由并发转发观察，文件顺序不推断车辆执行因果关系。

| type | 内容 |
|---|---|
| `capture_start` | 开始录制 |
| `connection_start` | `connection`、`metadata` |
| `data` | `connection`、`metadata`、`direction`、`offset`、`data`（小写 hex） |
| `frame` | 上述连接/方向及 `frame` 索引对象 |
| `marker` | 全局注释，`marker` 对象包含 `Sequence`、`Generation`、`At`、`Label`；不属于某一 TCP 连接 |
| `connection_end` | 自然结束、固定 `code:connection_error`，或录制主动截止 `code:capture_stopped`；主动截止不表示工具 TCP 已断开 |
| `incomplete` | 存在丢录/限制/故障的固定 `code` |
| `capture_stop` | 正常收尾；不完整时附固定 `code` |

`offset` 为 0 时省略，读取方按 0 处理。`metadata` 字段沿用结构体名：`Generation`、`Network`、`Channel`、`Local`、`Peer`、`Target`。内部查询没有本地工具 socket，`Local` 为 `internal`。

`frame` 字段为 `Sequence`、`Connection`、`Generation`、`Offset`、`At`、`Network`、`Channel`、`Direction`、`Control`、`BodyLength`、`HasAddresses`、`Source`、`Destination`、`PrefixHex`、`PrefixTruncated`、`Status`。`Control` 是十进制 uint16，GUI 按十六进制显示。仅诊断/ACK 且已有两个正文地址字节时 `HasAddresses=true`；未知 control 和零正文均可保留。

标记事件的顶层 `at` 等于 `marker.At`。`Sequence` 是文件内标记序号，独立于帧序号；`Generation` 为插入时 Runtime 的最新代次，尚未连接时为 0。标记不改变原始字节 offset、载荷字节计数或 HSFZ 帧解析。最近标记列表使用与最近帧相同的条数预算，最多 200 条；全部已写入标记仍保留在文件中。

按 `(capture, connection, direction)` 分组 `data`，将 hex 解码后按 offset 拼接，可重建所有已记录的原始字节。offset 不连续就是缺口，不能把后续字节直接拼成完整流。检查 `incomplete`、终态和帧 `Status`，不能只看是否存在某一条完整帧。

## 限额与失败语义

默认文件预算 64 MiB、待处理队列预算 4 MiB、最多追踪 128 个连接、最近索引 200 条。库内硬最大值分别为 1 GiB、16 MiB、1024、200；GUI/CLI 使用默认值。队列按原始字节及事件开销计费，单块最多 16 KiB。JSONL hex 和索引存在编码开销，文件预算不等于可录制的载荷量。

观察回调只做有界复制及非阻塞入队，不做磁盘 I/O。队列/连接容量不足会丢录并增加 `DroppedEvents`，录制标为 `Incomplete`；不会因此中断健康转发。文件达到预算或发生写入/关闭错误时停止录制，界面保留固定错误码，会话继续。

增量帧器只保留头部和固定前缀，不按正文声明分配内存。`complete` 表示完整观察一帧；`partial` 表示截止时帧未完；`gap` 表示方向 offset 有缺口；`oversized` 表示正文声明超过 16 MiB。缺口或超大声明之后，该方向不再解码，也不扫描字节猜测下一帧；已接收的 raw data 仍可保留。

达到文件预算时仅写得下的完整 JSONL 行，并尽量预留终态空间；极小预算可能连终态都放不下，快照仍明确 `file_limit`。系统 Write 自身失败可能留下尾部半行，标为 `write_failed`，不会声称无损。文件未出现 `capture_stop` 时也可能是异常退出，不应视作完整录制。关闭不承诺断电持久性；同步文件系统调用不可硬取消，Stop 的等待 context 可以取消，后台收尾仍继续并可再次等待。
