# 包契约（Wave 1）

Wave 1 已整合；应用层接口见 [Wave 2](WAVE2.md)，广播 socket 与 CI 稳定性改动见 [Wave 3](WAVE3.md)，app 运行时、观测、专用连接测试与 Windows 独占的新增契约见 [Wave 4](WAVE4.md)。本文件保留原始基础接口约定，后续新增字段与能力以 Wave 4 及实际包 README 为准。发现状态中的 `IdentRaw` 在应用层明确表示包含六字节 HSFZ 头的完整 UDP 数据报，`hsfz.Identification.Raw` 仍表示正文。

TUN数据包设备、IPv4/TCP用户态呈现、跨平台设备/路由所有权与手动验收契约见 [Wave 8](WAVE8.md)，新增包为 `packetio`、`l3`、`tunadapter`。现有基础协议、会话与生产HSFZ路径继续沿用，不把实验性工具的本机fixture当作真实车辆或mgu_kit兼容性证据。

Operator 本地 HSFZ 记录器、成功写入 Tap、Controller/GUI/CLI 接线见 [Wave 12](WAVE12.md)，新增独立叶子包 `hsfzcapture`；用户操作与独立 JSONL schema v1 见[抓取说明](CAPTURE.md)。Client/Hub 不解析载荷，既有网络和配置格式保持不变。

两端手动连接自检见 [CHECKUP](CHECKUP.md) 和 [Wave 15](WAVE15.md)。`checkup.Run` 只观察已保存设置与公共端点，不进入 session/carside 业务流程；`trust.ReadPeer` 不创建存储或消费 token，`tunadapter.InspectAuto` 与创建路径复用冲突规则但不调用原生创建/配置。独立 `checkup/v1` 报告不改变匿名诊断 schema。

本文件是并行实现的接口合同。已经存在并且**不要改动**的基础：`go.mod`（module `remote-enet`）、`internal/transport/transport.go`、`internal/identity/`。需要基础层新增能力时，在报告里写出期望签名，不要自己改。

依赖方向：`cmd → {carside, operator} → {session, tunnel} → transport → identity`；`secure`、`hub`、`transport/hub` 在 transport 之下实现它。`hsfz` 是独立叶子包。

## A. `internal/hsfz` + `internal/testutil/fakezgw` + `cmd/fakezgw`

**依据**：`docs/enet-protocol-research.md` Q1、Q2、Q3 与「假 ZGW 最小规格」。

### `internal/hsfz`（叶子包，只依赖标准库）

```go
// 帧：外层 [uint32_be L][uint16_be Control][Body: L bytes]，总长 6+L。
type Control uint16
const (
    ControlDiag          Control = 0x0001
    ControlAck           Control = 0x0002
    ControlTerminal15    Control = 0x0010
    ControlVehicleIdent  Control = 0x0011
    ControlAliveCheck    Control = 0x0012
    ControlStatusInquiry Control = 0x0013 // on 6801 this is the alive response
    ControlErrIncorrectTesterAddress Control = 0x0040
    ControlErrIncorrectControlWord   Control = 0x0041
    ControlErrIncorrectFormat        Control = 0x0042
    ControlErrIncorrectDestAddress   Control = 0x0043
    ControlErrMessageTooLarge        Control = 0x0044
    ControlErrAppNotReady            Control = 0x0045
    ControlErrOutOfMemory            Control = 0x00FF
)
type Frame struct { Control Control; Body []byte }
func (f Frame) MarshalBinary() ([]byte, error)
// ReadFrame 从流中读一帧，容忍拆包/合包；L 超过 maxBody 返回错误而不是分配。
func ReadFrame(r io.Reader, maxBody int) (Frame, error)
func WriteFrame(w io.Writer, f Frame) error

// 只对 ControlDiag / ControlAck 有意义。
type DiagBody struct { Src, Dst byte; Payload []byte }
func ParseDiagBody(body []byte) (DiagBody, error)
func (d DiagBody) Marshal() []byte

// 发现
var IdentRequest = []byte{0, 0, 0, 0, 0, 0x11}
type Identification struct { DiagAddr byte; MAC [6]byte; VIN string; Raw []byte }
func ParseIdentification(body []byte) (Identification, error) // 标记 DIAGADR/BMWMAC/BMWVIN，相邻 ASCII，无分隔符；容忍未知扩展字段；DIAGADR 是 2 字符十六进制
func (id Identification) MarshalBody() []byte               // 重算 L 由 Frame 负责
// Alive response: 00 00 00 02 00 13 00 <tester>
func AliveResponse(tester byte) Frame
// ACK: 地址不交换，payload 取请求前 min(len,5) 字节
func AckFor(req DiagBody) Frame
```

