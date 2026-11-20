# 中心契约一致性向量

第三方实现可按仓库版本标签读取向量，先核对顶层的 `format` 和 `contract_version`（机器可读，如 `v1.9`；`contract` 为对应的展示文本），再遍历 `cases`。每例从 `input` 运行自己的实现，按字段比对 `expect`。字节串按小写 hex 解码，文本和 SAS 保留原样。时间使用 Unix 秒整数。完整格式见[发布约定](../../docs/CONFORMANCE.md#3-文件格式)，每个文件的 `input` / `expect` 字段见[各文件字段](../../docs/CONFORMANCE.md#6-各文件字段)，拒绝原因见 [reason 枚举](../../docs/CONFORMANCE.md#7-reason-枚举)。规则本身以中心契约为准，各文件对应的章节列在下表。

| 文件 | 用法 |
|---|---|
| [v01-account-code.json](v01-account-code.json) | 对原始输入按[契约 §4.1](../../docs/CENTER-CONTRACT.md#41-账号码) 规范化，比对 `normalized` 和 `display`。四个 `alphabet-*` 用例合计覆盖全部 32 个合法符号。 |
| [v02-numeric-code.json](v02-numeric-code.json) | 对原始输入按[契约 §4.2](../../docs/CENTER-CONTRACT.md#42-数字码) 规范化，按字符串比对 `normalized`，保留前导零。 |
| [v03-request-signature.json](v03-request-signature.json) | 按[契约 §5.1](../../docs/CENTER-CONTRACT.md#51-签名请求) 与 [§6.4](../../docs/CENTER-CONTRACT.md#64-时间) 重算设备和节点请求的签名输入、签名及请求头，核对请求目标规范化、验签拒绝、时间窗边界与时间戳文本。节点用例的 `public_key` 取激活登记的公钥。 |
| [v04-ticket.json](v04-ticket.json) | V4/V5：按[契约 §8.1–§8.3](../../docs/CENTER-CONTRACT.md#8-票据) 从测试密钥与签发策略重新签发 dial、wait 票据，比对 kid、签名输入与整张票据；含硬截止夹取 `exp` 与 `ticket_max_ttl` 等号边界，最大尺寸为 326 字节。 |
| [v06-pake-ci.json](v06-pake-ci.json) | 按[契约 §10.1](../../docs/CENTER-CONTRACT.md#101-pake-输入) 分别重算直连、自建 Hub、中心模式的 CI。对完整的 `hc`、`ho`（含四字节帧长）做 SHA-256，按协商能力追加绑定字段。中心的 `center_id` 取 ASCII 字节。 |
| [v07-pairing-messages.json](v07-pairing-messages.json) | 按[契约 §10.2](../../docs/CENTER-CONTRACT.md#102-sas-与承诺)，直连、自建 Hub 与中心模式各一轮。将 `ya`、`yb` 作为小端私有标量，由 `prs`、`sid`、`ci` 推导双方 share、承诺、ISK、确认标签与 SAS。`cbor11` 到 `cbor16` 仅含 CBOR 信封；消息 16 表示接受。 |
| [v08-ticket-reject.json](v08-ticket-reject.json) | V8–V15：按每例的节点配置、路由头、TLS 指纹和时间运行[契约 §8.4](../../docs/CENTER-CONTRACT.md#84-hub-校验) 第 1–9 步的锁外校验，比对接受结果或拒绝步骤。 |
| [V16 自建票据](../../internal/ticket/testdata/) | wait、dial 与 bootstrap 三份 COSE 票据及独立生成器。 |
| [gen/](gen/) | 公共编码与签名工具、各组生成器、统一入口与 Python 单测。 |

目录与格式约定见 [CONFORMANCE](../../docs/CONFORMANCE.md#2-目录)，向量计划见[契约 §15](../../docs/CENTER-CONTRACT.md#15-一致性测试向量计划)。

## 测试密钥

Ed25519 种子取 UTF-8 字符串 `renet-conformance-v1/` 加名称的 SHA-256。下表为原始公钥，复算命令见 [CONFORMANCE 第 4 节](../../docs/CONFORMANCE.md#4-测试密钥与时间)。向量中 `key` 字段的值就是下表的名称。种子公开，仅供测试。固定时间为 `1795000000`。V6/V7 的指纹和 TLS exporter 沿用既有 CPace 测试输入。

<!-- public-keys:start -->

| 名称 | 原始公钥（hex） |
|---|---|
| `center-signing-1` | `0426e7503c3184fe5b49cc4c7d6e35be402022f8c0ab7169940c8625145ca76e` |
| `device-operator` | `b01a6d5bcafb46f5499121df645baa5b375bce3b96bcc867ce85cbc42ad2043d` |
| `device-client` | `77a2582f4cb85a2ea130cd7ab38c10c2fc6e926c98a1f6fb8cdcf3a847c77327` |
| `hub-node-1` | `5ba9c5882d78400216a26cb9ab33ed9f028f049fbd7aff11a75888154f2f86fb` |

<!-- public-keys:end -->

## 重新生成

依赖与 CI 的 check 任务一致：Python 3.10+ 标准库、支持 Ed25519 的 OpenSSL 3.x 命令行（零字节消息改用 `openssl dgst`，已验证 3.5.6），以及可由系统动态加载、含 ristretto255 的 libsodium 1.0.18+（Debian / Ubuntu 的 `libsodium23`）。V6/V7 由 `gen/pake.py` 调用[既有 CPace 独立实现](../../internal/pairing/cpace/testdata/gen/application.py)生成。

在仓库根目录运行：

```sh
python3 -B testdata/conformance/gen/run_all.py
python3 -B testdata/conformance/gen/run_all.py --check
python3 -B -m unittest discover -s testdata/conformance/gen -p 'test_*.py'
```

入口按文件名排序扫描 `gen/*.py`，排除 `common.py` 和 `run_all.py`，调用模块的 `generate(check: bool)`。生成器把 `common.write_vector` 返回的差异路径列表交给入口，检查模式逐字节比较、列出缺失或不同的文件并退出 1。缺少依赖时直接报错，不跳过任何生成器。

CI 的 check 任务安装 `libsodium23` 后依次运行 `internal/pairing/cpace/testdata/gen/application.py --check`、`internal/ticket/testdata/gen/generate.py --check`、上面的 Python 单测与 `run_all.py --check`，全部生成器（含 V6/V7 与 README 公钥表）都在 CI 中逐字节复现；各 Go 包的测试再读取同一份 JSON 逐例验证。
