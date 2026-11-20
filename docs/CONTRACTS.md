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
    // R3 已删除 Authorize / AuthorizeToken，改为 Options.Policy（见下文 R3 节）。
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
    // 独立 Hub：Acceptor 主动注册 wait。R3 已删除静态 Token，改为 WaitToken / OnRegister（见下文 R3 节）。
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

CPace-ristretto255-SHA512，固定草案 -21，套件标识 `CPACE-RISTR255-SHA512`。`Binding` 携带内层 TLS exporter、Client / Operator 完整指纹和双方原始 Hello 帧的 SHA-256。Client 的 Start / Reveal / Finish 和 Operator 的 Respond / Open / Verify 的状态都只能用一次。`Reason` 是跨包共享的拒绝原因枚举。

### `internal/pairing`（新增）

`NewManager(Config) *Manager`：每个 Operator 监听一个，口令只存在于内存。`Issue` / `Status` / `Display(generation)` / `Revoke(ctx, generation)` / `Begin(ctx)` / `Close`。`Attempt` 在包内持有口令并完成 CPace 响应，状态为 attempting → committing → redeemed / revoked；`Commit(save)` 是成功授权的线性化点，save 在锁外使用独立的有界 context。`Status` 不含口令。

### 既有包的新增接口

| 包 | 新增 |
|---|---|
| `internal/trust` | `Store.Remember(ctx, fp)`：设置 Trusted，文件格式仍为版本 1 |
| `internal/identity` | `ErrFingerprintMismatch`，由 `hub`、`secure` 包装返回 |
| `internal/hub` | `ConnectBootstrap(ctx, addr, local) (net.Conn, identity.Fingerprint, error)`：在同一条外层连接上读取 H 并自拨；`Connect` 仍拒绝零指纹。`SetupLimits` 限制未认证 setup 的并发与速率 |
| `internal/secure` | 内层握手后导出 `EXPORTER-renet-pake-sid-v1`，session 提供 `PairingExporter() [32]byte` |
| `internal/transport/hub` | `New` 返回 `*Connector`，新增 `DialDirectPair(ctx, addr)`（R3 已由 `DialBootstrap` 取代并删除）；包装后的 session 保留 `PairingExporter` |
| `internal/proto` | 11 PakeCommit、12 PakeShareB、13 PakeShareA、14 PakeConfirmB、15 PakeConfirmA、16 PairDecision，只能用 `ReadPair` 严格解码；Hello 能力 `pair-direct/1` |
| `internal/session` | `Options.DigitalCode` / `DigitalManager`（`DigitalResponder`）/ `LocalFP` / `Remember` / `ConfirmSAS`（nil 自动通过）；`(*Session).PairingOutcome()` 区分 `Legacy` 与 `DigitalPaired`。Operator 确认并落盘后发 16 accepted，Client 确认并收到该决定后才报告成功 |
| `internal/invite` | `ParseInput` 区分 `Raw` / `Direct` / `Center`；`ParseAddress`；错误为固定文案，不含输入 |
| `internal/app` | `ClientConfig.DirectPair`、`ClientHooks.OnPaired`、`OperatorConfig.ShareAddress`；`Runtime.IssueNumericCode` / `NumericCodeStatus` / `NumericCodeDisplay` / `RevokeNumericCode`；`PairingErrorKind` |

---

## R3. 自建 Hub 与中心地址配置（2026-11-20 冻结）

