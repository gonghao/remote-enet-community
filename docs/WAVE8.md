# Wave 8 · TUN 呈现基础与跨平台边界

用户于2026-11-17确认：短码暂不做，记录TUN＋用户态协议栈主路线并开始实现；同时要求考虑macOS Operator及mgu_kit本地自动检测。工作区包含Wave 7成果；各工作包不独立commit/push，由整合者统一验收。用户随后授权统一提交并推送，不重写历史。

## 范围与阶段

本轮完成最小IPv4/TCP呈现基础、跨平台设备适配和独立手动验收入口，不替换现有GUI/HSFZ默认路径。先用单个模拟ECU IP验证TCP 22及HTTP，核对宿主SSH 22共存、半关闭、取消、路由及设备清理。SSH banner验证必须如实标为banner，不能宣称认证登录通过。

后续分别交付MGU IP发现、UDP/DoIP、本地自动发现、HTTP回连。当前转发核心只支持TCP；TUN不自动解决广播、ECU地址探测、HTTP通告IP或进程独占。车端仍用普通socket，不建TUN、不提权、不解释诊断内容；Hub、mTLS、yamux、CBOR v1与现有消息字段不改。

Windows使用Wintun，macOS使用utun，Linux使用TUN；用户态协议栈及到现有TCP stream的桥接共用。代码保持CGO=0。新依赖由整合者统一固定：gvisor.dev/gvisor v0.0.0-20261116062528-83282de1f21f（官方go分支，含标准Go构建所需生成文件）；golang.zx2c4.com/wireguard v0.0.0-20261024220952-b3bc704ef0ee（仅复用tun设备，不使用WireGuard传输/加密）。gVisor用于成熟TCP状态机，WireGuard tun用于已有跨平台驱动适配，避免自写TCP或复制utun/Wintun ABI。

初期只安装明确目标IPv4的/32路由。不得接管默认路由或整个169.254/16；本地地址、回环、组播/广播、重复目标与路由冲突须拒绝或报明确信息。原生配置只作用于本进程新创建设备，不能打开/删除客户已有适配器，不删除系统Wintun驱动；失败回滚及正常Close均回收本轮拥有的资源。系统配置操作由Operator侧完成。

## 共享接口（整合者提供）

`internal/packetio`提供数据报设备接口及有界内存双端设备。ReadPacket返回完整包，缓冲不足返回io.ErrShortBuffer；WritePacket复制数据，不借用调用方缓冲。Close幂等并解除阻塞。独立调用ctx取消不关闭其他调用或整个设备。

```go
type Device interface {
    ReadPacket(context.Context, []byte) (int, error)
    WritePacket(context.Context, []byte) (int, error)
    MTU() int
    Close() error
}
func NewPair(mtu, capacity int) (Device, Device, error)
```

## A · internal/l3（独立worktree）

只编辑internal/l3。gVisor IPv4/TCP栈接收Device的裸IP包，将目标流转为现有OpenTunnel{Kind:"tcp",Target:dst,Origin:src}，不在Windows宿主监听ECU端口。目标白名单为明确IPv4地址，IPv6/UDP/其他IP流不转发到Opener。禁止把TUN字节作为原始IP隧道送到车端。

```go
type Opener interface {
    OpenTunnel(context.Context, proto.OpenTunnel) (transport.Stream, error)
}
type Options struct {
    Targets []netip.Addr
    Opener Opener
    MaxConnections int // zero defaults to 64, positive bounded upper limit
    Admit func(context.Context, netip.AddrPort, netip.AddrPort) (func(), error)
}
func Start(context.Context, packetio.Device, Options) (*Stack, error)
func (*Stack) Close() error
type Snapshot struct { Active, Accepted, Rejected uint64 }
func (*Stack) Snapshot() Snapshot
```

Admit的参数依次为目标ECU端点、原始工具源端点，便于后续按Windows客户端TCP表识别PID。nil不拦截；非nil返回的release在流最终退出时恰好调用一次。必须在OS客户端可建立TCP状态后调用，不能照抄宿主listener时序而使PID解析一直停在SYN_SENT。

Start/Close拥有栈及其worker、TCP流，调用方仍拥有Device；栈取消不得要求销毁设备才能解除某一次ReadPacket。Close后不留Opener/Pipe goroutine；Opener须配合ctx。收到超额连接应有界拒绝，不能按每个SYN无限生成worker或无界缓冲。验证实际TCP对话、目标/Origin保留、半关闭后回写、失败/取消隔离、准入与资源预算，不仅测试手写helper。测试用内存设备、loopback服务和既有session，不需要管理员或外部服务。

