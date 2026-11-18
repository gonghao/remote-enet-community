# app 运行时

`cmd/renetd` 的 `invite`、`client`、`operator` 均接入本包。CLI 负责参数、日志输出目标及退出码；本包持有身份/信任存储、transport、session、forwarder/presenter 的生命周期。一个 `Runtime` 只运行一次，停止由运行入口的 context 控制；重新启动应用需创建新的实例。

## 导出入口

```go
func New() *Runtime
func IssueInvite(ctx context.Context, cfg InviteConfig) (string, error)
func (r *Runtime) RunClient(ctx context.Context, cfg ClientConfig) error
func (r *Runtime) RunOperator(ctx context.Context, cfg OperatorConfig) error
func (r *Runtime) Snapshot() Snapshot
func (r *Runtime) Subscribe(ctx context.Context) <-chan Snapshot
func (r *Runtime) WithSession(ctx context.Context, fn func(context.Context, uint64, *session.Session) error) error
func (r *Runtime) TestLink(ctx context.Context, options linktest.Options) (linktest.Result, error)
func (r *Runtime) TestLinkForGeneration(ctx context.Context, generation uint64, options linktest.Options) (linktest.Result, error)
```

配置类型为 `InviteConfig`、`ClientConfig`、`ClientHooks`、`OperatorConfig`；快照类型为 `Snapshot`，阶段类型为 `Phase`。具体字段见对应 Go 声明。运行时错误包括 `ErrAlreadyRun`、`ErrNotConnected`、`ErrNICChanged`。

网卡选择新增接口：

```go
func carside.SelectNICFrom(nics []carside.NIC, name string, addr netip.Addr) (carside.NIC, error)
```

`carside.ErrNICUnavailable` 表示选择的地址暂不可用，可等待；`carside.ErrNICAmbiguous` 表示有多个候选，需要明确选择。现有 `SelectNIC` 使用同一规则。

## 连接与恢复

- `ClientConfig` 三选一：邀请码、UTF-8 邀请文件（支持 BOM）、已配对指纹。配对后保存对端；成功握手后重连不再携带 token。Operator 仍先查信任，再原子消费一次性 token。
- 客户端默认发现超时 3 秒、无车重试 1 秒、网卡检查 1 秒、网络退避 100 毫秒至 5 秒。app 的时长零值使用默认值，负值报错。CLI 新增 `--discovery-retry`、`--nic-check`，均须为正数。
- 无网卡、所选固定 IP 不在、无车辆均进入等待。每次发现/重连重新枚举；发现完成后再次验证源地址/前缀，避免用发现期间失效的地址拨号。多候选网卡、多网关直接停止。
- 连接期间只重新枚举本机网卡。确认名称/地址/前缀变化或所选地址消失时，取消旧代次，关闭旧 transport 和车辆 TCP，等待 forwarder 退出后重新发现。不重放旧隧道请求。
- 枚举失败本身不能证明地址改变：未连接时退避，已连接时保留会话并记录错误。不在连接期间用 UDP 发现检查车辆存活，也不增加车辆 UDS 探测。
- 客户端配对拒绝、版本不兼容、存储错误和明确配置错误停止，不无限重试。Operator 保留服务端语义：拒绝有问题的来访会话后继续监听；授权存储损坏或本地 listener 配置失败停止整个运行时。
- Operator 串行接收；旧 presenter 的 listener 和 worker 全部回收后才调用下一次 `Accept`。Hub 的 busy、不排队、不抢占规则沿用既有实现。

`ClientHooks` 可以注入枚举、发现和等待函数，所有阻塞必须随传入的 context 退出。生产零值使用真实网卡、UDP 发现和定时器。

## 状态与后续接线

快照包含独立复制的 `Metrics observe.Snapshot`、值类型的 `Exclusive exclusive.Snapshot`，以及角色、阶段、设备/对端指纹、网卡/源地址/前缀、网关/VIN、路径、代次、最近错误、下次重试、RTT 和本地呈现端点。`RTTValid=false` 表示当前没有有效 RTT；真实测量的 0 仍是有效样本。`Generation` 在成功握手后的新会话开始观测时递增，在单次运行中不复用；失败的拨号不会递增。代次在发布 `connected` 前已经建立，等待车辆状态或配对存储失败的已握手会话也会结束其观测代次。断线后保留最近选定/接收的车辆信息供诊断，阶段、路径和呈现端点表示当前是否可用；重新发现时清除旧网关/VIN。

