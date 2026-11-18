# Wave 11 · 自动化台架、L3弱网与验收探针

基线3d5e209，用户确认启动。按Paseo独立worktree并行，最后OpenCode DeepSeek只读复核；提交推送仍须用户明确指令。代码/注释/日志English，文档中文，Go1.27.3/CGO=0。默认测试只用loopback、net.Pipe、内存设备，不联网、不提权、不创建TUN/路由。真实WAN与实车仍后补，本轮准备只读步骤并明确证据边界。

## 不变量

车端继续普通权限、载荷透明、不做L3；Hub只搬密文。CBOR、TLS、settings JSON v1、身份/邀请码格式不变。不实现车辆编码、SSH使能、刷写或HTTP回连业务。测试SSH仅固定host key与临时身份访问隔离fixture，命令为固定只读测试协议，不接受任意远程命令。

本轮将golang.org/x/crypto/ssh固定为已在Wave10临时探针验证的v0.57.0，仅供独立台架探针及loopback测试；生产Client/Operator不引用它。go.mod/go.sum只由Root统一修改。

## A · 仓库内Linux隔离fixture

拥有tools/lab-fixture.py、tools/test_lab_fixture.py、docs/LAB.md。以/tmp/remote-enet-wave10/fixture-lab.py为只读起点（不是已审定最终实现），标准库Python整理，不能仅原样复制后宣称完成。子代理不运行sudo/unshare/tc/sshd/真实网络操作，不访问VM/真实状态/私钥，不commit/push。

默认dry-run、--help不创建目录/密钥/进程或改网络；只有显式--run启动。固定隔离169.254.91.0/29拓扑：宿主test veth .1、子namespace gateway .2、MGU .3；全IPv4地址及所有非默认路由重叠预检，已有覆盖拒绝。随机私有veth名+ifalias nonce，核对实际child PID和netns inode，成功步骤才登记回收。生命周期、命令输出、进程等待有界；错误保留阶段与部分报告，删除只限本次已核对所有权资源，不改管理网卡/默认路由/全局防火墙或实际sshd配置。

actor从显式uid/gid对应的现存普通账户验证，不硬编码ghsky。fakezgw必须降权运行（可固定调用setpriv），Client由外部普通用户运行，不由root fixture顺手启动。发现worker尽可能独立降权，若保留root协调线程须解释必要性；root仅负责资源协调与受控sshd监听22所需权限。发现服务wildcard6811、精确六字节请求、只允许宿主test veth来源、IP_PKTINFO固定回源.2:6811及完整56字节fixture，启动前实测回包，退出汇合。

Root配置/host私钥保持root0700目录；actor使用独立0700兄弟目录，临时client key/host公钥/export.json只给actor。禁止修改真实authorized_keys、用户密码或HOME权限。受控sshd固定公钥认证、单actor、禁密码/PTY/转发/用户RC/环境，不安装软件。ForceCommand为固定fixture协议：空命令或read-only-marker返回remote-enet-mgu-fixture；read-only-bulk（兼容1MiB）、read-only-bulk 1048576/8388608输出对应零字节；read-only-upload 1048576/8388608读取精确字节并回JSON{bytes,sha256}，不落盘，未知命令失败而非默认成功。命令本身也需超时与大小上界。测试全部使用mock/纯逻辑，不实际改网络。

导出ready/report记录身份路径但不输出密钥/邀请码内容；面向共享的export.json仅fixture地址、用户名、host公钥及相对client key路径等明确白名单，权限严格。报告区分进程正常退出、信号退出、强杀、线程汇合、接口/路由/namespace是否实际回收；不能只靠命令返回0判通过。

## B · 真实加密会话与L3故障回归

拥有internal/integration/l3_fault_test.go及必要同目录私有test helper，不改其它包/依赖。复用现有integration的真实TLS/Hub/session fixture，结合gVisor测试端+packetio内存设备和l3.Start；车侧测试回调只做opaque echo/固定数据，不解析业务。目标为已知精确IPv4，核对Target/Origin及计数。

默认回归覆盖：1MiB与8MiB双向数据和SHA256；确定性packet delay/drop（记录实际注入次数、区别stream暂停与IP丢包）；双向暂停/黑洞后的取消和有界回收；断开旧底层会话/设备后新代次可用、旧未完成请求不会被应用重新打开/重放；正常半关闭与失败流隔离。不得通过固定sleep、增加任意timeout或放松哈希/字节数断言掩盖故障。资源ctx、worker、握手与go routine全部有汇合证据；普通测试时间预算适合CI，不做无理由高次数。

