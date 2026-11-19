# remote_enet 设计文档（v0.10）

> 社区源码快照（2026-11-19）：本文件保留早期设计演进；当前已记录 F20 的 E-Sys/ISTA 只读验收、公网会话与八小时 loopback/内存设备长稳，边界见 [VALIDATION](VALIDATION.md)。下文各版本的“待验收”是其历史状态，不覆盖后续专项报告；SaaS、数字配对和 HTTP 回连等规划不因本次源码公开而成为已实现能力。

> 视觉规范更新（2026-11-18）：后续界面与品牌统一采用[Pixel Workbench](UI-DESIGN.md)，即浅色像素工作台、直角组件、整数间距与RJ45 Car Logo；旧浅色研究作为历史记录保留。协议和运行时架构不因视觉更新改变。

> 部署模式与中心协议更新（2026-11-19）：Client、Operator 各只有一个通用版本，直连、自建 Hub、中心协调三种模式由运行配置和输入格式区分；8 位数字码作为 PAKE 口令；中心地址可配置，官方中心实现不在本仓库。见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md) 与 [CENTER-PROTOCOL](CENTER-PROTOCOL.md)。第 9.5、9.9 节与之冲突的旧设想（同仓 `renet-coord`、商业构建裁剪自托管能力、具体云平台选型）以这两份文档为准，均不代表已经实现或部署。

> 历史状态（v0.10 时的记录，已被文首 2026-11-19 说明取代）：Wave 1–7 核心、固定双端 Native GUI、跨平台CI修复及Mac→Windows原生LAN假车链路已验收；用户确认开始Wave 8 TUN呈现基础。真实WAN及M1实车验收尚未完成。标注「⚠ 待验证」的地方要按 `docs/enet-protocol-research.md` 的置信度处理，不能把未知项当已知实现。
> v0.10 变更（2026-11-17）：确定TUN＋用户态协议栈为长期呈现主路线，按TCP最小验证→MGU发现/SSH→UDP/DoIP/自动发现→HTTP回连分阶段验收。Windows用Wintun、macOS用utun；mgu_kit兼容需保留本地发现Presenter，不把TUN等同以太网广播。短码暂不做；关闭按钮统一完整退出。首阶段契约见 `docs/WAVE8.md`。
> 配色补充（2026-11-16）：按最新偏好采用冷中性浅色，语义 token 与原生窗口外观统一，保留 Wave 6 的紧凑分组和固定双端边界；见 `docs/research/ui-light-theme.md`。
> v0.9 变更：参考 remote-enet 的紧凑分组布局，拆分 Client／Operator 独立入口；固定角色、配置、窗口/托盘与单实例标识分开，Native 宿主共用，不改变协议。
> v0.8 变更：引入 mygo v0.9.1 Native UI、desktop 控制器与独立 JSON 设置；加入逐动作异步回执、排队启动撤销和托盘生命周期；Windows 普通用户双端界面验收通过，macOS 仅交叉构建。
> v0.7 变更：增加 app 运行时、无车/网卡恢复等待、地址变化重连、真实转发指标、协商后的 peer-only 连接测试及 Windows 进程独占；记录 Windows 工具和原生 socket/PID 实测，保留未验收的实车与 HTTP 行为。
> v0.6 变更：并入 M0 文献调研结论（`docs/enet-protocol-research.md`）。协议修正：HSFZ 基础头 6 字节、发现标记 `DIAGADR`、alive check 为网关发 `0x0012` / tester 回 `0x0013`、ACK 不交换地址、`DF` 是功能地址、6811 另有 TCP 控制通道、SLP 为 427/udp、E-Sys/ISTA 的 HTTP 端口不固定。M0 从「实车抓包」改为「文献 + VM 内假 ZGW 台架」。
> v0.2 变更：吸收了与 ChatGPT 的讨论（PVE 部署背景、rawenet、三工具兼容性），传输层首选改为 TCP+TLS。
> v0.3 变更：HSFZ 发现/帧编解码复用 `~/src/mgu_kit`；UI 功能清单按 remote-enet.com 对齐；确认公网 IPv4、UDP 不可靠，QUIC 降为可选。
> v0.4 变更：直连与中继统一为 Hub 模型（内嵌/独立），删除 QUIC 与 P2P 路线；商业版只走独立 Hub；独占按进程；车端黑盒，白名单简化为「车侧子网内任意端口」。
> v0.5 变更：角色命名改为 Client / Operator（代码包 carside / operator）；一个 Operator 同时只有一个会话；分发/控制平面（CF Worker）与数据平面（Hub）分离，沿用 mgu_kit 的发布体系；商业版 Operator 不编入内嵌 Hub。

## 1. 目标与边界

**要做的**：让家里 PVE 上的 Windows VM（跑 E-Sys / E-SysX dongle 直通 / ISTA / Tool32）通过公网连到一台插着 ENET 线的车，效果和本地插线一样。近期是 F20 的 FDL/VO coding；之后是 MGU ID7 台架，会涉及 Ethernet ECU 直连和 HTTP 刷写。

**部署背景（已确定）**：

```
车辆现场                                    家
MacBook（现在）/ 任意 Win·mac 笔记本（以后）     UCG-Fiber  ── 端口转发 ──►  PVE · Windows 11 x64 VM
  └─ USB-C 网卡 ── ENET 线 ── ZGW            DDNS 指向这里                  E-Sys + E-SysX dongle(USB 直通)
                                                                             ISTA / Tool32
```

- DDNS 在家（Operator 端），车端主动向家里 dial。车端在手机热点、酒店 Wi-Fi、CGNAT 下都不需要配置。
- 用户自己的 Mac 已经能通过 Surge 回家，但目标是**不依赖 WireGuard/Surge/ZeroTier**，车端只跑一个最小客户端。
- UCG-Fiber 只对公网开放这一个隧道端口，不暴露 RDP、6801 或任何诊断端口。
- Operator也规划在macOS上为mgu_kit等原生工具提供远端ECU访问。用户态协议栈和隧道共用，TUN设备、地址/路由、本地发现及进程识别分别做平台适配；Mac原生发现兼容性单独验收。

**现阶段的边界**：

| 维度 | 现阶段 | 未来 |
|---|---|---|
| 连通方式 | DDNS + 端口转发直连 | 独立TCP Hub中继；不做STUN/P2P打洞 |
| 用户规模 | 自用，点对点 | 多客户、多会话 |
| 协议 | HSFZ（UDP 6811 发现 / TCP 6801 诊断） | DoIP 13400、ICOM |
| 授权 | 无 license | 再说 |

**网络接入目标**：自行提供TCP加密隧道，自用DDNS与商业独立Hub共用上层会话/转发；诊断呈现与公网路径选择分离。

## 2. 角色与术语

| 术语 | 含义 |
|---|---|
| **Client 端**（车端，对应 remote-enet 的 Client） | 插着 ENET 线的电脑或盒子。只用普通 socket，不装驱动，不需要管理员权限。 |
| **Operator 端**（诊断端，对应 Operator） | PVE 里的 Windows VM。负责「假装」本地插着一辆车。 |
| **Peer** | 对端。身份 = Ed25519 公钥指纹。 |
| **Session** | 两个 peer 之间一条已认证的加密连接，上面可以多路复用很多 stream。 |
| **Tunnel / Stream** | 一个被转发的 TCP 连接（或一组 UDP 报文），对应 session 里的一个 stream。 |

**角色和连接方向解耦**：谁是 Client、谁是 Operator，与谁 listen、谁 dial 无关。默认 Operator 端 listen、Client 端 dial；反过来（车里放一台带 4G 的树莓派常驻，DDNS 在车端）只改配置，不改代码。

## 3. 总体架构

```
 Operator 端 (PVE Windows VM)                                 Client 端 (Win/mac/Linux/树莓派)
┌──────────────────────────────────┐                     ┌──────────────────────────────────┐
│ E-Sys / ISTA / Tool32            │                     │                                  │
│   │ tcp 6801 / ECU 直连 / UDP 6811│                     │                                  │
│   ▼                              │                     │                                  │
│ operator/ 虚拟车辆网络             │                     │ carside/  车辆侧                  │
│   - GatewayListener (6801)       │                     │   - NIC 探测 (169.254/16)         │
│   - DiscoveryResponder (6811)    │                     │   - HSFZ 车辆发现 (6811)          │
│   - Exposer: 本地造 ECU IP        │                     │   - Dialer: 连 ZGW / ECU          │
│   - 反向监听接收端 → 本地 :8888   │                     │   - ReverseListener (:8888 回连)  │
│   │                              │                     │   - 目标白名单                    │
│   ▼                              │                     │   ▲                              │
│ tunnel/  stream ⇄ socket 搬运     │                     │ tunnel/                          │
│ session/ 握手·心跳·重连·路由      │                     │ session/                         │
│ transport/ ── TLS+yamux / QUIC ──┼──── 公网 (DDNS) ────┼─ transport/                      │
│ identity/  密钥·配对·指纹钉扎     │                     │ identity/                        │
├──────────────────────────────────┤                     ├──────────────────────────────────┤
│ app/  状态仓库 + 命令 + 事件      │                     │ app/                             │
│ ui/   mygo native view           │                     │ ui/   mygo native view │ renetd   │
└──────────────────────────────────┘                     └──────────────────────────────────┘
```

