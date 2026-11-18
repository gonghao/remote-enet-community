# 发现 UDP socket

接收 socket 绑定 IPv4 通配地址，但每个包必须携带有效的入站网卡与目的 IP。只有所选网卡上的绑定地址、该前缀的定向广播或有限广播会交给调用者；回复始终通过原 socket 发送并携带固定源 IP 与网卡的控制消息，因而保持接收端口。缺失、重复、损坏或截断 metadata 和截断 payload 均丢弃，不降级为普通 `WriteToUDP`。

接口的 `ctx` 控制 socket 生命周期；单次 `Read` / `Write` 的 context 可更短，通过串行化 deadline 中断本次 I/O，返回前等待取消回调结束并重置 deadline，避免污染下一次操作。`Close` 等待自身已触发的 owner 取消回调，关闭 socket 唤醒标准库 poller，不另建事件循环。

控制消息编码不使用 unsafe，按本机字节序处理长度和接口整数、按网络序处理 IPv4 字节，并校验每条消息的长度、类型和对齐：

| 平台 | 控制头 | packet info |
|---|---|---|
| Linux | 原生 SIZE_T 长度，原生指针对齐 | 12 字节：ifindex、spec_dst、addr |
| macOS | 4 字节长度与对齐，包括 arm64 | 12 字节：ifindex、spec_dst、addr |
| Windows | WSACMSGHDR 的 SIZE_T 长度与对齐 | 8 字节：addr、ifindex |

实现核对了本机 Go 1.27 的 `internal/poll/fd_windows.go`、`internal/syscall/windows/syscall_windows.go`、`syscall/sockcmsg_unix_other.go` 与 Go 官方 x/net 的平台布局；Windows 收包使用 Go 标准库自带 `WSARecvMsg` 支持。[WSACMSGHDR](https://learn.microsoft.com/en-us/windows/win32/api/ws2def/ns-ws2def-wsacmsghdr) 描述 Windows 控制头，[IN_PKTINFO](https://learn.microsoft.com/en-us/windows/win32/api/ws2ipdef/ns-ws2ipdef-in_pktinfo) 描述地址与网卡布局。Windows 的 `WSAEMSGSIZE` 接收结果按截断丢弃；不同于 Unix，这一路径可能同时返回错误。

首次 Windows Server 2025 / Go 1.27.4 原生 CI 暴露了同步发送计数差异：[Go 的 WriteMsgInet4](https://github.com/golang/go/blob/go1.27.4/src/internal/poll/fd_windows.go) 向 `WSASendMsg` 传入空的字节计数指针，同步成功且不等待完成通知时会返回 `0, nil`。[Winsock 文档](https://learn.microsoft.com/en-us/windows/win32/api/winsock2/nf-winsock2-wsasendmsg) 中，这表示数据报发送已立即完成。只对 Windows 上“无错误、字节数为零、控制数据长度完整、payload 非空”的成功原子发送补齐计数，不重发数据，也不忽略任何 I/O 错误、非零短写或控制数据短写。原生测试同时检查返回计数与实际接收的 payload/源地址/端口。

macOS 的空载荷边界也由原生测试确认：[Go 的 BSD sendmsgN](https://github.com/golang/go/blob/go1.27.4/src/syscall/syscall_bsd.go) 会在只有控制信息时补一个字节。发现响应应为非空报文，因此 `Write` 在调用系统接口前统一拒绝空 payload，防止载荷被静默改变；接收仍可读取空数据报，由上层按无效发现请求忽略。原生测试验证空回复明确失败，随后非空及单字节零值载荷仍原样送达。

本地 Linux 已运行真实单播、loopback 定向广播、固定源 IP/端口、错误目的地过滤、截断数据丢弃和取消/复用测试。`TestNative...` 测试使用真正绑定的 `127.0.0.1`，已纳入 Windows/macOS 原生 CI；`TestBroadcastFixedSourceAndDestinationFilter` 只在 Linux 使用 `127.0.0.2` 作为整个 loopback 前缀内的回复源，不修改系统别名。各平台结果以对应提交的 [GitHub CI](https://github.com/gonghao/remote-enet-community/actions/workflows/ci.yml) 为准；原生 socket 测试不等于目标诊断工具兼容性验收。
