# 桌面预览包

`tools/build-desktop.py` 仅用 Python 标准库，统一构建 Windows amd64、macOS arm64 的 Client 和 Operator；只构建和读取二进制，不启动 GUI 或网络助手，不提权、不安装驱动、不读取签名私钥。需要 Python 3.10+、本地 Go 1.27.3+ 及已缓存的 Go 依赖；构建强制 `CGO_ENABLED=0`、`mygo_nowebview`、`GOPROXY=off`、`GOSUMDB=off`、`GOTOOLCHAIN=local`，不会自动下载依赖或工具链。

项目自有源码使用 [AGPL-3.0-only](../LICENSE)；第三方组件、固定版本与许可见根目录 [THIRD_PARTY_NOTICES](../THIRD_PARTY_NOTICES.md) 和 [许可清单](../third_party/manifest.json)。四种桌面包均附带项目 LICENSE、索引与完整第三方许可快照，macOS 放在 `.app/Contents/Resources/`，Windows 放在解压目录；MPL-2.0 的 yamux 源码获取方式和 Wintun 预编译 DLL 的独立条款也随索引提供。打包校验 `go.mod`、项目 LICENSE 与许可文件摘要，缺失、变更或依赖清单未复核即失败，不发布不完整产物。增加许可材料不更改 manifest v1 的字段，文件纳入已有 `files` 摘要表；许可快照包含可选平台组件不代表所有角色都链接或分发该组件。

当前构建材料仍是私有审查产物。正式对外发行前须提供对应版本的相应源码及构建材料，并完成 Windows 带 DLL 制品的组合许可复核；附带 LICENSE 不等于这些条件已经完成。

```sh
python3 tools/build-desktop.py --platform all --role all \
  --output /tmp/remote-enet-wave10-preview --version 0.1.0-preview.wave10.0 \
  --wintun-dll /path/to/verified/amd64/wintun.dll
```

`--platform` 接受 `windows|darwin|all`，`--role` 接受 `client|operator|all`，两者默认 `all`；`--output` 默认仓库 `dist`。`--version` 默认 `0.1.0-preview.<short revision>`，版本严格遵循诊断模块格式，最终版本（包含自动追加的 `-worktree`）最多 64 个 ASCII 字节。接受 `dev`、可选 `v` 前缀的 `major.minor.patch`、`-alpha.N` / `-beta.N` / `-rc.N`，以及固定预览后缀 `-preview.waveN`、`-preview.waveN.N` 或 `-preview.<commithex>`；仅预览后缀允许附加 `-worktree`。主版本三段及 alpha/beta/rc 序号各为 1–9 位，除单独 `0` 外不允许前导零；wave 数字段各为 1–3 位；commithex 为 7–40 位小写十六进制。拒绝任意 tag、构建元数据和其他后缀。Git 有已修改或未跟踪文件时追加 `-worktree`（已有则不重复），因此 dirty 构建必须使用预览格式，`dev`、正式版和 alpha/beta/rc 的 dirty 构建会失败；manifest 独立记录真实 dirty 布尔值和完整 commit。输出到仓库内的未忽略目录会影响下一次构建的 dirty 状态，建议使用仓库外的专用目录。

旧调用继续有效：

```sh
RENET_BUILD_VERSION=0.1.0-preview.wave10.0 tools/build-macos-client.sh /tmp/client-preview
```

shell 的第一个位置参数仍是输出目录；未传时使用仓库 `dist`，未设置版本环境变量时采用上述默认版本。统一 Python CLI 的版本只由 `--version` 控制。

## 平台布局与校验

两个角色使用独立产品名 `Remote ENET Client` / `Remote ENET Operator`、标识 `dev.remote-enet.client` / `dev.remote-enet.operator`；mygo 的 product name、package identifier、package version 和入口 `main.version` 由构建参数注入。Windows GUI 还使用 `-H=windowsgui`，并在打包前检查 PE32+、amd64 与 GUI subsystem。

Windows ZIP 根目录包含对应 `renet-client.exe` 或 `renet-operator.exe`。仅 Operator 需要调用者显式提供 `--wintun-dll`，脚本校验官方 amd64 DLL 的固定 SHA256：

```text
e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce
```

同时检查 PE 架构，再以 `wintun.dll` 放入 EXE 同目录。缺失、摘要不符、架构不符均失败；CLI 没有覆盖固定摘要的参数。不联网下载、不安装、不提交 DLL。固定摘要识别经外部核验的官方文件，本脚本不执行 Windows Authenticode 验签。

