# Wave 2：headless 应用接线

基线为 `d8fb0a7`。本轮目标是通过真实启动入口跑通模拟工具 → Operator → Hub/secure/session → Client → 假 ZGW；不声称完成实车验收。沿用 `AGENTS.md` 和 Wave 1 协议，不修改公共消息格式，不增加第三方依赖，不实现 UI、DoIP、L3、HTTP 回连或车辆业务。

## 分工与共享约定

- A 只修改 `internal/carside/`：网卡、发现、白名单和透明拨号。
- B 只修改 `internal/operator/`：HSFZ 呈现、UDP 发现应答及两个 TCP 转发端口。
- C 只修改 `cmd/renetd/`、`internal/trust/`、`.github/workflows/` 和根 `README.md`：启动、持久化、重连、CI 和运行说明。
- 整合者修改本文件、设计稿和 `internal/integration/`，接线后跑真实应用链路测试。
- 使用独立 worktree；不 commit、push 或开 PR。其他包需要修改时先报告，由整合者处理。
- 代码、注释、日志和标识符用英文；文档用中文。所有网络测试限 loopback；竞态检测单独使用 `CGO_ENABLED=1`，生产构建和普通检查使用 `CGO_ENABLED=0`。
- 时间和端口均可配置；测试使用临时端口。持有资源的 `Close` 必须取消并等待自己的工作协程退出。

## A：车端接口

```go
package carside

type NIC struct {
    Name    string
    Address netip.Addr
    Prefix  netip.Prefix
}
func Interfaces() ([]NIC, error)
func SelectNIC(name string, addr netip.Addr) (NIC, error)

type DiscoverOptions struct {
    NIC          NIC
    Destinations []netip.AddrPort
    Timeout      time.Duration
}
type Gateway struct {
    Address        netip.Addr
    Identification hsfz.Identification
    IdentRaw       []byte
}
func Discover(ctx context.Context, o DiscoverOptions) ([]Gateway, error)
func (g Gateway) VehicleState(nic NIC, seq uint64) proto.VehicleState

type Forwarder struct { /* private */ }
func NewForwarder(nic NIC, logger *slog.Logger) (*Forwarder, error)
func (f *Forwarder) HandleTunnel(ctx context.Context, h proto.OpenTunnel, st transport.Stream)
func (f *Forwarder) Close() error
```

`Interfaces` 列出启用的 IPv4 网卡地址，允许显式选择 loopback 做测试；无显式选择时仅自动选唯一的非 loopback link-local 地址，歧义必须报错。IPv6 不在本轮范围。

发现 socket 绑定所选 IPv4 地址的临时端口。缺省目的地为该子网的定向广播及 `255.255.255.255:6811`；显式目的地用于单播与假网关临时端口。只接收同子网中有效且长度精确的 `0x0011` 响应，解析发现字段、按源 IP 去重，超时返回已有结果，取消返回可识别的 context 错误。`IdentRaw` 是**包括六字节 HSFZ 头的完整 UDP 数据报**，`Identification.Raw` 仍是正文。`VehicleState.NICAddr` 为本机源 IP 字符串，`GatewayAddr` 为网关 IP 字符串，`IdentRaw` 不做重写。

Forwarder 只接受 `kind=tcp` 和字面 IPv4 `ip:port`；端口非零，地址必须属于所选 Prefix，拒绝本机所有地址、未指定地址、组播、广播和子网外目标。不要用 DNS 解析来扩大边界。允许显式 loopback 网卡访问同子网的其他 loopback 地址以便测试。拨号源地址固定为 NIC.Address；后续字节直接交给 `tunnel.Pipe`，不得解析或记录载荷。

测试覆盖发现取消、畸形包、来源过滤、原样保留、去重；子网拒绝与源地址绑定；双向半关闭、并发、取消和 Close 退出。

## B：Operator 接口

```go
package operator

type Opener interface {
    OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error)
}
type Ports struct { Diagnostic, Control, Discovery int }
type Options struct {
    Bind                 netip.Addr
    Ports                Ports
    State                proto.VehicleState
    RemoteDiagnosticPort uint16
    RemoteControlPort    uint16
    Opener               Opener
    Logger               *slog.Logger
}
type Presenter struct { /* private */ }
func Start(ctx context.Context, o Options) (*Presenter, error)
func (p *Presenter) Addr() (diag, ctrl *net.TCPAddr, disc *net.UDPAddr)
func (p *Presenter) Update(state proto.VehicleState) error
func (p *Presenter) Close() error
```

本地 Ports 的零值表示临时端口，CLI 负责提供 6801/6811/6811 默认值；远端两个端口零值分别表示 6801/6811。三端点必须全部绑定成功，否则清理所有已创建资源。Bind 为显式 IPv4 地址，不自动提权或修改网卡。

State 可暂为空：尚无有效网关时不应答发现、拒绝 TCP 连接。非空状态需校验 GatewayAddr 和完整 IdentRaw。Update 原子更换并复制状态；新连接捕获当时状态，已有流不切换目标。只接收正确的 `hsfz.IdentRequest`，把缓存的完整 IdentRaw 从绑定地址原样单播回请求源 IP:port。TCP 两个 listener 分别打开目标为 `GatewayAddr:remotePort` 的新隧道；双向字节原样搬运，不代答 alive。Session 由上层持有，Presenter.Close 不关闭其他组件的会话。

