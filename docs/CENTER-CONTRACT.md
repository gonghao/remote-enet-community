# 中心契约 v1

记录日期：2026-11-19。状态：已定稿，尚未实现。本文把 [CENTER-PROTOCOL](CENTER-PROTOCOL.md) 的约束落成可实现的接口：中心 HTTPS API、请求签名、连接票据、Hub 验签、租约与占位、首次配对的消息流程和错误码。第 10 节是配对消息编号、编码与状态机的唯一权威定义；[PAKE 选型](research/pake-selection.md) 只保留选型理由与安全分析。中心服务端可以用任意语言和存储实现，Go 端（Hub、Client、Operator）按本文接入。第 14 节的参数于 2026-11-19 确定。

本文不涉及支付、定价、套餐或任何具体部署。管理端只向中心写入两类与收费相关的事实：账号有效期和并发额度。本文不修改现有 `renet1:` 邀请码、Settings 或 Hub 外层协议 v1（[internal/hub/PROTOCOL.md](../internal/hub/PROTOCOL.md)）；session 控制消息只新增第 10.2 节的消息编号。

## 1. 术语与编码约定

| 术语 | 含义 |
|---|---|
| 会话 | 一次接待：一个 Operator 设备服务一个 Client 设备，绑定一个 Hub 节点。`session_id` 从创建时起固定，票据、租约、占位和 PAKE 都绑定它 |
| 设备记录 | Operator 设备登录后，中心把其公钥登记到账号下的记录。之后的请求用该设备私钥逐个签名，中心不发放任何不记名凭据 |
| 占位 | 会话占用的一份账号并发额度。会话结束后，占位要等 Hub 确认关闭或越过承载上界才释放 |
| 承载上界 | 中心为每个占位持久记录的时间：遵守租约的 Hub 不可能在此之后继续承载该会话 |
| 节点代次 | 中心在 Hub 进程每次激活时授予的递增整数。票据、心跳和租约都绑定代次 |
| 票据 | 中心签名的短期入站准入凭据，放在 Hub 路由头的 token 字段 |
| 租约 | Hub 继续承载某个会话的期限，由节点心跳续期 |
| 钉住记录 | 一端在本会话内完成配对后保存的对端指纹，按 `(center_id, session_id)` 存储 |

| 数据 | 编码 |
|---|---|
| 设备指纹、Hub 指纹 | JSON 中用 `identity.Fingerprint.String()` 形式（RFC 4648 base32 大写无填充，四字符一组以 `-` 分隔）；接收方同时接受无 `-` 形式。CBOR 中为 32 字节原始值 |
| 公钥 | Ed25519 原始 32 字节，JSON 和请求头中为 base64url（无填充） |
| `account_id`、`session_id`、`node_id`、`jti` | 16 字节 CSPRNG 随机数，JSON 中为 base64url（22 字符），CBOR 中为字节串 |
| `center_id` | 1–32 字节 ASCII，字符集 `[a-z0-9.-]`，由中心运营者选定，写入各端配置；不同中心不得相同 |
| 时间 | Unix 秒（整数）；时长字段以 `_s` 结尾 |
| 签名输入 | 一律为 CBOR 定长数组，按 RFC 8949 §4.2.1 核心确定性编码。数组没有键序问题，各语言实现结果一致 |

设备指纹 = SHA-256(Ed25519 原始公钥)，与 `internal/identity` 一致。中心收到公钥时自行计算指纹，不接受调用方自报的指纹。

## 2. 信任模型

| 参与方 | 中心模式下的信任假设 |
|---|---|
| 中心与 API 入口代理 | 授权事实（账号、额度、期限、节点）的权威来源。中心分配数字码，入口代理终止 TLS 也能看到数字码，所以**首次配对时它们是受信方**：被攻破的中心或入口可以把两端引到自己控制的路径，分别与两端完成 PAKE，从而冒充对端。两端**必须**口头核对第 10.2 节的短校验码（SAS），以发现这种冒充 |
| Hub | 不受信于身份。不知道数字码，PAKE 使它无法冒充对端。它是受信的执行节点：撤销和租约到期后停止承载，依赖 Hub 遵守本契约；恶意 Hub 可以继续转发密文、阻断、拖慢连接或伪报用量，但读不到内容 |
| 网络路径 | 不受信 |
| 同一会话内的重连 | 只依赖钉住记录，不再依赖中心对身份的断言 |

SAS 只防中间人，不防猜中数字码的人抢占接待：猜中者知道口令，会与真 Operator 算出相同的 SAS。后一种情况要靠 Operator 与客户的独立沟通发现，见第 13 节。

## 3. 中心持有的事实与原子性

### 3.1 记录

| 记录 | 字段（语义） | 写入方 |
|---|---|---|
| 账号 | `account_id`、显示名、状态（`active` / `disabled`）、`valid_until`、`quota`（默认 1）、账号码 HMAC（唯一） | 管理端 |
| 设备记录 | 设备指纹（主键：一台设备同时只属于一个账号）、公钥、`account_id`、标签、最近登录 / 最近使用时间、状态（`active` / `revoked`） | 登录、吊销 |
| 会话 | `session_id`、`account_id`、Operator 指纹、`node_id`、状态、Client 指纹与公钥、`code_expires_at`、`redeemed_at`、`pair_deadline`、结束原因与时间、`version`（每次变化加一） | API |
| 码记录 | 码 HMAC（唯一）、`session_id`、到期时间、码密文（以中心密钥 AEAD 加密，关联数据为 `session_id`）。只在码可兑换期间存在 | 创建会话；兑换、结束或到期时删除 |
| 占位 | `session_id`（主键）、`account_id`、`node_id`、`released`、`carry_bound`、`old_gen_bound` | 创建会话、领票、心跳、激活 |
| 票据记录 | `jti`、`session_id`、角色、代次、`exp`、票据字节 | 领票 |
| 节点 | `node_id`、地址、端口、区域、登记指纹、容量、管理状态（`pending` / `active` / `draining` / `disabled`）、公钥、当前代次、本代次最后 `seq`、最近心跳时间 | 管理端登记；激活、心跳 |
| 激活记录 | (`node_id`, 幂等键) → 授予的代次 | 激活 |
| 用量 | (`session_id`, `node_id`, 代次) → 累计字节与配对时长的最大值 | 心跳 |
| 幂等记录 | 作用域、键、请求体 HMAC、状态码、结果引用（资源类型与 ID，或错误码） | API |
| 请求 nonce | nonce、过期时间（第 6.5 节） | 读取数字码的请求 |
| 计数器 | 登录、兑换、码生成、按账号成功兑换的滚动计数 | API |

账号码与数字码只以 HMAC-SHA256(中心 pepper, 规范化码) 作为查找键。持久层、幂等记录和日志都不保存明文码；码密文只用于向 Operator 再次展示，码不可兑换时随码记录删除。管理后台的接口不属于本契约，第三方中心可以自行实现，只要写入的事实满足本节语义。

### 3.2 事务

码核销与并发占位必须由单个强一致数据库的条件写完成，不能依赖最终一致的存储、缓存或各 Hub 的本地状态。随时间发生的迁移（码到期、配对期限、账号到期与宽限、节点离线、空闲超时、占位越过上界）可以惰性执行：任何读写在同一事务里先把已到期的迁移落库，再做本次判断；另有周期清理兜底。惰性落库的结束时间记为迁移到期的时刻，而不是被观察到的时刻；同一会话的几个截止同时越过时取最早的那个，与 `valid_until` 相同时按 `account_expired` 结束。管理端修改 `valid_until` 或账号状态时，先在同一事务里按旧值落库已到期的迁移，延长有效期不会让已越过截止的会话复活。与时间有关的前提（期限、码到期、节点在线、nonce 窗口）在提交时按数据库执行语句时的时钟复核，不沿用规划时读到的时间：规划之后、提交之前跨过截止的写入回滚，按新时间重新规划（例如 `/established` 提交时已到配对期限，得到 `session_ended`，不会晋升为 `active` 并获得宽限）。终局的业务拒绝（如 `quota_exhausted`、`code_invalid`、`session_ended`）同样在提交时复核拒绝条件仍成立，否则重新规划。

带幂等键的写请求，事务的第一步都是按作用域条件插入幂等记录（认领），最后一步写入结果引用；预扣的尝试预算也在同一事务内。哪些结果入表见第 6.3 节。

| 操作 | 一个事务内完成 |
|---|---|
| 创建会话 | 认领幂等键 → 释放本账号已越过 `carry_bound` 的已结束占位 → 检查账号 `active` 且未到 `valid_until`、设备记录有效、本设备没有非 `ended` 会话（设备指纹上的部分唯一约束）、本账号未释放占位数 < `quota`、节点可分配、本账号码生成计数未超限、全中心码记录数 < `max_open_codes` → 插入会话（`open`）与占位 → 删除同 HMAC 的过期码记录后唯一插入新码记录（冲突则重新生成）→ 计数 → 写结果 |
| 兑换 | 认领幂等键 → 预扣来源 IP、设备、全局兑换计数（超限即回滚并返回 `rate_limited`）→ 按码 HMAC 找到未过期的码记录，且会话为 `open`、账号 `active` 且未到 `valid_until`、节点可分配（对已分配到该节点的会话：不计容量，本会话的占位已计入；`draining` 只影响新分配，照常可兑换；节点停用或离线时会话已按 `node_disabled` / `node_lost` 惰性结束）、占位未释放 → 检查并递增本账号成功兑换的滚动计数，超限则以 `redeem_limit` 结束该会话并按 `code_invalid` 处理 → 会话 `authorized`，写入 Client 指纹与公钥、`redeemed_at`、`pair_deadline` → 删除码记录（数字码在此核销）→ 写结果 |
| 登录 | 认领幂等键 → 预扣来源 IP、账号码前缀、全局登录计数 → 查账号 → 成功则登记设备记录并退还本次预扣（只计失败）→ 写结果 |
| 领票 | 事务外先签好票据 → 认领幂等键 → 确认会话为 `authorized` / `active`、调用方资格（第 7.7 节）、`exp ≤ 硬截止 − 2 × clock_skew`（第 9.2 节）、节点在线且代次仍为票据中的代次 → 插入票据记录 → `carry_bound = max(carry_bound, exp + 2 × clock_skew)` → 写结果。提交成功后才返回票据 |
| 建立 | `authorized → active`，前置状态与调用方作为条件 |
| 结束 | 会话 `ended`，删除码记录；从未签发过票据的占位立即释放 |
| 心跳 | 校验代次为当前且 `seq` 递增 → 续租并抬高 `carry_bound` → 处理关闭确认、合并最终用量、释放占位 → 合并用量 |
| 激活 | 幂等键已有激活记录：代次仍为当前则返回原代次，否则 `node_superseded`。没有记录：代次加一，记录 (键 → 新代次)，该节点所有未释放占位 `old_gen_bound = max(old_gen_bound, carry_bound)` |

管理员把额度调低到当前占用以下时，既有会话继续到结束（包括兑换、领票与建立）；未释放占位数降到新额度以下之前，不能创建新会话。未释放占位数达到额度时，若其中未结束会话的占位数已低于额度（其余占位属于已结束、正在等待释放的会话），返回 `quota_releasing`，否则返回 `quota_exhausted`。

## 4. 码格式

### 4.1 账号码

- 管理员开户或重置时由 CSPRNG 生成 40 位随机数，编码为 8 个 Crockford base32 符号（字母表 `0123456789ABCDEFGHJKMNPQRSTVWXYZ`），显示为 `XXXX-XXXX`。
- 规范化：去掉 `-` 和空白，转大写，`I`、`L` 视为 `1`，`O` 视为 `0`；含 `U` 或其他字符即为格式错误，客户端直接提示，不发请求。
- 在所有未删除账号中唯一，只在 Operator 设备首次登录时输入，客户端不保存。
- 重置账号码：吊销该账号全部设备记录，这些设备的非 `ended` 会话以 `device_revoked` 结束。

### 4.2 数字码

- 中心用 CSPRNG 拒绝采样生成 `00000000`–`99999999` 的均匀值，按字符串处理，保留前导零，显示为 `XXXX XXXX`。
- 规范化：去掉空白和 `-`，全角数字转为 ASCII；结果必须恰好 8 个 ASCII 数字。
- 绑定一个会话，有效期 `code_ttl`，只能兑换一次，**兑换成功即核销**：码记录随之删除，之后同一个码对任何人都无效。码同时是两端的 PAKE 口令（第 10 节），Client 只把它保存在内存中，配对完成或会话结束后丢弃。
- 码到期只结束尚未兑换的会话；已兑换的会话由配对期限和后续规则约束。

## 5. 请求认证

### 5.1 签名请求

除 `GET /v1/meta` 外，所有请求都用请求方的 Ed25519 私钥签名：

| 请求头 | 内容 |
|---|---|
| `Renet-Device-Key` 或 `Renet-Node-Id` | 设备公钥（base64url），或节点的 `node_id` |
| `Renet-Timestamp` | Unix 秒，按第 6.4 节校正后的时间 |
| `Renet-Client` | `<role>/<app_version>`（第 6.1 节） |
| `Renet-Nonce` | 可选，16 字节随机数，base64url（第 6.5 节） |
| `Idempotency-Key` | 写请求必带（第 6.3 节） |
| `Renet-Signature` | base64url 的签名 |

签名输入：

```
["renet-center/v1 request", center_id, kind, principal, method, path_and_query, timestamp, renet_client, nonce, idempotency_key, sha256(body)]
```

`kind` 为 `"device"` 或 `"node"`；`principal` 为设备公钥或 `node_id` 的字节串；`method` 为大写文本；`path_and_query` 为请求目标规范形式的字节（见下）；`renet_client` 为该头的值；`nonce` 为字节串，没有则为空串；`idempotency_key` 为文本，没有则为空串；`body` 为原始请求体字节，没有则为空串。

