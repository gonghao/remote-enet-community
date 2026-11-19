# 票据向量

在仓库根目录运行 `python3 internal/ticket/testdata/gen/generate.py` 可重建三份 JSON。依赖 Python 3.8+ 标准库和 PATH 中支持 Ed25519 的 OpenSSL 3.x 命令行工具。

脚本以固定的 32 字节测试种子派生签发公钥，手写确定性 CBOR 编码，用 OpenSSL 对 Sig_structure 签名。运行后可用 `git diff --exit-code -- internal/ticket/testdata/*.json` 检查逐字节一致性。