## B · internal/tunadapter（独立worktree）

只编辑internal/tunadapter。公开入口：

```go
type Config struct {
    Name string
    Local netip.Prefix
    Targets []netip.Addr
    MTU int
}
func Open(context.Context, Config) (*Adapter, error)
// Adapter implements packetio.Device.
func (*Adapter) Name() string
func (*Adapter) Close() error
```

Local为明确IPv4/32（含ENET常用169.254/16的link-local地址），Targets为非本机、非Local的合法单播IPv4，MTU=0取1500。配置校验在创建或修改系统资源之前完成。Name规则按平台说明，自动分配时返回实际名称；不接管已有同名设备。只安装本轮Targets的host route，并记录实际成功步骤，失败逆序回收；调用ctx结束也需完整清理。

优先使用固定版本wireguard/tun设备，平台系统地址与路由配置可用有界、可取消的系统命令或成熟原生接口。所有命令参数结构化传递，不构造shell字符串，不修改管理网卡地址、防火墙、默认路由。Close顺序必须先回收拥有的路由/地址，再关闭原生设备。错误必须传播；资源已因设备关闭消失的情况要根据真实结果区分，不能吞掉所有清理错误。

普通测试仅覆盖内存/纯逻辑及失败回滚，不创建真实TUN/路由。Win/mac/Linux交叉构建不等于原生通过；真正系统变更只在显式手动验收入口执行。没有实现的平台不能返回空成功。

## 整合者

提供packetio实现及测试，统一go.mod/go.sum，整合A/B，新增明确独立于生产GUI的手动TUN最小验收工具。默认自动测试始终不加载驱动、不改路由；原生手动验收目标使用不与管理网重叠的测试/32地址，设时间预算，记录创建前后设备/路由与已有22端口，结束回收。

已有Wave 7修改不得覆盖。按用户既有授权使用Paseo独立worktree并行，完成后由整合者核对，DeepSeek只读复审；子代理不访问VM、不安装驱动、不sudo、不commit/push，不改其他包及依赖文件。

## macOS与mgu_kit验收条件

本地源码证据：mgu-go/internal/mgudiag/discovery.go向各网卡定向广播及255.255.255.255、169.254.255.255发送六字节6811探测，从UDP源IP取网关；internal/svc/discovery_service.go随后连该IP:6801，调用ReadMguIP并取返回首个IP作为SSH主机，端口22。

因此支持mgu_kit无修改自动检测，必须同时满足：本机收到广播、向原源端口单播回原始发现回应、回应源IP的6801可达、返回的真实MGU IP具有正确路由、MGU:22可达。utun是点到点IP设备，不能假定有以太网广播语义，也不能把“创建utun成功”当作mgu_kit可发现。保留平台发现Presenter，与TUN数据呈现组合；mgu_kit的HSFZ发现可在Operator本地回答，不必等待通用UDP隧道。MGU探测及路由就绪后再宣告可用。两个阶段分别用模拟发现/只读DID fixture、实际mgu_kit和最后真实台架验收。

macOS已有的PID解析目前返回ErrUnsupported；不能把Windows独占保证带到Mac。原生Operator需要单独的系统初始化权限；本轮不要求用户Mac开启SSH或无人值守访问，用户仍手动验收。发布签名、公证、Network Extension分发形态与privileged helper另行落实。

## 已交付与验证（2026-11-17）

已新增：

- `internal/packetio`：完整IP包边界、有界内存双端设备、缓冲所有权及单次调用取消。
- `internal/l3`：gVisor IPv4/TCP栈，精确目标白名单、SYN/活跃连接预算、可选Admit及释放、原样Target/Origin、半关闭和取消汇合。
- `internal/tunadapter`：Wintun/utun/Linux TUN、系统地址和host route预检、只操作新建设备、成功步骤逆序清理、路由冲突拒绝及Windows地址就绪判定。
- `cmd/renet-tuncheck`：显式 `--run` 的独立原生验收入口，默认dry-run，无GUI/生产会话接线。

