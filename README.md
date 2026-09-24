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
```

DSN 前缀决定驱动（`sqlite://` / `mysql://` / `postgres://`）。HTTP 端口取自 `settings` 表的
`HTTP_PORT`（默认 8080）。

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
| `internal/vfs` | 路径规范化与 `checkName`、zstd 往返与压缩效果、AES-GCM 加解密与篡改检测、口令生命周期、ShareFS 命名空间/权限/配额/稀疏/版本提交/快照隔离、切片级写时复制、Truncate 的截断裁剪与对齐边界、`RootFS` 根聚合（可见性、只读视图、跨 Share、密钥保留）、**错误码标准化**（每个哨兵都带 `Code`/`StrCode`，并从真实操作取码校验） | 82% |
| `internal/api` | 真实 chi 路由 + `httptest`：建池/查池/下线/加盘/换盘/重建/chunk 上传-下载-覆写，以及各类 400 | 80% |

**不测**：`cmd/zenofs`（进程装配）与后台 worker 的定时/异步时序
（`StartParityWorker`、`StartCacheCleaner` 的 ticker 循环、并行调度的等待逻辑）——这类用例要等时钟、
容易 flaky。它们背后的同步内核（`calculateStripeParity`、`rebuildStripe`、`cleanupColdCache`、
`Flush`）都由上面的用例直接驱动，逻辑本身是覆盖到的。

## 目录结构

```
cmd/zenofs/        入口：配置 → DB/AutoMigrate → PoolManager → 后台 worker → HTTP
internal/api/      chi 路由与 JSON 响应
internal/config/   配置加载（argv > ZENOFS_DSN > 默认值）
internal/db/       GORM 连接与全部数据模型
internal/errs/     统一错误码与 ZenoError
internal/hash/     摘要算法收口（BLAKE3-256；分片/校验分片/口令校验值都用它）
internal/pool/     核心：存储池 / 磁盘 / chunk / parity / 读缓存
internal/testutil/ 测试脚手架：临时 SQLite 库 + 本地盘存储池 + Share 行（只被 _test.go 引用）
internal/token/    访问凭证（access token）：生成 / 单向摘要 / 过期校验，SMB / SFTP / WebDAV 共用
internal/smb/      SMB2/3 服务端（jfjallid/go-smb）：共享注册 + NTLMv2 认证 + vfs 适配
internal/sftp/     SFTP 服务端（x/crypto/ssh + pkg/sftp）：公钥 / token 口令认证 + vfs 适配
internal/webdav/   WebDAV 服务端（x/net/webdav）：挂在 HTTP API 服务上（同一端口，默认 /dav）
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
| `pools` | RS 存储池 | `name`(唯一) `data_shards` `parity_shards` `chunk_size`(KB) `status` |
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

## 核心流程

### 存储池与磁盘

- `AddPool(name, chunkSizeKb)`：`chunk_size` 限 1~65536 KB，名称唯一。
- `AddDisk(poolId, path, backend, diskType, addParity)`：
  - `DataDisk`：事务内建盘并调整池配置——`addParity=false` 使 `DataShards++`，`true` 使 `ParityShards++`；
    随后为该池**已有**的每个 stripe 补一个 `ChunkReserved` 的 slot（新盘补齐条带位置）。
  - `CacheDisk`：只建记录，不参与条带化。
- 注意：parity 盘在表里 `type` 仍是 `DataDisk`，靠 `addParity` 决定它在条带中占 data 位还是 parity 位。
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

写完数据后**需要调用 `Flush(chunkIds)`**（`chunkIds` 为空表示全库），parity 才会被计算：

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
- **写**：**切片级写时复制**。`idx = offset / Share.SliceSize`，只重建被写到的切片；
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

## 文件协议（SMB / SFTP / WebDAV）

三个协议服务都接 `internal/token` 的 access token 认证、以 `internal/vfs` 为文件系统；
真正决定能读写什么的是 `share_users` 里的授权，而不是协议层的共享权限。

其中 **SMB 与 SFTP 各自监听一个端口**（见下），**WebDAV 不额外占端口**：它挂在 HTTP API
服务上（同一个端口、默认 `/dav` 前缀），见下面 WebDAV 一节。

### 启用与配置

端口存在 `settings` 表里，**为空表示不启用**——445 / 22 是特权端口，默认关掉才能让普通用户
直接 `go run ./cmd/zenofs` 跑起来：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `SMB_PORT` | 空（不启用） | 例如 `1445`；标准端口 `445` 需要 root |
| `SMB_NETBIOS_NAME` | `ZENOFS` | NTLM TargetInfo 里对客户端可见的服务端名 |
| `SFTP_PORT` | 空（不启用） | 例如 `2222`；标准端口 `22` 需要 root |
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

- 登录后的根目录就是该用户可见的 Share 列表：`/<share>/...`（`vfs.RootFS`）。
- 两种认证都支持：SSH 公钥（按 `fingerprint` 匹配）与"用户名 + token"口令。
- 打开 / 读写 / 列目录 / 改名 / 删除 / mkdir / statvfs 都翻译成 vfs 调用
  （`internal/sftp/handlers.go`），vfs 错误翻成 `syscall.Errno`，由 pkg/sftp 转成客户端的 `SSH_FX_*`。

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

| 端点 | 说明 |
|---|---|
| `POST /api/users/{id}/tokens` | 生成一条随机 token；响应里的 `token` 是明文，**只返回这一次** |
| `GET /api/users/{id}/tokens?kind=secret\|public_key` | 列出凭证（只有元数据，永不回传摘要） |
| `POST /api/users/{id}/pubkeys` | 注册一条 SFTP 公钥（`public_key` 用 authorized_keys 行格式） |
| `DELETE /api/tokens/{id}` | 吊销凭证（硬删除，立即失效） |

请求体里的 `expires_at` 是 Unix 秒，`0` 或省略表示永不过期。

```bash
curl -X POST localhost:8080/api/users/1/tokens -d '{"name":"laptop"}'
# => {"id":1,...,"token":"<明文，仅此一次>"}
```

## 约定

- 错误一律用 `internal/errs` 的 `ZenoError`（数值 `Code` + 字符串 `StrCode` + `InnerErr`）；
  API 层把 `ZenoError` 映射为 400 + 结构化 JSON，其余 error 映射为 500。
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