- 中心检查时间戳在 `request_window` 内，然后验签：设备请求用头中的公钥，节点请求用激活时登记的公钥。
- 请求目标的规范形式：按 WHATWG URL 标准解析后序列化的 pathname，后接 search；只有 `?` 而没有查询串时视为没有查询，片段从不发送。已有的百分号编码按原样保留（不改大小写、不解码）。客户端必须按规范形式发送请求目标并对它签名。中心所在的 HTTP 栈只提供规范化之后的 URL，非规范形式（如含 `.`、`..` 段或末尾裸 `?`）到达中心时已被改写，验签随之失败（第 15 节 V3 反例）。
- 上表中的任何一个头出现多次，中心返回 `bad_request`。有的 HTTP 栈会把重复头合并成以 `, ` 连接的一个值；上表各头的合法取值都不含逗号，中心把含逗号的值按重复头处理。
- 除 `GET /v1/meta` 外没有免签名的端点，也没有单独的健康检查端点；探活使用 `GET /v1/meta`。
- 入口代理必须原样转发请求方法、请求目标、请求体和上表各头，不得改写、合并或补加。代理若需要路径前缀，由中心在验签前按配置还原为客户端发出的请求目标（规范形式）。
- 一个签名最晚可以在收到后 `2 × request_window` 内仍被接受（时间戳本身可以超前 `request_window`）。防重放的记录必须覆盖这段时间：写请求的重发命中幂等记录（保留期远长于此）；返回数字码的读取要求一次性 nonce（第 6.5 节）；其余 GET 的重发只是重复读取无秘密的数据；心跳靠代次内递增的 `seq`。

### 5.2 各类调用方

| 调用方 | 主体 | 有效条件 |
|---|---|---|
| Operator（登录） | 任意设备公钥 | 请求本身即持有证明；登录成功后登记设备记录 |
| Operator（其他请求） | 设备记录 | 记录为 `active`，未超过空闲到期与绝对到期（第 7.2 节） |
| Client（兑换、列出会话） | 任意设备公钥 | 兑换成功后公钥写入会话 |
| Client（会话操作） | 会话的 Client 指纹 | 路径中的会话的 Client 指纹等于签名者，且会话未结束或距 `ended_at` 不到 10 分钟（便于读取结束原因） |
| Hub | 节点 | 已登记；除激活外，还要求代次为当前代次 |

登录与兑换的请求体含 16 字节随机 `salt`。签名覆盖请求体哈希，加入 salt 后，即使请求头（含签名）被单独记录，也无法据此离线穷举请求体里的码。

吊销设备 = 把设备记录置为 `revoked`，之后它的签名请求一律 `unauthenticated`。

## 6. API 通用约定

### 6.1 版本、缓存与日志

- 只用 HTTPS，路径版本 `/v1/...`。请求与响应体为 UTF-8 JSON；未知字段一律忽略。新增必须理解的语义时升主版本（`/v2`）。
- 每个请求带 `Renet-Client: <role>/<app_version>`，`role` 取 `operator`、`client`、`hub`，`app_version` 为语义化版本（SemVer 2.0.0，按其优先级比较）。`role` 必须与端点的调用方一致：`/v1/operator/*`、`GET /v1/nodes`、`POST /v1/sessions` 为 `operator`，`POST /v1/redeem`、`GET /v1/client/sessions` 为 `client`，`/v1/node/*` 为 `hub`，`/v1/sessions/{id}` 下的端点为签名者在该会话中的身份（`role` 决定按哪一方核对：签名者是会话的另一方时返回 `bad_request`，两方都不是时返回 `not_found`）；不一致返回 `bad_request`，防止借用其他角色的最低版本。低于中心最低版本时，写请求返回 `upgrade_required`。
- 所有响应带 `Cache-Control: no-store`，入口代理不得缓存任何 `/v1/` 响应。
- 响应带 `Renet-Request-Id`。中心与代理日志不得记录请求体、响应体、码和完整票据。

### 6.2 错误格式

非 2xx 响应体：

```json
{"error": {"code": "quota_exhausted", "message": "human readable", "retry_after_s": 30, "details": {}}}
```

`code` 是稳定的机器可读值（第 12 节），客户端只按 `code` 分支。没有这个信封的错误响应（例如代理生成的 502）按传输故障处理。

### 6.3 幂等

除心跳外，所有 POST 必须带 `Idempotency-Key`（16–64 字符 `[A-Za-z0-9_-]`，建议 128 位随机数的 base64url）。

| 规则 | 内容 |
|---|---|
| 作用域 | (主体, 方法, 路径, 键)。`/v1/sessions/{id}` 下的端点，主体包含签名者在该会话中已核验的角色：同一设备兼任两方时，一个角色下的结果不会在另一个角色下重发 |
| 提交边界 | 认领、预扣预算、业务条件写和结果引用在同一事务内提交（第 3.2 节）。事务回滚则什么都不留下 |
| 入表的结果 | 终局结果：2xx，以及第 12 节标为 B 的错误（如 `code_invalid`、`quota_exhausted`）。同键重发直接返回它 |
| 不入表的结果 | 暂态结果：标为 R、W、F 的错误（如 `rate_limited`、`quota_releasing`、`session_not_ready`、`clock_skew`、`unavailable`）。事务回滚，同一个键之后可以重新执行 |
| 同键同体重发 | 不再执行，返回原状态码，响应体按结果引用的资源**当前**状态重新生成，并带 `Idempotency-Replayed: true`。这就是结果查询：超时后用同一个键原样重发，换入口也一样。重发需要重新签名（新时间戳），请求体不变 |
| 同键异体 | `idempotency_key_reused` |
| 并发同键 | 只有一个事务认领成功，另一个读到已提交的记录后按重发处理。中心在返回暂态错误（如 `rate_limited`）之前再查一次同键记录，已有终局结果时按重发返回该结果 |
| 保留期 | `idempotency_ttl`。过期后用资源 GET 查询结果：`GET /v1/operator/account`、`GET /v1/sessions/{id}`、`GET /v1/client/sessions` |
| 不重复计数 | 命中幂等记录的重发不计入任何尝试预算 |
| 格式校验 | 请求头或请求体格式校验失败的 `bad_request`（字段缺失、类型或长度不符、码的格式错误等）在认领幂等键之前返回：不入表，不扣任何尝试预算，同一个键修正请求后可以执行 |
| 授权失效 | 执行过程中发现调用方授权已失效（如验签之后设备记录被吊销或移到其他账号）时返回 `unauthenticated`，不入表 |

结果里可能出现的秘密只有数字码：创建会话的结果含码，重发时按第 6.5 节处理。登录、兑换和领票的结果不含可单独使用的秘密（票据绑定设备指纹，没有设备私钥无法使用），重发不需要轮换任何东西。注销或吊销本设备后，重发得到 `unauthenticated`，客户端视为已经完成。

### 6.4 时间

- 设备请求的时间戳超出 `request_window` 时，中心返回 `clock_skew`，`details.server_time` 为中心时间。客户端记下偏移量，校正后重新签名重发（同一个幂等键）。`GET /v1/meta` 也返回 `server_time`。
- Hub 必须校时。心跳响应带 `server_time`；Hub 发现偏差超过 `clock_skew` 时记录错误并拒绝新准入，直到偏差恢复。

### 6.5 返回数字码的请求

明文数字码只在请求带新鲜的 `Renet-Nonce` 时返回：`POST /v1/sessions`（含同键重发）和 Operator 对 `open` 会话的 `GET /v1/sessions/{id}`。中心把 nonce 记录到签名时间戳 + `request_window` 之后；同一 nonce 再次出现返回 `nonce_reused`（F：换新 nonce 重新签名）。不带 nonce 的请求照常成功，只是响应里没有 `code` 字段。这样，拿到请求日志的人也无法通过重放取得数字码。其他请求也可以带 `Renet-Nonce`，签名照常覆盖它，但中心不记录、不去重，也不因它写入任何状态。Operator 读取 `open` 会话时，中心在**收到请求时**记录 nonce，而不是在长轮询结束后：否则重放的请求可以靠等待越过 nonce 记录的保留期。记录 nonce 与复核时间窗是同一个原子操作，按数据库时钟进行：只在该时钟仍处于签名时间窗内时记录，旧记录只在该时钟越过其保留期后才可覆盖；时间窗已过返回 `clock_skew`。读取时会话已不是 `open` 的，它不会再回到 `open`，不记录 nonce；Client 的读取从不记录。

### 6.6 长轮询

`GET /v1/sessions/{id}?since_version=N&wait_s=W`，`W ≤ longpoll_max`，超出返回 `bad_request`；不带 `since_version` 时立即返回，不带 `wait_s` 视为 0。`version > N` 时立即返回，否则最多等待 W 秒后返回当前状态。返回前中心重新核对调用方的资格。长轮询请求的超时为 `W + fallback_attempt_timeout`，不套用普通请求的单次超时。

## 7. 端点

### 7.1 公共

**`GET /v1/meta`**（无认证；唯一的免签名端点，兼作探活）：`center_id`（客户端必须与本地配置一致，否则拒绝使用该入口）、`api_versions`、`server_time`、`min_app_version`（`{operator, client, hub}`）。

### 7.2 Operator 账号与设备

**`POST /v1/operator/login`**（任意设备签名；幂等）

| 请求字段 | 说明 |
|---|---|
| `account_code` | 账号码，中心再做一次规范化 |
| `device_label` | 可选，≤ 64 字节 |
| `salt` | 16 字节随机数 |

中心按账号码找到账号，登记或刷新签名设备的记录（刷新最近登录时间，即重置绝对到期）。设备已属于其他账号时，若它没有非 `ended` 会话则移到本账号，否则返回 `device_busy`。响应：`device`（`device_fp`、`label`、`expires_at`）和 Account 对象（`account_id`、`label`、`status`、`valid_until`、`expiry_grace_s`、`quota`、`quota_in_use`、`server_time`）。

错误：`account_code_invalid`、`account_disabled`、`account_expired`、`device_limit`、`device_busy`、`rate_limited`、`upgrade_required`、`clock_skew`。

设备记录空闲 `device_idle` 未使用即失效，自最近登录起 `device_max` 后失效，失效后须重新输入账号码登录。设备有非 `ended` 会话期间，空闲到期与绝对到期都顺延到会话结束，不会切断进行中的接待。每账号有效设备记录数上限为 `device_limit`。

**`GET /v1/operator/account`**：Account 对象、本设备的 `expires_at`、本账号非 `ended` 会话摘要（`session_id`、`state`、`node_id`、`operator_fp`）。供界面提前提示到期和显示额度占用。

**`GET /v1/operator/devices`**：本账号设备记录列表（`device_fp`、`label`、`last_login_at`、`last_used_at`、`current`）。

**`POST /v1/operator/devices/{device_fp}/revoke`**（幂等）：吊销同账号下的一个设备记录，包括自己；被吊销设备的非 `ended` 会话以 `device_revoked` 结束。这是账号管理操作，用于处理丢失的电脑。

### 7.3 节点列表与会话创建

**`GET /v1/nodes`**：返回可分配的节点，每项为 Node 对象：`node_id`、`region`、`addr`、`port`（Hub 外层 TLS 入口）、`hub_fp`（节点身份指纹）、`state`（`available` / `full`）、`load`（0–100）。`draining` 与离线节点不列出。实测延迟由客户端自己测。

**`POST /v1/sessions`**（幂等；带 `Renet-Nonce` 才返回码）：请求 `node_id`。响应 `201`：Operator 视角的 Session 对象，状态 `open`，含 `code`、`code_expires_at`。事务内容见第 3.2 节。错误：`account_disabled`、`account_expired`、`quota_exhausted`、`quota_releasing`、`device_busy`、`node_unavailable`、`rate_limited`（含全中心开放码已满）、`nonce_reused`。

切换节点：结束当前会话后重新创建，旧码随会话作废。

### 7.4 会话状态

会话状态只前进，不回退：

| 状态 | 含义 | 迁出 |
|---|---|---|
| `open` | 码已签发，等待兑换 | 兑换 → `authorized`；`code_ttl` 到 → `ended(code_expired)` |
| `authorized` | 码已核销，两端可领票并进行首次配对（第 10 节） | Operator 提交建立 → `active`；硬截止到 → `ended(pair_timeout)` 或 `ended(account_expired)` |
| `active` | 配对完成，中心已确认，可以转发诊断流量 | 结束、吊销、到期、空闲 → `ended` |
| `ended` | 终态；占位按第 9.4 节释放 | — |

任何非 `ended` 状态都可能因 `account_expired`、`account_disabled`、`device_revoked`、`node_lost`、`node_disabled`、`admin_revoked`，或两端主动结束进入 `ended`。`active` 另有 `idle_timeout`：节点心跳连续 `session_idle_timeout` 未报告该会话的任何连接，从 `established_at` 与最近一次在 `sessions` 中报告它的心跳两者中较晚的时刻起算。

Session 对象：

| 字段 | Operator 可见 | Client 可见 |
|---|---|---|
| `session_id`、`version`、`state` | 是 | 是 |
| `node`（Node 对象） | 是 | 是 |
| `code`、`code_expires_at` | 仅 `open`，且请求带新鲜 nonce；`code` 为规范化的 8 位数字，显示分组由客户端完成 | 否 |
| `client_fp`、`redeemed_at`、`pair_deadline` | `authorized` 起 | 是 |
| `operator_fp`、`operator_label` | 是 | 是 |
| `expires_at` | 本会话的硬截止（第 9.2 节），供到期提示；`open` 时为 `min(code_expires_at, valid_until)`；已结束时省略 | 否 |
| `end_reason`、`ended_at` | 是 | 是 |

不适用于当前状态的字段省略，不以 `null` 出现（例如 `open` 会话没有 `client_fp`，未结束的会话没有 `end_reason`）。

### 7.5 会话操作

**`GET /v1/sessions/{id}`**（可长轮询）：会话的 Operator 设备或 Client 设备可以读取；其他主体（包括同账号的其他设备）一律 `not_found`。

**`POST /v1/sessions/{id}/tickets`**（幂等）：前置状态 `authorized` 或 `active`。Operator 得到 wait 票据，Client 得到 dial 票据。每条新连接都要一张新票据（`jti` 在 Hub 提交路由时消费）。

| 响应字段 | 说明 |
|---|---|
| `ticket` | base64url 的票据字节，两端视为不透明数据 |
| `ticket_expires_at` | |
| `role` | `wait`（Operator）/ `dial`（Client） |
| `peer_fp` | 路由头 target 必须填它 |
| `node` | Node 对象 |

错误：`session_not_ready`（仍为 `open`）、`session_ended`（附 `end_reason`）、`node_unavailable`、`rate_limited`（含 `ticket_issue_budget`）。距硬截止已不足 `2 × clock_skew + 1` 秒时返回 `session_ended`，原因按即将到期的截止给出。账号到期或停用的会话已被惰性迁移结束，领票得到 `session_ended`（`end_reason` 为 `account_expired` / `account_disabled`）。同键重发返回同一张票据（票据记录不可变，且绑定持票设备）。

