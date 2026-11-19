# Wave 7 · 跨平台稳定性与链路验收

基线：`b043610`。用户确认继续下一阶段，并确认 Mac 由用户手动验收、真实 WAN/DDNS 后补；当前先完成 CI 修复与隔离弱网验证。只做发现/呈现/透明转发，不增加车辆编码或刷写逻辑，不改变 CBOR、TLS 接受字节、JSON v1 或身份/配对格式。

## 已确认的 CI 失败

`43858459825`：

1. Windows settings `TestLoadMissingOnly`：配置路径父级为普通文件时，系统可能把 Lstat 错误映射为 NotExist，错误返回默认配置。必须修生产校验，并核对 FixedRole 载入路径。
2. macOS desktop `TestRealAppsPairRememberReconnectTestAndRelease`：绑定未分配的 `127.0.0.2` 失败。保持车端拒绝本机地址的边界，台架不能要求客户或 CI 获得管理员权限/创建地址别名。
3. Linux race `secure/TestLifetimeCancellation`：本地 ctx 取消后，终止原因偶发记录为 EOF。尚未把根因定为测试或生产；需先确定语义并可控复现。

## 并行分工

用户此前授权 Paseo Codex 并行开发、整合者审查与 DeepSeek 独立复审。本轮独立 worktree，全部不 commit/push，不访问用户 Mac、Windows VM 或 DDNS，不扩展范围。

### A · 严格配置路径

拥有 `internal/settings/` 及 `internal/desktop/controller.go`、`fixed_role_test.go`（必要时新增路径聚焦测试）。修复 Load 与 FixedRole 的“真正缺失”判定：父级为文件/损坏 symlink/权限或 I/O 错误不能降级默认；合法缺失多层目录仍能得到默认且不创建磁盘内容。保持既有 symlink 与 JSON 校验约束、返回错误为零 Config。优先复用共用判断，避免 FixedRole 绕过 settings 校验；无需改变外部 API/schema，不放宽现有错误测试。

补聚焦路径回归与父文件多层场景。Linux 普通/race；Windows 测试二进制交叉编译供整合者原生执行，禁止用 GOOS 交叉编译宣称已运行。

### B · secure 生命周期取消

拥有 `internal/secure/`。分析 lifetime ctx、本地取消、mux EOF、ping 失败与显式 Close 的终止时序。先用可控同步（不是延长 timeout/sleep）复现本地取消丢原因，再做最小修复。调用方取消整个 session 应可识别 context.Canceled/DeadlineExceeded；个别 OpenStream 取消不得终止共享会话。真实远端断连、显式 Close(reason) 仍应保留合理且稳定的原因，不能把所有 I/O 失败都伪装成取消。

不改变 mTLS 验证、ALPN、接受字节、yamux 行为与既有半关闭。聚焦普通/race与有解释的重复回归，完成后固定源码，避免无限加测。

### C · macOS 台架兼容

拥有 `internal/desktop/app_test.go` 及必要的 testutil 测试辅助（不修改生产 carside/app/白名单）。审查 `127.0.0.2` 与跨平台单机仿真限制，给出并实现诚实策略：仅识别出 fixture 需未配置 loopback 地址且该能力不可用时可明确跳过这一真实双 app 用例；其他监听/资源/权限错误必须失败。继续在 Linux/支持该拓扑的 Windows 跑完整断言；其余 Mac 的真实 Operator、TLS/session 和受控 runtime 测试保留。若能在不提权、不增加生产测试后门、不放宽本机目标限制的前提下覆盖同等拓扑，可实现并给证据，不能凭空假定 wildcard 路由可用。

补可确定性验证的 fixture 能力分类回归，解释跳过范围。不得 `GOOS==darwin` 整包跳过、不得自动创建地址别名或仅削减断言掩盖失败。文档改动由整合者汇总。

## 整合者工作

- 核对三份实现，执行本地全量门禁；在现有 Windows VM 执行 settings/desktop/secure 原生回归；用 DeepSeek 对最终差异只读复审。
- 构建可复现的 macOS arm64 Client `.app` 预览，核对 Mach-O 架构/最小版本/可执行权限/Go ad hoc signature，保留未 Developer ID 签名公证与实机待验收的边界。
- 提供用户 Mac 的分层验收步骤；只有用户回报/证据才标记实机通过。
- 弱网使用独立 Linux network namespace 与 loopback，不改变主机管理网卡的 qdisc、路由或防火墙。tc 若缺失，优先仅解压到临时工具目录；不安装无关依赖。独立脚本/报告记录参数、延迟定义、真实内核丢包、读请求/重连/旧连接回收等结果。
- 正常自动化门禁仅使用 loopback/net.Pipe/内存，netem 是可选手动验收，不要求 CI/root、真车或外部服务。
- 真实 DDNS、手机热点及 F20 阶段按用户决定后补；本轮不伪称 WAN 或实车已验收。

