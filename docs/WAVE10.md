# Wave 10 · Operator权限、双端预览包与诊断

基线cad9153；用户确认推进。使用Paseo独立工作区并行开发，最后OpenCode DeepSeek只读复审。开发阶段不自动commit/push，提交按用户明确指令执行。代码、注释及产品文案English，文档中文；CGO_ENABLED=0，无新增依赖优先。所有自动测试仅内存/net.Pipe/loopback；子代理不得sudo、访问VM/Mac、创建真实TUN/修改路由或运行提权助手。

## 目标与边界

普通用户从Operator应用启动，在界面明确请求MGU直连后完成所需系统授权；GUI及身份/信任文件保持普通权限。Windows显示真实管理员能力及可操作的提示。仍只做发现/呈现/透明转发，不实现车辆编码、SSH使能或刷写逻辑，不改变CBOR、TLS或现有settings JSON v1。

Mac采用按次启动、退出回收的受限网络助手，先验证纯Go可行性。助手只接收本次单一目标IPv4，复用tunadapter.OpenAuto预检和资源所有权；不接受shell命令、任意本地地址、路由前缀、任意文件读写或身份目录。IPC需校验本机对端凭据、限定目录/连接/帧长/预算；助手不读取邀请码、密钥、信任或GUI设置。取消、拒绝授权、GUI退出/崩溃、IPC断开都须回收资源，清理失败须传播。不得通过让整个Mac GUI以root运行替代此目标，不安装常驻root服务。

Windows本轮检测权限与助手/驱动能力并给出明确提示，不改变用户系统权限策略。真实系统授权和打包产物是否可用必须分开验收，跨编译不等于原生运行。

## A · 网络权限助手

拥有internal/netpriv/、cmd/renet-net-helper/；其它目录由整合者接线。先向整合者报告设计与纯Go可行性，再完成实现及模拟测试；不得自行创建原生网卡或提权。

拟定接口（如必要调整先报告，Root统一协调）：

```go
type Options struct { HelperPath string }
type Status struct {
    Platform string
    Elevated bool
    HelperAvailable bool
    AuthorizationRequired bool
}
func Inspect(Options) Status
func Open(context.Context, []netip.Addr, Options) (*Device, error)
// Device implements packetio.Device, Name() string, Local() netip.Prefix.
```

Device失败时必须返回真正nil指针，不以typed-nil接口掩盖失败。Windows已具备权限时可复用原生OpenAuto；权限不足明确返回os.ErrPermission。Mac具备root权限的CLI保持原生路径；普通GUI通过系统授权启动随包助手，以受限IPC提供设备I/O及生命周期。Linux不引入授权流程。root helper入口默认/无效参数无副作用；只接受内部IPC参数。

助手使用已有tunadapter的单目标/32能力；所有路由/地址预检、Darwin fallback及逆序清理不复制另一份实现。私有IPC的内容和目标不能通过日志/命令行错误泄露。packet I/O要有固定内存与并发预算，单次ctx取消不破坏设备，Close幂等并汇合。GUI进程消失后助手必须结束，不依赖再次启动GUI清理。

## B · 双端打包

拥有tools/build-desktop.py、tools/test_build_desktop.py、tools/build-macos-client.sh及docs/PACKAGING.md；不修改CI/依赖/入口源码。

标准库Python统一构建Windows amd64/macOS arm64的Client和Operator。CLI支持--platform windows|darwin|all、--role client|operator|all、--output、--version、--wintun-dll。保留build-macos-client.sh原调用语义。使用mygo_nowebview、CGO=0、独立产品/单实例标识及明确的版本/commit/dirty标记。Mac Operator.app包含Contents/MacOS/renet-net-helper；该入口由A提供，未出现前可先实现脚本及纯打包测试，不写桩。Windows Operator包必须包含经固定官方SHA256和PE架构检查的Wintun DLL，缺失或不符失败；不自动下载/安装驱动，不提交DLL。

产出独立ZIP、SHA256与manifest，记录role/platform/architecture/version/revision/dirty/helper及DLL摘要、签名状态。Mac检查Mach-O/最低系统版本/可执行权限/ad hoc签名标记，区分Developer ID及公证状态；Windows GUI用-H=windowsgui。仅构建不启动程序。临时目录限定本次所有权、输出避免半成品；普通测试不联网下载依赖或运行真实系统操作。Root负责CI接线与真实产物复验。

## C · 脱敏诊断

拥有internal/diagnostics/，不改desktop/UI/cmd。纯构建器，不读磁盘、环境、剪贴板或拨号；Root负责显式复制按钮。

```go
type Permission struct {
    Platform string
    Elevated, HelperAvailable, AuthorizationRequired bool
}
type Input struct {
    Version, Role string
    Runtime app.Snapshot
    Running, Stopping, Testing, DirectWorking bool
    Permission Permission
}
func Build(Input) ([]byte, error)
```