**`POST /v1/sessions/{id}/established`**（幂等）：Operator 在第 10.4 节的数据握手完成后提交，请求 `client_fp`，必须等于会话记录中兑换该码的 Client 指纹。`authorized → active`。已是 `active` 时返回成功。会话为 `open`，或 `client_fp` 与会话记录不符时返回 `state_conflict`；Client 调用返回 `forbidden`。这是诊断流量的授权提交点。

**`POST /v1/sessions/{id}/end`**（幂等）：请求 `reason`。Operator 可取 `operator_ended`、`rejected`、`confirm_timeout`、`pake_failed`、`pairing_lost`；Client 可取 `client_ended`、`rejected`、`confirm_timeout`、`pake_failed`、`pairing_lost`。已 `ended` 时返回当前对象（包括惰性迁移刚结束的会话，结束原因保持迁移的原因）。之后不再签发票据，节点在下次心跳收到撤销。

### 7.6 Client

**`POST /v1/redeem`**（任意设备签名；幂等）：请求 `code`、`salt`。响应 Client 视角的 Session 对象，状态 `authorized`。事务内容见第 3.2 节。

码不存在、已过期、已兑换、会话已结束或触发按账号的兑换上限，一律返回 `code_invalid`，不区分原因，避免泄露码的存在性。同一设备换一个幂等键再次兑换同一个码，同样得到 `code_invalid`；要找回已兑换的会话，用下面的列表。错误：`code_invalid`、`rate_limited`、`upgrade_required`、`clock_skew`。

**`GET /v1/client/sessions`**：列出签名设备作为 Client 参与、未结束的会话。用于兑换响应丢失且幂等记录已过期时找回会话。

### 7.7 主体与票据指纹来源

| 端点 | 允许的主体 | 票据 `dev` / `peer` |
|---|---|---|
| `POST /v1/operator/login` | 任意设备（例外：不要求已有设备记录） | — |
| `POST /v1/redeem`、`GET /v1/client/sessions` | 任意设备 | — |
| `POST /v1/sessions`、`GET /v1/nodes`、其余 `/v1/operator/*` | 有效的 Operator 设备记录 | — |
| `GET /v1/sessions/{id}` | 会话的 Operator 设备、会话的 Client 设备 | — |
| `POST .../tickets` | 同上 | `dev` = 签名者指纹（即会话记录中的对应指纹）；`peer` = 会话记录中的另一方指纹 |
| `POST .../established` | 会话的 Operator 设备 | — |
| `POST .../end` | 会话的 Operator 设备、会话的 Client 设备（各自的 `reason` 范围见上） | — |
| `/v1/node/*` | 签名节点，只作用于自身 | — |

同账号的其他设备不能读取或操作别人的会话。Operator 的唯一性按稳定的设备指纹约束，不按登录次数。

### 7.8 节点

**`POST /v1/node/activate`**（节点签名，用请求中的 `node_pub` 验签；幂等，键在每次进程启动时新生成，重试沿用）

| 请求字段 | 说明 |
|---|---|
| `node_pub` | 节点身份公钥；中心要求 SHA-256(`node_pub`) 等于管理员登记的指纹 |
| `version`、`kids` | Hub 版本（语义化版本）；Hub 已配置的票据验签密钥 `kid` 列表，每项为 8 字节 `kid` 的 base64url，至多 16 项 |

首次激活时，中心对登记的地址和端口做连通性校验：以任意自签 Ed25519 设备证书发起外层 TLS 1.3（ALPN `renet-hub/1`），核对 Hub 出示的证书指纹等于登记指纹，然后关闭连接。通过后节点才能被分配。地址只取管理员登记的值。校验也可以由中心运营者的工具在节点激活之后完成：这时激活照常成功、不返回 `node_check_failed`，通过校验之前中心不分配该节点，Hub 无需感知这一区别。

每个新幂等键授予一个新代次，之前的代次立即被取代；同一个键的重试返回原代次，若原代次已被取代则返回 `node_superseded`（第 3.2 节）。重试必须原样重发同一请求体，否则得到 `idempotency_key_reused`。响应：`gen`、`node_state`、`server_time`、`heartbeat_interval_s`、`lease_ttl_s`。错误：`node_unknown`、`node_key_mismatch`、`node_check_failed`、`node_superseded`、`unauthenticated`、`clock_skew`。中心先用 `node_pub` 验签，再查登记；`node_unknown` 与 `node_key_mismatch` 取决于调用方身份，在认领幂等键之前返回，不入表。

**`POST /v1/node/heartbeat`**（节点签名；每 `heartbeat_interval` 一次；不带幂等键）

心跳失败后不重放旧请求，而是在下一个周期发送 `seq` 加一的新心跳；中心按最新状态回复。

| 请求字段 | 说明 |
|---|---|
| `gen`、`seq` | 当前代次；本代次内从 1 起严格递增 |
| `version`、`kids` | 同激活 |
| `load` | `{conns, sessions, waiting, rx_bps, tx_bps}` |
| `sessions` | 当前有连接的会话：`{session_id, state: waiting|paired, rx_bytes, tx_bytes, paired_s}`，计数为本代次内该会话的**累计值** |
| `closed` | 已封存的会话及其最终累计值：`{session_id, rx_bytes, tx_bytes, paired_s}`（第 9.3 节） |

`rx_bytes` = Hub 从该会话两端收到的字节合计，`tx_bytes` = Hub 发往该会话两端的字节合计，均按外层 TLS 解密后的内层字节计；`paired_s` = 该会话处于已配对状态的累计秒数。

`sessions`、`closed` 各至多 `heartbeat_max_entries` 项，同一 `session_id` 在两个列表中合计至多出现一次；计数为 0 到 2⁵³−1 的整数。`kids`、`sessions`、`closed` 为 `null` 时按空列表处理。违反这些格式返回 `bad_request`。

| 响应字段 | 说明 |
|---|---|
| `server_time`、`node_state` | `node_state` 只有 `active` / `disabled` 两种对 Hub 有意义；`draining` 只影响中心的分配，Hub 照常工作。中心内部尚未通过连通性校验的节点按 `active` 下发 |
| `heartbeat_interval_s`、`lease_ttl_s` | 中心策略，Hub 采用最近一次收到的值 |
| `renew` | `{session_id, lease_s}` 列表 |
| `revoke` | `{session_id, reason}` 列表 |

中心对上报的每个会话给出续租或撤销二者之一（第 9.2 节）。`revoke` 还包含绑定本节点、已结束但占位尚未释放的全部会话，直到占位释放为止。用量按 (`session_id`, `node_id`, `gen`) 取最大值合并，`closed` 中的最终值同样合并，响应丢失后重报不会重复记账。

节点只能作用于绑定本节点的会话（第 7.7 节）。`sessions` 中出现的未知会话、其他节点的会话或仍为 `open` 的会话一律撤销，原因固定为 `admin_revoked`，不泄露其他节点会话的状态，也不写入任何状态；`closed` 中的这类会话，以及仍未结束的会话，被忽略。心跳不带 `Idempotency-Key`；带了也只参与签名，不起作用。

`gen` 不是当前代次时返回 `node_superseded`；`seq` 不递增时返回 `bad_request`。错误：`node_superseded`、`node_disabled`、`unauthenticated`、`clock_skew`、`unavailable`。

**Hub 对激活与心跳结果的处理。** 本表是唯一权威，第 9.3 节的心跳失败表是它的摘要。「暂态」指：会话照常转发直到各自的 D，不新增也不撤销任何会话的状态，下个周期发送 `seq` 加一的新心跳（激活则用同一个幂等键、同一请求体按退避重试）。

| 结果 | 激活时 | 心跳时 |
|---|---|---|
| 2xx | 记下 `gen`，`seq` 从 1 起，按 `node_state` 开始工作 | 处理 `renew` / `revoke`（第 9.1、9.3 节），采用新的 `heartbeat_interval_s` / `lease_ttl_s` |
| 传输错误、超时、无错误信封的 5xx、`unavailable`、`internal` | 暂态：同键重试 | 暂态 |
| `clock_skew` | 按 `details.server_time` 记录偏差并报错，校时恢复前不进入中心模式；校时后同键重试 | 暂态；同时停止新准入，直到时钟偏差回到 `clock_skew` 以内（第 6.4 节） |
| `bad_request` | Hub 缺陷或配置错误：记录错误，不进入中心模式 | 暂态（Hub 缺陷，例如 `seq` 未递增或格式错误）；记录错误，下个周期照常发送新心跳 |
| `upgrade_required` | 不进入中心模式，提示升级 | 同 `unauthenticated` |
| `unauthenticated` | 不进入中心模式（节点凭据或时间配置错误） | 立即关闭全部会话，停止准入，继续心跳以便恢复 |
| `node_disabled` | —（激活不返回；停用的节点激活成功，`node_state` 为 `disabled`） | 立即关闭全部会话，停止准入，继续心跳以便恢复 |
| `node_superseded` | 本进程的代次已被取代：不进入中心模式；运维重新启动进程会取得新代次 | 立即关闭全部会话，停止准入并退出中心模式 |
| `node_unknown`、`node_key_mismatch`、`node_check_failed`、`idempotency_key_reused` | 配置错误或 Hub 缺陷：记录错误，不进入中心模式 | — |

`node_state` 为 `disabled` 时，Hub 的行为与收到 `node_disabled` 相同。未进入中心模式的 Hub 不准入任何连接，不回退到无鉴权模式（第 8.6 节）。

## 8. 票据

### 8.1 格式

票据是带标签的 COSE_Sign1（RFC 9052，CBOR tag 18），算法 EdDSA（Ed25519）：

| 部分 | 内容 |
|---|---|
| protected | 字节串，内含 CBOR map `{1: -8, 4: kid}`，确定性编码，恰好这两个键 |
| unprotected | 空 map |
| payload | 字节串，内含 8.2 的 CBOR map，确定性编码 |
| signature | 64 字节 |

签名输入为 COSE 的 `Sig_structure = ["Signature1", protected, h'', payload]`。验证方用收到的 protected 和 payload 原始字节构造签名输入，不重新编码。

`kid` = SHA-256(授权签名公钥) 的前 8 字节。授权签名密钥只签票据，不签其他对象，也不同于安装包更新签名密钥。

### 8.2 字段（payload，keyasint）

| 键 | 字段 | 类型 | 说明 |
|---|---|---|---|
| 1 | `v` | uint | 固定 1 |
| 2 | `center` | tstr ≤ 32 | `center_id` |
| 3 | `acct` | bstr 16 | `account_id`（不是账号码） |
| 4 | `dev` | bstr 32 | 持票设备指纹 |
| 5 | `role` | uint | `1` = dial（Client），`2` = wait（Operator），与 `hub.RoleDial` / `hub.RoleWait` 数值一致 |
| 6 | `peer` | bstr 32 | 对端设备指纹 |
| 7 | `sid` | bstr 16 | `session_id` |
| 8 | `node` | bstr 16 | `node_id` |
| 9 | `hub` | bstr 32 | 节点身份指纹 |
| 10 | `iat` | uint | 签发时间 |
| 11 | `nbf` | uint | 生效时间 |
| 12 | `exp` | uint | 到期时间 |
| 13 | `jti` | bstr 16 | 票据唯一标识 |
| 14 | `gen` | uint ≤ 2³²−1 | 节点代次 |

中心签发时取 `iat = nbf = 当前时间`，`exp = min(iat + ticket_ttl, 硬截止 − 2 × clock_skew)`。时间关系：`iat ≤ nbf < exp`，`exp − iat ≤ ticket_max_ttl`。键 1–63 为约束字段，验证方遇到不认识的约束键必须拒绝；键 ≥ 64 留给不影响准入的信息字段，验证方忽略。改变准入语义只能升 `v`。

### 8.3 尺寸上限

时间与代次按 uint32 编码（各 5 字节）。各字段取最大值：

| 部分 | 字节 |
|---|---|
| payload map 头 | 1 |
| `v`、`role` | 2 + 2 |
| `center`（键 1 + 头 2 + 32） | 35 |
| `dev`、`peer`、`hub`（各为键 1 + 头 2 + 32） | 35 × 3 = 105 |
| `acct`、`sid`、`node`、`jti`（各为键 1 + 头 1 + 16） | 18 × 4 = 72 |
| `iat`、`nbf`、`exp`、`gen`（各为键 1 + 5） | 6 × 4 = 24 |
| **payload 小计** | **241** |
| payload 字节串头 | 2 |
| COSE tag + 数组头 | 2 |
| protected（字节串头 1 + map 头 1 + 键 1、值 -8、键 4 共 3 + kid 字节串头 1 + 8） | 14 |
| unprotected 空 map | 1 |
| signature（头 2 + 64） | 66 |
| **票据合计** | **326** |

票据最大 326 字节（已用 fxamacker/cbor 按核心确定性编码实际核对），低于 Hub 路由头 token 上限 1024 字节，路由头总长不超过 361 字节。中心实现须有测试断言所签发票据 ≤ 512 字节；两端发送前检查不超过 1024 字节。

### 8.4 Hub 校验

中心模式的 Hub 对每个入站请求先在路由锁外完成第 1–9 步，任何一步失败都回 `unauthorized`（状态 3）并关闭连接，原因只写调试日志：

1. token 非空。中心模式下不启用内嵌交接，也不接受空 token。
2. 严格解码：tag 18，4 元素数组，定长，无重复键，嵌套深度有界；protected 恰为 `{1: -8, 4: kid}`；unprotected 为空；signature 为 64 字节。
3. `kid` 在本节点配置的验签公钥中。
4. Ed25519 验签。验签通过之前不解释 payload。
5. payload 严格解码；`v == 1`；必需字段齐全且类型与长度准确；没有未知约束键。
6. `center`、`node`、`hub` 分别等于本节点配置的 `center_id`、`node_id` 和本节点身份指纹；`gen` 等于本进程激活得到的代次。
7. `dev` 等于本连接 TLS 客户端证书算出的设备指纹。
8. `role` 等于路由头 role；`peer` 等于路由头 target（非零）。wait 方必须带非零 target。
9. 时间关系满足 8.2；`iat ≤ now + clock_skew`；`nbf − clock_skew ≤ now ≤ exp + clock_skew`。全部用有符号 64 位运算，避免无符号下溢。

