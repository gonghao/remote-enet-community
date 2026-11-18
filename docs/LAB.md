# Linux 隔离台架

工具仅提供发现、固定 SSH 协议与透明 fakezgw，不实现车辆编码、刷写或真实车辆业务。固定网络为 `169.254.91.0/29`：宿主 test veth `.1`，独立 namespace gateway `.2`，MGU `.3`。任何已有 IPv4 地址或任意表非默认路由覆盖此段时拒绝启动，不删除覆盖。

默认和帮助均不读密钥、不创建目录、不启动进程、不探测网络：

```sh
python3 tools/lab-fixture.py --help
python3 tools/lab-fixture.py
python3 -m unittest discover -s tools -p test_lab_fixture.py -v
```

仅协调者确认环境后显式运行，工具不会自行提权或安装软件：

```sh
python3 tools/lab-fixture.py --run \
  --fakezgw-bin /absolute/path/fakezgw \
  --output /tmp/renet-fixture-unique \
  --actor-uid 1000 --actor-gid 1000 --lifetime-seconds 1800
```

输出祖先目录必须由 root 所有且不可被普通用户改写（允许 `/tmp` 这类 root 所有的 sticky 目录），并允许 actor 遍历，避免运行中替换路径。需要 root 协调权限及已有 Linux pidfd、iproute2、unshare/nsenter/setpriv、OpenSSH、`/run/sshd`。actor 必须是现存普通登录账户，gid 必须是主组；示例 UID/GID 应换成本机账户。工具不修改密码、HOME、真实 authorized_keys、系统 sshd 配置。使用 `UsePAM no`，若系统账户策略拒绝锁定账户，直接失败，不修改认证策略绕过。Client 由外部普通用户另行启动。

root 输出目录为 0700，配置和 host 私钥仅留在此目录。独立随机后缀的 actor 0700 兄弟目录存放临时 client key、host 公钥、导出文件（0600）及非秘密 responder 配置。私有 `ready.json` 给出本次绝对身份路径，仅供本次操作；目录与临时身份保留供协调者复核，停止后不应复用。人工清理须核对本次路径，不得将临时密钥或报告复制进仓库。

`export.json` schema v1 只有明确白名单：

| 字段 | 值 |
| --- | --- |
| `schema_version` | `1` |
| `gateway` | `169.254.91.2:6801` |
| `target` | `169.254.91.3` |
| `ssh_user` | 经验证的 actor 名称 |
| `identity_file` | 相对路径 `userkey` |
| `host_key_file` | 相对路径 `hostkey.pub` |
| `host_public_key` | 本次 fixture host 公钥 |

导出不含 host 私钥、邀请码、原始异常、真实用户密钥或系统配置。元数据可共享不表示 client 私钥可公开；actor 目录仅交给本次操作者。

受控 sshd 仅监听隔离 `.3:22`，root 启动满足端口与认证降权要求，认证后以 actor 执行固定协议。禁密码、交互认证、PTY、全部转发、用户 RC、用户环境、root 登录，无 AcceptEnv/Include。ForceCommand 嵌入 root 私有配置，不依赖 actor 可替换的脚本，绝不执行 `SSH_ORIGINAL_COMMAND`。

| 命令 | 行为 |
| --- | --- |
| 空或 `read-only-marker` | 精确返回 `remote-enet-mgu-fixture`，无换行 |
| `read-only-bulk` | 兼容输出 1 MiB 零字节 |
| `read-only-bulk 1048576` / `read-only-bulk 8388608` | 输出指定长度零数据 |
| `read-only-upload 1048576` / `read-only-upload 8388608` | 精确读取指定零数据及 EOF，回 JSON `{"bytes":N,"sha256":"..."}` |
| 其它 | 非零退出，不返回成功 marker |

每条命令独立 30 秒 alarm、最多 8 MiB，流式处理、不落盘。上传方必须关闭写方向发送 EOF；短流、超长或非零数据失败。

fakezgw 和发现 worker 使用固定 `setpriv --reuid/--regid --clear-groups --no-new-privs` 降权，无多线程 preexec_fn。root 只保留资源协调及受控 sshd。发现监听 wildcard UDP 6811，仅接受宿主 `.1` 精确六字节请求，以 IP_PKTINFO 固定 `.2:6811` 返回完整 56 字节；ready 前从宿主 test veth 实测广播回包。发现 worker 是独立进程；`discovery.state` 为 `not-started` 时 `joined` 为 null，已启动后区分 `reaped`/`reap-failed`。启动错误单独记录，正常取消不算清理失败。