macOS ZIP 包含独立角色 `.app`，主程序位于 `Contents/MacOS/renet-<role>`。Operator 额外包含 **`Contents/MacOS/renet-net-helper`**，从真实 `cmd/renet-net-helper` 构建；源码未整合时直接失败，不提供占位助手。两种可执行文件均检查 arm64、MH_EXECUTE、macOS 最低系统版本、代码签名 SuperBlob/CodeDirectory 的 ad hoc 标记；ZIP 中保留 `0755` 执行权限。此检查是结构及标记检查，不是完整密码学签名验证。

`Info.plist` 记录角色 name/identifier、短版本（三段数字，可选 `v` 前缀不写入；`dev` 映射为 `0.0.0`）、日历 bundle build 版本、完整预览版本、commit 和 dirty。`LSMinimumSystemVersion` 取主程序及助手要求的较高值。每日构建号不保证同一天递增，本流程仅用于预览分发，不支持自动更新。没有 Developer ID 签名或 Apple 公证；Go ad hoc 标记不能替代它们。包内中文 `README.txt` 说明版本、权限、签名及跨编译的验收边界；macOS GUI 和身份目录保持普通用户权限，仅 helper 按次获得系统授权。Windows 本轮检测权限和驱动能力并提示，用户可按需通过 `Run as administrator` 运行 Operator，不自动提权。

## 输出与 manifest v1

每个组合独立生成三个文件，macOS 文件名使用 `macos`（manifest 平台仍为 `darwin`）：

```text
remote-enet-<role>-<windows|macos>-<amd64|arm64>-<version>.zip
remote-enet-<role>-<windows|macos>-<amd64|arm64>-<version>.zip.sha256
remote-enet-<role>-<windows|macos>-<amd64|arm64>-<version>.manifest.json
```

ZIP 内还有相同的 `manifest.json`。manifest 字段：

| 字段 | 含义 |
| --- | --- |
| `schema_version` | 固定 `1` |
| `role`, `platform`, `architecture` | 构建组合 |
| `name`, `identifier`, `version` | 产品及完整预览版本 |
| `revision`, `dirty` | 完整 Git commit 和工作区状态 |
| `minimum_macos` | 最低 macOS 版本；Windows 为 null |
| `helper_sha256`, `wintun_sha256` | 对应随包文件的 SHA256；不适用为 null |
| `signatures` | Developer ID、公证、Authenticode 验证均为 false；Mac 额外记录主程序/助手最低版本及 ad hoc 标记 |
| `files` | 包内内容的相对路径到 `sha256`、八进制 `mode` 的映射，不包含 manifest 本身，避免自引用 |

`.zip.sha256` 使用 `sha256sum` 格式，覆盖最终 ZIP，包括包内 manifest。外置 manifest 与包内 manifest 完全一致，ZIP 摘要单独存放以避免循环依赖。

所有选择的组合在输出目录中的本次私有临时目录完成构建、打包、ZIP 读取复核，再通过同文件系统 `os.replace` 发布。单个文件不会出现半成品；三个文件及多个组合不是跨文件原子事务，发布时异常退出可能留下不同批次的完整文件，应重新执行并核对 ZIP SHA256。相同文件名会替换旧文件；失败不清理调用者目录，只清理本次临时目录。不修改 HOME 等系统环境变量。

## 自动验证与验收边界

```sh
python3 -B -m unittest discover -s tools -p test_build_desktop.py -v
sh -n tools/build-macos-client.sh
git diff --check
```

测试使用内存模拟 Mach-O/PE，不执行二进制、不访问网络、不安装驱动、不进行系统授权。覆盖四份包的内容/权限/摘要、plist、DLL 双重验证、损坏结构、缺失助手/DLL、Windows GUI 标志、离线构建参数和失败保留已有输出。测试允许 mock 固定 DLL 摘要，仅生产 CLI 不提供绕过参数。

跨编译和结构校验不代表 Windows/macOS 原生可运行，也不代表系统授权、真实 TUN、路由、清理、实车或诊断工具验收。整合者需在真实源码和官方 DLL 到位后生成四份最终包，并单独记录原生验收结果。

## 本轮整合记录

Root已用真实源码及固定官方DLL生成四份wave10.1-worktree ZIP，完成结构/权限/manifest/摘要回读检查。Windows普通权限/管理员GUI和Mac普通用户GUI+授权helper的具体原生结果见[Wave 10](WAVE10.md)；Mac已验证1MiB SSH与GUI崩溃后回收。GitHub工作流增加打包单测、官方ZIP及DLL双摘要校验和预览artifact上传，实际远端结果以提交后的运行记录为准。
