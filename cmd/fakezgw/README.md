# 假 ZGW 台架

这是传输层测试工具，不仿真完整车辆识别，也不证明真实 ZGW 的定时器、容量或 tester 隔离行为。三个默认监听端点为 `6801/tcp`、`6811/tcp`、`6811/udp`。所有连接与收发帧（十六进制）写入 stderr。

```sh
go run ./cmd/fakezgw --bind 127.0.0.1 --profile no-alive

go run ./cmd/fakezgw --bind 192.168.1.50 --vin WBAXXXXXXX0000001 \
  --profile long-alive --alive 5s --alive-timeout 2s --kl15 04
```

第二例的周期、超时和 KL15 都是显式选择的模拟值，不是实车事实。VIN/MAC 默认值来自脱敏 fixture。`--tester` 默认 `0xF4`，可通过 `--diag-addr`、`--mac` 修改发现字段。

| profile | 行为 |
|---|---|
| `normal` | 按显式参数发送短 alive；默认 `--alive 0` 禁用 |
| `no-alive` | 禁用 alive，即使传入非零周期 |
| `drop-alive` | 未按时收到正确 tester 应答则断连；要求显式正周期与超时 |
| `long-alive` | 发送 32 字节 ASCII alive；要求显式正周期 |
| `error-frame` | 每次诊断连接先发 `0x0041 L=0`，之后正常处理 |

短 alive 的正文内容缺乏确定证据；启用时必须传 `--short-alive-body`（两个字节的十六进制，如测试值 `1234`）。`--alive-timeout 0` 不因缺应答断开。`--kl15` 未提供时控制端口接受连接但不回复 KL15 查询；未确认的控制端口 `0x0013` 行为不仿真。

`--alive-response-control` 默认仍为 `0x13`。Windows 台架中的 E-Sys 3.36.2 曾在长 alive 后发送 `00 00 00 02 00 12 00 F4`；可显式选择 `--alive-response-control 0x12` 做对照，此时只接受这个控制字和匹配 tester 的两字节正文，不同时放宽到两种控制字。该选项只影响假网关，不修改生产隧道，也不把单个版本的观察当作所有车辆的协议事实。详见 [M0b 实测](../../docs/research/m0b-win11-2026-11-16.md)。

未知 UDS 请求默认只收到传输 ACK，后续保持沉默。可用 `--unknown-nrc 11` 显式选择模拟负响应，或通过 `--responders path.json` 提供请求/响应十六进制映射：

```json
{"22f101": "62f10100"}
```

回放内容由测试者明确提供；工具不会把任意请求自动判为成功。`--max-conns` 为所有 TCP 通道的同时连接上限，零表示不限。`--diag-port`、`--ctrl-port`、`--disc-port` 可独立覆盖，零表示系统分配临时端口。

对功能寻址或需要固定物理源地址的实验，用独立的 `--multi-responders path.json`。其条目优先于普通 responders，`source` 是显式的十进制字节；空列表表示该请求不回诊断响应，不回退到普通 responders：

```json
{"22f186": [{"source": 16, "payload": "62f18601"}]}
```

随仓库提供的 `fixtures/m0b-readonly.json` 仅有网关 `0x10` 的两个只读回包：`22 F1 86` 的 `62 F1 86 01` 来自 `mgu_kit/hsfz_captures/analysis/hsfz_esys_id8_write_ncd_20261102_141846.tsv` 第 145 行（网关到 tester 的实际响应）；`22 F1 90` 使用脱敏模拟 VIN。它不是完整 ECU 列表或车辆识别仿真，也不包含编码/刷写请求。通过显式物理 source，避免把功能地址 `DF` 错当成应答 ECU。

进程内使用 `fakezgw.Start(ctx, bind, profile, fakezgw.WithPorts(0, 0, 0))`；退出时调用 `Close()` 等待连接 worker 结束。`Stats()` 返回累计连接数、当前连接数、拒绝数、诊断请求数、ACK、alive 与有效应答数。功能寻址多 ECU 回包通过 `Profile.MultiResponders` 显式指定物理源地址。其他故障通过 `Profile.Faults` 配置 RST、半关闭和空 `0x0041/0x00FF`。

Windows VM 的发现收发与部分真实工具连接已实测，见 M0b 记录；完整车辆诊断、真实网关并发路由及定时器仍未验收。不实现 ICOM/DoIP、HTTP 回连、车辆编码或刷写业务。
