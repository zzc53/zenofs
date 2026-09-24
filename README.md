# zenofs

ZenoFS —— 基于 Reed-Solomon 纠删码的 NAS：客户端通过 **SMB / WebDAV / SFTP** 接入。

## 项目简介

ZenoFS 是一个自托管网络存储（NAS）：客户端用 SMB（Windows/macOS 共享）、WebDAV 或 SFTP 挂载，
看到的是"用户 → Share → 目录/文件"这棵树；三种协议的服务端共用同一套 `internal/vfs`
文件系统接口，差异（分隔符、转义、错误码）只在协议层抹平。

存储层不直接对外提供文件访问：写入的数据被切成 N 个 data chunk，用 RS 编码算出 M 个 parity chunk，
打散落到池内不同磁盘上，任意丢失不超过 M 个分片都能重建。另外提供一个 HTTP REST API，
用于存储池 / 磁盘 / 分片这一层的管理——它不是面向最终用户的文件访问接口。

当前状态：存储层（pool / disk / stripe / chunk / parity / cache）已可用；Share Layer 的模型
（User / Share / Inode / Version）已在 `internal/db/models.go` 定义并建表；`internal/vfs` 提供
SMB / WebDAV / SFTP 共用的文件系统接口、单 Share 实现（`ShareFS`）与多 Share 聚合层
（`RootFS`）。**尚缺**：三个协议的服务端本身，以及用户认证与授权管理接口。

## 技术栈

| 用途 | 依赖 |
|---|---|
| 语言 | Go 1.26.5（见 `go.mod`） |
| HTTP 路由 | `github.com/go-chi/chi/v5` |
| ORM | `gorm.io/gorm` + `driver/sqlite`、`driver/mysql`、`driver/postgres` |
| 纠删码 | `github.com/klauspost/reedsolomon` |
| 哈希 | `github.com/zeebo/blake3` |
| 并发 | 标准库 `sync` + 项目内自建的 `parallelEach`（`internal/pool/parallel.go`） |

依赖放在本地的 `vendor/` 目录（构建默认走 `-mod=vendor`，该目录不入库）；改动依赖后用 `go mod vendor` 重新同步。

## 常用命令

```bash
go build ./...                        # 构建
go vet ./...                          # 静态检查
gofmt -l ./cmd ./internal             # 格式检查
go test ./...                         # 单元测试（见"测试"一节）

go run ./cmd/zenofs                   # 默认 sqlite://zenofs.db
go run ./cmd/zenofs "postgres://..."  # 也可用 ZENOFS_DSN 环境变量

cd web && npm ci && npm run build     # 构建前端（产物 embed 进二进制，见"Web 前端"一节）
```

DSN 前缀决定驱动（`sqlite://` / `mysql://` / `postgres://`）。HTTP 端口取自 `settings` 表的
`HTTP_PORT`（默认 8080）。

SQLite 这边是三件互相配合的事（见 `internal/db`）：

1. **连接参数**：WAL（读不挡写）、`foreign_keys`、`busy_timeout=5000`（锁等待上限），以及
   `_txlock=immediate`（事务用 `BEGIN IMMEDIATE`）。最后这条针对的是一个具体的坑：
   `getNewChunks` 的事务是"先读预留槽位、再建 stripe"，如果事务以默认的 `BEGIN DEFERRED`
   开始，两个并发写事务会各自持着共享锁再抢升级，SQLite 把这种情况判成死锁并**立即**返回
   `SQLITE_BUSY`（`busy_timeout` 对死锁无效——等下去也不会有人放手），日志里就是
   `chunk.go:... database is locked`。
2. **单连接**：`SetMaxOpenConns(1)`。SQLite 是单写者数据库，与其放多个连接去抢那把写锁
   （抢不到就耗 `busy_timeout`，请求一多看起来就是卡死），不如只留一个连接、让事务在应用层
   排队——排队是能被超时打断的。WAL 下读本来就不挡写，代价是并发读。
3. **事务超时**：所有事务都走 `DbManager.Tx()`（而不是直接 `DB.Transaction`），默认 15 秒，
   由 `TxTimeout` 调。超时会取消底层 context，卡住的事务最坏只是失败，不会把后面的请求
   一起拖死。

`Tx()` 还会把跑得慢的、以及被超时掐断的事务记进日志，并附上连接池状态：

```
db: transaction took 300ms (timeout 300ms, err=context deadline exceeded) [pool open=1 in_use=1 idle=0 wait=1]
```

`wait` 持续增长就说明有事务占着连接不放——这正是"数据库卡死"的样子；光看错误信息是看不出来的，
所以这几个数字要留在日志里。慢 SQL（>1s）由 GORM 自己记录。


## 测试

```bash
go test ./...                            # 全部包
go test -race ./...                      # 竞态检测（并发读写池/文件系统的路径值得跑一遍）
go test -cover ./...                     # 覆盖率
go test ./internal/vfs -run TestRoot -v  # 单包 / 单组用例
```

测试一律打真实实现：每个用例用 `t.TempDir()` 里的临时 SQLite 库 + 临时磁盘目录，
SQLite 库通过 `internal/testutil` 建好并 `AutoMigrate`，不起任何 mock。
因此 **测试需要 CGO**（`mattn/go-sqlite3` 是 cgo 驱动）：交叉编译或用 `CGO_ENABLED=0` 的环境跑不了。
随机数据由 `testutil.RandBytes(seed, n)` 生成，固定 seed 可复现。

| 包 | 覆盖内容 | 覆盖率 |
|---|---|---|
| `internal/hash` | 摘要定长/确定性、`SumArray` 与 `Sum` 一致、`Equal` 的"没有期望值就不校验"语义 | 100% |
| `internal/errs` | 数值码与字符串码唯一、`Error()` 文案、`errors.As` 取出 `*ZenoError`、`Unwrap` 让 `errors.Is` 穿透包装 | 100% |
| `internal/config` | 配置优先级 argv > `ZENOFS_DSN` > 默认值 | 100% |
| `internal/db` | DSN 分支、`AutoMigrate` 建表/幂等/默认配置、`GetSetting` | 93% |
| `internal/pool` | 池/盘参数校验、chunk 写入-读取-覆写、预留槽位复用、write queue → stripe queue 搬运、RS parity 编码（与 `reedsolomon` 独立复算比对）、坏块重建与换盘恢复、读缓存落盘与冷却淘汰、`parallelEach` 并发上限 | 77% |
| `internal/vfs` | 路径规范化与 `checkName`、zstd 往返与压缩效果、AES-GCM 加解密与篡改检测、口令生命周期、**进程级密钥表（解锁后跨实例可读、上锁后读写均失败）**、回收站（列出/恢复/彻底删除/父目录已删时回到根）、ShareFS 命名空间/权限/配额/稀疏/版本提交/快照隔离、切片级写时复制、Truncate 对边界、`RootFS` 根聚合、**错误码标准化** | 81% |
| `internal/api` | 真实 HTTP 服务 + `httptest`：bootstrap/登录/JWT（含 401/403/404/409/423 映射）、用户与 Share 管理、文件上下传与回收站、加密 Share 的解锁/上锁、`?access_token=` 鉴权与日志脱敏、静态资源挂载与保留路径、建池/加盘/换盘/重建/chunk 读写 | 64% |
| `internal/auth` | bcrypt 密码、TOTP 二次验证、JWT 签发/过期/篡改/用户被删、用户 CRUD 与连带清理、密钥与 TTL 的配置读取 | 83% |
| `internal/otp` | RFC 6238 官方测试向量、±1 步长漂移、坏输入、密钥大小写/空格/填充容错、`otpauth://` URI | 94% |
| `internal/token` | token 生成与唯一性、只落单向摘要（BLAKE3 + NT hash）、过期判定、公钥注册与指纹、跨用户拒绝、SMB 候选摘要 | 87% |
| `internal/smb` | 端到端（真 SMB 客户端登录 + 读写落盘）、错误 token/过期 token 被拒、未授权 Share 拒绝、路径/错误码映射 | 46% |
| `internal/sftp` | 端到端（公钥与 token 口令两种登录）、文件往返与目录操作、只读 Share 拒绝、主机密钥持久化与复用 | 67% |
| `internal/webdav` | 认证（401/凭证无效）、GET/PUT/PROPFIND/MKCOL/MOVE/COPY/DELETE、LOCK/UNLOCK 恒成功、未授权 Share 404、路径穿越、只读 Share 写 403 | 77% |
| `internal/webui` | 静态资源与 SPA 回退、`index.html` 禁缓存、产物文件长缓存、路径穿越回退 | 73% |

