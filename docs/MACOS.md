# macOS Client 手动验收

当前用户选择手动操作 Mac；本阶段先做界面与 LAN 假网关验证，DDNS/WAN 后补。未收到结果的项目保持待验收。

2026-11-16 用户回报：打开、Settings 保存与 Log 基本正常；en0 IPv4 为 `192.0.2.188`。旧版关窗后无法通过 Dock/Finder 恢复。增加 OnActivate 接线后，首次复验仍运行 Downloads 中的旧 b043610；明确从新目录启动 wave7.1 后用户确认恢复正常。随后用户要求关闭按钮直接 Quit；从 wave7.2 起两端都改为完整退出，以下步骤按这一最新行为验收。顶部托盘图标未看到，不作为关闭或启动应用的必要入口。

用户随后对 wave7.2 回报“没问题”，将 Mac 新关闭行为记为实机通过；接着完成了下文 B 的原生 LAN 假车链路验收。

Wave 7 当前工作区的 arm64 包为 `dist/remote-enet-client-macos-arm64-0.1.0-preview.wave7.2-worktree.zip`。可在 Mac 下载后解压；worktree 表示包含尚未提交的本轮修复。旧包不会获得新的关闭行为，以下检查及后续链路验收使用最新包。

```sh
scp operator@192.0.2.111:/home/example/src/remote_enet/dist/remote-enet-client-macos-arm64-0.1.0-preview.wave7.2-worktree.zip ~/Downloads/
mkdir -p "$HOME/Downloads/remote-enet-wave7.2"
ditto -x -k "$HOME/Downloads/remote-enet-client-macos-arm64-0.1.0-preview.wave7.2-worktree.zip" "$HOME/Downloads/remote-enet-wave7.2"
open -n "$HOME/Downloads/remote-enet-wave7.2/Remote ENET Client.app"
```

替换前先用 Dock 的 Quit 退出旧进程，再启动新目录中的应用；同名 bundle 的旧进程仍运行时，系统或单实例机制可能把打开请求交还旧实例。可用 `ps -axo pid=,comm= | /usr/bin/grep '[r]enet-client'` 核对正在运行的实际路径。

## A · 不接车的界面检查