## 首轮整合验证

三份 CI 修复合入后，Linux 全仓 CGO=0 vet/普通测试及 CGO=1 race 通过。Windows 原生 settings、desktop 全包通过；`GetFileAttributesEx` 实测：缺失叶文件为 `0x2`，缺失父目录与父级是文件均为 `0x3`，均会匹配 NotExist，因此必须继续检查父级。新的检查没有放宽原来的错误回归。

Windows secure 全包补测发现原有 TestStreams 的非零 RTT 判据不成立。yamux 的计时从发送返回后开始，极快的 net.Pipe 应答可能得到合法零值；已在私有测试字段使用负数哨兵，只有成功的周期 Ping 发布样本后才通过，接受零而不制造非零数值，原 5 秒期限保留。临时 overlay 证明：真实 Ping 后发布零值可通过，移除采样发布仍失败。Windows 原生 TestStreams 连续 20 次通过，真实周期 Ping 均采得零 RTT；secure 全包及关闭原因分类重复回归也通过。CI 的原生运行补入 secure 包，避免只在 Linux 验证这一边界。

macOS 真双 app 回归仅在严格识别目标 `127.0.0.2:0` 的 listen/bind/EADDRNOTAVAIL 时报告 fixture 不可用；不创建别名或放宽本机目标限制。其他 Mac 测试继续执行，Linux/支持此拓扑的 Windows 上完整链路仍执行。

## Mac 手动验收

用户选择手动操作 Mac，真实 WAN/DDNS 后补。详细分层步骤见 `docs/MACOS.md`。用户已回报基本界面操作与网卡地址，恢复窗口的实机问题已定位并复验；wave7.2的关闭即退出和随后Mac→Windows假车链路均已实机通过，不以交叉编译或headless UI测试替代。

新增 `tools/build-macos-client.sh` 可从当前源码重建 arm64 `.app` ZIP：`RENET_BUILD_VERSION=0.1.0-preview.wave7 tools/build-macos-client.sh`。工作区未提交时版本明确带 worktree，包结构、Mach-O 架构、最低系统版本、可执行位及 linker signature marker 均有校验；Info.plist 记录完整预览标识、源版本与构建时间，标准版本字段保留语义基础版及日期构建号。预览不做 Developer ID 签名与公证。

### Mac 用户反馈与关闭行为

用户确认基本打开、Settings 保存重开、Log 正常，en0 为 `192.0.2.188`。实机发现关闭后旧窗口无法从 Dock/Finder 唤回：原宿主只订阅 OnSecondInstance，而 AppKit 重新打开现有应用通过 OnActivate 分发，不一定启动第二个进程。补上无可见窗口时恢复原窗口的接线后，用户第一次复验实际仍运行旧 b043610；用明确的新路径启动 wave7.1 后，用户确认恢复正常。该修复由实机反馈确认，单独的 headless/交叉编译不曾作为通过依据。

随后用户明确要求关闭按钮直接 Quit，已统一 Client/Operator 行为：关闭先 PreventDefault，再进入现有 OnBeforeQuit；取消、controller.Close 和 worker 汇合完成后设置 quitReady 并发起最终 Quit，才允许原生窗口销毁。重复关闭不启动重复清理或绕过汇合。托盘不再是恢复已关闭主窗口的必要入口，源接线仍覆盖运行中实例激活。

最新 Mac 包为 `0.1.0-preview.wave7.2-worktree`，包含关闭即退出行为；操作步骤见 `docs/MACOS.md`。Windows 双端构建、宿主聚焦 vet/普通/race 与 DeepSeek 关闭顺序复核通过。Windows 交互桌面实际运行两个新入口，分别用原生 WindowPattern.Close（对应窗口关闭按钮）关闭：Client 与 Operator 均退出0；Operator 在关闭前确实监听 loopback Hub 47071，退出后端口释放。测试无强杀，临时计划任务已清理。用户随后确认 Mac 新关闭行为“没问题”，记为实机通过。

### Mac → Windows LAN 原生链路

2026-11-16 至17完成真实 Mac Client→Windows GUI Operator→Linux假网关验收。用户手动选择en0/192.0.2.188后发现并连接成功；peer-only测试10/10且车辆连接、收发计数仍为0。普通桌面权限探针核对了完整发现数据报、ACK/VIN、6811控制回应与半关闭。用户Stop、清空邀请码后再次Connect，产生新Hub TCP会话，第二次探针同样通过。保留诊断TCP时，用户直接关闭Mac应用，Windows观察到EOF、旧车辆连接回收；Operator仍监听且不再有Mac会话。

Windows Operator随后正常退出，测试监听、限定源防火墙规则与临时计划任务均已回收。Linux源过滤测试入口只接受192.0.2.188，共四个车辆TCP连接、两次VIN与两次控制请求，全部关闭；fixture子进程退出0且无残留worker。完整拓扑、逐项证据和Windows混合权限限制见 `docs/MACOS.md`。高权限SSH探针未通过，普通桌面权限探针通过；原生OpenProcess对照确认普通进程读取高权限进程被拒绝，本轮没有绕过独占检查，未把混合权限组合判为通过。