**不测**：后台 worker 的定时/异步时序
（`StartParityWorker`、`StartCacheCleaner` 的 ticker 循环、并行调度的等待逻辑）——这类用例要等时钟、
容易 flaky。它们背后的同步内核（`calculateStripeParity`、`rebuildStripe`、`cleanupColdCache`、
`Flush`）都由上面的用例直接驱动，逻辑本身是覆盖到的。

## 目录结构

```
cmd/zenofs/        入口：配置 → DB/AutoMigrate → PoolManager → 后台 worker → HTTP
web/               前端源码（Vite + Preact + TypeScript）；构建产物输出到 internal/webui/dist
internal/api/      chi 路由与 JSON 响应
internal/auth/     用户与登录态：bcrypt 密码 + TOTP 二次验证 + JWT 签发与校验
internal/config/   配置加载（argv > ZENOFS_DSN > 默认值）
internal/db/       GORM 连接与全部数据模型
internal/errs/     统一错误码与 ZenoError
internal/hash/     摘要算法收口（BLAKE3-256；分片/校验分片/口令校验值都用它）
internal/otp/      TOTP 实现（RFC 6238：SHA-1 / 30 秒 / 6 位）与密钥生成
internal/pool/     核心：存储池 / 磁盘 / chunk / parity / 读缓存；Keyring = 加密 Share 的内存密钥表
internal/testutil/ 测试脚手架：临时 SQLite 库 + 本地盘存储池 + Share 行（只被 _test.go 引用）
internal/token/    访问凭证（access token）：生成 / 单向摘要 / 过期校验，SMB / SFTP / WebDAV 共用
internal/smb/      SMB2/3 服务端（jfjallid/go-smb）：共享注册 + NTLMv2 认证 + vfs 适配
internal/sftp/     SFTP 服务端（x/crypto/ssh + pkg/sftp）：公钥 / token 口令认证 + vfs 适配
internal/webdav/   WebDAV 服务端（x/net/webdav）：挂在 HTTP API 服务上（同一端口，默认 /dav）
internal/webui/    把前端产物 embed 进二进制并提供静态资源 / SPA 回退
internal/vfs/      文件系统层（SMB / WebDAV / SFTP 共用）：接口定义 + 基于 Share/Inode/Version 的实现
                   （ShareFS = 单 Share，RootFS = 把用户可见的多个 Share 挂在同一个根下）
```

## 数据库设计

模型全部在 `internal/db/models.go`，建表在 `db.go` 的 `AutoMigrate`——**新增模型必须登记到那里**，
否则运行时会报 `no such table`。

### Storage Layer

层级关系：`pools` 1─N `disks`、`pools` 1─N `stripes` 1─N `chunks`（每个 chunk 落在某块 disk 上）。
`write_queues` / `stripe_queues` 是写入与重建的任务队列，`read_caches` 记录分片在缓存盘上的副本。

| 表 | 用途 | 关键字段 |
|---|---|---|
| `pools` | RS 存储池 | `name`(唯一) `data_shards` `parity_shards` `chunk_size`(KB，同时是池内所有 Share 的文件切片大小) `status` |
| `disks` | 池内磁盘 | `path`(唯一) `pool_id` `backend` `type` `status`；索引 `(pool_id, status, type)` |
| `stripes` | RS 条带 | `pool_id` |
| `chunks` | 分片（data / parity 同表） | `status` `path` `size` `hash`(BLAKE3) `disk_id` `stripe_id` `pool_id` `type` `index` |
| `write_queues` | 写入意图与结果 | `chunk_id` `stripe_id` `status` |
| `stripe_queues` | 条带级任务队列（parity 计算 / 故障重建） | `stripe_id` `type` `task_id` `status` |
| `read_caches` | chunk 在缓存盘上的副本 | `chunk_id`(唯一) `path` `disk_id` `access_count` `status` |
| `settings` | 全局 KV | `name`(唯一) `value` |
| `tasks` | 后台作业（一次重建 = 一条） | `name`(形如 `rebuild-pool:<poolId>`) `status` `message` `metadata` |

### Share Layer

`users` `shares` `share_users` `inodes` `versions` `version_chunks` `inode_histories`

`users` 里除了用户名与 bcrypt 密码哈希，还存 TOTP 密钥（`otp_secret`，base32）：登录必须
**同时**通过密码与六位验证码。软删除的 `inodes`（`deleted = 1`）就是回收站的来源，
`inode_histories` 记录创建/改名/移动/删除/恢复事件。

`internal/vfs` 定义了 SMB / WebDAV / SFTP 共用的 `FileSystem` / `File` 接口，并给出两个实现：
`ShareFS`（一个实例挂一个 Share）与 `RootFS`（把用户可见的多个 Share 挂在同一个根下，
见下面"文件系统（vfs）"一节）。VFS 属性到模型的对应关系：

| VFS 属性 | 来源 |
|---|---|
| `Id` | `inodes.id` |
| `Uid` | `inodes.created_by`（属主用户） |
| `Gid` | `inodes.share_id`（所属 Share） |
| `Mtime` / `Atime` / `Ctime` | `inodes.updated_at` |
| `Mode` | `inodes.executable`（只区分是否可执行）；读写权限由 `share_users.permission` 在挂载层控制 |
| `Size` | `versions.size`（目录为 0） |
| `Target` | `inodes.link_id` 指向的目标 inode |

`StatFS` 的容量上限来自 `shares.quota`（单位 MB）：`TotalBytes = quota`，`UsedBytes` 为该 Share
下所有文件大小之和，`quota = 0` 表示不限制（此时容量以底层存储池为准）。

### Access Layer

`access_tokens` —— 协议访问凭证：由 `internal/token` 管理，SMB / SFTP（以及后续的 WebDAV）
共用同一张表。

| 表 | 用途 | 关键字段 |
|---|---|---|
| `access_tokens` | 一条协议访问凭证 | `user_id` `kind` `name` `token_hash` `nt_hash` `public_key` `fingerprint` `expires_at` `last_used_at` |