另外提供一个显式手动入口测试TestL3ExternalNetem，未设置RENET_MANUAL_L3_NETEM=1默认Skip。该用例允许较长有界预算，真实Hub使用loopback TCP、fixture业务回调使用内存，便于Root在仅含lo的独立Linux netns施加内核netem；不依赖外部sshd/TUN/VM。记录完整1MiB/8MiB哈希、有效路径和取消回收，不能把packet模拟冒充内核测试。Root编译test binary后降权执行，子代理仅做默认普通/race及交叉编译。

## C · 可移植fixture探针

拥有cmd/renet-labprobe/，不改go.mod/sum或其它包。可先用/tmp modfile验证x/crypto v0.57.0，依赖由Root统一接线。默认dry-run及help/version均不读密钥、不拨号、不写报告；显式--run才执行。CLI至少支持--gateway（literal IPv4:port）、--target（literal测试IPv4）、--ssh-user、--identity-file、--host-key-file、--payload-bytes（1MiB/8MiB）、--timeout（有界）、--report。

全流程仅固定台架协议：mgu.Discover只读确认目标与--target一致；SSH固定公钥认证+固定host key，无agent/密码/TOFU/用户ssh配置；先固定marker，再验证下载及上传的精确字节数与零载荷SHA256/JSON回执。所有阻塞调用属于同一caller预算，取消关闭当前连接且汇合回调，不依赖系统ssh.exe。输入拒绝DNS、mapped/非法IP、零端口和不合理大小，不接受任意远程命令或任意payload文件。

独立labprobe报告schema v1，记录版本/平台、阶段/耗时、预期与实际字节/哈希、成功/取消/超时、清理结果；报告不含密钥、邀请码、真实路径、任意原始错误或数据载荷。错误使用固定stage/code，保留部分结果，失败不假报成功。报告如需写盘使用同目录临时文件+sync+rename、0600及symlink拒绝；成功/失败exit code正确。测试用loopback临时SSH服务、生成的临时keys及内存HSFZ fixture，验证相同协议和host pin、错key、畸形回执、超时/取消/部分数据等。

## Root · 编排、内核验证与指南

固定新依赖与合同，协调A/C命令协议。新增tools/l3-netem-check.py及其纯逻辑测试：默认dry-run，--run仅接受已unshare且只有lo的root namespace；每次tc前重新核验隔离身份，计数与所有命令入审计，禁止影响宿主网络。降权执行编译好的TestL3ExternalNetem，分别baseline和200ms/1% loss，统计包/drop与测试完整结果，失败保留报告并有界清理。内核人工测试、Windows原生探针与原生路由检查由Root执行，子代理不得执行。

文档准备本地直连对照、实车只读和DDNS/手机热点步骤；WAN需用户准备公网端点，实车需用户在场及明确范围，均不伪称已完成。最后全仓gofmt、CGO0 vet/test、相关race、Win/Mac纯Go构建，DeepSeek复核，再如实报告已有证据及边界。

## 实现与验收结果（2026-11-17）

已交付三个默认无副作用的工具/入口：隔离fixture及可选`--probe-bin`一条命令编排；跨平台`renet-labprobe`；显式手动`TestL3ExternalNetem`及隔离内核runner。本轮L3组合测试覆盖真实TLS/独立Hub relay/session/L3、1MiB/8MiB双向哈希；既有stream故障用例另覆盖内嵌与独立Hub。L3部分包含包级延迟/丢弃、黑洞与取消、断开重建/防重放、半关闭和stream暂停隔离。手动入口未设置环境时Skip，不由默认CI模拟结果冒充内核证据。

