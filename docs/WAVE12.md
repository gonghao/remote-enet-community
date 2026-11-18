# Wave 12 · Operator HSFZ 抓取与JSONL

基线34024cd。用户明确要求Operator可抓取记录HSFZ，已选择Operator内查看帧+JSONL导出，不做PCAP。按既有Paseo并行及DeepSeek只读复审；提交和推送按用户明确指令执行。Go1.27.3、CGO=0，无新依赖。代码/日志English，文档中文。

## 边界与用户行为

抓取默认关闭、只在Operator显式开启，用户选择新的本地JSONL文件；Start成功后保存双向原始流块和帧索引，Stop/结束Runtime/退出会汇合并关闭文件。原有配置JSON v1、CBOR/TLS、身份/邀请格式不变。Client仍载荷透明、不解析帧；Hub只搬密文。MGU SSH/L3数据、peer-only测试、邀请码与私钥不进入抓取。

覆盖Operator呈现的诊断/控制TCP、UDP发现请求/实际应答，以及内部MGU只读地址查询。开始前已存在的TCP连接不在中途猜帧边界，需诊断工具重连；停止后旧句柄不得写入下次新文件。支持未知control/空body/拆包合包/半关闭/截断的如实记录，不扫描任意偏移“恢复成功”。不解析UDS业务，不做编码刷写。

JSONL是应用层字节抓取，不是IP/TCP抓包。记录中可能包含VIN和诊断载荷，只保存用户选定的本地文件，不进入匿名诊断/普通事件日志。GUI简短说明用途及新连接范围，不增额外确认流程。数据观察必须不阻塞转发；队列/文件/连接/实时列表有硬上限，超限/丢录/部分帧必须明确标记，不声称无损。磁盘失败停止录制并显示固定错误，健康会话继续。

## A · internal/hsfzcapture

仅拥有internal/hsfzcapture/。标准库实现独立有界Recorder/增量帧索引/JSONL writer，不依赖app/desktop/UI/tunnel，避免循环。先落真实实现API并及时通知B/C，不能用桩满足编译。

```go
type Direction string
const (
    ToVehicle Direction = "to_vehicle"
    FromVehicle Direction = "from_vehicle"
    ToOperator Direction = "to_operator" // UDP request
    FromOperator Direction = "from_operator" // UDP reply
)
type Metadata struct {
    Generation uint64
    Network string // tcp or udp
    Channel string // diagnostic, control, discovery, mgu-query
    Local, Peer, Target string
}
type Options struct {
    MaxFileBytes int64
    QueueBytes int
    MaxConnections int
    RecentFrames int
}
func New(Options) *Recorder
func (*Recorder) Start(context.Context, string) error
func (*Recorder) Stop(context.Context) error
func (*Recorder) Snapshot() Snapshot
func (*Recorder) Begin(Metadata) *Connection // nil when disabled or capacity rejected
func (*Recorder) Datagram(Metadata, Direction, []byte)
func (*Recorder) Mark(label string, generation uint64) (Marker, error)
func (*Connection) Record(Direction, []byte)
func (*Connection) End(error)
```

Snapshot包含Active/Stopping/Incomplete bool、Path string、StartedAt/StoppedAt time.Time、BytesWritten/ObservedBytes/RecordedBytes/Frames/DroppedEvents uint64、ActiveConnections int、Error string（固定码）、Recent []Frame。Frame包含Sequence/Connection/Generation/Offset uint64、At time.Time、Network/Channel string、Direction、Control uint16、BodyLength uint32、HasAddresses bool、Source/Destination byte、PrefixHex string、PrefixTruncated bool、Status string（complete/partial/gap/oversized）。快照集合独立，字符串有界。帧PrefixHex包含最多256字节原始HSFZ header+body前缀；完整数据在raw data事件中，不误把前缀称完整帧。

默认硬预算建议：文件64MiB、队列4MiB、同时追踪连接128、最近帧200；Options零值用默认，明确最大值并允许测试小预算。callback只复制有界数据/更新计数/非阻塞入队，绝不读写磁盘；大块按有限大小拆分或明确缺口，队列容量以实际内存计。frame解析worker只保留6字节header及256前缀，不按声明长度分配body。超长声明停止该方向解码并记录oversized，仍可记raw；EOF如实partial。丢队列后用方向offset识别gap，停止该方向解码且不猜重同步；map/帧缓存/历史均有上限，End事件丢失也不能泄漏无界parser状态。

JSONL独立schema version1，事件类型至少capture_start/connection_start/data/frame/gap或incomplete/connection_end/capture_stop；带capture代次、时间、连接/会话代次、网络/通道/方向/offset及raw data hex，未解释字节保持原样。捕获包络不含邀请/密钥。错误只固定类别，不序列化任意err.Error。Metadata来自Operator，长度和控制字符有界处理。最终文件与目录处理沿用目录句柄方式，允许OS正常目录别名，最终拒链接/目录/已有文件（不覆盖旧抓取），0600/Windows目录ACL。写入超限保留完整JSONL行并明确limit，不写半行后声称成功；固定磁盘错误状态，后续新录制需旧worker关闭。