随机 veth 名与 ifalias nonce，核验实际 child PID/netns inode 并持有 pidfd/netns fd。先在独有 child namespace 内创建 pair，再显式给两端设置 alias 并复核；每步重验 parent/child 身份，最后只把已标记 host 端移回 parent，复核后才配 IP。未标记或部分标记的 pair 留在独有 namespace 内，关闭其持有 FD、回收进程后由 namespace 销毁回收，宿主不按名字猜删。只有本次尝试移动且别名匹配的 host 接口可删除，移动超时也重新按实际 alias 判断。残留进程按已确认的 namespace 身份通过 pidfd 回收。不改管理网卡、默认路由或全局防火墙。外部命令有时间及 1 MiB 输出上限；总生命周期限制为 1..86400 秒。异常仅记固定错误码与阶段，保留部分报告。

私有报告区分正常/信号退出、强杀、worker 汇合、接口消失、路由消失与 namespace 进程回收，释放本进程持有的 namespace FD 后记录回收状态。未确认的清理会失败，不以删除命令返回零作为唯一证据。纯逻辑/mock 测试不代表真实 Linux、sshd、降权或内核验收；这些由 Root 单独执行，WAN、热点、实车仍需独立证据。

私有 root 目录内 `debug.log` 以 0600、拒绝 symlink 追加，最多 64 KiB；每条含阶段、异常类别和最多 2048 字符的诊断，敏感标记会抑制内容。外部命令失败附带有界输出帮助定位；公开报告只保留固定 code，不嵌入诊断。诊断写失败不会阻断清理。

认证排障由 Root 只读检查专用 sshd.log、actor 锁定状态与本次 daemon 有效配置；不修改账户密码、用户 HOME、系统目录权限或系统 sshd 设置。

原生 run02 已由 Root 确证 `/tmp` 授权文件祖先被 StrictModes 拒绝，actor 未锁定。因此当前保持 `UsePAM no`、`StrictModes yes`，改为 `AuthorizedKeysFile none`；本次 root 私有配置内直接钉住刚生成的 ed25519 公钥算法/base64，以固定 `/usr/bin/echo` 作为 AuthorizedKeysCommand，CommandUser 为 actor。启动前核验该可执行文件、解析目标与祖先均 root 所有且不可 group/other 写，目标必须是可执行普通文件。仅传经过结构/base64 严格验证的两个固定参数，不传用户原始参数或 token，不执行用户命令，不查真实 authorized_keys。没有新增系统路径或权限修改。该配置已由Root在run03及one-command run05完成实际认证与双向数据验收。

## 一次运行探针并自动回收

在原有显式运行参数上增加 `--probe-bin /absolute/path/renet-labprobe`，可选 `--payload-bytes 1048576`（默认）或 `8388608`。省略 probe-bin 仍为交互服务模式，默认 dry-run/help 无副作用。probe 必须是 actor 可执行、所有祖先可遍历的绝对普通文件；不由 root 执行数据探针。

ready 后通过固定 setpriv 参数降至 actor，以固定地址、账户、本次 client key 和 host 公钥运行 C 探针，预算最多 120 秒且受本次剩余生命周期限制，不接受附加任意命令或参数。stdout/错误输出由 root 持有的 pipe 收集，最多 1 MiB，保存本次 root 私有目录的 `probe-output.json`（0600）；不要求 actor 写 root 目录。成功或失败均立即进入 finally 回收全部服务和网络资源。

报告 `probe_result` 独立记录退出码、process_reaped、成功与固定 code；只有 labprobe/v1 完整 schema、全部六阶段按序 ok、marker 阶段成功、双向精确字节/SHA256、success/cleanup true 且无取消/超时，才判探针成功。fixture_ready 不是 probe 通过证据。`fixture_lifecycle` 记录 ready/cleanup/child_ok；顶层 success 会因后续清理失败或探针取消而失败，保留原 probe_result 供区分。原生 one-command 验收由 Root 完成，mock 不能冒充该证据。

## 系统命令与服务日志边界

root 协调命令只从固定 `/usr/sbin:/usr/bin:/sbin:/bin` 解析，不读取继承 PATH；可执行文件、解析目标及两侧祖先必须 root 所有、不可 group/other 写，目标为普通可执行文件。sys.executable 单独作同等验证；nsenter/unshare 内层命令也固定解析。子进程只继承固定 PATH 与 LC_ALL=C，不传 HOME/TMPDIR 或其它父进程环境。显式 fake/probe 路径继续由 setpriv 降权后执行，不要求 actor 二进制 root 所有。默认 dry-run/help 不进行这些文件检查。

child 协调进程、sshd、fakezgw 的私有日志由独立 worker 从非阻塞 pipe 读取，每服务最多落盘 8 MiB，满后持续 drain 并丢弃新增数据，不因日志满阻塞服务或无界积累内存。进程回收记录的 log（child 为 child_log）给出 written_bytes、dropped_bytes、truncated、eof、started、joined；截断本身不伪装服务失败，worker 的读取/写入或 join 失败才属于清理错误。退出时设置 stop event，selector 最长 0.2 秒轮询，join 最多 2 秒，不等待残留后代持有的 pipe EOF；eof=false 表示停止时尚未观察到 EOF，不把它写成已完整读尽。未启动 worker 的 joined 为 null，不误报回收失败。debug.log 仍保持独立 64 KiB 上限，export/report 不混入原始服务日志。

