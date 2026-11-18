# Wave 4：稳定性、观测与诊断工具独占

基线 `ed66f7f`。本轮用户已确认按四个工作包推进，先完成 CLI 核心与 Windows 工具兼容性收尾，再进入 M3 GUI。仍不实现车辆编码/刷写业务、DoIP、L3 或商业控制平面。

## 工作方式与范围

三个独立 worktree 的 Codex 代理负责 B/C/D；整合者负责 A 的 Windows 实测、接口接线、跨包回归和文档。完成后由 Paseo/OpenCode DeepSeek 做只读复审。所有代理不 commit、不 push、不改其他工作包的目录；报告新增文件、检查结果、接口变化和未验证边界。

Go 1.27，生产 CGO=0，race 单独 CGO=1。无新增依赖。自动化测试仅 loopback/net.Pipe/内存，不访问 Windows VM 或真车。Windows VM 的人工/远程自动化实测单列记录，不写入单元测试。现有主仓文档修改须保留。

## A：工具兼容性收尾（整合者）

已有实测见 `docs/research/m0b-win11-2026-11-16.md`：E-Sys 与 ISTA 的 LAN 发现成立；Tool32 Autodetect 报 NET-0012；E-Sys 首个未配置请求为 `22 F1 86`，在长 alive fixture 后观察到 `00 00 00 02 00 12 00 F4`。

先对照 Tool32 的显式 IP 与 Autodetect，核对实际配置路径与发包，不先假定必须 link-local。修改 Windows 应用配置前备份，仅改本次测试字段，结束恢复。需要独立网段时另建隔离测试地址，不能替换管理网卡地址或影响 SSH。

假网关可增加显式的实验配置以比较 alive 控制字；默认依据原文献/PCAP，不把一次观察提升为通用协议规则。仅补有来源的只读 fixture，未知请求不默认成功。生产隧道原样转发，不代答 HSFZ alive。

## B：运行时、重连与 app 状态层

目录：`internal/app/`、`cmd/renetd/`、`internal/carside/`。不改 session、operator、secure、proto、tunnel、integration 或依赖文件。

把目前 CLI 中的连接生命周期抽到 `internal/app`，CLI 保留参数解析、日志与退出码，GUI 以后可直接复用 app。实际连接逻辑必须使用新 app 层，不能只新增未接入的状态类型。保留 invite 文件读取/BOM、信任存储、单会话和 CLI 参数行为。

app 至少提供客户端与 Operator 的配置/运行入口、可复制的状态快照和有界状态订阅。状态包含角色、阶段（发现/连接/已连接/退避/停止等）、选定网卡/源地址、网关/VIN、对端/路径、会话代次、最近错误、下次重试时间与 RTT。不得把邀请 token、私钥或车辆隧道内容写入快照和日志。订阅通知应合并，慢订阅者不阻塞网络；取消与退出回收订阅 goroutine。

具体导出入口可按现有代码选择，提交报告列出签名。另提供读取当前已建立 `*session.Session` 的受控入口或回调，供整合者接入 C 的连接测试；不能在 app 外长期拥有连接生命周期。不要引用尚不存在的 observe/linktest/exclusive 类型，整合者负责跨包接线。

生命周期要求：

- 网络连接错误进入可取消、有上限的退避；配对拒绝、版本不兼容、存储损坏和明确配置错误仍应停止，不能无限重试隐藏错误。
- 无车、所选网卡暂时消失或临时无地址可以进入等待状态并重新探测。多网卡、多车歧义不任意选第一项；显式固定 IP 消失时不得偷偷切换到其他 IP。
- 重连时重新枚举指定网卡，不能持有启动时的过期地址/前缀。运行中所选地址/前缀确定发生变化时，关闭旧会话及 TCP，重新发现、重新建立；不能将旧请求重放到新连接。
- 增加可配置的网卡检查与发现重试周期，测试用注入枚举/发现/等待函数控制事件，不依赖真网卡热插拔。
- 不因一次 UDP 发现未回复而切断仍健康的诊断 TCP；不增加车端 UDS 轮询。
- Operator 接收重连后必须先回收旧 presenter 与端口。保留 busy、不排队、不抢占的 Hub 语义。

