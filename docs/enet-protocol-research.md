# BMW ENET 协议与工具行为调研

调研日期：2026-11-15。范围：`docs/research/M0-brief.md` 的 Q1–Q9，以及 `docs/DESIGN.md` 全部 `⚠`。本报告只研究协议、发现与连接行为，不包含编码、刷写或开启调试服务的操作步骤。

## 结论与证据边界

**M1 的 HSFZ 字节透明转发已有足够协议依据；M2 的三工具自动发现仍须在目标 Windows VM 上验证；M4 的 HTTP/IP 镜像尚不能按确定方案实现。** 可确认的修正有：

- HSFZ 基础头是 **6 字节**；只有部分类型后跟 `src/dst`，不能所有帧都按 8 字节头解析。
- 发现标记是 `DIAGADR`，不是 `DIAGADDR`。已知 `0x0011` 发现正文没有 IP，地址来自 UDP 源地址。
- `0x0002` 是传输 ACK；正常诊断响应仍是 `0x0001`。ACK 保留请求的地址方向，并可能只回显前 5 字节 UDS。
- 网关发送 `0x0012` alive check，tester 回 `0x0013`；本地 PCAP 和 EdiabasLib 一致。rawenet 对此的文字说明不可靠。
- `DF` 是本批车辆的功能寻址地址，不能当作物理网关地址；发现样本给出的网关诊断地址是 `10`。
- **6811 不仅用于 UDP 发现，还存在 TCP 控制通道**。只转发 `6801/tcp + 6811/udp` 不足以宣称完整 EDIABAS 兼容。
- SLP 是 `427/udp`、组播 `239.255.255.253`；ICOM 的 `50160/tcp` 是诊断端口，不是 SLP 端口。
- E-Sys HTTP server 默认 `8888`，但可配置；公开 ISTA/PSdZ 客户端代码会从端口范围内选空闲端口，不能统一写死 `8888`。
- remote-enet 的 HTTP 模式开关，页面明确说明是**决定哪个 tester 获得 link-local alias 发现地址**；不能解释成仅切换反向转发的本地端口。

以上依据详见对应 Q 节。未解决的关键项：真实 ZGW 的连接上限、tester 地址隔离、空闲回收定时器、目标工具版本的本机发现兼容性，以及 HTTP 回连 URL/IP 的具体承载方式与地址选择规则。

### 证据使用方法

置信度：**高**表示源码与/或可复核字节直接支持所述事实；**中**表示有实现或文档支持，但未覆盖目标工具/车辆版本；**低**表示仅有未经交叉验证的行为声明。**未知**表示当前证据无法回答，不用通用 UDS/DoIP 常识填补 BMW 实现细节。

优先采用实现源码，再看原始 PCAP，最后用文档/经验补充。源码里的常量、注释、未实现方法和测试桩分别对待；“库支持”不等于“所有 BMW 网关或原版工具都支持”。

本报告的源码引用缩写如下；例如 `E:4940–4952` 表示下表所列文件的对应行，均以本次 checkout 为准。公开来源同时提供固定 commit 链接，便于脱离本机复核。

| 缩写 | 本地路径 / 公开来源 |
|---|---|
| `E` | `docs/research/scratch/ediabaslib/EdiabasLib/EdiabasLib/EdInterfaceEnet.cs`；[固定版本][e] |
| `EC` | `docs/research/scratch/ediabaslib/docs/EdiabasLib.config_file.md`；[配置文档][ec] |
| `X/<file>` | `docs/research/scratch/ediabaslib/Tools/Psdz/PsdzClientLibrary/BMW/Rheingold/xVM/<file>`；[公开代码目录][x] |
| `R/<file>` | `docs/research/scratch/rawenet/<file>`；[固定版本][r] |
| `S/hsfz.py`、`S/doip.py` | `docs/research/scratch/scapy/scapy/contrib/automotive/bmw/hsfz.py`、`.../automotive/doip.py`；[HSFZ][sh] / [DoIP][sd] |
| `M/<file>` | `/home/example/src/mgu_kit/mgu-go/internal/mgudiag/<file>` |
| `C/<file>#N` | `/home/example/src/mgu_kit/hsfz_captures/<file>` 的原始 PCAP 包号 `N`；不是重组后的消息序号 |
| `A` | `docs/research/scratch/capture-audit.md`，本次生成的拓扑、ACK 和控制帧复核；`复核结果`（私有研究附件，未随公开快照分发） |

固定版本：EdiabasLib `390e5944fb12d4477e45e46a5524d7f9fb043732`（2026-11-14）；rawenet `1422c25c02a245cf532e8a26ed70f619c107b6b1`（2026-06-11）；Scapy `0e2f6ba2669dc9c6487d7c0a4220b3c3dfd6d4ef`（2026-11-13）。它们均克隆于 `docs/research/scratch/`，没有运行其车辆访问逻辑。

注意：EdiabasLib 的 `Tools/Psdz/PsdzClientLibrary` 包含整理/修改过的 BMW/ISTA 客户端代码，不是已验证对应本机 ISTA 版本的官方源码发行版，部分方法标有 `PreserveSource` 且抛 `NotImplementedException`。引用它可确认公开代码的行为，不能据此保证所有 ISTA 版本一致。`M` 的代码也只能证明现有实现；本次没有连接车辆。

## Q1：HSFZ 发现，UDP 6811

### Q1.1 请求和响应布局

**结论：**请求为 `00 00 00 00 00 11`：`L=0`、控制字 `0x0011`、空正文。响应也用 `0x0011`，`L` 是 **6 字节头之后的正文长度**，不是含头总长。发现正文为相邻的 ASCII 标记与值，不是带 `=`、分隔符或长度前缀的通用键值协议。已知样本没有 IP 字段。

**置信度：高（请求、已知响应布局）；中（跨网关版本的扩展字段）。**

**证据：**`E:748–751,3353–3380`；`R/pkg/hsfz/protocol.go:29–41,53–89`；`R/pkg/hsfz/protocol_test.go:9–28`；`M/discovery.go:14–15,77–89`。EdiabasLib 从收到数据报的 `recIp` 构造连接，ISTA 公开 HSFZ 解析器亦取 `remoteEP.Address`（`X/ENET/VCIEnetHsfzService.cs:25–47`）。

可直接回放的 **56 字节 UDP payload**：

```text
00 00 00 32 00 11
44 49 41 47 41 44 52 31 30
42 4D 57 4D 41 43 41 41 42 42 43 43 44 44 45 45 46 46
42 4D 57 56 49 4E 57 42 41 58 58 58 58 58 58 58 30 30 30 30 30 30 31
```

来源：`R/pkg/hsfz/protocol_test.go:9–10`（MIT 仓库），作者称来自 F 系抓包；MAC/VIN 明显经过替换。**这是作者发布的脱敏 fixture，不是本次独立捕获的原车响应。** 本次用 Python 核对实际长度 56、`L=50`、MAC 12 字符、VIN 17 字符；原有 Go fixture 测试亦通过。

| 从 UDP payload 起算的偏移 | 字节数 | 本例内容 / 编码 |
|---|---:|---|
| `0–3` | 4 | `00000032`，大端正文长度 50 |
| `4–5` | 2 | `0011`，大端控制字 |
| `6–12` | 7 | ASCII `DIAGADR` |
| `13–14` | 2 | ASCII `10`，十六进制逻辑诊断地址；不是二进制 `0x10` |
| `15–20` | 6 | ASCII `BMWMAC` |
| `21–32` | 12 | ASCII `AABBCCDDEEFF`，无冒号的十六进制 MAC |
| `33–38` | 6 | ASCII `BMWVIN` |
| `39–55` | 17 | ASCII `WBAXXXXXXX0000001`，无末尾 NUL |

**仍需实测：**F20 的原始发现响应、其他 `DIAGADR` 值、NUL/字段顺序/扩展字段的变体。rawenet 只硬编码验证 `DIAGADR10`，并没有实现“任意 DIAGADDR 字段解析”；不要直接把这部分当作通用解析器。缓存原始字节的 Presenter 对上述已知格式无需改写 IP；“所有未来扩展也无 IP”仍是未知。

### Q1.2 广播、单播、响应端口

**结论：**常见客户端使用广播；也存在直接向已知 IP 发送探测的实现。已知客户端从发送 socket 的临时端口接收响应，Responder 应向请求的源 `IP:port` 单播回复，不能固定回复到 tester 的 6811。

**置信度：高（客户端和 rawenet responder 行为）；中（真实网关对所有单播/广播变体的兼容性）。**

