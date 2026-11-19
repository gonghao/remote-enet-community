generator.json、exchange.json、points.json 分别解码自 [CPace -21 附录 B.3.1.1、B.3.9、B.3.11.1](https://www.ietf.org/archive/id/draft-irtf-cfrg-cpace-21.txt) 的 Base64 JSON。保留原字段，包括本套件未启用的并行模式和可选会话标识。

应用向量 `application.json` 对应中心契约 §10.1、§10.2、§10.6 和 §15 的 V6/V7。顶层字段描述直连配对，`center` 给出中心模式的 Hello、附加字段和 CI。两种模式共用 fpA、fpB。Hello 包含大端长度前缀，CBOR11 至 CBOR16 仅含 CBOR 信封。除六位十进制 SAS 外，各值均为十六进制。[中心契约](../../../../docs/CENTER-CONTRACT.md)

独立生成器使用 Python 的 hashlib、hmac 和 libsodium 的 ristretto255 群运算。公开测试码为 `00001234`，发起方沿用原示例标量 12345。响应方从原示例标量 67890 开始递增搜索，67907 首次得到带前导零的 SAS `043910`。

安装 Python 3.10+ 和支持 ristretto255 的 libsodium 1.0.18+ 后，在仓库根目录运行

```sh
cd internal/pairing/cpace/testdata
python3 gen/application.py > application.json
```

同目录的 `application-hub.json` 使用相同的生成器与字段格式。两个 Hello 声明 `pair-hub/1`，CI 的末字段是完整的 `hub_fp`。从 testdata 目录运行即可复现：

```sh
python3 gen/application.py --hub > application-hub.json
```
