# 第三方许可快照维护

组件、版本、许可边界和源码获取说明以根目录 [THIRD_PARTY_NOTICES](../THIRD_PARTY_NOTICES.md) 为入口；[manifest](manifest.json) 保存当前 `go.mod`、项目 LICENSE 及各原文文件的 SHA256。项目自有源码已选用 [AGPL-3.0-only](../LICENSE)，第三方材料继续分别适用其上游许可。

## 当前范围

- Go 1.27.3、`CGO_ENABLED=0`、`mygo_nowebview`，Linux amd64 / Windows amd64 / macOS arm64 的 `./cmd/...` 实际依赖并集。
- 桌面构建基线的 17 个第三方 Go 模块、数字配对新增的 ristretto255 与 edwards25519、Go toolchain 的 LICENSE/PATENTS、Wintun 0.14.1 预编译条款、Pixel Code Bold 2.2 OFL。
- 已发现的 mygo shader、go-text HarfBuzz 和 gVisor AUTHORS/附加条款分别保留。未链接的模块图条目不自动加入，未来新增构建目标需重新核对。

## 更新流程

1. 使用项目固定工具链和缓存执行实际构建目标的离线依赖枚举，例如：

   ```sh
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
     go list -deps -json -tags mygo_nowebview ./cmd/...
   ```

2. 对每个目标重复，比较模块版本与实际使用包；同时检查嵌入文件、目录级许可和代码来源。不能仅把全部 module graph 的根许可复制一遍就宣称审查完成。
3. 从已校验的固定模块版本或官方制品提取许可、版权与必要通知，保留原文字节。Wintun DLL 从已固定摘要的官方归档核对，字体来源与摘要继续按字体 README 管理。
4. 核对各组件的源码提供、再分发及组合义务，更新根通知索引和 manifest。依赖变更时最后更新 `goModSHA256`，不能只改摘要绕过复核。
5. 执行 `python3 -B -m unittest discover -s tools -p 'test_build_desktop.py'`，并实际生成目标包，检查通知、许可和 manifest 的内容及文件摘要。

## 发行位置与边界

打包脚本将相同目录结构复制到 Windows 解压目录或 macOS `.app/Contents/Resources/`。字体原有的许可路径继续保留；所有新增材料进入现有 manifest v1 的文件摘要表。

原文可能包含 CRLF、行尾空格或不同排版，`.gitattributes` 为许可快照和项目 LICENSE 保留原文字节，并固定 `go.mod` 为 LF，避免 Windows 换行转换造成误报。修改原文、缺失快照或 `go.mod` 未复核会使打包失败。

本清单不包括第三方手册和私有抓包研究资料的再分发授权，不能据此将现有私有历史直接公开；详见 [开源准备审查](../docs/OPEN-SOURCE-READINESS.md)。
