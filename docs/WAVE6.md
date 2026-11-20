# Wave 6 · 固定双端应用与紧凑界面

用户要求：参考 https://remote-enet.com/ 的公开应用布局，拆分 Client / Operator 两个独立软件。基于主工作区尚未提交的 Wave 5，不重写转发、认证、CBOR 或车辆逻辑。仅参考界面组织，保留本项目一个 Operator 同时一个会话的约束。

## 布局取舍

已读取参考页面及其双端视图：深色底、细边框、紧凑顶部工具栏、横向指标、分组键值信息、集中操作，设置与日志独立于日常操作页。使用已有 VIN、网关/网卡、对端、阶段、RTT、吞吐/字节、活动隧道和独占数据，不凭空补充电压、PAD、车况或安全评分。

- Client：默认 820×640，最小 680×560。固定标题 Client，顶部 Settings / Log；状态与延迟/流量指标；车辆、连接与当前状态分组；邀请码输入与 Connect、Stop relay。网卡/测试偏好放 Settings；事件放 Log。连接后的主操作与重要信息在正常窗口内直接可见。
- Operator：默认 1080×760，最小 900×620。固定标题 Operator，顶部 Settings / Log；只有一个当前 Client/会话；横向指标；Vehicle / Connection / Session 信息组；监听/结束、邀请/复制、连接测试、真实独占状态和测试结果。监听/公开端点/呈现 IP/端口等配置放 Settings。
- 两端保持深色原生视图，不使用 WebView。设置采用明确 Save，未保存表单不会被状态更新覆盖；逐动作 Complete 回执与取消准入规则保留。布局按 100%/150% 缩放可滚动，未知值为 `—`。功能按钮均连接现有后端。

## 并行分工

沿用此前用户授权的 Paseo Codex 并行开发、DeepSeek 独立审查。各工作包不 commit/push，不访问 VM；整合者负责 Windows 验收。

### A · `internal/ui/`

仅编辑 UI 包。以 initial.Config.Role 固定视图角色，移除所有角色切换入口；继续使用 `New(initial, Actions)`、SetConfig/SetState、Complete(Action) 和既有动作签名。可以新增页面私有状态/渲染文件。主视图为 dashboard，Settings/Log 可切换至单独页面；Operator 初始未配置时可直接展示 Settings。角色不可被 Settings 或后台配置更新改写，收到角色不匹配的 Config 不应跨角色显示。

保留动作可访问名称：Connect client、Start operator、Stop runtime、Generate invitation、Copy invitation、Test connection、Save settings、Refresh adapters、Quit Remote ENET。Settings 与 Log / Back to session 的访问名称明确且唯一。运行中禁止改网络设置，动作回执不按任意 Error 解锁，生成中清除旧邀请码不解锁。

更新 headless 测试为真实打开 Settings、输入/粘贴/保存、返回主页面、连接/停止/测试、打开日志；角色分离（Client 没有 Operator 控件，Operator 没有 Client 控件）、正常窗口主操作可见、缩放与长文本、新旧回执/Peer/资源限制回归。导出临时 preview PNG 供整合者审查。

### B · `internal/settings/` 与 `internal/desktop/`

保持现有 Config v1 JSON schema 和通用 `DefaultPaths()` 行为。新增：

```go
func settings.DefaultPathsForRole(role string) (configPath, stateDir string, err error)
// config: UserConfigDir()/remote-enet/{client|operator}/settings.json
// stateDir: UserConfigDir()/remote-enet/state，Controller 内再按角色分目录

// desktop.Options 新增：
FixedRole string // empty retains generic library behavior; otherwise client/operator only
```

FixedRole 非空时：缺省路径选角色专用配置；不存在的配置用该角色默认值；存在配置的 Role 不匹配则报错，不能静默改文件或换角色。Save 不能改固定角色；错误角色的启动/邀请拒绝。显式 --config/--state-dir 支持隔离与旧配置复用，但既有 JSON 需角色匹配。不要迁移/删除旧 shared settings.json 或身份，不改变 trust.json、消息 payload、schema 版本。身份仍由 stateDir/client、stateDir/operator 分开，两个程序可并行存在。

补默认角色、独立路径、缺失/损坏/错角色配置、角色 Save/启动拒绝、双控制器隔离测试；既有泛用 Controller 回归保持。只做必要修改，完成聚焦普通/race 后交付固定源码，不重复扩展测试范围。

### 整合者 · 入口、共享宿主、CI 与验收

把现有 `cmd/renet` 的宿主与动作调度迁入 `internal/desktopui`，新增 `cmd/renet-client`、`cmd/renet-operator` 两个薄入口，移除旧统一入口。各自 App name / Window title / 托盘 / mygo identifier / 默认 config 区分；role 来自编译入口，不接受 --role。核心库共用。

