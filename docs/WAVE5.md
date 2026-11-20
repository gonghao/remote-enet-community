# Wave 5 · 双端 Native GUI

基线：`4cd473f`。目标：无需 CLI 即可选择角色、配置地址/网卡、生成或粘贴邀请、连接/停止、查看车辆与链路状态。复用 `internal/app`；不修改 HSFZ、CBOR、配对规则及透明转发边界。UI 锁定 mygo v0.9.1，自绘 Native UI，Windows 构建使用 `mygo_nowebview`，不使用 HTML/JS/WebView。

## 分工与边界

三个独立 worktree 并行，无人自行 commit/push。整合者负责 `cmd/renet`、mygo 依赖、托盘、跨包接线、CI、文档和 Windows VM 验收；最终由 Paseo/OpenCode DeepSeek 只读审查。

### A · `internal/ui/`

原生视图与 headless 交互测试。仅编辑此目录，不编辑 go.mod（整合者提供依赖）。依赖 mygo `ui`、settings 配置和 app 快照，不能直接访问磁盘、拨号或启动 Runtime。

```go
type State struct {
    Runtime app.Snapshot
    Running, Stopping, Testing bool
    Invitation, Notice, Error string
    NICs []carside.NIC
    TestResult linktest.Result
    Events []string
}
type Actions struct {
    Save func(settings.Config)
    Refresh func()
    Connect func(string)
    Listen func()
    Stop func()
    IssueInvite func()
    TestLink func()
    Copy func(string)
    Quit func()
}
func New(initial settings.Config, actions Actions) *View
func (*View) SetConfig(settings.Config) // UI thread only; persisted baseline / save receipt
func (*View) SetState(State) // UI thread only; must not overwrite unsaved form edits
func (*View) Complete(Action) // complete only the returned asynchronous request
func (*View) Render(*ui.Context)
```

每次状态更新依次 `SetConfig → SetState → Invalidate`；异步动作返回后取最新快照，再执行 `SetConfig → SetState → Complete(对应 Action) → Invalidate`，用每动作请求 ID 排除迟到回执。只有与本次提交相符的 Config 才确认保存，不覆盖提交期间的继续编辑，Peer 自动合入。窗口 1080×780，最小 880×680，可滚动。English 产品文案。简洁浅色工作台，顶部角色切换与明确阶段，Client 设置网卡与邀请码（粘贴/输入后连接），Operator 配置监听/公开端点与呈现 IP、生成/复制邀请、开始/停止、独占开关；两端显示 VIN/网关/对端/RTT/流量/速率、连接测试、最近事件。未知数据用占位符，禁止虚构电压/安全评分。配置需要明确 Save，再启动使用已保存值；运行中禁用角色/配置编辑。NIC 下拉项同时显示名称和 IP，支持 Automatic（歧义由 app 拒绝）；暂缺固定项要保留而非自动替换。邀请码不得进入事件、设置或测试快照。Copy 仅显式用户操作。测试覆盖按钮准入、运行/停止/测试、保存、角色、错误和未知指标。

### B · `internal/desktop/`

不依赖 mygo 的 GUI 控制器与实际 app 生命周期。仅编辑此目录。可通过接口/工厂注入 Runtime 做确定性测试；真实 app 默认实现必须可用。

```go
type Options struct { ConfigPath, StateDir string }
type State struct {
    Config settings.Config
    Runtime app.Snapshot
    Running, Stopping, Testing bool
    Invitation, Notice, Error string
    NICs []carside.NIC
    TestResult linktest.Result
    Events []string
}
func New(ctx context.Context, opts Options) (*Controller, error)
func (*Controller) Snapshot() State
func (*Controller) Subscribe(context.Context) <-chan State
func (*Controller) Save(ctx context.Context, cfg settings.Config) error
func (*Controller) RefreshNICs(context.Context) error
func (*Controller) StartClient(ctx context.Context, invitation string) error
func (*Controller) StartOperator(context.Context) error
func (*Controller) IssueInvite(ctx context.Context) (string, error)
func (*Controller) TestLink(context.Context) error
func (*Controller) Stop(context.Context) error
func (*Controller) Close(context.Context) error
```

