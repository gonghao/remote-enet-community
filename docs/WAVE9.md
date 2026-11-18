# Wave 9 · MGU发现、会话路由与Operator接线

基线e2045a4；用户确认继续。短码、UDP/DoIP、HTTP回连、编程逻辑继续后置。当前工作不自动commit/push。全部自动测试只用内存、net.Pipe或loopback；真实TUN/路由只由整合者在已授权手动台架执行。

## 用户行为与边界

Operator连接后可明确启用MGU direct link，执行一次只读MGU地址查询并准备目标/32路由。成功显示MGU IP、状态；不进行SSH使能、登录或刷写。禁用/断线/车辆网关改变/退出会关闭该路径、撤销所拥有资源。默认不开启，不对普通F20会话发额外MGU请求。

本轮保持JSON v1、CBOR/TLS/身份/邀请码格式。Direct link是会话内操作，不新增持久化设置。原有HSFZ广播及6801/6811呈现继续工作。MGU查询、权限或路由失败不终止健康HSFZ会话。Windows/macOS系统网络初始化仍需相应权限，不隐式提权；macOS原生验收可用独立测试目录下的headless Operator，不让root进程改普通用户真实配置。

车端只转发字节，所有MGU响应解析在Operator侧。现有CarSide目标子网边界保持，MGU响应不能扩大车侧访问范围。

## A · internal/mgu

只新增internal/mgu，复用hsfz/proto/transport，不改其它包或依赖。

```go
type Opener interface {
    OpenTunnel(context.Context, proto.OpenTunnel) (transport.Stream, error)
}
type Info struct {
    Address netip.Addr
    Netmask netip.Addr
    Third netip.Addr // wire value; semantic meaning unconfirmed
}
type Options struct {
    Tester byte // zero defaults to F4
    Timeout time.Duration // zero defaults to 5s, maximum 30s
    MaxFrames int // zero defaults to 64, hard upper bound 1024
}
func Discover(context.Context, Opener, netip.AddrPort, Options) (Info, error)
```

证据来自本地mgu_kit/mgu-go/internal/mgudiag/diag.go的ReadMguIP。只发送一条HSFZ诊断请求，src=Tester、dst=63、UDS=22 17 2A。只匹配src=63、dst=Tester的正响应62 17 2A 00，后续至少12字节解析三个IPv4；第一项必须为有效非loopback单播，后两项保留数值、不以第三项配置网关或扩大路由。Gateway端点允许loopback以支持隔离台架，MGU地址仍拒绝loopback/unspecified/multicast/broadcast/0段。

允许ACK、无关诊断帧与alive check；使用现有hsfz.AliveResponse回复，不猜测未知alive类型。UDS 7F 22 78仅在总时间与帧预算内等待，其他NRC返回明确错误。畸形/截断/超大帧失败；不扫描任意偏移认成功，不默认成功、不重发请求。上下文取消必须Close stream解除阻塞，计时包括OpenTunnel与所有读写，不泄漏worker。返回错误时Info为零值。标准库即可，无新依赖。

测试用真实session/stream或有界memory传输，验证精确请求、拆包/合包、ACK/alive/pending、错误来源/DID、NRC、超限、取消后其它流可用。

## B · app运行时、资源与共享准入

拥有internal/app、internal/operator、internal/l3，以及必要的internal/tunadapter自动本地地址选择。不得改desktop/ui/cmd/依赖或A目录。

```go
type DirectPhase string
const (
    DirectDisabled DirectPhase = "disabled"
    DirectDiscovering DirectPhase = "discovering"
    DirectStarting DirectPhase = "starting"
    DirectReady DirectPhase = "ready"
    DirectStopping DirectPhase = "stopping"
    DirectFailed DirectPhase = "failed"
)
type DirectState struct {
    Phase DirectPhase
    Address netip.Addr
    Local netip.Addr
    Adapter string
    LastError string
}
// app.Snapshot adds Direct DirectState.
type DirectDevice interface {
    packetio.Device
    Name() string
    Local() netip.Prefix
}
type DirectFactory func(context.Context, []netip.Addr) (DirectDevice, error)
// OperatorConfig adds DirectFactory DirectFactory (nil uses real native factory)
// and AutoMGU bool (CLI opt-in; GUI uses explicit operations).
func (*Runtime) ConnectMGU(context.Context, uint64) error
func (*Runtime) DisconnectMGU(context.Context, uint64) error
```