一行凭证要么是**随机 token**（`kind = TokenSecret`，可登所有协议），要么是 **SSH 公钥**
（`kind = TokenPublicKey`，只用于 SFTP）。token 明文只在创建时返回一次，落库的只有两份单向摘要：

- `token_hash`：BLAKE3-256(token)，用于"客户端把 token 原样交上来比对"的协议
  （SFTP 口令、WebDAV、HTTP API）；
- `nt_hash`：MD4(UTF-16LE(token))，即 MS-NLMP 的 NTOWFv1——SMB 的 NTLMv2 校验在数学上
  必须用它（HMAC-MD5 的密钥就是它），换不了别的摘要。

两者都不可逆，而 token 是 32 字节随机串，所以即便库被拖走也无法还原明文或离线爆破
（MD4 本身早已被攻破，这里是协议兼容要求，保密性来自 token 的熵）。公钥不是秘密，
明文存 `public_key`，另记 `fingerprint`（SHA256:base64）供查表与展示。
`expires_at`（Unix 秒，0 = 永不过期）对两种凭证都生效。

### 枚举

| 类型 | 取值 |
|---|---|
| `DiskPoolStatus` | `Online`(0) `Offline`(1) `Repair`(2) |
| `DiskBackend` | `LocalBackend`(0) `S3Backend`(1，预留) |
| `DiskType` | `DataDisk`(0) `CacheDisk`(1) |
| `ChunkStatus` | `ChunkReserved`(0 预分配未写入) `ChunkAllocated`(1 已写入) |
| `ChunkType` | `DataChunk`(0) `ParityChunk`(1) |
| `TaskStatus` | `TaskPending`(0) `TaskRunning`(1) `TaskSuccess`(2) `TaskFail`(3) |
| `StripeQueueType` | `StripeQueueParity`(0) `StripeQueueRebuild`(1) |
| `CacheStatus` | `NotCached`(0) `Cached`(1) |
| `AccessTokenKind` | `TokenSecret`(0，通用 token) `TokenPublicKey`(1，仅 SFTP 公钥) |
| `UserRole` | `UserNormal`(0) `UserAdmin`(1) |
| `SharePermission` | `ShareRead`(0) `ShareWrite`(1) `ShareAdmin`(2) |
| `InodeEventType` | `InodeCreated`(0) `InodeRenamed`(1) `InodeMoved`(2) `InodeDeleted`(3) `InodeRestored`(4) |

## 核心流程

### 存储池与磁盘

- `AddPool(name, chunkSizeKb)`：`chunk_size` 限 1~65536 KB，名称唯一。它同时决定
  池内所有 Share 的文件切片大小（写入时一个切片就是一个 chunk，所以切片必须 ≤ chunk 上限）。
- 加盘的 API 交互：先选 `type`（`"data"` = 参与条带化 / `"cache"` = 仅读缓存），只有 `data` 盘
  才谈得上 `add_parity`。三条服务端校验：**池里还没有数据分片时不许加 parity**（没有数据分片就
  没有东西可保护，报 `POOL_BAD`）、**池里还没有数据盘时不许加缓存盘**（缓存存的只是读副本，
  没有数据盘就没有东西可缓存，同样报 `POOL_BAD`）、**缓存盘带 `add_parity` 直接 400**
  （而不是悄悄忽略）。Web UI 也按这个来：选了缓存盘就不显示 parity 选项；池里还没有数据盘时
  缓存盘选项直接禁用。
- 加盘的路径规则：`path` **必须是非空的绝对路径**（相对路径会随进程工作目录漂移，空路径则会
  得到一块"看着加上了、其实不知道往哪写"的盘）→ `400 DISK_BAD_PATH`；**同一个目录只能当一块盘**，
  重复会撞 `disks.path` 唯一约束 → `409 DISK_EXIST`（而不是把底层的 UNIQUE 报错兜成 400）。
  选盘建条带 / 选缓存盘时都会跳过 `path` 为空的脏数据盘，避免把 chunk 写进进程工作目录。
- `DeleteCacheDisk(diskId)` / `DELETE /api/disks/{diskId}`：**删除缓存盘**（仅管理员），
  连同盘上的缓存文件与 `read_caches` 记录一起清掉，返回 `{"status":"deleted","removed_caches":N}`。
  只允许删缓存盘——数据盘（含 parity 位）是唯一数据副本，删掉会丢数据，那种情况走
  "下线 / 换盘 + 重建"。删除是安全的：读路径在缓存文件读不到时会回退到源盘，
  所以即使此刻正好有请求命中这块盘，也只是多一次回退。
- `AddDisk(poolId, path, backend, diskType, addParity)`：
  - `DataDisk`：事务内建盘并调整池配置——`addParity=false` 使 `DataShards++`，`true` 使 `ParityShards++`；
    随后为该池**已有**的每个 stripe 补一个 `ChunkReserved` 的 slot（新盘补齐条带位置）。
  - `CacheDisk`：只建记录，不参与条带化。
- 注意 `disks.type` **只区分"是否参与条带化"**（`DataDisk` / `CacheDisk`），它**不是**
  "数据盘 vs 校验盘"的角色：parity 盘在表里同样是 `DataDisk`。真正决定冗余度的是池的
  `data_shards` / `parity_shards`，而 `addParity=true` 的效果就是 `ParityShards+1`
  （为新盘在**已有**条带里补一个 parity 槽位）。
- 条带里的 data / parity 槽位在建**每个**条带时按打乱后的盘序分配（`getNewChunks`），
  所以同一块盘在不同条带里可能承担不同角色。也正因如此，Web UI 只在池级别展示
  Data / Parity 分片数，不给每块盘标"角色"。
- 关键约束：建新条带时要求**在线 DataDisk 数 == DataShards + ParityShards**，否则报 `DISK_OFFLINE`
  （见 `getNewChunks`）——即池内数据盘数量必须与 RS 参数严格一致。
- `OfflinePool` 改池状态；`SwapDisk` 把盘标记为 `Repair` 并换路径。

### 分片创建 —— `AddChunks` / `AddChunk`

1. **算 size / hash**（事务外）：BLAKE3。
2. **预分配** `getNewChunks`，单事务三 Phase：
   - Phase 1：按 `status = ChunkReserved AND type = DataChunk AND pool_id = ?` 取 `Limit N` 个槽位
     （`SKIP LOCKED`），优先复用；
   - Phase 2：不够则按 `numStripes = ceil(need / dataShards)` **一次性**建全部 stripe 与全部 slot（data + parity）；
     磁盘用 `crypto/rand` 种子 shuffle 均匀铺开；本次要写的 data chunk 直接置 `ChunkAllocated` 并填 size/hash，
     其余 slot 留 `ChunkReserved`；路径由 `generateChunkPaths` 批量生成（`YYYY/MM/DD/HH/MM/<random>`）；
   - Phase 3：Phase 1 复用的槽位批量 upsert `status/size/hash` 置 `ChunkAllocated`。
3. `enqueueWriteIntents`：批量写 `write_queues`（`TaskPending`，充当 WAL 写入意图）。
4. `writeChunkFiles`：`parallelEach` 并发写盘，逐下标返回错误。
5. `enqueueWriteResults`：批量写 `write_queues`（`TaskSuccess` / `TaskFail`）。

