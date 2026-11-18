# 桌面控制器

本包依赖 settings、app、相关内部数据与信任包及标准库，不依赖 mygo。`New(ctx, Options)` 加载配置；通用模式中未指定的路径使用 `settings.DefaultPaths()`。`StateDir/client` 与 `StateDir/operator` 分别交给 app 管理身份和信任，配置文件独立保存。网卡列表通过 `RefreshNICs` 显式刷新，缺失的固定选择保留在 Config 中。

## 接线接口

```go
type Options struct {
    ConfigPath, StateDir string
    FixedRole string
}
func New(ctx context.Context, opts Options) (*Controller, error)
func (*Controller) Snapshot() State
func (*Controller) Subscribe(context.Context) <-chan State
func (*Controller) Save(ctx context.Context, cfg settings.Config) error
func (*Controller) RefreshNICs(context.Context) error
func (*Controller) StartClient(ctx context.Context, invitation string) error
func (*Controller) StartOperator(context.Context) error
func (*Controller) IssueInvite(ctx context.Context) (string, error)
func (*Controller) TestLink(context.Context) error
func (*Controller) StartCapture(ctx context.Context, path string) error
func (*Controller) StopCapture(context.Context) error
func (*Controller) MarkCapture(ctx context.Context, label string) (hsfzcapture.Marker, error)
func (*Controller) Stop(context.Context) error
func (*Controller) Close(context.Context) error
```

State 的字段遵循 `docs/WAVE5.md`。UI 在订阅更新后，先把 `State.Config` 交给 `View.SetConfig`，再调用 `View.SetState`；UI 线程调度与剪贴板由整合入口负责。除 Snapshot/Subscribe 外，操作应由 UI 的异步任务调用，尤其不能在 UI 线程等待连接测试或停止。

`StartClient`/`StartOperator` 返回 nil 表示已准入异步启动，不表示已经连接或监听。请求 ctx 只约束准入，Runtime 使用 New 的父 ctx；请求返回后取消不会停止 Runtime。运行结果从 State 的阶段、Error 和 Runtime.LastError 读取。Operator 启动前要求已保存的 Endpoint、Bind；不支持的平台显式开启 Exclusive 会直接返回 `exclusive.ErrUnsupported`。

`Options.FixedRole` 为空时保留通用库行为；固定应用只允许 `client` 或 `operator`。固定模式使用 `settings.DefaultPathsForRole` 补全缺省路径，ConfigPath、StateDir 仍可分别显式覆盖。文件不存在才使用对应角色的默认配置；存在文件继续严格解码，角色不匹配返回可匹配的 `ErrRole`，不静默改角色或重写文件。Save 不得改变固定角色，错误角色的启动、Client 生成邀请均被拒绝。两端配置各自保存，StateDir 保持同一个根目录，再由控制器附加角色目录；不自动导入旧 shared settings.json，不迁移或删除既有配置、身份和信任记录，JSON v1 schema 不变。

## 生命周期与并发

每次启动创建一个全新的 app.Runtime。控制器在自己的锁下串行准入 Start/Stop/Close；运行、停止和 worker 汇合期间，Save/Start 返回 `ErrBusy`，不隐式停止。配置与邀请的磁盘操作使用独立的可取消门锁，不在状态锁内进行 I/O。

Stop 先取消运行，再等待 Runtime、快照 pump、当前连接测试和运行中的邀请请求退出。取消 Stop 的等待 ctx 只会结束等待，后台清理继续；在清理完成前仍是 Running/Stopping，不能重启。Stop 结束会停止 Operator 监听。无运行时，Stop 也会清除临时邀请码并取消、汇合正在生成的邀请码。Close 立即永久拒绝新请求，并等待所有已准入操作退出；New 的父 ctx 取消会自动执行相同清理。后续 Close 可以继续等待之前取消等待的清理。

每次 Stop（包括空闲时）都递增 stop epoch。Start/IssueInvite 在准入的同一状态锁内捕获 epoch，取得 I/O 门锁后再核对；被 Stop 越过的排队请求返回 `context.Canceled`，不启动 Runtime、不生成邀请码，也不覆盖停止状态。Stop 不等待全局操作队列；并发 Save 的原子文件替换继续按既有语义完成。正常停止或自然成功结束发布 `Notice="Runtime stopped."` 并清空 Error；空闲 Stop 也发布该状态，避免残留旧邀请文案。运行失败保留错误状态，过去的操作失败仍可查看事件历史。

连接测试捕获当前 Runtime 实例及 `app.Snapshot.Generation`，通过 `TestLinkForGeneration` 执行。断连、换代和停止都会取消旧测试；旧代次的部分结果也不提交。旧测试尚未返回时保持 Testing，拒绝新测试。成功、不支持、忙、超时、取消与其他失败分别返回真实错误并显示状态。测试只向已协商的 renet 对端发送数据，使用 linktest 的默认限额；不发往车辆，也不计为车辆隧道流量。

Subscribe 容量为 1，慢订阅覆盖未读状态；发送不阻塞控制器。取消订阅或 Close 关闭通道；最终状态仍可能留在通道中。每个订阅和直接 Snapshot 独立复制 NIC、Events 和所有嵌套指标切片、LastError 指针。控制器阶段/操作事件最多 200 条，不接收任意远端日志或隧道内容。

## 保存与秘密

Save 使用已保存配置作为运行基线；成功更新 Config，清空 Error，发布 `Notice="Settings saved."`，相同值保存也有回执。失败保留原 Config，发布 Error 并清空 Notice。