Start的请求ctx取消不能在成功返回后结束录制；Stop先停止新事件，再drain/flush/close，ctx取消可放弃等待但后台必须继续可汇合，重复Stop幂等。文件worker的生命周期/错误/End及旧Connection epoch归属需要可审计。承认同步文件系统syscall不可强制取消，不用无界goroutine假装硬超时。

测试只内存/临时文件：拆包合包/0长度/unknowncontrol/地址字段/UDP、截断/超大声明/gap不误解码、队列容量/文件限制、磁盘失败不影响Record返回、并发/Stop/新旧epoch、路径别名/链接/已有文件拒绝、所有数据byte-for-byte。测试应验证行为和资源汇合，不做无理由百轮。

## B · 透明转发接线

拥有internal/tunnel/、internal/operator/、internal/app/，必要时新增这些目录内测试；不得改A/UI/desktop/CLI/依赖。复用A包的实际实现，不复制另一套解析器。

tunnel.Stats可新增Payload Tap，Tap接口In([]byte)/Out([]byte)，只接收目标Write成功的p[:n]（含partial write+error），与现有Counter方向一致；nil零开销，接口必须并发安全/立即返回，不解释内容。两方向都不可绕过writer hook，保留半关闭与取消汇合。

operator.Options新增Capture *hsfzcapture.Recorder及可选CaptureGeneration func() uint64，Presenter在准入后为新TCP Begin（Metadata.Generation/Channel/Local/Peer/Target），成功写入方向映射FromVehicle/ToVehicle，结束End固定error分类。UDP在本地socket读到的数据报和真正成功发送应答记录ToOperator/FromOperator，完整帧/来源/端口保留，注明本地缓存发现应答不代表新车辆流量。没有Capture时原逻辑不变。

app.OperatorConfig新增Capture *hsfzcapture.Recorder，不在app Snapshot或metrics中塞payload。配置交给Presenter，generation取实际本次会话，不把新accept归旧代次。内部MGU查询使用专用Opener/Stream透明包装，记录channel=mgu-query、stream.Read实际收到与Write成功字节，不影响原始流所有权/CloseWrite/取消；不要记录其它session控制或L3/SSH流。Root负责Recorder Start/Stop/文件策略，B不在数据路径创建文件或停止全局capture。

测试真实Pipe/Presenter/session及fake gateway：双向完整bytes与帧、diag/control/UDP/internal query覆盖、关闭/半关闭/gate拒绝不伪记录、启用前老TCP不被中途附着、旧capture不写新文件、capture超限/失败后普通转发继续、generation/Update隔离。

## C · 原生视图

仅拥有internal/ui/，A包出现后可作为只读依赖复制。State新增Capture hsfzcapture.Snapshot；Actions新增StartCapture/StopCapture func()；Action追加ActionStartCapture/ActionStopCapture。开始/停止独立latch，旧Error/无关Complete不能解除；显式Complete对应动作。接口其它含义不变。

Operator增加独立HSFZ capture页面入口（可在Log页按钮导航，避免主header拥挤），Client没有入口/按钮。页面顶部状态/路径/字节/帧/丢录和Start/Stop；Start仅Running且非Stopping、当前未active/stopping；Stop仅active/stopping且对应请求未pending。说明默认关闭、记录诊断载荷到本地以及只捕获新TCP。后台worker负责I/O，视图不访问文件/网络或解释字节。

最近帧列表最多200，按时间/连接/代次/方向/control/body长度显示，可按方向或连接筛选，选择后查看原始前缀hex和src/dst；大帧明确前缀截断，partial/gap/limit有可见状态，不把RawPrefix称完整payload。只在用户显式点击时复用Actions.Copy复制当前显示hex。控制字符/过长文本布局安全，默认尺寸主操作可见，缩放滚动、无障碍标签齐全。新页Back始终可达。

headless实际点击/过滤/选择/复制/开始停止完成隔离/旧state/帧列表容量/Client角色隔离/长hex/小窗口测试，不引入新依赖。不修改匿名诊断的文字或把payload混入普通Log。

## Root · Controller/CLI/宿主与验收

负责desktop持有Recorder、OperatorConfig注入、当前run准入、StartCapture(ctx,path)/StopCapture(ctx)、snapshot深拷贝与Stop/Close汇合；Save/状态不持久化capture开关和路径。录制属于一次Runtime运行，可跨自动重连但必须按generation分开，不跟随已结束run。

desktopui新增保存文件对话框（新JSONL，不覆盖既有文件），I/O放后台动作；取消对话框正常完成不报成功录制。StopCapture使用独立coalesced槽位，不被普通动作4槽位堵住，GlobalStop仍可取消开始。每次完成在UI线程读取最新快照，沿用requestID隔离。

CLI operator加显式--capture-hsfz PATH，默认不抓。失败创建文件应返回明确错误；退出先结束Runtime转发再Stop汇合抓取。匿名diagnostics保持既有白名单，Capture数据永不进入邀请/普通事件/console logs。文档说明JSONL解读及丢录/新连接限制。