测试：rawenet 的 56 字节发现 fixture（报告 Q1.1 给了十六进制）；ACK 示例 `00 00 00 05 00 01 F4 63 22 F1 01` → `00 00 00 05 00 02 F4 63 22 F1 01`；alive `L=32` 与 `L=2` 两种；`0x0041 L=0` 空正文帧可解析；拆包/合包用 `iotest`。

### `internal/testutil/fakezgw`

进程内可启动的假网关，供其他包的 e2e 测试和 M0b 台架使用。

```go
type Profile struct {
    Ident            hsfz.Identification // VIN/MAC/DiagAddr 可配
    TesterAddr       byte                // 期望的 alive 应答末字节，默认 0xF4
    AliveInterval    time.Duration       // 0 = 不发
    AliveTimeout     time.Duration       // 未收到 0x0013 则断开；0 = 不断
    AliveLongBody    bool                // true: L=32 ASCII 正文；false: L=2
    MaxConns         int                 // 0 = 不限
    Responders       map[string][]byte   // key = hex(UDS request bytes)，value = UDS 响应；未命中 → 超时或 NegativeResponse（见 Unknown）
    Unknown          UnknownPolicy       // Timeout | NegativeResponse(NRC)
    Faults           []Fault             // 注入：RST、半关闭、发送 0x0041 L=0、发送 0x00FF
}
type Server struct{ ... }
func Start(ctx context.Context, bind netip.Addr, p Profile) (*Server, error) // 监听 bind:6811/udp、bind:6801/tcp、bind:6811/tcp；端口可通过 Option 覆盖以便测试用临时端口
func (s *Server) Addr() (diag, ctrl *net.TCPAddr, disc *net.UDPAddr)
func (s *Server) Stats() Stats   // 连接数、收到的 diag 数、发出的 ACK/alive 数、收到的 0x0013 数
func (s *Server) Close() error
```

行为（报告「必须实现的确定行为」表）：6811/udp 收到精确 `IdentRequest` 才回 ident，**单播回源 IP:port**；6801 对合法 `0x0001` 先回 ACK 再按 `Responders` 回 `0x0001`（地址交换）；每条连接独立缓冲与 alive 状态；6811/tcp 对 `0x0010` 回配置的 KL15 字节。**禁止**对未知请求默认成功。

### `cmd/fakezgw`

CLI：`fakezgw --bind 192.168.1.50 --vin ... --alive 5s --profile normal|no-alive|drop-alive|long-alive|error-frame`，slog 打印每个连接/帧到 stderr（含十六进制），用于在 Windows VM 上做 M0b 实测 1–3。

---

## B. `internal/invite` + `internal/secure`

### `internal/invite`

邀请码 `renet1:` + base32（无填充、大小写不敏感），正文为 CBOR（见 D 的依赖说明）或自定义紧凑二进制——**自己选，但要写在 doc comment 里并有向量测试**。

```go
type Invite struct {
    Version   uint8
    Peer      transport.PeerInfo // ID + Endpoints（含 Hub 指纹）
    Token     [16]byte           // 一次性配对令牌
    ExpiresAt time.Time
}
func (i Invite) Encode() (string, error)
func Decode(s string) (Invite, error) // 校验前缀、版本、过期（允许传入 now 的变体用于测试）
```

### `internal/secure`

把一条裸 `net.Conn` 变成 `transport.Session`：TLS 1.3 双向认证（双方都用 `identity.Local.Cert`，`InsecureSkipVerify=true` + `VerifyPeerCertificate` 里做 **Ed25519 证书指纹钉扎**，禁止 TLS 1.2），上面跑 `github.com/hashicorp/yamux`（这是本包唯一允许的第三方依赖）。

```go
type Opts struct {
    Local      *identity.Local
    Expect     identity.Fingerprint // 非零：对端指纹必须相等；零：接受任何有效 Ed25519 自签证书，Peer() 返回实际值（首次配对用，由 session 层校验 token）
    IsClient   bool
    Path       transport.PathInfo
    ALPN       string // 默认 "renet/1"
    KeepAlive  time.Duration // yamux keepalive，默认 15s
}
func WrapStream(ctx context.Context, conn net.Conn, o Opts) (transport.Session, error)
```