订阅通道容量为 1，更新覆盖尚未读取的快照；取消或运行结束关闭通道，结束时合并发布最终状态。每个订阅通过 context 回调回收，没有长期驻留的订阅 goroutine。正常消费不保证看到每个中间阶段。

整合连接测试时使用 `WithSession`：回调取得当前代次与 session，其 context 同时受调用方和该代次约束；回调须遵守取消，不得保留或关闭 session。不会把旧代次操作切换到新会话。`TestLink` 使用同一借用机制，只向已协商的 renet 对端交换测试数据；部分结果可与错误一起返回。`TestLinkForGeneration` 给排队的 CLI/UI 请求绑定代次，不匹配时返回 `ErrNotConnected`；代次为 0 等同 `TestLink`。

Operator 只能获得既有 `VehicleState` 携带的网卡名称和地址，前缀保持未知；本轮没有修改协议 payload。

## 验证

新增测试使用可控枚举/发现/等待函数、内存 transport 和 loopback TCP，覆盖等待恢复、IP/前缀变化、固定地址不漂移、歧义停止、发现期间地址变化、暂时枚举失败、有界可取消退避、代次借用取消、旧车辆 TCP 关闭且不重放、慢订阅及退出并发。

`TestPublicRuntimesReconnectAndReleasePresenter` 使用真实 app 客户端/Operator、端到端 TLS 与假网关，确认同一配对身份自动重连、新代次复用固定的三个呈现端口、旧工具 TCP 关闭、多诊断流和 RTT 可用、断开后端口可以重新绑定。旧 CLI 生命周期测试迁到 `connection_test.go`，原有跨包 CLI 黑盒保持原样。

未验证真实网卡热插拔、Windows 原生运行、真车、真实网络丢包、工具兼容性和 HTTP 回连；跨平台构建不代替这些验证。

## 指标接线

Runtime 持有一个有界 `observe.Tracker`。每个成功握手的会话通过专属句柄调用一次 `BeginSession`，退出时在所有 forwarder/presenter worker 汇合后调用 `EndSession`。旧字节句柄、RTT 回调和排队测试都不能移到新代次；终身字节总量保留，当前代次计数与 RTT 重新开始。

`session.Options.OnRTT func(time.Duration)` 仅在有效 Pong 后调用一次，在 `pingMu` 锁外执行，回调必须快速且不做 I/O。app 直接把该样本提交给对应代次，不从快照轮询 RTT 来构造样本。

`operator.Options.Observer tunnel.Observer` 与 `carside.WithObserver(tunnel.Observer)` 覆盖打开失败、准入拒绝及真正的 Pipe 转发；后者通过可选构造参数注入，不修改既有两参数调用。公共接线接口：

```go
type Counter interface {
    AddIn(uint64)
    AddOut(uint64)
}
type Activity interface {
    Counter
    End(error)
}
type Observer interface {
    Begin(target string) Activity
}
```

`Stats.Counter` 可选，Pipe 在每次成功写入后提交实际写入长度，部分写入也只计算成功部分。原有计数、函数签名、双向 EOF/半关闭与取消行为保留，没有每条隧道的轮询 goroutine。

指标按本机视角记录车辆 TCP 透明载荷：`In` 为远端 renet → 本机 TCP，`Out` 为本机 TCP → 远端 renet；包含原本的 HSFZ 字节，不包含 OpenTunnel 头、控制 Ping/Pong、TLS 或专用连接测试字节。app 不解释这些载荷。`Inbound/Outbound` 描述隧道发起方，与字节方向分开。

Tracker 活动容量耗尽时继续透明转发，记录 `metrics-incomplete` 事件并设置 `Snapshot.MetricsIncomplete`。容量之外的连接不计入活动数、字节和历史；这些指标是部分测量。该标记跨代次保留，因为终身总量也会缺失这些字节。正常订阅和直接快照分别拥有独立的切片及错误事件指针，调用方修改自己的快照不会影响其他订阅者。

## CLI 与独占

客户端和 Operator 都新增以下参数：

| 参数 | 默认 | 行为 |
|---|---|---|
| `--allow-link-test` | `true` | 协商、接受专用对端测试；可用 `--allow-link-test=false` 关闭 |
| `--connection-test` | `false` | 对每个新连接代次运行一次专用测试，打印英文结构化结果 |
| `--connection-test-timeout` | `10s` | 使用 linktest 的配置校验及硬上限，0 沿用其默认值 |
| `--status-interval` | `5s` | 打印阶段、代次、RTT 有效性/样本数、当前/终身字节、速率、活动连接、指标缺失与独占状态；0 关闭 |

