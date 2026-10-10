# Linux I/O 栈 30 问（面试防守版）

> 适用场景：简历/自我介绍写了「了解 Linux I/O 栈」，面试官据此往下钻。
> 考察的不是"你跑过什么实验"，而是**你能不能画出分层、说出数据结构、说出函数调用链、说清楚每层的开销从哪来**。
> 30 题按 7 层编排、难度递增（⭐ 基础 → ⭐⭐⭐⭐⭐ 深水区），每题给参考答案，标注 `[深水区]` 的是最容易被问倒的。
>
> **重要提醒（也是加分点）**：内核里很多名字随版本变（`readpage`→`read_folio`、`readpages`→`readahead`、radix tree→XArray、`page`→`folio`、`generic_file_buffered_read`→`filemap_read`、`submit_bio`→`submit_bio_noacct`）。回答时**主动说明版本差异**比背一个过时的函数名强得多，下面的答案里都标了"旧名/新名"。

---

## 0. 先背这张图（一句话一层）

```
① 用户态         read/write/pread/mmap/io_uring        数据: 应用 buffer
② 系统调用层      ksys_read → vfs_read                  数据: struct kiocb, iov_iter
③ VFS            struct file / dentry / inode / address_space
④ 具体文件系统    ext4 / xfs / btrfs（extent、journaling、delalloc）
⑤ 页缓存 (仅 buffered)  address_space 的 XArray 里存 folio/page
⑥ 通用块层        bio → request → request_queue（blk-mq）→ 调度器
⑦ 设备驱动        NVMe / SCSI / virtio（DMA、多队列）
⑧ 物理设备        SSD/HDD（FTL、GC、易失写缓存）
```

**两条关键分叉**：

- `O_DIRECT`：跳过 ⑤ 页缓存，用户 buffer 直接被 DMA 写/读（要求对齐），⑥⑦⑧ 一层不少。
- `mmap`：⑤ 的 folio 直接映射进用户地址空间，磁盘 I/O 由缺页异常触发，省掉"页缓存→用户"的拷贝。

**一句话总结整个栈的职责分工**：**VFS/FS 决定"数据在哪个块"，页缓存决定"要不要碰磁盘"，块层决定"什么时候、以什么顺序发出去"，驱动/设备决定"多久能真正落盘"。**

---

## 一、整体图景（第 1–4 题 ⭐）

### 1. 画出 Linux I/O 栈的分层，并说出每层的核心数据结构。
**答**：

| 层 | 核心数据结构 | 职责 |
|---|---|---|
| 系统调用/VFS | `struct kiocb`、`struct iov_iter`、`struct file`、`struct dentry`、`struct inode`、`struct address_space` | 名字解析、权限、把"文件+偏移"变成一次读写的语义描述 |
| 具体文件系统 | `extent tree`、`jbd2_transaction`（ext4）、`xfs_bmbt_irec` | 文件逻辑块 → 设备物理块；日志/一致性 |
| 页缓存 | `struct folio`（旧：`struct page`），索引在 `address_space->i_pages`（XArray，旧：radix tree） | 缓存文件内容、预读、脏页管理 |
| 通用块层 | `struct bio`、`struct request`、`struct request_queue`、`blk_mq_hw_ctx`/`blk_mq_ctx` | 合并/排序/限流/派发，多队列 |
| 设备驱动 | `struct blk_mq_tags`、NVMe SQ/CQ、`dma_addr_t` | 翻译成硬件命令、DMA 映射、doorbell |
| 设备 | FTL 映射表、NAND page/block、DRAM 写缓存 | 真正落盘；缓存刷新语义 |

**加分**：说明"越往下粒度越粗"——应用是字节，VFS 是 `iov_iter` 段，块层是 512 字节扇区为单位（`bio->bi_iter.bi_sector`），设备是 NAND page（16KB+）。很多"性能对不上账"的问题都源于**层与层之间的粒度转换**。

### 2. 一次 buffered `read()` 从用户态到设备，把关键函数按顺序说出来。
**答**（内核 5.18+ 命名，旧名标注在后面）：

```
read()                          libc wrapper
 └─ ksys_read()                 系统调用入口
     └─ vfs_read()              权限/偏移检查，file->f_mode 校验
         └─ file->f_op->read_iter()  = ext4_file_read_iter()
             └─ generic_file_read_iter()
                 └─ filemap_read()        ← 旧名 generic_file_buffered_read
                     ├─ filemap_get_pages()
                     │   ├─ filemap_get_folio()         查 XArray
                     │   ├─ page_cache_sync_readahead() 缺页且顺序 → 触发预读
                     │   └─ page_cache_async_readahead() 命中预读窗口内部 → 提前启动下一窗
                     ├─ 若 folio 非 uptodate：
                     │   └─ filemap_read_folio() → a_ops->read_folio()/readahead()
                     │        └─ ext4 读路径（mpage 或 iomap）
                     │            ├─ ext4_map_blocks() / ext4_ext_find_extent()  查 extent
                     │            └─ submit_bio()
                     │                └─ submit_bio_noacct()   ← 旧名 submit_bio / generic_make_request
                     │                    └─ blk_mq_submit_bio()
                     │                        ├─ blk_mq_sched_bio_merge()  尝试合并
                     │                        ├─ blk_add_rq_to_plug()     先塞进 plug 批量
                     │                        └─ blk_mq_run_hw_queue() → ->queue_rq() = nvme_queue_rq()
                     │                             └─ nvme_map_data()（DMA 映射）→ nvme_submit_cmd() → doorbell
                     └─ copy_page_to_iter()   把页缓存拷到用户 buffer（这一步在"读进程"上下文里做）
```

**要点**：真正的"设备 I/O"发生在 `submit_bio`；`copy_page_to_iter` 是 CPU 拷贝（`perf` 里的 `copy_user_enhanced_fast_string` 热点）；本仓库 `c/doc/RAEDME.md` 实验5 让找的 `ext4_file_read_iter`、`page_cache_sync_readahead`、`find_get_page`、`blk_mq_submit_bio` 就分布在这条链上。

### 3. 一次 buffered `write()` 的调用链，和 `read` 的差别在哪？
**答**：

