# WAN会话探针

显式手动工具，默认dry-run。它复用生产Hub、TLS、session、trust及peer-only测试，发布合成车辆元数据以允许临时Operator进入会话；不枚举或绑定真实车辆网卡，不发发现广播，不调用carside Forwarder，不连接车辆服务、不配置TUN/路由，所有来自对端的车辆隧道均关闭并计数。

```sh
renet-wancheck --run --expected-endpoint home.example:47000 \
  --state-dir /private/new-client-state < /private/invitation.txt > /private/report.json
```

邀请仅从已经打开的普通stdin文件读取，上限128KiB，不进入命令参数或日志；必须包含唯一的、与expected-endpoint完全相同的embedded Hub端点和钉住身份。state-dir必须新建，不覆盖已有配置、身份或信任。两阶段依次进行首次配对、从本轮已保存端点复连；第二阶段使用同一身份和零token，验证Operator持久化信任。

每阶段10个采样、1MiB发送/接收、校验过的bulk回写，单次peer-only测试最多15秒；总预算默认2分钟，可设30秒至3分钟。每阶段完成后默认保活5秒供外部观测，`--hold`允许0至15秒。会话取消和关闭后汇合传输及session；退出码、cleanup、固定错误码和字节计数共同决定是否成功。无新增依赖，不改变协议或匿名诊断schema。

stdout为独立`wancheck/v1` JSON，只有固定阶段、状态、采样/字节和RTT；不输出邀请、指纹、地址、私钥、路径或原始错误。只有两个阶段全部成功、清理确认且rejectedTunnels=0才success=true。该结果不能单独证明跨公网：内网NAT回环也可能通过，须结合用户确认手机热点、Mac路由记录及Operator侧实际连接来源；也不能作为完整Client NIC/发现、真实车辆、MGU或业务验收。

默认、help和version不读stdin、不创建状态或拨号；自动化测试仅使用loopback。
