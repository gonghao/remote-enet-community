# 手动 TUN 端到端验收入口

本工具仅用于独立的本机 fixture 验收，不接 GUI、真实 MGU、发现广播、UDP、HTTP 回连或短码。普通测试与默认执行均不创建 TUN、不改地址和路由。

```sh
CGO_ENABLED=0 go build -o /tmp/renet-tuncheck ./cmd/renet-tuncheck
/tmp/renet-tuncheck --help
/tmp/renet-tuncheck --version
/tmp/renet-tuncheck --local 198.18.8.1 --target 198.18.8.2
```

最后一条仅输出 dry-run JSON，`success` 保持 false，退出码为 0。只有显式 `--run` 才打开原生设备。工具不提权、不安装驱动；原生运行由操作者在已获准的测试主机上执行：

```sh
/tmp/renet-tuncheck --run --local 198.18.8.1 --target 198.18.8.2 --timeout 30s --report /tmp/renet-tuncheck.json
```

Windows 可构建对应 `.exe` 后使用相同参数。`--name` 默认空，交由适配器分配私有名称；macOS 自动分配 `utun`。显式名称遵循 `internal/tunadapter` 的平台规则，JSON 的 `interface` 为实际名称。`--local`、`--target` 只接受 `198.18.0.0/15` 内的单个 IPv4 地址或 `/32`；两者必须不同且不能是本机已有地址。适配器另行检查现有网段及路由冲突。工具不接管默认路由。

`--timeout` 为整个操作阶段的最大预算，范围 `1s` 至 `120s`，验证完成即退出。取消或失败也会等待协议栈、fixture 和设备回收。设备清理由适配器自身的有界预算控制，清理后的宿主 SSH 检查额外最多 3 秒；因此超时或信号不表示立即退出。Ctrl+C、SIGTERM 均触发清理。

## 验收路径与限制

启动前先从 `127.0.0.1:22` 读取已有宿主 SSH 服务的 banner；服务不可用时，在创建设备之前失败。工具不启动 sshd、不登录、不发送密钥或认证信息，不读取用户状态目录。运行中和清理后再次读取宿主 banner，并与隧道 banner 核对。

工具生成两个仅驻留内存的临时身份。`net.Pipe` 上建立双向指纹钉扎的 mTLS、yamux 和既有 session；operator 只信任本次 fixture client。原生 OS 使用配置的 Local 源地址连接 Target 的 22/8080，经过 TUN、gVisor 和加密 session stream 到达本机 fixture。fixture 只接受：

- `Target:22` → `127.0.0.1:22`，仅读取 banner。
- `Target:8080` → 本工具创建的动态 loopback HTTP 服务，检查固定 GET 响应。

fixture 拒绝其他 Kind、目标、端口和不匹配 Local 的 Origin。HTTP 客户端禁用代理、keepalive 和重定向；读写均有期限。不将任意目标转发到公网。

JSON 记录平台、实际设备名、Local/Target、各阶段结果、原样 Target/Origin、L3 计数、SSH banner、HTTP 验证及清理错误。仅在所有检查与清理结束后标记成功；`Active` 必须归零，`Accepted` 至少为 2。IPv6/DAD 等噪声可能增加 `Rejected`，不以其为零作为成功条件。报告文件默认权限为 0600；不指定 `--report` 时输出到 stdout。参数/操作失败、报告写入失败均返回非零。

这里的成功只代表本机 fixture 的 IPv4 TCP 转发与 SSH banner 验证，不代表 SSH 认证、真实车辆或 mgu_kit 自动发现通过。设备、地址、路由与宿主 22 监听的前/中/后系统快照由外部验收者保存。Windows/macOS 交叉构建不能替代原生运行验收。

## 普通测试

```sh
CGO_ENABLED=0 go vet ./cmd/renet-tuncheck
CGO_ENABLED=0 go test ./cmd/renet-tuncheck
CGO_ENABLED=1 go test -race ./cmd/renet-tuncheck
```

测试只使用 loopback、内存设备和 `net.Pipe`；覆盖无原生副作用的入口、地址拒绝、SSH 预检失败、真实加密 session fixture、目标/Origin 边界、取消汇合和清理失败报告，不调用真实原生设备工厂。
