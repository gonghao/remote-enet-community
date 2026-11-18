# M0 调研任务书：用公开资料替代实车抓包

> 目标：把 `docs/DESIGN.md` 里所有标 ⚠ 的待验证项，尽可能用公开资料、开源代码和 `~/src/mgu_kit` 里已有的抓包回答掉，并明确哪些**只能上台架/实车验证**。
> 产出：`docs/enet-protocol-research.md`（中文，标识符/字节/命令保持英文）。

## 规则

- 每个问题给出：**结论 → 置信度（高/中/低）→ 证据（URL 或本地文件路径+行号）→ 仍需实测的部分**。证据不足就写「未知」，不要编。
- 优先级：开源实现源码 > 真实抓包 > 官方/半官方文档 > 论坛经验。论坛经验必须标注来源和年份。
- 不要修改 `~/src/mgu_kit` 和 `docs/DESIGN.md`。临时脚本放 `docs/research/scratch/`。
- 只做协议与工具行为研究。不需要、也不要写任何刷写/编码操作步骤。

## 建议先看的资料

- 本地：`docs/DESIGN.md`（找所有 ⚠）；`~/src/mgu_kit/mgu-go/internal/mgudiag/{discovery.go,hsfz.go,diag.go}`；`~/src/mgu_kit/hsfz_captures/`（pcap + `analysis/REPORT.zh-CN.md`、`CAPTURE_ANALYSIS.md`、`decode_captures.py`）。
- GitHub：`uholeschak/ediabaslib`（`EdiabasLib/EdInterfaceEnet.cs` 是最完整的 HSFZ + DoIP 开源实现，含发现、控制字、alive check、多连接行为）；`rawmind0/rawenet`（`pkg/hsfz/`、`docs/hsfz-protocol.md`）；其他搜 `HSFZ 6811`、`BMW ENET discovery`、`DoIP 13400 BMW`。
- 文档：`emdzej.github.io/ediabasx-docs`（xenet32 分析）；EdiabasLib 的 `EdiabasLib.config` 文档；E-Sys 用户手册中 Connection / Ethernet ECU / HTTP 相关章节；`remote-enet.com` 页面文字（它暗示了商业实现的做法）。

## 问题清单

### Q1 HSFZ 发现（UDP 6811）
- 请求帧精确字节与语义（`00 00 00 00 00 11` 是什么控制字）。
- 响应帧完整布局：控制字、payload 的键值格式（`DIAGADDR`、`BMWVIN`、`BMWMAC` 等）、各字段编码、长度。**payload 里是否包含 IP？**
- 请求是广播到 `255.255.255.255` / `169.254.255.255` / 定向广播，还是也接受单播？响应是单播回源端口吗？
- 给出一份可直接回放的响应样本（十六进制），来源注明。

### Q2 HSFZ TCP 帧（6801）
- 所有已知控制字及含义（diag、ack、alive check、ping、error/reject 等），长度字段的计算口径，src/dst 地址字节。
- 网关对 ack/alive check 的行为：是否每个 diag 请求都回 ack；alive check 的周期和超时；**长时间无流量会不会被网关断开**（影响隧道保活）。
- 对比 mgu_kit `hsfz.go` 的常量，列出差异。

### Q3 ZGW 并发连接
- 网关是否允许多条 6801 TCP 连接同时存在？是否按 tester 地址（`F4` 等）区分会话？
- 用 `~/src/mgu_kit/hsfz_captures/` 的 pcap 数一数：E-Sys 一次会话建了几条 TCP、分别到哪些 IP:port、用了哪些 src 地址。
- 结论要能回答设计里的问题：Operator 自己的车况轮询（电压/PAD）用另一条连接、另一个 tester 地址，会不会干扰 E-Sys。

### Q4 E-Sys 连接模式与行为
- E-Sys 的连接方式有哪些（Gateway URL、ENET、WiFi ENET、ICOM、直连 Ethernet ECU …），各自的发现/地址要求。
- **WiFi ENET / 非 link-local 网段的网关，E-Sys 是否接受自动发现？**（rawenet README 声称可以）
- E-Sys 发现时发的是什么包、发到哪些地址、监听哪个端口收响应。
- Ethernet ECU 直连：E-Sys 怎么得到 ECU 的 IP（UDS DID？发现响应？），连 ECU 的哪个端口。
- **HTTP 刷写模式**：E-Sys 在本机起 HTTP server 的端口；ECU 通过什么机制得知要回连的 URL/IP（UDS RequestDownload 的 payload？专有服务？）；E-Sys 用哪个本地 IP 填进去（出站连接的源地址？指定网卡？）。这是设计里最关键的未知项（DESIGN.md 4.5 「IP 镜像」）。

### Q5 ISTA 的 ENET 发现
- ISTA 用 ENET 连车时的发现机制（6811 广播？经 EDIABAS XEthernet？），是否要求响应方在 `169.254/16`，是否同时发 DoIP 13400 发现。
- ISTA 找 ICOM 的 SLP 细节（端口 427 还是 remote-enet 提到的 50160，组播地址）。
- ISTA 连上后是否持续轮询网关（影响「独占」）。

### Q6 Tool32 / INPA（EDIABAS XEthernet）
- `EDIABAS.ini` 的 `[XEthernet]` 完整参数与 `RemoteHost=Autodetect` 的发现流程。
- EdiabasLib 的 `RemoteHost=ip:protocol:diag:ctrl` 格式细节。

### Q7 DoIP（G 系）
- BMW 工具对 G 系走 ENET 线的流程：是否仍是 HSFZ，还是 DoIP 13400；车端需要 DHCP 服务器吗（谁给谁分地址）；Vehicle Identification Request/Response 和 Announcement 的字节布局；是否用到 TLS 3496。
- 响应里是否包含 IP（影响 Presenter 要不要改写）。

### Q8 MGU 等以太网 ECU
- MGU IP 的获取方式（mgu_kit 用 `22 17 2A`，确认 DID 含义与返回格式）；MGU 上开放的端口（SSH 22、HTTP 刷写端口、其他）。
- RAM/MGU 这类 Ethernet ECU 的地址分配（固定 link-local？由网关分配？）。

### Q9 ICOM
- SLP 发现报文与端口；ICOM 作为网关时对 tester 的行为是否与 ZGW 相同。

## 额外产出

1. **假 ZGW 规格**：根据 Q1–Q3，写出 `internal/testutil/fakezgw` 需要实现的最小行为（发现响应样本、对 diag 的 ack 规则、alive check 规则），让项目不用真车就能做 e2e 测试。
2. **仍需实测清单**：哪些问题公开资料回答不了，分别需要什么环境（假 ZGW + E-Sys 即可 / 需要 F20 / 需要 MGU 台架），按成本排序。
3. 把 `docs/DESIGN.md` 里每个 ⚠ 对应到本报告的章节，给出建议的改写结论（不要直接改 DESIGN.md）。