**证据：**`M/discovery.go:50–60,69–81,94–115` 发全局、link-local 和各网卡定向广播，绑定 `0.0.0.0:0`；rawenet 绑定 ENET IP 的临时端口，发 `169.254.255.255:6811`（`R/pkg/hsfz/discovery.go:43–70`），服务端绑定 `0.0.0.0:6811`，回 `remoteAddr`（同文件 `75–78,96–116`）。`E:2882–2896,2936–2940,3190–3201` 支持指定 IP 单播，`E:3140–3170` 枚举定向广播。

**仍需实测：**F20 是否接受单播、是否始终从 6811 回复、有无广播回复变体，以及 Windows 同机工具发出的广播是否能到达本机 Presenter。任务所需的 E-Sys 自身精确目的地址/收包端口，不能由这些其他客户端实现代证，见 Q4.2。

## Q2：HSFZ TCP 帧、ACK 与保活

### Q2.1 帧结构与已知控制字

**结论：**统一外层为 `[uint32_be L][uint16_be control][body L bytes]`，总长 `6+L`。`0x0001/0x0002` 的正文才通常是 `[src:1][dst:1][UDS/echo...]`；`0x0011`、长 alive check、空错误帧不能强塞 `src/dst`。

**置信度：高。证据：**`E:4917–4948,5294–5304`；`S/hsfz.py:41–93,150–152`；本批重组结果 `A:191–208`。控制字的名称以 Scapy 枚举为起点，再用实际收发行为限定含义：

| 控制字 | 已知含义 | 正文/注意点 |
|---|---|---|
| `0001` | Diagnostic request / response | 两个地址字节后是 UDS；正响应与负响应均在此类型内 |
| `0002` | Acknowledge transfer | 请求方向的地址 + 请求前缀；不是 ECU 的执行结果 |
| `0010` | Terminal 15 查询/状态 | 存在 TCP 控制通道查询；不是普通 diag 帧 |
| `0011` | Vehicle identification data | 空正文请求；ASCII 标识响应 |
| `0012` | Alive check | 实际网关→tester；本地样本 `L=32`、正文为 ASCII 标识；Scapy 也支持 `L=2` 变体 |
| `0013` | Status data inquiry；在 6801 用于 alive response | EdiabasLib 与实际 Tool32 回 `L=2, body=00 F4`；不同通道的状态查询语义需区分 |
| `0040` | Incorrect tester address | Scapy 解析 expected / received 两字节；不是 UDS NRC |
| `0041` | Incorrect control word | 本地实见 `L=0` |
| `0042` | Incorrect format | 仅确认公开 dissector 的命名 |
| `0043` | Incorrect destination address | 同上，不能自动等同于任意 ECU 超时 |
| `0044` | Message too large | 同上 |
| `0045` | Diagnostic application not ready | 同上 |
| `00FF` | Out of memory / 客户端称 NACK | EdiabasLib 对 6 字节空正文该帧有重试处理，不是所有 reject 的总称 |

**证据：**[Scapy 枚举与条件字段][sh] `42–93`；`E:4940–4952,5336–5365`；`X/ENET/VCIEnetHsfzService.cs:50–56`。没有找到另外一个可通用称为 “ping” 的 HSFZ 控制字；不要自行分配。`3E` 是 UDS TesterPresent 服务，不是 HSFZ 控制字。

**仍需实测：**未捕获的错误类型正文、`0012` 短格式、控制通道 `0013` 请求/响应在目标 ZGW 上的行为。上表是本次找到的已知类型，不是 BMW 官方穷举规范。

### Q2.2 ACK 规则

**结论：**对于被接收处理的 tester diag 请求，客户端预期一个 HSFZ ACK；功能寻址请求也只需传输 ACK，随后可有多个 ECU 的 `0001` 响应。ACK 的 `src/dst` **不交换**，正文为请求正文前缀，不能把请求整个改成 `0002` 后原长回显大数据。

**置信度：高（正常路径）；未知（所有异常路径是否必有 ACK）。**

**证据：**`E:5310–5365` 等待 ACK、限制总长不超过 13，并将控制字临时改回后比较请求前缀。全部 11 份 PCAP 重组去重后有 **597 个 tester diag 请求、594 个 ACK**，所有 594 个 ACK 都能匹配同连接之前请求的正文前缀，`L=3–7`（即 UDS 回显最多 5 字节）；统计见 `A:46,64,82,101,119,140,157,176,193`。三个未配对请求出现在无应答/中断窗口，不能宣称“每个请求必回 ACK”。

例：`00 00 00 05 00 01 F4 63 22 F1 01` 的 ACK 为 `00 00 00 05 00 02 F4 63 22 F1 01`；后续 ECU 响应才是 `... 00 01 63 F4 62 F1 01 ...`。这是传输方向和类型示例，不是执行步骤。

**仍需实测：**F20 的 malformed/错误地址/应用未就绪路径如何应答，是否 ACK 后再报错；不能用本批代理后的成功 ACK 数推定网关的所有实现。

### Q2.3 Alive check、超时与空闲断开

**结论：**保活必须端到端透传；隧道自身的 Ping/Pong 与 OS TCP keepalive 都不能替代 HSFZ alive response，也不能保持 ECU 的 UDS 会话。**统一的网关 alive 周期、等待超时、空闲断开阈值：未知。**

**置信度：高（方向、应答格式、各层保活的区别）；低（从单批代理抓包推断网关定时器）。**

**证据：**`E:4940–4952` 收到 `0012` 后回精确字节 `00 00 00 02 00 13 00 <TesterAddress>`。`C/hsfz_tool32_20261031_010011.pcap`：

| 包号 | 相对时间 / 方向 | 事实 |
|---|---|---|
| `#120` | `53.849412 s`，远端→tester | `L=32,0012`，正文 `DIAGADR10BMWVIN...`；未见 `0013` |
| `#194 → #196` | `67.133067 → 67.133773 s` | `0012 → 0013`，应答间隔约 `0.706 ms` |
| `#464 → #466` | `121.008463 → 121.009188 s` | 再次 `0012 → 0013`，约 `0.725 ms` |
| `#468` | `121.108492 s`，远端→tester | `L=0,0041`；其后有断连，原因不能仅归于 idle 或 alive |

详见 `A:203–208`。这三次 `0012` 分布在**三条不同连接**，不能相减当作保活周期；它们距离上个 tester diag 分别约 `1.072 s`、`0.177 s`、`20.147 s`，也不能总结出固定的“空闲 N 秒”。

rawenet 的文档 `R/docs/hsfz-protocol.md:59–77` 把 `0012` 解释为 tester 发起、网关回 `0002`，与上述字节证据冲突。其 `AliveCheckPacket` 只是常量测试；实际 `R/pkg/hsfz/alive.go:9–19` 的 `CheckAlive` **仅做 TCP connect 后关闭**，没有证明那组 alive 字节能工作。EdiabasLib 的 `TcpControlTimer.Change(2000,...)`（`E:3890–3911`）是客户端关闭临时控制连接的计时器，也不是 ZGW 6801 空闲超时。

**仍需实测：**车侧直连、空闲与周期只读流量、正确回应/故意不回应 `0012`，分别观察谁发 FIN/RST 以及等待时长。需要记录网关型号、软件版本、供电/休眠状态；代理后的长连接不能代替该实验。

**2026-11-16 补充观察：**目标 Windows VM 的 E-Sys 3.36.2 在假网关长 `0012` fixture 后发送了 `00 00 00 02 00 12 00 F4`，并非上述 `0013`。该观察尚未用真网关或其他 fixture 对照，不能据此统一改写控制字规则；详见 [M0b 实测记录](research/m0b-win11-2026-11-16.md)。隧道继续原样透传，假网关严格等待 `0013` 是本轮测试边界。

### Q2.4 与 mgu_kit 常量、解析器的差异

| `M/hsfz.go` 现状 | 调研结论 / 使用限制 |
|---|---|
| `typeDiag=1`、`typeEcho=2`，行 `23–24` | 数值正确；`Echo` 应理解为 transport ACK，可截断，不能视为诊断响应 |
| `typeReject=255`，行 `25` | `00FF` 只是 out-of-memory/NACK；缺 `0040–0045`，不能代表全部 reject |
| `addrTester=F4`，行 `19` | 本批确实使用 `F4`，不表示只能用它，也不表示 `F5` 已验证可独立并发 |
| `addrGateway=DF`，行 `21` | 在本批证据中 `DF` 为功能地址；物理网关标识为 `10`。依赖 `DF` 的功能广播不能简单替换成 `10` |
| `addrMGU=63`，行 `20` | 本批 MGU 地址正确；不是跨所有平台的硬性协议常量 |
| `totalLength` 拒绝 `L<2`，行 `71–80` | 会挡住合法空正文控制/错误帧；例如本地 `0041,L=0`。不适合作为通用 HSFZ 外层解码器 |
| `parseFrame` 总取 `data[6:8]`，行 `83–93` | 不适用于 ASCII 发现/长 alive。缺 `0012→0013` 的专门接收处理 |