`WriteChunks`（覆写已有 chunk）走同样 5 步，第 2 步换成"批量更新元数据"，并在写之前先 `dropCaches`
清掉这批 chunk 的缓存。

### 分片读取 —— `ReadChunks`

1. 校验池在线；一次查出全部 chunk 元数据（校验属于该池）；按入参顺序重排。
2. 加载 disk map，`loadCaches` 一次查这批 chunk 的缓存记录。
3. `parallelEach` 并发读：`Status == Cached` → 读缓存盘文件（**读失败回退源盘**）；否则读源盘。
4. `updateCacheAccess` 批量维护缓存（阈值 `cachePromoteThreshold = 5`）：
   - 无记录 → 插入 `AccessCount=1` / `NotCached`；
   - 有记录、未落盘且 `AccessCount > 5` → 用 `generateChunkPaths` 写缓存文件，更新 `path` 并置 `status = Cached`；
   - 其余（含已 `Cached` 的命中）→ 一条 SQL 批量 `access_count + 1`。

### 条带 parity 计算 —— `Flush` → `calculateStripeParity`

写完数据后**需要调用 `Flush(chunkIds)`**（`chunkIds` 为空表示全库），parity 才会被计算。

**调用点**：`persistChunkData`（`AddChunks` / `WriteChunks` 共用）在写盘结果落库**之后**调一次
`Flush(ids)`——必须在结果落库之后，因为 `Flush` 靠 `Pending → Success/Fail` 的记录对判定"一轮写完"。
这样 HTTP API / SMB / SFTP / WebDAV 四条写入路径都会即时生成校验块。

服务启动时会跑一次 `CleanupWriteQueueOnStartup`：先全库 `Flush` 一遍（上次运行写完但还没搬的记录
重新进 `stripe_queues`，该算的校验块不会因为一次重启就漏掉），再把剩下的记录清掉——那些都是上次
运行半途中断的孤片（写盘意图记下了、结果没来得及记），对应的 chunk 仍是 `Reserved` 槽位，会被后续
写入当作可复用的位置，所以丢掉等价于"那次写入没提交"。

漏掉这一步的后果值得记一笔：`Flush` 是项目里**唯一**往 `stripe_queues` 投递的入口，
少了它 `stripe_queues` 永远是空的，校验块永远停在 `Reserved` 空占位（校验盘上只有一个预分配的
空文件）——冗余保护看着有、其实完全没生效。

1. `Flush`（单事务）：
   - 取 `write_queues`（按 `chunk_id, id` 排序），找出**相邻**的 `Pending → Success/Fail` 对，即"一轮写完"；
     同一 chunk 多轮写入会收集全部匹配对；
   - 其中 `TaskFail` 的 chunk，交给 `recalcChunksFromDisk` 按磁盘实际内容重算 `size` / `hash` 并批量 upsert
     （读不到文件则跳过、仅记日志）；
   - 按 stripe **去重**写入 `stripe_queues`（`StripeQueueParity` / `TaskPending`，不带 `TaskId`）；
   - 删除配对的那两条 `write_queues` 记录。
2. `StartParityWorker(ctx)` 后台轮询（1s 起，空闲退避至 5s）调用 `calculateStripeParity`：
   - 残留 `Running` 重置为 `Pending`；事务内 `SKIP LOCKED` 领走 `Pending` 的 Parity 任务并置 `Running`；
   - 按 stripe 去重，加载 stripe / pool / disk / chunk 元数据；
   - 组 job：**非 `ChunkReserved` 的 data chunk 才参与编码**（`Reserved` 表示预分配但未写数据）；
   - `parallelEach` 并发 `computeStripe`，并发上限 `maxParityConcurrency = 4`；
   - `computeStripe`：并发读 data chunk → padding 到等长 → `reedsolomon.Encode` → 并发写 parity shard 并算 BLAKE3；
   - 事务内批量回写 parity chunk 的 `size` / `hash`，并删除本批 `stripe_queues` 记录。

> ⚠️ `Flush` 目前**没有任何调用点**：写完数据后 parity 不会自动计算，必须显式调用。

### 故障重建 —— `RebuildPool` → `rebuildStripe`

1. `SwapDisk(diskId, newPath)`：盘置 `Repair` + 换路径，**并把所属 pool 置 `Offline`**
   （换盘后条带数据不完整，重建完成前不该对外服务）。
2. `RebuildPool(poolId)`（HTTP：`POST /api/pools/{id}/rebuild`，返回 `{"queued": N}`），单事务内：
   - 该 pool 已有进行中的重建作业（`tasks.name = rebuild-pool:<poolId>` 且状态为 Pending/Running）时
     直接返回 0 —— **作业级去重**；
   - 取该 pool 下 `status = Repair` 的盘，再取这些盘上**非 `ChunkReserved`** 的分片所属 stripe（去重）；
   - 建一条 `Task` 作为作业，再分批（每批 `rebuildBatchSize = 500`）写入带 `TaskId` 的
     `stripe_queues`（`StripeQueueRebuild` / `Pending`）。
3. `StartParityWorker` 每轮先 `rebuildStripe()` 再 `calculateStripeParity()`（重建优先）：
   - 领取/元数据逻辑与 parity 共用（`claimStripeTasks` / `loadStripeMeta` / `buildStripeJobs`，按 `type` 隔离）；
   - `rebuildStripeJob`：**data 与 parity shard 一起**放进 RS 解码器，缺失的置 `nil` 后 `Reconstruct` 一次恢复；
     缺失判定 = 读不出文件 **或** 内容 BLAKE3 与 `chunks.hash` 不符；缺失数 > parity 数则跳过；
   - 回写：data chunk 按 `chunks.size` 截断去掉 RS padding，parity 满长原样写；已有且校验通过的分片不重写；
     回写后按实际内容批量 upsert `size` / `hash` / `status`。
4. 作业收尾（`finishRebuildTasks`）——某个 `Task` 的子任务全部处理完后：
   - `recoverAfterRebuild` 逐盘校验（`diskDataComplete`：盘上所有非 `Reserved` 的 chunk 都读得出且 BLAKE3 一致）；
   - 校验通过 → 盘回 `Online`，该 pool 下所有盘都 `Online` 时 pool 也回 `Online`；
   - 仍有盘没修好 → 作业记 `TaskFail`，否则 `TaskSuccess`。

> 完成判定是**数据驱动**的：只有磁盘上真的每个 chunk 都读得出且校验一致才算修好，
> 光看队列空了会误判（某个条带可能因缺失过多而重建失败，那条记录同样会被消费掉）。

### 文件系统（vfs）

`internal/vfs` 把每个 Share 挂载成一个 `FileSystem`，一个实例绑定"一个用户 + 一个 Share"。

#### 多 Share 聚合（`RootFS`）

SMB / WebDAV / SFTP 这类协议有统一的根目录：客户端连上来要先回答"这个用户有哪些 Share"，
`RootFS` 就是这一层：根下每个 Share 一个一级目录（目录名 = `shares.name`），
路径第一段决定落到哪个 `ShareFS`，`/` 与 `/<share>` 是合成条目，其余部分原样转发。

- **可见性**：只列 `share_users` 里有该用户记录的 Share，权限取该记录的 `permission`。
  每次解析路径都重新确认授权，所以会话期间回收授权立刻生效；别人的 Share、
  以及不能作为目录名的 `shares.name`（含 '/'、NUL 或超过 255 字节）都不会出现在根下
  （后者记一条日志，因为它没有可寻址的路径）。
