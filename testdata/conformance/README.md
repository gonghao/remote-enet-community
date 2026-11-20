# 中心契约一致性向量

第三方实现可按仓库版本标签读取向量，先核对顶层的 `format` 和 `contract`，再遍历 `cases`。每例从 `input` 运行自己的实现，按字段比对 `expect`。字节串按小写 hex 解码，文本和 SAS 保留原样。时间使用 Unix 秒整数。完整格式见[发布约定](../../docs/CONFORMANCE.md)，各字段含义见[中心契约 §10](../../docs/CENTER-CONTRACT.md#10-中心模式的首次配对)。

| 文件 | 用法 |
|---|---|
| [v01-account-code.json](v01-account-code.json) | 对原始输入按 §4.1 规范化，比对 `normalized` 和 `display`。四个 `alphabet-*` 用例合计覆盖全部 32 个合法符号。 |
| [v02-numeric-code.json](v02-numeric-code.json) | 对原始输入按 §4.2 规范化，按字符串比对 `normalized`，保留前导零。 |
| [v03-request-signature.json](v03-request-signature.json) | 重算设备和节点请求的签名输入、签名及请求头，核对请求目标规范化与验签拒绝用例。 |
| [v04-ticket.json](v04-ticket.json) | V4/V5：从测试密钥与字段重新签发 dial、wait 票据，比对 kid、签名输入与整张票据；最大尺寸为 326 字节。 |
| [v06-pake-ci.json](v06-pake-ci.json) | 分别重算直连、自建 Hub、中心模式的 CI。对完整的 `hc`、`ho`（含四字节帧长）做 SHA-256，按协商能力追加绑定字段。中心的 `center_id` 取 ASCII 字节。 |
| [v07-pairing-messages.json](v07-pairing-messages.json) | 直连与自建 Hub 各一轮。将 `ya`、`yb` 作为小端私有标量，由 `prs`、`sid`、`ci` 推导双方 share、承诺、ISK、确认标签与 SAS。`cbor11` 到 `cbor16` 仅含 CBOR 信封；消息 16 表示接受。 |
| [v08-ticket-reject.json](v08-ticket-reject.json) | V8–V15：按每例的节点配置、路由头、TLS 指纹和时间运行锁外校验，比对接受结果或拒绝步骤。 |
| [V16 自建票据](../../internal/ticket/testdata/) | wait、dial 与 bootstrap 三份 COSE 票据及独立生成器。 |
| [gen/](gen/) | 公共编码与签名工具、各组生成器、统一入口与 Python 单测。 |

目录与格式约定见 [CONFORMANCE](../../docs/CONFORMANCE.md#2-目录)。

## 测试密钥

Ed25519 种子取 UTF-8 字符串 `renet-conformance-v1/` 加名称的 SHA-256。下表为原始公钥。种子公开，仅供测试。固定时间为 `1795000000`。V6/V7 的指纹和 TLS exporter 沿用既有 CPace 测试输入。

<!-- public-keys:start -->

| 名称 | 原始公钥（hex） |
|---|---|
| `center-signing-1` | `0426e7503c3184fe5b49cc4c7d6e35be402022f8c0ab7169940c8625145ca76e` |
| `device-operator` | `b01a6d5bcafb46f5499121df645baa5b375bce3b96bcc867ce85cbc42ad2043d` |
| `device-client` | `77a2582f4cb85a2ea130cd7ab38c10c2fc6e926c98a1f6fb8cdcf3a847c77327` |
| `hub-node-1` | `5ba9c5882d78400216a26cb9ab33ed9f028f049fbd7aff11a75888154f2f86fb` |

<!-- public-keys:end -->

## 重新生成

使用 Python 3.10+ 标准库和支持 Ed25519 的 OpenSSL 3.x 命令行（与票据生成器一致）。零字节消息使用支持 Ed25519 的 `openssl dgst`（已验证 3.5.6）；票据及请求签名输入均为非空 CBOR。完整再生成还需要可由系统动态加载的 libsodium 1.0.18+，含 ristretto255。配对生成器复用[既有独立实现](../../internal/pairing/cpace/testdata/gen/application.py)。

在仓库根目录运行：

```sh
python3 -B testdata/conformance/gen/run_all.py
python3 -B testdata/conformance/gen/run_all.py --check
python3 -B -m unittest discover -s testdata/conformance/gen -p 'test_*.py'
```

入口按文件名排序扫描 `gen/*.py`，排除 `common.py` 和 `run_all.py`，调用模块的 `generate(check: bool)`。生成器把 `common.write_vector` 返回的差异路径列表交给入口，检查模式逐字节比较、列出缺失或不同的文件并退出 1。普通运行缺少依赖会报错。

CI 运行公共工具单测及统一检查。既有 CPace 生成器仅在本地运行，因此 CI 检查显式设置 `CONFORMANCE_LOCAL_PAKE=1`，省略 V6/V7 的 Python 再生成。两份已发布向量均由 Go 测试在 CI 中逐例验证。
