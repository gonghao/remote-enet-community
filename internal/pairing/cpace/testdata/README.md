generator.json、exchange.json、points.json 分别解码自 [CPace -21 附录 B.3.1.1、B.3.9、B.3.11.1](https://www.ietf.org/archive/id/draft-irtf-cfrg-cpace-21.txt) 的 Base64 JSON。保留原字段，包括本套件未启用的并行模式和可选会话标识。

application.json 根据本仓库设计 §3 独立生成。群运算使用 libsodium 的 crypto_core_ristretto255_from_hash 和 crypto_scalarmult_ristretto255，摘要和确认使用 Python hashlib、hmac。所有值均以十六进制记录。公开示例码为 00001234，标量固定为 12345、67890，两个 Hello 含大端长度前缀。仅用于离线测试。