使用发给你的 macOS arm64 预览 ZIP，解压并打开 `Remote ENET Client.app`。最低系统版本从 Mach-O 构建元数据写入包内；当前是 macOS 13。预览包未做 Developer ID 签名或 Apple 公证，按 [Apple 官方说明](https://support.apple.com/102445)处理首次拦截，不修改系统全局安全策略。

1. 窗口标题应为 Client，界面为浅色；没有 Operator 角色切换或监听/生成邀请按钮。
2. Settings 的网卡下拉应能列出本机可用 IPv4。没有接线时保持 Automatic 即可，暂不要求车辆发现。
3. 切换一次 Allow peer connection tests，明确 Save。用 Quit 完全退出再重开，应保留该值；验证后可改回开启并保存。
4. Log 可打开，Back 返回主页面；窗口缩到最小和使用系统缩放时，内容可滚动，顶部导航仍可达。
5. 点击窗口红色关闭按钮，应完整退出；`ps` 不再列出 Client 进程。再次打开新目录中的应用，应启动一个新实例并保留已保存设置。
6. 窗口仍打开时再次打开同一应用，应唤回原实例；Quit 与红色关闭按钮都完整退出。发生问题时记录版本、所在步骤和错误文案；报告不包含邀请码、密钥或真实车辆数据。

## B · Mac Client → Windows Operator → 假车

由整合者准备一个受控窗口：Linux devbox `192.0.2.111` 运行假网关，Windows VM `192.0.2.107` 运行 Operator，Mac 只运行 Client。台架使用 LAN，不需要路由器转发或真车，不用在 Mac 新建 loopback 别名。

```text
Mac Client（明确选择 LAN 网卡）
    ├─ UDP 发现 → Linux 假网关（受控回放发现 fixture）
    ├─ TCP Hub → Windows Operator
    └─ 车辆 TCP → Linux 假网关
Windows 只读探针 / E-Sys 等 → 本机 Operator 呈现端口
```

本轮已确认 Mac en0 为 `192.0.2.188`；后续再次运行需核对当前地址，以便限定假发现 responder 与测试防火墙来源，只提供地址，不提供密码或邀请码。

1. 整合者先准备好假网关、Operator、限定来源规则和短期邀请，随后把本次测试邀请保存到私有测试文件供 SCP；邀请不贴到报告或日志。
2. Mac Settings 选择可以访问 `192.0.2.111` 的现有 LAN 地址并 Save。本阶段此处明确选 LAN 网卡；Automatic 的生产选择主要面向唯一的 link-local 车侧地址。
3. 返回主页面，粘贴本次邀请并 Connect。应看到 Connected、模拟 VIN、网关、网卡/源 IP 和真实 RTT。
4. Windows Operator 也应 Connected；执行 peer-only 连接测试、显式只读 VIN/6811 控制探针，核对回应和流量计数。
5. Mac Stop，Operator 保持等待下一会话；Mac 再次 Connect 时邀请留空，应使用记住的 Peer 成功。
6. Mac Quit，核对车侧连接退出；Operator 结束并退出后收回所有临时监听。整合者清理 Linux 服务、测试规则、计划任务与临时邀请。

这里验证的是实际 Mac Client 与假网关的透明链路，不代表完整车辆识别、FA/SVT/coding 或 HTTP 刷写验收。共享 LAN 下原始假车与呈现端可能同时响应广播，探针应明确目标，分别记录来源。

### 本轮实测结果（2026-11-16 至 17）

使用 wave7.2 Mac Client 与 Windows 原生 GUI Operator；Operator 和最终诊断探针均以普通桌面权限运行。Windows Hub 为 `192.0.2.107:47000`，诊断、控制和发现仅呈现在 Windows loopback `127.0.0.1:6801/6811`。Linux 在 `192.0.2.111:6801/6811` 暴露只允许源 `192.0.2.188` 的 TCP 测试入口，转发到 loopback 假网关；UDP 6811 仅向同一 Mac 回放完整发现 fixture。Windows 临时入站规则同样限定 Mac 地址、TCP 47000、Private profile 和测试 Operator 可执行文件；未改动已有规则。

| 项目 | 观察结果 |
|---|---|
| 网卡选择与发现 | Automatic 下等待网卡；用户明确选择并保存 en0/192.0.2.188 后 Connected，源绑定和模拟网关为预期地址 |
| 真实两端会话 | Windows GUI 显示模拟 VIN、en0、源 IPv4 和 direct 路径；首个 Hub 对端端口为52814 |
| Peer-only 连接测试 | 10/10 探针完成；UI 显示 echo RTT p50=3.6ms、p95=4.2ms，echo throughput=4.4MiB/s；测试后车辆 TCP 连接数仍为0，车辆收发计数均为0 |
| 发现原样应答 | Windows UDP 6811 返回完整56字节 fixture，回源为127.0.0.1:6811 |
| 只读诊断 | 完整11字节 ACK 与28字节 VIN 回应逐字节匹配，不依赖仅搜索 VIN 文本 |
| TCP 控制通道 | 6811 的显式测试状态请求得到7字节原样回应；04为台架配置值，不代表实车状态 |
| 半关闭 | 完整请求/回应后，工具关闭发送方向并收到 EOF，双端连接正常回收 |
| 无邀请码重连 | 用户 Stop、清空邀请码再 Connect 成功；新 Hub 对端端口为53054，第二次发现/VIN/控制校验再次通过 |
| 关闭活动会话 | 第二次诊断连接保持打开；用户直接关闭 Mac Client，应用退出，Windows 探针收到 EOF，假网关旧连接关闭；Operator仍监听且已无Mac会话 |
| 清理 | Windows Operator正常Quit，TCP/UDP端口释放；临时规则和计划任务移除；Linux所有worker回收、两个fixture子进程退出0，无强杀 |

界面显示的 RTT/echo throughput 为本次 LAN 测量及显示精度，不推算真实 WAN 性能或单向容量。Linux 最终收到四个源为192.0.2.188的 TCP 连接，共两次只读VIN和两次控制请求，四个连接全部关闭。私有探针与清理报告保留在测试目录，不提交实际邀请码或身份密钥；Mac下载的临时邀请文件可由用户删除。

### Windows 权限组合边界

最初由高权限 SSH 会话启动的探针被关闭，同样的探针以普通桌面权限运行时全部通过。补充原生 API 对照：普通权限进程对仍存活的高权限 SSH 进程调用 OpenProcess，分别请求0x1000（查询）和0x101000（查询加同步）均得到 ERROR_ACCESS_DENIED=5。现有独占解析需要读取创建时间并核对进程存活，因此不能识别该权限组合；没有通过关闭独占或取消进程校验掩盖结果。

本轮验收覆盖普通Operator＋普通诊断工具。诊断工具若以管理员权限运行，应让Operator使用相应权限；权限不足时的明确界面提示仍可后续改进。本轮不改变默认提权策略、访问控制或进程识别不变量。

本轮固定en0是LAN台架配置。实际连接ENET线前，应选择真正的车辆网卡，或切回适用的Automatic选择并保存。

## C · 后续真实 WAN

用户当前决定 WAN 后补。届时先确认 DDNS/公网端口映射，再用手机热点连接同一套假网关链路，记录实际 RTT、断开恢复、来源绑定和端口回收；之后单独安排 F20 的只读验证。
