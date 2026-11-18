# 第三方组件与许可

此文件记录 remote_enet 构建和发行涉及的第三方组件。项目自有源码采用 [AGPL-3.0-only](LICENSE)；上游许可、版权和专门条款继续分别适用，不由项目许可证替代。

核对基线：2026-11-19，Go 1.27.3，`CGO_ENABLED=0`、`mygo_nowebview`，对 Linux amd64、Windows amd64、macOS arm64 的 `./cmd/...` 执行离线依赖枚举。实际可执行文件按平台/角色使用其中的子集，不能把完整 module graph 或测试依赖都视为随包链接的代码。

完整许可快照位于 `third_party/licenses/`，来源、版本与 SHA256 见 [manifest](third_party/manifest.json)。快照保留上游原文和字节，不把第三方组件重新许可成项目未来的许可证。包内附带相同索引与许可文件；macOS 位于 `.app/Contents/Resources/`，Windows 位于解压目录。

## 组件清单

| 组件 | 固定版本 | 上游许可/范围 | 许可原文 |
|---|---|---|---|
| [github.com/ebitengine/purego](https://pkg.go.dev/github.com/ebitengine/purego@v0.11.1) | `v0.11.1` | Apache-2.0 | [LICENSE](third_party/licenses/go/github.com/ebitengine/purego/LICENSE) |
| [github.com/egoist/mygo](https://pkg.go.dev/github.com/egoist/mygo@v0.9.1) | `v0.9.1` | MIT；内部 shader 另附 Apache-2.0 | [LICENSE](third_party/licenses/go/github.com/egoist/mygo/LICENSE)、[LICENSE](third_party/licenses/go/github.com/egoist/mygo/internal/shader/LICENSE) |
| [github.com/fxamacker/cbor/v2](https://pkg.go.dev/github.com/fxamacker/cbor/v2@v2.10.0) | `v2.10.0` | MIT | [LICENSE](third_party/licenses/go/github.com/fxamacker/cbor/v2/LICENSE) |
| [github.com/go-text/typesetting](https://pkg.go.dev/github.com/go-text/typesetting@v0.3.5) | `v0.3.5` | Unlicense OR BSD-3-Clause；HarfBuzz 另附 MIT | [LICENSE](third_party/licenses/go/github.com/go-text/typesetting/LICENSE)、[LICENSE](third_party/licenses/go/github.com/go-text/typesetting/harfbuzz/LICENSE) |
| [github.com/google/btree](https://pkg.go.dev/github.com/google/btree@v1.1.2) | `v1.1.2` | Apache-2.0 | [LICENSE](third_party/licenses/go/github.com/google/btree/LICENSE) |
| [github.com/hashicorp/yamux](https://pkg.go.dev/github.com/hashicorp/yamux@v0.1.2) | `v0.1.2` | MPL-2.0 | [LICENSE](third_party/licenses/go/github.com/hashicorp/yamux/LICENSE) |
| [github.com/x448/float16](https://pkg.go.dev/github.com/x448/float16@v0.8.4) | `v0.8.4` | MIT | [LICENSE](third_party/licenses/go/github.com/x448/float16/LICENSE) |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto@v0.57.0) | `v0.57.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/crypto/LICENSE) |
| [golang.org/x/exp](https://pkg.go.dev/golang.org/x/exp@v0.0.0-20250711185948-6ae5c78190dc) | `v0.0.0-20250711185948-6ae5c78190dc` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/exp/LICENSE) |
| [golang.org/x/image](https://pkg.go.dev/golang.org/x/image@v0.46.0) | `v0.46.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/image/LICENSE) |
| [golang.org/x/net](https://pkg.go.dev/golang.org/x/net@v0.60.0) | `v0.60.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/net/LICENSE) |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys@v0.48.0) | `v0.48.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/sys/LICENSE) |
| [golang.org/x/text](https://pkg.go.dev/golang.org/x/text@v0.43.0) | `v0.43.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/text/LICENSE) |
| [golang.org/x/time](https://pkg.go.dev/golang.org/x/time@v0.15.0) | `v0.15.0` | BSD-3-Clause（Go Authors） | [LICENSE](third_party/licenses/go/golang.org/x/time/LICENSE) |
| [golang.zx2c4.com/wintun](https://pkg.go.dev/golang.zx2c4.com/wintun@v0.0.0-20230126152724-0fa3db229ce2) | `v0.0.0-20230126152724-0fa3db229ce2` | MIT（Go 封装，不含 DLL 许可） | [LICENSE](third_party/licenses/go/golang.zx2c4.com/wintun/LICENSE) |
| [golang.zx2c4.com/wireguard](https://pkg.go.dev/golang.zx2c4.com/wireguard@v0.0.0-20261024220952-b3bc704ef0ee) | `v0.0.0-20261024220952-b3bc704ef0ee` | MIT | [LICENSE](third_party/licenses/go/golang.zx2c4.com/wireguard/LICENSE) |
| [gvisor.dev/gvisor](https://pkg.go.dev/gvisor.dev/gvisor@v0.0.0-20261116062528-83282de1f21f) | `v0.0.0-20261116062528-83282de1f21f` | Apache-2.0，附 MIT/BSD 文件条款及 AUTHORS | [LICENSE](third_party/licenses/go/gvisor.dev/gvisor/LICENSE)、[AUTHORS](third_party/licenses/go/gvisor.dev/gvisor/AUTHORS) |
| [Go toolchain](https://go.dev/LICENSE) | `go1.27.3` | BSD-3-Clause；另附 PATENTS | [LICENSE](third_party/licenses/go-toolchain/LICENSE)、[PATENTS](third_party/licenses/go-toolchain/PATENTS) |
| [Wintun prebuilt DLL](https://www.wintun.net/builds/wintun-0.14.1.zip) | `0.14.1` | Wintun Prebuilt Binaries License（单独条款） | [LICENSE](third_party/licenses/wintun-binary/LICENSE) |
| [Pixel Code Bold](https://github.com/qwerasd205/PixelCode/releases/tag/v2.2) | `2.2` | SIL OFL-1.1 | [OFL.txt](third_party/licenses/pixel-code/OFL.txt) |

## 源码与特殊组件

- yamux 使用 MPL-2.0。对应未修改的 v0.1.2 源码可从 [上游固定标签](https://github.com/hashicorp/yamux/tree/v0.1.2) 获取，也可通过 `go.mod` 中的固定版本取得。修改该依赖后，应同步提供相应的受覆盖源码和通知；不能只保留许可证文件而忽略源码提供义务。
- WireGuard/Wintun 的 Go 包为 MIT；随 Windows Operator 提供的官方 `wintun.dll` 是单独制品，适用 [归档中的预编译许可](third_party/licenses/wintun-binary/LICENSE)。[Wintun 官方说明](https://www.wintun.net/) 区分 GPLv2 源码与预编译二进制条款，不应将两者或 Go 封装的许可混为一谈。
- WireGuard Go 上游源码头部包含 `Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.`，并有 `Copyright 2015 Microsoft`、`Copyright 2021 The Go Authors. All rights reserved.`；Wintun Go 源码头部包含 `Copyright (C) 2017-2021 WireGuard LLC. All Rights Reserved.`。这些归属声明与各自 MIT 许可共同保留，不将 Go 封装的条款扩展到 DLL。
- Windows Operator 仅使用官方 Wintun 0.14.1 amd64 DLL，经现有固定摘要校验，不修改 DLL，按公开的 Wintun API 使用；其它桌面角色不附带 DLL。许可快照可统一随包提供，不代表每个平台均包含或使用该组件。项目已选择 AGPL-3.0-only，Windows 二进制发行的组合与再分发条件仍须独立核对。
- Pixel Code Bold 2.2 未修改，来源为 [上游固定版本](https://github.com/qwerasd205/PixelCode/releases/tag/v2.2)。SIL OFL 许可和版权通知必须保留，字体不改按项目许可证授权。包内保留原有 `licenses/PixelCode-OFL.txt`，并附许可索引中的同一原文。
- mygo shader、go-text HarfBuzz 与 gVisor 已发现的附加条款分别保留；模块根许可不能自动覆盖所有嵌入资源。当前清单是工程初审结果，不替代进一步的文件来源和发行范围复核。

## 更新与发行要求

依赖、构建标签、目标平台、字体或 DLL 更新时，重新枚举实际依赖并核对上游许可。更新对应快照、版本、来源和文件摘要，再更新 manifest 的 `goModSHA256`；不能仅刷新摘要来消除构建错误而跳过许可复核。打包脚本对 `go.mod` 和许可快照做一致性检查，不访问网络下载替代文本。

E-Sys 手册、抓包研究资料以及其它用户/第三方内容不因出现在仓库中就被本清单授予再分发权。公开源码和历史的发布审查另见开源准备文档。