```
write() → ksys_write() → vfs_write() → ext4_file_write_iter()
  ├─ O_DIRECT → ext4_dio_write_iter() → iomap_dio_rw() → submit_bio()
  └─ 否则 buffered：
      generic_perform_write()            ← 对应 read 的 filemap_read
        ├─ a_ops->write_begin()  = ext4_write_begin / iomap_write_begin
        │    └─ 查/分配页缓存 folio，delalloc 场景只预留空间不分配块
        ├─ copy_page_from_iter()           用户 buffer → 页缓存（CPU 拷贝）
        ├─ a_ops->write_end()              folio_mark_dirty / mark_buffer_dirty
        └─ balance_dirty_pages()           脏页限流：超阈值就睡在这里！
...（异步，另一个上下文）...
writeback workqueue → wb_workfn() → do_writepages() → a_ops->writepages() = ext4_writepages()
  └─ （delalloc）ext4_map_blocks() 真正分配块 → ext4_bio_write_page() → submit_bio() → blk-mq → nvme_queue_rq()
```

**四个关键差别**：① `read` 的"慢"在缺页/读设备，`write` 的"慢"在 `balance_dirty_pages` 限流和后续回写；② `write()` **返回时数据通常还没落盘**（这就是本仓库 A 阶段 583 MB/s 的来源）；③ `write` 路径多了一个 `balance_dirty_pages` 这个"可阻塞点"，它是线上写延迟毛刺的常见来源；④ 回写发生在**内核工作队列线程**里，不是发起写的进程。

### 4. 一次 `O_DIRECT` 的读，链路上跳过了哪些层、哪些层还在？
**答**：

```
pread(O_DIRECT) → ext4_file_read_iter → ext4_dio_read_iter()
   └─ iomap_dio_rw()
       ├─ ext4_iomap_begin() / iomap_iter()   查 extent —— 文件系统层还在！
       ├─ iov_iter_get_pages()                pin 住用户页（get_user_pages）
       └─ iomap_dio_bio_iter() → bio 直接指向用户页 → submit_bio()
           └─ blk-mq → nvme_queue_rq → DMA 直接写入被 pin 的用户 buffer
```

**跳过的**：⑤ 页缓存的 `filemap_get_folio`（没有缓存查找）、预读（`readahead` 完全不参与）、`copy_page_to_iter`（**省掉一次 CPU 拷贝**）。
**还在的**：②③ 系统调用与 VFS、④ 文件系统 extent 映射（要在磁盘上找到这个偏移对应的物理块）、⑥ 块层全部机制（合并、调度、cgroup 限流都还在）、⑦⑧ 驱动与设备。
**还多出来的**：① `get_user_pages` 把用户页 pin 住的开销（除非用 `io_uring` 注册缓冲区，见第 30 题）；② 对齐校验，不满足直接 `EINVAL`；③ 块大小取整导致的 RMW（非对齐或非整块时）。

> **这就是"O_DIRECT 绕过页缓存但不绕过文件系统"的完整含义**。面试里能把"跳过了什么、没跳过什么"分清，比背"O_DIRECT 快/慢"有价值得多。

---

## 二、VFS 与文件抽象（第 5–9 题 ⭐⭐）

### 5. `struct file`、`dentry`、`inode`、`super_block`、`address_space` 是什么关系？
**答**：

```
super_block  ← 一个已挂载的文件系统实例（含块大小、s_op、s_fs_info）
   └── inode ← 磁盘上的一个文件/目录（元数据：i_size、i_mode、i_uid、i_mapping、i_fop、i_op）
        ├── address_space ← 就是 inode->i_mapping，页缓存的"宿主"（i_pages 索引 + a_ops）
        │     └── i_mmap ← 反向映射：哪些进程 mmap 了这个文件的哪些页
        └── (n 个) dentry ← 名字到 inode 的绑定，缓存在 dcache
                             （dentry 可以是"负"的：名字不存在）
file ← 一次 open() 的产物（open file description）
   ├── f_path = { dentry, vfsmount }
   ├── f_inode
   ├── f_op   ← file_operations（通常从 inode->i_fop 复制而来）
   ├── f_pos  ← 文件偏移（有状态！）
   ├── f_flags / f_mode / f_count（引用计数）
   └── f_ra   ← struct file_ra_state，预读状态
```

**关键点**：`inode` 是"文件"（磁盘实体，唯一），`file` 是"打开动作"（可以有很多个）。`address_space` 挂在 inode 上，所以**页缓存是按 inode 共享的**。

### 6. `file_operations` 里的 `read`/`write` 和 `read_iter`/`write_iter` 有什么区别？为什么内核都转到了 iter 版本？
**答**：老接口 `ssize_t read(struct file *, char __user *, size_t, loff_t *)` 一次只能描述**一段连续用户缓冲**。新接口：

```c
ssize_t (*read_iter)(struct kiocb *, struct iov_iter *);
ssize_t (*write_iter)(struct kiocb *, struct iov_iter *);
```

`struct iov_iter` 抽象了"数据在哪、有多少段、是用户态还是内核态、是 iovec 还是 bvec/kvec"；`struct kiocb` 携带 `ki_pos`、`ki_flags`（`IOCB_DIRECT`/`IOCB_NOWAIT`/`IOCB_DSYNC`）、以及异步完成回调 `ki_complete`。

**换成 iter 的收益**：① 一套代码同时支持 `read`/`write`/`readv`/`writev`/pread/pwrite（分散聚集天然支持）；② 同一份实现同时服务 buffered 和 direct I/O（`ki_flags` 里区分）；③ 可以对接异步完成（`io_uring`、AIO）；④ 能对内核页/用户页用同一套迭代器操作。`vfs_read` 会优先调 `read_iter`，没有才退化到 `read`；**现代文件系统只实现 iter 版本**。

### 7. 一个进程 `open()` 同一文件两次，哪些结构是共享的、哪些是独立的？
**答**：共享的是 `inode`、`dentry`、`address_space`（因此**页缓存是同一份，两个 fd 看到的数据一致**）；独立的是两个 `struct file`，各自有独立的 `f_pos`、`f_flags`、`f_count`、`f_ra`（预读状态）。

