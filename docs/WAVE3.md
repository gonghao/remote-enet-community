# Wave 3：发现广播与 CI 稳定性

基线 `4f8eb5f`。先修复首次远端 CI 暴露的关闭时序问题，再补齐 Operator 的广播接收与固定源地址应答。本轮仍不宣称完成真实 E-Sys / ISTA / Tool32 验收，不写 GUI、DoIP、L3 或车辆业务，不改现有控制消息格式。

## 已确认的问题

首次 GitHub CI（run `43713580670`）的格式、vet、交叉构建通过，普通测试与 race 失败：

- `session.Run` 在配对拒绝时可能把已得到的 `ErrPairing` 覆盖为内部 context 取消，导致 CLI 无法稳定识别拒绝。
- `TestVersionMismatch` 对仿真对端在已被断连后完成写入的要求存在时序假设。
- 假网关注入 RST 时，连接重置可发生在 Dial 返回之前，不限于第一次 Read。

这些路径已用未缓存的聚焦重复测试复现，不能通过放宽“必须拒绝”的断言或任意延时掩盖。

## 分工

- A：只改 `internal/session/`、`internal/testutil/fakezgw/` 及必要的 `cmd/renetd/main_test.go`；修复错误归因与不确定测试时序，保持协议和 API 不变。
- B：新增标准库实现的 `internal/udpdiscovery/`，负责通配 UDP 接收、入站网卡过滤及固定源 IP/端口回复；支持 Linux、Windows、macOS。
- C：修改 `internal/operator/` 使用 B；补广播回归及受支持平台的原生 socket 测试 CI。其他目录由整合者协调。
- 整合者：跨包测试、README/设计和 M0b 说明、最终检查及 Paseo/OpenCode DeepSeek review。

独立 worktree，不 commit/push，不新增依赖。代码、注释、日志英文，文档中文。所有测试只用 loopback、内存或 net.Pipe；生产与普通检查 CGO=0，race 单独 CGO=1。实现完成后停止写入，报告供整合者复制，不互相改文件。

## B：UDP socket 契约

```go
package udpdiscovery

type Conn struct { /* private */ }
func Listen(ctx context.Context, bind netip.Addr, port int) (*Conn, error)
func (c *Conn) LocalAddr() *net.UDPAddr
func (c *Conn) Read(ctx context.Context, b []byte) (int, *net.UDPAddr, error)
func (c *Conn) Write(ctx context.Context, b []byte, peer *net.UDPAddr) (int, error)
func (c *Conn) Close() error
```

`Listen` 绑定 `0.0.0.0:port` 以接收广播，port=0 使用临时端口；对外 LocalAddr 的 IP 仍返回显式 bind，端口为实际接收端口。bind 必须是有效且可归属到启用网卡的 IPv4 地址。显式 loopback 测试可通过本机 loopback 前缀识别其他 127 地址，无需配置系统别名。

使用 Go 自带 `net.UDPConn.ReadMsgUDP/WriteMsgUDP` 和 `SyscallConn`，按平台启用、解析、构造 packet info：Linux `IP_PKTINFO`、Windows `IP_PKTINFO/IN_PKTINFO`、Darwin packet info。不要自建 WSA 事件循环，不用 cgo；阻塞由标准 net poller 与 context/Close 管理。不得退化为未限定源地址的普通 WriteToUDP，也不得在缺少必要 metadata 时继续应答。官方 x/net/ipv4 当前未实现 Windows control message，增加该依赖不能解决本问题。

`Read` 只交出来自选定网卡的有效 IPv4 数据报，并校验目的地址为 bind 或允许的本地广播；丢弃截断的 payload/control data、缺失/无效 metadata 和其他接口/其他目标 IP，不把全局通配监听变成全网卡响应。不要限制请求源必须与 bind 同子网，以免破坏同机 LAN/alias 测试。`Write` 始终固定回复源 IP=bind、源端口=接收端口、接口=选择的网卡，回复目的地仍是请求源 IP:port。payload 不解析；回复必须非空，以避免 BSD 标准库在只有控制消息时自行补字节。