依赖方向是单向的：`ui → app → {session, tunnel, carside, operator, diag} → transport → identity`。`ui` 只读 `app` 的状态快照、调 `app` 的命令；核心层不知道 UI 存在。同一套核心既能跑 GUI，也能跑 headless 的 `renetd`。

## 4. 关键设计

### 4.1 Transport 层（可插拔的核心）

```go
package transport

// Session 是两个已认证 peer 之间的一条多路复用连接。
type Session interface {
	OpenStream(ctx context.Context) (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	Peer() identity.Fingerprint
	Path() PathInfo // Kind: direct | relay；本地/远端地址
	Stats() LinkStats // RTT、收发字节
	Close(reason error) error
	Done() <-chan struct{}
	Err() error
}

type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error // 半关闭，保留读取方向；Close 则解除本地读写阻塞
}

// Connector 负责通过内嵌或独立 Hub 连接对端。
type Connector interface {
	Dial(ctx context.Context, peer PeerInfo) (Session, error)
}

type Acceptor interface {
	Accept(ctx context.Context) (Session, error)
	Close() error
}

type PeerInfo struct {
	ID        identity.Fingerprint
	Endpoints []Endpoint // {Kind: "hub", Addr: "home.example.com:47000", Hub: hubFingerprint}
}
```

**实现顺序（v0.2 调整）**：

1. **M1：`hub` 获取连接，`secure` 提供 TLS 1.3（ALPN `renet/1`，双向证书）+ hashicorp/yamux。** 理由：
   - 只需要在 UCG 上转发一个 TCP 端口；酒店/公司网封 UDP 时也能用。
   - 依赖最少：`crypto/tls` 标准库 + yamux 一个包。
   - coding 流量很小，TCP-over-TCP 的队头阻塞在这个场景里感知不到。
   - ChatGPT 建议的 WebSocket over TLS 在这里没有额外价值：不经过 HTTP 反向代理就不需要 WebSocket 帧；以后要和别的服务共用 443 可以靠 SNI 分流，裸 TLS 同样支持。
2. **v0.4 起：直连和中继统一为 Hub 模型（见 9.2）**。路线图上只有一个 Path 实现 `hub`：自用时 Hub 内嵌在 Operator 进程里（Client 直连 DDNS），商业版时 Hub 是独立的 `renet-hub`（双方都出站连它）。`Connector` 注册表保留但不再规划第二个实现。
3. **不做的**：QUIC、STUN/P2P 打洞。理由：国内运营商 UDP 不可靠；车端多为手机热点（CGNAT、对称 NAT），两侧都在 NAT 后时打洞成功率差；而独立 Hub 已经覆盖所有网络环境。接口允许以后加，但不是路线图的一部分。

**网络条件（已确认）**：家里有公网 IPv4，DDNS 指向它，UCG 转发一个 TCP 端口即可。IPv6 作为补充：DDNS 同时更新 AAAA，dial 方用 happy-eyeballs 并发尝试 v4/v6。

### 4.2 身份与配对（不需要服务器）

- 每个安装实例首次启动时生成一个 Ed25519 密钥，并自签一张证书。指纹 = 公钥的 SHA-256，展示时取前若干位，用 base32 分组。
- **邀请码**：由 listen 方生成，内容是 `{version, endpoints[], listener 指纹, 一次性 token, 过期时间}`，编码成 `renet1:` 前缀的 base32 字符串，也可以显示成二维码。
- 配对流程：
  1. Operator 端在 UI 点「生成邀请」，把邀请码发给车主（微信等渠道）。
  2. Client 端粘贴邀请码后 dial。双方做 TLS 双向认证，Client 端校验 listener 指纹（钉扎）。
  3. 握手后的第一条控制消息带上 token。Operator 端校验 token 有效，就把 Client 端指纹记入 `trusted_peers`，token 作废。
  4. 之后用指纹直接互认，不再需要 token。
- **安全要点**：listener 暴露在公网，**未认证的连接在 TLS 握手阶段就拒绝**（首次配对时客户端证书未知，握手通过后由 token 决定是否接纳，之前不转发任何数据）。按源 IP 限速，失败次数多了封一段时间。

### 4.3 Session 与控制协议

- 每个 session 的第一个 stream 固定为 **control stream**，由 `Options.Initiator` 指定的一端打开，与 Client / Operator 角色无关。帧格式为 `uint32_be 长度 + CBOR`，整数键 envelope 保存消息类型和正文；未知消息类型可跳过。具体消息编号与字段见 `internal/proto`，不引入 protobuf codegen。
- 握手：`Hello{proto_version, role, app_version, capabilities[]}`。双方取能力交集；主版本不同时拒绝连接，并在 UI 上提示升级。
- 消息集合（第一版）：

| 消息 | 方向 | 用途 |
|---|---|---|
| `Hello` / `PairToken` | 双向 / C→O | 握手、配对 |
| `VehicleState` | C→O | NIC、网关 IP、MAC、VIN、发现响应原始字节；变化时推送 |
| `Ping/Pong` | 双向 | 心跳和 RTT（每 1s） |
| `ConnTest{dur, rate}` / `ConnTestResult` | O→C / C→O | 连接测试：打流统计丢包、抖动、RTT 分布 |
| `ExposeReverse{port}` | O→C | 让车端在车侧网卡上监听某个端口（HTTP 回连） |
| `Log` | C→O | 车端事件日志同步 |
| `Chat`（以后） | 双向 | — |

- **数据 stream**：每个被转发的 TCP 连接对应一个新 stream。stream 开头是一个 `OpenTunnel{kind: tcp, target: "169.254.x.y:6801", origin}` 头，之后是裸字节，双向 `io.Copy`，支持半关闭。
- **车端目标白名单（不可见的安全底线）**：Client 端放行**车侧网卡所在子网内**的任意 `ip:port`（通常是 `169.254.0.0/16`），其他目标一律拒绝。它的目的只有一个：防止 Operator 端把车主电脑当成进入其家庭/公司网络的跳板。它不是授权机制——车端是黑盒中继，不解析、不审批、不记录隧道内容（v0.4 决定，见 9.7）。
- **重连**：session 断开后，dial 方指数退避重连。已有的 tunnel 直接断开，不尝试恢复，由 E-Sys 自己重连。每条诊断连接都有明确的生命周期和超时。刷写过程中断线的风险靠刷写前的「连接测试」来规避，不靠协议层的魔法。

### 4.4 Client 端：车辆侧

- **NIC 探测**：枚举网卡，找有 `169.254.x.x` 地址、或用户手动指定的网卡。
- **车辆发现**：在该网卡上发 HSFZ 车辆识别请求（UDP 6811）。`~/src/mgu_kit` 里已经有一份能用的实现（`mgu-go/internal/mgudiag/discovery.go`）：
  - 探测报文是 6 字节 `00 00 00 00 00 11`，即 HSFZ 帧 `length=0, type=0x0011`，空 payload。
  - 发往 `255.255.255.255`、`169.254.255.255` 以及每块网卡的定向广播地址；socket 绑 `0.0.0.0` 随机端口；整体超时 2–3s，单次读 100ms，收到首个响应后遇到一次读超时就结束。
  - 响应解析目前只取 VIN：去掉 NUL 后找 ASCII 标记 `BMWVIN`，取后面 17 字符；网关 IP 用 UDP 响应的源地址，**不是从 payload 里解析的**（调研 Q1 已确认：已知 `0x0011` 响应正文没有 IP 字段）。
  - 响应正文格式（调研 Q1，置信度高）：6 字节头 `[L:4 BE][0x0011]` 后是**相邻的 ASCII 标记+值**，无分隔符：`DIAGADR` + 2 字符十六进制诊断地址（如 `10`）、`BMWMAC` + 12 字符 MAC、`BMWVIN` + 17 字符 VIN。标记是 `DIAGADR`，**不是** `DIAGADDR`。rawenet 的 56 字节脱敏 fixture 可直接用作测试数据。
  - 响应要**单播回请求的源 `IP:port`**（客户端用临时端口收），不能固定回 6811。
  - 本项目需要在此基础上补：绑定到指定车侧网卡（多网卡的笔记本上不能乱发）、保留**原始响应字节**、解析上述三个字段并容忍未知扩展字段。F20 的原始响应仍要在实测 4 补抓（见第 6 节）。