**由此推出的经典语义**：① 两个 fd 各自 `read` 互不影响偏移；② `dup()` 或 `fork()` 复制的是**同一个 `struct file` 的引用**（共享 `f_pos`）——所以父子进程共享偏移是"设计如此"；③ `open` 两次 + `O_APPEND` 才有原子追加语义；④ 想并发随机读同一个文件，用 `pread` 而不是共享 fd 的 `read`。

### 8. 页缓存挂在 `inode` 还是 `file` 上？多进程/多 fd 是否共享？
**答**：挂在 **`inode->i_mapping`（`address_space`）** 上，所以**按 inode 全局共享**：同一文件的所有 fd、所有进程、甚至 `mmap` 都命中同一份页缓存。这带来三个直接后果：① 一个进程读过的数据，另一个进程立刻受益（跨进程缓存共享）；② 一个进程写脏的数据，另一个进程读到的是"内存里的新值"，但**是否落盘与此无关**（仍受回写时机控制）；③ **`O_DIRECT` 放弃了这份共享**——它自己的 buffer 只有本进程能看见，另外两个进程各自读盘，这是它"省内存"不可避免的代价。块设备也有对应的 `bdev->bd_inode->i_mapping`（buffer cache 就是它）。

### 9. 硬链接和软链接对 I/O 路径有什么影响？
**答**：**硬链接**：多个 `dentry` 指向**同一个 `inode`**，因此**共享同一份页缓存**（不是两份缓存），I/O 路径完全相同，没有额外开销。**软链接**：路径解析阶段由 `walk_component()` → `step_into()` / `pick_link()` 处理，需要**再解析一段路径**，如果是多级/循环符号链接还会触发 `ELOOP` 限制（默认 40 层）。**关键区别**：软链接的开销在**路径解析（lookup）**，不在 I/O 数据路径上——一旦 `open()` 成功，后续 `read`/`write` 与普通文件无异。另外硬链接不能跨文件系统（因为要共享 inode），软链接可以。

---

## 三、页缓存与地址空间（第 10–14 题 ⭐⭐⭐）

### 10. `struct page`、`folio`、`address_space` 三者的关系？为什么要有 `folio`？
**答**：`struct page` 是**一个物理页的元描述**（约 64 字节），关键字段：`flags`（`PG_uptodate`/`PG_dirty`/`PG_writeback`/`PG_locked` 等）、`mapping`（指向所属 `address_space`，或表示匿名页）、`index`（在 mapping 内的页偏移）、`private`、`_refcount`。`address_space` 是**一份文件内容在内存中的索引表**（`i_pages` XArray + `a_ops` 操作方法 + 回写状态）。

**`folio` 解决的问题**：为了减少页表/描述符开销，内核对大块使用**复合页**（compound page，2^order 个连续物理页）。用 `struct page` 表示复合页时，每个子页都要维护 `flags`/`_refcount`，而且"这个 page 是 head 还是 tail、是哪个复合页的一部分"要靠位运算猜，极易出错。`folio` 就是**"一个 head page + 它的 order"**：一个 folio 只有一个锁、一个 refcount、一组 flags，覆盖 `2^order` 页。页缓存现在以 folio 为单位分配和操作（大 folio 减少 XArray 条目数、减少锁操作、预读可以一次分配 order-2/3 的 folio）。所以：**`folio = 页缓存的逻辑单位`，`page = 物理内存的描述单位`**。

### 11. `address_space` 用什么数据结构索引页？为什么从 radix tree 换成 XArray？
**答**：旧的是 **radix tree**（`page_tree`），4.20（2018）起换成 **XArray**。索引键是 `pgoff_t index = 文件偏移 >> PAGE_SHIFT`，值是 folio 指针。两者都是"稀疏、按无符号长整型键索引、无锁读（RCU）"的树。

**换 XArray 的原因**：① **内存效率**：radix tree 节点比较稀疏浪费，XArray 每个节点 64 项且会自动收缩（shrink），小文件/大空洞的场景省内存；② **标记（marks）机制更清晰**：`PAGECACHE_TAG_DIRTY`、`PAGECACHE_TAG_WRITEBACK`、`PAGECACHE_TAG_TOWRITE` 直接挂在 XArray 上，**回写线程靠这些标记快速找到脏页/回写中页**，不用遍历整棵树；③ 单次查找只需少数几个 RCU 保护的内存访问，高并发读走查很快；④ 支持"多值条目"（把几个 folio 打包进一个 entry）。**这一点直接关系到回写性能**：`/proc/vmstat` 的 `nr_dirty` 统计和 `writeback` 扫描的范围都由这些标记驱动。

### 12. 内核怎么判断"文件的某一页在不在内存里"？
**答**：`filemap_get_folio(mapping, index)`（旧名 `find_get_page`/`find_get_entry`）在 XArray 里查，命中则返回 folio 并增加引用计数，然后检查 `folio_test_uptodate()`——**只有 uptodate 才代表数据内容有效**（页可能是刚分配出来的空洞页，内容还没填）。后续：

- 如果命中且 uptodate → 直接 `copy_page_to_iter`（命中）；
- 如果命中但未 uptodate → 说明已有别的线程在读了，`folio_lock` + `folio_wait_bit` 等它变 uptodate（**避免重复发起磁盘 I/O**，这就是"同页并发读只读一次盘"的机制）；
- 如果没命中 → 分配 folio、插入 XArray、加锁、发起 I/O（或预读），完成后 `folio_mark_uptodate()` 再唤醒等待者。

`mmap` 的缺页路径 `filemap_fault()` 用的是同一套查找逻辑——**所以 mmap 和 read 共享页缓存，不是两套**。

### 13. 预读（readahead）的机制是什么？为什么随机读会被它拖累？
**答**：状态在 `struct file_ra_state`（`start`/`size`/`async_size`/`ra_pages`/`mmap_miss`），上限 `ra_pages` 来自 `bdi->ra_pages`（用户可调 `read_ahead_kb`，默认 128KB）。两个入口：

- `page_cache_sync_readahead()`：首次缺页时调用，判断是否像顺序访问，是则**发起一个预读窗口**（初始通常 2 页，逐次翻倍到上限）；
- `page_cache_async_readahead()`：当读到的页**落在已有预读窗口的靠后位置**时触发，提前把**下一个窗口**发出去——这是"异步流水线"的关键，让磁盘始终有活干。