然后在路由锁内一次完成提交：

10. 重新读取当前时间，再查一次 `now ≤ exp + clock_skew`（排队等锁期间票据可能已过期）；本进程处于可准入状态（已激活，未被取代或停用，时钟偏差在容忍内）；`sid` 不在封存集合中；`jti` 未被消费。
11. wait：登记等待项（附 `sid` 与票据 `exp`）。dial：目标 wait 存在且未配对，否则回 `unknown-peer` / `busy` 且**不消费** `jti`，Client 可以用同一张票据重试；dial 与 wait 的 `sid` 不同则回 `unauthorized`。
12. 消费 `jti`（保留到 `exp + clock_skew`），把连接登记到 `sid` 的撤销索引，按第 9.1 节设定或延长该会话的承载截止。

解锁后再写 `ok`。写 `ok` 失败时清理连接，`jti` 保持已消费。

Hub 外层协议的状态码不变：票据过期、被撤销、身份不符都表现为 `unauthorized`。界面需要的具体原因由两端向中心查询会话状态获得。

### 8.5 两端如何使用票据

| 端 | 路由头 | 失败处理 |
|---|---|---|
| Operator | `role=wait`，`target=client_fp`，`token=wait 票据` | `busy`：本设备已有 wait；`unauthorized`：查询会话状态，按结束原因提示，未结束则领新票据重试一次 |
| Client | `role=dial`，`target=operator_fp`，`token=dial 票据` | `unknown-peer`：Operator 尚未登记 wait，带退避用同一票据重试，临近到期时再领新票据；`busy`：提示「Operator 忙」；`unauthorized`：同上 |

两端都把 `node.hub_fp` 传给 `hub.Connect` 钉住 Hub，内层再按对端指纹校验。两种指纹分别校验，不能混用。每条新连接都要一张新票据。

### 8.6 密钥与轮换

- Hub 配置 `center_id`、`node_id`、中心 API 入口、票据验签公钥列表（`kid` 由公钥算出）。任一项缺失，Hub 拒绝以中心模式启动，不回退到无鉴权。
- 验签公钥只通过节点配置下发，不通过任何 API 响应下发：入口代理属于受信基础设施，但不能有能力引入新的签名根。
- 轮换：新公钥加入所有节点配置 → 心跳 `kids` 显示全部活动节点都已加载后，中心改用新密钥 → 经过 `ticket_max_ttl + clock_skew` 后从节点配置中移除旧公钥。
- 签名私钥泄露：立即从全部节点配置中移除对应公钥，所有未用票据随之失效；活动会话不受影响，需要时由中心撤销。

## 9. 租约、撤销与占位

本节的所有上界都以 **Hub 遵守租约** 为前提：Hub 是受信的执行节点，中心无法强迫一个不守规则的 Hub 停止转发。中心能保证的是，遵守本节的 Hub 不会在中心记录的上界之后继续承载。

### 9.1 Hub 的承载截止

Hub 为每个会话维护一个截止时间 D（本地单调时钟），到 D 即关闭该会话的全部连接。D 的设定与连接登记在同一次路由锁内完成（第 8.4 节第 12 步）：

- 准入时：`D = max(D, exp + clock_skew)`。新准入的会话在下一次心跳续租之前，最多承载到票据过期。
- 心跳响应的 `renew` 项：`D = 该次心跳请求的发出时间 + lease_s`（直接赋值，中心可以缩短）。以发出时间为起点，Hub 的截止一定不晚于中心按自己的响应时间记录的上界。
- 超过 `heartbeat_timeout` 才到达的响应丢弃。

中心可达时靠撤销及时关闭，不可达时由 `lease_ttl` 兜底，没有单独的失联宽限。

### 9.2 硬截止与续租条件

每个会话有一个硬截止：

| 状态 | 硬截止 |
|---|---|
| `authorized` | `min(pair_deadline, valid_until)` |
| `active`，且在 `valid_until` 时已是 `active` | `valid_until + expiry_grace` |
| `active`，`valid_until` 之后才进入 `active` | 不会发生：`authorized` 会话在 `valid_until` 就结束 |

`open` 与 `authorized` 会话不享有到期宽限，到 `valid_until` 以 `account_expired` 结束（`authorized` 若先到 `pair_deadline` 则以 `pair_timeout` 结束）。

中心对心跳上报的会话，满足下列全部条件时续租，否则撤销：会话为 `authorized` 或 `active`，绑定本节点，请求代次为当前代次，Operator 设备记录未被吊销，账号未停用，已签发过票据，且未越过硬截止。续租时 `lease_s = min(lease_ttl, 硬截止 − now − 1)`（`now` 为整秒，Hub 发出请求的时刻可能比它晚不到一秒，减一秒保证 D 严格早于硬截止；不足 1 秒时以即将到期的原因撤销），并在同一事务中执行 `carry_bound = max(carry_bound, 响应时间 + lease_s)`。

管理员缩短 `valid_until` 不追溯：它只约束之后签发的票据、续租和会话的惰性迁移；已签发的票据仍以占位的 `carry_bound` 为承载上界，占位保留到那时。需要立即切断时，管理员结束该会话（`admin_revoked`），由心跳撤销下发。管理端修改账号的响应列出该账号仍持有的占位及其当前 `carry_bound`，供管理员判断生效时间。

票据按第 8.2 节取 `exp ≤ 硬截止 − 2 × clock_skew`：Hub 的时钟最多比中心慢 `clock_skew`，又按 `exp + clock_skew` 接受，所以最坏情况下也不越过硬截止。续租用相对时长，不受时钟偏差影响。因此即使 Hub 与中心失联，配对期限和账号到期也是 Hub 执行的硬截止。

### 9.3 撤销与传播上限

| 事件 | 中心动作 |
|---|---|
| 任一端结束会话 | 会话 `ended`，停止签发票据，心跳 `revoke` |
| 管理员停用账号 | 该账号所有会话以 `account_disabled` 结束，无宽限 |
| 账号自然到期 | 见 9.2 |
| 管理员缩短 `valid_until` | 不追溯已签发票据，见 9.2；需要立即切断时结束会话 |
| 重置账号码、吊销设备记录 | 相关设备的非 `ended` 会话以 `device_revoked` 结束 |
| 节点停用 | 心跳响应 `node_disabled`，Hub 关闭全部会话并拒绝准入 |
| 节点离线 | 连续 `node_offline_after_s` 未收到心跳，中心停止为它创建会话和签发票据，其上会话以 `node_lost` 结束（惰性迁移，结束时间为最后一次心跳 + `node_offline_after_s`；节点恢复心跳、覆盖最后心跳时间之前，先在同一事务里落库这一迁移） |
| 管理员结束指定会话 | `admin_revoked` |

Hub 收到 `revoke` 后，在路由锁内一次完成：把 `session_id` 放进封存集合（拒绝其后续准入，保留 `ticket_max_ttl + clock_skew`）、从撤销索引取出该会话的全部连接并关闭。完成之后，才能在**同一代次**的后续心跳 `closed` 中报告该会话及其最终累计用量。中心停止在 `revoke` 中列出它之后，Hub 也停止在 `closed` 中报告。

心跳失败时，Hub 按下表区分：

| 心跳结果 | Hub 行为 |
|---|---|
| 传输错误、超时、`unavailable`、`internal`、无错误信封的 5xx、`bad_request` | 会话照常转发直到各自的 D，下个周期发送新的心跳 |
| `clock_skew` | 同上，另外停止新准入直到时钟偏差恢复 |
| `node_superseded` | 立即关闭全部会话，停止准入并退出中心模式；运维重新启动进程会取得新代次 |
| `node_disabled`、`unauthenticated`、`upgrade_required` | 立即关闭全部会话，停止准入，继续心跳以便恢复 |

完整的处理表（含激活）见第 7.8 节。

撤销传播上限：

- 中心与 Hub 之间可达时：不超过 `heartbeat_interval + heartbeat_timeout`（25 秒），另加中心处理时间。
- Hub 与中心失联时：不超过 `lease_ttl`（5 分钟），且不晚于 9.2 的硬截止。离线验签做不到即时全局吊销，界面与文档都不得宣称即时吊销。

### 9.4 占位释放

会话 `ended` 后，占位在满足下列任一条件时释放：

1. 该会话从未签发过票据；
2. 当前代次的心跳在 `closed` 中报告了该会话，且中心时间已超过 `old_gen_bound`；
3. 中心时间已超过 `carry_bound`。

`old_gen_bound` 在每次授予新代次时抬高（第 3.2 节），保证旧进程可能承载的时间不会被新进程的确认抵消。条件 2 在收到心跳时判定：中心时间尚未越过 `old_gen_bound` 时，这次确认不被记住，会话继续出现在 `revoke` 中，Hub 也就继续在 `closed` 中报告，越过之后的第一次心跳释放占位。旧代次进程恢复后，它的心跳会得到 `node_superseded`，票据的 `gen` 也通不过新进程的校验。

额度为 1 的账号在结束会话后立即创建新会话时，如果唯一的占位正在等待确认，返回 `quota_releasing` 并附 `retry_after_s`（`min(heartbeat_interval, 最早的 carry_bound − now + 1)`，至少 1），客户端在原入口自动重试。Hub 正常时这段等待不超过一个心跳周期；节点故障时最长等到 `carry_bound`，也就是最多 `lease_ttl`。

## 10. 中心模式的首次配对

本节定义首次配对的输入、能力协商、消息、状态机和授权提交点，是这部分的唯一权威。直连模式（R2）使用同一套消息与状态机，区别见第 10.6 节。

### 10.1 PAKE 输入

使用 CPace draft-irtf-cfrg-cpace-21 的 `CPACE-RISTR255-SHA512` 套件（群运算用 `github.com/gtank/ristretto255`）。Client 为发起方（initiator），Operator 为响应方（responder），在两端之间的内层 TLS 1.3 连接内、双方交换 `Hello` 之后运行。

| 输入 | 取值 |
|---|---|
| PRS | 规范化后的 8 位 ASCII 数字码 |
| sid | 内层 TLS 的 `ExportKeyingMaterial("EXPORTER-renet-pake-sid-v1", nil, 32)` |
| CI | `lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", cap, "renet/1", fp_client, fp_operator, SHA-256(HC), SHA-256(HO), hub_fp, center_id, session_id)` |
| ADa / ADb | 空字节串 |

- `lv_cat` 是草案定义的拼接：每个字段前加 LEB128 编码的字节长度。文本字段为不带终止符的 ASCII 字节。
- `cap` 是本次协商出的配对能力（第 10.2 节），中心模式为 `pair-center/1`。`"renet/1"` 是内层 TLS 的 ALPN。
- `HC`、`HO` 是本条控制流上 Client 发出、Operator 发出的 `Hello` 帧的**原始字节**（含 4 字节长度前缀），按收发的原样捕获，不得解码后重新编码。
- `fp_client`、`fp_operator` 取各端在内层 TLS 握手中**实际看到的证书**的指纹。两端以中心给出的对端指纹作为内层 `Expect`，不一致时 TLS 即失败，不进入 PAKE，也不扣预算。
- `hub_fp` 取外层钉住的 Hub 指纹，`center_id` 取本地配置，`session_id` 为 16 字节原始值。两端各自独立取得，不由对方提供。
- `renet-pake-v1` 同时标识草案 -21 与本契约的绑定方式。草案或 RFC 定稿后若有不兼容变化，改用新的标签与能力名，不在 v1 下改含义。
- 每次尝试都生成新的标量，状态只用一次；拒绝非规范编码的点和单位元。`ISK`、`Ta`、`Tb` 按草案 §10.4 计算：`mac_key = SHA-512("CPaceMac" ‖ sid ‖ ISK)`，`Tb = HMAC-SHA-512(mac_key, lv_cat(Yb, ADb))`，`Ta = HMAC-SHA-512(mac_key, lv_cat(Ya, ADa))`，标签 64 字节，用常量时间比较。ISK 不用作流量密钥。

### 10.2 SAS 与承诺

**为什么要承诺。** SAS 只有 6 位。按 CPace 原本的顺序，响应方看到 Ya 之后才选 Yb：冒充 Operator 的中间人可以先和真 Operator 完成 PAKE、得知那一侧的 SAS，再对 Client 一侧反复换 Yb，直到两侧 SAS 相同，约 10⁶ 次标量乘法即可。所以 Client 先发出对 Ya 的承诺，Operator 回复 Yb 之后 Client 才公开 Ya。这样中间人在 Client 一侧必须先发 Yb 才能看到 Ya，在 Operator 一侧发出承诺之后才能看到 Yb，两侧都无法磨 SAS。ISK、Ta、Tb 仍按发起方 / 响应方的固定顺序计算，与实际发送顺序无关。

**能力协商。** 内层 TLS 建立后，两端照常在控制流上交换 `Hello`（Client 先发），用能力决定连接用途：

- 中心模式的配对能力为 `pair-center/1`。Operator 在会话处于 `authorized` 或 `active` 时，Hello 中总是声明它。
- Client 只在本会话还没有钉住记录、要进行配对时声明 `pair-center/1`。双方都声明 → 配对连接，Client 接着发 11；Client 不声明 → 数据连接。
- Client 声明了而 Operator 未声明：Client 关闭连接，提示对端不支持，不计尝试。
- 与直连的 `pair-direct/1` 分开命名，是因为两种模式的确认规则（是否必须核对 SAS）、信任写入（会话钉住还是长期 `trusted_peers`）和 CI 字段都不同。能力名同时写进 CI，一端无法被诱导按另一种模式的规则完成配对。一条连接上 Client 最多声明其中一个。
- 双方 Hello 的原始字节进入 CI，篡改能力列表会使 PAKE 失败，起到降级防护作用。

**消息。** 配对消息与普通 session 消息一样，用 `internal/proto` 的帧（`uint32_be` 长度 + 规范 CBOR 信封 `{1: 消息号, 2: 正文}`）在控制流上传输。新增消息号：

| 号 | 名称 | 方向 | 正文 |
|---|---|---|---|
| 11 | `PakeCommit` | C → O | `{1: commit bstr 64}`，`commit = SHA-512(lv_cat("renet-sas-commit-v1", Ya, ADa))` |
| 12 | `PakeShareB` | O → C | `{1: Yb bstr 32}` |
| 13 | `PakeShareA` | C → O | `{1: Ya bstr 32}` |
| 14 | `PakeConfirmB` | O → C | `{1: Tb bstr 64}` |
| 15 | `PakeConfirmA` | C → O | `{1: Ta bstr 64}` |
| 16 | `PairDecision` | O → C | `{1: accepted bool, 2: reason tstr}` |