第二参数为期望session generation，零表示当前；过期请求必须在操作前拒绝，不切换重放到新会话。DirectDevice工厂须完整清理失败返回的部分资源。Caller请求ctx约束本次启用，但成功后的设备属于会话ctx；不因方法返回后请求ctx结束而销毁成功路径。Stop/断线/网关或NIC地址变更取消并汇合正在查询/启动的操作。旧manager回调不污染新代次状态。关闭先L3再设备，错误保留，不假报回收成功。

复用mgu.Discover，通过当前gateway及remote diagnostic port查询。启用前若已有HSFZ诊断/控制连接则返回忙，并原子阻止查询期间新工具连接；不能check后留准入竞态。建议Presenter.WithMaintenance(ctx, func(context.Context, netip.Addr) error)捕获当前gateway、预留worker，取消绑定Presenter寿命，Close汇合；禁止持Presenter锁执行回调。

Windows HSFZ与L3必须共享同一个exclusive.Gate，第一工具可开多个端口，其他工具不能跨路径绕过独占。新增operator.Options.Gate *exclusive.Gate（只有Exclusive时生效，nil保持旧行为），L3.Options.Admit复用其Admit。MGU内部查询通过maintenance，不附着外部owner身份。Mac仍不宣称PID解析可用。

L3.Options可增加Observer tunnel.Observer，透传successful-write增量并确保所有失败/关闭End调用一次；与现有metrics合并，peer-only测试仍不进车辆统计。

默认工厂选择与现有接口、非默认路由、MGU目标均不重叠的本地TUN /32，可用少量固定候选（benchmark/documentation IPv4）；不做HTTP IP镜像承诺。原tunadapter.Open的显式Local契约保持，可新增OpenAuto(ctx, targets)及Adapter.Local()。只在创建前冲突时尝试下个候选；权限/系统配置错误不要盲目重复。错误须可分类（例如ErrConflict），预检后仍执行原严格校验，不复用/删除已有设备，不接管默认路由。

运行时模拟测试必须覆盖：无MGU/NRC不影响HSFZ，maintenance拒绝并发工具、共享owner跨网关/ECU，启用/禁用/Stop和新代次并发，失败回滚，旧源IP/目标不重放，流量与快照隔离。注入工厂用packetio内存设备，不运行真实原生代码。

## 整合者 · desktop/ui/CLI与验收

新增会话操作按钮及状态展示，不改settings schema。新UI Action常量追加，显式Complete回执，不让旧错误解除pending；Client界面无Operator新动作。Controller捕获当前Runtime及Generation，运行操作计入该次run的workers，Stop取消并汇合，迟到完成不覆盖新run。错误为固定产品文案，Runtime诊断字符串按现有规则脱敏；不将邀请码/数据内容写日志。

生产UI展示MGU IP/Direct状态及启用、禁用，不暴露DID等实现细节。权限错误可显示所需网络配置权限，不能自动提权或假装已支持。CLI增加显式--mgu-direct（默认false），启用AutoMGU并输出Direct状态；不改变旧入口默认行为。

先用假HSFZ/DID和模拟MGU验证完整数据通路；原生台架可用隔离Linux network namespace，让fake gateway和MGU拥有真正不同的车侧IP，Client仍普通权限。原版mgu_kit验收分开检查本地广播回应来源、6801地址查询、真实目标IP路由和22访问。SSH认证只在受控测试服务或用户授权MGU上执行；不把banner当登录。

按用户既有授权采用Paseo独立worktree并行，最后DeepSeek只读复审；子代理不改系统网络、不访问用户Mac/VM、不sudo、不commit/push。整合者再做Windows原生与macOS手动验收，未知能力如实保留边界。

## 当前整合与原生结果

已交付mgu只读探测、会话Direct manager、共享准入与维护预留、L3成功写入统计、自动本地/32候选，以及Controller/UI/CLI接线。启用与禁用使用独立Action完成回执；禁用有独立的保留准入槽位。JSON v1及CBOR、身份格式保持。

2026-11-17 Windows原生GUI台架通过：普通Linux Client明确绑定测试veth169.254.91.1/29；隔离namespace中gateway=169.254.91.2、MGU=169.254.91.3。GUI启用后日志只有一次22172A读请求，响应给出MGU地址；Wintun安装目标169.254.91.3/32，选中本地198.18.0.1。Windows ssh.exe用独立测试密钥及预置host key，经TUN/加密session/普通Client，完成受控sshd公钥认证并收到固定ForceCommand结果。不是只检查banner，也不是真实MGU验收。

GUI禁用后目标路由消失，原HSFZ仍读取到模拟VIN，Hub仍监听；随后正常结束Client和Operator，临时防火墙规则与计划任务均回收。假车台架的网卡、路由与namespace也已核验回收。测试不改真实SSH授权文件，不使用用户真实私钥。