**顺序读**：窗口越来越大，一次 I/O 覆盖 128KB+，吞吐高。**随机读**：每次访问都不在窗口内，内核要么误判顺序（把后面用不到的 127KB 读进内存，浪费带宽、污染缓存），要么每页都触发一次"同步预读"——**开销比直接读 4KB 还大**。所以随机访问要 `posix_fadvise(FADV_RANDOM)`（把 `ra_pages` 置 0），或直接用 `O_DIRECT`。仓库里 `c/doc/RAEDME.md` 把 `page_cache_sync_readahead` 列为 perf 要观察的热点，正是要看"预读到底在帮忙还是添乱"。

### 14. 脏页是怎么被限流的？`balance_dirty_pages` 和 `wb_workfn` 各在什么时候跑？
**答**：写路径在 `generic_perform_write`（iomap 是 `iomap_write_iter`）里，`write_end` 把 folio 标脏之后会调 **`balance_dirty_pages()`**：

- 脏页（全局 + 该 bdi 的）低于 `dirty_background_ratio` → 不管，直接返回（最快）；
- 超过 `dirty_background_ratio` → **唤醒回写线程**（`wb_wakeup_delayed`），自己继续写；
- 超过 `dirty_ratio`（或 `dirty_bytes`）→ **在 `balance_dirty_pages` 里睡眠**，并且**主动做回写**（`writeback_inodes_sb`，"用写者去做回写"），这就是"应用被 I/O 阻塞"的现场。

**回写线程**：`writeback` workqueue 上的 `wb_workfn()` → `do_writepages()` → `a_ops->writepages()`（`ext4_writepages`）。它干三件事：① 到期脏页（`dirty_expire_centisecs`，默认 30s）；② 水位超限的脏页；③ 周期性唤醒（`dirty_writeback_centisecs`，默认 5s）。**回写时用的是 `PAGECACHE_TAG_DIRTY`/`TOWRITE` 标记来选页**（第 11 题）。

**工程含义**：`balance_dirty_pages` 是**应用线程可以睡几十毫秒到数秒的地方**，也是组提交/批量写能显著改善延迟的原因——减少它被唤醒的次数。

---

## 四、文件系统层（第 15–18 题 ⭐⭐⭐）

### 15. buffered write 在文件系统层怎么落地？`buffer_head` 和 `iomap` 是什么关系？
**答**：**`buffer_head`（bh）** 是"**一个块**的 I/O 描述符"，挂在页的 `private` 上（一页 4KB 若块大小 1KB 就有 4 个 bh），用来跟踪"这一块是否脏、是否在写、映射到哪个物理块"。老式 buffered 写路径是 `block_write_begin` → 填页 → `mark_buffer_dirty` → 回写时遍历 bh 逐个 `submit_bh`。问题是：bh 本身有开销，元数据（间接块映射）I/O 和数据的 I/O 分离，代码分散在几十个 `*_write_begin/end` 里。

**`iomap`** 是统一的新接口（XFS 首创，ext4/gfs2/zonefs 采用）：用 `struct iomap { addr, offset, length, type }` 描述"这一段逻辑偏移对应的物理布局"，`type` 区分 `HOLE`（空洞）/`DELALLOC`（已预留未分配）/`MAPPED`（已映射）/`INLINE`（内联数据），由 `iomap_ops->iomap_begin()`/`iomap_end()` 提供。然后**一套 `iomap_apply()` 框架同时支撑 buffered 读（`iomap_readahead`/`iomap_read_folio`）、buffered 写（`iomap_file_buffered_write`）、direct I/O（`iomap_dio_rw`）和 DAX**——把"文件系统特定逻辑（分配/映射）"和"通用 I/O 逻辑（页缓存/块层交互）"彻底分离。

**回答技巧**：不要断言"ext4 已经全部 iomap 化"，说"**XFS 早已全面 iomap；ext4 长期以 buffer_head/mpage 路径为主，direct I/O 和 DAX 走 iomap，buffered 路径在近年版本里逐步迁移**，所以看 perf 热点时两种函数名都可能出现"——这是准确的表述，也体现了你知道迁移还在进行。

### 16. ext4 的 delayed allocation（延迟分配）是什么？好处和风险？
**答**：`write()` 时**只预留空间（reserve block）并标记 `DELALLOC`，不真正写 extent 树**；等到**回写时才调用 `ext4_map_blocks()` 真正分配物理块**。好处：① **分配决策更聪明**——回写时能看到"这一批要写多少页"，一次分配一大片连续块，减少碎片（尤其对 HDD 和 SSD 的顺序写都友好）；② **减少元数据写**——多次小块写合并成一次 extent 分配，省掉反复写 extent 树和位图；③ 短命文件（写了就删）可能压根不用分配，省了分配+回收。风险：① **`ENOSPC` 延迟暴露**——`write()` 可能成功，回写时才失败，所以应用必须处理"写入成功但最终丢数据"的情况；② **崩溃一致性**——元数据提交前数据必须先落盘，否则可能读到未初始化的旧数据（这正是下面 `data=ordered` 要解决的）；③ **延迟不可预测**——回写时集中分配会让某些写变慢。对应挂载选项 `nodelalloc` 可以关掉换取可预测性。

### 17. journaling（日志）在 I/O 栈的哪一层？`data=ordered` / `journal` / `writeback` 有什么区别？
**答**：日志在**具体文件系统层**（ext4 由 `jbd2` 实现）。它不改变 I/O 栈的结构，而是在文件系统层**额外产生一组日志 I/O**（写到 journal inode），用于崩溃后重放恢复一致性。三种模式：

| 模式 | 日志内容 | 崩溃后风险 | 性能 |
|---|---|---|---|
| `data=writeback` | 只记元数据 | 元数据一致，但文件可能有**旧数据**（stale data 暴露） | 最快 |
| `data=ordered`（默认） | 元数据入日志 + **强制数据块先于元数据提交落盘** | 不会暴露旧数据（要么老版本要么新版本） | 中等 |
| `data=journal` | 数据和元数据都写日志 | 最安全 | 最慢（**数据写两遍**：日志 + 最终位置） |