设计、票据字段、Hub 校验步骤与审查处理见 [SELFHOST](SELFHOST.md)；中心模式的相关条文见 [CENTER-CONTRACT §8.2、§10.7、§16、§17](CENTER-CONTRACT.md#18-修订记录)。以下签名取自实现，以代码为准。R3 删除的接口：`hub.Options.Authorize` / `AuthorizeToken`、`transport/hub.Options.Token`、`(*Connector).DialDirectPair`；不保留兼容层。

### `internal/ticket`（新增，自建票据 v = 2）

```go
const ( WaitTTL = 10 * time.Minute; DialTTL = 30 * 24 * time.Hour )
type Kid [8]byte                                   // SHA-256(公钥) 的前 8 字节
func KidOf(pub ed25519.PublicKey) Kid
type Claims struct {
    Device, Peer, Hub                       identity.Fingerprint
    Role                                    uint8     // 1 = dial，2 = wait
    IssuedAt, NotBefore, Expires, PairUntil time.Time // 零值的 Device / Peer / PairUntil 在签名时省略
    JTI                                     [16]byte
    Bearer                                  bool
}
func Sign(priv ed25519.PrivateKey, c Claims) ([]byte, error)   // 先按字段表校验，秒级精度
func Verify(raw []byte, key func(Kid) (ed25519.PublicKey, bool)) (Kid, Claims, error) // A2–A5；验签通过前不解释 payload
func Inspect(raw []byte) (Kid, Claims, error)                  // 不验签，只供提示与运维展示
func CheckTime(c Claims, now time.Time, skew, maxTTL, bootstrapMaxTTL, pairingWindowMax int64) error // A10
type Issuer struct{ ... }                                       // 签发密钥，与设备身份密钥分开
func LoadOrCreateIssuer(path string) (*Issuer, error)          // PKCS#8 PEM；硬链接独占发布，不支持时回退 O_EXCL；非 Windows 检查权限位
func (i *Issuer) PublicKey() ed25519.PublicKey
func (i *Issuer) Kid() Kid
func (i *Issuer) Wait(now time.Time, dev, hub identity.Fingerprint, pairUntil time.Time) ([]byte, error) // pairUntil 为零则不带窗口
func (i *Issuer) Dial(now time.Time, client, operator, hub identity.Fingerprint) ([]byte, error)
func (i *Issuer) Bootstrap(now, expires time.Time, operator, hub identity.Fingerprint) ([]byte, [16]byte, error) // 返回不记名票据与 jti
```

票据向量由 `internal/ticket/testdata/gen/generate.py` 独立生成。

### `internal/hub`（改动）

```go
type Request struct { Role Role; Target identity.Fingerprint; Token []byte }
type Grant struct { Ref any; Expires, PairingUntil time.Time } // 含时钟偏差的含边界截止时刻；零值：无期限 / 窗口关闭
type Policy interface {
    Admit(dev identity.Fingerprint, req Request, now time.Time) (Grant, error) // 锁外，A 组检查；不做外部 I/O，错误可安全写日志
    Valid(g Grant, dev identity.Fingerprint) bool                              // 锁内，只判撤销条件，不看时间
    PairingOperator() identity.Fingerprint                                     // 零值表示配对入口关闭
}
type Options struct {
    Local       *identity.Local
    Policy      Policy
    Now         func() time.Time // 只控制 Grant 期限，不影响 socket deadline
    Embedded    *Embedded
    Logger      *slog.Logger
    SetupLimits SetupLimits      // 内嵌与独立 Hub 都适用
}
func (s *Server) SetPolicy(p Policy) (closedWaits, closedPairs int) // 锁内替换并关闭失效路由；nil 撤销全部中继
```

无 Policy 时：有 Embedded 的 Hub 只接受内嵌自拨，其余 `unauthorized`；没有 Embedded 的 Hub，`Serve` 返回错误。独立 Hub 上 target 为本机指纹的 dial 走配对入口，配对后须在 `SetupLimits.Timeout` 内发出首字节。外层协议格式与状态码不变，详见 [PROTOCOL](../internal/hub/PROTOCOL.md)。

### `internal/hubauth`（新增）

```go
type Config struct { Version int; Mode string; IdentityDir, Listen string; Issuers []Issuer; PairingOperator string
    TicketMaxTTLS, BootstrapMaxTTLS, PairingWindowMaxS, ClockSkewS int64; SetupLimits SetupLimits; Deny Deny } // JSON 键见 SELFHOST 4.1
func Load(path string) (Config, error)            // 严格解码并校验；缺省值已应用
func (c Config) Validate() error
func (c Config) ReloadCompatible(old Config) error // identity_dir、listen、mode、setup_limits 变化需重启
func NewPolicy(cfg Config, hubFP identity.Fingerprint) (*Policy, error) // 实现 hub.Policy，不可变快照
func Explain(cfg Config, hubFP identity.Fingerprint, raw []byte, dev identity.Fingerprint, now time.Time) error // 离线执行 A2–A7、A9–A11
```

### `cmd/renet-hub`（新增）

`renet-hub init|fingerprint --identity-dir DIR`；`check|serve --config FILE`（`serve` 另有 `--debug`）；`ticket inspect --config FILE --ticket-file FILE [--device FP] [--hub-fp FP]`。Unix 上 SIGHUP 重载，信号接线在带构建约束的文件里。

### `internal/transport`、`internal/transport/hub`（改动）

```go
// transport
type Endpoint struct { Kind, Addr string; Hub identity.Fingerprint; Ticket string } // Ticket 是不透明票据字节，string 保持可比较
type PathInfo struct { Kind PathKind; LocalAddr, RemoteAddr, Via string; Hub identity.Fingerprint } // Hub = 外层钉住的指纹

// transport/hub
type Options struct {
    Local *identity.Local; Wrap Wrap; Embedded *router.Embedded; Listen string; SetupLimits router.SetupLimits
    Endpoints []transport.Endpoint
    WaitToken  func(ctx context.Context, hub identity.Fingerprint) ([]byte, error)    // 每次注册前现签；nil 发送空 token
    OnRegister func(hub identity.Fingerprint, token []byte, err error)                // 注册尝试结束后异步回调，token 为副本（签发失败为 nil）
}
func (a *Connector) Refresh()                                                          // 作废尚未收到任何字节的空闲注册（含签票与重试中的），再重新注册
func (a *Connector) DialBootstrap(ctx context.Context, addr string) (transport.Session, identity.Fingerprint, error) // 返回外层 H；内层 Expect 为零
```

`OnRegister` 的调用相互之间不保证顺序，可能在 Close 之后到达，必须快速返回。

### `internal/proto`、`internal/pairing/cpace`、`internal/session`（改动）

| 包 | 新增 |
|---|---|
| `internal/proto` | `PairHubCapability = "pair-hub/1"`、`HubTicketCapability = "hub-ticket/1"`；`TypeHubTicket MsgType = 17`；`HubTicket{Ticket []byte}`（键 1，1–1024 字节）；`ReadHubTicket(r io.Reader) (HubTicket, error)`，信封上限 1100 字节，只接受规范编码；`ReadPair` 仍只接受 11 至 16 |
| `internal/pairing/cpace` | `Binding` 增加 `Capability string` 与 `HubFP [32]byte`：`pair-direct/1` 时 `HubFP` 为零，`pair-hub/1` 时非零，CI 末尾追加 `hub_fp` |
| `internal/session` | `Options.HubFP`（非零时数字配对只用 `pair-hub/1`）、`Options.EndpointHub`（Client 期望的端点 Hub 指纹）、`Options.RequireHubTicket`、`Options.GrantTicket func(client identity.Fingerprint) ([]byte, error)`（Operator；非 nil 时声明 `hub-ticket/1`，在提交配对前预签）、`Options.Now`；`(*Session).HubTicket() []byte`；`ReasonHubTicket cpace.Reason = "hub_ticket"`。Client 总是声明 `hub-ticket/1` |

### `internal/trust`、`internal/invite`、`internal/settings`、`internal/identity`

| 包 | 新增 |
|---|---|
| `internal/trust` | 文件版本 2：端点记录可带 `ticket`；读取接受版本 1 与 2，版本 1 带 `ticket` 判为损坏；有任一端点带票据时写版本 2，否则写版本 1。`SavePeer` / `Peer` / `ReadPeer` 透传 `Endpoint.Ticket`。`(*Store).UpdateEndpointTicket(ctx context.Context, peer, hub identity.Fingerprint, addr, ticket string) error`：按 peer、Hub 指纹与地址唯一定位端点，原子替换票据 |
| `internal/invite` | `Invite.Version` 可为 2：每个端点的 `hub` 之后附 `ticket length:u16 + ticket`（0–1024）；有票据必须编码为版本 2，无票据为版本 1（字节不变）；`Decode` 接受 1 与 2；旧解码器拒绝版本 2 |
| `internal/settings` | `Version = 2`；`Operator.Mode`（`"direct"` / `"hub"`）、`Operator.Hub{Addr, FP, Share string}`、顶层 `Center{ID string; API []string}`；`Load` 将版本 1 在内存中升级（缺省中心取构建期 `-ldflags -X remote-enet/internal/settings.defaultCenterJSON=...`）；`Save` 总是写版本 2；版本 1 升级时三个 Hub 字段均为空字符串。直连模式使用 `Endpoint`，允许保留任意已填写且合法的 Hub 字段；Hub 模式要求 `Addr`、`FP`，分享地址取 `Share`，为空则用 `Addr`；`Config.Equal` |
| `internal/identity` | `LoadExisting(dir string) (*Local, error)`：只读，私钥必须存在，证书缺失或不符时在内存中重建、不落盘；`Create(dir string) (*Local, error)`：仅当私钥路径未被占用时创建 |

### `internal/center`（新增，只做无需签名的 `GET /v1/meta`）

```go
type Meta struct { CenterID string; APIVersions []string; ServerTime time.Time; MinAppVersion MinAppVersion }
type MinAppVersion struct { Operator, Client, Hub string }
func FetchMeta(ctx context.Context, client *http.Client, entry, role, appVersion string) (Meta, error)
type Status string // Verified | Unreachable | IDMismatch
type EntryResult struct { Entry string; Status Status }
func Check(ctx context.Context, c settings.Center, role, appVersion string) []EntryResult // 逐入口各自 10 秒
type HTTPError struct { StatusCode int; Code string }
```

请求携带 `Renet-Client: <role>/<app_version>`（非 SemVer 版本发送 `0.0.0-dev`），拒绝重定向，要求校验证书的 TLS 且不带凭据，响应体上限 16 KiB。

### `internal/app`、`internal/desktop`、`cmd/renetd`

```go
type HubConfig struct { Addr string; FP identity.Fingerprint }
// OperatorConfig.Hub *HubConfig（非 nil 时 Listen 必须为空）；InviteConfig.Hub *HubConfig
func IssuerEntry(ctx context.Context, stateDir, name string) (string, error) // 一行可粘贴进 Hub 配置 issuers 的 JSON
// Snapshot.HubWaiting bool：独立 Hub 已确认当前空闲 wait
const ( HubIssuer PairingErrorKind = "hub_issuer"; HubIdentityMismatch = "hub_identity_mismatch"; HubWaitUnauthorized = "hub_wait_unauthorized"
        HubPairingDisabled = "hub_pairing_disabled"; HubAuthorizationExpired = "hub_authorization_expired_or_revoked"
        HubUnreachable = "hub_unreachable"; OperatorOffline = "operator_offline" )
func (c *Controller) SetCenter(ctx context.Context, center settings.Center) error // 只能在空闲时；Save 不能修改中心
func (c *Controller) IssuerEntry(ctx context.Context) (string, error)
func (c *Controller) CheckCenter(ctx context.Context) (settings.Center, []center.EntryResult, error)
```

`app.ClientConfig` 的配对来源仍为 `Invite` / `InviteFile` / `DirectPair` / `Peer` 之一；Client 经 `DialBootstrap` 配对，已保存配对在拨号前按 `Endpoint.Ticket` 的 `exp` 过滤端点，续期按 `PathInfo.Hub` + `PathInfo.Via` 调用 `UpdateEndpointTicket`。`renetd`：`operator --hub HOST:PORT --hub-fp FP [--share-address HOST:PORT]`（与 `--listen` 互斥）、`issuer --state-dir DIR [--name NAME]`、`invite --hub HOST:PORT --hub-fp FP [--ttl D]`。

## R5. 各端接入中心（2026-11-20 冻结）

设计、任务与提交对应关系、审查处理见 [CENTER-CLIENT](CENTER-CLIENT.md)；协议以 [CENTER-CONTRACT](CENTER-CONTRACT.md)（第十一版）为准。以下签名取自实现，以代码为准。R5 删除的接口：无；`locales.Text` 中「纯 8 位码改写为直连提示」的 R5 前特例已删除。

### `internal/hub`（改动）

```go
type SessionID [16]byte // 零值表示自建授权
type Grant struct {
    Session               SessionID // 非零时启用契约 §8.4 第 10–12 步
    JTI                   [16]byte  // 锁内消费，保留到 Expires（含边界）
    Ref                   any
    Expires, PairingUntil time.Time
}
type Options struct {
    CenterMode  bool // 锁内拒绝 Session 为零的 Grant（unauthorized，记错误日志）
    Local       *identity.Local
    Policy      Policy
    Now         func() time.Time
    Embedded    *Embedded
    Logger      *slog.Logger
    SetupLimits SetupLimits
    AfterFunc   func(time.Duration, func()) (stop func() bool) // 与 Now 同一时钟；nil 用 time.AfterFunc
    SealTTL, SessionRetention time.Duration                    // 非正值取契约缺省（各 330 秒）
}
type Usage struct { Session SessionID; Paired bool; Conns int; RxBytes, TxBytes uint64; PairedFor time.Duration } // 本进程累计
func (s *Server) SetAdmitting(open bool)                          // 只关新准入，不断开已有路由
func (s *Server) Renew(id SessionID, deadline time.Time) bool     // 直接替换 D，允许缩短；没有会话记录时返回 false
func (s *Server) Revoke(id SessionID) Usage                       // 锁内封存并关闭整组连接；未知会话同样封存，用量为零；幂等
func (s *Server) CloseAll() int                                    // 关闭全部中心路由，不封存，保留尾部用量
func (s *Server) Usage() []Usage                                   // 只列有连接的会话
func (s *Server) TrafficTotals() (rx, tx uint64)                   // 含已断连与已退役会话，供心跳按两次采样差算吞吐
func (s *Server) SetSessionRetention(retention time.Duration)      // 同时重排已断连会话的清理时刻
```

自建 Grant（`Session` 为零）的行为不变。中心会话的自拨（target 为本 Hub 指纹）在锁内同样 `unauthorized`。外层协议格式与状态码不变，中心会话一节见 [PROTOCOL](../internal/hub/PROTOCOL.md#中心会话)。

### `internal/hubcenter`（新增）

```go
const ( ClockSkew = 30 * time.Second; TicketMaxTTL = 300 * time.Second; HeartbeatTimeout = 10 * time.Second )
var ErrSuperseded = errors.New("node generation superseded; another process may be using this identity")
type Config struct {
    Version                   int
    Mode, IdentityDir, Listen string
    Center                    settings.Center
    NodeID                    [16]byte
    TicketKeys                []ed25519.PublicKey
    SetupLimits               hubauth.SetupLimits
} // JSON 键见 CENTER-CLIENT 3.1
func Load(path string) (Config, error)                 // 严格解码并校验，不打开身份目录
func (c Config) ReloadCompatible(old Config) error     // 只允许 ticket_keys 变化，入口顺序变化也需重启
func NewPolicy(cfg Config, hubFP identity.Fingerprint) (*Policy, error) // 实现 hub.Policy；代次为 0 时拒绝一切
func (p *Policy) SetGeneration(gen uint32)
func (p *Policy) SetKeys(keys []ed25519.PublicKey) error // 1–16 把、kid 不重复，校验通过后整体替换
func (p *Policy) Kids() []ticket.Kid
type NodeOptions struct {
    Config Config; Local *identity.Local; Server *hub.Server; Policy *Policy; API *center.Client
    Version string; Now func() time.Time; Logger *slog.Logger
}
func NewNode(o NodeOptions) *Node
func (n *Node) Run(ctx context.Context) error          // Serve 之后调用一次；返回 ErrSuperseded、终局错误或 ctx 错误，退出前关闸并 CloseAll
func (n *Node) Reload(cfg Config) error                // ReloadCompatible 后 SetKeys
```

`Policy.Admit` 在 `ticket.VerifyCenter` 之后另行拒绝全零 `sid` 与 target 为 Hub 自身的 dial（契约第十一版 §8.4）；`Valid` 恒为真，`PairingOperator` 恒为零。`Node` 的激活键满 23 小时换新；保留期按 `lease_ttl + clock_skew` 调用 `SetSessionRetention`；`renew` 只作用于本次上报的会话；`closed` 每次至多 1000 项，保留到中心不再在 `revoke` 中列出。`internal/hubauth` 新增 `DecodeStrict(r io.Reader, limit int, dst any) error`，自建语义不变。

### `internal/center`（扩展为签名 API 客户端）

```go
type ClientOptions struct {
    Center           settings.Center
    Role, AppVersion string             // Role: "operator" | "client" | "hub"
    Device, NodeKey  ed25519.PrivateKey // 设备主体或节点主体
    Node             [16]byte
    HTTP             *http.Client       // nil：校验证书的缺省客户端
    Now              func() time.Time
    Logger           *slog.Logger
}
func NewClient(o ClientOptions) (*Client, error)        // 不做网络 I/O
type Call struct { Method, Path string; Query url.Values; Body any; Key string; Nonce bool; Wait time.Duration }
type Response struct { Status int; Replayed bool; Entry string }
func (c *Client) Do(ctx context.Context, call Call, out any) (Response, error)
func (c *Client) Now() time.Time                        // 设备主体按 server_time 校正；hub 角色为本地时钟
type Class uint8                                        // ClassBusiness | ClassRetry | ClassWait | ClassFix
func ClassOf(code string) Class                         // 未知 code 视为 B
type Error struct { Status int; Code string; Class Class; RetryAfter time.Duration; Details jsonwire.RawMessage }
func (e *Error) EndReason() string; func (e *Error) State() string; func (e *Error) ServerTime() (time.Time, bool)
var ErrUnreachable = errors.New("center unreachable")            // 入口或预算耗尽
var ErrIDMismatch = errors.New("center identity mismatch")       // 全部入口都报告了别的 center_id
var ErrEndpointResponse = errors.New("center: invalid endpoint response")
var ErrProtocol = ErrEndpointResponse                           // 成功响应格式或取值无效
func NewIdempotencyKey() string
// Operator
func (c *Client) Login(ctx context.Context, key, accountCode, label string) (Device, Account, error)
func (c *Client) Account(ctx context.Context) (AccountView, error)
func (c *Client) Devices(ctx context.Context) ([]DeviceEntry, error)
func (c *Client) RevokeDevice(ctx context.Context, key string, fp identity.Fingerprint) error
func (c *Client) Nodes(ctx context.Context) ([]Node, error)
func (c *Client) CreateSession(ctx context.Context, key string, node NodeID) (Session, error)
func (c *Client) Established(ctx context.Context, key string, id SessionID, client identity.Fingerprint) (Session, error)
// 两端
func (c *Client) Session(ctx context.Context, id SessionID, since int64, wait time.Duration, nonce bool) (Session, error)
func (c *Client) Ticket(ctx context.Context, key string, id SessionID) (Ticket, error)
func (c *Client) End(ctx context.Context, key string, id SessionID, reason string) (Session, error)
// Client
func (c *Client) Redeem(ctx context.Context, key, code string) (Session, error)
func (c *Client) ClientSessions(ctx context.Context) ([]Session, error)
// Hub
func (c *Client) Activate(ctx context.Context, key string, req Activation) (Activated, error)
func (c *Client) Heartbeat(ctx context.Context, req Heartbeat) (HeartbeatResult, error)
```

线格式结构体（`Session`、`Ticket`、`Node`、`Account`、`AccountView`、`SessionSummary`、`Device`、`DeviceEntry`、`Activation`、`Activated`、`NodePolicy`、`Heartbeat`、`Load`、`SessionUsage`、`ClosedUsage`、`HeartbeatResult`、`Renewal`、`Revocation`）与 ID 类型（`SessionID`、`NodeID`、`AccountID`、`KeyID`，base64url 文本）按契约 §7 定义，未知字段忽略。`Do` 一次调用至多 3 次尝试，单次 10 秒、合计 30 秒，长轮询两者各加 `Wait`；没有错误信封的非 2xx 一律 R 类；已向入口发出的心跳不重发。心跳响应的 `renew` / `revoke` 可缺失或为 `null`。R3 的 `FetchMeta` / `Check` 改为也接受 `hub` 角色。

### `internal/session`、`internal/pairing`、`internal/proto`（改动）

```go
// session
type CenterBinding struct { CenterID string; Session [16]byte; Hub identity.Fingerprint }
// Options 新增：
//   Center *CenterBinding                       非 nil 时 ConfirmSAS 必填、隧道闸门初始关闭
//   Pinned func(identity.Fingerprint) bool      Operator 判定数据连接的对端是否已钉住
//   HoldTunnels bool
//   ConfirmTimeout time.Duration                中心模式缺省 3 分钟
//   BeginClientAttempt func() bool              Client 在发 11 前扣会话预算；false 表示已用尽
func (s *Session) ReleaseTunnels()             // 幂等；配对连接永不放行
var ErrTunnelsHeld = errors.New("session: tunnels held until the center confirms the session")

// pairing/cpace：ReasonRejected = "rejected"、ReasonConfirmTimeout = "confirm_timeout"、ReasonNotPaired = "not_paired"

// pairing
func NewSessionResponder(code string, budget int) *SessionResponder
func (r *SessionResponder) Begin(ctx context.Context) (*SessionAttempt, error) // 收到 11 后、发 12 前计数
func (r *SessionResponder) AttemptsLeft() int
func (r *SessionResponder) Close()
func (a *SessionAttempt) Context() context.Context
func (a *SessionAttempt) Respond(b cpace.Binding, commit [64]byte) (*cpace.Responder, [32]byte, error)
func (a *SessionAttempt) Commit(save func(context.Context) error) error
func (a *SessionAttempt) Finish()

// proto
func ReadCenterControl(r io.Reader) (any, error) // 数据连接控制流：配对帧仍按 256 字节上限
```

`pairing` 不导入 `session`，`*SessionAttempt` 由调用方适配为 `session.DigitalResponder`。

### `internal/centerstate`（新增）

```go
var ErrNoPin = errors.New("centerstate: pin not found"); var ErrCorrupt = errors.New("centerstate: corrupt state")
func Open(roleDir, centerID string) (*Store, error) // state/<role>/centers/<center_id>/，不触碰其他中心与 trust.json
type NodeRef struct { ID [16]byte; Addr string; Port uint16; HubFP identity.Fingerprint }
type Pin struct { CenterID string; Session [16]byte; Role string; Peer identity.Fingerprint; Node NodeRef; PairedAt time.Time }
func (s *Store) SavePin(ctx context.Context, p Pin) error
func (s *Store) Pin(id [16]byte) (Pin, error)       // ErrNoPin / ErrCorrupt
func (s *Store) Pins() ([]Pin, error)               // 返回可用记录，并 join 不可用项的错误
func (s *Store) DeletePin(ctx context.Context, id [16]byte) error
type Login struct { DeviceFP identity.Fingerprint; DeviceLabel, AccountID, AccountLabel string; ExpiresAt, LoggedInAt time.Time }
func (s *Store) SaveLogin(ctx context.Context, l Login) error
func (s *Store) Login() (Login, bool, error)
func (s *Store) ClearLogin(ctx context.Context) error
```

### `internal/centerflow`（新增）

```go
type Confirm func(context.Context, SASPrompt) (bool, error)
type SASPrompt struct { ID uint64; Code string; Peer identity.Fingerprint; Deadline time.Time }
type TicketConnector interface {
    WaitTicket(context.Context, transport.Endpoint, identity.Fingerprint, []byte) (transport.Session, error)
    DialTicket(context.Context, transport.Endpoint, identity.Fingerprint, []byte) (transport.Session, error)
}
type Dependencies struct {
    API *center.Client; CenterID string; LocalFP identity.Fingerprint; Store *centerstate.Store
    Connector TicketConnector; Confirm Confirm; RetryMin, RetryMax time.Duration
}
type OperatorDeps = Dependencies
type ClientDeps struct { Dependencies; CheckNIC func(context.Context) error } // 兑换前必须通过
type State struct {
    Phase string; Reason Reason; SessionShort, Region, PeerShort string
    AttemptsLeft int; CodeExpiresAt, ExpiresAt time.Time
} // 不含码、票据与完整会话 ID
type OperatorState = State; type ClientState = State
type NodeLatency struct { center.Node; Latency time.Duration } // 探测失败为负

func NewOperator(d OperatorDeps) *Operator
func (o *Operator) Login(ctx context.Context, code, label string) error
func (o *Operator) Logout(ctx context.Context) error                    // 吊销本机设备记录
func (o *Operator) Account(ctx context.Context) (center.AccountView, error)
func (o *Operator) Nodes(ctx context.Context) ([]NodeLatency, error)    // 并发不超过 4 的 TCP 探测，不做 TLS
func (o *Operator) Start(parent context.Context, node center.NodeID) error // 有限预算（1 分钟），可取消
func (o *Operator) Run(ctx context.Context, base session.Options, serve func(context.Context, *session.Session) error) error
func (o *Operator) Invitation() string                                  // 只供本地显示码
// Operator、Client 共有：End(ctx context.Context) error；Snapshot() State

func NewClient(d ClientDeps) *Client
func (c *Client) Redeem(ctx context.Context, code string) error
func (c *Client) Resume(ctx context.Context, ids ...center.SessionID) error // 多个存活会话时须指定 ID
func (c *Client) Retry(ctx context.Context) error
func (c *Client) Run(ctx context.Context, base session.Options, serve func(context.Context, *session.Session) error) error

type Reason string                     // 稳定的界面键；中心业务码与结束原因保留线上拼写
func ReasonOf(err error) Reason
const ErrChooseSession Reason = "choose_session"
type SessionCandidate struct { ID center.SessionID; ShortID string; RedeemedAt time.Time }
type ChooseSessionError struct{ /* 不可变 */ }
func (e *ChooseSessionError) Candidates() []SessionCandidate // Error() 不含会话 ID；Unwrap() 为 ErrChooseSession
```

`Reason` 常量：`peer_version_incompatible`、`center_protocol_error`、`nic_missing`、`center_unconfigured`、`center_unreachable`、`center_id_mismatch`、`upgrade_required`、`rate_limited`、`device_busy`、`code_invalid`、`pairing_lost`、`hub_unauthorized`、`operator_waiting`、`busy`、`confirmation`、`awaiting_center`、`account_code_format`、`account_code_invalid`、`account_disabled`、`account_expired`、`device_limit`、`unauthenticated`、`quota_exhausted`、`quota_releasing`、`node_unavailable`、`not_found`、`forbidden`，以及契约 §12 的全部结束原因。

### `internal/transport/hub`（改动）

```go
func (a *Connector) WaitTicket(ctx context.Context, ep transport.Endpoint, target identity.Fingerprint, token []byte) (transport.Session, error) // 单次，target 同时钉内层对端
func (a *Connector) DialTicket(ctx context.Context, ep transport.Endpoint, peer identity.Fingerprint, token []byte) (transport.Session, error)   // 单次，peer 同时为 dial target 与内层对端
var ErrHubFingerprintMismatch = errors.New("transport/hub: hub fingerprint mismatch") // 外层钉扎失败；同时匹配 identity.ErrFingerprintMismatch
```

两者不重试、不领票；Hub 的拒绝可用 `errors.Is` 匹配包内的 `ErrUnknownPeer`、`ErrPeerBusy`、`ErrUnauthorized`（与 `internal/hub` 同值）。

### `internal/app`、`internal/desktop`、`internal/settings`、`internal/testutil/fakecenter`

```go
// app
type CenterConfig struct { Center settings.Center; Node string; AppVersion string } // Node 为空表示恢复已有接待
// OperatorConfig.Center *CenterConfig；ClientConfig.CenterCode string、ClientConfig.CenterResume center.SessionID（恢复所选候选会话）、ClientConfig.Center *settings.Center、ClientConfig.AppVersion string
type CenterSnapshot struct { centerflow.State; SASID uint64; SASDeadline time.Time } // Snapshot.Center
func NewWithCenterHTTP(client *http.Client) *Runtime
func (r *Runtime) CenterDisplay() (invitation string, prompt centerflow.SASPrompt) // 明文码与 SAS 只经此处取得，只供本地控件
func (r *Runtime) VerifySAS(id uint64, input string) bool                          // Operator：输入（忽略空格、接受全角数字）与本机 SAS 一致才标记已核对
func (r *Runtime) AnswerSAS(ctx context.Context, id uint64, match bool) error      // Operator 未核对时答「一致」返回 ErrSASNotVerified，提示保持待答
func (r *Runtime) EndReception(ctx context.Context) error
func (r *Runtime) RetryPairing(ctx context.Context) error
func (r *Runtime) CenterAccount(ctx context.Context, dir string, cfg CenterConfig) (*centerflow.Operator, error)
var ErrSASNotVerified = errors.New("app: matching operator SAS input required")
var ErrSASExpired = errors.New("app: SAS prompt expired or no longer pending")

// desktop
type CenterView struct {
    app.CenterSnapshot
    LoggedIn bool; EffectiveExpiry int64; Account center.Account; Devices []center.DeviceEntry
    Nodes []centerflow.NodeLatency; RequiresSASInput bool
} // State.Center
func (c *Controller) CenterLogin(ctx context.Context, accountCode, label string) error
func (c *Controller) CenterLogout(ctx context.Context) error
func (c *Controller) RefreshCenter(ctx context.Context) error
func (c *Controller) RevokeCenterDevice(ctx context.Context, fp string) error
func (c *Controller) StartReception(ctx context.Context, nodeID string) error
func (c *Controller) MatchSAS(id uint64, input string) bool
func (c *Controller) AnswerSAS(ctx context.Context, id uint64, match bool) error
func (c *Controller) RetryPairing(ctx context.Context) error
func (c *Controller) EndReception(ctx context.Context) error
func (c *Controller) CenterDisplay() (string, centerflow.SASPrompt)
func (c *Controller) CenterSessionChoice() *centerflow.ChooseSessionError
func (c *Controller) ResumeCenterSession(ctx context.Context, id center.SessionID) error // 只接受上一次失败的 Client 运行给出的候选
var ErrSASInputRequired = app.ErrSASNotVerified
var ErrCenterOperation = errors.New("desktop: center operation failed") // 隐藏无法识别的服务端错误
```

| 包 | 新增 |
|---|---|
| `internal/settings` | `operator.mode` 增加 `"center"`，版本仍为 2：中心模式要求已配置中心，`listen` 可为空，直连与 Hub 字段可保留（非空仍校验）。配置文件不存在时 `Default()` 在构建期缺省中心有效且非空时取 `center`，否则 `direct`；已有文件读入（含版本 1 升级）不改模式 |
| `internal/locales` | `CenterMessage(reason, role string) string` 只接受已知原因并按角色给出文案 |
| `internal/checkup` | 中心模式 Operator 与输入纯 8 位码的 Client 只做中心入口的 meta 检查，不兑换 |
| `internal/testutil/fakecenter` | 只供测试的内存 TLS 假中心，不是参考中心实现，不提供命令；中心实现仍须对真实数据库跑 C1、C2、C9、C10 |

`renet-hub`：配置的 `mode` 为 `"center"` 时，`check --config FILE [--online]` 打印 `center_id`、`node_id`、Hub 指纹、各 `kid` 与入口（`--online` 用匿名 meta 核对各入口，有入口未通过即失败；不带时不联网）；`serve --config FILE [--debug]` 先监听再激活，Unix 上 SIGHUP 只重载 `ticket_keys`；`ticket inspect` 对中心模式报不支持。退出码：0 正常（含收到信号）；2 中心模式 `serve` 的配置或激活终局错误（以及 `serve` 读不出 `mode` 或 `mode` 未知）；3 `node_superseded`；1 其他错误（用法、`check` 失败、自建模式 `serve` 的错误）。构建期版本变量 `-ldflags -X main.version=...`，缺省 `0.0.0-dev`。

`renetd`：`operator --center-node ID [--config FILE]`（中心取自 GUI 设置文件，须已在同一状态目录经 GUI 登录；与 `--hub`、`--listen`、`--share-address` 互斥）；`client --center-code-file FILE [--config FILE]`（与 `--invite-file`、`--invite`、`--peer`、`--pair-file` 五选一）。命令行的 SAS 核对：Operator 显示来访设备与本机校验码，输入客户读出的 6 位，不一致提示后可重输，输入 `no` 拒绝；Client 回车确认、`no` 拒绝。Ctrl-C 退出时向中心提交结束。退出码沿用 0 / 1。

---

## 通用要求

- 分支名 `wave1/<A|B|C|D>-<slug>`；只改分配给自己的目录 + `go.mod/go.sum`。
- 完成标准：`gofmt -l .` 为空、`go vet ./...`、`go test -race ./...` 全绿；在最终报告里列出新增依赖、对其他包的期望签名、以及未覆盖的边界。
- 不要 push、不要开 PR；整合者负责合并。
