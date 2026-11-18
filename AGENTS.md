# remote-enet · 仓库约定

设计稿：`docs/DESIGN.md`（架构与决策）。协议事实：`docs/enet-protocol-research.md`（带置信度；标「未知」的不要当已知实现）。各包的职责与接口契约：`docs/CONTRACTS.md`。

## 语言
- 代码、注释、标识符、命令、日志文案：English。
- 文档、commit message、PR 描述：中文；conventional commit 前缀保持英文，如 `feat(hsfz): 发现响应解析`。

## 工具链
- Go 1.27.3（mygo v0.9.1 的最低要求），纯 Go，`CGO_ENABLED=0`。不引入 cgo 依赖。
- 提交前必须通过：`gofmt -l . `（无输出）、`go vet ./...`、`go test ./...`。
- 测试只用 loopback / `net.Pipe` / 内存传输，不访问公网，不依赖真车或外部服务。
- 新增第三方依赖要克制，并在 commit message 里列出理由。首选标准库。
- 日志用 `log/slog`；错误用 `fmt.Errorf("...: %w", err)` 包装并带上下文。

## 代码风格
- 导出 API 要有简短 doc comment，只写代码表达不了的东西：不变式、前置条件、踩过的坑。不写复述代码的注释。
- 语义上确定不为 nil/零值时用直接断言，不写防御性分支；真实可能失败的路径返回带信息的错误。
- `context.Context` 作为第一个参数贯穿所有阻塞调用；goroutine 必须能被 ctx 取消，退出时不泄漏。
- 不写空实现或桩来让调用方编译通过。接口暂时没有实现就不要先引用它。
- 不改动分配给其他包的目录；需要别的包提供新能力时，在你的报告里写出期望的签名，由整合者协调。

## 边界
- 车端（`carside`）永远不解析隧道内容、不提权、不做 L3。
- Hub 只搬密文；端到端加密在 `secure` 完成，Hub 代码里不能出现对内层字节的解释。
- 不写任何车辆编码/刷写的业务逻辑；本项目只做发现、呈现与透明转发。

## 界面与品牌
- 后续界面、图标与Logo改动遵循 `docs/UI-DESIGN.md` 的 Pixel Workbench：浅色、直角、整数网格、可读的简繁中文与工程数据。新增控件复用主题，不引入圆角胶囊、渐变、光晕或玻璃模糊。
- Logo主稿为 `internal/brand/assets/mark.svg`（32格），小尺寸源为同目录的 `mark-16.svg` / `mark-20.svg`；修改后使用 `go run ./internal/brand/cmd/brandassets --output internal/brand/assets --windows-resource internal/brand/icon_windows_amd64.syso` 更新导出资源，保持窗口、托盘、Windows EXE与打包图标一致。