构造与解析控制消息必须有长度、对齐和类型校验，不越界使用 unsafe。跨平台代码用 build tags；不支持的平台返回明确错误，不假装广播可用。Linux 要实际验证单播和 loopback 定向广播、源 IP 与端口保留、过滤/截断/取消。Windows/macOS 的 codec 与单播 metadata socket 测试需可在对应原生 runner 执行；交叉编译不能当作运行验证。

## C：Operator 与 CI

保留已有 `operator.Options` / `Presenter` 导出接口、两个 TCP 通道、State 原子更新与会话所有权；仅把发现 socket 换成 B 的实现。UDP 回放仍是完整 HSFZ 原帧，精确验证 probe，单播回复请求源端口。失败时全部监听资源必须回滚。

广播集成测试在 Linux loopback 内完成。与车端的 TCP 连接仍从选定源 IP 拨出，Hub 只搬密文。本轮不以操作系统原生 packet-info 测试代替 E-Sys/ISTA 应用兼容性测试。

CI 原有 Go 1.27、格式、vet、普通测试、race 和交叉构建保留；新增 Windows/macOS 原生 job 运行 B 的可移植单播 metadata/codec 测试及 Operator 可移植发现测试。原生测试不得依赖“所有 OS 都能 bind 未配置的 127.0.0.2”这一 Linux 假设，也不得借 skip 把必需功能失败隐藏。Linux 专有广播路由测试可限定 Linux，并明确平台范围。

## 验收

未缓存复跑 CI 失败的路径，覆盖配对拒绝和版本拒绝、双向关闭、RST 拨号/读取错误。全仓普通与 race 通过；Linux 完整 CLI 黑盒及广播发现通过；Win amd64/macOS arm64 纯 Go 构建通过。新增原生 CI job 等推送后才可确认实际结果，真实 Windows VM 工具验收仍待 M0b。

## 本轮结果

三项已整合到本地，未改控制消息格式、未新增依赖。握手 ctx 从调用方派生，内部关闭不再覆盖已解码的协议拒绝；新增确定性夹具在旧实现四条路径全部失败，修复后通过，已知失败路径的普通/race 聚焦重复检查通过。RST 测试仍要求 `ECONNRESET`，只消除了必须在 Read 阶段出现的假设。

Operator 使用标准库 packet-info socket，Linux 的低层广播、固定源 IP/端口、过滤和完整应用广播链路已通过；单次取消后可复用 socket，owner 回调会在 Close 中汇合。新增 CI Windows/macOS 原生 job，测试代码使用可移植的真实 `127.0.0.1` 地址。本轮另为 `--invite-file` 补 UTF-8 BOM 兼容及回归，方便 Windows PowerShell 创建文件。

整合后本地执行并通过：gofmt、vet、`CGO_ENABLED=0 go test -count=1 -timeout 120s ./...`、`CGO_ENABLED=1 go test -race -count=1 -timeout 120s ./...`、Windows amd64/macOS arm64 纯 Go build 及 UDP 测试二进制交叉编译。首次远端 CI 的失败记录不会因本地修复自动变绿，需后续提交推送触发新运行。

Paseo/OpenCode 的 DeepSeek V4 Pro 已完成只读复审，未发现确定缺陷；审查覆盖平台布局、过滤、定源回复、取消/截止时间、关闭回收与握手错误分类。Windows/macOS 原生运行时仍待 CI 执行，目标 E-Sys/ISTA/Tool32 仍待 [M0b 指南](M0B.md) 的工具级测试，不能用交叉编译替代这些验收。

推送后的首次原生 CI（run `43716790597`）中，Linux 常规/race、两种交叉构建及 macOS 原生测试通过，Windows 的非空数据报发送返回 `n=0, err=nil`，触发计数断言。经核对 Go 1.27.4 和 Winsock 文档，为这一 Windows 同步成功路径补齐原子数据报计数，不重发；错误、非零短写及控制消息短写仍拒绝。随后 run `43717519633` 的 Windows 原生检查通过，新增的空数据报边界暴露 macOS 标准库会补一个字节；发现层统一明确拒绝空回复，并保留实际非空/单零值字节载荷的原样收发断言。最终各平台结果以修复后提交的 CI 为准。