输出独立JSON诊断schema v1（不是settings格式）。只投影固定白名单字段：版本/平台、角色/阶段/代次、权限能力、连接/计数/RTT/速率、Direct阶段和错误是否存在等。不得直接marshal Runtime；默认不含IP、VIN、设备/对端指纹、MAC、路径/目录、网卡/适配器名称、邀请码、原始错误、Events或Tunnel Target。动态枚举只接受已知值；未知值归一化，不把输入带进错误文本。版本字符串严格有界验证，数字拒绝NaN/Inf；总输出<=64KiB。输入快照不可被修改。测试把秘密注入所有可变字符串、验证失败不返回部分内容，并覆盖未知枚举/边界/部分指标等。

## 整合者

负责desktop.Options的DirectFactory注入、desktopui启用路径、权限提示和显式Copy diagnostics按钮；补接口契约与无窗口交互/生命周期测试。保持逐Action完成隔离，Stop和禁用独立槽位可取消授权与setup；UI线程不执行阻塞授权/磁盘或网络操作。根README/DESIGN中旧Wave6/TUN后置描述同步更新，CI校验打包脚本和产物。

整合后全仓gofmt、CGO=0 vet/test/build、相关CGO=1 race、Windows/macOS纯Go构建，再由Root执行Windows原生及用户Mac手动验证。故障场景包括拒绝/取消授权、重复启用、IPC断开、退出/崩溃、VPN/路由冲突；无法原生执行的部分明确保留，不以模拟测试替代。

签名、公证、自动更新、真实WAN、实车只读以及HTTP回连分别留待后续；本轮先交付可核验预览包和完整权限生命周期。

## 实现与验收结果（2026-11-17）

A/B/C工作包已整合，Root补齐desktop工厂注入、权限提示、显式诊断复制与打包CI。独立复审后进一步收口：maintenance仅覆盖有界MGU查询，系统授权等待期间HSFZ可继续使用；L3暴露Done/Err，运行时在一秒tick和启用前检查存活，故障转Failed并回收，清理不确定时拒绝不安全重启。正常禁用及旧栈迟到完成不会误报新链路故障。cmd.Start取消优先返回ctx错误。

IPC写成功使用逐包W确认，必须等native完整写入；调用方取消后worker仍收敛原确认。父端16包读队列满时taildrop，不阻塞控制帧。确定性慢写、突发、取消、断链、清理错误及资源代次回归通过。DeepSeek完成全范围只读复审及R1/R2/R3聚焦复核，未发现剩余确定缺陷。

| 原生场景 | 结果与边界 |
|---|---|
| Windows普通权限 | GUI显示需要管理员能力，MGU启用失败后HSFZ仍connected，目标路由为0；该部分使用wave10.0候选包 |
| Windows管理员 | wave10.1包Ready，官方Wintun建立目标/32；临时Go SSH探针完成固定host key校验、公钥认证和1048576字节数据SHA256校验，LAN基线11ms、TUN本次17ms，仅为本机台架记录 |
| Windows诊断与退出 | 实际点击Copy diagnostics后捕获schema v1，elevated=true；Disable后DirectDisabled、目标路由0且Hub仍监听，Quit后进程及TCP/UDP监听退出，临时计划任务和防火墙规则移除 |
| Mac普通用户GUI | wave10.1报告elevated=false、helperAvailable=true；按次助手完成utun11初始化。会话重连后Direct回Disabled且不自动重放，用户显式重新启用后继续验收 |
| Mac数据通路 | 原版mgu_kit发现服务经headless wrapper发现网关和MGU；系统SSH完成认证及1MiB固定数据SHA256核对，使用真实utun11路径 |
| Mac停用/失败 | Disable后目标恢复en7的原有169.254/16、助手退出，GUI仍在；随后取消授权测试步骤回报DirectFailed且同一代HSFZ继续connected。用户未另外描述弹窗细节，不据此推断授权缓存机制 |
| Mac GUI崩溃 | 仅对当前用户及本次临时目录的精确Operator进程SIGKILL，脚本确认同路径助手退出、utun11消失、目标恢复原en7路由 |
| 测试机清理 | 普通Client、受限LAN基线中继、假车veth/路由/namespace与服务进程全部回收，无cleanup errors；测试目录及临时密钥保留私有供审阅 |

Windows系统OpenSSH在后台测试方式下出现认证前或输出阶段超时，普通LAN中继基线也复现，故未将它归为L3缺陷或放宽数据断言；改用独立Go SSH探针分别完成LAN与TUN认证及完整数据校验。没有修改系统SSH配置或关闭进程独占来掩盖问题。Mac进程检查临时脚本最初使用了错误的ps绝对路径，已改为/bin/ps；此前SSH字节数及哈希断言已独立通过。

全仓CGO=0 vet/test、相关CGO=1 race、打包脚本10项离线测试及四份真实跨编译包校验通过。包内及外置manifest一致，分别提供ZIP SHA256，Mac含真实helper，Windows Operator含固定摘要官方DLL。候选版本为0.1.0-preview.wave10.1-worktree；尚未Developer ID/公证或应用签名。新CI已配置打包校验及artifact上传，远端CI与artifact以包含本轮代码的推送后运行记录为准，不由本地跨编译推定通过。

本轮不宣称Linux原生TUN、真实WAN、真实车辆、mgu_kit完整GUI、HTTP回连或正式分发更新已经验收。
