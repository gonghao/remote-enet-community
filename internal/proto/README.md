# 控制消息编码

帧为 `uint32_be payload_length || CBOR`，长度不包含四字节前缀。CBOR 使用 canonical 编码，envelope 为 `{1: MsgType, 2: Body}`，正文也使用从 1 开始的整数键。消息编号固定为：Hello=1、PairToken=2、PairResult=3、VehicleState=4、Ping=5、Pong=6、ExposeReverse=7、ExposeResult=8、Log=9、OpenTunnel=10。字段键与 Go 结构体中的 tag 一致；新增字段只能用新键。

`Read` 返回消息值（不是指针）；`Write` 接受消息值或非 nil 指针。字符串枚举暂用字符串编码；Token 使用 CBOR 字节串。写入和 Session 读取上限均为 1 MiB；独立 `Read` 调用必须指定正的上限。拒绝重复 map 键，限制嵌套深度和容器大小，忽略未知字段。未知消息编号返回 `Unknown` 并保留原始 CBOR 正文，由 Session 跳过；不会越过帧边界读取后续隧道字节。

专用连接测试复用 Hello 的 `linktest/1` capability 与现有 `OpenTunnel{Kind:"linktest/1", Target:"", Origin:""}` 头，不新增消息编号或字段、不改变版本。头之后的有界二进制 echo 协议只在两个 renet session 内处理，见 [linktest](../linktest/README.md)，不作为车辆 TCP/reverse 隧道下发。

数字配对能力为 `pair-direct/1`。PairShareA=11、PairShareB=12、PairConfirmA=13、PairConfirmB=14 的正文均为键 1 对应 32 字节 bstr 的单字段 map；这四种帧要求规范编码。握手使用 `ReadPair`，读取长度后先检查 256 字节上限，也允许读取原有 PairResult 以处理拒绝。