ADa、ADb 固定为空，不在消息中传输。配对帧严格解码：规范 CBOR，含信封不超过 256 字节，先检查长度再分配；拒绝未知键、重复键、长度不符、乱序和重复的消息。协商出配对能力后，配对连接上只接受这几种消息，不开放任何流。除等待用户确认外，每一步的等待时间不超过内层 setup 的 10 秒。

`PairDecision` 的 `reason`：

| reason | 模式 | 发送时机与含义 |
|---|---|---|
| `accepted` | 两种 | 收到并校验 15 之后：Operator 已确认并写入信任 |
| `rejected` | 中心 | 收到 15 之后：Operator 用户判定 SAS 不一致 |
| `confirm_timeout` | 中心 | 收到 15 之后：Operator 用户在 `confirm_timeout` 内未处理 |
| `store_failed` | 两种 | 收到 15 之后：Operator 写入信任或钉住记录失败 |
| `budget_exhausted` | 两种 | 代替 12：本会话或本码的尝试预算已用尽 |
| `unavailable` | 直连 | 代替 12：没有可用的码（未生成、已撤销或已兑换） |
| `expired` | 直连 | 代替 12：码已过期 |
| `rate_limited` | 直连 | 代替 12：全局尝试窗口已满 |
| `busy` | 直连 | 代替 12：已有一次尝试在进行 |
| `not_paired` | 中心 | 数据连接：Operator 没有该 Client 在本会话的钉住记录 |

Operator 可以在收到 11 之后、发出 12 之前，用 16 拒绝本次尝试；这种拒绝不扣预算。PAKE 自身的失败（承诺不符、Tb 或 Ta 校验失败、点无效）不发送 16，直接关闭连接，两端都只显示统一的失败文案（第 10.3 节）。`accepted = true` 只在 Client 发出 15 之后有效，其他时刻收到视为协议错误。

**SAS。**

```
SAS = uint64_be(HKDF-SHA-512(IKM = ISK, salt = "", info = "renet-sas-v1", L = 8)) mod 10^6
```

显示为 `XXX XXX`，保留前导零。不能用设备指纹的短 ID 代替：`Fingerprint.Short()` 约 40 位，可以预先穷举出碰撞的密钥。

**中心模式两端都必须人工核对 SAS，不提供自动接受。** SAS 是发现中心或入口冒充的唯一手段，任何一端跳过核对都会使中间人只需骗过另一端；而且只有 Operator 与客户互相报出 SAS，Operator 才能确认屏幕上的来访者就是电话里的那位客户（第 13 节）。

**Client 状态机（配对连接，从协商出配对能力开始）。**

| 状态 | 事件 | 动作 | 下一状态 |
|---|---|---|---|
| C0 | 用户发起配对 | 本会话已用尝试数达到 `pake_budget` → 以 `pake_failed` 结束会话；否则计一次尝试，生成 `ya`、`Ya`，发 11 | C1 |
| C1 | 收到 12 | 发 13，计算 ISK | C2 |
| C2 | 收到 14 | 校验 Tb，失败 → 尝试失败；成功 → 发 15，计算 SAS | C3 |
| C3 | — | `fp_operator` 等于本会话已核对过 SAS 的指纹 → 视为用户已确认；否则显示 SAS，等待用户（最长 `confirm_timeout`） | C3 |
| C3 | 用户确认一致，且已收到 16 `accepted` | 写入钉住记录 `(center_id, session_id, operator_fp)`，记下已核对指纹，关闭连接 | 完成 |
| C3 | 用户确认一致，尚未收到 16 | 记下已核对指纹，继续等待 16 | C3 |
| C3 | 用户选择不一致 / 超时 | 以 `rejected` / `confirm_timeout` 结束会话 | 结束 |
| C1–C3 | 收到 16 拒绝 | 按 `reason` 提示；`budget_exhausted` 时以 `pake_failed` 结束会话，其余情况会话已由 Operator 结束 | 结束 |
| 任意 | 连接中断、超时、格式错误 | 本次尝试失败（已计数）；不自动重试，提示用户可以再次发起 | C0 |

**Operator 状态机（配对连接，从协商出配对能力开始）。**

| 状态 | 事件 | 动作 | 下一状态 |
|---|---|---|---|
| O0 | 收到 11 | 本会话已用尝试数达到 `pake_budget` → 发 16 `budget_exhausted`，以 `pake_failed` 结束会话；否则计一次尝试，保存承诺，生成 `yb`、`Yb`，发 12 | O1 |
| O1 | 收到 13 | 核对承诺，不符 → 尝试失败；计算 ISK、Tb，发 14 | O2 |
| O2 | 收到 15 | 校验 Ta，失败 → 尝试失败；成功 → 计算 SAS | O3 |
| O3 | — | `fp_client` 等于本会话已核对过 SAS 的指纹 → 视为用户已确认；否则显示 SAS 与来访设备，等待用户（最长 `confirm_timeout`） | O3 |
| O3 | 用户确认一致 | 写入钉住记录 `(center_id, session_id, client_fp)`，记下已核对指纹，发 16 `accepted`，关闭连接；写入失败则发 16 `store_failed`，以 `pairing_lost` 结束会话 | 完成 |
| O3 | 用户选择不一致 / 超时 | 发 16 `rejected` / `confirm_timeout`，以同名原因结束会话 | 结束 |
| 任意 | 连接中断、超时、格式错误 | 本次尝试失败（已计数），重新登记 wait 等待下一条连接 | O0 |

Operator 先于 Client 写入钉住记录。若 16 丢失，Client 未完成，由用户再次发起配对；Operator 对同一 `client_fp` 再次成功时不重复弹出确认，直接回复 `accepted`。Client 只有在本地确认且收到 `accepted` 后才写入钉住记录，所以不会出现 Client 已完成而 Operator 未完成的情况。

**数据连接。** Client 的 Hello 未声明 `pair-center/1` 时为数据连接。Operator 有该 `client_fp` 在本会话的钉住记录则继续正常 session，否则发 16 `not_paired` 并关闭。Client 收到 `not_paired` 说明配对状态无法恢复，以 `pairing_lost` 结束会话。

### 10.3 尝试预算

- 两端各自在本地计数，每会话 `pake_budget` 次，不上报中心计数，也不持久化。
- Client：每发出一次 `PakeCommit` 计一次，在收到 Tb 之前中断也计入。Operator：每收到一次 `PakeCommit`、在发出 Yb 之前计一次；以 16 代替 12 拒绝时不计。成功的尝试同样计入，不退还；同一会话内的重新配对也计入。
- 预算用尽即以 `pake_failed` 结束会话，需要 Operator 创建新会话、给出新码。
- 配对不自动重试：每次尝试都由用户发起（Client 点击连接或重试），Operator 只被动响应。
- 所有 PAKE 失败对用户统一报「数字码不正确或连接被篡改」，不区分原因，也不让耗时随原因变化。

### 10.4 顺序、授权提交点与恢复

1. 会话进入 `authorized`。两端各领一张票据，Operator 登记 wait（target 为 `client_fp`），Client dial。
2. 配对连接：双方 Hello 协商出 `pair-center/1`，按 10.2 完成。两端都人工确认 SAS 后写入钉住记录，关闭配对连接。
3. 两端用新票据建立数据连接（Client 的 Hello 不声明配对能力），完成 Hello 握手。中心模式在配对后总是关闭并重连：配对连接只承担配对，诊断流量必须等第 4 步的中心提交，这一步只能在数据连接上进行。
4. Operator 提交 `POST /v1/sessions/{id}/established`，**收到成功响应后**才开放隧道转发诊断流量。提交失败则用同一个幂等键重试；在此之前数据连接只承载控制消息，不开放任何隧道。Client 在通过 `GET /v1/sessions/{id}` 看到 `active` 之前，拒绝 `OpenTunnel`。
5. 到硬截止仍未提交成功，Hub 会在截止时刻关闭连接，会话以 `pair_timeout` 结束，界面提示无法连接中心。
6. 同一会话内的断线重连都是数据连接，只依赖钉住记录，不需要码。

恢复规则：

- 任一端在完成配对（写入钉住记录）之前重启，都以 `pairing_lost` 结束本次接待，需要新码。不尝试恢复口令：码只在 Client 内存中，预算也不持久化。
- 写了钉住记录但无法恢复配对状态（钉住记录丢失或损坏、对端回复 `not_paired`）时，同样以 `pairing_lost` 结束本次接待。
- 完成配对后重启，凭持久化的钉住记录直接建立数据连接。

### 10.5 钉住记录与信任隔离

钉住记录按 `(center_id, session_id)` 持久保存，会话结束即删除，不写入直连模式的 `trusted_peers`。下一次接待即使是同一对设备，也重新配对、重新核对 SAS。这样不同中心、不同会话的信任天然隔离。

### 10.6 直连模式

直连模式使用同一套消息号、编码、MAC、SAS 推导和状态机，区别如下：

