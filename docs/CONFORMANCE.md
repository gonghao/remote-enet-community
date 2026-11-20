# 一致性测试向量

记录日期：2026-11-20。本文规定 [CENTER-CONTRACT 第 15 节](CENTER-CONTRACT.md#15-一致性测试向量计划) 编码与签名向量（V1–V16）的发布格式。行为场景（S、P、C 组）需要 Hub 中心模式与中心假实现，属于 R5，不在本文范围。

## 1. 目标

- 第三方中心（包括官方中心）用同一份向量验证自己的实现与 Go 端逐字节兼容。
- 每组向量都有两份独立实现互相印证：`gen/` 下的生成器只用 Python 标准库与 OpenSSL 命令行（V6/V7 复用的 CPace 独立实现另经 ctypes 加载 libsodium），不调用、不导入 Go 代码；Go 端测试读取同一份 JSON 断言自己的实现。两边任何一方改错，CI 都会失败。
- 中心仓库按版本标签拉取本目录，不复制。向量随契约修订一起更新，文件内的 `contract` 与 `contract_version` 字段写明对应的契约版本。

## 2. 目录

```
testdata/conformance/
  README.md            面向第三方实现者：如何使用、字段约定、测试密钥说明
  v01-account-code.json
  v02-numeric-code.json
  v03-request-signature.json
  v04-ticket.json      合法票据，含 V5 kid 推导
  v06-pake-ci.json
  v07-pairing-messages.json
  v08-ticket-reject.json   V8–V15 合并为一个文件，每个用例标明编号
  gen/
    common.py          共用：确定性 CBOR 编码、hex、固定测试密钥、OpenSSL Ed25519 签名
    codes.py           V1/V2
    request.py         V3
    ticket.py          V4/V5 与 V8–V15
    pake.py            V6/V7，调用 internal/pairing/cpace/testdata/gen/application.py
    keys.py            刷新并核对 README 的公钥表
    test_common.py     公共工具单测
    run_all.py         重新生成全部文件；--check 时只比较不写入，有差异退出 1
```

V16（自建票据）已在 `internal/ticket/testdata/` 发布，本目录 README 给出链接，不重复。

## 3. 文件格式

每个文件是 UTF-8 JSON，键名小写下划线，两空格缩进，末尾换行，由生成器输出（不要手改）：

```json
{
  "format": 1,
  "contract": "CENTER-CONTRACT v1 第九版（2026-11-20）",
  "contract_version": "v1.9",
  "vector": "V4",
  "title": "合法中心票据",
  "generator": "gen/ticket.py",
  "cases": [
    {
      "name": "max-size",
      "note": "各字段取最大长度",
      "input": { },
      "expect": { }
    }
  ]
}
```

- `contract` 是给人看的契约版本文本；`contract_version` 是机器可读的契约版本，取 `v1.<修订版次>`（第九版为 `v1.9`）。实现方按 `contract_version` 判断向量对应的修订，不解析 `contract` 文本。
- 字节串一律小写 hex；文本保持原样；时间为 Unix 秒整数。
- 拒绝类用例的 `expect` 为 `{"result": "reject", "reason": "<短标识>"}`，`reason` 写契约中的步骤或错误码（取值见第 7 节），供实现者定位，不要求实现返回同样的字符串。接受类用例为 `{"result": "accept", ...}` 加需要逐字节比对的输出。
- 用例名在文件内唯一，使用小写短横线。

## 4. 测试密钥与时间

- 所有密钥由固定种子派生：Ed25519 私钥种子 = SHA-256(`"renet-conformance-v1/" + 名称`)，名称如 `center-signing-1`、`device-operator`、`device-client`、`hub-node-1`。README 列出每个名称及其公钥，第三方可自行复算。生成器的做法是把种子接在 Ed25519 PKCS#8 DER 前缀 `302e020100300506032b657004220420` 之后交给 OpenSSL，公钥取 SPKI DER 的最后 32 字节。在 bash 中可以这样复算（把 `device-client` 换成其他名称）：

  ```sh
  { printf '\x30\x2e\x02\x01\x00\x30\x05\x06\x03\x2b\x65\x70\x04\x22\x04\x20'
    printf '%s' 'renet-conformance-v1/device-client' | openssl dgst -sha256 -binary
  } | openssl pkey -inform DER -pubout -outform DER | tail -c 32 | od -An -tx1 | tr -d ' \n'
  ```
- 固定时间 `now = 1795000000`（2026-11-18T11:06:40Z）；其余时间都按它的偏移给出。
- 这些密钥只用于测试，任何部署都不得使用。

## 5. Go 端与 CI

- 每个被测 Go 包在自己的 `_test.go` 中读取 `testdata/conformance/` 下对应文件（以仓库根为基准定位），逐用例断言。
- CI 的 check 任务先安装 `libsodium23`（CPace 生成器需要 ristretto255），再对全部生成器做复现检查：既有 CPace 应用向量（`internal/pairing/cpace/testdata/gen/application.py --check`）、自建票据夹具（`internal/ticket/testdata/gen/generate.py --check`）、本目录生成器的 Python 单测，以及 `python3 -B testdata/conformance/gen/run_all.py --check`。`run_all.py` 扫描 `gen/` 下全部生成器，其中 `pake.py` 调用 CPace 独立实现重新生成 V6/V7，`keys.py` 核对 README 的公钥表（普通运行时刷新它）。任何提交的 JSON 或 README 公钥表不能被逐字节复现，CI 即失败。

## 6. 各文件字段

本节按 JSON 与 `gen/` 生成器的实际输出列出字段。共同约定：字节串为小写 hex；测试密钥字段的值是第 4 节的种子名称而不是密钥本身，字段名统一为 `key`；所有 `expect` 都有 `result`（`accept` / `reject`），拒绝时另有 `reason`（第 7 节），下表只列其余字段。

### v01-account-code.json（V1，§4.1）

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | （整个 `input`） | 文本 | 用户输入的原始账号码，可能含空白、`-` 与非法字符 |
| expect | `normalized` | 文本 | 规范化后的 8 个符号 |
| expect | `display` | 文本 | 显示形式 `XXXX-XXXX` |

### v02-numeric-code.json（V2，§4.2）

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | （整个 `input`） | 文本 | 用户输入的原始数字码 |
| expect | `normalized` | 文本 | 规范化后的 8 位 ASCII 数字，保留前导零 |

### v03-request-signature.json（V3，§5.1、§6.4）

`device-request` / `node-request` 为合法签名；`target-*` 只有 `raw_url`，测请求目标规范化；`time-past-60`、`time-future-60` 是时间窗边界上的合法签名，`expect` 与合法签名相同；其余用例都是拒绝用例，在 `input.headers` 中给出中心收到的请求头。

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | `key` | 文本 | 签名私钥的种子名称：设备为 `device-operator`，节点为 `hub-node-1` |
| input | `center_id` | 文本 | 签名输入中的 `center_id` |
| input | `kind` | 文本 | `device` 或 `node` |
| input | `principal` | hex | 设备公钥（32 字节）或 `node_id`（16 字节） |
| input | `method` | 文本 | 大写 HTTP 方法 |
| input | `target` | 文本 | 客户端签名所用的请求目标；拒绝用例中可能是非规范或不合法的形式 |
| input | `timestamp` | 整数 | 签名时间戳（Unix 秒） |
| input | `renet_client` | 文本 | `Renet-Client` 的值 |
| input | `nonce` | hex | `Renet-Nonce` 的原始字节，空串表示不带 |
| input | `idempotency_key` | 文本 | `Idempotency-Key`，空串表示不带 |
| input | `body` | hex | 原始请求体，空串表示没有 |
| input | `public_key` | hex | 仅节点用例：验签公钥，来自节点激活时登记的 `node_pub`（§7.8），不是请求头中的值 |
| input | `headers` | 对象：头名 → 文本数组 | 仅拒绝用例：中心收到的请求头，同名头出现多次时数组有多项 |
| input | `server_time`、`request_window` | 整数 | 仅 `time-*` 用例：中心时间与时间窗（秒） |
| input | `raw_url` | 文本 | 仅 `target-*` 用例：完整 URL 或 origin-form 请求目标 |
| expect | `signing_input` | hex | §5.1 签名输入的确定性 CBOR 编码 |
| expect | `signature` | hex | Ed25519 签名 |
| expect | `headers` | 对象：头名 → 文本数组 | 客户端应发送的签名相关请求头 |
| expect | `canonical_target` | 文本 | 仅 `target-*` 接受用例：规范化后的请求目标 |

### v04-ticket.json（V4/V5，§8.1–§8.3）

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | `key` | 文本 | 中心票据签名私钥的种子名称（`center-signing-1`） |
| input | `public_key` | hex | 对应的 Ed25519 公钥 |
| input | `claims` | 对象 | 期望的票据字段，键为 §8.2 的字段名（`v`、`center`、`acct`、`dev`、`role`、`peer`、`sid`、`node`、`hub`、`iat`、`nbf`、`exp`、`jti`、`gen`）；整数字段为整数，`center` 为文本，其余为 hex。`exp` 已按 §8.2 公式算出，供比对 |
| input | `ticket_ttl` | 整数 | 签发策略的 `ticket_ttl`（秒） |
| input | `hard_deadline` | 整数 | 会话硬截止（Unix 秒） |
| input | `clock_skew`、`ticket_max_ttl` | 整数 | 秒 |
| input | `now` | 整数 | 签发时间，等于 `iat` 与 `nbf` |
| expect | `kid` | hex | V5：SHA-256(公钥) 前 8 字节 |
| expect | `protected` | hex | protected 字节串的内容 |
| expect | `payload` | hex | payload 字节串的内容 |
| expect | `sig_structure` | hex | COSE `Sig_structure` 编码 |
| expect | `signature` | hex | 64 字节签名 |
| expect | `token` | hex | 完整票据（tag 18） |
| expect | `length` | 整数 | `token` 字节数 |

### v06-pake-ci.json（V6，§10.1、§10.6、§10.7）

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | `capability` | 文本 | `pair-direct/1`、`pair-hub/1` 或 `pair-center/1`，用例名分别为 `direct`、`hub`、`center` |
| input | `fp_client`、`fp_operator` | hex | 内层 TLS 实际看到的双方设备指纹（32 字节） |
| input | `hc`、`ho` | hex | Client、Operator `Hello` 帧的原始字节，含 4 字节长度前缀 |
| input | `hub_fp` | hex | 仅 `hub`、`center`：Hub 指纹 |
| input | `center_id` | 文本 | 仅 `center`：按 ASCII 字节进入 CI |
| input | `session_id` | hex | 仅 `center`：16 字节会话 ID |
| expect | `hc_sha256`、`ho_sha256` | hex | `hc`、`ho` 的 SHA-256 |
| expect | `ci` | hex | §10.1 的 CI（`lv_cat` 结果） |

### v07-pairing-messages.json（V7，§10.1、§10.2）

用例 `direct`、`hub`、`center` 与 V6 同名用例的 CI 对应。

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | `prs` | hex | 口令：规范化数字码的 ASCII 字节 |
| input | `ci` | hex | CI |
| input | `sid` | hex | 内层 TLS exporter 输出（32 字节） |
| input | `ya`、`yb` | hex | 发起方、响应方的私有标量（32 字节小端） |
| expect | `commit` | hex | 消息 11 的承诺 |
| expect | `isk` | hex | ISK |
| expect | `tb`、`ta` | hex | 确认标签（HMAC-SHA-512，64 字节） |
| expect | `sas` | 文本 | 6 位 SAS，保留前导零 |
| expect | `cbor11` … `cbor16` | hex | 消息 11–16 的 CBOR 信封 `{1: 消息号, 2: 正文}`，不含 4 字节帧长；消息 16 为 `accepted = true` |

### v08-ticket-reject.json（V8–V15，§8.4 第 1–9 步）

用例名前缀标明所属编号（`v8-` … `v15-`）。

| 位置 | 字段 | 类型 | 含义 |
|---|---|---|---|
| input | `token` | hex | 待校验的路由头 token，可能为空 |
| input | `config` | 对象 | 本节点配置：`center_id`（文本）、`node_id`（hex）、`hub`（hex，本节点身份指纹）、`gen`（整数，本进程代次）、`keys`（数组，每项 `{kid, public_key}`，hex）、`clock_skew` 与 `ticket_max_ttl`（整数，秒） |
| input | `route` | 对象 | 路由头：`role`（整数，1 = dial，2 = wait）、`target`（hex，对端指纹） |
| input | `tls_device` | hex | 本连接 TLS 客户端证书算出的设备指纹 |
| input | `now` | 整数 | Hub 当前时间（Unix 秒） |
| expect | — | — | 接受用例只有 `result`；拒绝用例另有 `reason`（`8.4/N`） |

## 7. reason 枚举

| 文件 | `reason` | 含义 |
|---|---|---|
| v01 | `4.1/length` | 规范化后不是恰好 8 个符号 |
| v01 | `4.1/character` | 含字母表以外的字符；与长度错误同时成立时取此值 |
| v02 | `4.2/length` | 规范化后不是恰好 8 位数字 |
| v02 | `4.2/character` | 含非数字字符；与长度错误同时成立时取此值 |
| v03 | `bad_request` | 请求目标不合法、签名相关头重复或含逗号、`Renet-Timestamp` 文本不规范 |
| v03 | `unauthenticated` | 验签失败，包括对非规范请求目标签名、请求头被改动 |
| v03 | `clock_skew` | 时间戳超出 `request_window` |
| v08 | `8.4/N` | 中心票据在 §8.4 第 N 步（1–9）被拒绝；Hub 对外一律回 `unauthorized`。Go 端以 `ticket.StepError` 的 `Step` 字段表示，取值同为 `8.4/N` |