- **可复用的代码（带修正）**：
  - `mgu_kit/mgu-go/internal/mgudiag/hsfz.go`：HSFZ TCP 帧编解码。复用时要按调研 Q2.4 扩大帧模型：**基础头是 6 字节** `[L:4 BE][control:2 BE]`，只有 `0x0001/0x0002` 的正文才是 `[src][dst][UDS]`；`0x0011`、长 alive check、空正文错误帧（如 `0x0041, L=0`）不能按 8 字节头解析，`L<2` 也不能拒绝。`DF` 是本批车辆的**功能寻址地址**，物理网关的诊断地址是 `10`。`0x00FF` 只是 out-of-memory/NACK，不代表全部 reject（另有 `0x0040–0x0045`）。
  - `mgu_kit/hsfz_captures/`：11 份 6801 抓包（2 份空），均经公网代理、tester 地址全是 `F4`，无 UDP 发现、无 HTTP。可作为假 ZGW 的 ACK/alive 行为依据（调研 `docs/research/scratch/capture-audit.md` 已逐包核对）。
  - [rawmind0/rawenet](https://github.com/rawmind0/rawenet)（MIT，Go）：`pkg/hsfz/protocol.go` 的发现响应 fixture 可用；**其 `docs/hsfz-protocol.md` 对 alive check 的方向写反了**（见 4.5），只取字节、不取结论。
  - [uholeschak/ediabaslib](https://github.com/uholeschak/ediabaslib) 的 `EdInterfaceEnet.cs`：最完整的 HSFZ/DoIP/SLP 开源实现，是本项目协议细节的首要参考。
- **Dialer**：收到 `OpenTunnel` 后校验白名单，绑定车侧网卡的源地址去连目标，然后开始双向搬运。
- **ReverseListener**：收到 `ExposeReverse{port}` 后，在车侧网卡 IP 上监听该端口。ECU 发起的 HTTP 下载连到这里，车端为它反向开一个 stream 给 Operator 端，Operator 端再连到本地 E-Sys/ISTA 的 HTTP server。**端口不能写死 8888**（调研 Q4.4）：E-Sys 默认 8888 但可配置为 8000/8080/8888/12000–13000；公开的 ISTA/PSdZ 客户端代码在 12300–12400 里找空闲端口，8888 只是 fallback。Operator 端要在会话开始时探测工具实际监听的 `IP:port`（Windows 上查监听表 + PID）再下发。
- **ICOM**（以后）：SLPv2 发现，**UDP 427、组播 `239.255.255.253`**，Attribute Request（function 6）/ Reply（function 7）；`50160/tcp` 是 ICOM 映射的 HSFZ 诊断端口、`50161` 是控制端口，不是 SLP 端口。SLP 回复里**内嵌 `IPADDRESS` 属性**，ICOM Presenter 需要改写，不能沿用 HSFZ/DoIP「不含 IP 所以原样回放」的结论。ICOM 还有占用/释放和多总线通道，不等价于换一个 ZGW IP。放在 `carside/icom`，MVP 明确不支持。
- **headless 形态 `renetd`**：同一套 vehicle 核心交叉编译到 linux/arm64，可以跑在树莓派或 OpenWrt 上，做成「插车上常驻 + 4G」的盒子。

### 4.5 Operator 端：虚拟车辆网络（技术风险最大的部分）

E-Sys/ISTA/Tool32 对「车」的期望有几种访问方式，各自对应一个组件：

| 工具行为 | 组件 | 实现 |
|---|---|---|
| UDP 6811 找车 | `DiscoveryResponder` | 在暴露地址上监听 6811/udp，收到 `L=0, 0x0011` 就把缓存的**原始响应字节**从暴露地址**单播回请求的源 IP:port**。已知响应正文不含 IP（调研 Q1，置信度高），不需要改写。对无效短包/错误类型不回复。不把广播转发过去。 |
| TCP 连网关 :6801 | `GatewayListener` | 在暴露地址监听 6801，每个 accept 开一个 stream 指向真实 ZGW。保活端到端透传：既有 PCAP/公开客户端有 `0x0012 → 0x0013`；本机 E-Sys 3.36.2 对长 fixture 回 `00 00 00 02 00 12 00 F4`，仅作为该环境观察。隧道 Ping/Pong 不代替 HSFZ 保活，Operator 不代答或改写。 |
| TCP 6811 控制通道 | `GatewayListener`（第二个端口） | 公开 EDIABAS 客户端会向 `ControlPort` 6811/tcp 发 `00 00 00 00 00 10` 查 KL15（调研 Q2.5）。只转发 `6801/tcp + 6811/udp` 不够，`hsfz` Presenter 要同时监听 6811/tcp 并透明转发。 |
| Ethernet ECU 直连（MGU/RAM 等） | `Exposer` | 在本机「造出」ECU 的 IP，监听后同样转发。ECU 实际暴露的端口组合未知，靠 L3 透传而不是猜端口。 |
| ECU 的 HTTP 下载回连诊断电脑 `IP:port` ⚠ | 车端 ReverseListener + 本地 connect | 见 4.4。端口按会话实测，URL/IP 的通告机制未知（见下）。 |

**关于「WiFi ENET 模式」**：这是 rawenet README 的说法，公开 E-Sys 手册里没有这个模式名（调研 Q4.1）。实际使用 `Connection via VIN`。Windows 实测确认 E-Sys 3.36.2 与 ISTA 4.39.20 能发现本机普通 LAN 地址；E-Sys 使用显式只读 fixture 后显示 G168 连接建立，Tool32 显式 IP 读取模拟 VIN 成功，但其 LAN Autodetect 仍报 NET-0012。基础 HSFZ 呈现可从 LAN 地址起步；完整诊断、其他版本和隔离 link-local 仍需分别验收，见 M0b 记录。

**呈现按IP与数据包设备分层**。Wave 8实际契约为 `packetio.Device`、`l3.Start` 和 `tunadapter.Open`，见 `docs/WAVE8.md`；TUN路径不为每个ECU端口调用宿主net.Listen。

1. **`tunNetstack`（长期主方案，v0.10确认）**：Windows用Wintun，macOS用utun，Linux用TUN；gVisor用户态协议栈共用。给已知ECU安装精确/32远端路由，工具访问真实 `ECU_IP:port`，IP包进入用户态栈，再把TCP流接入现有隧道。ECU地址不作为宿主本机别名，不与宿主SSH等服务争抢同一个监听地址。首阶段仅IPv4/TCP，UDP、广播、MGU发现、反向HTTP及PID独占接线分别验收；不能直接承诺支持所有工具或刷写。适配器和路由初始化需要Operator侧系统权限，Go保持CGO=0；Windows发行需携带官方签名Wintun DLL。车侧IP镜像仍是下面的条件性设想。
2. **`ifaceAlias`（简化选项）**：在专用网卡上增加IP别名并逐端口监听。PVE VM可用无上联vmbr的第二块VirtIO网卡；普通LAN/loopback的现有HSFZ Presenter继续可用。但同一Windows网络栈中的网卡别名不能自动规避已有wildcard监听，不能与TUN用户态栈混为同一机制。临时SSH高位端口映射可作调试手段，不扩展成主产品端口配置表。

**车端永远不做 L3**：车端要零权限、一键启动，这是产品边界，不为对称性破坏它。

**独占模式（Relay exclusive，可开关，默认开）**：粒度是**进程**而不是连接，因为 E-Sys/ISTA 一个工具会开多条 TCP（网关一条、Ethernet ECU 另一条）。`GatewayListener` 和 `l3` Presenter 对每个 accept 的本地连接用 `GetExtendedTcpTable` 反查 PID；第一个连上的进程成为 owner，其他进程的连接被拒并在 UI 提示，直到 owner 没有活动连接或 Operator 点「结束会话」。Operator 自己的车况轮询（4.7）不受独占限制，但有独立开关。

**诊断端源 IP 问题（⚠ 关键未知，文献调研无法回答）**：HTTP 刷写时 ECU 回连 tester 的 HTTP server 下载。调研 Q4.4 的边界：

- **已知**：E-Sys HTTP server 默认 8888 可配置；ISTA/PSdZ 动态选端口；remote-enet 的「E-Sys HTTP / ISTA HTTP / 关」开关，页面原话是**决定哪个 tester 在自动发现时拿到 link-local alias 地址**（另一个用 VPN IP），不是切换本地端口。
- **未知**：ECU 通过什么报文得知 URL/IP（UDS `34` 扩展？`38`？专有 Routine/DID？）；E-Sys 用哪个本地 IP 填 URL（不能假设等于诊断 TCP 的源地址）。
- **IP 镜像**（Operator 端在 TUN/专用网卡上使用**车端车侧网卡的那个 IP**，让 E-Sys 从这个地址连出去，ECU 回连落到车端 ReverseListener）：**条件性可行、尚未证实**。如果 E-Sys 通告的地址确实是它的出站源 IP，这条路成立；如果 URL 里嵌的是别的不可路由地址，L3 正向暴露也救不了。
- 备选：车端在车侧网卡上加 Operator 的 IP。需要车端提权，与零权限边界冲突，排第二。

验证路径（见第 6 节实测 3 和 6）：先在 VM 里只建连接、观察 E-Sys/ISTA 的 HTTP listener 端口与绑定地址（无需车）；URL 通告机制要么找到已有的合法 HTTP 会话抓包，要么等 MGU 台架。M4 的实现方式在此之前不定。

### 4.6 三个工具怎么连上虚拟网关

| 工具 | M1 手动模式 | M2 自动发现 | 备注 |
|---|---|---|---|
| **E-Sys** | Connection via gateway URL：`tcp://<Operator暴露IP>:6801` | Connection via VIN；3.36.2 已在 LAN 绑定下发现模拟 VIN 并显示 G168 连接建立 | FA/SVT、实际 coding 与 HTTP 回连未验收。 |
| **Tool32 / INPA（EDIABAS）** | `EDIABAS.ini`：`Interface=ENET`，`[XEthernet] RemoteHost=<IP>; HeaderFormat=0; TesterAddress=F4,F5; ControlPort=6811; DiagnosticPort=6801; TimeoutConnect=20000` | `RemoteHost=Autodetect`（6811 广播，取应答源地址） | 六项参数来自 EDIABAS 7.3.0 公开样本（调研 Q6.1）。会用 **6811/tcp** 控制通道查 KL15，Presenter 必须转发。 |
| **ISTA** | 不做手动模式（产品决定） | 公开实现自带 HSFZ、DoIP 和 SLP 发现；4.39.20 已在 LAN 绑定下列出模拟 VIN、类型 ENET、状态 Free | 完整车辆识别/诊断仍未验收。发现广播不占用 UDS 连接，进程独占针对本地诊断/控制 TCP。 |

### 4.7 诊断与观测（diag/）

- 每条 tunnel 记录：目标、开始时间、收发字节、当前速率（两端都有）。
- **HSFZ 帧解析只在 Operator 端做**，用于日志里的帧数、按 IP/诊断地址分组视图和 verbose 日志。车端不解析隧道内容（9.7）。
- **连接测试（Wave 4）**：Hello 协商 `linktest/1` 后使用独立有界 echo 流，只终止于两个程序，不进入车辆端口。报告有效样本数、RTT p50/p95、载荷字节、串行往返 echo 速率和超时/中断；TCP 测试不能给出 IP 丢包率。当前不输出缺乏实车依据的 safe/marginal/unsafe 结论。
- **电压、PAD 状态等车况读取**（M3 后半）：车端自己通过 6801 发 UDS 读 DID。mgu_kit 的 `mgudiag` 已经有 HSFZ/UDS 客户端（例如用 `22 17 2A` 读 MGU IP 配置），可以复用（按 4.4 的修正扩大帧模型）。**ZGW 的并发连接上限、`F4`/`F5` 的路由隔离、ECU 应用会话是否互扰，是三个不同问题，文献和现有抓包都回答不了**（调研 Q3；`0x0040 incorrect tester address` 只说明地址有校验）。因此默认策略：**外部诊断工具有活动连接时自动暂停自主轮询**，只有在目标网关上验证过的只读组合才允许同时开启；PAD 切换是状态改变，不能按只读轮询的结果外推。实测 4（F20 只读）后再放开。remote-enet 的做法是把电压 / 延迟画成历史曲线并标 13V 下限。
- 日志用 `log/slog`，结构化输出到文件和 UI 的 ring buffer；车端日志经 control stream 同步到 Operator 端。

### 4.8 app/ 与 UI（mygo native）

**app/ 层**（UI 无关，可单测）：

Wave 4 已实现 `Runtime.RunClient/RunOperator`、独立快照、合并订阅、代次借用和 `TestLink`，具体签名见 `internal/app/README.md`。下面的 `App` 命令是 M3 GUI 包装层的目标形态，并非已经实现的导出 API。

```go
type App struct { /* 持有 session 管理器、vehicle 或 tester 组件、配置 */ }

func (a *App) Snapshot() State          // 不可变快照，UI 每帧读
func (a *App) Subscribe() <-chan struct{} // 状态变化通知
func (a *App) Connect(invite string) error
func (a *App) GenerateInvite() (string, error)
func (a *App) RunConnTest(ctx context.Context) error
```

**ui/ 层**：mygo 的 native view 是「每帧根据状态重建」的即时模式。一个 goroutine 订阅 `desktop.Controller.Subscribe()`，通过 `Window.Update` 在 UI 线程应用最新配置/快照；异步动作由入口执行，完成时发送对应动作回执并重绘。视图不读磁盘、不拨号，不直接持有 Runtime。

**mygo 选型（2026-11-16 核对）**：当前最新稳定版为 [v0.9.1](https://github.com/egoist/mygo/releases/tag/v0.9.1)，Wave 5 已以此锁定依赖，最低 Go 版本为 1.27.3。选择 Go `ui` 包和 `WindowOptions.Content` 的自绘 Native UI，不使用 HTML、JavaScript 或 WebView；它不是 Win32 系统控件集合。仍处于 pre-1.0，`ui` API 可能变化。应对：锁定版本；UI 视图限制在 `internal/ui/`；桌面控制器在 `internal/desktop/`，复用 `app.Runtime` 快照和生命周期，将来要换 Gio、Fyne 或 Wails 只重写这一层。纯 Go、无 cgo，可以从 Linux 交叉编译 Windows 和 mac 产物；打包、签名、自动更新用它自带的能力。

**Operator 端在 VM 里的运行形态**：GUI + 托盘，开机自启，最小化到托盘。ChatGPT 提的 Windows Service 形态等 `renetd` 稳定后再考虑（service 跑核心、GUI 走本地 IPC），MVP 不拆。

**mygo 已有经验**：`mgu_kit/mgu-go` 使用 mygo v0.7.1 + WebView，包含 GUI 与 `cmd/mgucli`。可借鉴项目分层、打包和更新流程；本项目 Native UI 的组件与窗口实现需按 v0.9.1 API 编写，不照搬其 HTML 前端。

**功能清单（以 remote-enet.com 的两个窗口为蓝本）**：

| 区块 | 车端 (Client) | 诊断端 (Operator) | 阶段 |
|---|---|---|---|
| 连接状态栏 | 已连接/重连中、对端指纹、路径类型、RTT | 同左；仅一个当前会话，可保留最近配对记录 | M3 |
| 车辆信息 | VIN、型号/车型代码、生产日期、I-level（出厂/当前）、里程 | 同左（来自车端推送） | VIN/网关/MAC M3；型号、I-level、里程需 UDS 读 DID，M3 后半 |
| 连接详情 | 车侧网卡名、网卡 IP、ZGW IP、MAC、隧道公网地址 | 暴露地址、Exposer 类型、E-Sys Connect 面板（显示该填的 gateway URL） | M3 |
| 实时车况 | 电压、延迟、诊断模式、PAD(编程/编码)状态 | 同左 + PAD 开关 + 电压/延迟历史曲线（13V 下限线） | 4.7 节，M3 后半 |
| 操作 | 会话首页手动选择车侧网卡（即时保存）/ 刷新网卡 / 粘贴邀请码 / 连接 / 断开 / 停止中继 / 重新探测 | 生成邀请、结束会话、连接测试(safe/marginal/unsafe)、Relay exclusive(独占，拒绝第三方会话)、带宽上限 | M3；带宽上限 M4 |
| 防火墙状态 | 检测本机防火墙是否会拦 6811/6801（macOS/Windows 各自实现） | — | M3 |
| 日志 | 事件日志 | 事件 + 隧道数据 + HSFZ 帧（verbose 开关、按 IP 分组视图）、帧数 ↑↓ 与丢包计数、保存日志 | 事件 M3；帧解析 M3 后半 |
| 聊天 | 时间戳、已读、文件/图片、粘贴截图 | 同左 | 以后 |
| 设置 | 开机自启 | 监听端口、DDNS 域名、暴露网卡、Direct-link 自检、HTTP 刷写模式切换（E-Sys HTTP / ISTA HTTP / 关） | M3；HTTP 刷写 M4 |
| Direct link 面板 | — | 当前隧道、已暴露的 ECU 地址（如 `0x63 HU_MGU`、`0x37 RAM`）、Test tunnel、Detect | M4 |
| 其他 | 自动更新（含 Beta 通道）、内置帮助 | 同左 + 恢复工具(EnergyMode/HU/DTC)、K+DCAN 桥、SSH 使能、直发 UDS、发动机运转检测 | 自动更新 M5；恢复工具等看 mgu_kit 已有能力，另立项目 |

**界面草图**：

Client 端（单窗口，尽量傻瓜）：

```
┌ Remote ENET · 车端 ─────────────────────────────┐
│ ● 已连接 home.example.com   直连 · 28ms · TLS   │
│ 对端: Operator (ABCD-EFGH)                        │
├─ 车辆 ──────────────────────────────────────────┤
│ VIN  WBA...      网关 169.254.92.65  [重新探测] │
│ 网卡 en7  169.254.71.35  MAC ..                 │
├─ 活动 ──────────────────────────────────────────┤
│ 6801  ↑1.2MB ↓3.4MB   8888(回连) 空闲           │
├─────────────────────────────────────────────────┤
│ [粘贴邀请码……]                 [连接] [断开]    │
│ 日志 ▾                                          │
└─────────────────────────────────────────────────┘
```

Operator 端（左侧会话列表 + 右侧详情）：

```
┌ Remote ENET · 诊断端 ───────────────────────────────────────────┐
│ 会话          │ 车辆  VIN / 网关 / 车端网卡       路径: 直连 v6 │
│ ● WBA…(F20)   │ 状态  RTT 31ms  丢包 0%  [连接测试] → safe      │
│ ○ 待配对…     │ 暴露  gw 192.168.1.50:6801 → ZGW  (WiFi ENET)    │
│               │       ECU 0x63 169.254.145.208  ✓ 自检         │
│ [生成邀请]    │ 隧道  表格: 目标 / 时长 / ↑↓ / 速率              │
│ [设置]        │ 日志  [事件] [隧道] [HSFZ 帧]   [保存日志]        │
└───────────────┴─────────────────────────────────────────────────┘
设置页：监听端口、DDNS 域名（用于生成邀请）、暴露网卡、Exposer 自检、HTTP 回连开关
```

## 5. 代码结构

一个 Go module，两个产物入口，核心共享：

```
remote_enet/
  go.mod                      # Go >= 1.27.3；mygo v0.9.1
  cmd/
    renet-client/             # 车端 Native GUI，固定 Client
    renet-operator/           # 诊断端 Native GUI，固定 Operator
    renetd/                   # CLI：invite、client、operator
    fakezgw/                  # 假网关与显式只读 fixture
    # renet-hub/ renet-coord/ # 未来：独立 Hub 与协调服务
  internal/
    identity/                 # Ed25519 密钥、自签证书、指纹
    invite/                   # 自包含的一次性邀请码格式
    trust/                    # 原子消费 token、信任与对端端点持久化
    hsfz/                     # 帧与发现编解码；隧道仍透明转发
    hub/                      # 外层认证、配对、搬运；内嵌与独立共用
    transport/                # Session/Stream/Connector 与注册表
      hub/                    # Hub 接入、候选竞速与生命周期
    secure/                   # 端到端 mTLS + yamux
    directory/                # 静态目录；协调服务后续增加
    proto/                    # CBOR 控制与开流头
    session/                  # 握手、配对、心跳、流分发
    tunnel/                   # 透明双向转发、半关闭、增量计数
    carside/                  # NIC 选择、发现、子网目标限制、固定源转发
    operator/                 # 双 TCP 通道、发现呈现、独占接线
    udpdiscovery/             # 通配收包、packet info 过滤、固定源回包
    exclusive/                # Windows PID+创建时间解析及共享 lease
    observe/                  # 有界指标、历史与事件
    linktest/                 # 仅对端之间的有界 echo
    app/                      # 与 UI 无关的连接运行时及状态
    desktop/                  # GUI 控制器、角色约束、异步命令与生命周期
    desktopui/                # 双入口共用的原生窗口/托盘宿主
    settings/                 # 严格 JSON 偏好；与 trust/identity 分开
    ui/                       # mygo Native 视图与 headless 交互测试
    testutil/                 # fakezgw、memtransport
    integration/              # 库与 CLI 端到端回归
  docs/
    DESIGN.md
    CONTRACTS.md
    WAVE2.md ... WAVE5.md
    enet-protocol-research.md
    M0B.md
    research/
```

Client 与 Operator 用两个编译入口固定角色，不提供运行时切换；共享核心包，不用 build tag 复制后端实现。Operator 端特有的 Windows 代码用 `_windows.go` 文件隔离，其他平台编译成返回 `ErrUnsupported` 的实现。这是平台差异，不算空桩。

## 6. 里程碑

与 ChatGPT 讨论里的三阶段对应：Phase 1 ≈ M0–M1，Phase 2 ≈ M2–M3，Phase 3 ≈ M4 及之后。

| 阶段 | 内容 | 完成标志 |
|---|---|---|
| **M0a 文献调研（已完成 2026-11-15）** | 用 EdiabasLib / rawenet / Scapy 源码、E-Sys 手册、ISO 13400 和 mgu_kit 的 11 份抓包回答协议问题。 | `docs/enet-protocol-research.md`：Q1–Q9 结论 + 置信度 + 假 ZGW 规格 + 实测清单 |
| **M0b 假 ZGW 台架（无需车，与 M1 并行）** | 按报告「假 ZGW 最小规格」实现 `internal/testutil/fakezgw`；在 PVE VM（第二块隔离 VirtIO 网卡）上做报告的实测 1–3：三工具的实际模式名、LAN IP vs 169.254 别名下的发现/连接结果、是否碰 6811/tcp、E-Sys/ISTA HTTP listener 的实际端口与绑定地址。抓全流量，不要只抓 6801。 | 实测 1–3 的结果写进报告；决定 M2 的 Presenter 绑定方式 |
| **M1 headless 直连打通** | identity + 邀请码、hub（内嵌）+ secure、control 握手、6801/tcp + 6811/tcp 转发；Operator 端监听暴露地址，E-Sys 手动填 gateway URL。 | 在 VM 里远程读 F20 的 FA/SVT，做一次 FDL coding |
| **M2 稳定性 + 发现** | 车辆发现 + DiscoveryResponder（按 M0b 结论选绑定方式）、Tool32 EDIABAS 验证、ISTA 验证、心跳、重连、连接测试、tunnel 统计、白名单、独占。顺带做报告的实测 4（F20 只读：原始发现响应、单播接受、并发/`F5`/alive 定时器）。 | 弱网（`tc netem` 200ms / 1% 丢包）下读写稳定；三个工具都能连 |
| **M3 GUI** | app/ 运行时 + desktop/ 控制器、mygo 双端原生工作台、邀请配对、事件和托盘；Wave 5 已完成基础实现。 | 车主照着界面能独立完成连接 |
| **M4 Ethernet ECU + HTTP 刷写** | 前置：报告实测 6（HTTP URL 通告机制），要么找到合法的已有 HTTP 会话抓包，要么 MGU 台架。然后 `l3` Exposer、ReverseListener（动态端口）、按实测结论处理诊断端源 IP、带宽上限、Direct link 面板。 | 在 MGU 台架上远程刷写成功 |
| **M5 分发** | Windows 安装包、mac dmg、签名、自动更新、`renetd` arm64 产物。 | — |
| 未来 | 见第 9 节：独立 Hub + coord 服务、DoIP、MGU/SSH、ICOM、多租户商业化、聊天。 | — |

基础层历史进度（Wave 1–3）：基础库、假 ZGW CLI、`carside`、`operator`、`trust` 和 `renetd invite/operator/client` 已实现。跨包测试使用真实 TLS 和 loopback，覆盖内嵌 / 独立 Hub、两种拨号方向、并发诊断流、6811/tcp、alive 透传、反向流半关闭与配对拒绝。实际 CLI 子进程测试覆盖 Operator 启动后另进程签发邀请、端口转发、停止回收，以及两端重启后的 `--peer` 复连。Wave 3 修复首次 CI 暴露的握手错误归因，补 packet-info 广播收发与跨平台原生测试 CI；Linux 的完整应用链路已验证单播与定向广播。M1 实车验收和 M0b Windows 工具兼容性验证尚未完成。运行方式见根 `README.md`，Windows VM 验证步骤见 `docs/M0B.md`，接口和本轮范围见 `docs/WAVE2.md`、`docs/WAVE3.md`。

2026-11-16 首次 [M0b 实测](research/m0b-win11-2026-11-16.md) 已确认目标 Windows VM 上 E-Sys 3.36.2 与 ISTA 4.39.20 能自动发现本机普通 LAN 地址呈现的车辆；6801/6811 TCP 与广播回复也已验证。当时 E-Sys 连接检查受未配置识别响应与 alive fixture 限制，Tool32 的 LAN Autodetect 初始化报 `NET-0012`，因此仍不宣称三工具完整兼容或 M1 实车验收完成。

Wave 4 已增加 app 生命周期与重连、有界转发指标、peer-only 连接测试和 Windows 进程独占。Windows 已原生验证反向 TCP 四元组、PID/创建时间及跨进程拒绝/接管；真实 CLI 两端均完成 1MiB 专用测试，对应会话的车辆转发字节保持为零，随后只读 VIN 请求得到透明响应及相符的转发计数。Tool32 显式 IP 的 `status_vin_lesen` 返回 OKAY；E-Sys 补入有抓包依据的只读 fixture、显式选择实验 alive 控制字后显示 G168。该进展不等于 FA/SVT 或实车 coding 已验收，详见 `docs/WAVE4.md`。

Wave 5–9已完成双端Native GUI、隔离弱网与跨平台TUN/MGU会话通路；Windows GUI和Mac headless Operator均完成隔离假车的SSH认证与资源回收，Mac另验证原版mgu_kit发现服务。Wave 10进一步实现按次网络授权助手、统一双端预览包和脱敏诊断，原生结果独立记录于[Wave 10](WAVE10.md)。当前仍待真实WAN与实车只读验收；HTTP回连的URL通告机制、DoIP和正式签名更新分别保留后续阶段，不以现有TCP台架结果推定完成。

## 7. 测试策略

- `transport`、`session`、`tunnel`：用内存或 loopback 起两端做集成测试，覆盖半关闭、重连、并发 stream 和大流量。
- `carside` + `operator`：**假 ZGW**（`internal/testutil/fakezgw`），规格见 `docs/enet-protocol-research.md`「假 ZGW 最小规格」：6811/udp 回放 56 字节发现 fixture（VIN/MAC/DIAGADR 可配置、重算 `L`）；6801/tcp 按 `6+L` 切帧、容忍拆包合包和空正文控制帧；对 `0x0001` 先回 `0x0002` ACK（地址不交换、UDS 前缀 ≤5 字节）；只对测试显式配置的只读请求回 `0x0001`，未知请求超时或负响应，**禁止默认成功**；主动发 `0x0012` alive、期待 `00 00 00 02 00 13 00 F4`；多连接各自独立状态；可注入半关闭/RST/`0x0041, L=0`/`0x00FF`。alive 周期、超时、最大连接数都是**故障注入参数**，不是实车默认值。预留 6811/tcp 控制 listener 回模拟 KL15。
- 弱网：Linux 下用 `tc netem` 注入延迟和丢包，跑长时间稳定性测试。
- UI：mygo 的 view 可以不开窗口测试，覆盖关键状态（未配对、等待、已连接、失败等）的渲染。

## 8. 开放问题

1. ~~DDNS 在哪一端？~~ 已确定在家（Operator 端）。
2. 车端近期就是用户的 MacBook；`renetd` 盒子形态优先级低，等有需求再做。
3. 只做 BMW、先 HSFZ。DoIP 留接口，不在 MVP 里。
4. ~~家里的公网条件？~~ 有公网 IPv4。UDP 在国内跨省跨运营商不可靠，TCP 为主路径已定。
5. ~~ISTA 手动模式~~ 产品决定只做自动发现；M2 验证 ISTA 对应答方网段的要求。
6. 车端读车况时和 E-Sys 共用 ZGW：并发上限 / `F4`/`F5` 隔离 / 会话互扰三者均未知，默认「外部工具活动时暂停轮询」，实测 4（F20 只读）后再放开。
7. ~~G 系 DoIP 是否需要车端做 DHCP server~~ 普遍性问题已解决（调研 Q7.2）：ISO 13400-2 允许无 DHCP 直连用 AutoIP，Client 不需要充当 DHCP server；而且「G 系必走 DoIP」不成立（mgu_kit 抓包里的 G028 走的是 HSFZ），协议按发现结果决定。个别台架 ECU 若需要 DHCP，用独立路由器提供，不破坏车端零权限。
8. E-Sys 把自己的 IP 告诉 ECU（HTTP 回连）的报文与地址选择算法 ⚠ **未知**，文献无法回答。M4 的前置条件，见 4.5 和实测 6。
9. 目标版本的 LAN 基础路径已部分验证：E-Sys/ISTA 自动发现、E-Sys 模拟网关连接、Tool32 显式 IP 只读 VIN 成立；Tool32 LAN Autodetect 和三工具完整车辆识别仍待验证。
10. ZGW 的 alive check 周期、等待超时、空闲断开阈值 ⚠ 未知（抓包里三次 `0x0012` 分布在三条连接，不能推周期），实测 4 回答。
11. ~~调研临时文件清理~~ 已删除三个第三方克隆和手册 PDF，并加入 `.gitignore`；保留抓包核对脚本、结果、固定来源清单及报告引用的手册提取文本。
12. ~~Operator 具体 IP UDP 绑定无法接收定向广播~~ Wave 3 已改用通配接收、入站网卡/目的 IP 过滤和 packet info 固定回复源 IP/端口，包含 Linux、Windows、macOS 标准库实现，Linux loopback 的真实广播与完整应用链路已通过。Windows/macOS 原生测试纳入 CI，结果以对应提交为准；首次 Windows 原生运行暴露的 Go 同步发送计数差异已补兼容。M0b 仍需验证目标工具。车端继续零权限、载荷透明。

## 9. 扩展性架构：插件化与未来形态

### 9.1 「插件」在 Go 里的含义

不用 `plugin` 包（仅 Linux、ABI 脆弱）和进程外插件（每个字节多跨一次进程）。采用 **编译期注册表 + 配置选择**，即 `database/sql` 驱动模式：

```go
package transport

type Factory func(cfg map[string]any, id identity.Local) (Connector, Acceptor, error)

func Register(kind string, f Factory) // 各实现包在 init() 里调用
func New(kind string, cfg map[string]any, id identity.Local) (Connector, Acceptor, error)
```

- `cmd/renet-client`／`cmd/renet-operator` 的共用宿主通过注册接线决定编进哪些实现；配置文件决定启用哪些、参数是什么。
- 同一模式用于三类扩展点：**Path**（transport）、**Directory**（找对端）、**诊断协议**（车端 Probe / 诊断端 Presenter）。
- `Hello` 握手里带 `capabilities[]`（支持的 transport、协议、功能），双方取交集，新旧版本可以共存。

### 9.2 分层与 Hub 模型（v0.4）

```
┌─ UI / app ───────────────────────────────────────────────────────┐
├─ 诊断协议层（注册表）────────────────────────────────────────────┤
│  车端 Probe:      hsfz | doip | mgu(经 UDS 查 IP) | icom          │
│  诊断端 Presenter: hsfz(6811/6801) | doip(13400) | l3(TUN+netstack)│
├─ Session / Control（握手·心跳·白名单·隧道分发）──────────────────┤
├─ Secure + Mux（设备证书 mTLS，端到端；yamux）─────────────────────┤
├─ Path: hub ──────────────────────────────────────────────────────┤
│  内嵌 Hub（Operator 进程自带，Client 直连 DDNS）  ← 自用          │
│  独立 Hub（renet-hub，云上或自家 PVE，双方出站连） ← 商业版       │
├─ Directory（注册表）─────────────────────────────────────────────┤
│  static(邀请码, M1) | coord(账号·在线·配对码·令牌·配额, 未来)     │
└──────────────────────────────────────────────────────────────────┘
```

**「直连」和「中继」的差别只是谁在 listen**：自用直连 = Client 向 Operator 发起 TCP；商业中继 = Client 和 Operator 都向 relay 发起 TCP，relay 配对两条连接搬字节。于是统一成一个原语 **Hub**：

- Hub 是一个 TLS 监听端。设备连上后出示设备身份，声明「我是 A，要找 B」或「我是 B，等人找」。Hub 把配对成功的两条连接互相搬字节。
- 之后的所有字节是设备间的**端到端 mTLS + yamux**，Hub 看不懂。`secure.WrapStream(conn, id, expectPeer)` 是唯一的加密入口。
- **内嵌模式**：Operator 进程自带 Hub listener（这就是 M1 的 DDNS 直连）。Hub 发现「B 就是我自己」时，直接把连接交给进程内的 session 层，不多绕一跳，速度与裸直连相同。
- **独立模式**：`cmd/renet-hub` 单独部署。Operator 也只出站连 443，不需要公网端口；酒店、公司网、热点都能用。可以先在自家 PVE 上跑一个独立 Hub 验证整条商业链路，不需要云服务器。
- Hub 协议对 Client 完全透明，两种模式一套代码、一套测试。

**端到端加密是硬约束**：Hub 只搬密文。商业版里商家的操作内容车主看不到，Hub 运营方也看不到，既是卖点也是责任边界。

**中继的成本**：MGU 刷写一次可能是 GB 级流量，走独立 Hub 就是运营方的带宽。remote-enet 的「带宽上限 1.0 MB/s」大概率是为此。M4 做带宽上限，商业版按套餐配流量配额。延迟上国内云一个 Hub、两段各 20–40ms，UDS 一问一答能接受。

**Directory 负责「对方在哪」**：

```go
type Directory interface {
	Resolve(ctx context.Context, peer identity.Fingerprint) (PeerInfo, error) // 候选端点列表
	Announce(ctx context.Context, self PeerInfo) error                        // static 实现为空操作
}
```

- `static`：从邀请码/配置读出 `[{direct, "home.example.com:47000"}]`。
- `coord`（未来）：HTTPS + WebSocket 长连接；返回可用的 Hub 地址列表 + 短期令牌。
- 多个 Hub 候选时并发尝试，先成功的赢，其余取消（例如 Operator 内嵌 Hub 优先、独立 Hub 兜底）。

### 9.3 诊断协议插件

车端 `Probe` 和诊断端 `Presenter` 成对出现，但不要求一一对应（`l3` Presenter 不对应任何 Probe，它暴露所有已发现的 IP）。

```go
package carside

type Probe interface {
	Kind() string
	Discover(ctx context.Context, nic NIC, deps Deps) ([]Target, error)
}

// Target 是车端允许被访问的目标，发现结果直接构成白名单。
type Target struct {
	Kind     string      // hsfz-gateway | doip-gateway | ecu
	IP       netip.Addr
	Ports    []PortRule  // {tcp, 6801} {udp, 6811} {tcp, 22} ...；空表示按 profile 默认
	Info     map[string]string // VIN、MAC、DIAGADDR、逻辑地址、ECU 名
}
```

| Probe | 发现方式 | 产出 Target | 阶段 |
|---|---|---|---|
| `hsfz` | UDP 6811 广播 `L=0, 0x0011`，取源 IP + 解析 `DIAGADR`/`BMWMAC`/`BMWVIN` | 网关 `{6801/tcp, 6811/tcp, 6811/udp}` | M1 |
| `doip` | UDP 13400 Vehicle Identification Request（BMW 公开客户端用版本 `03/FC`，通用 parser 不能只认版本 3）；另在 13400/udp 接收上电 Announcement。响应 `type 0004` 正文 = VIN(17) + 逻辑地址(2) + EID(6) + GID(6) + further action(1) [+ sync(1)]，**不含 IP** | 网关 `{13400/tcp, 13400/udp, 3496/tcp(TLS，可能)}`。TCP 侧还有 routing activation / alive / `8001–8003`，隧道透传即可 | 未来 |
| `mgu` | 依赖 `hsfz` 会话，经网关对 `63` 发只读 `22 17 2A`，响应 `62 17 2A 00` 后 12 字节 = 3 个大端 IPv4 值（IP、netmask、第三项语义未定；mgu_kit 文案说「四个」但实现只取三个） | ECU，端口只是候选 `{22/tcp(视状态), 13400?, 6801?}`，实际暴露组合未知，靠 L3 透传 | M4 |
| `icom` | SLPv2 Attribute Request，UDP 427 / 组播 `239.255.255.253`，XID 递增；Reply 含 `DEVTYPE`、`IPADDRESS`、`VIN`、`GATEWAY`、`OWNER` 等 | 网关，HSFZ 映射 `{50160/tcp diag, 50161/tcp ctrl}`、DoIP 映射 `{50162, 50163}`；另有占用/释放管理路径 | 未来 |

```go
package operator

type Presenter interface {
	Kind() string
	Present(ctx context.Context, targets []carside.Target, bind BindConfig, open tunnel.Opener) error
	Teardown() error
	SelfTest(ctx context.Context) error
}
```

| Presenter | 做什么 | 阶段 |
|---|---|---|
| `hsfz` | 在绑定地址上应答 6811/udp（单播回源端口）、监听 6801/tcp 与 6811/tcp 转发到网关。绑定地址是 VM LAN IP 还是 169.254 别名，由 M0b 实测 1 决定 | M1/M2 |
| `doip` | 在绑定地址上应答 UDP 13400（回放原始响应，正文不含 IP）、转发 Announcement、监听 TCP 13400（及 3496）透传 | 未来 |
| `l3` | 跨平台TUN＋netstack，先对已知ECU配置/32路由及TCP转发；UDP/发现后续补齐。更大子网路由和车侧IP镜像另行验证 | Wave 8起，逐步推进M4 |
| `icom` | SLP 应答需改写 `IPADDRESS`；加上 ICOM 占用/释放的管理连接 | 未来 |

**白名单策略（v0.4 简化）**：车端放行车侧网卡所在子网内的任意 `ip:port`，不依赖 Probe 的端口规则，也没有任何用户可见的开关。`Target.Ports` 只给 Operator 端的 Presenter 用（决定监听什么）。

### 9.4 MGU HTTP 刷写与 SSH：为什么要 L3

- HTTP 刷写时 E-Sys/ISTA 直接连 MGU 自己的 IP，而不是经网关；SSH 到 MGU 同理。两者本质都是「诊断端要访问车内任意 `ip:port`」。
- `l3` Presenter在目标发现、路由及协议支持齐备后，可统一暴露真实IP和端口；是否无需修改具体工具，必须逐个验收。
- HTTP 回连方向（ECU → tester 的 HTTP server，端口按会话实测）：IP 镜像的设想是让 E-Sys 对外宣称的地址等于车端网卡 IP，ECU 回连落到车端 `ReverseListener`，反向隧道到诊断端本地 HTTP server。**这是条件性假设**（4.5）：URL 通告机制和 E-Sys 的本地 IP 选择算法未知。remote-enet 的「E-Sys HTTP / ISTA HTTP / 关」开关按其页面原话是决定哪个 tester 拿到 link-local alias 发现地址，不是切换本地端口。

### 9.5 多人商业服务（未来形态，现在只保证接口形状）

2026-11-19 的设计基线见 [DEPLOYMENT-MODES](DEPLOYMENT-MODES.md) 与 [CENTER-PROTOCOL](CENTER-PROTOCOL.md)。下文保留早期产物设想作为历史：中心服务不再作为本仓库的 `cmd/renet-coord` 产物，本仓库只提供 Hub、票据验签与客户端侧协议；Hub 的“无状态”仅指无需账号数据库，仍需运行状态与强制票据校验。授权契约和数字配对尚待实现。

**商业版只走独立 Hub，不提供直连**（v0.4 决定）。DDNS 直连（内嵌 Hub）只是自托管/自用形态。这样商业版的流量全部经过服务器，计量、配额、独占、审计都有落点；也避免了打洞不可靠的问题。

新增两个服务端产物，同一仓库：

| 产物 | 职责 | 备注 |
|---|---|---|
| `cmd/renet-hub` | 独立 Hub：按令牌放行、配对两条 TLS 连接互相搬字节、按字节计量 | 无状态，可多地域水平扩展；与内嵌 Hub 共用 `internal/hub` |
| `cmd/renet-coord` | 账号、设备公钥登记、配对码签发、在线状态、Hub 地址分配、令牌签发、配额与计费 | 有状态，一个即可；早期可以和 hub 合并部署 |

- 设备身份不变（本机密钥对）；coord 把账号映射到设备并授权使用 Hub。设备间仍是端到端 mTLS。
- Client 免费免注册、Operator 付费（与 remote-enet 一致）。配对码从自包含长邀请码变成 coord 签发的短码。
- **许可在服务端执行**（令牌是否签发、配额）；客户端本地校验挡不住人。
- 单会话：一个 Operator 同时只接一个 Client；中心模式的账号级并发额度与跨 Hub 并发限制由后续 coord / Hub 授权协议共同约束，不能仅靠各 Hub 的本地占位实现全局限制（见 9.8）。
- 安全：令牌短期有效；按租户限速和流量配额；审计只记元数据（谁、何时、多少字节），不记内容；车端 UI 始终显示当前连着哪个 Operator，并有「停止中继」。

**现在要做的只有**：`internal/hub` 从第一天起就是独立包（内嵌模式只是 Operator import 它），`Directory`、`PeerInfo.Endpoints[]`、`Hello.capabilities` 的形状定对，`secure.WrapStream` 独立于 Path。coord 进来时只新增包，不改 session/tunnel。

### 9.6 这一节里的决策记录（建议落成 ADR）

1. 插件 = 编译期注册表，不用动态加载。
2. 加密在 Path 之上、端到端；Hub 只搬密文。
3. Directory 与 Path 分离。
4. 诊断端 L3 暴露优先于逐端口；车端永远不做 L3。
5. 直连与中继统一为 Hub（内嵌 / 独立）；不做 QUIC、不做 P2P 打洞。
6. 商业版只走独立 Hub；直连只是自托管形态。
7. 独占粒度是进程，可开关，默认开。
8. 车端是黑盒中继（9.7）。

### 9.7 车端黑盒原则（v0.4）

车主不需要、也不应该看到隧道内容；商家也不希望操作被记录。因此：

- 车端**不解析**任何隧道字节，不做帧级日志，不做操作审批或解释。
- 车端界面只有：连接状态、对端名字/指纹、车辆基本信息（VIN、网关、网卡）、流量计数、「停止中继」、事件日志（连接/断开/发现/错误）。
- 白名单只是防跳板的底线（4.3），不是授权 UI。
- HSFZ 帧解析、UDS 日志、verbose 视图只在 Operator 端。
- 端到端加密保证 Hub 运营方同样看不到内容。

### 9.8 单会话与命名（v0.5）

- **一个 Operator 同时只有一个会话。** E-Sys/ISTA/Tool32 本来就不能多车并行，多车并行的商家用多台电脑、多个账号。
  - Presenter 是全局单例，不是每会话一份；「暴露地址」只有一套。
  - Operator 的会话列表 = 当前会话 + 最近配对过的 Client（来自 `trusted_peers`），不是并行会话。
  - 第二个 Client 连入时，Hub 直接回 `busy`；Client 端显示「Operator 忙」并定时重试，不排队、不抢占。
  - 中心模式（2026-11-19 修订）：一个账号可在多台 Operator 设备登录，不绑定单设备；同时进行的接待数受账号级并发额度约束，额度跨所有 Hub 统一计算。当前 Hub 只按设备在单实例内占位；中心模式由中心原子占位、签发绑定 Hub 的受限票据，见 [CENTER-CONTRACT](CENTER-CONTRACT.md)。
- **命名采用 remote-enet 的 Client / Operator**（车端 / 诊断端），代码包 `carside/` 与 `operator/`。不再用 "Tester" 称呼诊断端，因为在 UDS/HSFZ 语境里 "tester" 是协议术语（源地址 `F4` 那一方），容易混淆；文档里保留 "UDS tester 地址" 这种用法仅指协议字段。

### 9.9 分发、更新与控制平面（v0.5）

**两个平面分离**，部署、成本、可用性要求都不一样：

| 平面 | 承载 | 特点 | 部署 |
|---|---|---|---|
| 控制/分发平面 | 下载、更新 manifest、账号、设备登记、配对码、Hub 列表、令牌签发 | 短请求/响应；配对与并发需原子协调 | 按 [CENTER-PROTOCOL](CENTER-PROTOCOL.md) 实现的中心；官方中心的部署不在本仓库描述 |
| 数据平面 | Hub：长连接 TLS、实际转发流量 | 沿用原生 TCP 监听，独立承担带宽与并发 | Go 单二进制，普通云服务器，国内与海外分别部署 |

**分发与更新：沿用 mgu_kit 的体系**，它已经解决了这个项目会遇到的所有问题，可直接复制的部分：

- `tools/build-release.sh`（garble + `-X` 注入 mygo 的 package/version/feed/pubkey）、`tools/publish-update/main.go`（manifest `{version, notes, date, url, size, signature}`，Ed25519 签 SHA-256）、`tools/release.sh --from-ci`、`.github/workflows/release.yml`。
- Worker 的 `/dl-*`、`/updates/*` 路由；R2 布局 `download/`、`updates/`、各自的 `nightly/`。
- 原则：**CI 只构建，签名和发布在本地**，更新私钥不进 CI；GitHub Release 只是归档，用户不从那里下载。
- stable / nightly 两通道；中国大陆请求的大包 302 到火山 TOS 镜像，manifest 留在 R2。
- mygo 版本：mgu_kit 钉 v0.7.1 并用 `go:linkname` 改写 `packageUpdateFeed` 切通道。本仓库 Wave 5 已锁定 mygo v0.9.1，使用 Native UI 与 `mygo_nowebview` 构建；Go 最低版本为 1.27.3。自动更新尚未接入，届时先确认该版本 updater 的运行时 feed API，不直接照搬旧版 `go:linkname`。
- Client 和 Operator 是两个 package identifier、两条 feed，但共用一把更新签名密钥和一套发布脚本。

**不复制的部分**：mgu_kit 的 license 包、exe 完整性签名槽、加密资源包、Pro 分级。原因见下。

**中心职责**：账号、设备登记、数字码、节点分配与票据签发由中心统一管理；数字码核销和跨 Hub 并发须原子保证，不依赖最终一致的存储。中心签发受设备、角色、对端、会话、Hub 与期限限制的票据，Hub 用中心公钥本地验证，并按租约与撤销策略执行。Hub 无账号数据库，但保留连接、占位和限流等内存状态，定期向中心报告心跳、负载与用量。具体约束见 [CENTER-PROTOCOL](CENTER-PROTOCOL.md)，不能把本地验签等同于无限离线续用或即时全局吊销。

**Hub 区域与部署条件**：Operator 选择节点后，数字码将两端绑定到同一个 Hub；跨区线路需结合两端实测，运行中的诊断不静默切节点。国内部署条件按服务形态和云厂商要求另行核实，不把更换端口当作合规依据。

**服务资格由服务端执行**：中心签发受限票据，Hub 强制验签并执行额度和租约，不能只靠可修改的客户端判断有效期。改用自建中心或自建 Hub 是本项目支持的合法用法，不属于需要阻止的绕过；设备身份校验也不等同于证明程序未被篡改。威胁模型见 [CENTER-PROTOCOL 第 10 节](CENTER-PROTOCOL.md#10-威胁模型)。

- 官方发行版与社区构建使用同一份功能代码，不裁剪内嵌 Hub 或自建能力；差异只在默认中心地址、代码签名与更新通道（见 [DEPLOYMENT-MODES 第 6 节](DEPLOYMENT-MODES.md#6-中心地址可配置)）。
- Client 免费、同一个二进制通用。
- 正式代码签名与签名更新用于可信分发，不能替代服务端授权，也不能以防复制为由改变车端黑盒或端到端加密边界。

**代码签名**：mgu_kit 和 remote-enet 都没签，SmartScreen/Gatekeeper 会拦首次启动。Client 面向车主，这会直接影响商业版的转化，商业化前预算 Windows OV 证书 + Apple Developer 账号；自用阶段不签。

**中心入口的可达性**：小体积 API 不保证在所有网络下可达，Hub 部署在哪里也不解决中心访问问题。可预留随客户端分发的可信备用入口（如 HTTPS API 代理）；代理不复制授权事实、不自行签发许可，中心故障时也不能替代中心。入口回退、写请求幂等与故障边界见 [CENTER-PROTOCOL 第 8 节](CENTER-PROTOCOL.md#8-api-入口与回退)。