Session 实现要求：`OpenStream/AcceptStream` 尊重 ctx；`Stream.CloseWrite` 映射到 yamux 的半关闭；`Stats().RTT` 用 yamux 的 Ping 周期测量；`Close(reason)` 后 `Done()` 关闭、`Err()` 返回 reason（nil 时返回 `io.EOF` 一类的明确值）。

整合约定：`Stream.Close` 必须解除本地阻塞读写，与保留读取方向的 `CloseWrite` 区分。`WrapStream` 的 ctx 贯穿会话生命周期，不能在建立连接后立即取消。TLS 1.3 握手后、yamux 启动前，服务端发送加密的单字节 `0x01` 确认；客户端收到前不发送应用数据，两端须统一使用 `WrapStream`。

测试：`net.Pipe` 两端各一个身份；正确指纹通过、错误指纹在握手阶段失败且**不发送任何应用数据**；非 Ed25519 证书被拒；双向多 stream 并发 + 半关闭；一端 Close 后另一端 Done。

---

## C. `internal/hub` + `internal/transport/hub` + `internal/directory`

**依据**：DESIGN.md 9.2。Hub 是 TLS 监听端，设备连上后声明「我是 A，要找 B」或「我是 B，等人找」，Hub 配对两条连接互相搬字节。内层字节（B 的 e2e TLS）Hub 看不懂，**hub 包里不能 import secure**。

### Hub 外层协议（由你定义并写进 `internal/hub/PROTOCOL.md`）

要求：外层 TLS 用 Hub 的 `identity.Local.Cert` 做服务端证书、设备证书做客户端证书（Hub 从客户端证书取设备指纹，不信任任何自报身份）；ALPN `renet-hub/1`；握手后一条小的定长/长度前缀头：`{role: dial|wait, target: Fingerprint, token: []byte(可选，独立 Hub 鉴权用，内嵌模式忽略)}`；Hub 回 `{status: ok|busy|unknown-peer|unauthorized}`。`ok` 之后双方裸字节对拷直到任一方关闭。**单会话规则**：一个 wait 方同时只配一个 dial 方，第二个 dial 得 `busy`。

### `internal/hub`

```go
type Options struct {
    Local *identity.Local
    // Authorize 决定哪些设备可以 wait / dial；nil = 全部允许（自用内嵌模式）。
    Authorize func(dev identity.Fingerprint, role Role, target identity.Fingerprint) bool
    // 可选的独立 Hub 凭证校验；内嵌 self-dial 不校验 Token。
    AuthorizeToken func(dev identity.Fingerprint, role Role, target identity.Fingerprint, token []byte) bool
    // Embedded != nil 时，目标 == Local.Fingerprint() 的 dial 不走配对，直接交给进程内 Acceptor。
    Embedded *Embedded
    Logger *slog.Logger
}
type Server struct{ ... }
func NewServer(o Options) *Server
func (s *Server) Serve(ctx context.Context, ln net.Listener) error
// Embedded 是进程内的接收端：Accept 交出已剥掉外层头、仍是外层 TLS 内明文（即内层 e2e TLS 的裸字节）的 net.Conn 和对端设备指纹。
type Embedded struct{ ... }
func NewEmbedded() *Embedded
func (e *Embedded) Accept(ctx context.Context) (net.Conn, identity.Fingerprint, error)
```

### `internal/transport/hub`

注册 `transport.Register("hub", ...)`。为了不依赖 B，内层包装通过函数注入：

```go
type Wrap func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error)
type Options struct {
    Local *identity.Local
    Wrap  Wrap
    // Embedded 非 nil 时 Acceptor 从它取连接；Listen 非空时 Acceptor 还会自己起一个内嵌 hub.Server 监听该地址。
    Embedded *hub.Embedded
    Listen   string
    // 独立 Hub：Acceptor 主动注册 wait；Token 为 Hub 凭证，与 PairToken 不同。
    Endpoints []transport.Endpoint
    Token     []byte
}
func New(o Options) (transport.Connector, transport.Acceptor, error)
func Register(wrap Wrap) // 应用启动时显式注入后注册，配置入口支持 listen
```

Connector.Dial：对 `peer.Endpoints` 中 `Kind=="hub"` 的候选并发尝试（happy-eyeballs 风格，先成功者赢，其余取消）；外层 TLS 钉 `Endpoint.Hub`；发 `dial target=peer.ID`；收到 `ok` 后调用 `Wrap(conn, isClient=true, expect=peer.ID, path)`，`PathInfo.Kind` 内嵌 = direct、独立 = relay。`busy` 映射成可识别的错误类型 `ErrPeerBusy`。

