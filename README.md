# Remote ENET Community

BMW ENET 的远程发现、呈现与透明转发。Client 运行在车侧，Operator 运行在诊断电脑；两端通过设备身份钉扎的 TLS 1.3 和 yamux 通信。车端不解析隧道载荷、不提权、不配置 L3，也不实现车辆编码或刷写业务。

本仓库为 **AGPL-3.0-only 社区源码快照**，允许按许可证自建使用。源码从首个公开提交开始，未包含私有原型的 Git 历史、第三方手册全文、原始实车抓包或测试密钥。8 位数字配对与中心协调模式仍处于[规划阶段](docs/DEPLOYMENT-MODES.md)，当前使用原始邀请码或已保存的配对关系。此轮仅发布源码，CI 不上传二进制制品，Windows 带 Wintun DLL 安装包的组合许可另行核对。

当前为双端 Native GUI + CLI 版本，Wave 10补充网络授权助手、统一预览包和脱敏诊断：支持内嵌 Hub 的 DDNS 直连、一次性邀请码、持久化信任、单车发现、两个 TCP 通道，以及采用 packet info 的本地发现应答。Client 与 Operator 分为独立程序，提供网卡与地址配置、邀请配对、状态/事件、连接测试和托盘；其控制器位于 `internal/desktop`，底层运行时位于 `internal/app`，支持等待车辆、网卡恢复与地址变化后的重连、有界状态/指标快照、专用连接测试，以及 Windows 的诊断进程独占。UDP 通配接收后按所选网卡和目的地址过滤，回复固定源 IP 和接收端口。

基础库支持独立 Hub，CLI 仍接自用直连模式。已记录 F20 实车上 E-Sys 的 FA/SVT/ECU 只读读取与 ISTA 车辆识别/ECU 树，以及独立的公网入口和八小时 loopback/内存设备长稳验证；各项证据范围分别见[只读验收](docs/research/f20-readonly-2026-11-18.md)、[公网与长稳记录](docs/WAVE17.md)。Windows/Mac TUN 的 IPv4/TCP 和 SSH 已完成隔离假车验收。早期 Tool32 显式网关的模拟 VIN 与其它历史限制见 [M0b 实测](docs/research/m0b-win11-2026-11-16.md)；DoIP、ICOM、HTTP 回连及车辆写操作不据这些结果推定已支持或验收。

## 构建和检查

要求 Go 1.27.3 或更新的 1.27 补丁版（mygo v0.9.1 要求），生产构建关闭 cgo：

```sh
CGO_ENABLED=0 go build -o ./bin/renetd ./cmd/renetd
CGO_ENABLED=0 go build -o ./bin/fakezgw ./cmd/fakezgw
gofmt -l .
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=1 go test -race ./...
```