整合时补充的实测边界：Linux loopback 上，UDP 绑定具体 IP 可接单播，收不到定向广播；通配地址接收可以。当前测试只证明单播发现应答，M1 的手动 gateway TCP 链路不受影响。M0b 仍需验证目标 Windows 行为；M2 自动发现需完善通配地址接收、入站接口过滤和固定回复源 IP，不能把当前单播测试当成广播兼容验收。Linux 可用标准库 IP_PKTINFO，跨平台薄封装待 M0b 后实现，不在本轮扩展。

测试覆盖真实 UDP 回源端口、完整未知扩展字段保留、无效探测、无车辆状态、两个 TCP 通道、端口冲突清理、状态更新、并发、半关闭和退出。

## C：CLI、信任与 CI

使用标准库 flag 和 JSON。启动入口为 `cmd/renetd`：

- `invite --state-dir DIR --endpoint HOST:PORT [--ttl 15m]`：加载本机身份，生成一次性随机 token，持久化其哈希和有效期，输出邀请码到 stdout；日志不可包含 token。Endpoint 为 Operator 内嵌 Hub 的外部地址，Hub 指纹等于 Operator 指纹。
- `operator --state-dir DIR --listen HOST:PORT --bind IP`：启动内嵌 Hub、持久化信任校验和单会话接收循环；等首条车辆状态后启动 Presenter。默认本地监听 6801/tcp、6811/tcp、6811/udp，支持显式端口覆盖和远端诊断/控制端口覆盖，供假 ZGW 使用。
- `client --state-dir DIR [--invite-file FILE | --invite CODE | --peer FINGERPRINT] [--nic NAME] [--nic-addr IP] [--discovery-target IP:PORT] [--discovery-timeout 3s]`：选择 NIC，发现唯一网关，多车不任意选第一辆；拨 Operator、完成配对、推送车辆状态并挂接 Forwarder。成功后保存 Operator 的 PeerInfo，以便不持有过期邀请码也能重连。明确失败时给可操作的错误，不以桩跳过发现。

Operator 始终保持单会话；同一工具可有多条 TCP 连接。本轮不实现按进程独占。断线后清理旧 Presenter 和所有转发，再接下一条会话。Client 网络错误以有界指数退避重试、context 取消可立即打断；配对拒绝或版本不兼容作为可操作错误返回。底层 `WrapStream` 的 context 持有整个会话寿命，不能在 Dial 成功后取消。

`internal/trust` 的具体 API 由 C 决定，需线程安全、原子落盘和可识别错误。首次配对的 token 校验、消费、trusted 写入是同一事务；不落明文 token，不接受零 token，不在落盘失败时报告配对成功；并发消费最多一次。缺失文件表示空存储，损坏文件不能静默清空。持久化最近对端及端点不保存已消费配对 token。覆盖过期、错误 token、并发消费、重启和损坏存储。

CI 覆盖 gofmt、vet、普通测试和 Linux race；普通运行关闭 cgo，race 单独启用；Windows amd64 与 macOS arm64 做纯 Go 交叉构建。按项目已有 Go 1.27，不添加新的依赖。README 给出假 ZGW、邀请、两端 CLI 的可复现命令，说明 E-Sys/ISTA 真机兼容性仍待 M0b。

C 的 CLI 依赖 A/B；先实现 trust、flag、CI，A/B 完成后由整合者将代码同步到 C worktree，再做完整编译与应用测试。不要写空实现来解决依赖。

## 验收和独立 review

整合测试必须实际使用 A/B，不能继续用 Wave 1 测试里的内联车端 handler：通过假 ZGW UDP 发现 → VehicleState → Presenter 发现应答 → 本地 TCP → 加密隧道 → 假 ZGW。覆盖两个 TCP 端口、源绑定、半关闭、非法目标、断开清理和再次连接；CLI 验证身份/配对重启持久化。

整合后运行 gofmt、vet、普通测试、race 和交叉构建，再通过 Paseo 创建 OpenCode / DeepSeek 只读 reviewer。重点检查白名单、token 事务、源地址绑定、重连清理、端口占用和取消；问题需给文件行号、触发路径、影响和证据。整合者复核问题、修复并回归，不把模型建议直接当事实。

## 本轮结果

三个工作包已在本地整合，未新增第三方依赖，未修改 Wave 1 控制消息格式。`internal/integration/application_test.go` 使用真实两端组件和 TLS 验证发现、状态推送、单播回放、两个 TCP 通道、并发、半关闭、非法目标拒绝和取消回收。`internal/integration/cli_test.go` 实际构建并启动 CLI 子进程，验证 Operator 先启动、独立进程签发邀请码、Client 首次连接，以及两端重启后的 `--peer` 复连。

本地通过 gofmt、vet、普通测试、race，以及 Windows amd64 / macOS arm64 的 `CGO_ENABLED=0` 构建。CI workflow 已配置，尚未推送到 GitHub 执行；跨平台构建不代替操作系统运行时验证。

Paseo 的 OpenCode / DeepSeek V4 Pro 完成两轮只读 review，未发现确定缺陷。整合者复核并纠正了报告中的两处判断：第二个 dial 在 Hub 层立即得到 busy，不排队；已信任设备恢复存储后可使用未过期原邀请码重试，无须再次消费 token。后者补充了 `TestTrustedClientCanReuseConsumedInvitation` 回归，确认不同新身份仍无法消费旧 token。未依据 IP 的末字节在 Operator 判断网络号/广播，因为实际含义取决于车端网卡前缀；安全限制继续在车端执行。

保留的已知边界：SIGKILL/断电可能遗留跨进程锁目录，需要按 README 人工确认后清理；广播自动发现尚未完成；真实 E-Sys / ISTA / Tool32、Windows 运行时和实车验收均待 M0b/M1 后续验证。