**和 `fsync` 的关系**：ext4 的 `fsync` 路径 `ext4_sync_file()` → `file_write_and_wait_range()`（刷该文件的数据页）+ `jbd2_complete_transaction()`（等包含该文件元数据的**事务提交**，事务提交时要发 flush/FUA 保证日志真正落盘）。**这解释了两件事**：① 为什么 `data=ordered` 下 fsync 的成本是"数据页 + 日志提交"两段；② 为什么 fsync 的延迟里有一部分是在等**别的文件的元数据事务**（同一个 jbd2 事务里会合并很多操作）——这是 fsync 延迟抖动的来源之一。

### 18. 文件系统怎么把"文件偏移"翻译成"设备扇区号"？
**答**：三级翻译：

```
文件字节偏移 → 文件逻辑块号   off >> inode->i_blkbits（ext4 通常 12，即 4KB 块）
文件逻辑块号 → 设备物理块号   查 extent/间接块映射
设备物理块号 → bio 里的扇区号   << (block_bits - 9)（块层和设备统一用 512 字节扇区为单位）
```

具体函数：ext4 是 `ext4_map_blocks()` → `ext4_ext_find_extent()` / `ext4_ext_map_blocks()`（extent 树查找，对应 `ext4_extent`/`ext4_extent_idx`）；老的间接块映射走 `ext4_block_to_path()` / `ext4_get_branch()`（三级指针：直接块、一级间接、二级间接、三级间接）。XFS 是 `xfs_bmapi_read()`（B+ 树 `bmbt`）。拿到物理块号后，`iomap.addr` 或 `bh->b_blocknr` 被填进 `bio`，块层用 `bio->bi_iter.bi_sector`（**512 字节单位**）和 `bi_bdev` 定位设备。

**加分点**：说明**空洞（hole）**也在这层处理——`iomap` 返回 `HOLE` 类型，读的时候不需要发 I/O，直接填零页（`iomap_zero_iter` 或直接返回零），这是稀疏文件的实现基础。

---

## 五、通用块层（第 19–24 题 ⭐⭐⭐⭐）

### 19. `struct bio` 是什么？说出关键字段。
**答**：`bio` 是**块层的 I/O 描述单元**，"要把哪些内存页写到设备的哪些扇区"。关键字段：

- `bi_iter`：`bi_sector`（**512 字节扇区**为单位，设备地址）、`bi_size`、`bi_idx`（当前处理到第几个 bvec）、`bi_bvec_done`。**提交后 `bi_iter` 会随处理推进**，所以 bio 内容是不可变的、但游标会动。
- `bi_opf`：操作码 + 标志位。操作码 `REQ_OP_READ/WRITE/DISCARD/WRITE_ZEROES`；标志位关键的有 `REQ_SYNC`、`REQ_META`（元数据，影响调度优先级）、`REQ_PRIO`、**`REQ_PREFLUSH`（先刷设备缓存）**、**`REQ_FUA`（这一笔必须落到介质）**、`REQ_NOMERGE`、`REQ_IDLE`、`REQ_SWAP`。
- `bi_io_vec` + `bi_vcnt`：`struct bio_vec` 数组，每项是 `{bv_page, bv_len, bv_offset}` —— **所以一个 bio 覆盖的设备区间是连续的，但物理页可以不连续**（散列表）。
- `bi_bdev`：目标块设备。
- `bi_end_io`：完成回调（这是把结果往回传的钩子：`mpage_end_io`、`end_bio_bh_io_sync`、`iomap_dio_bio_end_io`）。
- `bi_private`：回调的私有数据。
- `bi_status`：完成状态（`BLK_STS_OK`/`BLK_STS_IOERR`/`BLK_STS_AGAIN`...）。
- `bi_blkg` / `bi_ioprio` / `bi_write_hint`：cgroup 归属、I/O 优先级、写提示（用于 SSD 的 stream 识别）。

**一个 request 可以包含多个 bio**（合并的结果）。

### 20. bio 是怎么变成 request 的？plug、merge、调度器各自在什么时机介入？
**答**：`submit_bio` → `submit_bio_noacct`（做分区重映射、只读检查、**cgroup 限流** `blk_throtl_bio`、zone 检查）→ `blk_mq_submit_bio`：

1. **合并（merge）**：`blk_mq_sched_bio_merge()` 先按"扇区范围哈希"查找能否并入已有的 request（`blk_attempt_merge` 检查方向、连续性、cgroup、`REQ_NOMERGE`）；能合就合，省一次设备 I/O。
2. **plug（插头）**：如果不能合，`blk_add_rq_to_plug()` 把 request 先塞进**发起进程的 plug 链表**（`blk_plug`），**故意不立即下发**。这是为了给后续请求留合并机会——一次 `write()` 里产生的多个页、或连续的系统调用，都能在同一批里被合并成大 I/O。换出时机是 `blk_finish_plug`/`blk_mq_flush_plug_list`（例如 writeback 批量提交结束时、或进程调度出去前）。
3. **调度器**：进入 `blk_mq_sched_insert_request()`，按所选调度器（`none`/`mq-deadline`/`kyber`/`bfq`）排序/限流，然后 `blk_mq_run_hw_queue()` → `blk_mq_dispatch_rq_list()` → 驱动 `->queue_rq()`。

**面试价值**：很多人只知道"块层会合并"，但不知道 **plug 是应用侧批量合并的关键**——这解释了为什么 `write()` 一个大 buffer 比循环 4KB `write()` 高效得多（前者更容易合并成大 I/O），也是本仓库 `exp2` A 阶段"每块 4KB 顺序 pwrite 反而拿到 583 MB/s"的原因之一：虽然每笔 4KB，但页缓存路径上的合并/plug 让回写时能形成大 I/O。

### 21. blk-mq 的软件队列、硬件队列、ctx 是怎么组织的？为什么需要它？
**答**：

```
每个 CPU × 每个 hctx 一个 struct blk_mq_ctx（软件队列/上下文）
   └── 用 CPU 本地锁保护，无跨核竞争
struct blk_mq_hw_ctx（硬件队列）← 对应设备的一个真实队列
   ├── dispatch 链表（即将下发的 request）
   └── blk_mq_tags（预分配的 request 池 + sbitmap 位图做无锁 tag 分配）
blk_mq_map_queues() 负责 ctx → hctx 的映射（通常按 CPU/节点就近映射）
```

