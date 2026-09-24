# zenofs

ZenoFS —— 基于 Reed-Solomon 纠删码的存储系统。

## 项目简介

写入的数据被切成 N 个 data chunk，用 RS 编码算出 M 个 parity chunk，打散落到池内不同磁盘上；
任意丢失不超过 M 个分片都能重建。对外通过 HTTP REST API 提供存储池管理、分片读写。

当前状态：存储层（pool / disk / stripe / chunk / parity / cache）已可用；Share Layer 的模型
（User / Share / Inode / Version）已在 `internal/db/models.go` 定义并建表；`internal/vfs` 提供
供 SFTP / SMB / WebDAV 共用的文件系统接口与单 Share 实现。**尚缺**：压缩/加密、多 Share 聚合层，
以及三个协议的服务端本身。

## 技术栈

| 用途 | 依赖 |
|---|---|
| 语言 | Go 1.26.5（见 `go.mod`） |
| HTTP 路由 | `github.com/go-chi/chi/v5` |
| ORM | `gorm.io/gorm` + `driver/sqlite`、`driver/mysql`、`driver/postgres` |
| 纠删码 | `github.com/klauspost/reedsolomon` |
| 哈希 | `github.com/zeebo/blake3` |
| 并发 | 标准库 `sync` + 项目内自建的 `parallelEach`（`internal/pool/parallel.go`） |

依赖已提交到 `vendor/`，构建默认走 `-mod=vendor`；改动依赖后用 `go mod vendor` 重新同步。

## 常用命令

```bash
go build ./...                        # 构建
go vet ./...                          # 静态检查
gofmt -l ./cmd ./internal             # 格式检查

go run ./cmd/zenofs                   # 默认 sqlite://zenofs.db
go run ./cmd/zenofs "postgres://..."  # 也可用 ZENOFS_DSN 环境变量
```

DSN 前缀决定驱动（`sqlite://` / `mysql://` / `postgres://`）。HTTP 端口取自 `settings` 表的
`HTTP_PORT`（默认 8080）。

## 目录结构

```
cmd/zenofs/        入口：配置 → DB/AutoMigrate → PoolManager → 后台 worker → HTTP
internal/api/      chi 路由与 JSON 响应
internal/config/   配置加载（argv > ZENOFS_DSN > 默认值）
internal/db/       GORM 连接与全部数据模型
internal/errs/     统一错误码与 ZenoError
internal/hash/     摘要算法收口（BLAKE3-256；分片/校验分片/口令校验值都用它）
internal/pool/     核心：存储池 / 磁盘 / chunk / parity / 读缓存
internal/vfs/      文件系统层（SFTP / SMB / WebDAV 共用）：接口定义 + 基于 Share/Inode/Version 的实现
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

### Share Layer（模型 + 接口，尚无实现）

`users` `shares` `share_users` `inodes` `versions` `version_chunks` `inode_histories`

`internal/vfs` 定义了 SFTP / SMB / WebDAV 共用的 `FileSystem` / `File` 接口（只有接口，无实现）。
VFS 属性到模型的对应关系：

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
- **配额**：写路径按「该 Share 所有 inode 当前版本大小之和」判断，超限返回 `ErrNoSpace`。
- **属性映射**：见上面的对照表；`SetAttr` 的 Uid/Gid 被忽略，Mode 只影响可执行位。

**压缩与加密按切片做**（每片自带完整的压缩流与密文），所以随机读、部分写都不受影响：

- 压缩：`Share.Compression = 1` 用 Zstandard（`klauspost/compress/zstd`，每片一条独立的 zstd 帧，
  默认档位 `SpeedDefault`，帧头只有十几字节，相对 MB 级切片可忽略）；
- 加密：`Share.Encryption = 1` 用 AES-256-GCM，密文格式 `nonce(12B) || ciphertext+tag`，每片随机 nonce，
  GCM 自带认证——密文被篡改时解密直接失败，会触发该条带的重建；
- 密钥：由用户口令经 **PBKDF2-HMAC-SHA256（600k 次迭代）**派生，**只存在于会话内存**（`ShareFS.Unlock`
  解锁、`Lock` 清除）。落库的只有 `shares.encryption_key_hash`，布局是 `salt(16B) || blake3(密钥)(32B)`：
  前者供派生使用，后者用来校验口令是否正确。首次启用加密用 `SetPassword` 生成 salt；
- 算法记在 **`versions.compression` / `versions.encryption`** 上，因此以后改 Share 的算法不会让历史数据解不开；
  例外：`compression = 1` 曾经表示 DEFLATE，现已改为 Zstandard——老库里按 flate 写下的切片解不开
  （需要重写数据；本地开发库无此类数据）；
- 加密的 Share 未解锁时，`Open` 直接返回 `ErrLocked`（`Stat`/`ReadDir` 等元数据操作不受影响）。

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

## 约定

- 错误一律用 `internal/errs` 的 `ZenoError`（数值 `Code` + 字符串 `StrCode` + `InnerErr`）；
  API 层把 `ZenoError` 映射为 400 + 结构化 JSON，其余 error 映射为 500。
- **DB 操作批量进行**：一次 `IN` 查询、一条 `UPDATE ... WHERE id IN (...)`、一次 upsert，
  不要写逐条往返的循环；一批操作尽量包在一个 `Transaction` 里。
- 文件读写统一走 `ChunkHandler` 接口（`Write` / `Read` / `Delete`，见 `internal/pool/handler.go`），
  按 `disk.Backend` 匹配实现，不要在业务层直接调 `os`。
- 注释用中文，日志用英文（`log.Printf("parity: ... failed: %v", err)`）。
- 新增模型必须登记到 `AutoMigrate`。

## 验证

仓库当前没有测试文件。改动后至少跑：

```bash
go build ./... && go vet ./... && gofmt -l ./cmd ./internal
```

涉及并发（`parallelEach` / `computeStripe` / 缓存 / 队列）的改动，建议临时补一个 `-race` 冒烟测试，
验证通过后**删除测试文件**，不要把临时测试留进仓库：

```bash
go test -race -count=1 ./internal/pool/
```

全仓 `gofmt -l` 保持干净。
