# M0b：Windows 工具兼容性验证

目标是验证所用版本 E-Sys / ISTA / Tool32 的发现与连接行为，不需要真车。自动化测试只证明我们自己的传输与模拟响应；它不能证明诊断工具已经接受车辆，也不能替代完整 ECU 仿真。

## 测试拓扑

先使用 Linux devbox 运行假 ZGW 与 Client，Windows VM 运行 Operator 与诊断工具。这样车侧模拟始终位于 Linux loopback，不需要在 Windows/macOS 上增加 loopback 地址别名：

```text
E-Sys / ISTA / Tool32 (Windows VM)
    -> Operator (Windows VM, explicit adapter IPv4)
    <- Hub TCP connection initiated by Client
Client (Linux, source 127.0.0.1)
    -> fake ZGW (Linux, 127.0.0.2)
```

两台机器只需能建立一条到 Windows Hub 端口的 TCP 连接。诊断 UDP 广播留在 Windows 本机网卡，不经过公网或隧道。先选已有的 LAN IPv4，再按需要使用隔离测试网卡的 link-local 地址；记录地址与掩码，不假设一定要 `169.254/16`，不改管理网卡来做实验。

## 准备产物

在开发机交叉构建，复制两个 Windows 可执行文件到 VM，Linux 本地也构建一份：

```sh
mkdir -p bin/windows-amd64
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/windows-amd64/renetd.exe ./cmd/renetd
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/windows-amd64/fakezgw.exe ./cmd/fakezgw
CGO_ENABLED=0 go build -o bin/renetd ./cmd/renetd
CGO_ENABLED=0 go build -o bin/fakezgw ./cmd/fakezgw
```

以下 `WINDOWS_PRESENT_IP` 为诊断工具所用网卡的 IPv4，`WINDOWS_HUB_HOST` 为 Linux 能访问到的 VM 地址或 DDNS。第一次 LAN 测试两者可以相同；隔离 link-local 网卡未必能从 Linux 访问，Hub 地址应继续使用可达的 LAN 地址。`state/operator` 和 `state/client` 各自保留设备身份，不能在两端混用同一份目录。

Windows PowerShell 启动 Operator，使用标准工具端口，远端仍指向模拟网关的高位端口：

```powershell
.\renetd.exe operator --state-dir .\state\operator `
  --listen 0.0.0.0:47000 --bind WINDOWS_PRESENT_IP `
  --remote-diag-port 16801 --remote-ctrl-port 16811
```

在另一窗口签发邀请，保存为纯 UTF-8 文本，避免 Windows PowerShell 旧版重定向默认产生 UTF-16 文件：

```powershell
.\renetd.exe invite --state-dir .\state\operator `
  --endpoint WINDOWS_HUB_HOST:47000 --ttl 15m |
  Set-Content -Encoding utf8 .\invite.txt
```

将邀请码文本安全传到 Linux devbox。CLI 接受带或不带 BOM 的 UTF-8 文件；保持正文完整即可。Windows PowerShell 的直接重定向可能产生 UTF-16，因此使用上面的显式 UTF-8 写法。编码行为依据 [Microsoft 文档](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_character_encoding?view=powershell-5.1)。邀请码含一次性秘密，不放入验证报告。

Linux 准备显式只读响应并启动假 ZGW：

```sh
mkdir -p /tmp/remote-enet-m0b
cat > /tmp/remote-enet-m0b/responders.json <<'EOF'
{"22f190":"62f1905742415858585858585830303030303031"}
EOF
./bin/fakezgw --bind 127.0.0.2 \
  --diag-port 16801 --ctrl-port 16811 --disc-port 16811 \
  --profile long-alive --alive 5s --alive-timeout 2s --kl15 04 \
  --responders /tmp/remote-enet-m0b/responders.json
```

另一个 Linux 终端启动 Client：

```sh
./bin/renetd client --state-dir /tmp/remote-enet-m0b/client \
  --invite-file /tmp/remote-enet-m0b/invite.txt \
  --nic-addr 127.0.0.1 --discovery-target 127.0.0.2:16811 \
  --discovery-timeout 300ms
```

确认 Operator 日志出现 `operator presenting vehicle`，三个本地监听端口为 `6801/tcp`、`6811/tcp`、`6811/udp`，再打开诊断工具。假 ZGW 的周期、KL15 值和 UDS 响应均为实验配置，不是实车事实。

## 最小测试矩阵

每种工具分别在 LAN 与隔离 link-local 绑定下测试；不要同时打开其他工具干扰结果。

| 工具 | 操作入口 | 分开记录的结果 |
|---|---|---|
| E-Sys | 记录实际版本与 UI 模式名；先测试手工 gateway URL `tcp://WINDOWS_PRESENT_IP:6801`，再试 VIN 自动发现 | TCP 是否建立、是否显示发现 VIN、是否进入后续诊断；不要使用未经证实的“WiFi ENET”模式名 |
| ISTA | 自动发现，保持产品不要求手动网关 IP | 是否发出 6811 广播、是否收到应答、列表是否显示 VIN、是否建立 TCP；DoIP/SLP 请求不等于本轮已实现这些协议 |
| Tool32 / INPA | `[XEthernet] RemoteHost=Autodetect`，另记录显式 IP 的对照 | 发现返回地址、6801/tcp 与 6811/tcp 是否建立、是否查询 KL15、测试使用的 EDIABAS 版本 |

假网关只对配置过的只读请求回应，其余请求不会默认成功。因此“发现列表显示车辆”与“完整诊断可用”必须分开。没有后续 UDS 响应、VIN fixture 不符合工具期望或工具要求额外车辆识别时，应记录具体触发行为，不把任意失败归咎于隧道，也不能据此宣称兼容。

若需查看 HTTP listener，可在 Windows 只读运行 `Get-NetTCPConnection -State Listen`，记录工具进程、实际监听地址与端口；这仅验证 listener 存在，不验证 ECU 回连 URL、地址通告或刷写。

## 结果记录

每项记录：代码 commit、Windows/工具/EDIABAS 版本、选定网卡与 IP/掩码、工具实际入口、发现是否显示 VIN、TCP 建连结果、第一次未被模拟的请求及原始错误。网络证据仅收集本次模拟链路，不写入邀请码、私钥或真实车辆操作记录。

当前自动化验收覆盖 packet-info、源 IP/端口、原样发现应答、配对、TCP 转发与退出。2026-11-16 已在目标 Windows VM 验证 LAN 广播收发、E-Sys 3.36.2 与 ISTA 4.39.20 的自动发现，以及部分 TCP 连接行为；完整诊断与其余矩阵尚未验收，见[实测记录](research/m0b-win11-2026-11-16.md)。本仓库中的测试不访问该 VM 或外部车辆。