**为什么需要**：老的单队列块层（`request_queue` 一把大锁）在 NVMe 上（百万 IOPS、几十个硬件队列）会让**锁竞争成为瓶颈**，CPU 加得越多越糟。blk-mq 的设计是"**每个 CPU 有自己的软件队列 + 属于自己的 tag 池 + 无锁 sbitmap 分配 tag**"，把提交路径上的竞争降到最低，然后把请求分发到设备的多个硬件队列（`nr_hw_queues`，NVMe 通常是每 CPU 一个），充分利用设备并行度。`nr_requests`/`queue_depth` 决定 tag 数（即队列深度上限）。驱动通过 `blk_mq_ops`（`queue_rq`/`map_queues`/`complete`）接入，`BLK_MQ_F_BLOCKING` 表示 `queue_rq` 可以睡眠。

### 22. I/O 调度器有哪些？各自适合什么设备？
**答**：

| 调度器 | 机制 | 适合 |
|---|---|---|
| `none` | 不排序，直接派发（仅保留 bio 合并） | **NVMe / 高性能 SSD**（无寻道成本，排序纯属浪费 CPU） |
| `mq-deadline` | 读/写各自 FIFO + 过期时间，保证读写不互相饿死 | SATA SSD、HDD、**写饥饿敏感**的场景 |
| `kyber` | 按目标延迟做 token 节流，区分 read/write/discard | 快速设备上需要**约束延迟**的场景 |
| `bfq` | 预算公平排队，按进程/cgroup 分配带宽 + 低延迟优先 | **桌面交互**、需要公平隔离的机械盘 |

**选择依据**：① SSD（尤其 NVMe）默认 `none` 通常最优，因为设备本身并行度高、无寻道，调度只增加 CPU 开销和延迟；② 出现"读被大量写饿死"（读延迟飙升）时考虑 `mq-deadline`/`kyber`；③ 需要"多个进程公平共享一块盘、且交互进程优先"才上 `bfq`（它的开销最大）。查看/切换：`/sys/block/<dev>/queue/scheduler`。**注意**：`none` 也**不是完全不合并**——`blk_mq_sched_bio_merge` 仍在工作（第 20 题）。

### 23. cgroup 的 I/O 限制（`io.max`/`io.weight`/`io.latency`）挂在栈的哪一层？
**答**：挂在**块层的入口**，具体在 `submit_bio_noacct` 之后、`blk_mq_submit_bio` 之前：`blk_throtl_bio()`（限速，对应 `io.max` 的 rbps/wbps/riops/wiops）、`blkcg_bio_issue_check()`/`bio_associate_blkg()`（把 bio 关联到 cgroup，记录在 `bio->bi_blkg`）。`io.weight` 由 **iocost** 实现（对设备建模，按 vtime 分配带宽份额）；`io.latency` 由 **blk-iolatency** 实现（监控各 cgroup 的延迟，超目标就限流）。

**这里有一个非常重要的坑（面试加分点）**：**buffered write 的限流发生在回写时，不是在 `write()` 时**。因为 `write()` 只是写页缓存，真正产生 bio 的是回写线程（`wb_workfn`），而回写线程的 cgroup 归属要靠 **cgroup writeback**（memcg → blkcg 的 inode 归属关联）来传递，逻辑复杂且不精确。后果是：**容器里限了 `wbps`，应用却感觉 `write()` 依然飞快**（数据只是进了页缓存），直到脏页堆积触发限流/回收才突然卡住。要精确限流和可预测延迟，配合 `O_DIRECT` 更可靠。

### 24. 一个 request 完成后，是怎么一步步唤醒读进程的？`[深水区]`
**答**：以 buffered 读为例，完整反向路径：

```
设备完成 → 中断（MSI-X）
 └─ nvme_irq() → nvme_handle_cqe() → nvme_complete_rq()
     └─ blk_mq_complete_request()
         ├─ 若需要"远端"完成（软中断上下文/其他 CPU）→ 发 IPI，走 blk_mq_complete_softirq
         └─ __blk_mq_complete_request() → blk_mq_end_request()
             └─ blk_update_request()       递减 request 剩余量、更新统计（iostat 的数据来源）
                 └─ blk_mq_free_request()  释放 tag、归还 request
                     └─ bio_endio(bio)
                         └─ bi_end_io 回调 = mpage_end_io / iomap_read_end_io
                             └─ folio_end_read() / folio_mark_uptodate() + folio_unlock()
                                 └─ folio_wake_bit()  唤醒等待该页的进程
...（读进程被唤醒，回到 filemap_read 里）
 └─ copy_page_to_iter()   ← 真正的"数据交给用户"发生在读进程自己的上下文
```

**三个容易被忽略的点**：① **数据拷贝在读进程被唤醒后才做**（不是中断里），所以中断处理必须尽量短（这就是 softirq/线程化中断 `threadirq` 存在的原因）；② **`blk_update_request` 是 `iostat` 等统计的来源**，也负责处理部分完成（partial completion）；③ 如果用 **IOPOLL**（`io_uring`/AIO 轮询），**没有中断**，由 `blk_mq_poll` 主动查 CQ；如果用线程化中断，完成走内核线程，延迟会多一次调度。**修 `blk_update_request` 或 `bio_endio` 里的 bug 会导致"数据看起来读对了但状态没更新"这类诡异问题**。

---

## 六、驱动与设备（第 25–27 题 ⭐⭐⭐⭐）

### 25. NVMe 驱动怎么把一个 request 变成硬件命令？
**答**：

```
nvme_queue_rq()                    驱动入口（blk_mq_ops.queue_rq）
 ├─ nvme_map_data()                dma_map_sg：把 bio 的页映射成设备可用的 DMA 地址
 ├─ nvme_setup_rw()                填 64 字节的 NVM 命令（opcode/NSID/LBA/NLB/PRP 列表）
 ├─ nvme_submit_cmd()              把命令写进主机内存里的 SQ 环项
 └─ writel(...) 到 doorbell 寄存器  通知控制器"有新命令"
...控制器 DMA 读走 SQE，执行，写 CQE（16 字节：result/status/sq_head/command_id）...
控制器写 CQ + 发 MSI-X 中断 → nvme_irq() → 按 command_id 找回 request → blk_mq_complete_request
```