最后全仓gofmt/vet/test、相关race及双平台纯Go构建；端到端fakeZGW验证真实字节与帧、控制/发现及内部query不重计/串录；DeepSeek只读复审。Root可在Windows原生操作GUI做短验收，子代理不访问VM、不sudo、不创建实际TUN或读真实状态/密钥、不commit/push。

## 完成与验收

- A/B/C及Root宿主接线已完成；使用说明和JSONL schema见[CAPTURE](CAPTURE.md)。没有新增依赖或更改Settings/网络序列化格式。
- Linux：全仓gofmt、vet、普通测试通过；hsfzcapture/tunnel/operator/app/desktop/desktopui/ui/renetd相关race通过。真实TLS/session/fake ZGW回归验证TCP两个通道、UDP、内部MGU查询、代次及完整字节；peer-only测试不进入抓取。
- DeepSeek两轮只读复审通过。收口了连接结束观测时间、Stop与晚到End的冻结、UDP完整发送条件、无变化时避免额外发布及初始写入失败提示。慢writer/屏障回归验证记录器不持磁盘I/O状态锁、不重写已冻结的连接结束信息。
- Windows amd64原生：记录器全包、app全包及desktop/desktopui抓取聚焦测试通过。修正两处测试平台假设：Windows文件mode不代表Unix权限；真实RTT允许0但必须有有效样本。
- Windows普通用户GUI：独立配置、loopback假车与真实Client，通过原生保存框选择新文件。诊断TCP 11+11字节、控制TCP 6+7字节、UDP发现6+56字节，共6帧97字节；导出按channel/direction重组逐字节一致。界面显示observed=recorded=97、dropped=0、incomplete=false，连接过滤、选帧地址/前缀和Copy动作通过。取消原生选择框返回未录制；Stop capture关闭文件但Runtime继续监听。
- 最终Windows预览包另复验2帧22字节，录制中直接Quit后JSONL存在capture_stop、字节一致、程序退出码0且监听释放。测试进程和本轮两个scheduled task均已回收，没有创建TUN、改变路由或增设防火墙放行规则。
- 双平台双端纯Go预览包生成及包校验通过，版本`0.1.0-preview.wave12.0-worktree`。Windows最终Operator可执行文件SHA256为`7e4a1696e3e71f6f4d129b0bc6771ba0bdbf203d5b40a6cef96678b56bf4f1ac`。产物位于`dist/`和`/tmp/remote-enet-wave12/preview/`，本轮原生证据位于`/tmp/remote-enet-wave12/native/`及Windows独立`remote-enet-test/wave12`。

macOS本轮仅交叉构建和包校验，未宣称原生GUI抓取验收完成；未做真车、真实WAN或IP层抓包验证。原生验收记录生成于提交前，对应提交的GitHub CI结果另行核对。

## 快捷标记追加 · wave12.1

用户要求在诊断工具操作前快速标记。新增`Marker{Sequence,Generation,At,Label}`、`Snapshot.Markers/RecentMarkers`及独立JSONL `marker`事件；它与原始data共用有界队列，但不改变字节offset、载荷计数或帧解码。空注释自动编号；注释单行、最多256 UTF-8字节；队列满返回固定错误，已准入但未成功写入的标记不计为已记录。

`Controller.MarkCapture(ctx,label)`直接在UI回调中同步完成有界内存准入，避免普通异步队列改变按键时间。标记代次取Runtime最新Snapshot；注释不进入普通Events、匿名诊断或Settings。UI最近标记列表有200条上限，可与方向/连接条件一起筛选首字节时间不早于标记的帧。

仅录制期间注册全局`CmdOrCtrl+Shift+M`，Operator各页面顶部提供Insert marker。注册失败不反复重试或影响录制，按钮仍可使用；停止、故障、退出释放自身快捷键。旧录制的迟到回调不能写入新录制，快捷键不激活或显示Operator窗口。

追加验证：全仓gofmt/vet/test、四包hsfzcapture/desktop/desktopui/ui的race通过；双平台双端`0.1.0-preview.wave12.1-worktree`预览构建与校验通过。单元回归覆盖跨帧中间插标、原始字节顺序和计数、磁盘阻塞下快速准入、队列满、Stop并发、编号重置、快照所有权、时间筛选、满动作队列、快捷键冲突/旧回调及注释脱敏。

Windows原生标记测试通过；独立普通用户Operator录制期间，向另一个独立测试程序前台发送Ctrl+Shift+M，观察到带预填注释的标记1；按钮插入自动编号Marker 2。文件两条marker的时间/代次/注释均一致，capture_stop完整、载荷字节仍为0。停止录制后，另一测试程序成功注册同一快捷键，证明已释放；两程序均正常退出，测试task和监听已回收。证据位于`/tmp/remote-enet-wave12/markers/`和Windows独立`remote-enet-test/wave12/markers`。macOS追加仍仅交叉构建，未宣称全局快捷键原生验收通过。