**置信度：高。证据：**上述具体行、Q2.1–Q2.3 的源码与 PCAP。**仍需实测：**长连接使用该客户端的稳定性。结论是复用时要扩大帧模型，不能宣称现有编解码器已经完整覆盖 HSFZ；本任务未改其代码。

### Q2.5 6811 的 TCP 控制用途

**结论：**除了 UDP 6811 发现，还有到 `ControlPort` 的 TCP 通道，公开客户端在此发 `00 00 00 00 00 10` 查询 KL15，接收 7 字节响应，用正文位判断点火状态。远程兼容性设计必须保留转发 `6811/tcp` 的可能性。

**置信度：高（存在实现）；中（目标 Tool32/INPA 的具体调用路径）。证据：**`E:766,1568–1596,3916–3948`，其中 `TcpControlClient` 是 TCP 客户端；`S/hsfz.py:45` 及 [XEnet32 分析][xd] 的双通道架构为辅助证据。

**仍需实测：**目标 EDIABAS 版本什么时候调用控制通道、实际 ZGW 的 KL15 位含义与响应。当前 EdiabasLib 的电压/点火逻辑还包含推断/固定值路径，不能把该库展示的电压当作车端实测电压；见 `E:770–775,814–815,1613` 附近注释。

## Q3：ZGW 并发连接与独立轮询

### Q3.1 能确认什么，不能确认什么

**结论：**不能从这些资料得出 F20 ZGW 的最大 6801 连接数，也不能确认“另一连接 + F5 一定不干扰 E-Sys”。**连接容量、同/不同 tester 地址的路由隔离、ECU 应用会话是否互扰，是三个不同问题。** `0040 incorrect_tester_address` 表明 tester 地址有校验语义，但不能反推出一个地址对应一条完全独立的 ECU 会话。

**置信度：高（证据边界）；未知（目标硬件容量与隔离规则）。**

**证据：**`S/hsfz.py:49,83–86`；EdiabasLib 允许配置 `EnetTesterAddress`（`E:795,923–926`）；原版 EDIABAS 示例含 `TesterAddress=F4,F5`，见 Q6。它们只证明字段/配置存在。rawenet 的并发断言（`R/docs/hsfz-protocol.md:79–85`）没有网关版本、原始连接抓包或隔离测试；其代理主循环还同步处理连接（`R/pkg/proxy/tcp.go:80–96`），不能用它证明真网关并发能力。XEnet32 分析所说“32 channels”是**驱动 channel 数组**，不能当成 ZGW 允许 32 条 TCP。

**仍需实测：**F20 上，先保持 E-Sys 的只读连接，再增加同 `F4` 与不同 tester 地址的只读请求；检查应答是否串流、连接被替换、返回 `0040`、既有读取受影响。不要在生产诊断会话里假设安全隔离。对当前设计的建议：外部诊断活动期间默认暂停自主车况轮询；只有目标网关上验证过的只读组合才允许同时开启。PAD 的切换属于状态改变，不能按只读轮询的结果外推。

### Q3.2 全部本地 PCAP 的连接统计

**结论：**本次统计 **11 份 PCAP，2 份空包，4,009 个网络包，2,495 个去重 HSFZ 帧**。所有有内容文件中只见通往公网代理 `43.129.66.216:4048` 的 TCP；没有 UDP 发现、直连 ECU IP/端口或 HTTP。文件名可提示工具来源，PCAP 本身没有 PID，不能验证进程归属。每个有效 E-Sys 窗口只有 **1 条承载 HSFZ 数据的连接**，ID8 `141154` 文件则先后有 2 条；额外短 TCP 连接大多无 payload。

**置信度：高（捕获窗口内）；未知（代理后车侧拓扑及窗口外行为）。**原始抓包使用的捕获过滤表达式未知；“未捕获某类流量”不能解释为工具从未发送它。

下表文件均在 `C/`；全名列在 `A`。`SYN` 是按 TCP 初始序号去重的新建尝试，不把 SYN 重传重复计数；“旧片段”指窗口开始时已存在连接的收尾。所有有 HSFZ 的连接，其 tester `src` 均为 `F4`；无 payload 连接的 tester 地址未知。

| PCAP 名称去掉 `hsfz_` 和 `.pcap` | tester IP | 本地端口（均→远端 `:4048`） | 新 SYN / 旧片段 | 有 HSFZ 的连接 | 同时已建立峰值下界 |
|---|---|---|---:|---:|---:|
| `esys_20261031_002635` | — | — | `0/0` | 0 | 0 |
| `esys_20261031_003030` | — | — | `0/0` | 0 | 0 |
| `esys_20261031_003807` | `192.0.2.191` | `51296,51298` | `2/0` | 1 | 1 |
| `esys_20261031_003948` | `192.0.2.191` | `51804,51806` | `2/0` | 1 | 1 |
| `esys_20261031_004555` | `192.0.2.191` | `51806` 旧；`52606,52608` 新 | `2/1` | 1 | 1 |
| `esys_20261031_004623` | `192.0.2.191` | `52744,52747` | `2/0` | 1 | 2 |
| `esys_id8_20261102_141154` | `198.51.100.149` | `58999` 旧；`59028,59029,59042,59043` 新 | `4/1` | 2（先后） | 1 |
| `esys_id8_write_ncd_20261102_141846` | `198.51.100.149` | `60196` | `1/0` | 1 | 1 |
| `esys_write_vin_20261031_011615` | `192.0.2.191` | `55971,55973,55979` | `3/0` | 1 | 1 |
| `tool32_20261031_004843` | `192.0.2.191` | `53067` | `1/0` | 1 | 1 |
| `tool32_20261031_010011` | `192.0.2.191` | `54185,54315,54397` | `3/0` | 3（先后） | 1 |

**证据：**`A:38–208`，含每条连接的 SYN、SYN/ACK、FIN/RST 包号和 SHA-256；重组器对全部文件均未报缺段、截断或冲突。比如 `004623 #1/#7` 是 `52744` 的握手、`#9/#10` 已关闭且从未发 payload；`52747` 从 `#2/#3` 开始并承载诊断。故峰值 2 是**对公网代理的短暂并行连接**，不能说“ZGW 已证实能跑两套 UDS 会话”。`M` 的旧中文总结只统计早期 9 份，不能把其 2,988 个包当作当前目录全量。

**仍需实测：**E-Sys 真正一次完整连接生命周期的全端口车侧抓包，特别是 Ethernet ECU 直接连接。不能把本表的 `:4048` 改写成 `:6801` 后当作真实车侧统计。

## Q4：E-Sys 连接方式与 HTTP

### Q4.1 连接方式与非 link-local 网关

**结论：**公开 E-Sys 手册能确认 Gateway URL、按 VIN 自动发现、ICOM/D-CAN、ICOM/Ethernet、bus/direct 等模式；不能确认设计稿所写的独立 “WiFi ENET” 单选项是目标 E-Sys 版本的功能。**“能通过 Wi-Fi 使用 ENET”与“有一个叫 WiFi ENET 的模式并接受本机 LAN 代理”不能混为一谈。**

**置信度：高（手册所列模式）；低（rawenet 对特定软件兼容性的声称）。**

| 模式 | 可确认的发现/地址要求 |
|---|---|
| Gateway URL | 手工 `tcp://host:port`；HSFZ 示例 `6801`，不依赖 VIN 广播发现。手工 URL 不等于 HTTP 回连地址正确 |
| Connection via VIN / Ethernet | 广播搜车并列 VIN；手册描述 tester 与网关处于同子网，无 DHCP 时回退 APIPA |
| ICOM/Ethernet | tester 经 TCP 到 ICOM，端口映射；网关 `0x10` 对应 `50000 + 10×16 = 50160` |
| ICOM/D-CAN | 与 ICOM 建 TCP，再走其 CAN 通道；不同于裸 ZGW HSFZ 的整个访问路径 |
| bus/direct | 支持独立 ECU 的直接接口；`SYSTEMBUS_ETHERNET + MEDIA_CONVERTER` 使用 URL。它与正常车辆连接中自动另开 Ethernet ECU 连接是两件事 |
| “WiFi ENET” | rawenet README 有该术语；官方模式名、目标版本是否提供、非 link-local 自动发现是否接受：**未知** |

**证据：**下载的公开镜像 `docs/research/scratch/E-Sys_Manual_V23_10_ENG.txt:4485–4523`（PDF 第 231–232 页）；[E-Sys 24.06 手册镜像][es24] §5.2–5.3、p.63–66。`R/README.md:56–71` 声称 E-Sys/ISTA 可用，但 `R/docs/testing.md:5–8,38–47` 只写未具名的 “third-party app”，无版本号、工具截图或原始 PCAP，不能认定这个测试就是 E-Sys 或 ISTA。