普通测试已通过全仓CGO=0 vet/test及相关CGO=1 race，Windows amd64/macOS arm64纯Go构建通过；UI默认Operator尺寸仍可操作两组控件，连接测试使用两列指标以避免截断。Mac原生utun及原版mgu_kit发现服务随后完成单独手动验收，见下文；该结果不由跨构建或Windows结果代替。

### 复审及Mac首次尝试

DeepSeek只读复核后补齐两处：Controller成功回执核对当前Direct阶段，避免同代次网关变化后宣称Ready；MGU ACK仅按信封/帧预算读取并忽略正文，真正成功仍严格匹配来源、DID与地址。另修复默认native工厂的typed-nil接口转换：失败返回的*Adapter(nil)必须先规范化为nil接口，避免错误清理调用nil接收者Close导致panic；取消ctx和非法目标的默认工厂回归在任何系统配置前验证此边界。

Mac用户用独立临时目录启动headless Operator，实际LAN地址为198.51.100.159，测试机已验证可回连。普通Client已握手并完成MGU地址只读请求（11字节请求、35字节回应），随后Mac监听消失。退出现场终端错误尚待用户回报，不把已找到的typed-nil错误直接认定为首次现场退出根因。

修复版wave9.1保留同一测试身份复验后，HSFZ保持connected，周期RTT持续采样，MGU地址查询返回169.254.91.3；Direct明确失败于`target conflicts with route 169.254.0.0/16`，没有退出。该错误发生在原生设备创建前，因此本次尚未验证utun、目标路由或MGU访问。后续需结合Mac的`route -n get`与`netstat -rn -f inet`原始结果，区分通用link-local路由与实际网络占用；不得以删除系统`/16`、忽略全部较宽路由或修改默认路由绕过预检。

用户随后提供原始路由证据：目标查找命中en7上的169.254.0.0/16（UP/DONE/CLONING/STATIC）；表中同时有`169.254 link#30 UCS en7`和`169.254 link#15 UCSI en0`，以及不匹配当前目标的广播host条目。wave9.2据此将内部路由记录扩充为Prefix/Gateway/Flags/Netif，保留重复条目并拒绝缺失元数据的Darwin路由行。仅对Darwin目标检查允许精确形态的通用link-local `/16`：UCS/UCSI、link#索引与现存en数字接口匹配，接口Up/Broadcast且非Loopback/PointToPoint。Local与本机接口前缀检查保持，所有其它匹配目标的host/subnet/VPN路由仍拒绝；只按形态识别，不声称能够鉴别人为仿造的同形路由。

现场夹具、并存冲突、顺序无关和添加失败回滚回归已通过；不改原有add/delete/undo计划，不删除系统 `/16`。聚焦vet/test/race、全仓CGO=0 vet/test、Mac与Windows构建通过；DeepSeek对路由解析、放行边界和回滚作聚焦只读复核，未发现确定问题。

Mac用户复验wave9.2于11:44:13报告Direct Ready：MGU=169.254.91.3、Local=192.0.2.1、Adapter=utun11，error为空，HSFZ继续connected。后续`route -n get`明确命中utun11上的目标host route。原版mgu_kit的`DiscoveryService.AutoDetect`经临时headless wrapper、未修改原有发现实现，实测广播返回网关198.51.100.159:6801及模拟VIN，再读得MGU=169.254.91.3:22。Mac ssh使用独立测试密钥、严格固定host key和BatchMode，经utun11完成隔离sshd认证并收到预期ForceCommand结果。

Mac用户随后在11:47:44按Ctrl+C，Operator进入stopped并以状态0退出；目标169.254.91.3恢复命中en7上的169.254.0.0/16。退出后`ifconfig utun11`明确报接口不存在，netstat只剩原有en7/en0两条 `/16` 及广播host条目，本次目标 `/32` 已撤销。测试机同时确认Mac的47000不再监听。

Mac原生初始化、原有发现服务、完整SSH数据通路和正常退出回收全部通过。测试机随后停止普通Client与隔离假车，确认本轮veth、路由、namespace和服务进程均已回收，无cleanup errors；临时目录和测试密钥按台架策略保持私有供审阅。此结论仅覆盖隔离假车，不覆盖macOS GUI提权流程、mgu_kit完整GUI或真实MGU。

最终当前源码全仓CGO=0 vet/test、CGO=1 race、Windows/macOS纯Go构建通过；没有自动commit/push。临时假车通过新namespace和独占所有权veth隔离，已增加真正广播发现fixture，用于后续原始mgu_kit服务对照；不修改生产Client网络权限或诊断内容策略。
