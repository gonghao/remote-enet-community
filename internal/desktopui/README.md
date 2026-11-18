# 双端原生宿主

`cmd/renet-client` 与 `cmd/renet-operator` 分别传入编译入口固定的角色，调用 `Run(role, version, args)`。两者复用窗口、托盘、异步动作、controller 和关闭生命周期，各自具有不同的 App name、单实例键、窗口/托盘标题以及 mygo identifier；无运行时角色切换参数。

默认配置按角色位于系统配置根目录的 `remote-enet/client/settings.json`、`remote-enet/operator/settings.json`。身份与信任由 `remote-enet/state/client`、`operator` 分隔。显式 `--config` 可引用角色匹配的已有配置；`--state-dir` 指定包含角色子目录的根。不迁移或删除 Wave5 的 shared settings.json。`--help` 和 `--version` 不依赖用户配置目录，且不启动窗口、listener 或身份存储。

语言使用mygo的Locale/SetLocale及系统匹配。Settings语言选择即时更新当前视图、标题和托盘，不改变固定应用名、单实例标识或Controller；保存语言由框架负责，Windows/Linux的UI偏好目录为`<config-path>.ui`，macOS为应用语言偏好。切换语言保留表单、筛选与动作状态；详情见`docs/WAVE13.md`。

Client 仅接入 Save/Refresh/Connect/Stop/Test/Copy/Quit 回调，Operator 接入 Save/Refresh/Listen/IssueInvite/Stop/Test/Copy/Quit。GUI 只做 Go Native UI，生产构建传 `mygo_nowebview`，Windows 另用 `-H=windowsgui`。

动作在后台执行；普通请求上限 4，Stop 有独立的一个合并槽位。所有视图变更经过 UI 线程；每次从 controller 读取最新快照，逐动作请求 ID 过滤迟到完成，按 SetConfig→SetState→Complete 调用。临时动作错误不透出原始错误或邀请码；托盘不可用提示保留独立生命周期。关闭窗口与 Quit 共用退出路径，先取消并汇合 controller 与宿主 worker，再关闭原生窗口和事件循环；清理未完成时重复关闭不会绕过汇合。

Wave 15两端新增连接自检，入口位于设置和日志页。Run使用已保存设置和内存中的邀请码；Cancel有独立合并槽位，并先取消宿主尚未进入Controller的请求，普通四槽占满也能取消。订阅仅传固定字段结果，复制采用独立`checkup/v1`白名单，既有匿名诊断不扩字段。NetworkStatus仅调用netpriv.Inspect，不调用Open或系统授权；详情见`docs/CHECKUP.md`。

已运行应用的激活覆盖两种系统入口：Windows/Linux 再次启动进程由 OnSecondInstance 唤回原窗口；macOS Dock/Finder 的重新打开不会启动第二个进程，由 OnActivate 在无可见窗口时唤回同一个窗口。两种情况均不重建 controller 或重启 relay；退出过程中忽略唤回。用户在 Wave 7 确认改为关闭即退出，窗口不再通过关闭按钮隐藏到后台。

Wave 9新增仅Operator接线的MGU direct动作；Disable有独立合并槽位，和全局Stop分别准入，不被四个普通请求占满而阻塞。完成仍按Action请求ID核对、读取最新Snapshot后SetConfig→SetState→Complete；DirectWorking与Runtime.Direct独立展示，不写入用户配置。

## 权限和诊断（Wave 10）

Helper路径固定取当前可执行文件同目录的`renet-net-helper`，不从设置或工作目录选择。启动时只读取权限能力；仅Operator注入DirectFactory。Mac普通用户通过系统授权运行受限助手，Windows权限不足使用固定产品提示，Client不请求授权。

Log页Copy diagnostics在显式点击后从最新Controller快照构建白名单报告，再交给剪贴板；失败不复制部分内容。复制回执独立保存，不会被下一次指标订阅立即抹掉，接纳新动作时清除。Permission是实际启动能力的值拷贝；报告不包含配置、邀请、地址或原始错误。跨平台原生验收与边界见`docs/WAVE10.md`。

## HSFZ 抓取（Wave 12）

仅 Operator 接入 StartCapture/StopCapture，Log 页进入独立帧页面。原生保存对话框在 UI 线程运行，选择结果交给后台 controller；取消或 Stop 越过的迟到选择不创建文件。等待者可随宿主 ctx 取消，缓冲结果不要求 worker 等用户关闭迟到的原生对话框。

StopCapture 使用独立合并槽位，不占四个普通动作或全局 Stop/Direct Disable 的槽位。全局 Stop、StopCapture 会先取消尚在选择/创建文件的 Start 请求；完成时使用最新快照及匹配 Action ID。载荷只出现在专用 Capture 状态和显式前缀复制中，匿名诊断保持原白名单。

录制期间注册 `CmdOrCtrl+Shift+M` 全局标记快捷键，停止、故障或 Quit 时只注销自身的快捷键。注册失败保留按钮并提示，录制期间不反复抢占重试；迟到的旧录制回调按 StartedAt 拒绝。标记同步准入 Recorder 的有界队列，避免后台任务调度改变按键时间，不执行文件 I/O 或激活窗口。Wayland 可由桌面绑定不同按键，宿主按实际回调更新提示。