Windows/macOS 分别构建 Client 与 Operator，CI 更新入口与共享包；Windows 两个窗口可同时启动且角色不切换、配置互不覆盖、退出仍汇合并释放端口。保留 Wave 5 的动作配额/Stop 保留槽、原始错误屏蔽、系统提示、最新快照与逐动作迟到回执隔离。

## 验收边界

普通/race、vet、gofmt、纯 Go 跨平台构建，DeepSeek 只读复审；Windows 使用假网关验证新入口与核心流程。macOS 仍需原生实机验收，实车 FA/SVT/coding 继续独立验收。本轮不引入新的车辆命令、协议或云服务。

## 交付与验证记录

- 已移除统一 `cmd/renet` 入口，新增 `cmd/renet-client`、`cmd/renet-operator` 和两个独立 mygo identifier；宿主与动作测试迁入 `internal/desktopui`。Client 不注入 Operator 操作回调，Operator 不注入 Client.Connect。
- 默认配置分为 `remote-enet/client/settings.json`、`remote-enet/operator/settings.json`，共享 state 根再由角色分目录。现有错角色/损坏配置拒绝加载；未知角色不能解析为路径，Save 不得改固定角色。旧 shared 配置、身份与 JSON v1 未迁移或删除。
- 原生主界面改为深色紧凑工具布局，顶部状态和横向指标，Vehicle / Connection / Session 信息组，主要操作位于默认窗口内。Settings / Log 独立页面，主导航始终可达；没有角色切换或未接入后端的业务按钮。源码和预览直接对应实际 mygo 视图。
- Paseo Codex A/B 完成视图与角色边界；整合者合入双入口、CI、文档和平台验证。Paseo/OpenCode DeepSeek 独立只读复审，未发现确定缺陷。

### 自动检查

主仓 gofmt、diff、`go mod tidy -diff` 无异常；CGO=0 全仓 vet、普通测试和 `mygo_nowebview` 构建通过；CGO=1 全仓 race 通过。Windows amd64 / macOS arm64 的 Client、Operator 四个纯 Go 产物均编译成功；Windows 使用 `-H=windowsgui`。Windows VM 原生执行 UI、desktop、desktopui 三个测试二进制，均 PASS。CI 同步构建两个软件，并执行共享宿主测试；本节记录本地与 VM 的验收结果，远端任务状态见 GitHub Actions。

### Windows 双程序实测（2026-11-16）

环境沿用已有 Windows 11、1920×1080 虚拟显示器与 ghSky console 会话；两个程序均以 Limited token 运行。使用 `remote-enet-test/wave6` 下各自配置文件和按角色隔离的测试身份，不改默认用户配置或 Wave5 记录。

1. 同时出现 `Remote ENET Client`、`Remote ENET Operator` 两个独立窗口，分别为 820×640、1080×760，均无角色选择控件。再次启动 Client 只激活原窗口，进程仍各一个，Operator 不受影响。
2. Operator 开始监听、生成/复制邀请；Client 在自己的窗口粘贴并连接。双方进入 Connected，显示模拟 VIN。Client 保存了自己的 Peer，Operator 配置中的 Peer 保持原值，身份目录各自建立。
3. 界面连接测试完成；Client 打开 Log、返回主页面；Operator 运行中的 Settings 输入框均禁用且可返回主页面。Log 不包含邀请码。
4. 普通权限的交互探针经 Operator 呈现地址 `192.0.2.107` 得到 56B 原始发现帧，诊断 ACK 11B、VIN 响应 28B、6811 控制应答 7B；假网关观测到车侧 TCP 源为所选 Windows 网卡地址。只使用显式只读 fixture。
5. Client 停止后以空邀请码再次 Connect，使用已记住的对端成功重连。退出 Client 后 Operator 仍监听；再结束并退出 Operator，窗口与监听回收。

本轮两端和原始假车同处一个测试 LAN，广播探针会同时收到原始假车与 Operator 的回包。台架脚本因此定向探测 Operator 的单播地址，避免把不同来源的测试回包混为一处；生产发现逻辑未修改。真实实车、WAN 弱网和 macOS 原生窗口/托盘仍待独立验收。

测试结束已停止两个 GUI 与 Linux 假网关/广播 fixture，移除 Wave6 的四个临时计划任务和两个限定来源的 Private 防火墙规则，并清除测试剪贴板邀请码。SSH 和既有桌面自动化仍可用；测试构建保留在 Windows `C:\Users\<user>\remote-enet-test\wave6\renet-client.exe` 与 `renet-operator.exe`。最终变更由整合提交统一记录。
