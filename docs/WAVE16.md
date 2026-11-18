# Wave 16 · 长稳与原生验收

基线为Wave 15与其CI修复。用户确认推进八小时持续传输、故障恢复和资源监测，并准备Mac一键验收。已发布Wave 15的CI修复单独提交。生产协议、设置和诊断schema不变，不新增依赖，不操作真实车辆或系统路由。

## 长稳设计

- 独立持续连接与恢复循环两条lane，真实loopback Hub/TLS/session/L3、内存packet设备。持续lane保持同一TCP流，恢复lane轮换取消、物理传输断开和设备关闭，逐次确认旧流结束、重建哈希及资源汇合。
- 不直接把`testing.T.Run`无限循环八小时。恢复周期使用可回收作用域，清理回调每轮执行，成功日志只计数；持续lane不为每个包注册清理回调，避免测试框架自身累积污染资源趋势。
- 私有目录记录固定字段checkpoint、累计字节、1MiB/8MiB哈希、实际时长、Go heap/协程及外部RSS/FD采样。报告输出有界，结束状态必须区分running/passed/failed/canceled。
- 先进行短smoke和race回归，再启动八小时。八小时实际结束且所有校验/清理通过前，不能记为通过；进程异常、超时、取消或报告缺失均不能成功。

## 原生与外部边界

Mac脚本先提供独立配置的双端GUI自检验收，不覆盖用户现有配置、不自动停止应用、不生成邀请码、不请求授权或创建TUN。真实休眠/网卡切换和MGU授权/清理需要用户在场，分别记录证据。Windows原生回归使用已有隔离台架。

DDNS/手机热点与实车只读继续按[VALIDATION](VALIDATION.md)，本轮loopback证据不替代外部验收。

## 可复现入口

监督器使用Linux的`/proc`进行外部RSS/FD监测，Go测试二进制使用生产要求的CGO=0。默认dry-run不启动进程或写盘；显式运行需新的输出目录、测试二进制和源码revision。

```sh
CGO_ENABLED=0 go test -c -o /tmp/remote-enet-stability.test ./internal/integration
python3 -B tools/stability-check.py
python3 -B tools/stability-check.py --run \
  --test-bin /tmp/remote-enet-stability.test \
  --output /tmp/remote-enet-stability-new-run \
  --revision COMMIT_HEX --duration 8h --interval 30s
```

将`COMMIT_HEX`替换成实际7–40位小写commit hash；有未提交测试改动时加`-worktree`。时长支持1分钟至8小时，间隔支持1至60秒；短smoke可用`--duration 1m --interval 5s`。

持续连接在两个预热传输后采集基线，恢复lane在三个预热周期后采集基线；实际计时使用monotonic clock，累计字节包含预热。Go heap为显式GC后的存活量，超过基线64MiB或协程超过基线16则失败；监督器在进程标识匹配后监测RSS/FD，预算为基线256MiB/32个FD。阈值只表示预算内，不等同于数学上证明无泄漏。

`status.json`原子更新最终状态，`steady.json`/`recovery.json`保留Go检查点，`resources.jsonl`记录外部趋势。子进程非零、字节/哈希/时长/清理证据不完整、超预算或取消均不能成功；只有两lane结束并回收后才记`success=true/cleanup=true`。

Mac工具`tools/mac-gui-check.sh`默认dry-run，`--help`给出参数；显式运行需要`--source LOGIN:/ABS/PACKAGES --version VERSION`。它只改私有副本的Bundle ID并重新ad-hoc签名，不证明发布签名或公证。

## 执行状态（2026-11-18）

- 已发布Wave 15的CI硬化分开提交：`974ce9e`处理Windows锁目录delete-pending期间的瞬态拒绝，`b1af1e4`汇合Hub监听Close回调，`8a5b8de`修正接收端拒绝与Send返回之间的测试时序断言。后续CI结论以对应run为准，不把某次重跑替代根因验证。
- `8a5b8de`的私有原型 CI `44253510652`（原始记录未随社区快照公开）全部通过，包括Linux check/race、Windows/macOS原生测试与双平台构建。此CI结论只覆盖当时已推送代码；本轮长稳工具另通过本地smoke/race与86项离线工具测试。
- Windows真实delete-pending状态与并发身份初始化连续50次通过；Hub旧实现被无sleep屏障回归确定性复现提前退出，修复后通过；发送端关闭测试正常100次、race20次通过。
- 新双lane监督器一分钟smoke完成，持续连接lane运行63.7秒、恢复lane75.6秒后，各自完成字节/哈希、旧请求隔离与清理。监督器整轮82.1秒，最终`passed/success=true/cleanup=true`。短smoke不替代八小时证据。
- 八小时任务于14:21（Asia/Shanghai）启动，基于冻结的`b1af1e4-worktree`测试二进制；后续`8a5b8de`仅改app单元测试，不影响此二进制的生产路径。预计22:21以后完成，当前仅记为running。证据目录`/tmp/remote-enet-wave16/eight-hour-01/`，启动身份和源文件/二进制摘要分别在上级`eight-hour-launch.json`、`source-manifest.json`；任务结束前不记八小时通过。
- 启动约28分钟时两lane仍running：持续lane 59次传输，协程42/FD11；恢复lane 57次传输，协程4/FD6；哈希正常且当前资源在预算内。这是中途检查，不是八小时通过结论。监督器会自行收尾并将最终状态原子写入`status.json`，不需要保持Mac或聊天终端运行。

后续更新：原任务约97.2分钟时recovery超时，监督器停止并回收两lane，最终success=false/cleanup=true。上面的running为历史中途记录，八小时未通过；测试客户端队列短写现场、确定性回归及从零重跑要求见[Wave 17](WAVE17.md)。原22:21完成预估失效。

2026-11-19最终更新：修正测试客户端队列后，从零重跑的两lane均完整超过28800秒，监督器28816.18秒后`passed/success=true/cleanup=true`，工作协程汇合、资源预算和不重放断言均通过。原失败任务不改记为通过；分lane累计字节与证据见[Wave 17](WAVE17.md#2026-11-19完整长稳结果)。范围仅loopback/内存设备上的TLS/session/L3，不是原生TUN或实车八小时测试。

## Mac用户回报

候选包从干净的`b1af1e4`构建，版本`0.1.0-preview.wave16.0`，私有副本增加validation Bundle ID并重新ad-hoc签名，避免三语言选择污染正式应用的NSUserDefaults。用户实际运行后的白名单JSON如下：

| 场景 | 实际结果 |
|---|---|
| Client空闲，14:43:46 | 设置、接口和所选loopback网卡通过；未配对显示pairing_missing，未运行会话显示not_running |
| Operator空闲，14:44:22 | 四个端口均port_available；权限助手authorization_on_demand；不发MGU查询 |
| Operator监听中，14:29:55 | 四个端口均skipped/runtime_active；会话等待对端；公开地址提示remote_check_required |

三份报告均complete=true、canceled=false、timedOut=false，并已验证原生GUI显式复制。具体JSON保留在`/tmp/remote-enet-wave16/mac-{client-idle,operator-idle,operator-active}.json`。这些结果不证明真实对端、外网可达、MGU授权或TUN；Mac手动繁中/英文切换与真实休眠/物理网卡变化尚无回报。完成此轮自检后可关闭测试窗口，不影响测试机上的八小时任务。
