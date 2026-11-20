# R3 自建 Hub · 家庭 LAN 台架人工验收（A 段）

2026-11-20。对应 [SELFHOST 第 10 节](../SELFHOST.md#10-测试计划)「人工验收」一行中可以在家庭局域网内完成的部分：真实进程、Windows 原生 GUI Operator、独立 `renet-hub`、Linux CLI Client 与假网关。Hub 位于家庭网络以外、两端都在 NAT 之后的 B 段不在本轮范围内，另行执行。

## 版本与构建

| 项目 | 内容 |
|---|---|
| 提交 | `8a865dc`（分支 `continue-followup-development`），工作区干净 |
| 工具链 | Go 1.27.3，mygo v0.9.1，全部 `CGO_ENABLED=0`、`-trimpath` |
| Linux 二进制 | `renet-hub`、`renetd`、`fakezgw`（`go build ./cmd/...`） |
| Windows GUI | `renet-operator.exe`，按 `tools/build-desktop.py` 的参数构建：`-tags mygo_nowebview`、`-H=windowsgui`、版本 `0.1.0-preview.8a865dc`。唯一差异是 `mygo.packageIdentifier` 改为测试专用值，使单实例锁与正式 Operator 分开，避免测试实例把参数交给自用实例 |

各二进制的 SHA256 记录在私有证据中。

## 拓扑

```
Linux CLI Client ──▶ renet-hub ◀── Windows GUI Operator（Hub 模式）
  │  (devbox，loopback 假网关)   (devbox)     (Windows 11 VM，隔离配置与状态目录)
  └──── 内层 TLS + session，经 Hub 转发 ────────────────┘
```

- Hub：devbox 上 `renet-hub init` 生成临时身份，监听 devbox 的局域网地址和一个非 47000 的测试端口，配置文件按第 4.1 节手写，`--debug` 打开拒绝原因日志。
- Operator：Windows VM 上的 GUI，用 `--config` 与 `--state-dir` 指向本轮专用目录；呈现地址为 `127.0.0.1`、呈现端口为非标准测试端口，不触碰用户自用 Operator 的端口与配置。开始前确认自用 Operator 未在运行、默认配置目录不存在，结束后核对仍是如此。
- Client：devbox 上 `renetd client`，车辆侧绑定 `lo`，假网关 `fakezgw` 绑定 `127.0.0.2`（车端发现只接受同网段网关）。邀请码与「地址 + 码」都经文件传入（`--invite-file`、`--pair-file`），不进入 shell 历史。
- GUI 操作全部通过桌面自动化完成（截图、点击、剪贴板读写），没有人工代看代点。

## 逐项结果

| # | 项目 | 结果 | 证据要点 |
|---|---|---|---|
| 1 | 设置切到「自建 Hub」、复制授权条目、Hub 授权后进入等待 | 通过 | 授权前状态栏为「Hub 未授权本机，请检查授权条目与系统时间」并显示授权条目；Hub 调试日志为 `A3: unknown issuer`。剪贴板内容是一行 JSON，键为 `name`、`operator`、`public_key`。加入配置后 `renet-hub check` 退出 0，SIGHUP 重载成功；下一次 30 秒重试后状态变为「已在 Hub 等待」 |
| 2 | GUI 邀请码首次配对 | 通过（见说明） | 邀请码为版本 2，1 个 `hub` 端点，携带不记名票（`bearer=true`，`exp − iat` = 15 分钟，198 字节），`ticket inspect` 离线检查通过。Client 配对后连接，`path=relay`，1 MiB 连接测试成功。Client 的 `trust.json` 为版本 2，端点带 `ticket` 字段；票据为设备 dial 票（`bearer=false`、`dev` = Client 指纹、30 天）。Operator GUI 的连接卡片显示「连接方式：经 Hub <地址> · <Hub 短 ID>」。说明：Client 用的是 CLI，日志只给出 `path=relay`，Client GUI 上的「经 Hub」显示本轮未覆盖 |
| 3 | 无码重连与续期 | 通过 | 停止 Client 后只带 `--peer` 重启，凭设备票直接连上；票据被替换：`jti` 改变，`exp` 后移约 84 秒（与两次连接的时间差一致） |
| 4 | 「Hub 地址 + 8 位码」 | 通过 | Hub 配置 `pairing_operator` 指向测试 Operator 并重载；GUI 生成码，显示 5 分钟与 5 次。错码一次：Client 以 `pairing rejected: confirmation` 结束、不保存配对，GUI 剩余次数 5 → 4。正确码：新状态目录的 Client 配对成功，`trust.json` 版本 2、带设备票。去掉 `pairing_operator` 重载后再生成码：Hub 日志 `A9: issuer scope`，GUI 显示「Hub 未为本机开放数字配对」并撤码（复制、撤销按钮变灰），Operator 仍为「已在 Hub 等待」 |
| 5 | 按设备撤销 | 通过（见发现 3） | 会话进行中把 Client 指纹加入 `deny.devices` 并 SIGHUP：Hub 日志 `closed_pairs=1`，会话立即 EOF；Client 重试一次得到 `unauthorized`（Hub 日志 `A11: revoked`），以 `hub_authorization_expired_or_revoked` 停止，不再重试。Operator GUI 从「已连接」回到「已在 Hub 等待」 |
| 6 | Hub 重启 | 通过 | Client 会话中停止并重启 `renet-hub`：Client 先得到 EOF，Hub 不可达期间按退避重试（`hub_unreachable`），Hub 起来后约 1 秒以第 2 代会话重连成功；Operator 自动重新注册，GUI 回到「已连接」 |
| 7 | 中心显示与原生确认框 | 通过（文案小问题见发现 5） | 设置里填中心 ID 与 `https://127.0.0.1:9/`，点「更换中心…」弹出 Windows 原生对话框：标题为产品名，正文「确认更换中心？」，下方列出 ID 与 URL，按钮「取消」「更换中心…」。确认后主界面连接卡片「中心」显示该 ID；「检查中心」结果为「中心不可达」并列出入口。清空后同样经确认框，状态恢复为「未配置」，配置文件中 `center` 为空 |
| 8 | 三语与 Pixel Workbench | 通过（截断见发现 2） | 简体、繁體、English 三种语言下检查了设置页 Hub 区域、「已在 Hub 等待」、「Hub 未授权本机…」及授权条目、「Hub 未为本机开放数字配对」、连接卡片「经 Hub」。界面保持浅色、直角、无渐变与模糊；授权条目按设计中段省略，提供复制按钮。English 下连接卡片的 Hub 地址被中段截断 |
| 9 | 直连回归 | 通过 | 切回直连，监听测试端口（仅为该端口加了专用入站规则），GUI 生成的邀请码为版本 1、端点不带票据；新 Client 配对连接成功，`path=direct`，1 MiB 连接测试成功，`trust.json` 保持版本 1，GUI 显示「连接方式：直连」 |

附加核对：Hub 模式运行期间 Windows 上测试端口没有监听，只有一条到 Hub 的出站连接；直连模式下只监听所配的测试端口。

## 发现

### 1. 从直连切到 Hub 模式时，分享地址沿用直连的公开地址（产品缺陷，中）

复现：

1. 直连模式下设置「公开地址」为 Operator 自己的地址并保存。
2. 设置页把连接方式切到「自建 Hub」，填写 Hub 地址与指纹并保存，开始监听。界面「分享地址」一栏已经预填了第 1 步的直连地址。
3. 生成邀请码。解码可见端点为 `kind=hub`、Hub 指纹正确，但 `addr` 是 Operator 的旧直连地址。
4. Client 用该邀请码配对：一直报 `hub_unreachable`（拨向 Operator 已不再监听的直连端口），无法完成首次配对。

根因：`settings.Operator.Endpoint` 同时承担直连「公开地址」与 Hub 模式「分享地址」两种语义。`internal/ui/forms.go` 的 `operatorForm` 切到直连时清空 `Hub`，切到 Hub 时却保留 `Endpoint`；`internal/desktop/runtime.go` 的 `runtimeConfigs` 在 Hub 模式下只有 `Endpoint` 为空时才回退到 `hub.addr`。于是旧的直连地址被静默写进邀请码和数字码的地址里。

影响：配置看起来正确（Operator 已在 Hub 等待），但发出的所有邀请码都无法使用，排查方向容易被误导到网络。建议：切换模式时清空或分别保存两种地址，至少在 Hub 模式下分享地址与 Operator 自己的监听或公开地址相同时给出提示。本轮未改代码。

反方向也有一个较小的问题：切到直连时，Hub 地址与指纹在保存前就从草稿中清空，再切回 Hub 需要重新填写。

### 2. English 下连接卡片的 Hub 地址被截断（界面缺陷，低）

窗口为缺省宽度 1080 时，English 的「Connection mode」一行显示为 `Via hub 192.0.2.1…391 · <短 ID>` 的形式，地址被中段省略；简体与繁體在本轮的 IP 地址下正好放得下。实际部署多用 DDNS 名，地址更长，三种语言都会被截断，而这一行是用户核对「连的是哪个 Hub」的主要位置。建议换行显示或提供悬停全文。

### 3. 移出 `deny.devices` 后旧设备票立即恢复可用（文档与实现不一致）

撤销后把 Client 指纹从 `deny.devices` 移除并重载，Client 不需要重新配对：用保存的设备票直接连上。原因是设备票本身仍在有效期内，且不在 `deny.jtis` 中，Client 被拒后也不会删除本地票据。SELFHOST 3.6 写的是「要恢复访问，需要 Operator 发新码重新配对，同时把设备从 `deny.devices` 移除」，读起来像移出拒绝列表还不够。实现行为是合理的，建议文档改为：移出 `deny.devices` 即撤销撤销；如果希望该设备必须重新配对，需要同时把它的设备票 `jti` 加入 `deny.jtis`（或轮换签发密钥）。

### 4. Hub 未授权时状态标题仍为「正在监听」（界面文案，低）

Hub 模式下 wait 被拒时，状态标题显示「正在监听 / Listening / 接聽中」，下方横幅才是「Hub 未授权本机…」。Hub 模式下 Operator 并不监听任何端口，标题容易误导。建议在 Hub 模式下用「正在连接 Hub」之类的表述。

### 5. 中心确认框的小问题（文案，低）

确认按钮沿用了带省略号的「更换中心…」，按惯例省略号表示还会弹出后续对话框；对话框标题使用英文产品名「Remote ENET Operator」，而窗口标题是本地化的「诊断端」。正文把 ID 与 URL 排在一行，没有标签。都不影响功能。

### 操作记录（非产品问题）

- 首次用模拟按键输入 Hub 地址与指纹时，冒号变成全角、指纹里的数字丢失。原因是 VM 上的中文输入法在模拟 Shift 时切换了中英文模式。改为写剪贴板后粘贴，之后所有输入都正确，配置文件内容与预期一致。
- Operator 第一次在 `127.0.0.1` 上开始呈现时，Windows 防火墙弹出放行询问；本轮按取消处理，系统因此生成了两条针对测试程序的阻止规则。这两条规则在直连回归前删除，收尾时再次核对已不存在。

## 清理

- devbox：Hub、假网关、所有 Client 进程已停止，测试端口无监听；临时目录（Hub 身份、Client 状态、邀请码与票据文件）已删除，只保留不含秘密的日志、配置与截图作为私有证据。
- Windows：测试 GUI 经界面「退出」正常结束（退出码 0）；本轮的三个计划任务、一条入站规则与防火墙自动生成的两条规则都已删除；本轮专用目录（含测试身份、签发密钥与信任文件）已删除；剪贴板已清空。
- 自用 Operator：开始前与结束后都没有在运行，默认配置目录都不存在，47000 端口都没有监听；VM 上原有的其他测试进程与 mygo 单实例套接字未变化。

## 未覆盖

- B 段：Hub 位于家庭网络以外、两端都在 NAT 之后的公网验收。
- Client GUI 上的「经 Hub」显示与授权失效提示（本轮 Client 为 CLI）。
- 真实车辆与真实诊断工具（本轮只用假网关与连接测试）。

## 复测（台架修复后）

同日下午，在同一套台架上对本记录发现 1、2、4、5 的修复做 GUI 复测。被测修复：Hub 分享地址与直连公开地址分开保存（`2225b57`）、连接卡片 Hub 地址单列并可复制（`0775830`）、Hub 状态标题区分注册中、等待与未授权（`a64a146`）、中心确认框标题本地化与确认按钮去掉省略号（`0d3e867`）、自检在 Hub 模式解析分享地址（`bb7f226`）。

### 版本与方法

| 项目 | 内容 |
|---|---|
| 提交 | `bb7f226`（分支 `continue-followup-development`），工作区干净 |
| 构建 | 与上文相同：Linux 的 `renet-hub`、`renetd`、`fakezgw`；Windows GUI 按 `tools/build-desktop.py` 的参数构建，版本 `0.1.0-preview.bb7f226`，`mygo.packageIdentifier` 换成本轮专用值 |
| 隔离 | 新的临时 Hub 身份与非 47000 测试端口；GUI 使用本轮专用的 `--config` 与 `--state-dir`；测试对象（目录、计划任务）都带本轮日期名称 |
| 操作 | 全部经桌面自动化完成：截图、点击、悬停、剪贴板读写；配置文件与邀请码内容经 SSH 读取后只解码非秘密字段 |

长地址测试在 Windows hosts 中临时加入一个 53 字符的 DDNS 风格测试名（保留的 `.test` 域）指向 Hub 所在主机，结束后用测试前的副本恢复 hosts，哈希与测试前一致。

### 逐项结果

| # | 项目 | 结果 | 证据要点 |
|---|---|---|---|
| 1 | 模式切换不再串用地址 | 通过 | 直连模式填写公开地址并保存后切到「自建 Hub」：「分享地址」为空（只显示占位符）。填 Hub 地址与指纹保存，配置中 `operator.endpoint` 仍为直连地址、`operator.hub.share` 为空。授权后生成邀请码：版本 2，1 个 `hub` 端点，`addr` 为 Hub 地址，携带 198 字节不记名票。CLI Client 用该邀请码配对成功，`path=relay`，1 MiB 连接测试通过，`trust.json` 版本 2、端点带设备票。随后把 Hub 地址改为长测试名、分享地址显式填 Hub 的 IP 地址并保存，切回直连：公开地址仍为原值；在直连模式下保存，配置里 `operator.hub` 三个字段原样保留；再切回 Hub：Hub 地址、指纹、分享地址都还在。此时生成的邀请码 `addr` 为显式分享地址（不是 Hub 地址）；第二个 Client 用它配对、连接与连接测试均成功 |
| 2 | 连接卡片长 Hub 地址 | 通过 | Operator 经长测试名连上 Hub、Client 已连接时，「连接方式」一行只显示「经 Hub <Hub 短 ID>」，「Hub 地址」单独一行。缺省宽度 1080 与最小宽度 900、简体 / 繁體 / English 六种组合都截图核对：地址中段省略，保留主机名尾部与端口（最窄的 English 900 下保留 `…ab.test:<端口>`）；悬停显示完整地址；「复制 Hub 地址」在简体 1080 与 English 900 下各点一次，读回剪贴板与完整地址逐字节一致 |
| 3 | Hub 状态标题 | 通过（English 换行见发现 R2） | 三种语言各看一遍：Hub 未加本机签发者时标题为「Hub 未授权本机 / Hub has not authorized this device / Hub 未授權本機」（琥珀色），Hub 调试日志 `A3: unknown issuer`；停止 Hub 后下一次重试变为「正在连接 Hub / Connecting to hub / 正在連線至 Hub」，运行信息「无法连接 Hub」；加入签发者并 SIGHUP 重载后下一次重试变为「已在 Hub 等待 / Waiting on hub / 已在 Hub 等待」。Hub 模式下不再出现「正在监听」 |
| 4 | 中心确认框 | 通过 | 填写中心 ID 与 `https://127.0.0.1:9/` 后点「更换中心…」，三种语言各弹一次原生对话框，用 UI Automation 读取：标题分别为窗口的本地化标题（简体「…诊断端」、繁體「…診斷端」、English「Remote ENET Operator」），与主窗口原生标题一致；按钮为「取消 / 更换中心」「Cancel / Change center」「取消 / 更換中心」，确认按钮不带省略号。繁體下确认后配置 `center` 写入；清空两栏后再经确认框（正文显示「未設定」）确认，配置 `center` 恢复为空，主界面「中心」显示未设定 |
| 5 | 自检解析分享地址 | 通过 | 把直连公开地址改为一个在 VM 上必然解析失败的名字（`.home.arpa` 下的测试名）作为对照：直连模式自检 `public_endpoint` 为 `failed / dns_failed`。切到 Hub 模式（直连地址不变）：分享地址为 IP 时 `public_endpoint` 为 `warning / remote_check_required`（解析通过，提示需外部验证）；分享地址改为同样不可解析的名字时为 `failed / dns_failed`；分享地址清空时回退到 Hub 地址（经 hosts 可解析）为 `warning / remote_check_required`。三次 Hub 模式自检的 `hub_tls` 均为 `pass / hub_verified`。结果来自「复制自检结果」导出的报告 |

附加观察：复测中途 Operator GUI 退出后重新启动并开始，已配对的第二个 Client 不需要新码，凭设备票经分享地址自动重连成功。

### 发现

#### R1. 修复前保存的版本 2 配置无法加载（兼容性，低）

复现：用缺少 `operator.hub.share` 键的版本 2 配置（`2225b57` 之前的程序保存的格式，例如上一轮台架的配置）启动 GUI，进入「启动失败」窗口：`settings: decode config: config.operator.hub.share: missing required field`，提示现有设置已保留、修正文件后重启。

根因：`2225b57` 给 `operator.hub` 新增 `share` 字段，但配置版本仍为 2；`internal/settings/store.go` 的 `decodeConfig` 对版本 2 严格解码，`decodeField` 把缺失键当错误。版本 1 会在内存中补缺省值，版本 2 没有这条路径。

影响：版本 2 于同一天引入，尚无发布标签，只影响开发与台架环境里在这段时间保存过配置的机器；失败界面信息准确，文件不会被改写。可以接受为未发布格式的调整，也可以在解码时把缺失的 `share` 视为空字符串。本轮未改代码。

#### R2. English 下未授权标题在缺省宽度换行（界面，低）

窗口宽度 1080 时，「Hub has not authorized this device」在页头折成两行，右侧副标题被挤窄；简体与繁體在一行内。不影响信息，但页头高度跳变。可以考虑更短的英文标题，或让副标题在窄时换到下一行。

#### 仍存在的小问题

- 中心确认框正文仍把中心 ID 与 URL 排在一行、没有标签（上文发现 5 的第三点，本轮修复不涉及）。

### 操作记录（非产品问题）

- VM 的 DNS 由代理软件提供，对不存在的名字也返回地址，所以自检的对照用 `.home.arpa` 下的名字（该特殊用途域在本机快速返回解析失败）。
- 设置页内容超出窗口高度时，滚动条在鼠标悬停后才出现；先悬停再拖动滚动条即可。
- 首次在 `127.0.0.1` 呈现时防火墙同样弹出询问，按取消处理，生成的两条阻止规则在收尾时删除。

### 清理

- devbox：Hub、假网关与两个 Client 进程已停止，测试端口无监听；临时目录（Hub 身份、Client 状态、邀请码文件、二进制）已删除，只保留不含秘密的日志、配置、自检报告与截图作为私有证据。
- Windows：测试 GUI 经界面「退出」正常结束（退出码 0）；本轮计划任务（含清空剪贴板用的临时任务）、防火墙自动生成的两条规则、专用目录（含测试身份与签发密钥）均已删除；hosts 已恢复且哈希与测试前一致；剪贴板已清空（读回长度 0、无格式）。
- 自用 Operator：开始前与结束后都没有在运行，默认配置目录不存在，47000 端口无监听；VM 上原有的其他测试进程、既有防火墙规则与 mygo 单实例套接字与测试前一致。