`.github/workflows/ci.yml` 配置格式、vet、普通测试、Linux race、Windows amd64／macOS arm64 纯 Go 交叉构建，以及这两个系统的原生 socket 测试；Windows 另实际运行所有者解析和多进程独占集成测试。对应提交的执行结果见 [GitHub CI](https://github.com/gonghao/remote-enet-community/actions/workflows/ci.yml)；交叉构建本身不代表运行时兼容性已验证，原生 socket 测试也不等于目标诊断工具兼容。

## Native GUI

界面与品牌采用[Pixel Workbench视觉规范](docs/UI-DESIGN.md)：浅色像素工作台、全直角组件、方形状态灯、等宽数据和统一RJ45 Car Logo；后续页面沿用同一规范。

顶部四项指标使用内置Pixel Code Bold开源像素字体，数值及英文单位跨语言保持一致；简中/繁中标签继续正常翻译。字体采用SIL OFL 1.1，安装包附带完整许可。

`cmd/renet-client` 与 `cmd/renet-operator` 是两个固定角色的程序，使用 mygo v0.9.1 自绘 Native UI；没有角色切换入口。两端各自有窗口、托盘、单实例标识与默认配置，底层复用同一套 app/transport。布局参考 [remote-enet.com 的双端应用](https://remote-enet.com/)：紧凑工具栏、横向实时指标、分组键值信息与集中操作；Settings 与 Log 从日常界面分离。当前采用浅灰底、白面板、深灰正文与蓝色强调的浅色方案，见 [配色研究](docs/research/ui-light-theme.md)。

```sh
CGO_ENABLED=0 go run -tags mygo_nowebview ./cmd/renet-client
CGO_ENABLED=0 go run -tags mygo_nowebview ./cmd/renet-operator

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags mygo_nowebview -ldflags '-H=windowsgui' -o ./bin/renet-client.exe ./cmd/renet-client
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags mygo_nowebview -ldflags '-H=windowsgui' -o ./bin/renet-operator.exe ./cmd/renet-operator
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags mygo_nowebview -o ./bin/renet-client-macos ./cmd/renet-client
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags mygo_nowebview -o ./bin/renet-operator-macos ./cmd/renet-operator
```

Operator 在 Settings 填写公开的 `DDNS:端口` 与本机呈现 IPv4 并保存，返回主页面开始监听、生成邀请，再交给车侧的 Client。Client 通过 Settings 选择车侧网卡并保存，主页面粘贴邀请码、Connect；配对成功记住对端后，后续连接可留空邀请码。网络参数运行中锁定，停止后可改；新界面保留一个 Operator 同时一个 Client 的约束。

两端的 Settings 顶部提供界面语言：简体中文、繁體中文、English，或跟随系统。切换即时生效并记住选择，运行中也可切换，尚未保存的网络表单会保留。窗口、菜单、操作提示、连接测试、MGU 与抓取界面均有翻译；原始诊断数据、标记注释和机器格式不变。语言偏好独立保存，不更改Settings JSON v1；说明和验证见[多语言](docs/WAVE13.md)。

主页面只展示已有的 VIN、网关/网卡、对端、阶段、RTT、实际载荷计数与速率、活动隧道与独占状态。连接测试仍只在两端程序间交换有界 echo；电压/车辆状态与编程安全结论不由 UI 推算。停止会结束 Runtime 并释放其监听与转发；Operator 的 End session 同时停止 Hub 监听，再次 Start listening 可恢复。

默认配置分别是系统用户配置目录下的 `remote-enet/client/settings.json` 和 `remote-enet/operator/settings.json`，身份与信任分别位于 `remote-enet/state/client`、`operator`。两端可同时启动，默认配置互不覆盖；邀请码不持久化。`--config PATH --state-dir PATH` 可用于隔离测试或显式复用角色匹配的旧数据；错角色或损坏配置直接报错，旧 shared `settings.json` 不自动迁移或删除。原来的统一 GUI 入口已移除，headless `cmd/renetd` 保留。

点击窗口关闭按钮或 Quit 都会完整退出，取消并汇合转发与后台任务，释放监听端口。运行中再次启动对应程序会唤回原实例。生产构建使用 `mygo_nowebview`，Windows 产物排除 WebView2；macOS arm64 `.app` 预览可用 `tools/build-macos-client.sh` 生成。自动启动安装、更新发布和签名仍独立处理。范围与验证见 [Wave 5](docs/WAVE5.md)、[Wave 6](docs/WAVE6.md)、[Wave 7](docs/WAVE7.md)。

双端预览包统一使用 `tools/build-desktop.py` 构建，生成ZIP、SHA256及独立manifest。Mac Operator包内附带受限网络助手；Windows Operator包要求提供固定摘要的官方amd64 Wintun DLL。构建不启动程序或安装驱动，参数及签名边界见 [打包说明](docs/PACKAGING.md)。

Wave 8的独立TUN验收工具 `cmd/renet-tuncheck` 用于IPv4/TCP、SSH banner/HTTP及资源回收测试，默认dry-run。Wave 9把MGU只读地址发现和会话TUN接入Operator：连接后可在主页面启用/禁用MGU direct link，成功显示目标IP；结束会话会回收资源。系统网卡/路由初始化需要Operator侧相应权限，车端仍普通权限运行；GUI配置JSON v1不变，直连开关不持久化。Windows Operator还需要可加载的官方Wintun DLL。CLI可显式 `operator --mgu-direct` 在新会话自动启用，默认关闭。验收边界见 [Wave 9](docs/WAVE9.md)、[Wave 8](docs/WAVE8.md) 与[手动工具说明](cmd/renet-tuncheck/README.md)。

**NBT EVO SSH 直连**。Operator 连接车辆后选择「启用 EVO」，CLI 可在新会话自动执行 `operator --evo-direct`（与 `--mgu-direct` 互斥）。程序经车端依次检查 `169.254.199.119:22` 和 `169.254.199.99:22`，选用第一个返回 SSH 标识的地址，然后建立目标 /32 路由，仅转发 TCP 22。用 SSH 客户端连接界面显示的 IPv4 并核对主机密钥。两个地址来自 evo_kit 的兼容记录。探测只确认 SSH 服务可达，主机型号仍由操作者选择。车端 Client 选定的网段须覆盖目标地址，且主机须已提供 SSH 服务。禁用、断线或车辆网关变化会回收直连。EVO 实车仍待验收。

Wave 10的Mac GUI只在明确启用主机直连时请求系统授权，助手只持有本次设备、单目标路由和受限设备I/O，不读取GUI的身份、信任或设置，也不安装常驻服务。Windows界面显示管理员能力，权限不足时提示使用相应权限启动；普通HSFZ会话不因该能力缺失而停止。Log页的Copy diagnostics只在显式点击后复制固定字段报告，省略地址、VIN、指纹、路径、邀请码和原始错误。实现与原生验收状态见 [Wave 10](docs/WAVE10.md)。

Operator 的 Log → HSFZ capture 可显式开启双向原始字节记录和实时帧查看，保存为新的本地 JSONL 文件；默认关闭，只接入新 TCP 连接。覆盖诊断/控制 TCP、UDP 发现和内部 MGU 地址查询，支持按连接/方向筛选及前缀复制。录制期间可用 Insert marker 或全局 Cmd/Ctrl+Shift+M 在工具操作前打标，支持短注释及按标记筛选。载荷不进入匿名诊断，录制限额或磁盘错误不停止健康转发。使用、JSONL 格式及不完整记录语义见[抓取说明](docs/CAPTURE.md)。

## 自动化台架与故障回归

Wave 11提供仓库内隔离假车、固定SSH双向验收探针和内核弱网runner：`tools/lab-fixture.py`可生成临时身份、启动降权服务、运行`renet-labprobe`并自动清理；`tools/l3-netem-check.py`只在明确隔离的Linux网络命名空间里操作lo。两者默认dry-run，只有显式`--run`执行，普通CI不提权或修改系统网络。命令及权限见[LAB](docs/LAB.md)。

探针使用固定host key和临时公钥身份，验证发现目标、固定marker及1MiB/8MiB下载与上传SHA256。新增Go官方`x/crypto/ssh`只供该独立工具和测试，Client/Operator不引入SSH业务。真实TLS/session/L3组合回归区分IP包延迟/丢弃、stream暂停及内核netem，结果与边界见[Wave 11](docs/WAVE11.md)。真实WAN与实车只读步骤见[验收指南](docs/VALIDATION.md)，未执行项不视作已通过。

Wave 15为两端新增“连接自检”：从设置或日志页手动检查网卡、端口、Hub DNS/TCP、MGU权限与已知目标的路由冲突，支持取消和复制固定字段报告。运行中跳过端口探测，不请求授权或向车辆发查询；另提供默认跳过的恢复soak入口。操作与结果边界见[连接自检说明](docs/CHECKUP.md)。

## Linux 本机假网关链路

以下命令在同一台 Linux 机器上使用不同 loopback 地址及高位端口，不需要真车。Windows/macOS 可把假 ZGW 放在另一台测试机，使用实际网卡地址；程序不会创建地址别名。

先准备一份显式的只读测试回包：

```sh
mkdir -p /tmp/remote-enet-demo
cat > /tmp/remote-enet-demo/responders.json <<'EOF'
{"22f190":"62f1905742415858585858585830303030303031"}
EOF
```

终端一启动假网关。周期、超时和 KL15 都是测试配置，不是实车事实：

```sh
./bin/fakezgw --bind 127.0.0.2 \
  --diag-port 16801 --ctrl-port 16811 --disc-port 16811 \
  --profile long-alive --alive 5s --alive-timeout 2s --kl15 04 \
  --responders /tmp/remote-enet-demo/responders.json
```

生成一次性邀请码；这个命令和后面的 Operator 必须使用同一个 `--state-dir`。邀请码只写 stdout，可直接保存到文件，不应出现在日志或聊天记录中：

```sh
./bin/renetd invite --state-dir /tmp/remote-enet-demo/operator \
  --endpoint 127.0.0.1:47000 --ttl 15m \
  > /tmp/remote-enet-demo/invite.txt
```

终端二启动 Operator。两个远端端口对应假 ZGW，本地三个端口供测试工具连接：

```sh
./bin/renetd operator --state-dir /tmp/remote-enet-demo/operator \
  --listen 127.0.0.1:47000 --bind 127.0.0.1 \
  --diag-port 26801 --ctrl-port 26811 --disc-port 26811 \
  --remote-diag-port 16801 --remote-ctrl-port 16811
```

终端三启动 Client，使用 `127.0.0.1` 为源地址，单播发现 `127.0.0.2` 假网关：

```sh
./bin/renetd client --state-dir /tmp/remote-enet-demo/client \
  --invite-file /tmp/remote-enet-demo/invite.txt \
  --nic-addr 127.0.0.1 --discovery-target 127.0.0.2:16811 \
  --discovery-timeout 300ms
```

连接后 Operator 日志显示本地监听地址。模拟工具向 `127.0.0.1:26811/udp` 发完整探测 `00 00 00 00 00 11` 可获得原始发现应答；诊断与控制 TCP 分别为 `26801` 和 `26811`。真实 E-Sys／ISTA 自动发现通常使用标准端口，不能由此高位端口测试推断其兼容性。

也可以用数字码首次配对。在 Operator 桌面端生成数字码，将「复制地址和数字码」得到的一行写入仅本人可读的 UTF-8 文件，格式为 `host:port 1234 5678`，文件最多 4 KiB。用 `--pair-file` 传入文件路径，配对后删除临时文件。`--pair-file` 与 `--invite-file`、`--invite`、`--peer` 互斥。

```sh
./bin/renetd client --state-dir /tmp/remote-enet-demo/client \
  --pair-file /tmp/remote-enet-demo/pair.txt --nic-addr 127.0.0.1 \
  --discovery-target 127.0.0.2:16811 --discovery-timeout 300ms
```

首次配对成功后，Client 会保存 Operator 的指纹和端点。后续不需要旧邀请码，复制 Operator 日志里的完整 `device` 指纹并使用：

```sh
./bin/renetd client --state-dir /tmp/remote-enet-demo/client \
  --peer OPERATOR_FINGERPRINT --nic-addr 127.0.0.1 \
  --discovery-target 127.0.0.2:16811 --discovery-timeout 300ms
```

按 Ctrl+C 退出。网络断开会关闭旧连接和转发，再以有界指数退避重连；配对拒绝、版本不兼容、存储损坏或车辆发现歧义会返回明确错误。无车、所选网卡暂时消失时进入等待，并重新枚举/发现；显式固定的 IP 不会被另一个地址替代。运行中确认源地址或前缀变化后，先关闭旧会话再重新发现，不重放旧诊断请求。已连接期间不会因一次 UDP 发现未回复而切断健康 TCP。

一个 Operator 同时只接一个 Client，同一个诊断进程可以建立多条 TCP 连接。运行时快照及 GUI 复用入口见 [app 说明](internal/app/README.md)。

## 状态、连接测试与独占

Client 和 Operator 默认每五秒输出英文结构化状态日志。`--status-interval 1s` 可缩短间隔，`--status-interval 0` 关闭周期日志。指标包括阶段、会话代次、有效 RTT 样本、双向载荷字节、当前速率、活动连接和最近错误；载荷指透明 TCP 数据，不含隧道头或 TLS/yamux 开销，不解析车辆内容。历史和 RTT 样本有界，`metrics_incomplete=true` 表示部分流量未被记录，实际转发继续。

双方默认允许经 capability 协商的专用连接测试，可用 `--allow-link-test=false` 关闭。任一侧加 `--connection-test` 会在每个新代次执行一次，`--connection-test-timeout 3s` 设置本次预算（1ms–15s）：

```sh
./bin/renetd operator --state-dir /tmp/remote-enet-demo/operator \
  --listen 127.0.0.1:47000 --bind 127.0.0.1 \
  --diag-port 26801 --ctrl-port 26811 --disc-port 26811 \
  --remote-diag-port 16801 --remote-ctrl-port 16811 \
  --connection-test --connection-test-timeout 3s --status-interval 1s
```

测试只在两个已认证程序之间交换有界 echo，数据不进入车辆转发回调或车辆端口。结果区分完成、不支持、超时与取消，包含 RTT p50/p95、已传输字节和串行往返 echo 的有效载荷速率；不表示上/下行容量、IP 丢包率或刷写安全结论。单次测试取消不关闭共享会话。API 与具体预算见 [linktest 说明](internal/linktest/README.md)。

Windows Operator 默认 `--exclusive=true`，可用 `--exclusive=false` 关闭。所有者以本地客户端 socket 的实际 PID 和进程创建时间识别；诊断/控制端口共享占用，最后一条连接结束后释放。独占开启时未知 owner 和其他进程被拒绝，不按 exe 名或父子进程树自动合并。其他平台默认关闭；没有可用解析器时显式开启会报错。具体边界见 [exclusive 说明](internal/exclusive/README.md)。

Tool32 本轮在 `Interface=ENET`、`[XEthernet] RemoteHost=<Operator bind IP>` 下读取模拟 VIN 成功；LAN `Autodetect` 尚未通过。生产程序不会自动修改 EDIABAS 配置。

## DDNS 直连

在家里的 Operator 机器上将 `--listen` 绑定到实际端口，例如 `0.0.0.0:47000`；路由器把这一 TCP 端口转发到它。`invite --endpoint` 填外部可达的 `DDNS:47000`，`operator --bind` 填诊断工具所在网卡的 IPv4 地址。默认本地端口为 `6801/tcp`、`6811/tcp`、`6811/udp`；远端默认 `6801/tcp` 和 `6811/tcp`。

车侧 `client` 用 `--nic` 或 `--nic-addr` 指定 ENET 网卡。未指定时仅自动选择唯一的非 loopback link-local 地址；多车或多网卡歧义不会任意选择。车端只允许所选网卡子网中的字面 IPv4 目标，并拒绝本机所有地址、组播和广播；转发固定从该网卡地址发起，不解析载荷。

## 身份和信任文件

每个角色使用独立的 `--state-dir`。其中 `identity/` 保存设备密钥和自签证书，`trust.json` 保存可信设备、邀请码哈希及最近对端。新建目录权限为 `0700`，密钥与信任文件为 `0600`，公开证书为 `0644`；Windows 的实际访问控制仍由目录 ACL 决定。首次身份创建也使用同一跨进程锁，避免同时启动 invite 和 operator 生成不同设备身份。

信任操作每次重新读取文件，邀请码的校验、消费和信任写入在同一锁内原子替换，因此 Operator 运行时也可以另开进程签发邀请。损坏文件不会静默重置。锁等待最多五秒，并可被 context 取消；如果进程异常退出留下 `trust.json.lock`，先确认没有 `renetd invite/operator/client` 进程正在使用该目录，再手工删除这个**锁目录**后重试，不要删除身份或信任文件。

`--invite-file` 接受带或不带 BOM 的 UTF-8 文件，方便使用 Windows PowerShell 的显式 UTF-8 输出；不支持把 UTF-16 文件当邀请码正文读取。Windows VM 与 Linux 假网关的测试步骤见 [M0b 指南](docs/M0B.md)，分别记录“显示 VIN”和“完整诊断连接”，不需要真车。

架构见 [docs/DESIGN.md](docs/DESIGN.md)，协议证据见 [docs/enet-protocol-research.md](docs/enet-protocol-research.md)，应用接口见 [Wave 2](docs/WAVE2.md)，广播和 CI 稳定性改动见 [Wave 3](docs/WAVE3.md)，运行时与观测验收见 [Wave 4](docs/WAVE4.md)，Native GUI 与配置契约见 [Wave 5](docs/WAVE5.md)，双端拆分与布局见 [Wave 6](docs/WAVE6.md)。

直连、自建 Hub、中心协调三种部署模式，以及 8 位数字码（PAKE）配对入口见 [部署模式与配对入口](docs/DEPLOYMENT-MODES.md)；中心的职责、票据、授权期限与威胁模型见 [中心协议](docs/CENTER-PROTOCOL.md)。两份文档记录设计基线与待定条件，不表示现有程序已经支持这些能力。后续工作顺序见 [路线图](docs/ROADMAP.md)。

## 许可证与官方服务

项目自有源码采用 **AGPL-3.0-only**，即仅 GNU Affero General Public License 第 3 版，不包含自动适用后续版本的授权；完整文本见 [LICENSE](LICENSE)。第三方代码、字体、驱动与研究资料按各自许可或权利处理，见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES.md)，不因项目根许可证而重新授权。

允许遵守许可证的修改、自建和商业使用。官方 SaaS 的账号、数字配对、节点及带宽属于独立运营服务，服务资格与额度由中心和官方 Hub 执行；开源不授予免费使用官方服务资源的权利，也不公开服务端密钥或用户数据。

社区源码入口为 [gonghao/remote-enet-community](https://github.com/gonghao/remote-enet-community)。第三方资料排除、历史审查和发布边界见[开源准备记录](docs/OPEN-SOURCE-READINESS.md)。正式二进制发行仍需落实对应版本的相应源码、构建材料及适用的第三方组合条款。
