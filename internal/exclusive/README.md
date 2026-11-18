# 本地诊断进程独占

`exclusive` 只负责连接准入，不读取隧道内容。一个 `Gate` 对应一个 Presenter，诊断和控制两个 listener 共用它。同一 `PID + Created` 可以持有多个 lease，其他进程在现有 lease 全部释放前被拒绝；`Created` 是进程创建时间的原始 Windows FILETIME。既不按 executable 名分组，也不推断父子进程的共享权限。

## 接口与接线

```go
type Identity struct { PID uint32; Created uint64 }
type Resolver interface {
    Resolve(ctx context.Context, acceptedLocal, acceptedRemote netip.AddrPort) (Identity, error)
}
func NewResolver() Resolver
func NewGate(resolver Resolver) *Gate
func (g *Gate) Admit(ctx context.Context, local, remote netip.AddrPort) (*Lease, error)
func (g *Gate) Snapshot() Snapshot
func (l *Lease) Release()
type Snapshot struct { Enabled bool; Owner Identity; Active int }
```

`Resolver` 的参数顺序是 accepted/server socket 的本地地址和远端地址。实现必须支持并发查询并响应 ctx；测试可以注入解析器。`Admit` 查询身份后，在锁内原子判断 owner 和增加活动计数；未知 PID、零创建时间、查询失败或超时均拒绝，没有按 IP 放行的回退。`ErrBusy`、`ErrUnknownOwner`、`ErrUnsupported` 和底层错误可通过 `errors.Is` 判断。快照全为值字段；`Active` 统计已准入的连接，包括正在打开上游 tunnel 的连接。

`operator.Options` 新增 `Exclusive bool` 和 `Resolver exclusive.Resolver`。`Exclusive` 零值关闭，此时不创建 gate，也不调用解析器。开启且 `Resolver=nil` 使用平台实现。`Presenter.ExclusiveState() exclusive.Snapshot` 可供 app 和日志读取；关闭模式返回零值快照。两个 listener 都在打开上游 tunnel 前准入，打开失败与所有退出路径统一关闭连接并释放 lease。单方向 EOF 不释放 owner，等待既有 `tunnel.Pipe` 完成双向半关闭；Presenter 取消或 Close 会等待解析、打开与转发 worker 退出，再返回空 owner 状态。车辆 Update 仍只作用于之后 accept 的连接。

CLI 的 Windows 默认开关已接入，`--exclusive=false` 可关闭；app 配置和 Presenter 的零值仍关闭。其他平台的默认解析器返回 `ErrUnsupported`；app 在启动前拒绝没有解析器的显式独占配置，直接使用 Presenter 时连接会被拒绝，不会静默降级。要在非 Windows 上运行独占测试，需注入解析器。

## Windows 实现与边界

当前 Presenter 只监听 IPv4，原生解析器同样只支持 IPv4（接受 IPv4-mapped 地址）；IPv6 返回 `ErrUnsupported`。`GetExtendedTcpTable(AF_INET, TCP_TABLE_OWNER_PID_CONNECTIONS)` 中只匹配客户端 socket 的完整反向四元组，排除 listener、SYN、TIME_WAIT 和 PID 0。表项的地址与端口按照 [Microsoft 的 MIB_TCPROW_OWNER_PID 定义](https://learn.microsoft.com/en-us/windows/win32/api/tcpmib/ns-tcpmib-mib_tcprow_owner_pid) 解码。

查询有 500ms 总预算，缺少 owner 表项时从 10ms 指数退避到 100ms，再读取新表；调用者更早的 deadline/取消优先。表缓冲上限 16MiB，单次读取最多扩容 8 次。系统调用之间检查 ctx，不为取消遗留后台 goroutine；同步 Win32 API 本身不提供可中断参数。权限拒绝或其他查询错误立即失败。

取到 PID 后，以 `PROCESS_QUERY_LIMITED_INFORMATION | SYNCHRONIZE` 打开进程，用 [GetProcessTimes](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getprocesstimes) 读取创建时间；持有进程 handle 时复核反向表项及进程存活，所有路径关闭 handle。不缓存 PID 身份。远端网络客户端、无法查询的受保护进程、已经退出的进程均不能获得 lease。本机制是本机工具并发协调，不是对本机管理员的安全隔离。

ISTA 等工具若由多个辅助 PID 建立连接，会按不同 owner 拒绝；是否引入明确的进程组策略须由实际工具实测决定。没有跨父子 PID、跨 executable 或跨 Presenter 的隐式共享。

## 验证

```sh
CGO_ENABLED=0 go vet ./internal/exclusive ./internal/operator
CGO_ENABLED=0 go test -count=1 -timeout 60s ./internal/exclusive ./internal/operator
CGO_ENABLED=1 go test -race -count=1 -timeout 60s ./internal/exclusive ./internal/operator
```

Windows 原生运行以下命令；只用 `127.0.0.1` 和内存传输，不依赖真实诊断工具。`TestNativeOwnerResolverReverseTuple` 启动独立客户端测试进程，核对返回的是客户端 PID、系统创建时间，另测未知表项、取消和进程退出。`TestNativeExclusiveDefaultResolver` 验证默认解析器对 Presenter 两个端口共享 owner，以及最后连接结束后释放。

```sh
go test -count=1 -v -timeout 60s -run '^TestNative' ./internal/exclusive ./internal/operator
```

Linux 注入测试覆盖同 owner 多连接、异 owner/PID 重用拒绝、双端口同时竞争、未知与查询失败、打开失败释放、关闭模式不查询、半关闭保留 owner、解析/打开/转发阶段的 ctx 取消及 Close 回收；表解码和延迟重试逻辑也可在非 Windows 执行。

Wave 4 已在 Windows VM 原生运行上述检查，另运行 `internal/integration/TestNativeExclusiveMultiProcess`：两个实际子进程被明确 ErrBusy 拒绝且未打开上游，最后连接释放后新进程可接管、echo、关闭并释放端口。交叉编译本身不算原生通过；真实工具的多辅助 PID、受限权限组合仍未验收。
