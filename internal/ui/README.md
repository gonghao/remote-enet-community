# 固定双端原生界面

仅构建 mygo 原生控件，不读取磁盘、不枚举系统网卡、不拨号或创建 app.Runtime。产品文案支持English、简中和繁中，由宿主调用`SetLanguage`；缺失观测值显示 `—`。布局参考 [Remote ENET 公开应用页面](https://remote-enet.com/) 的紧凑工具栏、横向指标、三组信息和集中操作，使用原创 Go 控件组合、配色与间距，没有复制 CSS 或品牌装饰。没有 WebView、车辆健康评级、电压、PAD、聊天、编码或刷写按钮。

## 双端与页面

`New(initial, actions)` 将 `initial.Role` 固定为视图的应用角色，整个生命周期内不提供角色选择入口。`SetConfig` 收到角色不匹配的配置时整份忽略，不替换表单、保存基线或待确认提交。

- Client：宿主窗口固定为 440×700 DIP，不可调整大小或最大化；工作区不足时收紧，不低于 420×480 DIP。首页将网卡选择和刷新放在同一行，邀请码和连接按钮各占一行。下方是两列指标，以及一张含 Vehicle / Connection / Session 三节的详情卡片。记住的 Peer、测试偏好与连接测试放在 Settings。
- Operator：宿主初始窗口 1080×760，最小 900×620。主页面呈现一个当前 Client，包含同组指标与车辆/链路信息、实际呈现端口与独占状态、Start listening / End session、邀请生成/复制、连接测试与结果。监听、公开端点、呈现 IP/端口和独占偏好放在 Settings。初始配置缺少公开端点或呈现 IP 时直接打开 Settings。
- Settings 与 Log 是独立页面。两端主窗口均无系统框，固定页眉可拖动窗口，`Settings`、`Log` 与 `Back to session` 的访问名称唯一；可从已滚到底部的日志直接返回会话。首次打开时，两端的初始尺寸按主屏工作区收紧，保留最小尺寸。Client 在 440×700 DIP、100% 缩放下可完整显示空闲和已连接状态的首页及一条网卡刷新提示，headless 回归测试要求内容底部留出至少 24 DIP。小窗口或放大界面时可纵向滚动，Client 的逻辑宽度低于 360 DIP 时指标排列为一列。
- Log 按输入 Events 数组由旧到新的约定倒序呈现，最多 200 条；不放在日常操作页面。

## 接线与线程

`New`、`Render`、`SetConfig`、`SetState`、`Complete` 均由 UI 线程调用。所有 `Actions` 在 UI 线程同步触发，必须快速返回。异步 controller 调用、剪贴板和退出生命周期由整合器负责；缺失 action 对应按钮禁用。宿主使用 mygo `ui.View(view.Render)`。

`State`、`Actions` 和 `Action` 保持 Wave 5 回执修复后的接口：

```go
func New(initial settings.Config, actions Actions) *View
func (*View) SetConfig(settings.Config)
func (*View) SetState(State)
func (*View) Render(*ui.Context)
func (*View) Complete(Action)
```

每次收到 controller 状态后，切回 UI 线程依次调用：

```go
view.SetConfig(state.Config)
view.SetState(ui.State{
    Runtime: state.Runtime,
    Running: state.Running, Stopping: state.Stopping, Testing: state.Testing,
    Invitation: state.Invitation, Notice: state.Notice, Error: state.Error,
    NICs: state.NICs, TestResult: state.TestResult, Events: state.Events,
})
// Invalidate the host content after applying both updates.
```

每个异步方法返回后，无论成功或失败，整合器在 UI 线程读取 controller 的最新 Snapshot，先执行 `SetConfig → SetState`，再 `Complete(对应 Action)` 并刷新界面。整合器按动作请求 ID 排除迟到旧完成，防止它解除新请求的等待。

| Action | 回调 |
| --- | --- |
| `ActionSave` | `Save(settings.Config)` |
| `ActionRefresh` | `Refresh()` |
| `ActionConnect` | `Connect(string)` |
| `ActionListen` | `Listen()` |
| `ActionStop` | `Stop()` |
| `ActionInvite` | `IssueInvite()` |
| `ActionTest` | `TestLink()` |

`Action` 底层为 `uint8`。Copy 与 Quit 保持原有回调，不需要上述完成回执。

## 表单、回执与凭据

- 编辑值与保存基线分离；导航、`SetState` 与普通状态更新不覆盖未保存编辑。只有合法且与保存基线语义不同的值才能 Save；`06801` 与 `6801` 不触发重复保存。
- Save 记住本次提交，等待匹配的 `SetConfig`；旧配置不能确认新保存。保存确认时保留提交期间的新编辑。保存失败经 `State.Error` 呈现，再由 `Complete(ActionSave)` 解除等待。首页网卡保存失败时，仅将草稿中的网卡名称和 IPv4 恢复为已保存值，可以重新选择同一网卡重试。Settings 保存失败时保留表单编辑。未保存或保存等待期间禁止启动与生成邀请。
- Peer 不可编辑；同角色的 `SetConfig` 将最新 Peer 合入草稿和待确认提交，防止保存其他设置时清掉首次配对后记住的 Peer。
- 点击后立即锁住对应请求。旧 Error、无关错误和其他动作的 Complete 不会解除等待。Connect / Listen、Refresh、Stop、Test、Invite 分别持有等待标记；Running/Stopping/Testing、匹配 Config 和新非空 Invitation 可按成功状态解除等待。生成中仅清空旧邀请码不会解除生成等待。
- 运行、开始和停止期间禁止网络设置及偏好编辑；Settings 可查看，连接测试仍按实际连接和保存偏好准入。停止后修改配置，明确 Save 后在下一次启动生效。
- Client 会话首页的网卡下拉显示名称和 IPv4，选择后立即提交这两个字段；Automatic 清空两个字段。提交以已保存配置为基线，Settings 中其他未保存的编辑保留在草稿里。运行中或有保存在途时禁用网卡下拉。刷新或固定网卡消失时保留原选择，只固定名称或 IP 的配置也会保留。歧义交由 app 判断。
- Client 支持系统粘贴/手动输入；controller 接受启动后清空临时邀请码。Operator 输出为只读单行编辑器，长码水平滚动、不撑宽布局；仅显式 Copy 才触发复制。邀请码不进入 Config、Events 或指标。传入的 Events、Notice、Error 应由 controller 保证不含凭据。

保留动作访问名称：`Connect client`、`Start operator`、`Stop runtime`、`Generate invitation`、`Copy invitation`、`Test connection`、`Save settings`、`Refresh adapters`、`Quit Remote ENET`。输入框通过原生 Field 绑定标题，错误和阶段使用无障碍实时区域。Windows UIA 实机验收由整合器负责。

## 聚焦验证与临时预览

```sh
CGO_ENABLED=0 go vet ./internal/ui
CGO_ENABLED=0 go test ./internal/ui
CGO_ENABLED=1 go test -race ./internal/ui
```

headless 测试真实导航、点击、粘贴、输入和首页网卡选择，覆盖网卡即时保存、回执确认后恢复连接按钮、保留其他草稿编辑及保存在途时禁用下拉。其他回归覆盖保存失败重试、旧回执、提交期间编辑、Peer 合并、无关完成隔离、清空旧邀请码、运行中禁用、未知与有效零指标、显式复制、固定角色、独立 Settings/Log、200 条日志边界，以及最小窗口、150% 缩放、长邀请码和高像素密度。

可导出临时 PNG，均使用无真实凭据的测试夹具，不能提交截图到源码：

```sh
RENET_UI_PREVIEW_DIR=/tmp/wave6-ui-preview CGO_ENABLED=0 go test ./internal/ui -run TestWorkspacePreview -v
```

输出 `client-100.png` / `client-150.png`（440×700）、`client-min-100.png` / `client-min-150.png`（420×480）、`operator-100.png` / `operator-150.png`（1080×760），另有各端 `-settings.png`、`-log.png` 和 Client 简繁中文首页预览。

## 浅色主题

默认采用冷中性浅色：浅灰工作区、白面板、深灰正文与蓝色强调，状态使用深绿/棕黄/暗红及对应的浅提示底。语义色集中于 `theme.go`；主界面、Settings、Log、启动错误和原生窗口外观保持一致。控件轮廓与装饰分隔线分开，焦点环使用实色。研究、候选比较与颜色对计算见 [浅色方案](../../docs/research/ui-light-theme.md)。

## MGU direct link

Operator主页面新增MGU direct link，显示阶段与真实主机IP。`Actions.ConnectMGU/DisconnectMGU`和追加的`ActionConnectMGU/ActionDisconnectMGU`分别完成；旧Error或其它Action的Complete不解除其pending。Disable在启用中可用于取消；Client不展示这些动作。宽窗口与连接测试并排，连接测试使用两列指标以保持数值可读，小窗口保持滚动。

## 权限提示与诊断复制（Wave 10）

`State.Permission`只携带宿主观测的能力标志，视图不检查文件、不请求授权。MGU状态和地址并排，操作按钮在权限提示之前；1080×720含常见通知时主操作仍可见。`Actions.CopyDiagnostics`为同步快速通知，仅Log页显式点击触发，由宿主构建和复制报告；不把邀请输入交给该动作。
