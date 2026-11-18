# Wave 17 · 单端暂停与公网入口分层验收

基线9285386。用户配置公网TCP端口转发，并指定DDNS测试；Mac实际位于公司网络，WG按规则分流，无需切手机热点。此轮不改变生产协议、Settings或匿名诊断schema，不增加依赖，不操作真实车辆。临时入口及实际地址、身份、邀请、日志仅保存在私有台架目录，不提交仓库。

## 单端暂停

Linux生产Client连接临时Windows Operator，车辆端为本机合成loopback fixture；只暂停Client，Windows对端时钟和心跳持续运行。

- SIGSTOP 2秒后SIGCONT，保持代次1。
- SIGSTOP 35秒后SIGCONT，重连到代次2。
- 两个代次分别完成10个采样、1MiB发送/接收的peer-only测试，测试进程均退出0，cleanup=true。
- 假车未收到诊断帧；此项不证明车辆TCP请求的重放行为。真实OS睡眠、USB驱动断电和物理网卡变化仍需原生证据。

证据：`/tmp/remote-enet-wave17/lan-pause-01/report.json`。测试使用自己的pidfd发送信号，不暂停八小时lane、管理网卡或用户应用。

## 公网会话探针

新增手动工具[`renet-wancheck`](../cmd/renet-wancheck/README.md)。它复用生产Hub/TLS/session/trust，第一阶段配对，第二阶段从本轮保存的端点复连并使用零token；发布合成车辆元数据，所有车辆隧道均拒绝，不调用车辆Forwarder、不创建设备或路由。默认dry-run；邀请仅由普通stdin文件输入，不进入argv、输出或日志。

普通测试仅使用loopback，覆盖钉扎会话、只消费一次token的复连、默认/help/version无副作用、非法输入秘密不外泄以及取消时握手socket回收。手动公网行为只有显式`--run`执行，和普通CI的禁止公网边界分开。

临时Windows入口仅公网Hub TCP，呈现端口为loopback临时端口，MGU关闭；单独的防火墙规则按本次程序路径、目的地址和端口限定，计划两小时回收。测试身份不与正常Operator共享，原有防火墙规则不改变。此配置不作为最终诊断工具的呈现配置。

## 2026-11-18用户回报

Mac从公司网络执行，目标路由含tunnel接口，因此将Mac路径记录与Windows入站来源结合判断。两个连续会话与服务端观测到的同一个其它公网IPv4、不同源端口和代次5/6相互吻合；家庭内网回环预检另记，不混为这次结果。

| 阶段 | 结果 | 双向字节 | RTT p50 / p95 |
|---|---|---|---|
| first_pairing | ok，10 samples | 1048576 / 1048576 | 5.419 / 6.284 ms |
| paired_reconnect | ok，10 samples | 1048576 / 1048576 | 5.850 / 6.682 ms |

报告success=true、cleanup=true、timedOut=false、canceled=false、rejectedTunnels=0；Windows侧时间与两阶段吻合。记为**公网入口、TLS钉扎、配对复用与peer-only echo通过**，不据此推断无任何代理、中转、真实车辆、GUI网卡发现、HSFZ透明转发或MGU路径通过。

私有证据：`mac-wan-company-report.json`、`wan-remote-peers.jsonl`、`devbox-public-address-report.json`、`wan-setup.json`；Mac本机保留脚本打印的evidence目录。实际域名和地址只在该私有目录记录。

## 八小时长稳的失败与修正

原`eight-hour-01`在约97.2分钟失败，不能记为八小时通过。recovery完成186次成功传输后，下一次1MiB只收到992580字节而超时；记录client queue_short=241，显式丢包计数0。资源未超预算，监督器取消steady lane并回收，最终success=false、cleanup=true，原始证据保留。

确定性突发回归显示：合成客户端的原128包队列只能接收一次最大TCP发送窗口的2913包中的128包。固定gVisor最大TCP buffer为4MiB，测试客户端队列现限定为4096包以容纳该突发及ACK余量；所有完整bulk和持续传输额外断言无客户端隐式短写。显式丢包注入仍由原fault gate控制，不增加重试、不减少载荷，也不放宽哈希和关闭断言。

这仅修正测试客户端的基线队列；生产L3配置不改变，不能据此声称所有丢包/超时原因都排除。需先过同载荷smoke/race，再以新二进制、独立目录从零重跑八小时。原22:21预计完成时间已失效，最终结论以新任务的实际完整时长、校验及清理为准。

修正后确定性突发回归、1MiB/8MiB bulk、故障恢复、smoke及对应race均通过；新一分钟监督器试跑两lane均passed，整轮63.1秒、cleanup=true。新任务于11月18日16:22启动，目录`/tmp/remote-enet-wave17/eight-hour-queuefix-02/`，源文件/二进制摘要和冻结源码另存同级`queuefix-source-manifest.json`、`frozen-queuefix-sources/`。以下最终结果只计算本次从零重跑，不与原任务累计时长。

### 2026-11-19完整长稳结果

监督器实际运行28816.18秒，最终`passed/success=true/cleanup=true`；两lane均退出0，`workersJoined`、`resourcesWithinBudget`、`noReplay`全部true。

| lane | 实际时长 | 成功传输 | 每方向累计字节 | 协程基线/峰值/清理后 |
|---|---:|---:|---:|---|
| steady | 28812.151秒 | 962 | 4539285504 | 42 / 42 / 4 |
| recovery | 28813.271秒 | 936 | 4416602112 | 4 / 4 / 4 |

完整1MiB/8MiB载荷哈希与故障恢复断言均通过；冻结二进制SHA256为`30a9bfea20f6a0c954bfb08ce30639ac15b30c1bed919932fed234e364a11726`。本项仅证明loopback上的真实TLS/session/L3与内存设备长稳、恢复和资源回收，不是八小时公网、原生TUN或实车验收；原失败报告继续保留。