### `internal/directory`

```go
type Directory interface {
    Resolve(ctx context.Context, peer identity.Fingerprint) (transport.PeerInfo, error)
    Announce(ctx context.Context, self transport.PeerInfo) error
}
// static：内存表 + 可选 TOML/JSON 文件持久化（trusted_peers）。Announce 为空操作。
func NewStatic(entries ...transport.PeerInfo) *Static
```

测试：Hub 在 loopback 起，三个身份（hub、A、B）；B wait、A dial → 两端对拷字节一致；第二个 dial 得 busy；dial 未知目标得 unknown-peer；非 Ed25519 客户端证书握手失败；内嵌模式下 target==self 走 Embedded.Accept 且 Path 为 direct。Wrap 用一个把 `net.Conn` 直接包成最小 Session 的测试桩（**只在 `_test.go` 里**）。

---

## D. `internal/proto` + `internal/session` + `internal/tunnel` + `internal/testutil/memtransport`

**依据**：DESIGN.md 4.3。

### `internal/proto`

控制消息编码：`uint32_be length + CBOR`，用 `github.com/fxamacker/cbor/v2`（本包唯一第三方依赖；结构体打 `cbor:"1,keyasint"` 之类的整数键以保持紧凑和可演进）。消息集合（第一版）：

| 消息 | 方向 | 字段 |
|---|---|---|
| `Hello` | 双向 | `ProtoVersion uint16`、`Role (client|operator)`、`AppVersion string`、`Capabilities []string` |
| `PairToken` | client→operator | `Token [16]byte` |
| `PairResult` | operator→client | `Accepted bool`、`Reason string` |
| `VehicleState` | client→operator | `NIC string`、`NICAddr string`、`GatewayAddr string`、`VIN`、`MAC`、`DiagAddr byte`、`IdentRaw []byte`、`Seq uint64` |
| `Ping` / `Pong` | 双向 | `Seq uint64`、`SentAt int64(unix nano)` |
| `ExposeReverse` | operator→client | `Port uint16`、`Enable bool` |
| `ExposeResult` | client→operator | `Port uint16`、`OK bool`、`Reason string` |
| `Log` | client→operator | `Level`、`Msg`、`Attrs map[string]string`、`At int64` |

`OpenTunnel` 头（每条数据 stream 的第一个消息，同样编码）：`Kind (tcp|reverse)`、`Target string ("ip:port")`、`Origin string`。

```go
type Envelope struct { Type MsgType; ... } // 或 tagged union，自己选，但解码未知 Type 不得 panic
func Write(w io.Writer, msg any) error
func Read(r io.Reader, maxLen int) (any, error)
```

### `internal/session`

一条 `transport.Session` 上的协议状态机。stream 0（第一个由 dial 方打开的 stream）是 control stream。

```go
type Role string
const ( RoleClient Role = "client"; RoleOperator Role = "operator" )
type Options struct {
    Initiator   bool // 谁打开 control stream，与 Role 无关；双方必须相反
    Role        Role
    AppVersion  string
    // Operator 侧：校验首次配对 token；返回 true 则把对端指纹加入 trusted。已信任的对端不需要 token。
    Trusted     func(peer identity.Fingerprint) bool
    AcceptToken func(peer identity.Fingerprint, token [16]byte) bool
    // Client 侧：要发送的 token（零值 = 不发，已配对）。
    Token       [16]byte
    // 收到对端 OpenTunnel 时的处理（client 侧会接；operator 侧接 reverse）。handler 负责关闭 stream。
    OnTunnel    func(ctx context.Context, hdr proto.OpenTunnel, s transport.Stream)
    OnMessage   func(msg any) // VehicleState / Log / ExposeResult 等非请求响应式消息
    PingInterval time.Duration // 默认 1s
    Logger *slog.Logger
}
type Session struct{ ... }
func Run(ctx context.Context, ts transport.Session, o Options) (*Session, error) // 完成 Hello 交换与配对后返回；之后在后台跑 ping、分发 stream；ctx 取消或底层 Done 时结束
func (s *Session) Send(msg any) error
func (s *Session) OpenTunnel(ctx context.Context, hdr proto.OpenTunnel) (transport.Stream, error)
func (s *Session) RTT() time.Duration
func (s *Session) Peer() identity.Fingerprint
func (s *Session) Done() <-chan struct{}
func (s *Session) Err() error
```

规则：主版本不同 → 握手失败并返回可识别错误；operator 对未信任且 token 错误的 client 立即 Close；握手超时 10s；重连不在本包（上层循环负责）。