- **只读视图**：根与挂载点根都不是真实的 inode，这两层上的写操作
  （`Mkdir` / `Remove` / `Rename` / `SetAttr` / `Symlink` / 带 `Create` 的 `Open`）
  一律返回 `ErrNotSupported`，跨 Share 的 `Rename` / `Copy` 返回 `ErrCrossDevice`。
  Share 的增删改属于授权管理，不走文件系统接口。`Copy` 的目标必须不存在（沿用 `ShareFS` 语义）。
- **条目编号**：根下 Share 目录的 `FileInfo.Id` 取 `-shares.id`。`shares` 与 `inodes`
  两张表各自自增，取负数才能保证同一个挂载点内编号唯一（协议层可拿它当 FileId）。
- **符号链接**：`Target` 与 `Readlink` 的返回值都带挂载点前缀（如 `/work/a/b`）；
  链接只能指向同一个 Share 内已存在的路径（`ShareFS` 用 inode 引用表达链接），
  跨 Share 或指向根的目标返回 `ErrNotSupported` / `ErrCrossDevice`。
- **加密口令**：启用加密的 Share 在根目录里照旧可见（元数据不需要密钥），
  但文件读写返回 `ErrEncrypted`，要用 `RootFS.UsePassword(name, password)` 提供口令；
  密钥缓存在 `RootFS` 里，因此权限/配额等元数据变化触发挂载实例重建时不会丢密钥。
- **配额**：`StatFS("/")` 只汇总该用户所有可见 Share 的已用量（根没有统一配额），
  Share 内的路径转发给 `ShareFS`（上限来自该 Share 的 `quota`）。
- 另外提供 `Share(name)`（取 Share 记录与权限）和 `Mount(name)`（直接拿某个 Share 的
  `*ShareFS`），供协议层绕开路径解析使用。

#### 单 Share（`ShareFS`）

- **路径**：绝对路径、'/' 分隔、纯词法规范化；`ParentId IS NULL` 的 inode 就是 Share 根下的一级条目，
  根目录本身没有 inode 记录（用 Id 0 合成）。
- **读**：以"句柄打开时的那个版本"为准（**快照语义**）。按 `VersionChunk.Idx` 懒加载切片，
  缺少某个 Idx 就是稀疏空洞，读作零。读失败或校验不过会顺手为该条带投递重建任务
  （`PoolManager.RebuildByChunks`）。
- **写**：**切片级写时复制**。`idx = offset / <切片大小>`，只重建被写到的切片；
  切片大小**取自所属池的 `chunk_size`**（Share 上不再单独配置——见 `vfs.sliceSize`），
  并从中扣掉编码余量（压缩膨胀 + 加密的 nonce/tag，见 `sliceOverheadFor`）。原因：写进池的
  是编码**之后**的字节，而池会拒绝超过 chunk 上限的块——不留这点余量，"文件大小正好是切片
  整数倍"时的满切片会被 `CHUNK_SIZE_EXCEED` 拒收。未压缩未加密时开销为 0，切片精确等于
  池的 `chunk_size`。
  新版本会先继承旧版本的全部切片映射，未触及的 Idx 直接沿用旧 chunk（chunk 不可变且不回收，
  多版本共享是安全的）。新切片实时写入存储池并 upsert `VersionChunk`；
  同一 Idx 反复写时只在内存里改一份明文缓冲，切到别的 Idx 或 Close/Sync 时才落盘。
- **提交**：`Version.Size` 与 `Inode.VersionId` 在 Close 时一次性切换；打开后没写过任何字节
  则丢弃那个空版本。`SetAttr` 的 Size 走同一条 Truncate 路径。
- **稀疏**：Truncate 扩大不写数据，中间留空洞；全零切片等价于空洞，不占存储。
- **截断**：Truncate 缩小会丢弃截断点之后的整片，并把截断点所在的切片裁到截断点
  （`trimSlice` 走一次切片级 read-modify-write）。被截掉的数据真的从存储层消失，
  之后再扩大文件只会读到零——注意旧版本仍引用原 chunk，所以历史版本照旧可读。
- **配额**：写路径按「该 Share 所有 inode 当前版本大小之和」判断，超限返回 `ErrNoSpace`。
- **属性映射**：见上面的对照表；`SetAttr` 的 Uid/Gid 被忽略，Mode 只影响可执行位。

**压缩与加密按切片做**（每片自带完整的压缩流与密文），所以随机读、部分写都不受影响：

- 压缩：`Share.Compression = 1` 用 Zstandard（`klauspost/compress/zstd`，每片一条独立的 zstd 帧，
  默认档位 `SpeedDefault`，帧头只有十几字节，相对 MB 级切片可忽略）；
- 加密：`Share.Encryption = 1` 用 AES-256-GCM，密文格式 `nonce(12B) || ciphertext+tag`，每片随机 nonce，
  GCM 自带认证——密文被篡改时解密直接失败，会触发该条带的重建；
- 密钥：由用户口令经 **PBKDF2-HMAC-SHA256（600k 次迭代）**派生，**只存在于会话内存**（`ShareFS.UsePassword`
  提供口令、`ClearKey` 清除）。落库的只有 `shares.encryption_key_hash`，布局是 `salt(16B) || blake3(密钥)(32B)`：
  前者供派生使用，后者用来校验口令是否正确。首次启用加密用 `SetPassword` 生成 salt；
- 算法记在 **`versions.compression` / `versions.encryption`** 上，因此以后改 Share 的算法不会让历史数据解不开；
  例外：`compression = 1` 曾经表示 DEFLATE，现已改为 Zstandard——老库里按 flate 写下的切片解不开
  （需要重写数据；本地开发库无此类数据）；
- 加密的 Share 未提供口令时，`Open` 直接返回 `ErrEncrypted`（`Stat`/`ReadDir` 等元数据操作不受影响）。
  这个错误说的是"该 Share 是加密的、当前会话没有密钥"，与 `Locker` 的"锁被占用"（`ErrBusy`）是两回事。

> 尚未做：修改口令 / 密钥轮换、派生参数的版本化（迭代次数写死，将来调整需要兼容策略）。

### 读缓存

缓存盘是 `CacheDisk`，缓存文件路径由 `generateChunkPaths` 生成随机路径（不按 chunkId 推导）。
`StartCacheCleaner` 每 60s 调 `cleanupColdCache`：`updated_at` 早于 `cacheIdleTTL = 1h` 的条目被淘汰，
其中 `Cached` 的条目连同缓存文件一起删除。

### 并发工具

```go
// internal/pool/parallel.go
func parallelEach(n, limit int, fn func(i int) error) []error
func firstErr(errs []error) error
```

对下标 `[0, n)` 并发执行，`limit > 0` 时用缓冲 channel 做信号量（`<= 0` 表示不限）。
它**保证所有任务执行完**（不像 errgroup 那样 fail-fast），因为调用方常需要知道每个下标的成败。
返回值与 `n` 等长，第 `i` 项即 `fn(i)` 的错误；`fn` 只应写自己下标的槽位。

## Web 前端

轻量 Web UI：**英语为默认语言，可切中文**；首次运行自动进入引导流程
（创建管理员 → 建存储池 → 加磁盘 → 建共享），之后是文件管理与管理界面。