**仍需实测：**目标 E-Sys/E-SysX 版本 + 假 ZGW，在本机 VM LAN 地址与 link-local alias 上分别发现和建立连接。先记录实际 UI 选项名称。若非 link-local 自动发现失败，M1 的手工 URL 仍有独立验证价值，不能把 M1 成功当作 M2 成功。

**实测更新（2026-11-16，M0b）：**在 Windows 11 VM 的普通 LAN 地址上（未添加 `169.254/16` 别名），E-Sys 3.36.2 的 `Open Connection` 与 ISTA 4.39.20 均自动发现了模拟车辆，非 link-local 自动发现对这两个版本成立；Tool32 的 LAN `Autodetect` 初始化失败（`NET-0012`），显式 IP 可读取 VIN。详见 [M0b 记录](research/m0b-win11-2026-11-16.md)。2026-11-18 F20 实车上，E-Sys 与 ISTA 经隧道完成只读识别，见 [F20 只读验收](research/f20-readonly-2026-11-18.md)。上表“未知”一格保留调研时的原始结论；其他版本仍需各自实测。

### Q4.2 E-Sys 自身的发现包

**结论：**手册确认刷新连接列表时广播搜 VIN；HSFZ 生态普遍使用 Q1 的 `0011` 请求。**目标 E-Sys 实际请求字节、完整广播目的地址集合、接收 socket 端口：未知，尚无其发现阶段原始 PCAP。**其他开源客户端使用临时端口，不足以证明 E-Sys 也固定如此。

**置信度：高（未捕获边界）；中（HSFZ 发现格式的预期）。证据：**[手册][es24] §5.3.1；本地全量 PCAP 仅 IP protocol `6`，见 `A`；Q1 的请求实现是 EdiabasLib/rawenet/mgu_kit，不能冒名为 E-Sys 抓包。

**仍需实测：**无需车，在假 ZGW 同机部署下捕获所有 UDP、记录请求的源/目的地址和端口，验证响应源地址确实成为 TCP 目的地址。还需测试指定单播 alias 的响应、无 MAC 变体、同 VIN 多响应的处理。无需向公网转发广播。

### Q4.3 Ethernet ECU 的 IP 与直接端口

**结论：**MGU 可经网关用 DID `172A` 读取 IPv4 配置，见 Q8；已知 `0011` 发现帧不携带 MGU/RAM 的 IP。商业工具作者明确描述 E-Sys 会另开连接到 Ethernet ECU 自己的 IP，但**E-Sys/PSdZ 获取它的确切服务、目标 ECU 直接诊断端口及状态切换后的端口：未知**。不能仅由 MGU 客户端的 DID 实现断言 E-Sys 使用同一流程，也不能默认所有 ECU 都在 `13400`。

**置信度：高（MGU DID 的现有代码）；中（存在额外直接连接的行为描述）；低（E-Sys 内部路径）。**

**证据：**`M/diag.go:208–234`；[remote-enet 页面][re] 的 Direct ECU link 段；`A` 没有这些直连流量。[手册][es24] §5.3.3 的 Media Converter 模式仅说明用户输入 URL，不证明自动 IP 获取机制。

**仍需实测：**MGU/RAM 台架或含相应 ECU 的车，全端口抓包中找 DID 响应与随后 SYN 的关联，分别记录网关路径和 ECU 路径。只为确定连接行为即可，不要求在本调研中执行任何写操作。

### Q4.4 HTTP server 端口、URL 传递、源 IP 与镜像

| 问题 | 结论 | 置信度 | 证据 | 仍需实测 |
|---|---|---|---|---|
| E-Sys HTTP server 端口 | 默认 `8888`，可配置为 `8000/8080/8888/12000–13000`；不是 MGU HTTP 入站端口 | 高（手册） | 本地手册文本 `375–383`；[24.06 手册][es24] §5.6，p.69 | 目标版本的监听地址、PID、实际端口及防火墙 |
| 谁发 HTTP 请求 | 商业实现描述 ECU 下载回连 tester；应将 ECU→tester 的 TCP 单独考虑 | 中 | [remote-enet][re] “How the HTTP path works” | 原始 SYN、HTTP request/Host/URL，确认与正向诊断流的关系 |
| ECU 如何获知 URL/IP | **未知**。没有足够证据确定是 UDS `34` 扩展、`38`、专有 Routine/DID，或其他报文 | 未知 | 现有 PCAP 无 HTTP；所审 EdiabasLib 核心是诊断传输，不含该 E-Sys/PSdZ 业务实现 | 需要合法已有 HTTP 会话日志/抓包，或具备该路径的台架 |
| E-Sys 用哪个本地 IP 填 URL | **未知**；不能把“诊断 TCP 的源地址”直接当作其算法 | 未知 | 手册未给出；公开 PSdZ 接口也未提供可验证的 E-Sys 选择实现 | 双网卡、alias、不同路由条件下同时比较 socket 源 IP、通告的地址、ECU 回连地址 |
| 是否只有 link-local 才选 HTTP | 商业页面这么描述；不是已验证的 E-Sys 全版本协议要求。启动 HTTP server 也不等于某 ECU 一定使用 HTTP | 中/低 | [remote-enet][re]；[手册][es24] §5.6 还列有 Gateway URL 等启动条件 | 同一版本在 LAN/link-local 发现地址下比较能力选择 |
| IP 镜像是否成立 | **条件性可行，尚未证实**：若通告地址等于 Client 车侧地址，且端口正确且 ECU 可达，那么 ReverseListener 可承接；alias 本身不保证通告地址如此 | 中（网络推论），低（具体产品行为） | Q1 的源地址发现与商业 alias 描述；无对应车侧抓包 | 同时验证通告内容、ECU SYN 落点、反向流映射及重连后变化 |
| remote-enet 的 E-Sys HTTP / ISTA HTTP / Off | 页面说明选择哪个 tester 在自动发现时获得 alias IP，另一个使用 VPN IP；不是“已证明只换本地端口” | 高（页面描述），未知（内部算法） | [remote-enet][re] Settings → HTTP flashing (alias IP) | 如何区分两个 tester、两者同机并行发现时的分配策略 |

额外证据：`docs/research/scratch/ediabaslib/Tools/Psdz/PsdzClientLibrary/RheingoldPsdzWebApi/Adapter/PsdzWebService.cs:593–612` 的 ISTA/PSdZ 公开客户端代码先找 `12300–12400` 中空闲端口，feature 分支为 `12304–12319`，可由注册表范围覆盖，fallback 才是 `8888`。`.../BMW/Rheingold/CoreFramework/Utility/NetUtils.cs:13–39` 确认是依次查找空闲端口。该代码对“统一固定 8888”构成直接反证，但不证明目标 ISTA 必选某个值。

**设计含义：**ReverseListener 应以本次会话实际观测/配置的 `IP:port` 为依据；目前不能确定只支持一个固定端口即可。若未知 URL 中嵌入了其他不可路由地址，单纯 L3 正向 Exposer 也不能修复回连。车端零权限的 IP 镜像方案应保留为待验证假设，不能写成已完成保证。

## Q5：ISTA 的 ENET 发现与后台流量

### Q5.1 发现机制及网段限制

**结论：**公开 ISTA 客户端代码直接实现 HSFZ 与 DoIP 发现服务，不必先经 `EDIABAS XEthernet` 才能找车；诊断连接使用 EDIABAS 是另一层。ENET 开启时同时注册两类发现；HSFZ 发 `000000000011` 到 UDP 6811，DoIP 发 `03 FC 00 01 00 00 00 00` 到 UDP 13400。发现解析器本身未限制应答方必须为 `169.254/16`；**目标已安装 ISTA 全流程是否接受本机 LAN IP：未知**。

**置信度：高（公开实现）；中（推及特定 ISTA 版本）。**

**证据：**

- `X/ConnectionManagerDeviceService.cs:112–133` 同时注册 `VCIEnetHsfzService`、`VCIEnetDoIpService`。
- `X/ENET/VCIEnetHsfzService.cs:10–12,25–47`：6811、6 字节请求、取源 IP。
- `X/ENET/VCIEnetDoIpService.cs:42–44,63–84`：13400、版本 3 请求、响应 VIN/逻辑地址/EID 的偏移。
- `X/ENET/AbstractVCIEnetService.cs:154–198`：枚举启用的非 loopback 网卡的所有 IPv4 定向广播，为每个广播建 `0.0.0.0:0` UDP socket；不是固定监听本机 6811。
- 同文件 `230–258` 直接生成 ENET device，再应用可配置 Filter；`X/ConnectionManagerDeviceService.cs:232–293` 的所见 filter 主要按设备类型/序列筛选，未显示 APIPA 限制。
- `.../BMW/Rheingold/CoreFramework/DatabaseProvider/VCIDevice.cs:304–308` 另有 `IsApipa` 和 `IsLocalMachine` 属性，说明其他路径可能区别处理，不能由解析器局部逻辑保证整个 GUI/连接链路成功。