## 隔离内核弱网验收

新增 `tools/netem-check.py`，仅作 Linux 手动验收工具，不加入默认测试。Python 使用标准库，不安装依赖。整形进程必须处于与 PID 1 不同且仅有 lo 的 network namespace；每次 tc 查询与修改均重新检查并记入报告。全部 renetd、fakezgw 和诊断 worker 使用明确的非 root UID/GID、清空附加组；root 仅编排隔离网络。仅 TCP 47000 的两个方向进入 netem，车辆/诊断端口不整形。

从仓库根目录执行以下命令，要求系统已提供 unshare、ip 和 tc，且可 sudo。每次输出目录必须不存在；不要把工具直接运行在主机网络中。

```sh
netem_dir=$(mktemp -d /tmp/remote-enet-netem.XXXXXXXX)
CGO_ENABLED=0 go build -o "$netem_dir/renetd" ./cmd/renetd
CGO_ENABLED=0 go build -o "$netem_dir/fakezgw" ./cmd/fakezgw
sudo -n unshare -n -- /usr/bin/python3 tools/netem-check.py \
  --renetd-bin "$netem_dir/renetd" \
  --fakezgw-bin "$netem_dir/fakezgw" \
  --output "$netem_dir/run" \
  --actor-uid "$(id -u)" --actor-gid "$(id -g)"
```

报告位于输出目录 `report.json`；目录及状态、日志、邀请码仅所有者可读，不提交实际密钥或邀请码。测试包含：baseline 三次只读 VIN、单向 100ms/配置丢包 1% 的十次请求、双向 100% 黑洞、旧连接释放与自动重连、恢复后三次请求、两次 peer-only 测试。逐次核对原始 ACK/VIN；peer-only 测试必须完整成功且不增加车辆连接或载荷计数。报告记录真实 tc packets/bytes/drops；样本不等于测得 1% 丢包率。

### 首轮暴露的在途请求语义

`netem-run-01` 保留为失败证据：baseline 三次与 degraded 十次成功，黑洞观察三秒内 fake 仍为 13；停止 Operator 后过早恢复网络，一条在途请求随后在同一旧车辆 TCP 连接上送达。该第 14 条发生在 Client generation 2 建立之前，不能定为跨代重放，也不能声称旧请求没有执行。Operator 正常退出和本地工具 EOF 均不能证明远端旧会话已经关闭。

最终脚本保持 100% 黑洞，有界等待 Client 本地 Ping 超时与对应旧 fake 连接关闭，期间仍要求 fake=13。两个证据齐备后才恢复网络、重启同状态 Operator 并等待新代次；新工具请求前仍要求 fake=13，三次恢复请求后严格等于16。该断言验证跨会话不重放，不承诺在故障前已经发送的操作绝未在旧连接执行。无需修改生产 TCP 关闭方式或解释车辆内容。

### 已执行结果与边界

2026-11-16 的完整运行 `netem-run-03` 通过：3/10/3 次请求全部成功；degraded p50=200.388ms、p95=355.858ms（从请求发送开始至完整 ACK+VIN 返回，10 个样本，线性插值）；degraded 内核计数109包/1次丢弃；黑洞初段16次丢弃。保持黑洞约22.294秒后 Client 报告 Ping 超时，旧车辆连接同毫秒关闭；自动恢复到 generation 2，fake 最终16条、两个先后连接，无其他诊断帧。两个 peer-only 测试均完成10个样本、每方向1MiB，车辆计数为零；所有子进程正常退出且无需强杀，隔离 qdisc 已删除，主机 qdisc 与运行前一致。

DeepSeek 只读复核确认上述时序与计数，原报告不覆盖修改。run-03 之后仅增加逐次 tc 查询审计和报告字段改名；最终源码直接执行 `netem-run-04` 再次通过：16次请求、两次完整 peer-only 测试、generation 1→2、旧连接回收与不重放断言均通过，29次 tc 查询全部记录且所有命令返回成功。degraded p50=200.389ms、p95=353.309ms，计数101包/1次丢弃；黑洞约22.296秒后旧会话结束。六个子进程均退出0，无强杀或清理错误，隔离 qdisc 已删除。

Linux CI修复整合后的全仓格式、CGO=0 vet/test/build与CGO=1 race均通过；后续宿主关闭变更补做聚焦vet/普通/race、双端Windows构建及原生点击关闭。Windows settings/desktop/secure原生通过；Mac基本界面、新版关闭及LAN原生假车链路通过，arm64包已重建并验证元数据。这些是实现阶段的本地与原生验收记录，远端CI另以对应提交的Actions结果为准。Windows混合权限组合未验收，真实WAN/DDNS和实车仍未验收。