启动方法的 ctx 约束本次请求，长期运行由 New 的父 ctx 管理；启动不得把请求结束即取消长期运行。Start/Stop/Close 并发需串行状态，Stop 完成必须等 runtime、订阅和测试 worker 退出，才能再次启动；不得跨运行代次提交旧测试结果或快照。非阻塞、有界、独立深拷贝订阅。每次启动新 app.Runtime。StateDir 下 client/operator 独立身份与信任目录，配置不混入 trust.json。Client 可用邀请首次配对，成功后将对端指纹保存为 Config.Client.Peer，后续空邀请码用该 peer（不保存原邀请）。保存失败不能假报已记住。IssueInvite 用 Operator 的 Endpoint、TTL 15min，原始码只在返回值/临时 State.Invitation 中，清除历史邀请后再生成，不进配置/事件/日志；可在监听期间发邀请。运行期间配置更改返回 busy，不隐式停止。可支持 Stop 后配置修改，Operator Stop 本轮定义为结束当前会话并停止监听，GUI 可再次 Start。连接测试绑定当前 app generation，忙/不支持/超时/取消分别显示，不向车辆发送测试。事件只记录可展示的阶段/操作/错误（有界最多 200 条，不记录隧道字节）。

### C · `internal/settings/`

无第三方依赖的严格配置与持久化，仅编辑此目录。

```go
const Version = 1
const RoleClient = "client"
const RoleOperator = "operator"
type Client struct { NICName, NICAddr, Peer string }
type Operator struct {
    Listen, Endpoint, Bind string
    DiagnosticPort, ControlPort, DiscoveryPort int
    Exclusive bool
}
type Config struct {
    Version int
    Role string
    AllowLinkTest bool
    Client Client
    Operator Operator
}
func Default() Config
func (Config) Validate() error
func Load(path string) (Config, error) // missing only => defaults, corrupt => error
func Save(path string, cfg Config) error // validated, temp+sync+rename, fail closed
func DefaultPaths() (configPath, stateDir string, err error)
```

JSON 字段 snake_case，小写 struct 标签，未知字段/尾随/超大文件/无效端点与 IP/端口 fail-closed。端口 1..65535，诊断与控制 TCP 不同，UDP 可同控制端口。默认 Client，AllowLinkTest=true，Operator.Listen=0.0.0.0:47000，Endpoint/Bind 空（尚未配置允许保存；启动前控制器必须拒绝），呈现 6801/6811/6811，Exclusive 按 Windows 默认开启。空 NICName/NICAddr 表示自动选择；显式 NICAddr 必须字面单播 IPv4，Peer 必须有效指纹。对非当前角色尚未填的字段允许空，但填写的字段必须合法。DefaultPaths 使用 os.UserConfigDir()/remote-enet/{settings.json,state}，不使用 cwd，不写密钥或邀请。覆盖丢失默认、严格损坏、保存失败/原文件不变、跨平台路径和往返。

## 验收

- gofmt / CGO=0 vet、test、build，CGO=1 race；原 CLI 黑盒保持通过。
- GUI headless 实际输入/点击测试，不仅断言静态文案；Windows amd64 / macOS arm64 纯 Go 产物。
- Windows VM：原生窗口/托盘、保存设置重启、配对/诊断发现、连接测试、停止释放端口，避免残留后台进程；保留原身份和测试环境，测试数据独立目录。
- 本轮不做自动启动安装、自动升级/下载/账号/云服务，不改变防火墙全局策略。不新增车辆 DID 轮询或刷写支持。

## 整合结果