**仍需实测：**实际 ISTA 版本、ENET VCI 设置、LAN IP vs link-local alias、同机 vs 第二台机器 responder；分别记录“显示车辆”和“真正连接成功”，不要合并成一个通过项。DoIP 服务的存在也不代表每个配置/版本始终同时发两种广播。

### Q5.2 是否持续轮询

**结论：**公开发现服务运行期间**每 2 秒持续发送发现广播**；这与“一直通过 6801 发 UDS 抢占诊断会话”是两回事。目标 ISTA 在连接前后是否持续打开 TCP/执行车况作业、对应强度：未知。

**置信度：高（发现线程）；未知（目标版本诊断后台行为）。证据：**`X/ENET/AbstractVCIEnetService.cs:279–324`。`A` 不包含 ISTA 会话；不能用 E-Sys/Tool32 数据代证。

**仍需实测：**分别在 ISTA 开启但未连接、已连接但空闲、关闭连接管理器三个状态观察 UDP 与 TCP，记录 owner 生命周期。进程独占可保留为产品防冲突策略，但不能声称“持续 6811 广播本身会占用 ZGW UDS 会话”。发现响应可继续，诊断连接的独占另判。

### Q5.3 ICOM SLP

**结论：**SLP 使用 UDP **427**，组播 **239.255.255.253**，还能发广播/已知 IP 单播，详见 Q9。`50160` 是 ICOM 映射诊断端口，设计中的 SLP 端口需要改正。

**置信度：高。证据：**`X/SlpClientData.cs:19–35,58–79`；[BMW AOS 端口表][aos]（2020-06-04 导出），PDF 第 3 页 `SLP_1`。**仍需实测：**ICOM 实际型号/固件的属性与 reservation 生命周期。

## Q6：Tool32 / INPA 与 EDIABAS XEthernet

### Q6.1 原版 EDIABAS.ini 的参数范围

**结论：**能给出公开 EDIABAS 7.3.0 样本中 `[XEthernet]` 的完整六项；**跨 EDIABAS 所有版本的完整参数规范未知**。不要把 EdiabasLib 的扩展配置当成原版参数全集。

**置信度：中（公开样本），高（字段原文）。**

| 参数 | 样本值 | 可确认的含义与边界 |
|---|---|---|
| `RemoteHost` | `192.168.16.102`；其他公开例为 `Autodetect` | 指定 IP 或自动发现；非 link-local IP 配置确实存在，不证明自动发现成功 |
| `HeaderFormat` | `0` | 有此字段；各值的正式含义/兼容范围未知 |
| `TesterAddress` | `F4,F5` | 可列多个 tester 地址；具体分配、回退、并发规则未知，不能据此保证隔离 |
| `ControlPort` | `6811` | 控制端口；须考虑 TCP 控制通道，不只 UDP 发现 |
| `DiagnosticPort` | `6801` | HSFZ TCP 诊断端口 |
| `TimeoutConnect` | `20000` | 连接超时示例值；另有样本用 2000，不能当作网关协议超时 |

**证据：**[just-push-it 的 EDIABAS 7.3.0 配置][ini]（2020-06-03），已保存为 `docs/research/scratch/ediabas-7.3.0-example.ini:63–71`；EdiabasLib 作者的 `Tools/ExtractEcuFunctions/ISTA-D_Debug.txt:185–190` 也列出 `RemoteHost=Autodetect`、`F4,F5`、端口与 `TimeoutConnect=2000`。`TimeoutReceive`、`TimeoutFunction` 在所保存文件属于 **`[TCP]`**（`50–56`），不能移植成已证实的 `[XEthernet]` 参数。

`Autodetect` 的预期是 6811 广播找车、取应答源地址再连接。`EC:17–22` 明确把 EdiabasLib 的 `auto` 对应到原版 `Autodetect`，但原版闭源驱动的完整发包列表、多个车时选择规则和目标版本网卡过滤仍未知。[XEnet32 分析][xd] 可用于定位 channel/control 结构；其摘要把 `0002` 写成诊断响应、`0010/0011` 写成发现请求/应答，与实现和 PCAP 冲突，不能据此制定帧规范或填补未知。

**仍需实测：**实际 Tool32/INPA 使用的 EDIABAS 版本，在 VM LAN IP 与 link-local alias 上分别测试显式 IP、Autodetect，观察 TCP 6811、tester 地址选择、是否必须真实 KL15 响应。只读识别作业即可确认连接层，不需要执行任何编码。

### Q6.2 EdiabasLib 的 host 格式与发现流程

**结论：**文档中的格式是 `ip[:protocol][:port...]`，但不同协议后面的端口含义不同：

| 形式 | 对应含义 |
|---|---|
| `192.0.2.10` | 显式 IPv4；无发现广播。协议选择仍受 `VehicleProtocol` 等设置影响 |
| `192.0.2.10:HSFZ:6801:6811` | HSFZ diagnostic port、control port |
| `192.0.2.10:DoIP:13400:3496` | DoIP plain port、TLS port；最后一项**不是 HSFZ control port** |
| `auto` | 使用 `HostIdentService`；本次代码默认 `255.255.255.255` 时转为枚举接口 |
| `auto:all` / `auto:<prefix>` | 所有接口 / 接口名匹配前缀（忽略大小写） |

这些是配置格式示例，`192.0.2.10` 是文档保留地址，不是实际车辆地址。

**置信度：高（本次版本）。证据：**`EC:16–29`；`E:1926–2056` 按 `HSFZ/DoIP/RPLUS` 分支解析；`E:2936–2940,3140–3170,3190–3249,3353–3405` 为绑定临时端口、发发现、按源地址识别的实现。HSFZ 显式带端口时内部被标为 `Icom` 类型，是否执行 allocation 还取决于其他配置；不能仅从这个内部名字判断实际硬件就是 ICOM。源码末尾用 `IPAddress.Parse`，本段格式不要当成支持任意 DNS 名称或 IPv6 的 URL 语法。

默认参数：HSFZ `F4/6801/6811`，DoIP tester `0EF3`、gateway `0010`、plain `13400`、TLS `3496`（`E:710–720,795–804`）；连接超时默认 5000 ms（`E:813`）。这是库默认值，不能当作原厂网关所有超时。

**仍需实测：**原版 EDIABAS 不保证接受这套冒号扩展；本项目若对接原版 Tool32 应使用其自身的 INI 参数语法。多网卡下最终选用地址、发现响应源端口过滤、显式端口带来的 ICOM allocation 行为须以所用库版本验证。

## Q7：DoIP、G 系、DHCP 与 TLS

### Q7.1 不能按车系一刀切协议

**结论：**ENET 是物理接入方式，不等于一个固定应用协议；不能写“G 系必走 DoIP，F 系必走 HSFZ”。当前本地已有 G028 车辆的 **HSFZ** 流量；公开 ISTA/EdiabasLib 则同时支持 HSFZ 与 DoIP。具体 gateway/车型年代/软件版本用哪个协议，应发现或配置决定。

**置信度：高。证据：**`M` 原有 `hsfz_captures/analysis/REPORT.zh-CN.md:60–65` 解出的 FA 为 G028，原始流量由 `A` 验证为 HSFZ；`E:794` 协议列表；Q5 的双服务注册。**仍需实测：**目标 G 系/BDC 是否同时开放 6801、13400、3496，工具实际选择顺序；不要由 VIN 或车型前缀推断服务。

### Q7.2 DHCP 的角色

**结论：**“使用 DoIP 所以 Client 必须运行 DHCP server”不成立。DoIP 的标准直连场景支持在无 DHCP server 时自动配置 link-local；网络化场景可以由外部基础设施提供 DHCP。**车辆外部 DoIP entity 与 tester 通常是地址配置的客户端，不应为了诊断无条件向已有网络提供 DHCP。**本项目可把零权限、既有 OS IP 配置作为默认，不必为了 DoIP 先内置 DHCP server。

**置信度：高（标准允许无 DHCP 直连）；中（特定 BMW 外部接口是否正常回退）；未知（独立 RAM/MGU 的所有台架配置）。**

**证据：**[ISO 13400-2:2019 公开镜像][iso] §6.2.1（正文 p.6，PDF 第 16 页）明确描述无 DHCP 的直接连接和自动配置；§9.1.1 的 DoIP-014/101（正文 p.33）描述 DHCP client 和不得干扰 tester 网络的要求。E-Sys [手册][es24] §5.3.1 也描述无 DHCP 时网关回退 APIPA。此标准没有证明 BMW 的每个内部 Ethernet ECU 都遵循外部 DoIP 地址分配机制。

