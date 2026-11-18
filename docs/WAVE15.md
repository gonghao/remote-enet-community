# Wave 15 · 连接自检与恢复验收

基线14daf1a。用户确认继续非UI后续工作，按已建议的“连接自检＋可靠性验收”推进；不增加依赖，不改变Settings JSON、CBOR/TLS、邀请码或匿名诊断schema。

## 连接自检

两端提供显式手动入口，使用已保存设置。车端可读取当前输入的邀请码（仅内存解码，不消耗或保存token）；未输入时只读取已有配对的公共端点。报告只含固定项目、状态、错误码及耗时，不包含地址、指纹、网卡名称、邀请码、私钥或原始异常。

- 检查配置、所选车辆网卡/呈现IPv4是否可用；自动车端网卡选择保持唯一link-local规则。
- Operator空闲时短暂绑定并立即关闭本轮配置的TCP/UDP端口，区分占用、地址缺失、权限及系统失败；运行中全部跳过，不把跳过记成可用，也不接管已有listener。
- Client按正常系统路由解析/连接最多4个Hub候选，DNS/连接使用总预算和单项预算；不绑定车辆网卡、不发送协议数据或配对请求。TCP可达不代表TLS身份、配对、WAN或车辆可用。Operator的公开端点仅检查设置和DNS，不从内网回连自身公网地址以推断外网可达。
- 网络权限仅查询能力，不弹系统授权；MGU地址未发现时跳过路由检查，不发地址查询。已启用Direct时跳过与自身路由冲突检查；其它情况复用tunadapter的严格只读预检，不创建TUN、改地址/路由或删除资源。
- 默认总预算10秒；ctx取消会汇合已准入工作。同步网卡/文件系统API不能被硬取消，前后检查ctx，不用遗弃goroutine模拟硬超时。

结果只作为当次观测，不自动修改配置、不影响健康会话。Stop/Close可取消并汇合自检，配置/运行实例或代次变化后不提交旧结果。UI保持Pixel Workbench并补简繁中文与英文文案；检查详情和建议由固定码映射，不把任意内部错误暴露给用户。

## 恢复与长时间运行

自动化仅使用loopback、内存和net.Pipe。补充重复故障/重连下的真实TLS、透明字节、旧连接EOF、代次隔离与端口/worker回收；可选手动soak入口默认Skip，运行预算显式配置，报告实际耗时和循环数。短验证不冒充数小时、真实OS休眠唤醒、WAN或实车结果。

真实WAN和实车只读继续按[VALIDATION](VALIDATION.md)执行，需要对应环境与用户在场。自动恢复保持会话级重连，旧诊断请求不由应用重新发起。

## 完成标准

自检正常/失败/取消、缺失/歧义网卡、端口占用、DNS/连接失败、IPv6端点、有界候选、路由冲突、全局Stop、过期完成及秘密注入有行为回归。连接/资源故障与自检不影响对方正常工作。通过全仓格式、vet/test、相关race、离线工具及双平台纯Go构建；原生、soak和外部场景分别记录实际证据。

## 本轮完成与证据（2026-11-18）

两端设置/日志页已接入手动自检、取消、固定建议和独立JSON复制，简中、繁中、英文完整。Controller取消入口独立于普通四槽；在途Start前、等待配置门、空闲探测、运行中、Stop/Close、配置/代次变化和后台保存新配对均有回归。空闲探测与启动串行，运行中不持配置门等待网络查询。现有匿名诊断、Settings、邀请码和网络序列化格式不变。

| 验证 | 实际结果 |
|---|---|
| Linux格式/vet/全仓test | 通过，生产`CGO_ENABLED=0` |
| race | checkup、desktop、desktopui、ui、trust、tunadapter、integration通过；race使用CGO=1 |
| 离线工具 | 80项Python测试通过，未执行特权操作 |
| 公开Runtime恢复回归 | `TestPublicRuntimesReconnectAndReleasePresenter -count=3`通过；真实loopback TLS、模拟NIC前缀变化、旧TCP结束、代次隔离、捕获字节和端口释放 |
| Windows 11（26100） | 实际执行checkup/trust测试与desktop自检生命周期、ui三语言/取消/小窗口headless测试通过，含IPv4和IPv6 loopback；未启动原生GUI或创建TUN |
| macOS arm64 | 测试包和双端应用纯Go交叉构建通过；本轮未执行Mac原生GUI/系统授权 |
| 统一预览包 | Windows amd64及macOS arm64的Client/Operator四包、manifest和SHA256生成校验通过；版本`0.1.0-preview.wave15.0-worktree` |

显式一分钟soak实际运行60.035秒，完成97轮：cancel 33、transport 32、device 32，97轮实际路径均为relay。交替1MiB/8MiB，每方向累计454,033,408字节，逐轮上传/下载哈希一致，旧流未重放，L3/session/callback/packet/Hub/transport清理均完成后输出成功汇总。两个固定载荷SHA256：

| 字节 | SHA256 |
|---|---|
| 1048576 | `c196c433e60b1bdafd22bbacfabb8f7b7068f912940259ed2690df5ccedb3f8c` |
| 8388608 | `187ba28c9df62b0be31185ef307a2a2adcd0353dc11260ce19deb9e855287016` |

本机证据保留在`/tmp/remote-enet-wave15/`：`recovery-soak-1m.log/json`、`public-runtime-recovery.log`、`test-final.log`、`race.log`、Windows回报及`preview/`四包；这些临时产物不纳入仓库。

未完成的外部验收仍包括数小时持续运行、OS真实休眠/唤醒与物理网卡切换、真实WAN及实车只读。此一分钟内存设备soak不能作为这些项目的通过证据。操作方式见[CHECKUP](CHECKUP.md)，外部步骤见[VALIDATION](VALIDATION.md)。