### `internal/tunnel`

```go
type Stats struct { BytesIn, BytesOut atomic.Uint64; StartedAt time.Time }
// Pipe 双向拷贝直到两个方向都 EOF 或 ctx 取消；一侧 EOF 时对另一侧 CloseWrite，不提前 Close 整条连接。
func Pipe(ctx context.Context, a transport.Stream, b net.Conn, st *Stats) error
```

### `internal/testutil/memtransport`

给 D 自己和后续包的测试用：基于 `net.Pipe` + yamux 的进程内 `transport.Session` 对（可以依赖 yamux；不要依赖 B 的 secure）。`func Pair(t testing.TB, a, b identity.Fingerprint) (transport.Session, transport.Session)`。

测试：Hello 版本不匹配；配对成功/失败；ping RTT 有值；多条 tunnel 并发、半关闭语义（一端 CloseWrite 后另一端读到 EOF 但仍能写）；未知消息类型被忽略而非崩溃；底层 Done 后 Session.Done 关闭。

---

## R2. 直连数字配对（2026-11-19 增补）

设计与安全论证见 [PAIRING](PAIRING.md)。以下只记录跨包接口，实现细节以代码为准。

### `internal/pairing/cpace`（新增）

CPace-ristretto255-SHA512，固定草案 -21，套件标识 `cpace21-r255-sha512-ir`。`Binding` 携带内层 TLS exporter、Client / Operator 完整指纹和双方原始 Hello 帧的 SHA-256，任一为零即拒绝。`Start` / `Respond` / `Initiator.Confirm` / `Initiator.Verify` / `Responder.VerifyAndConfirm` 的状态都只能用一次。`Reason` 是跨包共享的拒绝原因枚举。

### `internal/pairing`（新增）

`NewManager(Config) *Manager`：每个 Operator 监听一个，口令只存在于内存。`Issue` / `Status` / `Display(generation)` / `Revoke(ctx, generation)` / `Begin(ctx)` / `Close`。`Attempt` 在包内持有口令并完成 CPace 响应，状态为 attempting → committing → redeemed / revoked；`Commit(save)` 是成功授权的线性化点，save 在锁外使用独立的有界 context。`Status` 不含口令。

### 既有包的新增接口

| 包 | 新增 |
|---|---|
| `internal/trust` | `Store.Remember(ctx, fp)`：设置 Trusted，文件格式仍为版本 1 |
| `internal/identity` | `ErrFingerprintMismatch`，由 `hub`、`secure` 包装返回 |
| `internal/hub` | `ConnectBootstrap(ctx, addr, local) (net.Conn, identity.Fingerprint, error)`：在同一条外层连接上读取 H 并自拨；`Connect` 仍拒绝零指纹。`SetupLimits` 限制未认证 setup 的并发与速率 |
| `internal/secure` | 内层握手后导出 `EXPORTER-renet-pair-direct-v1`，session 提供 `PairingExporter() [32]byte` |
| `internal/transport/hub` | `New` 返回 `*Connector`，新增 `DialDirectPair(ctx, addr)`；包装后的 session 保留 `PairingExporter` |
| `internal/proto` | `PairShareA`=11、`PairShareB`=12、`PairConfirmA`=13、`PairConfirmB`=14，只能用 `ReadPair` 严格解码；Hello 能力 `pair-direct/1` |
| `internal/session` | `Options.DigitalCode` / `DigitalManager`（`DigitalResponder`）/ `LocalFP` / `Remember`；`(*Session).PairingOutcome()` 区分 `Legacy` 与 `DigitalPaired`。数字模式下 `PairResult(true)` 视为协议错误 |
| `internal/invite` | `ParseInput` 区分 `Raw` / `Direct` / `Center`；`ParseAddress`；错误为固定文案，不含输入 |
| `internal/app` | `ClientConfig.DirectPair`、`ClientHooks.OnPaired`、`OperatorConfig.ShareAddress`；`Runtime.IssueNumericCode` / `NumericCodeStatus` / `NumericCodeDisplay` / `RevokeNumericCode`；`PairingErrorKind` |

---

## 通用要求

- 分支名 `wave1/<A|B|C|D>-<slug>`；只改分配给自己的目录 + `go.mod/go.sum`。
- 完成标准：`gofmt -l .` 为空、`go vet ./...`、`go test -race ./...` 全绿；在最终报告里列出新增依赖、对其他包的期望签名、以及未覆盖的边界。
- 不要 push、不要开 PR；整合者负责合并。
