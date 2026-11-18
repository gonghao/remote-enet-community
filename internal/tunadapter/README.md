# TUN 适配器工作包

只拥有本次新建的设备、Local IPv4 `/32` 地址与明确目标的 host route。普通测试使用模拟原生设备和系统操作，不加载驱动，不修改网络。Windows与Darwin已完成独立手动台架验收，结果及边界见 [Wave 8](../../docs/WAVE8.md) 与 [Wave 9](../../docs/WAVE9.md)；Linux原生TUN尚未验收，交叉编译不能替代原生验收。

## 平台规则

- Linux：Name 留空时随机分配 `enet` 前缀名称；显式名称限定 ASCII 字母开头、字母/数字/下划线/连字符、最多 15 字符。使用内核 `IFF_TUN_EXCL` 排除同名设备；通过 WireGuard 的无事件监听 FD 适配器传包，MTU、地址与路由由绝对路径 `ip` 配置，不启用 GSO/VNET。
- Darwin：Name 为 `utun` 或 `utunN`，留空等同 `utun`；内核分配时返回真实名称。需要 `ifconfig`、`route` 与 `netstat`。自动命名时拒绝设置了 `WG_TUN_NAME_FILE` 的环境，避免上游额外写文件。
- Windows：Name 是最多 15 字符的标签前缀，规则同 Linux；实际新建设备名附加随机 128 位后缀，必须使用 `Adapter.Name()` 查看。需要系统目录下的 `netsh.exe`、`route.exe`，以及可由固定版 wintun Go 包加载的官方 Wintun DLL；本包不安装 DLL、驱动或删除公共驱动。上游 [Wintun 适配器源码](https://git.zx2c4.com/wintun/tree/api/adapter.c) 的 `NciSetAdapterName` 可能在别名冲突时重命名其他适配器，因此不直接使用可预测的完整 Windows 名称。随机后缀避免现实碰撞，但不保证抵御恶意外部进程同时抢占同一个随机名称。

MTU 默认 1500，范围 576..65535。目标数限制为 1..256，拒绝重复、Local、本机、回环、组播与非法 IPv4。创建前检查当前网卡地址与 IPv4 路由：Local 或目标落入现有网卡网段、非默认路由时保守拒绝。默认路由只读取、不修改。Linux 检查所有路由表；Darwin 无法解析的数字目标及 Windows 非连续掩码返回错误。

Windows 配置成功后，Open 还会按本轮新设备的 LUID 与精确 Local `/32` 检查地址就绪：仅接口 Up 且地址 DadState=Preferred 时成功；未出现或 Tentative 时继续采样，其他状态明确失败并完整回滚。不禁用 DAD，不修改管理网卡。等待预算默认 5 秒（内部参数），与调用方 context 取较早截止时间，每 50 毫秒重新查询真实状态，不以固定延迟判定成功。查询使用已有 x/sys/windows.GetAdaptersAddresses，缓冲从 15KB 起、最多三次尝试、最大 1MiB；不引入命令或依赖。此 Win32 API 仅支持同步调用，无法中断已经开始的单次查询；context 会在查询前后及两次查询之间检查，不额外启动可能遗留的查询 goroutine，因此不能宣称该系统调用有硬实时超时保证。状态探针、取消、超时与回滚均有模拟测试；Windows 原生就绪修复已完成台架复验，详见上述验收记录。

每次系统命令最长 5 秒、总输出最多 1 MiB；不调用 shell，不使用 PATH 中的可执行文件。成功的地址/路由步骤才进入回滚清单；逆序尝试所有删除，清理总预算 10 秒，再关闭原生设备并汇合 worker。失败与取消后的清理错误通过后续 `Close()` 保留并返回；不会将“资源不存在”等所有命令错误笼统吞掉。命令在系统已经生效后报告失败的极端情形，最终关闭新建设备还会撤销其关联地址和路由，返回原始失败供人工核对。

固定一个 native read worker、一个 native write worker、一个 Events 消费 worker 和一个生命周期观察 goroutine。读取队列为 16 包，原生 batch 最多 128；单次写入先获取有界入口再复制内存。调用取消不会关闭整个设备；已经进入原生 Write 的包无法撤回，返回取消不表示包绝对没有发出。Close 先清理网络资源，再关闭原生设备解除阻塞并汇合。调用方必须等待 `Close()` 返回后才认为已清理完毕。

## 自动验证

```sh
CGO_ENABLED=0 go vet ./internal/tunadapter/...
CGO_ENABLED=0 go test ./internal/tunadapter/...
CGO_ENABLED=1 go test -race ./internal/tunadapter/...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/tunadapter-windows.test.exe ./internal/tunadapter
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go test -c -o /tmp/tunadapter-darwin.test ./internal/tunadapter
```

race 检测器本身需要 CGO；生产及跨平台构建仍为 CGO=0。

## 显式原生验收交接

以下操作只交给用户在获准的测试主机上手动执行，代理未执行；需要用户自行以已有管理员权限运行，不提供提权或安装命令。先确认测试地址不属于管理网、不已有路由；预检会拒绝冲突。统一入口由整合者和 A 工作包提供：`cmd/renet-tuncheck`，本工作包不保留第二套长期命令。以下 CLI 为整合者约定，尚待其工具落地后核对。

Linux/macOS：

```sh
CGO_ENABLED=0 go build -o /tmp/renet-tuncheck ./cmd/renet-tuncheck
/tmp/renet-tuncheck --run --local 198.18.8.1 --target 198.18.8.2 --name enetcheck --timeout 30s --report /tmp/renet-tuncheck.json
```

macOS 的 `--name` 需改为 `utun` 或未占用的 `utunN`；Windows 名称为前缀。

Windows PowerShell：

```powershell
$env:CGO_ENABLED = '0'
go build -o "$env:TEMP\renet-tuncheck.exe" ./cmd/renet-tuncheck
& "$env:TEMP\renet-tuncheck.exe" --run --local 198.18.8.1 --target 198.18.8.2 --name enetcheck --timeout 30s --report "$env:TEMP\renet-tuncheck.json"
```

分别在启动前、运行期间、退出后保存快照：

| 平台 | 设备/地址与路由只读快照 | 宿主 22 端口只读快照 |
| --- | --- | --- |
| Linux | `ip -j link show`、`ip -j -4 address show`、`ip -j -4 route show table all` | `ss -ltn 'sport = :22'` |
| macOS | `ifconfig -a`、`netstat -rn -f inet` | `lsof -nP -iTCP:22 -sTCP:LISTEN` |
| Windows | `Get-NetAdapter`、`Get-NetIPAddress -AddressFamily IPv4`、`Get-NetRoute -AddressFamily IPv4` | `Get-NetTCPConnection -State Listen -LocalPort 22` |

预期仅出现一个新设备、一个 Local `/32` 与目标 `/32` 路由；设备附带的本地/直连路由也属于该新设备。30 秒后或者 Ctrl+C，等待命令退出，核对新增资源消失、默认路由/管理网卡/已有 22 监听保持。再用冲突 Name、已有本机目标、非 `/32` Local、超量目标验证拒绝不产生变更。预算与回滚异常通过模拟测试覆盖，不能假装已完成原生故障注入。

最终 TCP 22 banner、HTTP、半关闭及端到端流转由整合者的 L3 手动工具验收；本工作包不宣称完成认证登录、mgu_kit 自动发现或原生运行验证。

## 会话自动本地地址

`OpenAuto(ctx, targets)` 依次预检 `198.18.0.1/32`、`192.0.2.1/32`、`198.51.100.1/32`、`203.0.113.1/32`。例如 Surge 已占用 benchmark 网段时，会选择 documentation 地址；所有候选都冲突则失败。仅创建前的本地地址冲突返回可用 `errors.Is(err, ErrConflict)` 分类的错误并换候选。目标冲突、权限、原生创建和系统配置失败均直接返回，保持原严格回滚。不会接管默认路由或已有设备。`Open` 的显式 `Local` 语义保持，`Adapter.Local()` 返回实际配置的 `/32`。

## Darwin link-local fallback

目标为 IPv4 link-local 时，仅允许形态严格匹配现场证据的 Darwin 通用 `169.254.0.0/16` fallback 与新增目标 `/32` 共存：路由 Flags 为 `UCS` 或 `UCSI`，Gateway 为 `link#` 加十进制正索引，Netif 为 `en` 加数字，且同名现存接口的 Index 精确匹配、具备 Up/Broadcast、不具备 Loopback/PointToPoint。不要求 Running。此规则只按路由形态识别 fallback，不能鉴别所有人为仿造的路由；其它形式保守拒绝。

该例外只用于目标路由检查。本机接口地址前缀重叠、Local 冲突、其它包含目标的 host/subnet/VPN/网关路由仍拒绝；所有路由逐条检查，不因存在合格 fallback 而提前通过。只新增并回收自身接口上的目标 host route，不修改或删除系统 `/16`。添加失败不重试，也不撤销未成功添加的路由；后续失败只回滚已登记的自身资源。