**仍需实测：**G 系 ENET 外部接口上电抓 DHCP Discover/Offer/ARP/Announcement，确认哪个设备扮演什么角色。若台架 ECU 确需 DHCP，先确认需求来自 ECU/ICOM/拓扑而非 DoIP 本身，并用独立现成路由器提供，避免提前破坏 Client 零权限边界。

### Q7.3 Vehicle Identification / Announcement 字节布局

**结论：**DoIP 基础头为 8 字节：`version:1, inverse:1, payload_type:2 BE, payload_length:4 BE`。BMW 公开客户端用版本 `03/FC`；Scapy 还支持 `02/FD` 等版本，不能在通用 parser 中只写死版本 3。

**置信度：高（布局与公开实现）。证据：**`S/doip.py:104–169`、`E:760–765,3391–3405`、`X/ENET/VCIEnetDoIpService.cs:42–44,63–84`。

| 消息 | payload type | 正文 |
|---|---|---|
| Vehicle Identification Request | `0001` | 空，`L=0`；BMW 版本 3 样本 `03 FC 00 01 00 00 00 00` |
| Request with EID | `0002` | 6 字节 EID |
| Request with VIN | `0003` | 17 字节 VIN |
| Vehicle Identification Response / Announcement | `0004` | 相同正文结构，见下表；不是用不同 type 区分两者 |

| 响应绝对偏移 | 正文字段 | 长度 |
|---|---|---:|
| `8–24` | VIN，ASCII | 17 |
| `25–26` | Logical address，大端 | 2 |
| `27–32` | EID，原始字节；不等于 IP | 6 |
| `33–38` | GID，原始字节 | 6 |
| `39` | Further action required | 1 |
| `40`（可选） | VIN/GID sync status | 1 |

即常见正文长 32 或 33，总长 40 或 41。**上述标准正文没有 IPv4/IPv6 地址字段**；客户端取 UDP 源地址构造网关目标，无需为更换 Presenter IP 去改 VIN/EID/GID。Identification response 回请求 socket；上电 Announcement 是另外的广播接收路径，应在 UDP 13400 上接收，不能只等一个主动请求的临时端口。

**仍需实测：**目标车型真实样本、版本、是否发 Announcement、次数与时序、sync/further-action 状态变化；动态状态不能永远回放旧缓存。DoIP TCP 还需要 routing activation（`0005/0006`）、alive（`0007/0008`）和 `8001/8002/8003` 诊断/ACK/NACK；它不是把 HSFZ 端口改成 13400，参见 `S/doip.py:104–120`。隧道可以透明搬运，让真实网关和工具完成这些握手。

### Q7.4 TLS 3496

**结论：**DoIP 支持 `3496/tcp` 上的 TLS，不能声明“BMW 不用 TLS”；也不能声明“所有 G 系必须 TLS”。公开库有标准 `3496`、ICOM 映射 `50163` 及相应 TLS 路径；本批 PCAP 没有 TLS 使用证据。

**置信度：高（标准/实现支持），未知（目标车要求）。证据：**`E:715–720,2163–2172`；`S/doip.py:341–342,369–370`；[ISO][iso] §11.1 Table 39。**仍需实测：**目标车与工具是否强制安全路由激活/证书、是否接受 plain fallback；外层中继 TLS 不替代车端原有 DoIP TLS，也不应由 Presenter 擅自终止它。

## Q8：MGU/RAM 的 IP 配置与端口

### Q8.1 DID 172A 的返回

**结论：**`M/diag.go` 确实向 MGU `63` 发只读 `22 17 2A`，找 `62 17 2A 00`，再按大端读取 **12 字节、三个 4 字节值**。代码注释和错误文案说“四个 IPv4 地址”，但实现只返回三个值，不能照抄文案。现有公开现场日志支持它们为 IP、netmask、第三个配置值；第三项是否在所有版本均是 default gateway、前导 `00` 的正式定义：**未知**。

**置信度：高（实现字节）；中（字段含义，缺 OEM DID 定义）。**