| 验证 | 结果与证据边界 |
|---|---|
| Linux probe | 真实隔离sshd固定host pin/临时公钥认证，1MiB及8MiB分别完成下载/上传SHA256，所有阶段ok/cleanup=true |
| 一条命令编排 | run04与最终加固run05均在外层独立netns执行，8MiB六阶段报告完整；fixture与探针都结束后接口/路由/namespace全部回收、日志worker已汇合，无cleanup errors |
| Windows原生probe | 沿用已验证Windows Operator候选包，仅替换独立探针；普通Linux Client明确绑定本轮veth，Win管理员模式保持进程独占，真实Wintun目标路径上1MiB/8MiB双向全哈希通过。原生普通文件key读取通过；不是系统ssh.exe替代性退出码测试 |
| kernel netem04 | 同一独立仅lo的netns，执行普通uid/gid=1000的编译integration test；baseline与degraded各输出1MiB/8MiB两条完整tag，真实path=relay、active_after=0、workers_joined=true |
| 内核计数 | baseline增量8050 packets/0 drops；degraded在lo IPv4 TCP配置200ms及1% loss，增量6408 packets/62 drops。1MiB实际SHA为c196c433e60b1bdafd22bbacfabb8f7b7068f912940259ed2690df5ccedb3f8c，8MiB为187ba28c9df62b0be31185ef307a2a2adcd0353dc11260ce19deb9e855287016，与生成预期一致 |
| 内核回收 | 总历时92.84s，本轮46条命令全部joined，qdisc删除且lo恢复原Down，无cleanup errors。这里是每次lo TCP出队的配置延迟；relay存在多条腿，不推断端到端单向时延或实际网络丢包率 |
| 其它回收 | Windows正常禁用/退出，目标/32、呈现TCP/UDP及Hub监听消失，专用计划任务/防火墙规则移除；普通Client与交互fixture已停止。临时身份保持私有，不提交仓库 |

### 实跑暴露的边界与修正

- run01发现本机`ip link add ... alias`未保留预期alias，正确的保护拒绝删除未标记设备；Root核对本轮随机pair的ifindex/MAC/互指/veth/down/无地址后单独回收，保留失败报告。现改为仅在child netns内创建、显式标记并验证两端后才移host，失败不留未标记宿主网卡，不按前缀猜删。
- run02确认StrictModes因/tmp授权文件祖先拒绝，但actor并未锁定。只改本次daemon为AuthorizedKeysFile none+固定root-owned echo返回配置中钉住的临时公钥；保留StrictModes yes/UsePAM no，不改密码、HOME或实际SSH授权文件。
- netem01的初始Down lo返回空qdisc列表；现在仅原始Down允许该初始形态，拉起后必须再确认noqueue。netem03的prio含合法`multiqueue:false`；现在只显式兼容该可选false，true、非布尔及未知字段仍拒绝。两轮失败都保持fail-closed，外层netns退出后无宿主残留。
- `Child.stop`不向已reap PID/PGID发信号；部分内核mutation在返回前超时也按实际树和原始lo状态复核，不确定状态输出null，不冒报回收。
- 密钥文件Unix非阻塞/no-follow打开后核验类型与身份，FIFO/替换竞态不再阻塞；Windows仅本地普通文件路径，拒device/UNC/ADS与已知重解析形态，最终句柄必须DISK且非reparse。原生验证只覆盖本轮普通私有文件；云占位等其它父目录重解析及被其它软件占用时的拒绝行为保留边界。
- root系统工具固定可信路径/属主/权限，环境仅固定PATH与LC_ALL。服务日志每份8MiB，满后持续drain并记录截断；join失败使整体失败，日志线程为daemon以免异常文件系统阻塞解释器退出。工具信任调用者选择的已构建探针，不提供抗同用户伪造的远程证明。

### 门禁与分发

全仓gofmt/diff-check、CGO=0 vet/test、相关integration/probe race、80项离线Python工具测试及Windows amd64/macOS arm64纯Go构建通过，go mod tidy无差异。DeepSeek完成全范围只读审查及D1/R1–R4收口复核，未发现剩余确定缺陷；原生结果由Root独立执行记录，不由审查替代。新增依赖为golang.org/x/crypto v0.57.0，用于台架GoSSH；x/term仅新增模块校验项，生产Client/Operator不引用SSH包。

CI增加全部离线工具单测、Windows/macOS原生probe loopback/key测试和独立probe artifact；实际流水线结果以用户明确提交推送后的运行记录为准。本轮尚未自动commit/push。私有原始报告保留在Root测试目录，仓库只记必要测量与边界，不带keys/邀请码内容。

真实WAN、真实车辆、完整mgu_kit GUI、Linux真实TUN及新probe的Mac原生执行未验收；已有Mac产品/数据通路结果仍以Wave10为准。后续步骤见[VALIDATION](VALIDATION.md)，不以本轮假车或内核弱网代替实车结论。