- **技术栈**：Vite + Preact + TypeScript + signals，手写 CSS，无第三方 UI 库。
  构建产物（约 70 KB JS / 5 KB CSS，gzip 后约 22 KB）用 `//go:embed` 打进二进制，
  部署仍然只有一个文件。
- **挂载位置**：与 REST API、WebDAV **共用同一个 HTTP 服务**（同一个端口）。分发规则：
  `/api/*` → REST API，`/dav/*`（或配置的 WebDAV 前缀）→ WebDAV，其余 → 前端静态资源
  （未知路径回 `index.html`，前端自己用 hash 路由）。关掉 WebDAV 后 `/dav/` 会明确回 404，
  而不是让浏览器拿到一张 HTML。
- **开关**：`settings.WEBUI_ENABLED=0` 关掉前端（接口-only 部署）。

| 页面 | 内容 |
|---|---|
| 引导向导 | 四步：首个管理员 → 存储池 → 磁盘 → 共享。OTP 密钥**进页面就在浏览器里生成好**（带二维码、可点“换一个密钥”重新生成，密钥不经过任何 URL），第 1 步建完管理员会自动用它算码登录 |
| 登录 | 用户名 + 密码 + 六位验证码（TOTP） |
| 文件 | 共享切换、面包屑、目录列表、拖拽或选择上传、下载、新建文件夹、改名、删除（进回收站）、配额用量条、加密共享的解锁/取消解密 |
| 回收站 | 列出、恢复、彻底删除、清空 |
| 管理 · 共享 | 新建（可选口令启用加密）、改配额/压缩、授权（read/write/admin）、解锁/取消解密、强制删除 |
| 管理 · 用户 | 新建（角色 + OTP）、改密码/角色、重置 OTP（新密钥只显示一次）、删除 |
| 文件 · 历史 | 文件/目录的版本列表与变更记录；恢复到任一版本（只读共享禁用） |
| 回收站 · 历史 | 被删条目同样能查看版本与变更记录，再决定还原或彻底删除 |
| 管理 · 存储池 | 建池、加盘（先选用途：数据盘 / 缓存盘；数据盘且池里已有盘时才出现“计入校验分片”，池里没有数据盘时缓存盘不可选）、下线、换盘、重建（reconstruct）、删除缓存盘、后台任务列表 |
| 管理 · 凭证 | 生成访问 token（明文只显示一次）、注册 SFTP 公钥、吊销 |

```bash
cd web
npm ci            # 安装依赖（node_modules 不进仓库）
npm run dev       # 开发模式：vite 起 5173，/api 代理到 127.0.0.1:8080
npm run build     # 类型检查 + 构建，产物写到 internal/webui/dist（跟着仓库提交）
npm test          # 纯函数测试（前端算的 TOTP 必须与服务端一致，用 RFC 6238 向量校验）
```

**下载与 token**：浏览器用 `<a href>` 下载时发不出 `Authorization` 头，所以前端的下载直链把 JWT
放在 `?access_token=` 里；后端最外层会先把 token 从 URL 摘出来放进 context、再记访问日志
（`internal/api` 的 `stripAccessToken`），**日志里不会留下 token**。

## HTTP API

所有端点都在 `/api` 下（WebDAV 挂在 `/dav`，见下一节）。**除 bootstrap 与登录之外，
每个请求都要带 `Authorization: Bearer <JWT>`**；缺 token 或 token 失效回 401。

**关于超时**：HTTP 服务**不设** `ReadTimeout` / `WriteTimeout`——上传下载（包括 WebDAV）可能
持续几分钟到几十分钟，按固定时间掐断会把传输弄坏（典型表现是"上传到第 10 秒整失败、回 400"）。
抗慢速连接靠 `ReadHeaderTimeout`（10 秒，只管请求头）与 `IdleTimeout`（120 秒，回收闲置连接）；
要限制单次传输的时长或带宽，请在反向代理层做。上传中途断开（客户端掉线、连接被中断）时，
服务端会把**已写入的半成品删掉**，不会在目录里留下一个看着正常、其实不完整的文件。

### 认证与用户

- **首个用户**：`users` 表为空时 `POST /api/auth/bootstrap` 免认证创建，并且固定是管理员。
- **二次验证**：TOTP（RFC 6238，SHA-1 / 30 秒 / 6 位），密钥是 base32 文本。创建/重置用户时
  要么「给密钥 + 给当前六位验证码」（服务端校验码对不对，防止密钥录错），要么两个都不给——
  服务端生成密钥并在响应里返回一次（`otp_secret` + 可直接扫码的 `otp_uri`）。
- **登录**：`POST /api/auth/login` 提交用户名 + 密码 + 六位验证码，返回 JWT。
  有效期默认 24 小时（`settings.JWT_TTL`，单位秒），签名密钥是 `settings.JWT_SECRET`
  （首次启动自动生成 32 字节随机值；删掉它会让所有已签发的 token 失效）。
- 密码用 bcrypt 存哈希，JWT 用 HS256；用户名不存在与密码错误返回同一个错误（不泄露用户是否存在）。

| 端点 | 说明 |
|---|---|
| `POST /api/auth/bootstrap` | 创建第一个管理员（免认证，仅在还没有用户时可用） |
| `POST /api/auth/login` | 用户名 + 密码 + 六位验证码 → JWT |
| `GET /api/auth/me` | 当前登录用户 |
| `GET /api/users` | 列出用户（管理员） |
| `POST /api/users` | 创建用户（管理员） |
| `GET /api/users/{id}` | 查询用户（管理员或本人） |
| `PUT /api/users/{id}` | 改密码 / 角色 / 重置 OTP |
| `DELETE /api/users/{id}` | 删除用户（管理员；同时清掉授权与访问凭证） |

### Share、文件与回收站

| 端点 | 说明 |
|---|---|
| `GET /api/shares` | 列出可见的 Share（管理员看全部，普通用户只看有授权的） |
| `POST /api/shares` | 创建 Share（管理员） |
| `GET`/`PUT`/`DELETE` `/api/shares/{id}` | 查询 / 改配额·压缩 / 删除（非空需要 `?force=1`） |
| `GET`/`POST` `/api/shares/{id}/users` | 列出 / 设置授权（管理员） |
| `DELETE /api/shares/{id}/users/{userId}` | 撤销授权（管理员） |
| `GET /api/shares/{id}/list?path=/dir` | 列目录 |
| `GET /api/shares/{id}/stat?path=/a.txt` | 元数据 |
| `GET /api/shares/{id}/files?path=/a.txt` | 下载（支持 Range） |
| `PUT /api/shares/{id}/files?path=/a.txt` | 上传（body 就是文件内容） |
| `DELETE /api/shares/{id}/files?path=/a.txt` | 删除（软删除，进回收站） |
| `POST /api/shares/{id}/folders` | 新建目录 `{"path":"/dir"}` |
| `POST /api/shares/{id}/rename` | 改名 / 移动 `{"from":"/a","to":"/b"}` |
| `GET /api/shares/{id}/recycle` | 回收站列表（含删除时间与删除者） |
| `POST /api/shares/{id}/recycle/{inodeId}/restore` | 恢复（原父目录没了就回到 Share 根；同名冲突回 409） |
| `DELETE /api/shares/{id}/recycle/{inodeId}` | 彻底删除（清掉元数据；存储层 chunk 留给后续 GC） |
| `DELETE /api/shares/{id}/recycle` | 清空回收站 |