测试覆盖：无车后出现、网卡消失后恢复、IP/前缀改变、显式地址不漂移、歧义拒绝、重试取消、断开后旧 worker/端口释放、代次不混淆和快照/通知并发。已有 CLI 黑盒测试必须继续通过。

## C：观测与专用连接测试

目录：`internal/observe/`、`internal/linktest/`、`internal/session/`。必要时仅补 `internal/proto/README.md` 的扩展说明，不改既有消息数字、字段编码、版本号或车辆消息。不改 B/D 目录和依赖文件。

### observe

实现与 UI 无关、线程安全的指标聚合，纯叶子包，不 import app/session/operator/carside。支持会话代次、隧道开始/结束、字节计数、活动连接数、当前速率、RTT 样本及 p50/p95、最近错误/事件。快照不得暴露内部可变 map/slice；RTT、历史连接和事件必须有界，关闭统计幂等。字段区分测量方向和有效样本数，不把未测量写成真实的 0ms。

提供方便整合者在 Pipe 周围接线的句柄/方法，例如：

```go
tracker := observe.New(/* bounded options, if needed */)
lease := tracker.BeginTunnel(/* target/direction/session generation */)
lease.AddIn(n)
lease.AddOut(n)
lease.End(err)
snapshot := tracker.Snapshot()
```

签名可按实现调整，但不要自己修改 carside/operator 的转发；整合者接线。可用现有 tunnel.Stats 读取结束计数，也可提供受控 Writer 计数封装；统计不能改变半关闭与转发行为。

### linktest 与 session 接入

专用测试流终止在两个 renet 对端，永远不连接车辆网关。使用已有 Hello capability 机制协商 `linktest/1`，数据流仍使用现有 OpenTunnel 编码，保留 Kind `linktest/1`；Target/Origin 不承载车辆地址。没有协商就返回可识别 ErrUnsupported，不向车端 OnTunnel 或反向流回调下发测试数据。

session 增加 `Options.AllowLinkTest bool`（零值关闭），为 true 时自动声明 capability；协商成功后可用 `Session.TestLink(ctx, linktest.Options) (linktest.Result, error)`。非协商对端发来该 Kind 时拒绝该流，不破坏其他正常流。既有普通 tcp/reverse 流规则不改。

测试协议自己定义并记在 linktest README；每帧长度、请求总量、持续时间、并发数有硬上限。建议默认 RTT 探测 10 次、payload 上限 1MiB、总持续时间不超过 10s；任何配置也不能超过 8MiB、1024 样本、64KiB 单帧和 15s。每个 session 同时最多一个入站和一个出站测试；请求已收到的服务端也必须按自己的时间预算退出，不能依赖来访方主动关闭。所有阻塞随 ctx/会话取消解除，无每次取消遗留的后台 goroutine。

结果包含实际完成探测数、RTT p50/p95、字节数、持续时间和有效载荷吞吐；明确 echo 吞吐的测量语义。记录超时/中断，不把 TCP 测试当 IP 丢包率，也不声明刷写 safe。传输故障与协议错误须可区分，部分结果与错误可同时返回。

覆盖：协商/不支持、正常探测、延迟/慢读、黑洞、超大/畸形帧、限额、同时测试与隔离、取消后其他流仍可用、关闭回收；用实际内存/loopback transport 测试 session 接线，不只测裸 codec。

## D：Windows 工具独占

目录：`internal/exclusive/`、`internal/operator/`。不改 CLI/app/session/carside 或依赖文件。