CLI 的测试队列容量为 1，过期代次会被拒绝，成功、不支持、超时、取消和其他错误分别输出；不会把失败写成成功。专用 echo 吞吐只按 linktest 的测量语义显示，不能解释成单向带宽、TCP 丢包率或车辆作业安全性。状态 worker 和测试 worker 都随 context 或运行结束取消，在 CLI 返回前汇合。

app 的两个配置增加 `AllowLinkTest bool`；app/session 零值仍关闭，只有 CLI 默认启用。`OperatorConfig` 另外增加 `Exclusive bool` 和 `Resolver exclusive.Resolver`，传给实际 Presenter。CLI `--exclusive` 仅 Windows 默认开启，其他平台默认关闭，可显式 `--exclusive=false`。非 Windows 开启且未注入 resolver 时，app 在身份存储或 listener 启动前返回可匹配的 `exclusive.ErrUnsupported`；注入 resolver 的可用性由调用方负责。快照通过 Presenter 的真实 gate 读取 owner 与连接数，重连后不沿用旧 owner；不按父子进程或可执行文件名放行。

新增回归包括真实 app/CLI 连接测试的协商成功与不支持、超时后原诊断连接继续可用、连续代次自动测试、测试期间假网关连接/诊断计数不变、双向逐字节计数与速率、真实独占状态、失败隧道记录、容量满时透明半关闭、订阅间指标隔离以及有效/重复/无效 Pong 的单样本回调。原有跨包 CLI 黑盒未修改。

## 会话内 MGU direct link

`Snapshot.Direct` 独立于 HSFZ 会话状态。`ConnectMGU(ctx, expectedGen)` 和 `DisconnectMGU(ctx, expectedGen)` 的零代次表示当前；非零代次过期时操作前拒绝，不切换到替代会话。默认不查询 MGU，`OperatorConfig.AutoMGU` 仅供 CLI 显式启用；GUI 使用显式会话操作。

启用通过 Presenter maintenance 原子占用空闲网关，仅在一次有界只读发现期间持有 maintenance；发现完成后释放 maintenance，再等待工厂授权并启动 L3，因此授权等待不阻塞新 HSFZ 工具。随后为唯一 MGU IPv4 创建本地 `/32` 与目标 `/32` 路由。失败只更新 Direct，不关闭健康 HSFZ。Windows HSFZ 与 L3 共用同一 `exclusive.Gate`；内部查询不附着工具 owner。L3 successful-write 增量复用会话 observer，peer-only link test 不计入车辆流量。

`DirectFactory` 为测试注入内存设备，生产默认使用 `tunadapter.OpenAuto`。工厂必须响应上下文取消，并在失败时清理部分资源。调用方 ctx 约束启用；成功交接后资源属于会话，不随调用方 ctx 结束。禁用、会话结束、网关或 NIC 地址更新取消并汇合查询和启动，关闭顺序为 L3、设备；清理错误保留且阻止后续启用。地址更新只撤销旧链路，须再次显式启用，不自动重放查询。

所有自动测试使用 memory/loopback 和模拟工厂，不调用原生网络初始化。网络配置需要运行 Operator 的用户具备相应权限，不隐式提权，不修改其他用户配置。原生 Windows/macOS/TUN 路由验收由整合者另行执行。

L3 设备读写失败会触发栈退出；Operator 每秒检查并回收失效链路，启用前也同步检查，避免重复启用假成功。故障只将 Direct 标记为 Failed，健康 HSFZ 继续服务。回收由同一管理器串行执行，不新增 watcher；正常禁用不作为故障，旧管理器不能发布到替代会话。清理失败持续保留并禁止不安全重启。

`OperatorConfig.Capture` 可注入调用方管理的 `hsfzcapture.Recorder`。Runtime 不创建文件或启停 Recorder；只为准入后的新 Presenter TCP、发现 UDP 数据报和内部 MGU 只读查询接线，元数据绑定实际会话代次。已有 TCP 不会在 Start 后中途附着，Stop 后旧句柄不会进入下一文件。内部查询使用独立包装，`Local=internal` 表示没有本地 TCP socket；L3/SSH、握手和 peer-only 测试不进入抓取，payload 不加入 Runtime 快照或匿名指标。录制拒绝、丢录或文件失败不改变转发结果。