**权限**：能不能读写 Share 只看 `share_users` 里的授权，管理员也不例外——
`read` 只能读、`write` 能读写、`admin` 还能改授权；没有授权的 Share 一律按"不存在"处理（404），
不泄露 Share 名。

### 加密 Share（LUKS 式解锁）

加密 Share 的**密钥只存在进程内存里**（`pool.Keyring`，永不落库）：落库的只有"启用了加密"和
口令校验值（salt + `BLAKE3(密钥)`）。跟 LUKS 一样，进程重启后必须重新 unlock。

| 操作 | 端点 | 说明 |
|---|---|---|
| 初始化 + 解锁 | `POST /api/shares` 带 `password` | 只能在**新建的空 Share** 上做（等价 `luksFormat`），建完即解锁 |
| 解密（打开） | `POST /api/shares/{id}/unlock` `{"password":"..."}` | 校验口令后把密钥放进内存；此后**所有协议**（HTTP / WebDAV / SMB / SFTP）都能访问它 |
| 取消解密 | `POST /api/shares/{id}/lock` | 丢弃内存里的密钥（字节清零），所有读写立刻失败 |
| 查看状态 | `GET /api/shares/{id}` 的 `encrypted` / `unlocked` | `unlocked` 就是"密钥此刻在不在内存里" |

- 未解锁（或已上锁）时访问加密 Share，API 返回 **423 Locked**，客户端据此提示先解锁。
- 判断"能不能写"请用 `encrypted && !unlocked`：**未加密的 Share 同样返回 `unlocked: false`**
  （它没有密钥），只看 `unlocked` 会把普通 Share 误判成锁住。
- 只能给**新建的空 Share** 加密：对已有数据的 Share 事后加密会毁数据（老 chunk 是明文，
  读的时候却按密文解），所以除了 `POST /api/shares` 没有别的"启用加密"入口。
- unlock / lock 仅管理员可用（普通用户即使拿到该 Share 的 admin 授权也不行）；
  口令错返回 403，对未加密的 Share 调 unlock 返回 400。

### 存储池与磁盘

| 端点 | 说明 |
|---|---|
| `GET`/`POST` `/api/pools` | 列出 / 创建存储池 |
| `GET /api/pools/{id}` | 查询存储池 |
| `PUT /api/pools/{id}/offline` | 下线（暂停所有操作） |
| `GET /api/pools/{id}/disks` | 列出池里的磁盘 |
| `POST /api/pools/{poolId}/disks` | 加盘：`type` 选 `data` / `cache`，`data` 盘可带 `add_parity`；`path` 必须是没被占用的绝对路径 |
| `PUT /api/disks/{diskId}/swap` | 换盘（转 Repair 并投递重建） |
| `DELETE /api/disks/{diskId}` | 删除缓存盘（连同缓存文件与记录；管理员） |
| `GET /api/shares/{id}/inodes/{inodeId}/versions` | 文件版本列表（新的在前，标出当前版本） |
| `GET /api/shares/{id}/inodes/{inodeId}/history` | 元数据变更记录（创建/改名/移动/删除/恢复） |
| `POST /api/shares/{id}/versions/{versionId}/restore` | 恢复到某个版本（需写权限） |
| `POST /api/pools/{id}/rebuild` | 投递条带重建（reconstruct；`/reconstruct` 是同义别名） |
| `POST /api/pools/{poolId}/chunks`、`GET`/`PUT` `/api/chunks/{id}` | 分片级读写（存储层内部用） |

### 访问凭证（SMB / SFTP / WebDAV 用）

| 端点 | 说明 |
|---|---|
| `POST /api/users/{id}/tokens` | 生成访问 token（明文只返回一次） |
| `GET /api/users/{id}/tokens?kind=` | 列出凭证 |
| `POST /api/users/{id}/pubkeys` | 注册 SFTP 公钥 |
| `DELETE /api/tokens/{id}` | 吊销凭证 |

```bash
# 1) 首次启动后创建管理员：连 otp_secret 都不给，服务端生成并返回一次
curl -X POST localhost:8080/api/auth/bootstrap \
  -d '{"username":"root","password":"root-pw-123"}'
# => {"id":1,"username":"root","role":"admin","otp_secret":"GEZD...","otp_uri":"otpauth://totp/..."}
# 把 otp_secret 录进验证器 App，用它算出的六位码登录：
TOKEN=$(curl -s -X POST localhost:8080/api/auth/login \
  -d '{"username":"root","password":"root-pw-123","otp_code":"123456"}' | jq -r .token)
# 2) 建 Share（先要有 pool）并上传 / 下载
curl -X POST localhost:8080/api/shares -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"docs","pool_id":1}'
curl -X PUT --data-binary @f.bin -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/shares/1/files?path=/f.bin'
curl -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/shares/1/files?path=/f.bin' -o out.bin
# 3) 回收站
curl -H "Authorization: Bearer $TOKEN" localhost:8080/api/shares/1/recycle
```

## 文件协议（SMB / SFTP / WebDAV）

三个协议服务都接 `internal/token` 的 access token 认证、以 `internal/vfs` 为文件系统；
真正决定能读写什么的是 `share_users` 里的授权，而不是协议层的共享权限。

加密 Share 只要已经被管理员解锁（密钥在进程内存里），这三个协议同样能读写它——
密钥来自 `pool.Keyring`，不需要各自再传口令。

其中 **SMB 与 SFTP 各自监听一个端口**（见下），**WebDAV 不额外占端口**：它挂在 HTTP API
服务上（同一个端口、默认 `/dav` 前缀），见下面 WebDAV 一节。

### 启用与配置

SMB 的端口存在 `settings` 表里，**为空表示不启用**——445 是特权端口，默认关掉才能让普通用户
直接 `go run ./cmd/zenofs` 跑起来；SFTP 默认就监听 **2222**（非特权端口，不用 root），
WebDAV 默认挂在 HTTP 服务上：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `SMB_PORT` | 空（不启用） | 例如 `1445`；标准端口 `445` 需要 root |
| `SMB_NETBIOS_NAME` | `ZENOFS` | NTLM TargetInfo 里对客户端可见的服务端名 |
| `SFTP_PORT` | `2222` | 默认监听 2222（非特权端口，不用 root）；设为 `off` / `-` / `0` 关闭，改成 `22` 则需要 root |
| `SFTP_HOST_KEY_FILE` | 空 | 外部 SSH 主机私钥（PEM）；为空时用自动生成并持久化在 `settings.SFTP_HOST_KEY` 的 ed25519 密钥 |
| `WEBDAV_PREFIX` | `/dav` | WebDAV 挂在 HTTP API 服务上的前缀；`off` 或 `-` 表示关闭 |

### SMB

- 每个 zenofs Share 注册成一个同名 SMB 共享：`\\<host>\<share>`。共享注册是**启动时快照**，
  之后新建的 Share 要重启服务才可见。
- 认证：客户端把"zenofs 用户名 + access token"当账号密码用（NTLMv2）。服务端按用户名取出该用户
  所有未过期 token 的 NT hash 逐个试算 proof（`internal/smb/auth.go`），过期 token 直接拒绝；
  默认要求 SMB 签名并支持 SMB 3.x 传输加密（`Options.DisableSigning` / `DisableEncryption` 可关）。