实现可注入的连接所有者解析与独占 gate。独占针对同一个本地诊断进程，允许它建立多个诊断/控制 TCP；已有连接期间其他进程被拒绝，最后一条连接结束后释放 owner。进程身份包含 PID 与创建时间，避免 PID 重用继承旧 owner。不要按 exe 名、地址或端口把所有工具误分为同一 owner。

Windows 原生解析通过 GetExtendedTcpTable 查实际客户端 socket 的反向四元组，再通过进程创建时间确认身份；处理行出现延迟、进程退出、权限/查询失败，ctx 有界。任何未知所有者在独占开启时拒绝，不能静默放行。不需要安装驱动、提权车端或读取车辆数据。

建议契约（可在报告提出必要调整）：

```go
type Identity struct { PID uint32; Created uint64 }
type Resolver interface {
    Resolve(ctx context.Context, acceptedLocal, acceptedRemote netip.AddrPort) (Identity, error)
}
```

补 `operator.Options.Exclusive bool`、`Resolver exclusive.Resolver`。零值仍关闭，以兼容现有 presenter 调用；显式开启而 Resolver=nil 时使用平台默认解析器。Windows 有实现，其他平台明确 ErrUnsupported，不假装支持；Linux gate 测试可注入 resolver。CLI 的 Windows 默认开启和 `--exclusive=false` 由整合者接线，B 不等待 D 编译。

Owner 的占用必须在 admission 时原子决定；打开 tunnel 失败、ctx 取消、半关闭结束、Presenter.Close 均释放对应 lease，跨诊断与控制两个 listener 共用一个 gate。关闭模式不做 PID 查询，也不限制原有多连接。提供只读独占状态快照，供 app/日志接线。暂不自动按父子进程树放行，若实际 ISTA 使用多个辅助 PID，在实测报告中列为需要明确策略的兼容性边界。

测试覆盖：同 owner 多连接、异 owner 拒绝、两端口并发竞争、PID 重用、查不到 owner、打开失败释放、关闭模式、取消与 Close 释放。Windows 原生测试用真实 loopback TCP 验证四元组到 PID/创建时间，不依赖真实 E-Sys/ISTA。

## 整合与验收

整合者在 B/C/D 完成后接入指标、app 连接测试命令与 CLI 状态日志，以及独占开关。允许在本轮既定范围内添加必要的 Options 字段和 app API；既有 CBOR 消息格式不变，linktest 是 capability 协商后的新数据流用途。不能通过运行时配置向诊断目标发送连通性测试字节。

至少完成：

1. 全仓 gofmt、CGO=0 vet/test/build、CGO=1 race；Windows amd64/macOS arm64 纯 Go build。
2. CLI 黑盒：断线清理、重连后的新代次、等待车辆、可取消退避、多个诊断流、端口回收、连接测试不触及车辆和状态统计。
3. 可控弱网：延迟、短时黑洞和连接中断的 loopback 故障注入；内核级丢包另用隔离 netem 手动验证，不把删除 TCP 字节当网络丢包。
4. Windows 原生 owner 解析与独占手动实测，使用已经配置的 1920×1080 无 RDP 桌面。
5. 更新 README、CONTRACTS、设计稿与本轮结果，DeepSeek 复审后处理确定缺陷；未完成的实车、网关并发与 HTTP 回连继续明确保留。

## 本轮结果

B/C/D 已通过 Paseo 的 Codex 并行实现并整合，app/CLI、真实转发观测、专用连接测试和独占开关已接通；不是仅新增独立叶子包。未新增依赖，`proto.go` 的消息数字、字段和版本未改。

### 实现与入口