**关键概念**：① **SQ/CQ 是主机内存里的环形队列**，控制器通过 DMA 访问（不是 MMIO 传数据）；② **doorbell 才是 MMIO 写**，一次只写一个索引值，成本很低；③ `command_id` 是驱动用来把 CQE 映射回 request 的标识；④ **多队列**：NVMe 支持最多 64K 个队列、每队列最多 64K 深度，驱动通常每 CPU 一个队列（对应 blk-mq 的 hctx），还有独立的 admin 队列做控制命令；⑤ **PRP/SGL**：描述数据位置，PRP 是两个指针 + 页列表（简单但要求页对齐），SGL 是真正的散列表（支持非页对齐）。

### 26. DMA 和 IOMMU 在栈里起什么作用？bounce buffer 什么时候出现？
**答**：设备只会用**总线地址/IPOVA**，不能直接用内核虚拟地址。`dma_map_sg()`/`dma_map_page()` 在**有 IOMMU** 时建立"物理页 → IOVA"的映射（同时提供**设备隔离**，防止设备 DMA 越界访问任意内存，也是 VFIO 直通和容器安全的基础）；**没有 IOMMU** 时就是物理地址直通（要求地址落在设备的 DMA mask 范围内）。DMA 完成后要 `dma_unmap_*` 解除映射（或延迟到下次复用）。

**bounce buffer（弹跳缓冲）出现的时机**：当待传输的缓冲区**不能被设备直接 DMA** 时，内核先把它拷进一块"设备能访问的缓冲"——这就是多出来的一次拷贝。典型原因：① 页的物理地址超出设备的 DMA mask（老 32 位设备 + 高内存，`swiotlb` 负责弹跳）；② 缓冲区跨页且设备要求连续（`blk_queue_bounce`，老式高端内存场景）；③ **部分块/非对齐的 I/O 需要读-改-写**（`O_DIRECT` 的非整块写：先把整块读进内核缓冲、改一部分、再整块写回）。**所以"O_DIRECT 一定没有拷贝"是错的**——在非对齐、RMW、或设备 DMA 受限时，bounce/取整拷贝依然存在（这也是第 4 题里"多出来的成本"）。

### 27. 设备侧的 FTL、GC、写缓存分别是什么？栈里哪一层负责保证"真落盘"？
**答**：**FTL（Flash Translation Layer）** 是 SSD 主控里的逻辑地址→物理地址映射层（LBA→PBA），负责磨损均衡、坏块管理、掉电恢复。**GC（垃圾回收）**：NAND 只能按块（MB 级）擦除，GC 会把有效页搬走、擦掉整块再复用，**GC 与前台 I/O 争抢 NAND 带宽，是 SSD 延迟毛刺的主要来源**（对应本仓库 mmap 实验里 P999 10ms 的毛刺）。**写缓存**：SSD 有一块 DRAM 易失缓存，写命令返回时数据往往只到了这里（本仓库 `exp2` D 阶段注释说的就是这件事）。

**"真落盘"由栈的这两层协作保证**：**文件系统/块层设置标志（`REQ_PREFLUSH` + `REQ_FUA`）→ 驱动翻译成设备命令**（NVMe Flush、SCSI SYNCHRONIZE CACHE、ATA FLUSH CACHE；FUA 则是把"必须落介质"的比特放进写命令本身）。设备收到 Flush 必须把整个易失缓存刷到 NAND 后才回完成。**如果设备有 PLP（断电保护电容）**，它的写缓存被认为是安全的，Flush 可以很快返回甚至优化掉——**这就是"服务器 SSD 带电容"为什么值钱**：它让 `fsync` 从"必须等几十毫秒"变成"可以立即返回"。

---

## 七、观测与工程收口（第 28–30 题 ⭐⭐⭐⭐⭐）

### 28. 栈的每一层分别用什么工具观测？把 tracepoint 和 perf 热点函数对上。
**答**：

| 层 | 工具 | 关键热点/事件 |
|---|---|---|
| 用户态/系统调用 | `strace -c -T`、`perf trace` | `pread64`/`pwrite64`/`fsync` 次数与耗时 |
| VFS/文件系统 | `perf record -g`、bpftrace kprobe | `vfs_read`、`ext4_file_read_iter`、`ext4_mpage_readpages`、`jbd2_journal_commit_transaction` |
| 页缓存/预读 | `perf`、`/proc/vmstat`、`cachestat` | `page_cache_sync_readahead`、`filemap_get_pages`、`pgpgin`/`pgmajfault`/`pgsteal_*` |
| 脏页/回写 | `/proc/vmstat`、writeback tracepoint、bpftrace | `nr_dirty`/`nr_writeback`、`wb_workfn`、`do_writepages`、`writeback:writeback_dirty_folio` |
| 块层 | `blktrace`/`btt`、bpftrace、`iostat` | `submit_bio`、`blk_mq_submit_bio`、`block:block_rq_insert/issue/complete`、`aqu-sz` |
| 驱动 | `perf`、NVMe tracepoint | `nvme_queue_rq`、`nvme_complete_rq`、`nvme:nvme_sq` |
| 设备 | `iostat`、SMART、厂商遥测 | `r_await`/`w_await`/`%util`、`WA`、温度降频 |

**回答这题的价值**：能立刻把"某层慢"和"用哪个工具看"对应起来，是"了解 I/O 栈"最实用的体现。仓库 `c/doc/RAEDME.md` 实验5 给的函数清单（`blk_mq_submit_bio`/`submit_bio`/`ext4_file_read_iter`/`generic_file_read_iter`/`page_cache_sync_readahead`/`find_get_page`/`copy_user_enhanced_fast_string`）正好横跨 4 层，可以直接拿来当例子。

### 29. 怎么用 eBPF 把一次 I/O 在栈里各层的耗时拆开？`[深水区]`
**答**：思路是**在每个层的进出点打时间戳，按 request/bio 指针关联，算差值**：

```
t0  vfs_read 进入            ← syscall 层耗时 = t1 - t0
t1  filemap_read 里拿到 folio ← 页缓存查找/命中判断
t2  readahead/bio 提交        ← 缓存未命中的处理
t3  block_rq_issue            ← 排队时间 = t3 - t2（plug/scheduler 里等了多久）
t4  block_rq_complete         ← 设备服务时间 = t4 - t3  ← 这一段才是"盘"的时间
t5  bio_end_io / folio_end_read ← 回传与唤醒开销
t6  copy_page_to_iter 结束     ← CPU 拷贝时间
```