- 请求映射在 `internal/smb/vfs.go`：CREATE 的 disposition/options 翻成 `vfs.Open` / `vfs.Mkdir`，
  READ/WRITE 走 `File.ReadAt` / `WriteAt`，SET_INFO 处理截断、删除标记、时间戳与改名；
  vfs 的 POSIX 错误映射成 NTSTATUS。
- 挂载鉴权：每个请求按会话登录名查出用户，再经 `RootFS.Mount(share)` 确认该用户对这个 Share 有授权
  ——没授权的共享连得上，但一操作就是 `STATUS_ACCESS_DENIED`。

### SFTP

- **默认监听 2222**（`sftp.DefaultPort`）：非特权端口，`go run ./cmd/zenofs` 直接就能用；
  想收回就设 `SFTP_PORT=off`。
- 登录后的根目录就是该用户可见的 Share 列表：`/<share>/...`（`vfs.RootFS`）。
- 两种认证都支持：SSH 公钥（按 `fingerprint` 匹配）与"用户名 + token"口令。
- 打开 / 读写 / 列目录 / 改名 / 删除 / mkdir / statvfs 都翻译成 vfs 调用
  （`internal/sftp/handlers.go`），vfs 错误翻成 `syscall.Errno`，由 pkg/sftp 转成客户端的 `SSH_FX_*`。

```bash
sftp -P 2222 alice@localhost   # 口令填 access token
```

### WebDAV

- **与 HTTP API 同一个端口**：`api.NewRouter` 最外层按前缀分发，`/dav/...` 交给 WebDAV、
  其余交给 chi 的 `/api`。之所以不用 `chi.Mount`：WebDAV 的方法集（`PROPFIND`/`PROPPATCH`/
  `COPY`/`MOVE`/`LOCK`/`UNLOCK`）不在 chi 预置的方法里，直接挂会被判成 405。
- **认证**：HTTP Basic，用户名 = zenofs 用户名、密码 = access token。未认证或凭证无效统一回
  401 + `WWW-Authenticate`（不区分"用户不存在"与"token 不对"）；客户端看到 401 会带凭证重试。
- **视图**：`/dav/<share>/...`，该用户可见的 Share 列表就是根（`vfs.RootFS`）。
- **写权限**：进入 WebDAV 处理器之前先按 Share 授权判一次，只读 Share 上的
  PUT/MKCOL/DELETE/MOVE 直接回 403——x/net 会把 `OpenFile` 的任何错误都当 404，
  不预判的话客户端只会看到误导性的"资源不存在"。
- **锁**：`LOCK`/`UNLOCK` 一律成功（`internal/webdav/lock.go`）。vfs 不提供锁（设计如此），
  服务端不做互斥，只回一个合法的 `opaquelocktoken`，让 Office / Finder 这类要求 LOCK 成功的
  客户端能正常写入；`LockSystem.Confirm` 放行所有条件（包括 `If` 头里的 token）。
- `ETag` 由 inode id + 修改时间合成，`Content-Type` 按扩展名给，两者都通过 x/net/webdav 的
  可选接口暴露，避免服务端为了猜类型去读文件内容。

```bash
# 根目录 = 该用户可见的 Share 列表
curl -u alice:$TOKEN -X PROPFIND -H 'Depth: 1' http://localhost:8080/dav/
# 上传 / 下载
curl -u alice:$TOKEN -T ./file.bin http://localhost:8080/dav/docs/file.bin
curl -u alice:$TOKEN -O http://localhost:8080/dav/docs/file.bin
```

macOS Finder 里"连接服务器"填 `http://<host>:8080/dav`，账号是 zenofs 用户名，密码是 token。

### 凭证管理 API

生成 / 列出 / 吊销访问凭证用 HTTP API 里那组端点（`POST /api/users/{id}/tokens` 等，
见上面「访问凭证」一节）——它们同样要 Bearer JWT。token 明文只在创建响应里返回一次，
`expires_at` 是 Unix 秒（`0` 或省略表示永不过期）。

## 约定

- 错误一律用 `internal/errs` 的 `ZenoError`（数值 `Code` + 字符串 `StrCode` + `InnerErr`）；
  API 层按错误码选 HTTP 状态（401 认证 / 403 权限 / 404 不存在 / 409 冲突，其余 400），
  响应体是结构化 JSON，其它 error 映射为 500。
- **API 鉴权**：除 `/api/auth/bootstrap`、`/api/auth/login` 与 `/dav`（WebDAV 走自己的
  Basic token）之外，所有端点都要 `Authorization: Bearer <JWT>`；文件与 Share 的读写一律以
  `share_users` 授权为准（管理员不例外），没授权的 Share 按不存在处理（404）。
- **凭证只存单向摘要**：token 明文只出现在创建响应里一次；`access_tokens.token_hash`（BLAKE3）
  与 `nt_hash`（MD4/UTF-16LE，NTLM 必需）都不可逆，任何接口都不回传这两个字段。
- `internal/vfs` 的 POSIX 哨兵错误（`ErrNotExist` / `ErrPermission` / `ErrCrossDevice` …）本身就是
  `*errs.ZenoError`，码是 `errs.ECODE_VFS_*`：协议层既能 `errors.Is` 判定后映射成自己的错误码
  （SFTP status / NTSTATUS），也能直接把 `Code` / `StrCode` 回给客户端。`ZenoError.Unwrap` 暴露
  `InnerErr`，因此被 `fmt.Errorf("%w")` 包装后依然能 `errors.Is` / `errors.As` 穿透。
  新增这类错误时必须同时给出数值码与字符串码——`errs_test.go` 会校验码不重复。
- **命名约定（两件不同的事，别混用）**：
  `Lock` / `Unlock` 专指"锁定文件"——WebDAV 的 LOCK、SMB 的字节范围锁，见 `Locker` 接口；
  加密会话的状态用另一组词：`UsePassword`（用口令派生并持有密钥）、`ClearKey`（清除密钥）、
  `HasKey`（当前是否持有密钥）。相应地，`ErrEncrypted` 说的是 Share 的加密属性（会话里没有密钥），
  `ErrBusy` 才是锁被占用。
- **DB 操作批量进行**：一次 `IN` 查询、一条 `UPDATE ... WHERE id IN (...)`、一次 upsert，
  不要写逐条往返的循环；一批操作尽量包在一个 `Transaction` 里。
- 文件读写统一走 `ChunkHandler` 接口（`Write` / `Read` / `Delete`，见 `internal/pool/handler.go`），
  按 `disk.Backend` 匹配实现，不要在业务层直接调 `os`。
- 注释用中文，日志用英文（`log.Printf("parity: ... failed: %v", err)`）。
- 新增模型必须登记到 `AutoMigrate`。

## 验证

提交前至少跑一遍（细节见上面的"测试"一节）：

```bash
go build ./... && go vet ./... && gofmt -l ./cmd ./internal
go test ./... && go test -race ./...
```

新增或修改功能时，在对应包的 `*_test.go` 里补用例，不要临时写脚本验证完再删：
用例统一用 `internal/testutil` 建临时库与盘，跑完自动清理，仓库里不留任何临时文件。