- `internal/app` 持有身份/信任、传输、session、presenter/forwarder，CLI 只做参数与日志。Runtime 提供独立快照、合并订阅、代次借用及 `TestLink/TestLinkForGeneration`，每实例单次运行。
- 等待车辆和网卡恢复、重新枚举源地址/前缀、可取消退避及干净重连已接入；固定地址不漂移，旧 TCP 结束后才重新发现，不重放旧请求。
- 两端实际 Pipe 提交成功写入的字节增量，RTT 仅在有效 Pong 时记录一次。观测容量满不影响转发，以 `MetricsIncomplete` 标记部分测量。
- CLI 增加 `--status-interval`、`--allow-link-test`、`--connection-test`、`--connection-test-timeout`、`--exclusive`。Windows 独占默认开，其他平台无解析器时显式开启在启动前报错。
- 假网关增加显式 alive 实验控制字、带物理源地址的 multi-responder JSON 与仅两条只读响应的 M0b fixture，默认控制字和生产透明转发规则保留。

具体签名和协议预算见各包 README；运行示例见根 README。

### 检查与实测

全仓 gofmt、CGO=0 vet/test/build、CGO=1 race 通过，Windows amd64/macOS arm64 纯 Go 构建通过。普通测试与 race 均在最终整合后未缓存运行；CI 已加入 Windows 原生多进程测试，远端执行结果以 GitHub 对应提交的运行记录为准，不能用本机检查替代。

跨包测试使用生产 Hub、TLS、yamux 和 session，覆盖两个 Hub 形态下的 stream 延迟、双向暂停、取消后同一普通流恢复、内容校验及底层断开回收；没有删掉 TCP 字节模拟丢包，也未宣称真实 netem 200ms/1% 已验收。

Windows VM 已原生执行反向四元组/PID/创建时间解析、双端口共享 owner、两个独立竞争进程的 ErrBusy 拒绝与无上游开流、最后连接释放后新进程接管和端口回收。最新 CLI 另在真实 Windows Operator / Linux Client 上完成双向 1MiB 专用测试；车辆转发计数不增加。随后 VIN 探测成功，Operator 的 In=39/Out=11 与 HSFZ 原帧长度一致，最后 owner/活动连接归零。

工具级对照：Tool32 显式 IP 的 `status_vin_lesen` 返回模拟 VIN、JOB_STATUS OKAY，恢复原始 INI 后 LAN Autodetect 仍报 NET-0012。E-Sys 显式只读 fixture 和实验 alive 控制字下显示 G168 连接建立，并连续保活五分钟以上；未配置的后续 DID 不默认成功。细节见 [M0b 记录](research/m0b-win11-2026-11-16.md)。

### 复审与测试时序修正

Paseo/OpenCode DeepSeek 完成 C/D、最终 app/CLI 接线及修复后的三轮只读复审，未发现确定的生产逻辑缺陷。缺行查询改用 10ms 到 100ms 的退避，减少反复重建 TCP 表；最新已结束代次仍可记录运行时事件的语义已补文档，字节和 RTT 保持冻结。

最终普通门禁曾复现一次 gen2 重连等待超时。确定性注入证明根因是测试把网络退避和 NIC 检查都放进手动等待队列：真实 Hub 短暂 busy 后，100ms 退避无人驱动，客户端停在 backoff。测试仅对 NIC 检查保留手动控制，其他等待用正常可取消 timer；未放宽 3 秒断言或修改生产重连语义。新增 busy、取消队列与 timer 回归后，聚焦重复、最终全仓普通与 race 均通过，DeepSeek 已核对该修复。

### 保留边界

Tool32 LAN Autodetect、ISTA 完整识别/多辅助 PID 的独占策略、真实 ENET 热插拔、内核级丢包、F20 FA/SVT/coding、网关并发及 HTTP ECU 回连仍未验收。个别取消结束事件可能保留通用 `context.Canceled` 文案，运行时能识别源地址变化并正确清理；不以此宣称具备新的车辆诊断能力。

测试结束已停止本轮服务并释放 Windows 端口，恢复 EDIABAS 原始配置、禁用测试允许规则。Windows 测试目录中的 `renetd.exe` 已更新到本轮产物，保留上一版备份；SSH、无 RDP 的虚拟显示器和原有按需桌面任务继续可用。