| 项 | 直连模式 |
|---|---|
| 配对能力 | `pair-direct/1`。Client 只在用户输入了地址和数字码的那一次拨号中声明 |
| CI | `lv_cat("renet-pake-v1", "CPACE-RISTR255-SHA512", "pair-direct/1", "renet/1", fp_client, fp_operator, SHA-256(HC), SHA-256(HO))`，不含 `hub_fp`、`center_id`、`session_id` |
| SAS | 默认不显示，C3 / O3 的确认视为已通过 |
| 信任写入 | Operator 校验 Ta 后写入长期的 `trusted_peers`，成功后发 16 `accepted`，失败发 `store_failed`；Client 校验 Tb 并收到 `accepted` 后保存对端 |
| 连接沿用 | **直连专属：** 收到 16 `accepted` 后，同一条连接直接转为数据连接，开始正常 session 与隧道。中心模式因为要先提交 `/established`，仍关闭后重连 |
| 拒绝原因 | 可以使用 `unavailable`、`expired`、`rate_limited`、`busy` 代替 12 拒绝 |
| 预算与参数 | 码有效期、单码尝试次数、全局滚动窗口与提交顺序见 R2 设计 [PAIRING §4](PAIRING.md#4-发码计数与信任提交) |

## 11. API 入口回退

- 入口列表随安装包预置，或来自此前已验证的配置；不能在中心不可达时向同一中心查询备用入口。每个入口的 `GET /v1/meta` 返回的 `center_id` 必须与配置一致。
- 每次请求最多尝试 `fallback_attempts` 次，合计不超过 `fallback_total_timeout`，单次超时 `fallback_attempt_timeout`（长轮询见 6.6）。按配置顺序尝试，只有第 12 节标为 R 的错误才换下一个入口，重试时沿用同一个 `Idempotency-Key`。
- 标为 W 的错误按 `retry_after_s` 在原入口等待，不换入口：限流在所有入口间共享，换入口也绕不过去。标为 B 的错误直接呈现给用户，不重试，不换入口。

## 12. 错误码

分类：**R** = 可重试，可以换入口，必须沿用同一 `Idempotency-Key`；**W** = 在原入口按 `retry_after_s` 等待后重试，不换入口；**F** = 按 `details` 修正后重发（同一幂等键）；**B** = 业务拒绝，不重试，不换入口。R、W、F 是暂态结果，不进入幂等记录；B 和 2xx 是终局结果，进入幂等记录（第 6.3 节）。

| code | HTTP | 类 | 含义与客户端处理 |
|---|---|---|---|
| （传输错误、TLS 错误、无错误信封的 5xx） | — | R | 网络超时、连接失败、代理错误 |
| `unavailable` | 503 | R | 中心暂时不可用 |
| `internal` | 500 | R | 中心内部错误；幂等保证重试安全 |
| `rate_limited` | 429 | W | 限流（含登录、兑换、码生成预算和全中心开放码上限），附 `retry_after_s` |
| `quota_releasing` | 409 | W | 额度已满，但占用者已结束、正在等待 Hub 确认 |
| `session_not_ready` | 409 | W | 会话仍为 `open`，继续长轮询后再领票 |
| `clock_skew` | 401 | F | 时间戳超出窗口，附 `server_time`；校正后重新签名 |
| `nonce_reused` | 409 | F | `Renet-Nonce` 已用过；换新 nonce 重新签名 |
| `bad_request` | 400 | B | 字段缺失、格式错误、签名相关头重复、心跳 `seq` 不递增 |
| `idempotency_key_reused` | 422 | B | 同键不同请求体 |
| `upgrade_required` | 426 | B | 应用版本低于中心最低要求 |
| `unauthenticated` | 401 | B | 签名无效，或设备记录不存在、已吊销、已失效；Operator 回到登录，Client 回到输入数字码 |
| `account_code_invalid` | 401 | B | 账号码不存在 |
| `account_disabled` | 403 | B | 账号停用 |
| `account_expired` | 403 | B | 账号到期，附 `valid_until` |
| `device_limit` | 403 | B | 账号设备数已达上限；先吊销旧设备 |
| `device_busy` | 409 | B | 本设备已有未结束的会话 |
| `quota_exhausted` | 409 | B | 账号并发额度已满，附占用中的会话摘要 |
| `node_unavailable` | 409 | B | 节点离线、排空、已满或不可用；改选节点 |
| `code_invalid` | 404 | B | 数字码无效或已过期（不区分） |
| `state_conflict` | 409 | B | 会话不在操作要求的状态，附当前 `state` |
| `session_ended` | 410 | B | 会话已结束或即将越过硬截止，附 `end_reason` |
| `not_found` | 404 | B | 资源不存在或调用方无权访问 |
| `forbidden` | 403 | B | 主体有效但无权执行该操作（如 Client 调用 `/established`） |
| `node_unknown` | 404 | B | 节点未登记 |
| `node_key_mismatch` | 403 | B | 节点公钥与登记指纹不符 |
| `node_check_failed` | 409 | B | 连通性校验失败，附原因 |
| `node_superseded` | 409 | B | 代次已被取代 |
| `node_disabled` | 403 | B | 节点被停用 |

结束原因（`end_reason`）：`operator_ended`、`client_ended`、`rejected`、`confirm_timeout`、`pake_failed`、`pairing_lost`、`code_expired`、`redeem_limit`、`pair_timeout`、`idle_timeout`、`account_expired`、`account_disabled`、`device_revoked`、`node_lost`、`node_disabled`、`admin_revoked`。客户端按这些值给出界面原因，不依赖 Hub 的状态码。

## 13. 猜码预算与风险目标

规模假设：可登录账号 ≤ 1000；同时可兑换的数字码 ≤ `max_open_codes`（1000，由中心在创建会话时强制）。以下风险目标都以「两端人工核对 SAS」为前提，中心模式不提供关闭核对的选项。

**账号码**（40 位，空间 2⁴⁰ ≈ 1.1×10¹²）

- 风险目标：1000 个账号规模下，攻击者用满全局预算一年猜中任意账号的概率 < 1%。
- 预算：失败全局 10 次/分钟；每来源 IP 10 次/10 分钟；每账号码前 2 符号 20 次/小时。每次尝试在查验前预扣，登录成功在同一事务中退还（只计失败）。
- 计算：一年最多 10 × 60 × 24 × 365 ≈ 5.26×10⁶ 次；每次命中概率 ≤ 1000 / 2⁴⁰ ≈ 9.1×10⁻¹⁰；期望命中 ≈ 4.8×10⁻³，一年至少命中一次的概率 ≈ **0.48%**。
- 命中后果：攻击者的设备登记到该账号，出现在设备列表中；管理员重置账号码即清除。全局失败上限被攻击者占满时，合法用户的首次登录会暂时受阻，这是可接受的代价。

**数字码猜中后抢占接待**（空间 10⁸）

- 预算：兑换尝试全局 60 次/分钟；每来源 IP 10 次/10 分钟；每设备 10 次/10 分钟。计的是**尝试**，在查验前原子预扣，成功的兑换也计入。另有每账号成功兑换的滚动上限 30 次/小时，在兑换事务中原子检查，超限的码作废（`redeem_limit`）。
- 计算：每次猜测的命中概率 ≤ `max_open_codes` / 10⁸ = 10⁻⁵。用满全局预算时每天最多 86 400 次，满额 1000 个开放码时期望命中 ≈ 0.86 次/天；同时开放 50 个码时 ≈ 0.043 次/天。
- SAS 对这种情况**无效**：猜中者知道口令，与真 Operator 完成的是一次正常的 PAKE，两边显示相同的 SAS。兜底靠 Operator 与客户的独立沟通：Operator 只在客户本人报出自己 Client 屏幕上的 SAS、且与 Operator 屏幕一致时才确认。码被抢占的客户会在兑换时得到 `code_invalid`，屏幕上没有 SAS，Operator 据此拒绝并重新生成码。
- 风险目标：猜中数字码不导致未授权接入，只造成一次接待重来；Operator 若不经客户报出 SAS 就确认，这一目标不成立，界面文案须明确要求核对。满额规模下攻击者每天约能干扰一次接待；实际规模接近上限时应降低全局预算或 `max_open_codes`。

**中心或入口冒充（中间人）**

- 中间人要让两端的 SAS 相同。有承诺时，每一对尝试（Client 一侧一次、Operator 一侧一次）的 SAS 相互独立且无法被选择，相同的概率为 10⁻⁶；每次尝试都使用新标量。
- 两端各有 `pake_budget` = 3 次尝试，同一会话内最多 3 × 3 = 9 对组合，累计碰撞概率 ≤ 9 × 10⁻⁶。重新配对时若对端指纹与已核对的不同，两端都会重新显示 SAS，中间人无法借用之前的核对结果。

## 14. 参数（已定，2026-11-19）

Hub 需要的值经激活和心跳响应下发；Operator / Client 本地参数由客户端实现采用；其余为中心策略。

| 参数 | 值 | 理由 |
|---|---|---|
| `code_ttl` 数字码有效期 | 10 分钟 | 覆盖经聊天工具转达和客户准备的时间；更长会扩大在线猜中的窗口 |
| `max_open_codes` 全局可兑换码上限 | 1000 | 给每次猜测的命中概率设上界（第 13 节） |
| 码生成预算 | 每账号 30 个/小时 | 正常接待远低于此 |
| 每账号成功兑换上限 | 30 次/小时（滚动） | 与码生成预算一致；独立原子检查，不靠生成预算推导 |
| 兑换尝试预算 | 全局 60 次/分钟；每 IP 10 次/10 分钟；每设备 10 次/10 分钟（含成功） | 见第 13 节 |
| 登录失败预算 | 全局 10 次/分钟；每 IP 10 次/10 分钟；每账号码前 2 符号 20 次/小时 | 见第 13 节；合法登录很少 |
| `pair_timeout` 兑换后完成配对的期限 | 10 分钟 | 包含 Client 选网卡、PAKE、两端核对 SAS、数据握手与提交，避免已兑换但未连通的会话长期占位 |
| `confirm_timeout`（两端本地） | 3 分钟 | 双方正在通话，3 分钟足够核对 SAS；无人确认说明不在场 |
| `pake_budget`（两端本地） | 每会话 3 次 | 一次失败通常意味着替换或中途断线；预扣会把断线也计入，留 2 次余量 |
| `ticket_ttl` 票据有效期 | 120 秒 | 领到后几秒内使用；覆盖 `unknown-peer` 的重试 |
| `ticket_max_ttl` Hub 接受上限 | 300 秒 | 限制签名密钥误用时单张票据的有效窗口 |
| `clock_skew` 时钟偏差容忍 | 30 秒 | 中心与 Hub 都应校时；Hub 偏差超限即停止准入 |
| `request_window` 请求时间戳窗口 | ±60 秒 | 覆盖 `clock_skew` 与网络延迟；客户端按 `clock_skew` 错误自动校正 |
| nonce 保留期 | 签名时间戳 + `request_window` | 覆盖签名最晚失效时刻 |
| `heartbeat_interval` / `heartbeat_timeout` | 15 秒 / 10 秒 | 撤销传播上限 25 秒，请求开销可以接受 |
| `lease_ttl` | 5 分钟 | 同时决定中心故障时会话能撑多久、Hub 失联时撤销最晚多久生效、节点崩溃后额度最晚多久释放；覆盖常见的中心短暂故障与部署 |
| `expiry_grace` 自然到期宽限 | 30 分钟 | 避免诊断中途因到期被硬切断；只给到期时已 `active` 的会话 |
| `session_idle_timeout` | 10 分钟 | 给断网、换网络、睡眠唤醒留出重连窗口，之后释放额度 |
| `node_offline_after_s` | 45 秒 | 3 个心跳周期，及时停止向故障节点分配 |
| `device_idle` / `device_max` | 30 天 / 180 天 | 账号码只在首次登录时输入；半年重新登录一次可以接受；有未结束会话时都顺延 |
| `device_limit` | 每账号 5 台 | 支持多台 Operator 设备，同时限制账号码泄露后的扩散；实际并发仍由额度约束 |
| 默认并发额度 | 1 | 由管理端按账号改写 |
| `idempotency_ttl` | 24 小时 | 远大于签名的最长有效期，留足重试与查询时间 |
| `longpoll_max` | 25 秒 | 低于常见代理的 30 秒空闲超时 |
| `ticket_issue_budget` 领票频率（中心策略） | 每会话每角色 60 张/分钟 | 正常重连每条连接一张票据，远低于此；限制票据记录与幂等记录的增长，超出返回 `rate_limited` |
| `request_body_max` 请求体上限 | 16 KiB | 超出返回 `bad_request`，中心读到超限即停止读取，不先整体读入；心跳另见下一行 |
| `heartbeat_body_max` / `heartbeat_max_entries` 心跳请求体上限 | 512 KiB / `sessions` 与 `closed` 各 1000 项 | 心跳要列出节点承载和封存的全部会话，每项最长约 140 字节，满额约 280 KiB，留一倍余量；中心为每个节点登记的容量不超过 1000，诚实的 Hub 不会超出。验签前先确认节点已激活，未知节点不会读到请求体 |
| `fallback_attempts` / `fallback_total_timeout` / `fallback_attempt_timeout` | 3 次 / 30 秒 / 10 秒 | 有限次回退，用户最多等 30 秒就能看到「中心不可达」 |
| 计量口径 | Hub 外层 TLS 解密后实际转发的字节（即端到端密文流），按会话、按方向计，另计配对时长；按 (会话, 节点, 代次) 上报累计值，会话封存时在 `closed` 中报告最终值，中心取最大值合并后跨代次求和 | Hub 只能看到这一层；累计值合并天然幂等，不需要按 `seq` 去重 |

## 15. 一致性测试向量计划

对应 [CENTER-PROTOCOL 第 11 节](CENTER-PROTOCOL.md#11-一致性测试要求)。下列向量在实现时生成并发布，本文只列清单。向量使用固定的测试密钥与固定时间。Go 端测试只用 loopback、`net.Pipe` 和内存传输，中心由内存假实现替代；中心实现自身另须对真实数据库跑 C1、C2、C9、C10 的并发写入场景，验证唯一约束与条件写，内存假实现不能替代这一项。

**编码与签名向量**

| 编号 | 内容 |
|---|---|
| V1 | 账号码规范化：大小写、`I`/`L`/`O` 替换、`-` 与空白、含 `U` 的非法输入 |
| V2 | 数字码规范化：前导零、空格分组、全角数字、7 位与 9 位非法输入 |
| V3 | 请求签名：设备与节点各一例，含查询串、空请求体、带与不带 nonce 和幂等键；`Renet-Client` 被改动后验签失败；请求目标规范化对照表，以及按非规范请求目标签名时验签失败 |
| V4 | 合法票据：payload、protected、Sig_structure、完整票据字节、签名与长度（最大尺寸一例） |
| V5 | `kid` 推导 |
| V6 | 中心模式与直连模式的 CI 字节（`CPACE-RISTR255-SHA512`，含双方 Hello 原始帧的 SHA-256） |
| V7 | 配对消息 11–16 的 CBOR 编码；承诺、ISK、Tb、Ta（HMAC-SHA-512）与 6 位 SAS（含前导零一例）；CPace 层可与草案 -21 附录 B.3 向量对照 |

**票据拒绝向量**（期望 `unauthorized`）

| 编号 | 内容 |
|---|---|
| V8 | 篡改签名；篡改 payload 一个字节；未知 `kid` |
| V9 | protected 多一个键；unprotected 非空；缺 tag 18；不定长编码；重复键 |
| V10 | `v = 2`；未知约束键（< 64）；另有一张带未知信息键（≥ 64）的票据应当接受 |
| V11 | `center`、`node`、`hub`、`gen` 各不匹配 |
| V12 | `dev` 不等于 TLS 指纹 |
| V13 | `role` 与路由头不符；`peer` 与 target 不符；wait 方 target 为零 |
| V14 | 时间：`exp + clock_skew` 前后 1 秒；`nbf` 在未来；`iat` 超前超过 `clock_skew`；`exp − iat` 超过上限；`nbf ≥ exp`；`iat > nbf`；接近 uint32 上界的时间值 |
| V15 | 超过 1024 字节的 token |

**Hub 行为场景**

| 编号 | 内容 |
|---|---|
| S1 | 重放：同一 `jti` 第二次准入被拒；`unknown-peer` 时不消费 `jti`，同票据重试成功 |
| S2 | 跨会话配对：dial 与 wait 的 `sid` 不同被拒 |
| S3 | 撤销：`revoke` 后关闭两侧连接、封存，未用票据被拒，下次心跳 `closed` 带最终用量 |
| S4 | 竞态：撤销处理与正在提交的准入并发时，连接要么被拒，要么被登记后随即关闭；票据在等待路由锁期间过期时被拒 |
| S5 | 承载截止：新准入只撑到票据过期；心跳续租按发出时间计；失联到 D 时关闭；`node_disabled` 与 `node_superseded` 立即关闭 |
| S6 | 代次：新进程拒绝旧代次票据；旧进程心跳得到 `node_superseded`；激活重试返回原代次，被取代后返回 `node_superseded` |
| S7 | 时钟偏差超限时拒绝新准入 |
| S8 | 正常结束：任一端结束后两侧连接关闭、Hub 状态清空，无协程泄漏 |

**配对场景**

| 编号 | 内容 |
|---|---|
| P1 | 正常配对：两端确认 SAS 后写入钉住记录，数据握手后提交 `/established`，提交成功前不开放隧道；Client 未看到 `active` 前拒绝 `OpenTunnel` |
| P2 | 猜错码：Tb 校验失败，两端各扣一次预算；用尽后 `pake_failed` |
| P3 | 承诺不符；消息乱序、重复、长度错误；在 Tb 前断开仍计数；以 16 代替 12 拒绝时不计数 |
| P4 | 16 丢失：Operator 已完成、Client 未完成，用户再次发起后对同一指纹免确认完成 |
| P5 | 任一端 SAS 判定不一致或超时：会话以 `rejected` / `confirm_timeout` 结束 |
| P6 | 同会话内对端指纹变化：两端重新显示 SAS |
| P7 | 配对前重启、钉住记录丢失、收到 `not_paired`：会话以 `pairing_lost` 结束；配对后重启凭钉住记录重连 |
| P8 | 中间人：同时与两端配对时，两端 SAS 不同（固定随机源下的确定性测试） |
| P9 | 能力协商：Client 声明 `pair-center/1` 而 Operator 未声明时关闭且不计数；篡改任一 Hello（含能力列表）导致 PAKE 失败；数据连接不声明能力 |
| P10 | 直连：`pair-direct/1` 协商，16 `accepted` 后同一连接转为数据连接；`unavailable`、`expired`、`rate_limited`、`busy`、`store_failed` 各自的拒绝路径 |

**中心语义场景**

| 编号 | 内容 |
|---|---|
| C1 | 并发兑换同一码，只有一个成功；同设备换键重兑得到 `code_invalid`，可用 `GET /v1/client/sessions` 找回 |
| C2 | 跨节点额度：同账号在两个节点并发创建会话，额度 1 时只有一个成功 |
| C3 | 同一设备第二个会话 `device_busy`；同设备重复登录不产生第二个设备记录 |
| C4 | 幂等：超时后同键重发返回原状态码与当前资源；同键异体 `idempotency_key_reused`；并发同键只执行一次；`rate_limited` 等暂态结果之后同键可以重新执行 |
| C5 | 入口回退：R 类换入口并沿用同键，不重复创建会话、不重复消费码；B 类和 W 类不换入口 |
| C6 | 数字码读取：不带 nonce 时响应无 `code`；nonce 重复得到 `nonce_reused` |
| C7 | 码到期结束 `open` 会话；硬截止结束未建立的会话，且 Hub 在同一时刻关闭连接；`/established` 持续提交失败时的行为 |
| C8 | 到期：`valid_until` 结束 `open` / `authorized` 会话；`active` 会话在宽限内可重连领票，宽限结束时被撤销；失联的 Hub 在最坏时钟偏差下也不超过硬截止 |
| C9 | 占位释放三条件分别触发；代次切换后新代次的确认不早于 `old_gen_bound` 释放；`quota_releasing`；全中心开放码上限 |
| C10 | 降额度后既有会话继续、新会话被拒；重置账号码后设备记录失效、相关会话 `device_revoked`；按账号成功兑换上限触发 `redeem_limit` |
| C11 | 主体：同账号另一设备读取、领票、结束别人的会话均被拒；Client 只能操作自己的会话 |
| C12 | 用量：心跳响应丢失后重报累计值，不重复记账；`closed` 最终值合并 |

原有的转发、端到端加密和取消行为的测试不能退化。

## 16. Go 端接入点

以下是实现时需要的能力，具体签名在 R2、R3、R5 实现时由整合者确定：

- `internal/hub` 准入：现有 `AuthorizeToken(dev, role, target, token) bool` 在路由锁外执行、只返回布尔值，`wait` / `dial` 也在解锁后才写 `ok`，无法满足第 8.4 节的提交点。需要拆成两段：锁外的验票回调返回准入信息（例如 `Admission{SessionID, JTI, Expires, Gen}`），由 Hub 在路由锁内完成第 10–12 步（重查时间、代次、封存和 `jti`，核对配对双方 `sid`，登记路由、撤销索引与承载截止，消费 `jti`），解锁后写状态。验票回调仍须本地完成、快速返回，不访问网络。
- Hub 维护：每会话承载截止 D 与连接集合、封存集合、`jti` 集合、每会话按方向的累计字节。撤销在路由锁内完成封存与关连接后，才进入 `closed`。中心模式不启用 `Embedded`。
- 票据编解码与验签、中心 API 客户端（含请求签名、nonce 与时间校正）、节点激活与心跳循环分别放在新包中；Hub 包本身只依赖准入回调。
- `secure` 需要导出内层 TLS 的 exporter，标签为 `EXPORTER-renet-pake-sid-v1`。配对连接在 Hello 协商出配对能力后，控制流上只允许第 10.2 节的 11–16 号消息，不开放任何隧道。`internal/proto` 新增 11–16 号消息；session 需要采集双方 Hello 的原始帧字节，并提供 SAS 确认钩子（中心模式必须由用户确认，直连模式默认自动通过）。
- 数据连接上，Operator 在 `/established` 成功前不开放隧道，Client 在看到 `active` 前拒绝 `OpenTunnel`。
- Operator / Client 的钉住记录按 `(center_id, session_id)` 存储，与直连模式的 `trusted_peers` 分开。
- 两端把票据当作不透明字节，发送前检查长度不超过 1024。

## 17. 与 R3 自建授权的关系

[路线图 R3](ROADMAP.md#r3--自建-hub-与中心地址配置) 计划由自建者离线签发票据。离线签发沿用第 8 节的票据格式与验签流程；没有中心时也就没有代次、心跳与租约，8.4 中与代次相关的检查和第 9 节不适用，`ticket_max_ttl` 由自建者在 Hub 配置中设定。这种模式的期限与撤销语义在 R3 中另行规定，本文只覆盖中心模式。

## 18. 修订记录

### 2026-11-19 第二版

依据 [PAKE 选型](research/pake-selection.md) 和一次独立审查修订。审查编号对应如下：

| 编号 | 处理 |
|---|---|
| #1 中心掌握 PAKE 口令 | 新增第 2 节信任模型，写明中心与入口代理在首次配对时受信；缓解措施改为由 ISK 派生的 6 位 SAS 口头核对，不再用指纹短 ID；同步修订 CENTER-PROTOCOL §5、DEPLOYMENT-MODES §4 的措辞 |
| #2 关闭时间晚于额度释放 | 引入独立占位记录和持久化的承载上界 `carry_bound`；新准入只撑到票据过期，续租和票据都抬高同一上界；去掉失联宽限，并入 `lease_ttl`；占位只在 Hub 确认关闭或越过上界后释放 |
| #3 新进程不能证明旧进程已关闭 | 引入中心授予的节点代次，绑定票据（新字段 `gen`）、心跳和租约；旧代次心跳得到 `node_superseded`；新增 `closed` 撤销确认；代次切换时记录 `old_gen_bound`，新代次的确认不早于它释放占位 |
| #4 幂等重放修改凭据 | 去掉不记名令牌，改为逐请求设备签名，重放无需轮换凭据；幂等认领、业务写和结果引用同一事务提交；幂等过期后用资源 GET 和新增的 `GET /v1/client/sessions` 找回结果 |
| #5 幂等表破坏低熵码保护 | 幂等记录只存请求体 HMAC 和结果引用，不存响应体；码只以 HMAC 查找，另以中心密钥加密的短期码记录供 Operator 再次取回，码不可兑换时删除；登录与兑换请求体加 `salt` |
| #6 设备归属与授权 | 设备记录以设备指纹为主键，单 Operator 并发按指纹约束；新增第 7.7 节逐端点列出允许的主体与票据指纹来源；同账号其他设备不能读取或操作别人的会话；规定设备记录到期不切断进行中的会话，吊销则结束 |
| #7 期限未落到 Hub 的硬截止 | 规定只有到期时已 `active` 的会话享有到期宽限；配对期限与账号最终截止写入续租的 `lease_s` 和票据 `exp`；区分心跳的明确拒绝与传输失败，并规定前者立即关闭 |
| #8 猜码预算 | 新增第 13 节，写明规模假设、风险目标和计算；登录全局失败降为 10 次/分钟；兑换加全局开放码上限、全局 60 次/分钟；预算在查验前预扣且计入成功；Operator 本地 PAKE 预算在尝试开始时占用，配对前重启即作废会话 |
| #9 首次配对的提交条件 | 新增配对顺序：配对连接与数据连接按 Client 第一条消息区分；钉住记录写入条件、Ta 或结果丢失时重新配对；`/established` 失败重试，配对期限兜底；PAKE 绑定实际内层证书指纹并以内层 TLS exporter 作 sid；钉住记录按中心与会话隔离 |
| #10 Go 准入提交点 | 第 8.4 节拆为锁外验票和锁内提交；第 16 节写明需要拆分现有回调、锁内完成撤销与 `jti` 检查后再写 `ok` |
| #11 事务设计 | 新增第 3.2 节：占位与码记录独立，列出各操作在一个事务内的检查与写入顺序；票据先签名后登记，提交后才返回；规定降额度策略；第 15 节要求对真实数据库跑并发场景 |
| #12 码的预留、到期与核销 | 兑换限定为 `open`、码未过期、账号与节点合格、占位有效时的一次条件迁移，成功即删除码记录；同键同体按幂等恢复，其他情况一律 `code_invalid`；补充对应测试场景 |
| #13 时序与状态分叉 | 长轮询单列超时；离线判定统一为 `node_offline_after_s`；`draining` 只影响中心分配，Hub 照常允许既有会话重连；票据时间顺序和有符号运算写入第 8.2、8.4 节和拒绝向量 |
| #14 计量与入口 | 计量改为每 (会话, 节点, 代次) 的单调累计值，中心取最大值合并；去掉入口黏滞策略，只保留可信列表与有限回退 |

另外做的减法：去掉 `POST /v1/challenge` 与持有证明（签名请求本身即持有证明）；去掉中心侧的「确认来访设备」状态和 `confirm` / `reject` 端点（确认移到 PAKE 之后、在本地完成，才能显示 SAS）；去掉 `pake-failures` 端点和中心侧 PAKE 计数；去掉 `confirm_device` 字段；去掉心跳中的 `admitted` 上报和 `boot_id`（由 `closed` 确认和代次取代）；去掉 `idempotency_in_progress`、`challenge_invalid`、`proof_invalid`、`device_session_limit` 等错误码，以及 `outage_grace`、`challenge_ttl`、`idempotency_ttl_secret`、`fallback_sticky`、票据签发频率等参数；中心模式不再把配对结果写入长期信任。

### 2026-11-19 第三版（定稿）

用户确认信任模型（中心是首次配对的受信方，SAS 必须核对）与第 14 节全部参数，去掉所有「提议，待确认」标注。依据第二轮独立复核与 PAKE 选型作者指出的问题修订：

| 编号 | 处理 |
|---|---|
| 决定 1 · Client 侧确认 SAS | Client 写入钉住记录的条件改为 Tb 校验通过、本地用户确认 SAS 一致、且收到 Operator 的 `accepted`；中心模式取消自动接受，两端都必须人工核对，第 10.2 节写明原因 |
| 决定 2 · Client 侧预算 | Client 每发出一次承诺计一次，Tb 之前中断也计入，每会话 3 次；配对不自动重试；第 10.2 节状态机与第 10.3 节措辞统一 |
| 决定 3 · 同会话重新配对 | 对端指纹与本会话已核对过 SAS 的指纹相同则免核对，否则两端重新显示 SAS |
| 决定 4、复核 #5 · 重启与恢复 | 配对完成前重启，或钉住记录无法恢复（含对端回复 `not_paired`），都以新增的 `pairing_lost` 结束本次接待，需要新码，不尝试恢复口令 |
| 决定 5、复核 #5 · 授权提交点 | `active` 定为诊断流量的授权提交点：数据握手完成、Operator 收到 `/established` 成功响应后才开放隧道；Client 在看到 `active` 前拒绝 `OpenTunnel` |
| 决定 6 · 直连模式 | 新增第 10.6 节：直连复用同一套消息与状态机，默认不显示 SAS；DEPLOYMENT-MODES §4 补一句说明 |
| 消息权威 | 第 10.2 节写全配对消息号 11–16、CBOR 编码、SAS 推导和两端状态机，不再委托调研文档；新增配对测试场景 P1–P8 |
| 复核 #1 · 激活重试 | 新增激活记录：同一幂等键返回原代次，被取代后返回 `node_superseded`；`closed` 只能在同一代次下、于锁内完成封存与关连接之后上报 |
| 复核 #2 · 硬截止 | `authorized` 的硬截止统一为 `min(pair_deadline, valid_until)`；票据 `exp ≤ 硬截止 − 2 × clock_skew`，最坏时钟偏差下也不越过；Hub 在锁内重查时间并原子登记截止；第 9 节开头写明上界依赖 Hub 遵守租约 |
| 复核 #3 · 签名与重放 | 签名输入加入 `Renet-Client` 与可选 `Renet-Nonce`；规定代理原样转发请求目标与签名相关头，重复头拒绝；写明签名最长有效期为 `2 × request_window`，去重记录覆盖它；返回数字码的请求必须带一次性 nonce，新增 `nonce_reused`；删除「结果中没有秘密」的表述，改为准确描述 |
| 复核 #4 · 风险目标 | 第 13 节限定在两端核对 SAS 的前提下；写明两侧各 3 次尝试、每次新标量、中间人累计碰撞上界 9×10⁻⁶；如实写明 SAS 不防猜码抢占，兜底靠 Operator 与客户互报 SAS |
| 复核 #6 · 核销与幂等 | 数字码只在兑换时核销，删去 `/established` 处的核销表述；认领幂等键与预扣预算同一事务；规定终局结果入幂等表、暂态错误不入表且可同键重新执行；创建会话事务显式检查全中心开放码上限 |
| 复核 #7 · 主体与时间 | 第 7.7 节显式列出登录端点例外；设备空闲到期在有未结束会话时同样顺延；规定票据 `iat = nbf = 签发时间` 及 `iat` 的超前容忍 |
| 复核 #8 · 按账号兑换上限 | 新增每账号成功兑换 30 次/小时的滚动上限，在兑换事务中原子检查，超限的码以 `redeem_limit` 作废 |
| 复核 #9 · 上位文档与计量 | 同步 CENTER-PROTOCOL 的核销时点与心跳重试语义、DEPLOYMENT-MODES §4 的 PAKE 选型与信任范围；`closed` 携带会话最终累计用量 |
| 复核 #10 | 已闭合；S4 补「等待路由锁期间票据过期」场景 |

### 2026-11-19 第四版：与 R2 分支对齐

community 分支 `continue-planned-work` 已独立实现 R2 直连数字配对。比对后按以下决定修订第 10 节（编号对应比对得出的冲突清单）：

| 编号 | 处理 |
|---|---|
| C1 · 套件 | 采纳 R2：套件改为 `CPACE-RISTR255-SHA512`，群运算用 `github.com/gtank/ristretto255`；协议层不变。`renet-pake-v1` 标签承担草案修订号的版本区分 |
| C2 · Hello 先行（用户决定） | 采纳 R2：配对连接先交换 Hello，用能力协商用途；直连为 `pair-direct/1`，中心模式新定 `pair-center/1`，并说明分开命名的理由；双方 Hello 原始帧的 SHA-256 与能力名写进 CI。删去「Client 第一条消息决定连接用途」，同步修改状态机、数据连接判定、V6 与配对场景（新增 P9） |
| C3 · 连接沿用（用户决定） | 直连在 16 `accepted` 之后沿用同一条连接作为数据连接；中心模式仍关闭后重连，因为诊断流量要等 `/established` 提交。第 10.6 节标明这是直连专属差异（新增 P10） |
| C4 · 拒绝原因 | 16 增加直连用的 `unavailable`、`expired`、`rate_limited`、`busy` 和两种模式通用的 `store_failed`；写明 Operator 可在发出 12 之前用 16 拒绝且不扣预算；PAKE 自身的失败不发 16 |
| C5 · 直连参数 | 第 10.6 节改为表格，预算与参数指向 R2 设计 PAIRING §4 |
| 不让步项 | 确认 MAC 保持草案 §10.4 的 HMAC-SHA-512（64 字节）；CI 用草案的 LEB128 `lv_cat`；exporter 标签 `EXPORTER-renet-pake-sid-v1`；消息号 11–16。R2 分支需要相应调整 |
| 简化 | 配对消息正文改为单字段 map（16 为两字段），去掉 11 的版本字段和 12、13 中恒为空的 AD 字段，版本由能力名和 CI 承载；配对帧上限 256 字节 |

### 2026-11-19 第五版：实现反馈

一次中心实现与只读审查中发现的问题，按最保守的解释落定如下：

| 编号 | 处理 |
|---|---|
| 实现 1 · 请求目标 | 第 5.1 节把签名覆盖的 `path_and_query` 从「请求目标原样字节」改为 WHATWG URL 规范化后的 pathname + search（裸 `?` 视为无查询）。部分 HTTP 运行时拿不到原始字节，原定义无法实现；客户端必须按规范形式发送，非规范形式验签失败。V3 增加反例向量和规范化对照表 |
| 实现 2 · 重复头 | 第 5.1 节写明重复头按合并后的逗号判定：上表各头的合法取值都不含逗号 |
| 实现 3 · 免签名端点 | 第 5.1、7.1 节写明 `GET /v1/meta` 是唯一的免签名端点，兼作探活，不另设健康检查端点 |
| 实现 4 · `Renet-Client` 角色 | 第 6.1 节规定 `role` 必须与端点的调用方一致，`app_version` 按 SemVer 2.0.0 比较；不一致返回 `bad_request` |
| 实现 5 · 格式校验与授权失效 | 第 6.3 节新增两行：格式校验类 `bad_request` 在认领幂等键之前返回，不入表、不扣预算；执行中发现调用方授权失效返回的 `unauthenticated` 不入表 |
| 实现 6 · 暂态错误与同键 | 第 6.3 节「并发同键」补充：返回暂态错误前复核同键记录，已有终局结果时返回该结果，不返回 429 等暂态错误 |
| 实现 7 · nonce 范围 | 第 6.5 节写明只有返回数字码的请求消费并去重 nonce；其他请求的 nonce 只参与签名，不记录 |
| 实现 8 · 请求体上限 | 第 14 节新增 `request_body_max` = 16 KiB，超限即停止读取；心跳可能单独放宽 |

### 2026-11-19 第六版：实现反馈（会话）

实现中心会话部分时发现的歧义，按最保守的解释落定。都不改变协议外形，Go 端按原文实现即符合：

| 编号 | 处理 |
|---|---|
| 实现 9 · 惰性迁移的时刻与并列 | 第 3.2 节写明惰性落库的 `ended_at` 记为迁移到期的时刻，不是被观察到的时刻：否则同一会话的结束时间取决于谁先来读，Client 的 10 分钟可读窗口也随之漂移。几个截止同时越过时取最早的那个，与 `valid_until` 并列时按 `account_expired`。管理端修改 `valid_until` 或账号状态前先按旧值落库已到期的迁移：惰性执行不能让「延长有效期」追溯复活已越过截止的会话，Hub 在那个截止已经关闭了连接 |
| 实现 10 · 兑换时的「节点可分配」（用户决定） | 第 3.2 节写明对已分配到该节点的会话，兑换不计节点容量（会话的占位已在创建时计入，否则满载节点会拒绝自己的会话）；`draining` 只影响新分配，`open` 会话照常可兑换，与领票一致。节点停用或离线超时时，会话按 `node_disabled` / `node_lost` 惰性结束，Operator 轮询能看到原因，而不是停在一个永远兑换不了的 `open` |
| 实现 11 · `quota_releasing` 的判定 | 第 3.2、9.4 节写明：未释放占位数达到额度，且其中未结束会话的占位数已低于额度时才返回 `quota_releasing`（等待确认能腾出名额），否则 `quota_exhausted`；`retry_after_s` 取心跳周期与最早 `carry_bound` 的较小者。原文只描述了额度为 1 的情形 |
| 实现 12 · 会话路径的角色 | 第 6.1 节写明 `/v1/sessions/{id}` 下 `Renet-Client` 的 `role` 决定按哪一方核对：签名者是会话的另一方时 `bad_request`，两方都不是时 `not_found`（不泄露会话存在）。同一设备兼任两方时按声明的角色处理 |
| 实现 13 · 读取码时 nonce 的记录时机 | 第 6.5 节写明 Operator 读取 `open` 会话时在收到请求时记录 nonce。若在长轮询结束后才记录，重放的请求可以先等待到签名时间戳 + `request_window` 之后，那时 nonce 记录已过保留期，重放就能取得码。审查补充：记录与时间窗复核是同一条按数据库时钟执行的语句，否则重放在窗口最后一秒通过时间校验、写入时已跨到下一秒，旧记录被当作过期删除，同样能取得码 |
| 实现 14 · 长轮询参数 | 第 6.6 节写明 `wait_s > longpoll_max` 返回 `bad_request`，缺省参数的含义，以及返回前重新核对调用方资格（等待期间设备可能被吊销） |
| 实现 15 · Session 对象 | 第 7.4 节写明不适用的字段省略而不是 `null`；`open` 会话的 `expires_at` 为 `min(code_expires_at, valid_until)`，已结束时省略；`code` 为规范化的 8 位数字。`code_expires_at` 按原表与 `code` 同进退，只在带新鲜 nonce 时出现 |
| 实现 16 · `/established` 的校验 | 第 7.5 节写明请求的 `client_fp` 必须等于兑换该码的 Client 指纹，否则（以及会话仍为 `open`）返回 `state_conflict`，Client 调用返回 `forbidden`。授权提交点不能提交一个与兑换者不同的对端。`forbidden` 在查幂等记录之前判定、不入表：它取决于调用方身份而不是业务状态，与「授权失效」同类 |
| 实现 17 · 领票的到期与重发（用户决定） | 第 7.5 节写明账号到期 / 停用的会话已被惰性迁移结束，领票得到 `session_ended`（`end_reason` 为 `account_expired` / `account_disabled`），并从领票端点的错误列表中删去 `account_expired`；同键重发返回同一张票据 |
| 实现 18 · Client 可读窗口的起点 | 第 5.2 节写明 10 分钟从 `ended_at` 起算 |
| 实现 19 · 提交时的时钟 | 第 3.2 节写明与时间有关的前提在提交时按数据库时钟复核。规划时的 `now` 可能早于提交（数据库延迟，部分运行时的时钟只在 I/O 后前进），沿用它会把截止后才提交的写入按截止前结算 |
| 实现 20 · 终局拒绝的复核 | 第 3.2 节写明 `quota_exhausted`、`code_invalid`、`session_ended` 等入表的拒绝在提交时复核条件仍成立。否则一个在提交前就已不成立的拒绝会成为该幂等键的永久结果 |
| 实现 21 · 兼任两方的幂等作用域 | 第 6.3 节写明会话端点的作用域含已核验的角色。同一设备作为 Operator 用某个键领到的 wait 票据，不能被它作为 Client 用同一个键重发取得 |
| 实现 22 · 节点离线的惰性迁移 | 第 9.3 节写明 `node_lost` 惰性落库及其结束时间，并要求恢复心跳前先落库，否则新心跳会抹掉离线期 |
| 决定 · 缩短 `valid_until`（用户决定） | 第 9.2、9.3 节写明缩短有效期不追溯已签发票据：只约束之后的票据、续租与惰性迁移，已签票据以占位的 `carry_bound` 为承载上界；需要立即切断时用管理员结束会话（`admin_revoked`）。管理端响应返回受影响占位的当前 `carry_bound` |
| 实现 23 · 领票频率（用户决定） | 第 14 节新增中心策略参数 `ticket_issue_budget`（每会话每角色 60 张/分钟），第 7.5 节的 `rate_limited` 注明包含它。领票本身没有其他上限，已认证的一方可以无限领票，使票据记录与幂等记录无界增长 |

### 2026-11-19 第七版：实现反馈（节点）

实现中心节点部分时发现的歧义，按最保守的解释落定。都不改变协议外形，Go 端按原文实现即符合：

| 编号 | 处理 |
|---|---|
| 实现 24 · 连通性校验的执行方 | 第 7.8 节允许由中心运营者的工具在激活之后完成连通性校验。部分 HTTP 运行时不能发起这种 TLS 连接（要出示自签客户端证书、协商 ALPN、钉住自签服务端证书），这时由运营者工具执行校验并上报 Hub 出示的指纹，中心核对后记录。激活因此不返回 `node_check_failed`；未通过校验的节点不分配。Hub 看到的行为与「激活时校验通过」相同 |
| 实现 25 · 未校验节点的 `node_state` | 第 7.8 节写明中心内部的 `pending`（未通过连通性校验）按 `active` 下发。Hub 只区分 `disabled` 与其余，原文没有 `pending` 这个值 |
| 实现 26 · `kids` 编码 | 第 7.8 节写明每项为 8 字节 `kid` 的 base64url（11 字符），至多 16 项；`version` 为语义化版本。原文未规定 JSON 中的编码 |
| 实现 27 · 心跳格式 | 第 7.8 节写明 `sessions` / `closed` 的项数上限、同一会话在两个列表中合计至多出现一次（一个会话要么还有连接，要么已封存）、计数范围，以及 `null` 列表按空处理（Go 把 nil 切片编码为 `null`） |
| 实现 28 · 不属于本节点的会话 | 第 7.8 节写明 `sessions` 中的未知、他节点或 `open` 会话以固定原因 `admin_revoked` 撤销且不写任何状态，`closed` 中的这类会话和未结束会话被忽略。原文要求对每个上报会话给出续租或撤销，但没有规定这类会话的原因；按会话的真实原因回答会泄露其他节点会话的状态 |
| 实现 29 · 激活的身份拒绝 | 第 7.8 节写明先用 `node_pub` 验签再查登记，`node_unknown`、`node_key_mismatch` 在认领幂等键之前返回、不入表（与第六版的 `forbidden` 同类）；同键重试须原样重发请求体 |
| 实现 30 · 续租的时间基准 | 第 9.2 节改为 `lease_s = min(lease_ttl, 硬截止 − now − 1)`：`now` 是规划时的整秒中心时间，Hub 发出请求的真实时刻可能比它晚不到一秒，原式会让 D 越过硬截止不到一秒；剩余不足 1 秒时以即将到期的原因（`pair_timeout` / `account_expired`）撤销。`carry_bound` 取提交语句的时钟加 `lease_s`，晚于发出时刻，所以不低于 D。规划到提交之间会话结束、硬截止被缩短或代次 / `seq` 变化时整次重新规划，续租总是按提交时的硬截止 |
| 实现 30a · 从未签票的会话不续租 | 第 9.2 节续租条件加上「已签发过票据」。诚实的 Hub 不会承载没有票据的会话；为它续租会把 `carry_bound` 从 NULL 改为非空，会话结束时占位就不能按条件 1 立即释放。这类上报以 `admin_revoked` 撤销 |
| 实现 30b · 批内时钟 | 实现说明，不改文本：同一批语句的时钟可能在语句之间跳到下一秒。心跳刷新 `last_heartbeat_at` 的语句自身复核没有到期未落库的迁移，批末再复核一次，否则整次重新规划。否则恰好静默 `node_offline_after_s` 的节点会在刷新时抹掉 `node_lost`，恰好到期的空闲会话会被迟到的报告复活 |
| 实现 30c · 用量的记录范围 | 实现说明，不改文本：`sessions` 中只记录本节点非 `open` 会话的用量，`closed` 中只记录本节点已结束会话的最终值，与第 7.8 节「忽略」的范围一致 |
| 实现 31 · 撤销列表的完整性 | 实现说明，不改文本：提交时复核本节点已结束且占位未释放的会话都在本次 `revoke` 中，否则重新规划。规划之后才结束的会话因此不会漏到下一次心跳，撤销传播上限按提交时刻成立 |
| 实现 32 · 越过 `old_gen_bound` 之前的 `closed` | 第 9.4 节写明条件 2 在收到心跳时判定，之前的确认不被记住：会话仍在 `revoke` 中，Hub 会继续报告，越过后的第一次心跳释放。比记住确认多等至多一个心跳周期，换来不需要新的持久状态 |
| 实现 33 · `idle_timeout` 的起点 | 第 7.4 节写明从 `established_at` 与最近一次报告它的心跳中较晚者起算；惰性迁移的结束时间为到期时刻，与 `node_lost` 同时取 `node_lost`，与状态截止同时取状态截止。心跳先落库已到期的迁移再记录本次报告，迟到的报告不会复活已空闲超时的会话 |
| 实现 34 · 心跳请求体上限 | 第 14 节新增 `heartbeat_body_max` = 512 KiB 与 `heartbeat_max_entries` = 1000，节点容量不超过 1000。心跳验签前先确认节点已激活，未知节点在读取请求体之前就得到 `unauthenticated` |
| 实现 35 · 激活记录的保留期 | 中心实现说明：被取代的激活记录保留到取代它的激活满 `idempotency_ttl` 为止，当前代次的记录一直保留。之后用同一个旧键重试会被当作新键、授予新代次，所以 Hub 对同一个键的激活重试应在 `idempotency_ttl` 内结束（Hub 只在启动时重试激活，实际远短于此） |
| 决定 · Hub 对错误的处理（用户决定） | 第 7.8 节新增 Hub 对激活与心跳各结果的处理表，第 9.3 节同步。原表只覆盖传输错误、`unavailable`、`node_superseded`、`node_disabled`、`unauthenticated`。新增：`internal`（R 类）与 `unavailable` 相同；`clock_skew` 按暂态失败处理，同时停止新准入直到校时恢复；心跳的 `bad_request` 是 Hub 缺陷，按暂态失败处理；`upgrade_required` 与 `unauthenticated` 相同（关闭全部会话、停止准入、继续心跳）；激活的各类终局错误一律不进入中心模式 |
| 决定 · 第 24、25、28 条与 `kids` 不下发（用户确认） | 实现 24（连通性校验由运营者工具在激活后完成）、实现 25（`pending` 按 `active` 下发）、实现 28（外节点会话撤销原因固定为 `admin_revoked`）经用户确认。心跳响应不下发 kid 列表：第 8.6 节规定验签公钥只经节点配置下发，中心只在管理端显示各节点是否已加载当前 kid |
| 向量 | 第 15 节 V3 新增 `node-activate` 一例：节点主体为 `node_id`，但用请求体中的 `node_pub` 验签，带幂等键。原有向量逐字节不变 |

| 决定 · 计量方向 | 第 7.8 节写定：`rx_bytes` = Hub 从会话两端收到的字节合计，`tx_bytes` = 发往两端的合计，按外层 TLS 解密后的内层字节计 |