## Root整合验收记录

交互fixture的真实广播、SSH认证以及1MiB/8MiB双向探针均通过；one-command run05完成8MiB后自动收回接口/路由/namespace，服务日志线程joined，无cleanup errors。内核runner最终netem04包含1MiB及8MiB两种尺寸，baseline和200ms/1%配置都通过，degraded记录62次真实drop，清理恢复lo原状态。具体版本、计数和失败反例见[Wave 11](WAVE11.md)。

工具的report/log不等同GUI匿名诊断：其中有明确选择的fixture地址和本次路径，保持私有供复核。日志线程正常必须join；异常文件系统使join失败时报告失败，daemon线程避免拖住协调器退出。Windows探针只验证普通本地私有key文件，特殊reparse/占用文件按保守边界处理。

## 从源码构建并执行

以下在普通用户shell中构建，`id`取本次现存普通账户；只有明确的sudo/unshare命令进入隔离root协调环境。输出路径必须尚不存在，重复执行请更换本轮后缀，不能批量删除旧报告来重用。

```sh
mkdir -p bin
CGO_ENABLED=0 go build -o bin/fakezgw ./cmd/fakezgw
CGO_ENABLED=0 go build -o bin/renet-labprobe ./cmd/renet-labprobe
CGO_ENABLED=0 go test -c -o bin/l3-integration.test ./internal/integration
lab_actor_uid=$(id -u)
lab_actor_gid=$(id -g)
sudo unshare -n -- /usr/bin/python3 "$PWD/tools/lab-fixture.py" --run \
  --fakezgw-bin "$PWD/bin/fakezgw" --probe-bin "$PWD/bin/renet-labprobe" \
  --payload-bytes 8388608 --output /tmp/renet-lab-unique-01 \
  --actor-uid "$lab_actor_uid" --actor-gid "$lab_actor_gid" --lifetime-seconds 180
```

这里在外层独立netns再创建fake车子netns，所有接口与测试路由均局限本轮；宿主不会获得169.254.91/29测试路由。要让另一台Windows/Mac通过普通Client测试时，按前面的交互模式在协调者已确认的宿主环境创建veth，再用普通Client绑定该veth；这种原生模式与上述完全隔离的一条命令模式分别记录，不把两者拓扑混为一谈。

## 内核L3弱网

编译binary后可先运行dry-run查看计划；真正内核验证仅在新建且只有lo的netns里运行，未知qdisc或身份变化会拒绝。

```sh
python3 tools/l3-netem-check.py
sudo unshare -n -- /usr/bin/python3 "$PWD/tools/l3-netem-check.py" --run \
  --test-binary "$PWD/bin/l3-integration.test" --output /tmp/renet-l3-netem-unique-01 \
  --actor-uid "$lab_actor_uid" --actor-gid "$lab_actor_gid" --profile all
```

每次profile必须实际执行非Skip的TestL3ExternalNetem，并给出1MiB、8MiB两条完整结果、相符SHA/uid/gid及清理证据；返回码0或单个PASS不足以判通过。报告记录各次tc查询/写入、内核packets/drops及原始lo状态；每个tc操作前重验namespace/lo身份。200ms/1%是配置，不把计数比值视为实测丢包率，也不推断WAN性能。工作/清理/总预算分别340/25/380秒，超时保留部分报告。

## Windows/macOS原生探针

先按产品正常方式连接Operator、启用MGU并确认Ready；独立临时身份和host公钥来自本次fixture的actor导出目录，私钥单独保持私有。gateway参数填本机Operator呈现地址，target填fixture的MGU地址。Windows权限需与Operator匹配，不为绕过所有者解析而关闭独占。

```sh
renet-labprobe --run --gateway 127.0.0.1:6801 --target 169.254.91.3 \
  --ssh-user fixture-actor --identity-file /private/test/userkey \
  --host-key-file /private/test/hostkey.pub --payload-bytes 8388608 \
  --timeout 30s --report /private/test/probe.json
```

路径和用户名替换为本轮导出的值；Mac呈现地址通常是所选LAN地址，不强行使用127.0.0.1。工具固定连接target的22端口，既不修改路由也不创建TUN，Go SSH不读系统/用户ssh配置、不使用agent/密码/TOFU。报告必须success和cleanup均true、全部阶段ok；超时/取消/短数据/错pin/错误回执非零退出。报告含平台、阶段、耗时、字节/哈希，不含密钥、邀请码或路径；不与GUI的匿名诊断schema混用。默认/help/version不读keys、拨号或写report。

报告写入固定在已打开的父目录句柄内，兼容macOS系统目录别名以及Windows大小写/短名表示，不以路径字符串不同拒绝正常目录。最终报告文件仍拒绝符号链接、悬空链接与目录；同目录独占临时文件同步后替换目标并清理。Unix权限为0600，Windows继承所选目录ACL，使用本次私有目录保存结果。