`Config.Client.Peer` 是控制器管理的只读字段，Save 始终合入当前 baseline 的 Peer，避免旧表单覆盖刚完成的配对。首次导入已配对指纹可由 settings 文件提供；更换配对通过新邀请码进行，不能通过表单清空/覆盖 Peer。运行中任何 Save 都返回 busy。app 报告成功连接（其信任端点已保存）后自动保存 Peer；配置保存失败不更新 Config，也不会声称已经记住对端。同一运行内同一指纹失败只提示一次；可停止后用邀请码重新尝试。

app 订阅可能合并掉极短会话的 connected 阶段。退出时会补查成功握手代次对应的持久化信任记录，只有其指纹和端点都与本次邀请完全一致才补存 Peer，不能仅凭 Generation 推断保存成功。补查使用独立于已取消 Runtime 的 1 秒清理预算；Stop/Close 等待该清理和配置写入完成后才释放运行占用。

IssueInvite 使用 Operator.Endpoint 和 15 分钟 TTL，与监听共享 operator 信任目录，可在监听期间调用。新请求生成前清空历史临时码，失败不恢复旧码；Save、Start、Stop 和 Close 清除 State.Invitation。原码只经返回值和临时 Invitation 交给显式显示/复制操作，不写入配置、事件或日志。app 诊断文本中的邀请码也会被遮盖。控制器事件仅用固定产品文案，app 日志使用丢弃目标；本包不持久化 State、测试结果或事件。

## 验证与边界

确定性测试通过包内私有依赖工厂控制 Run、订阅、测试和持久化的同步点，不把测试注入暴露为公共 API。覆盖并发 Start、重复 Stop/Close、父 ctx 与请求 ctx、等待取消后继续汇合、两种代次的结果隔离、保存失败与旧表单合并、秘密隔离、深拷贝和慢订阅。订阅合并回归另外检查持久化记录缺失、端点不匹配及补存配置失败，避免把握手成功误当作已经记住。

固定角色回归覆盖角色默认值、独立配置路径、共享身份根目录、显式路径复用、损坏与错角色配置拒绝、Save 和动作拒绝。双控制器通过真实 settings/identity/trust 存储及可控 Runtime 并行保存和运行，验证 Client 配对保存不会修改 Operator 配置，停止一端不影响另一端，既有身份和信任内容保持。

真实回归包含默认生产 Runtime 的 Operator 监听、并行邀请和重启，以及两端真实 app/TLS、UDP 发现、配对保存、空邀请码重连、peer-only 测试、透明诊断 ACK、旧连接关闭和固定 TCP/UDP 端口释放。完整往返测试仅在 `_test.go` 中替换网卡枚举、发现目标和假网关临时端口；网络流量全部为 loopback。

未验证真车、网卡热插拔、Windows GUI/托盘或 Windows VM；本包不实现 UI、剪贴板、车辆业务或跨进程配置协调。设置 Save 是不可中断的原子文件替换，ctx 在替换期间取消而写入已成功时仍确认成功；Close 等待所有已准入操作，Stop 仅等待属于本次运行和邀请的 worker。

## MGU direct link（Wave 9）

`ConnectMGU(ctx)`与`DisconnectMGU(ctx)`仅限Operator已连接的会话，捕获Runtime实例与Generation。操作计入当前run的workers，Stop/Close和换代取消旧操作并汇合；正在启用时可禁用，先取消并等待该次启用，再核对原代次，不能跟随到新会话。`State.DirectWorking`表示控制器操作准入，`Runtime.Direct`表示真实会话资源状态，两者不持久化，JSON v1保持。

请求返回后取消不会销毁已由Runtime接管的成功链路。方法返回nil表示该动作完成，界面仍使用最新Snapshot；健康HSFZ不会因MGU查询或原生配置失败而停止。产品错误固定，Direct.Adapter/LastError同其它诊断字符串脱敏。后续原生结果见 `docs/WAVE9.md`；前文“未验证Windows”等为原始工作包边界，不代替后续整合记录。

## 网络工厂注入（Wave 10）

`Options.DirectFactory`只交给Operator运行配置，不在New、读取配置或Start时请求系统授权。只有显式MGU操作才调用；Client路径不注入授权工厂。工厂仍遵守app的请求/资源ctx交接和失败资源归还合同，GUI身份与trust目录不进入助手。Root提供的工厂将具体nil指针规范化为nil接口，原有JSON v1保持。

## HSFZ 抓取（Wave 12）

每个 Operator run 持有独立 Recorder，默认关闭并注入 `OperatorConfig.Capture`。`StartCapture` 只准入当前仍运行的 Operator，创建新文件后请求 ctx 不再拥有录制寿命；`StopCapture` 可取消并汇合在途 Start，取消等待不会丢弃后台关闭。Runtime 结束先汇合转发及动作，再关闭记录器并释放 run。旧连接/旧 run 句柄不会进入下一份文件。

`State.Capture` 是独立快照，最近帧切片每次复制；订阅按 250ms 周期更新索引。路径和数据不持久化到 Config，不进入普通事件或匿名诊断。产品错误使用固定文案，录制故障不会取消 Runtime。格式及数据边界见 `docs/CAPTURE.md`。

`MarkCapture` 是可直接从 UI／全局快捷键调用的有界内存操作，使用 Runtime 最新代次，不经普通异步动作队列；返回成功表示队列准入，不承诺磁盘持久化。标记文本只出现在独立 Capture 快照和 JSONL，最近标记切片同样独立复制。