Linux最终全仓CGO=0 vet/test/build、CGO=1 race通过，gofmt及diff检查无输出；依赖tidy无差异，Windows amd64和macOS arm64纯Go构建通过。DeepSeek对核心三包、CLI清理和报告、Windows就绪查询做只读复核，确定问题均修复。没有对新默认GUI路径或真实车辆作未经执行的通过声明。

### 原生Windows结果

Windows 11 VM 192.0.2.107使用官方Wintun 0.14.1发行DLL：ZIP SHA256为 `07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51`，DLL Authenticode状态Valid、签名主体WireGuard LLC。程序以已授权的管理权限在独立测试目录运行，不修改现有管理网卡、防火墙或sshd设置。

最终无overlay的 `native-run-03` 完成17项阶段，退出0、success=true：

| 项目 | 结果 |
|---|---|
| 原生设备/路由 | 新建随机私有名Wintun，Local=198.18.8.1/32，只有目标198.18.8.2/32的远端host route |
| 数据通路 | OS TCP→TUN→gVisor→双向钉扎mTLS/yamux/session→loopback fixture |
| 22 | 虚拟目标198.18.8.2:22读取到宿主OpenSSH banner；只验证banner，不做认证登录 |
| HTTP | 虚拟目标198.18.8.2:8080 GET固定响应逐字匹配 |
| 地址保留 | 两个Target原样，Origin均为198.18.8.1加实际临时端口 |
| TCP资源 | Accepted=2、Active=0、Rejected=29；拒绝计数包含被拒包/尝试，不推算网络丢包率或作为零值成功条件 |
| 清理 | 所有清理阶段成功，无cleanup_errors；新设备、地址与路由回收 |
| 宿主共存 | 宿主22在前/中/后与隧道读取的banner一致；外部快照中sshd仍是PID5848，监听0.0.0.0:22与[::]:22 |
| 系统后置条件 | 网卡列表、IPv4路由和SSH监听与测试前逐项一致；Wintun共享驱动保留，不删除公共驱动 |

上一轮无overlay `native-run-02` 也完成相同通路，17阶段均通过；最终03另外覆盖重复地址条目就绪判定修复后的源码。原生报告、系统前后快照与二进制摘要保存在 `/tmp/remote-enet-wave8/native/`，不把身份密钥或邀请码放进报告。

### 原生发现的问题及修复

1. **ENET地址校验**：单纯IsGlobalUnicast会排除169.254/16。已允许IPv4 link-local单播，并保留已有管理地址/路由重叠时的拒绝策略，补Local/Target及无效地址回归。
2. **地址尚未可绑定**：首轮native-run-01创建成功但bind源地址返回10049。诊断overlay只用于观测，在同一新接口上采得Tentative→Preferred约2.8秒；不是端口22被占用。正式实现按新设备LUID、精确Local /32、接口Up及DadStatePreferred等待，5秒预算内轮询，异常/超时完整回滚，不用固定sleep或关闭DAD。静态地址重复条目聚合判断，异常状态优先，结果不依赖行顺序。GetAdaptersAddresses是同步API，检查ctx前后但不宣称单次系统调用可硬取消。[DAD状态定义](https://learn.microsoft.com/en-us/windows/win32/api/nldef/ne-nldef-nl_dad_state)、[API说明](https://learn.microsoft.com/en-us/windows/win32/api/iphlpapi/nf-iphlpapi-getadaptersaddresses)。
3. **探针提前断开**：只读SSH banner后立即关闭会让后端sshd在预认证阶段重置，工具的严格flow判错正确地给出失败。现在持有隧道探针到清理标记之后再关闭；运行期RST和目标/拨号错误仍使报告失败，未放宽数据校验。
4. **报告正确性**：verifyFlows现在拒绝任何真实flow.Error，包括已有两个必需成功流后附加的失败流。fixture目标/拨号错误始终保留，仅主动清理时的Pipe中断归为预期。

首轮与诊断失败报告保留，不覆盖改写成成功。普通测试不执行这些原生系统变更。

### 尚待验收

macOS utun和Linux真实TUN尚未运行；macOS只有跨构建证据，Linux本环境没有/dev/net/tun，未创建系统设备节点。mgu_kit真实自动发现及MGU IP查询、真实MGU SSH、通用UDP/DoIP、TUN路径的PID独占与GUI接线、HTTP回连、真实WAN及刷写都未由本轮证明。下一阶段先接MGU发现/目标路由与本地HSFZ呈现，再用原版mgu_kit单独验收。
