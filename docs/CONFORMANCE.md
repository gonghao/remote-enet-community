# 一致性测试向量

记录日期：2026-11-20。本文规定 [CENTER-CONTRACT 第 15 节](CENTER-CONTRACT.md#15-一致性测试向量计划) 编码与签名向量（V1–V16）的发布格式。行为场景（S、P、C 组）需要 Hub 中心模式与中心假实现，属于 R5，不在本文范围。

## 1. 目标

- 第三方中心（包括官方中心）用同一份向量验证自己的实现与 Go 端逐字节兼容。
- 每组向量都有两份独立实现互相印证：`gen/` 下的生成器只用 Python 标准库与 OpenSSL 命令行，不调用、不导入 Go 代码；Go 端测试读取同一份 JSON 断言自己的实现。两边任何一方改错，CI 都会失败。
- 中心仓库按版本标签拉取本目录，不复制。向量随契约修订一起更新，文件内的 `contract` 字段写明对应的契约版本。

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
    <每组一个生成器>.py
    run_all.py         重新生成全部文件；--check 时只比较不写入，有差异退出 1
```

V16（自建票据）已在 `internal/ticket/testdata/` 发布，本目录 README 给出链接，不重复。

## 3. 文件格式

每个文件是 UTF-8 JSON，键名小写下划线，两空格缩进，末尾换行，由生成器输出（不要手改）：

```json
{
  "format": 1,
  "contract": "CENTER-CONTRACT v1 第八版（2026-11-20）",
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

- 字节串一律小写 hex；文本保持原样；时间为 Unix 秒整数。
- 拒绝类用例的 `expect` 为 `{"result": "reject", "reason": "<短标识>"}`，`reason` 写契约中的步骤或错误码（例如 `8.4/5`、`code_invalid`），供实现者定位，不要求实现返回同样的字符串。接受类用例为 `{"result": "accept", ...}` 加需要逐字节比对的输出。
- 用例名在文件内唯一，使用小写短横线。

## 4. 测试密钥与时间

- 所有密钥由固定种子派生：Ed25519 私钥种子 = SHA-256(`"renet-conformance-v1/" + 名称`)，名称如 `center-signing-1`、`device-operator`、`device-client`、`hub-node-1`。README 列出每个名称及其公钥，第三方可自行复算。
- 固定时间 `now = 1795000000`（2026-11-18T11:06:40Z）；其余时间都按它的偏移给出。
- 这些密钥只用于测试，任何部署都不得使用。

## 5. Go 端与 CI

- 每个被测 Go 包在自己的 `_test.go` 中读取 `testdata/conformance/` 下对应文件（以仓库根为基准定位），逐用例断言。
- CI 在 check 任务中运行 `python3 -B testdata/conformance/gen/run_all.py --check`，确认提交的 JSON 可由生成器逐字节复现。