实现方式：① **tracepoint**（稳定、有 ABI 保证）：`block:block_rq_insert`（进队列）、`block:block_rq_issue`（下发）、`block:block_rq_complete`（完成）、`writeback:*`；② **kprobe/kretprobe**（灵活但要跟版本）：挂在 `submit_bio`、`nvme_queue_rq`、`blk_mq_end_request`；③ 关联键：block tracepoint 里有 `dev + sector` 和 `rq` 指针，可以据此把 bio/request 串起来，或直接用 `request` 指针做 key；④ 现成工具：`biosnoop`（按进程看 bio 延迟）、`biolatency`（延迟直方图）、`biostacks`（调用栈）、`ext4dist`/`xfsdist`（按操作类型分位延迟）、`offcputime`（看进程是否在等 I/O 睡掉）。

**两个诚实的坑**：① **buffered I/O 的归属很模糊**——页缓存命中时根本没有块层事件，"延迟"就是查找+拷贝；未命中时"提交者"和"等待者"可能是不同进程（预读提交者是 A 进程、等页的是 B 进程）；② **IOPOLL/线程化中断会改变完成路径**，同一个 hook 在不同配置下代表不同的东西。所以**先确认 I/O 走了哪条路径（buffered/direct/polled），再解释数据**，这是区分"会用工具"和"看懂工具"的地方。

### 30. `io_uring` 在栈里的位置是什么？它绕过了什么、没绕过什么？`[深水区]`
**答**：**`io_uring` 只改变了"I/O 请求如何进入和完成"，没有改变"请求要走哪些层"**——这是这题唯一的正确框架。

**架构位置**：它替代的是**②系统调用层**，位置和 `pread`/`pwrite` 平级：应用把请求写进 SQ ring（用户态内存写，不陷内核），内核消费后**仍然调用同一套 `->read_iter`/`->write_iter`**（`io_read()`/`io_write()` 最终走 `vfs_read` 那套）。所以：

- **没绕过的**：VFS、文件系统 extent 映射、页缓存（除非 `O_DIRECT`）、块层（合并/plug/调度/cgroup 限流）、blk-mq、驱动、DMA、设备——**一层都没少**。
- **绕过/优化掉的**：① 每次 I/O 的系统调用与用户/内核切换（改成了共享内存环）；② `SQPOLL` 模式下连提交系统调用都省了（内核线程轮询 SQ，代价是一个核常驻）；③ `IOPOLL` 模式下省掉完成中断（主动轮询 CQ）；④ 注册缓冲区（`REGISTER_BUFFERS` + `*_FIXED`）省掉每次 I/O 的 `get_user_pages`，配合 `O_DIRECT` 可以做到**零 CPU 拷贝**（第 26 题的 bounce 问题也一并消失）。
- **一个容易漏掉的点**：不是所有操作都能在提交上下文中直接完成——**可能阻塞的操作（包括 buffered I/O、非 `O_DIRECT` 的读、文件打开等）会被 `io-wq`（io_uring 的内核工作队列）接管到异步上下文中执行**（`IOSQE_ASYNC` 也可强制）。所以"用了 io_uring 就是全异步"是错的：buffered I/O 在 `io_uring` 里仍可能退化成工作线程里的同步操作。

**收口**：如果面试官追问"那 io_uring 到底优化了什么"，标准答案是：**它优化的是"每 I/O 的固定开销"（系统调用、上下文切换、页 pin、中断），不是"每 I/O 的物理成本"（设备延迟、DMA 时间）**。这也解释了为什么本仓库单线程小负载下 0.038s → 0.014s 的收益（省的是系统调用），而设备侧的延迟一分都没少。

---

## 附 A：白板速答模板（被要求"说说 Linux I/O 栈"时用）

按这 6 步讲，30 秒到 2 分钟都能伸缩：

1. **分层**：用户 → 系统调用/VFS → 文件系统 → 页缓存 → 块层(bio/request/blk-mq) → 驱动(NVMe) → 设备。
2. **两个抽象**：VFS 用 `file/dentry/inode/address_space` 把"文件"统一；块层用 `bio/request` 把"设备"统一。
3. **两条分叉**：`O_DIRECT` 跳过页缓存、`mmap` 直接映射页缓存。
4. **缓存的职责**：页缓存决定"要不要碰盘"，写缓存/FTL 决定"多久落盘"。
5. **三个开销点**：页缓存拷贝（`copy_user_*`）、块层排队（`aqu-sz`/plug/调度）、设备延迟（`await`）。
6. **两类语义**：性能（IOPS/带宽/延迟分位数）与正确性（`fsync`/`PREFLUSH`/`FUA`/PLP）是两个正交维度。

## 附 B：自检表

能**不查资料**答出下面这些，简历上的"了解 Linux I/O 栈"就站得住：

- [ ] 完整说出 buffered read 的调用链（到 `submit_bio` 和 `copy_page_to_iter`）
- [ ] 说清 `O_DIRECT` 跳过了什么、没跳过什么
- [ ] 说清 `folio`/`address_space`/XArray 的关系和 `folio` 的动机
- [ ] 说出 `bio` 的 5 个关键字段和 `REQ_PREFLUSH`/`REQ_FUA` 的含义
- [ ] 说清 blk-mq 的 ctx/hctx/tags 结构和它替代单队列的原因
- [ ] 说清 `plug` 是什么、为什么它影响合并效率
- [ ] 说清 cgroup 限流在 buffered write 上"延迟生效"的坑
- [ ] 说清完成路径：中断 → `blk_mq_end_request` → `bio_endio` → 唤醒 → 拷贝
- [ ] 说清 `io_uring` 只改变了"进入/完成方式"，没减少任何一层
- [ ] 能给每一层配一个观测工具和一个典型热点函数

**难度分布**：第 1–4 题 ⭐（必须秒答）；第 5–9 题 ⭐⭐（VFS 抽象）；第 10–18 题 ⭐⭐⭐（页缓存/文件系统，区分度最高）；第 19–27 题 ⭐⭐⭐⭐（块层/驱动，深水区）；第 28–30 题 ⭐⭐⭐⭐⭐（观测与 io_uring 定位，答好能直接翻转面试评价）。