**证据：**`M/diag.go:15,208–234`。上游 [issue #49 的现场日志][mgu49]（2026-11-07）含只读返回：

```text
62 17 2A 00 C0 A8 10 6C FF FF FF 00 00 00 00 00
            192.168.16.108  255.255.255.0   0.0.0.0
```

这是公开用户上传的日志，非本地原始抓包；引用仅用于 IP 配置语义，不采用其中写操作。日志打印前两项为 MGU IP / netmask，单靠它不足以证明第三项语义。所审本地 PCAP 的重组诊断内容没有 `22 17 2A` 请求/响应。EdiabasLib 作者笔记 `Tools/ExtractEcuFunctions/ISTA-D_Debug.txt:552–553` 也将 `172A` 称为 ECU port configuration，但未给响应字段规范。

**仍需实测：**目标 ID7 MGU 在常态和需要的连接状态下只读响应、所有返回项长度和含义；RAM 是否使用同一 DID/格式。DID 的正式 OEM 名称仍未知，不应写成“标准 DoIP IP 地址 DID”。

### Q8.2 端口与地址分配

| 问题 | 结论 | 置信度与证据 | 仍需实测 |
|---|---|---|---|
| SSH `22/tcp` | MGU 工具使用此端口，但是否开放取决于状态/配置；不能作为原厂常开服务 | 中；[上游 issue #49][mgu49] 有同车关闭/开放观测；`M` 的服务代码也使用 SSH | 目标机当前状态；本报告不提供开启步骤 |
| HTTP 服务端口 | 已证实的是 **tester** 的 HTTP server（Q4.4）；并未证实 MGU 必须监听一个固定 “HTTP 刷写端口” | 高（角色区分），未知（ECU 入站 HTTP 服务）；现有 PCAP 无 HTTP | ECU 实际发起/接受的连接方向、目标端口 |
| ECU 直接诊断 `6801/13400/3496` | MGU/RAM 各自实际暴露组合 **未知**，不能从网关默认端口或车型名推断 | Q4.3、Q7；没有目标 ECU 端点抓包 | 全端口 SYN/服务应答，不用固定端口白名单代替观测 |
| MGU 是否固定 link-local | 不是只能有固定 link-local；公开日志已有 `192.168.16.108/24`，也有 [issue #47][mgu47] 的 `169.254.160.165/16`（2026 年现场日志） | 中；配置返回可变；没有证明地址选择算法 | 冷启动、重连、DHCP 存在/不存在时地址变化 |
| 地址由 ZGW 分配吗 | **未知**。读取到 IP 不等于知道谁分配；外部 AutoIP/DHCP、车内静态配置和 ICOM 管理路径须分开 | Q7 的外部 DoIP 标准不覆盖所有内部 ECU | 需要实际台架启动阶段的 DHCP/ARP 与网关/ECU 双侧观测；RAM 单独验证 |

因此 `Target.Ports` 中 MGU 的 `{22, http?, 13400}` 只能是候选能力，不能写成探测即确定开放。黑盒中继按已授权车侧子网转发的策略不要求提前猜中所有端口；Operator 若自行读取 DID，仍受 Q3 并发限制。

## Q9：ICOM 发现和 tester 行为

### Q9.1 SLP 报文

**结论：**可确认的 BMW ICOM 发现路径是 SLPv2 UDP 427，组播 `239.255.255.253`；还存在定向/全局/link-local 广播及已知设备 IP 单播。公开 ISTA/EdiabasLib 使用 **Attribute Request（function 6）**，回复为 Attribute Reply（function 7）；不应把所有 SLP 发现写成仅 service-type request。

**置信度：高。证据：**`E:752–759,800,3414–3416`；`X/SlpClientData.cs:19–35,58–79`；`X/SLPAttrRqst.cs:9,48–124`；`X/SLPAttrRply.cs:9`；[BMW AOS 官方端口表][aos] PDF 第 3 页。

EdiabasLib 发出的 **33 字节 UDP payload**（已发布实现的请求，不是本次捕获的 ICOM 帧）：

```text
02 06 00 00 21 00 00 00 00 00 AB CD 00 02 65 6E
00 00 00 00 00 07 64 65 66 61 75 6C 74 00 00 00 00
```

布局：SLPv2 header 含 function `06`、24-bit 总长度 `000021`、flags、next-extension offset、transaction ID `ABCD`、lang 长度 `0002` 和 `en`；随后是 16-bit 长度前缀的 previous-responder list、URL、scope=`default`、tag list、SPI，除 scope 外均空。ISTA 的 XID 随请求递增（`X/SlpClientData.cs:65`），回复应匹配实际 XID，不可写死 `ABCD`。SLP Attr Reply 中的属性有 `DEVTYPE`、`IPADDRESS`、`MACADDRESS`、`VIN`、`GATEWAY`、`VCICHANNELS`、`OWNER` 等，见 `E:3414–3536`。

**仍需实测：**真实 ICOM 的完整属性、大小写/转义、广播与组播可达性、UDP 超长或其他 SLP 函数是否涉及 TCP 427。与 HSFZ/DoIP 的已知识别正文不同，**SLP 可以内嵌 IPADDRESS/URL**；将来 ICOM Presenter 不能直接沿用“不含 IP 所以原样回放”的结论。

### Q9.2 ICOM 并非只换一个 ZGW IP

**结论：**ICOM 可以提供与 ZGW 兼容的 HSFZ 诊断封装，但完整行为不等价。它有端口映射、设备占用/释放与多总线通道；HSFZ 典型映射为 `50160/tcp` diagnostic、`50161/tcp` control，DoIP/TLS 默认映射在本库为 `50162/50163`。RPLUS/NMP `6801` 又是另一条接口路径。

**置信度：高（公开实现和文档），未知（全部型号固件的一致性）。证据：**`E:712–720,1956–1988,3457–3536`；`EC:31–34,47`；[E-Sys 手册][es24] §5.3.2。库支持 ICOM allocation，可检查 OWNER，并通过独立的 HTTP/SOAP 设备管理路径占用/释放；这不是 ZGW TCP accept 的同义词。

**仍需实测：**实际 ICOM 型号/固件、空闲/已占用状态、HSFZ vs RPLUS 的握手与端口选择、是否需要持续 reservation。不能因 rawenet 能转发裸 HSFZ 就宣称 ICOM 功能齐全。当前 MVP 可明确不支持 ICOM，未来独立 Probe/Presenter 处理 SLP 地址重写与管理连接。

## 假 ZGW 最小规格：`internal/testutil/fakezgw`

这是后续实现规格，本次不新增项目服务代码。目标是无车验证发现、流量透明性、并发连接生命周期和反向隧道；**不是完整 E-Sys/ISTA 车辆仿真器，也不把真实写操作录制成自动成功回放。**

### 必须实现的确定行为

| 范围 | 最小行为 / 验收点 | 依据 |
|---|---|---|
| UDP 发现 | 在可配置 IP 的 `6811/udp` 接收 `L=0,0011`，回 Q1 的 56 字节 fixture；回到请求源 IP/端口，响应源 IP 为预期暴露地址 | Q1；本机广播交付须单独验证 |
| 发现变体 | fixture 中 VIN/MAC/DIAGADR 可配置；重算 `L`，保留原始未知字段的测试路径；无效短包/错误类型不应盲目回复 | rawenet responder 不验证请求（`R/pkg/hsfz/discovery.go:96–116`），本项目不照抄此缺陷 |
| TCP framing | `6801/tcp` 接收字节流；支持一帧被拆分、多个帧合并；以 `6+L` 取帧。空正文控制帧合法，错误帧不可卡住解析 | Q2.1、Q2.4 |
| diag ACK | 接受有效 `0001` 后先回 `0002`，正文为请求 `src,dst` 加 `min(UDS长度,5)` 字节 UDS 前缀；`L=2+min(n,5)`，地址不交换 | Q2.2；属于与本批样本及 EdiabasLib 相容的 profile，不冒充全车型规范 |
| 诊断回包 | 仅对测试显式配置的只读请求返回独立 `0001`；单播时交换 ECU/tester 地址；功能寻址时可注入多个物理 ECU 源地址。未知请求可由测试配置超时或负响应，禁止默认“成功” | Q2.2、Q3 的功能寻址观察 |
| alive | fake 主动发送 `0012`，可用 `L=32` 的 `DIAGADR10BMWVIN`+17 字符测试 VIN；tester 预期回 `00 00 00 02 00 13 00 F4`，改 tester 地址时末字节随配置变化 | Q2.3 的代码与 PCAP |
| 多连接 | 每条 TCP 有独立流缓冲、ACK、alive 状态；accept 数量可配置，含只 connect 即 close 的探测；不因一个测试工具多开连接就丢弃其他流 | Q3 抓包；容量是 fake 参数，不是实车事实 |
| 关闭与异常 | 半关闭、RST、超时、断开后新建连接、空 `0041`、`00FF` 等可注入；隧道不得把错误帧吞掉或主动重放 diag | Q2；恢复应由原工具决定 |

alive 的发送周期、等待超时、最大连接数、重复 tester 地址时的行为，全部是 **可配置故障注入参数**，不是测得的默认值。例如测试可选 `aliveInterval=1s, responseTimeout=1s` 加快执行，但必须标注为测试值。至少包含“无 alive、正常回答、丢应答后断连、长正文 alive、空错误帧”五种 profile。

### 三工具测试需要补的行为

- 预留 `6811/tcp` control listener：响应 `0010` 查询时用测试配置的 KL15 字节，明确为模拟点火状态。是否还需 `0013` control status 由工具抓包决定，不能预先假装真实车辆状态。
- `same tester` / `different tester` / “拒绝第二连接”等 profile 只测试中继是否正确搬运，不证明真 ZGW 会采用哪一种策略。
- 测试 HTTP reverse stream 可用无害测试文件和普通 HTTP client/server；验证 EOF、range/长连接行为、并发流量与重新建连。它能验证搬运能力，**不能证明 E-Sys 已把正确 URL 告诉 ECU**。
- 用少量只读 golden fixture 验证连接层；不能要求只做 ACK 的 fake 通过 E-Sys 的整个车辆识别。若 E-Sys 因缺 FA/SVT/DID 退出，区分“发现/握手已验证”和“完整工具会话未仿真”。

## 仍需实测清单：按最低环境成本排序

所有实验先固定工具版本、接口模式、Windows 网卡/路由配置，并分别留存 tester 侧与车侧 PCAP；不要只捕获 `tcp port 6801`，否则会再次漏掉发现、控制、DHCP 和 HTTP。以下仅列观测目标，不提供车辆写操作步骤。

| 顺序 / 环境 | 能回答的问题 | 需要记录的最小证据 / 通过标准 | 仍无法证明的范围 |
|---|---|---|---|
| 1：现有 PVE Windows VM + 第二块隔离 VirtIO 网卡 + 软件 fake ZGW；无需真车 | Q1 的本机广播交付；Q4 的实际 E-Sys 模式、LAN/alias 自动发现、源地址取值；Q5 ISTA、Q6 Tool32 自动发现；三工具是否碰 TCP 6811 | 两种暴露 IP 下的请求/响应 UDP 五元组、TCP SYN、实际工具列表和连接结果；注明 responder 与工具是否同机 | 真网关并发/idle、ECU HTTP URL 机制 |
| 2：同环境 + fake 的 ACK/alive/错误 profile + 普通测试 HTTP client/server | 端到端不吞控制帧、短探测连接、多连接、半关闭；反向隧道的基本网络能力 | 可复核 `0012→0013`、`0041,L=0` 不堵塞、每条流独立、配置端口反向可达；可加模拟公网延迟 | 与真实 ECU 的时限和 E-Sys HTTP 选择算法 |
| 3：VM 只建立连接并观察 E-Sys/ISTA HTTP listener；仍无需车 | Q4.4 的实际端口、绑定地址和 PID；确认不是统一 8888 | 工具版本、监听表、server 启动前后变化，必要时占用默认端口观察候选范围；不发车辆写命令 | URL 通告字段及 ECU 实际回连 |
| 4：已有 F20 + ENET + 一台可抓全流量的电脑 | F20 原始发现；单播接受；TCP 6811；ZGW 并发、F4/F5 路由、idle/alive；只读轮询是否扰动 E-Sys | 真 `6801/6811` 的两条连接、各 tester 地址/应答去向、alive/FIN/RST 时间；同时记录网关软件版本和车辆唤醒状态 | MGU/RAM 的 HTTP/direct 模式；其他网关型号 |
| 5：可借用的 G 系车或 BDC DoIP 网关台架 | Q7 的具体协议、AutoIP/DHCP、Announcement、13400/3496 与工具选择 | 上电阶段 DHCP/ARP/UDP，路由激活及 TLS 是否被选中；证明地址由哪方提供 | 不能按单车结果推广全部 G 系 |
| 6：已有合法 HTTP 会话全量日志/PCAP；无此资料才需 MGU ID7 + 对应网关/BDC + ENET + 稳定电源台架 | Q4 最关键的 URL/IP 承载、E-Sys 本地 IP 算法、MGU/RAM 直接 IP/port、镜像闭环 | 同时对照 URL 通告字节、tester 诊断源 IP、ECU 发出的 SYN/HTTP 请求、两个网络侧的反向流；保留源 IP/路由对比与状态转换结果 | 其他 ECU/工具版本，RAM 需单独覆盖；本任务不执行实际编程 |
| 7：能借到的 ICOM/ICOM Next + ENET/车 | Q9 的 SLP 属性、reservation、端口映射及 ICOM 状态 | 原始 SLP request/reply、IPADDRESS/OWNER、占用与释放后的连接差异 | 不同固件/多总线管理 |

最低成本推进路径：先做 1–3；可消除大量“工具到底发了什么”的不确定，不应为了验证本机发现就上车。F20 只需只读诊断即可解决 4；HTTP URL 机制若没有可复核的已有会话证据，仍需对应 MGU/PSdZ 实际路径，靠 fake 不能闭环。

## DESIGN.md 中每个 ⚠ 的对应与建议改写

这里只提出建议，不修改 `docs/DESIGN.md`。本次 `rg -n '⚠' docs/DESIGN.md` 共找到 12 个含标记行，其中 3、345 是总括说明。

| DESIGN 行 / 位置 | 报告位置 | 状态 | 建议替换成的结论 |
|---|---|---|---|
| `3` 总体“全部待验证” | 全文、实测表 | 部分解决 | 以本报告逐项置信度管理；公开实现已解决的格式与目标工具/硬件的未知分开，不要求把全部未知伪装成已确定 |
| `183` 发现 payload 无 IP，原样回放 | Q1、Q4.2 | 已知布局已解决；E-Sys 行为待验 | 已知 HSFZ `0011` 正文无 IP；缓存原始字节无需 IP 改写；Responder 须从预期暴露 IP 回请求源端口。目标 E-Sys 仍用软件环境确认 |
| `186` ECU→tester:8888 | Q4.4、Q8.2 | 部分解决 | HTTP 下载回连 tester；E-Sys 默认 8888 可配置，ISTA 可能动态选端口；监听/转发按实际 endpoint，方向与 URL 承载仍须目标验证 |
| `188` WiFi ENET / ISTA、Tool32 非 link-local | Q4.1、Q5.1、Q6 | 未解决工具兼容性 | rawenet 为未具名软件的经验线索；公开 ISTA/EdiabasLib 发现解析未见该硬限制，但目标 E-Sys/ISTA/原版 EDIABAS 的同机 LAN/alias 成功矩阵仍待软件实测。M0b 已实测：E-Sys 3.36.2、ISTA 4.39.20 的 LAN 发现成立，Tool32 LAN Autodetect 失败（见 Q4.1 实测更新） |
| `208` HTTP 诊断端源 IP | Q4.4 | 未知 | URL/IP 通告机制及地址选择算法未知；IP 镜像为条件性方案，不得从 TCP 源 IP 直接推定成功 |
| `221` ISTA LAN vs alias 与持续轮询 | Q5 | 部分解决 | 公开代码直接周期发现 HSFZ/DoIP，不必经 XEthernet；每 2s 的发现不等于 UDS 占用。目标版本完整连接与 TCP 后台流量用 VM 验证 |
| `228` ZGW 并发、F5 轮询隔离 | Q3 | 未知 | 当前代理侧抓包不能证实实车连接上限/地址隔离；外部工具活动时默认暂停自主轮询，待 F20 只读并发验证 |
| `345` M0 所有标记改确定 | 全文、实测表 | 部分完成 | M0 文献/已有抓包研究输出本报告；后续实测按成本矩阵补证据。“未知”应继续保留，不能用源码推测冒充目标验证 |
| `370` G 系 Client 是否必须 DHCP server | Q7.1–Q7.2 | 普遍性问题已解决；目标车待验 | DoIP 无 DHCP 直连可用 AutoIP，Client 无需普遍充当 DHCP server；G 系也可能使用 HSFZ，按发现能力判断；独立 ECU/ICOM 台架另验 |
| `371` HTTP 地址传递决定镜像 | Q4.4 | 未知 | 与 208 同一未知；需实际 URL 通告与 ECU 回连证据，不能仅依赖 alias 宣传描述 |
| `459` MGU HTTP/13400 端口 | Q8 | 未知，且角色需修正 | `172A` 可读 MGU 配置；SSH 22 是否开放看状态，直接诊断端口另测。HTTP server 位于 tester，不能把 8888 或 13400 当作 MGU 已确认入站能力 |
| `485` E-Sys HTTP/ISTA HTTP 开关 | Q4.4 | 页面语义已解决；实现待验 | 商业页面明确是控制哪个 tester 获得 link-local alias、另一个用 VPN IP；不是已确认的本地端口切换。端口与识别两个 tester 的机制各自验证 |

另有未标 ⚠ 但必须一并修正的前提：`169` 的 `DF=网关`、统一 8 字节 HSFZ 解析；`174/460` 的 `SLP 50160`；`173` 的统一 HTTP `8888`；`476` 的 DoIP 只做发现/13400 而遗漏 TLS 能力探测；`483` 只描述正向 ECU 访问却不能覆盖 HTTP 下载回连。Q2、Q4、Q7、Q9 给出证据与范围。

## 复核方法与产物

- 分析脚本：`audit_captures.py`（私有研究附件，未随公开快照分发）。仅用 Python 标准库读取原始 PCAP；通过 `compile/exec` 加载既有 decoder 的函数，未运行它会向 `mgu_kit` 写文件的 `main()`，也未在该目录生成 import cache。
- 结果：`capture-audit.md`（私有研究附件，未随公开快照分发）、`capture-audit.json`（私有研究附件，未随公开快照分发）、`capture-audit-summary.json`（私有研究附件，未随公开快照分发）。所有 11 份原始 PCAP 的 SHA-256 都记录在结果中。`peak` 只算窗口内观察到完成握手且尚未 FIN/RST 的连接，是下界；不将半关闭后的资源状态当作完整测量。
- 可重跑命令：`python3 docs/research/scratch/audit_captures.py`。原始文件只读，输出限定在 scratch；本次未使用 tshark、未向车辆发送任何报文。
- rawenet 的 `go test ./pkg/hsfz` 通过；只证明其解析/常量 fixture 自洽，**不证明 alive 的语义或真实工具兼容**。本次手工核对也确认发现 fixture `L=50`、总长 56、VIN 长 17。
- E-Sys 手册下载于[公开镜像][es23]，PDF/提取文本在 scratch，PDF 共 367 页，SHA-256 为 `0320c562b61c9a2631ed1dd19070b8ec025c9f3ac67adc6e491fe119cfb5c847`；用 `uv` 的临时 `pypdf` 环境提取，缓存位于 `/tmp/remote-enet-uv-cache`，未给系统 Python 安装包。文件名版本不作为官方真实性保证；关键模式/HTTP 项与另一份 24.06 手册镜像交叉核对。
- 未修改 `mgu_kit` 或 `docs/DESIGN.md`；没有 commit/push。当前工作区的 `.git` 是环境提供的空只读目录，无法获取本项目真实 Git 状态，未尝试修复或初始化它。

[e]: https://github.com/uholeschak/ediabaslib/blob/390e5944fb12d4477e45e46a5524d7f9fb043732/EdiabasLib/EdiabasLib/EdInterfaceEnet.cs
[ec]: https://github.com/uholeschak/ediabaslib/blob/390e5944fb12d4477e45e46a5524d7f9fb043732/docs/EdiabasLib.config_file.md
[x]: https://github.com/uholeschak/ediabaslib/tree/390e5944fb12d4477e45e46a5524d7f9fb043732/Tools/Psdz/PsdzClientLibrary/BMW/Rheingold/xVM
[r]: https://github.com/rawmind0/rawenet/tree/1422c25c02a245cf532e8a26ed70f619c107b6b1
[sh]: https://github.com/secdev/scapy/blob/0e2f6ba2669dc9c6487d7c0a4220b3c3dfd6d4ef/scapy/contrib/automotive/bmw/hsfz.py
[sd]: https://github.com/secdev/scapy/blob/0e2f6ba2669dc9c6487d7c0a4220b3c3dfd6d4ef/scapy/contrib/automotive/doip.py
[xd]: https://emdzej.github.io/ediabasx-docs/reference/interfaces/xenet32-analysis.html
[ini]: https://gist.github.com/just-push-it/fa7c1e75aad5b61bec188fab336069a2
[es24]: https://www.scribd.com/document/1060535565/ESYS-Manual-24-06-01-Eng
[es23]: https://git.davidpetric.com/thepetric/BMW_coding/raw/branch/main/guides/E-Sys_Manual_V23_10_ENG.pdf
[re]: https://remote-enet.com/
[iso]: https://www.pedestrian.com.cn/_downloads/4476a20abad82c59b612394da0b84f61/ISO%2013400-2-2019.pdf
[aos]: https://aos.bmwgroup.com/api/v2/downloads?key=port-list%2FTCP_IP_postlist_20_1_EN.pdf
[mgu49]: https://github.com/cauliflowermossgrape/idrive7-8-ssh-root/issues/49
[mgu47]: https://github.com/cauliflowermossgrape/idrive7-8-ssh-root/issues/47