- `cmd/renet` 使用 mygo `WindowOptions.Content`、窗口更新与托盘 API；`--config`、`--state-dir` 可隔离测试数据。默认系统用户目录，与 CLI 不隐式共用身份目录。
- 普通动作最多同时 4 个，Stop 有单独的一个合并请求位；后台执行，主线程只更新视图。按动作请求 ID 接收完成回执，读取控制器最新快照，防止旧通知回滚显示。错误采用控制器产品文案或固定回退文案，不显示异步错误的原始内容。
- 首版关闭窗口有托盘时隐藏；Wave 7 按用户明确要求改为点击关闭或 Quit 都完整退出，取消父 context 并汇合 Runtime、订阅与动作。系统文件操作若阻塞，清理仍等待该操作完成，不增加强杀或隐式丢弃写入。
- Start／Invite 的排队请求记录 Stop epoch，被后续 Stop 越过的旧请求取消，不能在停止完成后重新启动或发布邀请码。成功停止的最终 Notice 与阶段一致。
- 配置增加严格 IPv6 zone 分类与控制字符校验；未指定地址不能靠 zone 伪装为单播端点。保存继续保持原子替换、失败不覆盖原值。
- 新增直接依赖仅 mygo v0.9.1，原因是 Native UI、窗口、托盘及无窗口测试能力；purego、typesetting 和 `x/image`／`x/net`／`x/text` 为其传递依赖。未引入 cgo。

## 审查与验证

Paseo Codex 分别完成 UI、desktop、settings；整合者核对源码与 Windows 行为。Paseo/OpenCode 的 DeepSeek V4.1 Flash 独立只读审查，已复核 IPv6 zone／控制字符、旧错误误解锁、异步回执、Stop 排队撤销、最终状态文案、托盘持续提示与邀请重复准入的修复，最终无仍需修正的确定问题。

Linux 通过 gofmt、diff 检查、全仓 CGO=0 vet／普通测试／构建、CGO=1 全仓 race；最后两项纯视图/提示修复另通过 GUI、desktop、settings、入口聚焦 race。Windows amd64 与 macOS arm64 纯 Go 桌面产物构建通过。Windows VM 的 UI、desktop 和 GUI 入口包原生测试二进制均 PASS；新增 CI 在 Windows/macOS 执行这些无窗口包测试，本节记录本地与 VM 的验收结果，远端任务状态见 GitHub Actions。

Windows 11 VM 使用已有的 1920×1080 虚拟显示器与已解锁 console 会话，测试 GUI 的计划任务使用 Limited token。实际完成 Operator 配置保存、生成/复制邀请、与 Linux Client 配对、peer-only 连接测试；单播、定向与全局广播均返回同一 56B 发现原帧，源地址 `192.0.2.107:6811`；同交互会话的普通权限探针通过模拟 VIN 的 ACK/响应与 6811 控制通道。停止/退出后三个呈现端点及 Hub 均释放；重启读取已保存配置，旧配对可重连；Alt+F4 隐藏时仍监听，再次启动恢复同一窗口和会话。

Client 角色也在 Windows Limited token 实测：选择 `以太网 4 · 192.0.2.107`，从剪贴板粘贴 Linux Operator 邀请，完成发现、配对与 Peer 自动持久化；界面连接测试未触及假车，随后从 Linux 诊断端读取模拟 VIN 成功，假网关观察到车辆 TCP 源为 Windows 所选地址。停止后不再粘贴邀请码，直接 Connect 成功，Operator 观察到第 2 代会话。LAN 自动发现使用测试目录内的独立广播 responder 回放同一发现 fixture（假网关本身 UDP 绑定具体地址，不能据此要求它接收广播），未修改生产发现路径。结束后恢复 Windows GUI 默认 Operator 角色，保留双端测试身份，清除临时邀请码、测试任务和规则。

验证边界：仅模拟网关与只读 fixture，不代表完整车辆识别、FA/SVT/coding 或刷写通过；未执行 macOS 实机运行。Windows 独占继续拒绝无法确认 owner 的来源：SSH 会话下的诊断探针未通过，同一交互桌面的普通用户探针通过；尚未进一步归因权限与登录会话差异，未放宽未知 owner 策略。

清理已完成：Windows 无 GUI 后台进程，Hub/诊断/控制 TCP 与发现 UDP 均不再监听，Wave5 的三个临时计划任务和两个限定来源的 Private 防火墙规则已移除，测试邀请码文件与自有剪贴板内容已清除。保留可再次运行的 Native 构建、已配对测试身份和已有桌面自动化辅助；SSH、虚拟显示器与 EDIABAS 配置没有为本轮功能作永久变更。Windows 可执行文件留在 `C:\Users\<user>\remote-enet-test\wave5\renet.exe`，默认保存角色已恢复为 Operator。
