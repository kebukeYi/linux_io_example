# IOlab 项目面试 100 问

> 面向「存储 / 数据库内核 / 后端基础架构 / Linux 系统」方向岗位。
> 题目全部基于本仓库**真实代码与实测数据**，按 8 个主题、难度循序渐进排列（第 1 题最容易，第 100 题最难）。
> 每题都给了参考答案；标注 `[工程题]` 的是偏生产落地、事故排查、方案取舍的问题。

---

## 一、项目速览（面试开场 3 分钟版）

**一句话**：IOlab 是一套「最小可复现」的 Linux 存储 I/O 实验集，用 C 和 Go 两种语言，把 **系统调用 → 页缓存 → O_DIRECT → fsync → 块层 → 设备** 这条链路的行为差异，用 `strace` / `fio` / `iostat` / `perf` 的实测数据钉死。

**目录**：

```
c/src/    sync_read.c        1024 次 4KB pread64（同步逐个读）
          io_uring_read.c    同一负载改用 io_uring 批量提交
          direct_read.c      O_DIRECT 读，缓冲区 4K 对齐
          no_direct_read.c   普通缓冲读（走页缓存）
c/doc/    fio/iostat/mmap/perf 四个配套实验 + 讲解
go/exp1_fsync/     fsync 四种节奏（none/once/group/every）的代价
go/exp2_odirect/   Buffered vs O_DIRECT × 有无 fsync 的四阶段对比
go/doc/            Go 实验运行手册 + 内核脏页参数调优
```

**四组核心实测结论（面试可直接引用）**：

| 实验 | 数据 | 结论 |
|---|---|---|
| sync vs io_uring | 1026 次 `pread64` vs 47 次 `io_uring_enter`；0.038s vs 0.014s | io_uring 把系统调用次数降一个数量级 |
| fsync 节奏 | none 0 次 / once 1 次 / group 200 次 / every 20000 次；every 慢一个数量级（0.4 MB/s，3.5 分钟） | 持久性是用吞吐买来的 |
| Buffered vs O_DIRECT | A 583 MB/s、B 0.4 MB/s、C 0.4 MB/s、D 16.4 MB/s | **O_DIRECT 解决"谁来缓存"，fsync 解决"何时落盘"，两者正交** |
| mmap 随机读 | IOPS 2281、avg 435us、P99 1.9ms、P999 10ms、majflt 45628 | 尾延迟才是业务痛点 |

> **本仓库修复过的一个真实发现**：`sync_read` 的 `strace -c` 显示 1026 次 `pread64`，比循环的 1024 次多 2 次。经核对，这 2 次是 glibc 启动阶段读取 `/usr/lib/x86_64-linux-gnu/libc.so.6` 程序头（784 字节 @ offset 64），发生在 `main` 之前。**系统调用计数不能直接等同于业务次数，必须扣掉运行时启动开销。**（第 5 题）

---

## A 组｜入门：系统调用与缓冲 I/O（第 1–15 题）

### 1. 这个项目到底在验证什么？
**答**：验证一条核心命题——**Linux 存储 I/O 的性能和正确性，取决于"数据在哪个层级被缓存"和"何时被强制落盘"这两件正交的事**。项目用四组对照实验分别证明：① 系统调用方式影响开销（sync vs io_uring）；② 页缓存决定写的"快"（Buffered 583 MB/s）；③ `fsync` 决定写的"稳"（every 模式 0.4 MB/s）；④ `O_DIRECT` 决定"谁来管缓存"（避免大内存机器被脏页拖垮）。

### 2. `direct_read.c` 和 `no_direct_read.c` 代码上的唯一区别是什么？
**答**：只有 `open` 的 flags 多了一个 `O_DIRECT`：

```c
open("/tmp/testfile", O_RDONLY);            // no_direct_read.c
open("/tmp/testfile", O_RDONLY | O_DIRECT); // direct_read.c
```

其余代码（`posix_memalign` 对齐分配、`pread`、长度）完全一样。**面试点**：一个 flag 就能让内核走完全不同的数据路径，这类"看起来一样、行为完全不同"的开关是排查 I/O 问题的关键。

### 3. 既然对齐只对 O_DIRECT 是硬要求，为什么 `no_direct_read.c` 也用了 `posix_memalign`？
**答**：有两个原因。① 让两个程序**严格可比**：除了 `O_DIRECT` 这一个变量，其它条件全部对齐，否则测出的差异无法归因。② 便于"改一个 flag 就切换路径"的日常调试。代价是 buffered 路径下 4K 对齐没有任何收益（页缓存拷贝本来就不要求对齐），属于实验设计的刻意冗余。**反过来说**：如果这是生产代码，buffered 路径用 `posix_memalign` 就是无意义的复杂度，应该用普通 `malloc`。

### 4. `pread` 和 `read` 的区别？`io_uring_read.c` 里为什么用 `readv` 而不是 `read`？
**答**：`read` 使用并推进文件偏移量（fd 上有状态，多线程共享 fd 时需要额外加锁）；`pread` 带显式 offset、**不改变文件偏移**，天生线程安全，所以同步版的并发读用它。`io_uring` 版用 `io_uring_prep_readv` + 单个 `iovec`，功能上等价于 `pread`，但 `iovec` 是零拷贝/分散聚集（scatter-gather）接口的基础，将来要做"一次提交读多个不连续块"（`readv` 多 iovec、或 `IORING_OP_READ_FIXED`）时不用改调用形态。**面试点**：这里每个 SQE 只放 1 个 iovec 是"预留扩展能力"，不是最优写法——同样功能 `io_uring_prep_read` 更直观。

### 5. `sync_read.c` 循环 1024 次，为什么 `strace -c` 显示 1026 次 `pread64`？多余的 2 次从哪来？
**答**：另外 2 次是**进程启动阶段**的开销，发生在 `main` 之前——glibc/动态链接器在读取 `/usr/lib/x86_64-linux-gnu/libc.so.6` 的 ELF 程序头（784 字节 @ offset 64），用 `pread64` 实现。我在本仓库实测确认了这两次调用：

```
pread64(3</usr/lib/x86_64-linux-gnu/libc.so.6>, "\6\0\0\0\4\0\0\0@\0\0\0...", 784, 64) = 784
pread64(3</usr/lib/x86_64-linux-gnu/libc.so.6>, ..., 784, 64) = 784
pread64(3</tmp/testfile>, ..., 4096, 0) = 4096      ← 从这里开始才是业务
```

`io_uring_read.c` 的 strace 表里同样有 2 次 `pread64`，可以交叉印证。**工程结论**：做系统调用计数对比时，要么扣掉启动开销，要么用 `strace -c -f` 并关注量级差异（1024 vs 47 是 20 倍，2 次的噪声无关紧要），不要在小负载上做 1%~2% 级别的"优化"归因。

### 6. 项目里块大小为什么选 4KB？
**答**：4KB 是 **x86 页大小**，也是数据库/存储引擎最典型的随机 I/O 单元（InnoDB page、RocksDB block 默认 4K）。选它有双重意义：① 单块正好一页，能直接对应"一次缺页=一次磁盘 I/O"的分析；② 避免跨页/跨块边界带来的额外放大。反例：如果选 1KB，`mmap` 和 `O_DIRECT` 的对齐约束就失去代表性；如果选 1MB，测的就不是 IOPS 而是带宽。

### 7. `strace -c` 表里的 `% time`、`seconds`、`usecs/call` 该怎么读？
**答**：`calls` 是调用次数；`seconds` 是该系统调用**在内核态消耗的总时间**（不含用户态）；`usecs/call` = seconds/calls，是平均单次延迟；`% time` 是它占所有 trace 到的系统调用总时间的比例。以 `sync_read` 为例：`pread64` 1026 次、0.0373s、36us/次、占 99.24%——说明程序时间几乎全花在 I/O 系统调用上。**注意**：`strace` 只统计内核态时间，用户态 CPU（比如拷贝前的计算）不在这里，所以 `% time` 之和接近 100% 不代表程序总耗时。

### 8. `exp2` 的 A 阶段，源码注释写 13ms / 583 MB/s，但 `strace.md` 里是 375ms / 20.8 MB/s，差 30 倍怎么解释？
**答**：因为 `strace.md` 里那次是**在 strace 下跑的**。`strace` 基于 `ptrace`，每一次系统调用都会让被跟踪进程停下来、让跟踪进程处理一次，代价是**两次上下文切换 + 一次 ptrace 往返**（实测每次约 100~200us）。A 阶段有 2000 次 `pwrite64`，2000 × 90us ≈ 180ms，正好把 13ms 的量级抬到 375ms 左右。**工程结论**：① 用 strace 比较"次数"和"调用形态"是可靠的，比较"绝对性能"是不可靠的；② 性能数据必须在无 trace、可复现的环境下采集。

### 9. 准备测试文件时，为什么用 `/dev/urandom` 而不是 `/dev/zero`？什么时候反而该用 `/dev/zero`？
**答**：`/dev/urandom` 产出**不可压缩**的随机数据，模拟真实业务脏数据；`/dev/zero` 全零数据有两个陷阱——① 部分 SSD 主控会做零块压缩，测出的写带宽虚高；② 稀疏文件（sparse）可能不实际占盘，读起来是"算出来的"而不是"读出来的"。所以做 **IOPS / 延迟 / 随机读** 测试必须用 urandom（仓库 `c/doc/RAEDME.md` 就是两种都给了）。反过来，纯做**顺序带宽打到顶**的压力测试时用 `/dev/zero` 更快更省事，因为此时关心的是链路吞吐上限，不是数据真实性。

### 10. `echo 3 | sudo tee /proc/sys/vm/drop_caches` 里的 3 是什么？
**答**：是一个位掩码：`1` = 丢弃 page cache，`2` = 丢弃 dentry/inode 缓存（slab），`3` = 1|2 两者都丢。项目里用它来构造"冷缓存"起点，这样 `direct_read` 和 `no_direct_read` 的对比才有意义——否则第二次运行的数据已经在页缓存里，两个程序都变成纯内存操作。**注意**：`drop_caches` 是全局的、会影响同机其它进程，生产机器上慎用；且它只丢**干净页**，脏页会先被回写。

### 11. 为什么 README 要求必须在 Linux 原生目录跑，不能放 `/mnt/` 或 Windows 盘符？
**答**：WSL2 里 `/mnt/c` 走的是 `drvfs`/9p 协议映射到 Windows NTFS，它**不是真实的 Linux 文件系统语义**：`O_DIRECT` 可能不支持或语义失真（对齐要求、缓存行为都不同）、`fsync` 的持久化保证不成立、`strace` 看到的 I/O 行为也不代表真实磁盘。所以两个 Go 程序都有显式告警：

```go
if strings.HasPrefix(*dir, "/mnt/") {
    fmt.Println("!! 警告: drvfs 不支持 O_DIRECT 或语义失真")
}
```

正确做法是 `cp -r` 到 `~/`（WSL 里的 ext4，位于 VHDX 虚拟磁盘）再跑。**工程题**：这条经验同样适用于容器——挂载卷（overlayfs、NFS、云盘 CSI）上的 I/O 语义和裸设备不同，性能测试必须打在真实介质上。

### 12. 这四个 C 程序里，谁的返回值检查漏了？工程上有什么风险？
**答**：`sync_read.c` 完全没有检查 `open`/`pread`/`posix_memalign` 的返回值；`direct_read.c` 和 `no_direct_read.c` 检查了 `open` 和 `pread<0`，但 `posix_memalign` 的返回值没检查（它不设 `errno`，只返回错误码，`perror` 在这里是错的用法）。风险：`pread` 短读（返回 < 4096）时程序会静默拿到半块脏数据；`open` 失败时 `fd=-1` 会让后续所有 `pread` 返回 `EBADF` 而程序仍然"成功"退出 0。**修法**：

```c
if (posix_memalign(...) != 0) { /* 报错退出 */ }
ssize_t n = pread(fd, buf, BLOCK_SIZE, off);
if (n != BLOCK_SIZE) { /* 短读/错误：重试或报错 */ }
```

Go 版本在这点上明显更规范——所有 `Write`/`Pwrite`/`Fsync` 的 error 都检查并上报。

### 13. "4K 对齐"在 buffered 和 O_DIRECT 下分别是硬要求还是建议？
**答**：**buffered 下完全不需要对齐**——数据要经页缓存中转，内核自己处理任意 offset/length（还会做 read-modify-write 补齐整页）。**O_DIRECT 下是硬要求**，且要求的是三条：缓冲区**地址**、文件**偏移**、传输**长度**都要对齐到设备的逻辑块大小（现代 SSD 通常 512 或 4096；某些文件系统还会要求对齐到自己的 block size）。这三条任意一条不满足，内核直接返回 `EINVAL`，不会"尽力而为"。项目 `exp2` 里被注释掉的"阶段0"就是专门演示这个拒绝行为的。

### 14. 页缓存（Page Cache）到底是什么？怎么用 `free -m` 看出来？
**答**：页缓存是内核用**空闲内存**缓存磁盘文件内容的一层，单位是 page（4KB），由 `address_space` 对象按文件索引管理。`free -m` 里的 `buff/cache` 列就是它（含 Buffer Cache 和可回收 slab）。项目里的验证方法非常直接：

```bash
echo 3 | sudo tee /proc/sys/vm/drop_caches   # 清空
free -m                                       # 记录 buff/cache
./direct_read                                 # O_DIRECT 读
free -m                                       # buff/cache 几乎不变
./no_direct_read                              # 缓冲读
free -m                                       # buff/cache 明显增长
```

**注意**：`buff/cache` 增长不是内存泄漏，它属于"可回收内存"，应用需要内存时内核会回收它——但回收的代价正是第 23~24 题要讨论的事。

### 15. 同一个文件，第一次读和第二次读为什么速度差几个数量级？
**答**：第一次读要真正访问设备（冷缓存，走 `readahead` + `submit_bio` + 设备 I/O），第二次读命中页缓存，只是从内核内存拷贝到用户内存（`copy_user_enhanced_fast_string`），延迟从毫秒级降到微秒级。**这正是本系列实验最容易被"骗"的地方**：如果不做 `drop_caches` 或冷启动控制，测出的随机读 IOPS 可能是纯内存指标。fio 里对应的是 `--direct=1`（绕过缓存，每次真读盘）或 `--invalidate=1`（每轮失效），以及 `--ramp_time`/`--warmup` 用来排除预热阶段。

---

## B 组｜页缓存、回写与内存压力（第 16–30 题）

### 16. Page Cache 和 Buffer Cache 现在还是一回事吗？
**答**：在 Linux 2.4 之后它们已经**统一**成同一套页缓存框架（`address_space` + `page`），只是概念上沿用了两个名字：面向文件内容的叫 Page Cache，面向块设备块缓冲的叫 Buffer Cache。`free -m` 的 `buff/cache` 是合并统计。所以读文件时占用的是同一块内存池，不会出现"文件缓存和块缓存各占一份、互相不共享"的情况。

### 17. 请画出 `exp2` A 阶段一次 `write()` 的完整链路。
**答**：

```
用户态 pwrite(4KB)
  → sys_write / ksys_write
  → vfs_write → file->f_op->write_iter = ext4_file_write_iter
  → iomap/ext4 buffered 路径：generic_perform_write
  → grab_cache_page_write_begin 找到/分配 page cache 页
  → copy_from_user(copy_user_enhanced_fast_string)  ← 唯一一次数据拷贝
  → set_page_dirty，返回（此刻数据只在内存，未落盘）
...稍后（异步，由内核 flusher/kswapd 触发）...
  → writeback：ext4_writepages → submit_bio
  → blk-mq → I/O 调度器 → nvme_queue_rq → DMA → SSD
```

关键点：`write()` 返回时数据**只在页缓存里**，这就是 A 阶段 583 MB/s（≈内存拷贝速度）的来源，也是"此时断电会丢"的原因。

### 18. 脏页什么时候真正落盘？谁来管这件事？
**答**：三条触发路径，谁先到谁生效：① **时间**：`vm.dirty_expire_centisecs`（默认 3000 = 30 秒）到期的脏页会被后台回写线程（`flush-<maj>:<min>`，旧叫 `pdflush`）写出；② **水位**：脏页占比超过 `vm.dirty_background_ratio` 就启动后台回写，超过 `vm.dirty_ratio` 则**阻塞发起写的进程**做同步回写；③ **显式**：`fsync`/`fdatasync`/`msync`/`sync`。另外 `vm.dirty_writeback_centisecs`（默认 500 = 5 秒）控制后台线程的唤醒周期。

### 19. `dirty_ratio` / `dirty_background_ratio` / `dirty_expire_centisecs` 是什么关系？`go/doc/README.md` 里为什么设成 1 / 20 / 500？
**答**：三个参数是"预警线 / 恐慌线 / 保质期"：

| 参数 | 仓库取值 | 作用 |
|---|---|---|
| `vm.dirty_background_ratio` | 1 | 脏页超过内存 1% 就**启动后台回写**（预警） |
| `vm.dirty_ratio` | 20 | 脏页超过 20%，发起写的进程被**同步阻塞**直到回写（恐慌） |
| `vm.dirty_expire_centisecs` | 500 | 脏页最多活 5 秒就强制回写（加速淘汰） |

设计意图：把"预警"提前到 1%，让内核尽早、平滑地开始刷盘，**避免脏页瞬间冲到恐慌线**导致应用进程被同步回收长时间卡死。`dirty_expire_centisecs=500` 则缩短单页最大滞留时间，让回写更细水长流。

### 20. 那为什么不干脆把 `dirty_ratio` 调到很小（比如 1%）？调小的代价是什么？
**答**：代价是**吞吐下降和写放大上升**。小 `dirty_ratio` 意味着缓冲区很小，应用写很快、刷盘跟不上时就会被频繁阻塞（相当于把异步写退化成准同步写）；同时回写批次变小，设备失去写合并的机会，SSD 上表现为更多 4K 写和更高的写放大。这是典型的**"延迟的确定性"和"吞吐的最优"不可兼得**：仓库里 `dirty_background_ratio=1` 走的是"早刷、稳延迟"，而默认的 `10/20` 走的是"晚刷、高吞吐"。正确做法是按业务 SLA 定，而不是无脑调小。

### 21. `exp1` 里 `FADV_DONTNEED` 是干什么的？为什么必须先 `fsync` 再 `fadvise`？
**答**：`posix_fadvise(fd, off, len, FADV_DONTNEED)` 是**建议内核把这段区间从页缓存里丢出去**，避免 78MB 甚至更大的一次性文件长期占着内存、把别的有用数据挤掉。仓库里的封装：

```go
func EvictCache(f *os.File, offset, length int64, advise bool) error {
    if err := f.Sync(); err != nil { return err }   // 1. 先强制落盘
    if advise {
        return unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED) // 2. 再丢缓存
    }
    return nil
}
```

**必须先 fsync**：对脏页执行 `FADV_DONTNEED` 时，内核不会"丢掉未落盘的数据"（那等于丢数据），它会先把脏页回写再丢弃；如果不先显式 fsync，你就无法确定"丢缓存"这个动作到底有没有触发一次隐式写盘，程序行为变得不可预测。先 fsync 把语义钉死：**刷盘是显式的，丢缓存是纯释放**。

### 22. `FADV_DONTNEED` / `FADV_SEQUENTIAL` / `FADV_RANDOM` / `FADV_WILLNEED` 各自适用什么场景？
**答**：

- `FADV_DONTNEED`：一次性大文件读完/写完立刻丢弃，别占内存（本仓库用法）。对应的内存回收耗时是**纯 CPU（解映射+TLB 刷新）1~5ms**，且只影响本进程。
- `FADV_SEQUENTIAL`：顺序访问提示，内核**把预读窗口加倍**。适合日志回放、备份扫描。
- `FADV_RANDOM`：随机访问提示，**关闭预读**。适合 4K 随机读的关键值查询——否则预读会把后面 128KB 白白读进内存，既浪费带宽又污染缓存。
- `FADV_WILLNEED`：马上要用，**主动触发预读/预取**到页缓存。适合启动阶段预热索引。

### 23. 清理 1GB 文件缓存有几种方式？耗时差异说明了什么？
**答**：仓库 `strace.md` 里给了三行对比，这是很好的"分层延迟"教材：

| 方式 | 主要耗时点 | 量级 | 是否卡应用 |
|---|---|---|---|
| 主动 `fadvise(DONTNEED)` | 解页表映射 + TLB 刷新（纯 CPU） | 1~5 ms | 只短暂暂停调用线程 |
| 被动 `kswapd`（异步回收） | 扫描 LRU + 锁开销 | 50~200 ms | 不卡应用，但吃 CPU |
| 被动 `direct reclaim`（同步回收） | 扫 LRU + **强制回写脏页 + 等磁盘** | 500ms ~ 数秒 | **应用完全卡死** |

**结论**：同一个"回收 1GB 内存"的动作，因为有没有脏页、是谁触发，成本能差 3 个数量级。**工程的解药是不要让自己落到 direct reclaim 分支上**——主动 `fadvise`，或把 `dirty_background_ratio` 调低让回写提前发生。

### 24. 为什么"128GB 内存 + 1TB 数据的数据库，不加 O_DIRECT 会把整机拖卡"？
**答**：因为 buffered write 的返回速度是内存速度，应用写入速率远超磁盘。脏页会迅速爬升：先越过 `dirty_background_ratio` 触发后台回写（CPU 飙升），再逼近 `dirty_ratio`，此后**任何进程的 write 都会被同步阻塞去等磁盘**，出现数秒级卡顿（thrashing / direct reclaim）。受害的还不只是数据库自己——它把整台机器上所有进程的内存回收都拖下水。`O_DIRECT` 让写**不经过页缓存**，脏页永远不会堆积，内核不需要为它触发回收，代价是单次写变慢（D 阶段 16.4 MB/s）但**延迟稳定、不拖累 OS**。这就是"数据库自己管缓存"的根本动机。

### 25. 预读（readahead）对顺序读和随机读分别意味着什么？
**答**：顺序读时预读是**免费的加速器**：内核检测到顺序模式后把预读窗口逐步放大（最多到 `read_ahead_kb`，默认 128KB），一次 I/O 请求覆盖后续多个块，吞吐大幅上升。随机读时预读是**纯负担**：读进来的后 127KB 基本不会被用到，白白占用带宽、污染页缓存、还可能把小 I/O 放大成大 I/O 变更慢。所以随机读场景要 `FADV_RANDOM` 关掉预读，或者在 fio 里用 `--random_distribution=random` + `--direct=1` 从根上绕开缓存。

### 26. `buff/cache` 一直涨是内存泄漏吗？怎么判断？
**答**：不是。`buff/cache` 是**可回收内存**，内核会"用空闲内存做缓存，用完了再还回去"。判断方法是看 `free` 里的 `available` 列（估算的可用内存，含可回收部分）：如果 `available` 健康、`used` 稳定，`buff/cache` 大只是缓存效率高。真正要警惕的是**不可回收内存增长**（`Slab` 的不可回收部分、匿名页、`mmap` 的脏页）以及 `available` 持续下降——那才可能是泄漏。本仓库里 `exp1` 用 `FADV_DONTNEED` 主动控制缓存占用，属于"主动治理"而非"泄漏修复"。

### 27. 一个大文件顺序扫一遍就想丢掉缓存，工程上有几种做法？怎么选？
**答**：① `posix_fadvise(DONTNEED)` 分区间边读边丢（本仓库做法）：内存占用恒定，延迟最低，但需要自己管理区间；② `fadvise(WILLNEED)` + 读完后 `DONTNEED`：预热后读得快，占内存峰值高；③ `O_DIRECT` 读：从根上不进缓存，但要求对齐、失去预读，顺序读反而可能更慢；④ 直接依赖内核 LRU 自然淘汰：最省事，但不可控，且会污染缓存。**选择依据**：一次性批处理（备份、离线扫描）选 ①；需要重复读的热数据别丢；顺序大扫且不想动代码就 ③。

### 28. "读"为什么也会产生写盘？（脏页不只来自写）
**答**：几种情况：① **读引发预读**，读进来的页是干净的，不产生脏页；但如果读的是**文件系统元数据**（目录、inode），这些页被修改后一样是脏的，会触发元数据回写；② **读之前数据曾被写过但还没回写**，那么读只是命中脏页，脏页仍在，回写照旧；③ **mmap 写映射**：一次"看起来是读"的访问，如果页是写映射且触发 COW，就会产生脏页。所以 `iostat` 上"只有读负载却看到写"时要想到元数据和回写。

### 29. 内存回收和 I/O 互相拖累的机理是什么？内核有哪些缓解手段？
**答**：机理是**正反馈**：脏页多 → 内存紧张 → 回收器需要先回写才能回收 → 回写慢（设备是瓶颈）→ 回收更慢 → 应用分配内存更慢 → 应用变慢、又产生更多脏页。内核的缓解手段包括：`dirty_background_ratio` 提前后台回写、`dirty_expire_centisecs` 限制脏页寿命、**writeback throttling（WBT）** 限制单个设备的回写并发（`/sys/block/<dev>/queue/wbt_lat_usec`）、cgroup v2 的 `io.max`/`io.latency` 做隔离、`memcg` 级别的脏页限流。**工程手段**则更直接：`O_DIRECT`（本仓库 D 阶段）+ `fadvise` 主动丢缓存 + 写批量化（本仓库 group 模式）。

### 30. 怎么量化观测页缓存命中率和回写压力？
**答**：几个常用来源：① `/proc/vmstat`：`pgpgin/pgpgout`（换入换出页）、`pgfault/pgmajfault`（缺页/主缺页，**majflt 高说明在真读盘**）、`nr_dirty`/`nr_writeback`（当前脏页/回写中页数）、`pgsteal_*`（回收量）；② `/proc/diskstats`：按设备看 `rd_ios`/`wr_ios`/`rd_ticks`/`wr_ticks`/`in_flight`；③ `cachestat`（bcc）：直接给出 `HITS`/`MISSES`/`DIRTY` 比例；④ 应用侧用 `mincore(2)` 检查某段映射是否在内存中。本仓库里 mmap 实验的 `majf=45628` 就是 `/bin/fio` 报告里读 `/proc/vmstat` 得到的。

---

## C 组｜持久化：fsync / fdatasync 与提交语义（第 31–45 题）

### 31. `exp1_fsync` 四种模式的核心结论是什么？
**答**：核心结论是"**持久性是用吞吐买来的**"。实测（`strace.md`）：

| 模式 | fsync 次数 | 耗时 | 吞吐 |
|---|---|---|---|
| `none` | 0 | 3.79s | 20.6 MB/s |
| `once` | 1 | 4.18s | 18.7 MB/s |
| `group`（每 100 块） | 200 | 5.83s | 13.4 MB/s |
| `every` | 20000 | **3m31s** | **0.4 MB/s** |

`none` → `every` 差约 50 倍，且 `every` 模式下 `fsync` 占了 79.86% 的内核时间。**面试点**：`none` 到 `once` 只慢 10%，说明"整体刷一次"几乎不要钱；真正的成本来自**刷盘次数**，不是刷盘数据量——这正是"组提交"存在的理由。

### 32. `none` 模式也跑出了 20.6 MB/s，数据去哪了？为什么不是内存速度（GB/s）？
**答**：数据进了页缓存，`write()` 返回即"完成"。之所以只有 20.6 MB/s 而不是几个 GB/s，是因为 `write` 本身仍有开销：每次 4KB 的系统调用（进入/退出内核）、一次 `copy_from_user`、以及**每写一块就产生一个脏页，脏页积累到水位后内核开始在后台回写**，回写与写入争抢 CPU 和 I/O。strace 表里 20002 次 `write` 花了 1.958s，平均 97us/次——这就是"页缓存写入"的真实单价，它比纯内存拷贝贵得多。

### 33. 为什么 `every` 模式慢了一个数量级？fsync 的 14.5ms 花在哪？
**答**：`every` 模式下每写 4KB 就 `fsync`，20000 次共 3 分 31 秒，源码注释给了单价：**每次 fsync 平均 14.5ms**。这 14.5ms 不是在传数据（4KB 传输是微秒级），而是：① 提交**缓存刷新命令（cache flush）**给设备，等设备把易失性写缓存刷到 NAND 颗粒上；② 等待本次 bio 落盘完成；③ 跨 PCIe/驱动的往返延迟。消费级 SSD 的 flush 命令尤其慢（可能几十毫秒），这是 `every` 模式吞吐只有 0.4 MB/s 的根源。

### 34. `fsync` 和 `fdatasync` 的区别？本项目用的是哪个？
**答**：`fsync` 刷**数据 + 文件元数据**（大小、mtime、inode 等）；`fdatasync` 只刷**数据 + 检索数据必需的元数据**（比如文件大小），不影响读取的元数据（如 mtime）可以延后——所以 `fdatasync` 在元数据多的场景更快，语义上对"数据已持久"也够用。本项目用的是 **`fsync`**：Go 的 `f.Sync()` 在 Linux 上就是 `fsync(2)`，`strace.md` 里 `fdatasync` 那一行始终是 0 次可以印证。**工程建议**：日志/WAL 这类"只需保证数据可见"的场景优先 `fdatasync`，能省掉元数据刷新。

### 35. `fsync` 成功返回后，数据一定在 NAND 上吗？`O_DIRECT` 之后呢？
**答**：`fsync` **成功返回**后，数据（和元数据）已经越过设备易失性写缓存、到达持久介质——这正是它比 `fdatasync` 贵的地方，也是它存在的全部意义。相比之下，**`O_DIRECT` 不提供任何持久化保证**：它只绕过内核页缓存，数据可能还停在磁盘自己的 DRAM 易失缓存里，此时断电照样丢。仓库 `exp2` 的注释说得很准："D 阶段 476ms 是把 8MB 推到硬盘缓存"——**O_DIRECT 只是换了缓存层级，没有换来持久性**。（补充：WSL2 里 `/tmp` 底下还有一层 VHDX/virtio-blk 虚拟化，实际语义是相对的，所以这里的绝对数字只能横向对比、不能当设备真值。）

### 36. 为什么 `C`（O_DIRECT + 每块 fsync）没有比 `B`（Buffered + 每块 fsync）快？
**答**：因为两者**瓶颈是同一个**：每写一块都等设备真正落盘。C 省掉的只是"用户→页缓存"那一份内存拷贝和页缓存管理，而瓶颈 100% 在设备的 flush 延迟上（B 21.3s vs C 23.3s，同一个量级）。而且 C 还**丢掉了 buffered 路径的批处理红利**：脏页在页缓存里可以攒起来、由内核合并成大 I/O 再下发（B 的"快写 + 批量慢刷"），C 每个 4KB 都是独立的真实设备 I/O，调度器可合并的余地更小，所以某些机器上 C 甚至比 B 略慢。**结论**：`O_DIRECT` 和 `fsync` 是两个正交维度（谁来缓存 / 何时落盘），组合使用不会互相抵消成本。

### 37. `group` 模式的工程意义是什么？现实中谁这么干？
**答**：`group`（每 100 块 fsync 一次）就是数据库 **组提交（group commit）/ 批量化提交（batched commit）** 的原型：把多次持久化需求攒成一批，用一次昂贵的 `fsync` 覆盖。收益非常直接——本实验里 200 次 fsync、13.4 MB/s，比 `every` 的 0.4 MB/s 快 30 倍以上。真实系统：**InnoDB**（`innodb_flush_log_at_trx_commit=2` 或组提交把并发事务的 redo 合并成一次 `fsync`）、**PostgreSQL**（`commit_delay`/`commit_siblings` 让事务等一小会儿一起刷 WAL）、**RocksDB**（`WAL` 的 `bytes_per_sync` + 多线程 group commit）、**Kafka**（`flush.messages` 攒批）、**etcd/RAFT**（批量 append 后一次 fsync）。核心权衡永远是：**批越大吞吐越高，但最坏情况下的提交延迟越大**（一个事务要等别人）。

### 38. `-fsync group` 时程序打印"fsync调用=0次"，但 strace 显示 200 次。这是 bug 吗？怎么改？
**答**：是 **bug，但只是统计 bug，不影响 I/O 行为**。看代码：`group` 分支调用 `EvictCache(f, offset, sem, true)`，而 `EvictCache` 内部第一件事就是 `f.Sync()`——所以 fsync **确实执行了 200 次**（与 strace 的 200 一致），但循环里只写了 `groupCount++`，没有写 `fsyncCount++`，打印的却是 `fsyncCount`：

```go
if *mode == "group" && i%100 == 0 {
    EvictCache(f, offset, sem, true)  // 里面执行了 f.Sync()
    groupCount++                       // 但计数记到了没被使用的变量上
}
```

而程序自己还打印"strace 表格里的 fsync 列值应该和它一致"——这句话在 group 模式下被自己打脸了。**修法**：`fsyncCount++`（或直接用 `groupCount`），并给 `groupCount` 加 `_ = groupCount` 或干脆删掉，避免"写了没用"的死变量。**面试价值**：这类"埋点与实际路径不一致"的 bug 在生产里非常常见（监控计数和真实调用对不上），排查方法就是**用 strace/perf 去校验埋点**——本仓库的 `strace.md` 恰好扮演了这个校验器。

### 39. 一次 `fsync` 到底向设备提交了什么？PREFLUSH / FUA 是什么？
**答**：`fsync` 的语义是"我此前通过这个 fd 写的所有数据，返回时必须在持久介质上"。实现上，内核（`vfs_fsync_range` → `vfs_fsync`）会调用文件系统的 `fsync` 方法，把该文件所有脏页提交为 bio，并给最后一个 bio 打上 `REQ_PREFLUSH`（刷之前先让设备把写缓存清空）和/或 `REQ_FUA`（Force Unit Access：这一笔必须直接写到介质，不许停在易失缓存）标志。块层/驱动把它翻译成 NVMe 的 Flush 命令或 SCSI 的 SYNCHRONIZE CACHE / 带 FUA 位的写命令。**这就是 14.5ms 的来源**：一次真正的 cache flush 往返。

### 40. 什么是"`fsync` 两次"的问题？原子替换文件为什么要额外 `fsync` 目录？
**答**：有两个经典坑，合起来常被称为"fsync 不够用"：

1. **目录也要 fsync**：把临时文件 `rename` 成目标文件后，`rename` 只改了**目录项**，它属于**目录 inode 的元数据**。只 `fsync` 文件本身不能保证重命名这个目录项持久化，崩溃后可能"新文件内容在、但目录里还是老名字"或"临时文件又冒出来"。正确顺序是：写临时文件 → `fsync(临时文件)` → `rename` → **`fsync(父目录)`**。
2. **需要 fsync 两次 / fsync 返回值不可信**：部分内核/文件系统/驱动组合下，一次 fsync 可能无法可靠地把错误上报或确保落盘（LWN 的经典文章《Ensuring data reaches disk》讨论过相关边界）；更著名的现实事故是 **2018 年 PostgreSQL "fsyncgate"**：Linux 在脏页回写出错后**清除了脏页标记**，导致后续 `fsync` 直接返回成功——数据库以为数据落盘了，其实已丢失。PostgreSQL 后来引入了 `fsync=off` 时的告警和更严格的错误重试策略。

**工程结论**：持久化代码必须按"文件 fsync → rename → 目录 fsync"三件套写，并且**检查 fsync 的返回值并向上传播失败**（不能吞掉 `EIO`）。本仓库实验的价值就在于让你亲眼看到"不 fsync 也能 583 MB/s"——那种快是骗人的。

### 41. `O_SYNC` / `O_DSYNC` / `sync_file_range` / `msync` 的语义分别是什么？
**答**：

- `O_SYNC`：每次 `write` 返回前，**数据和元数据**都落盘（等价于每次写后 `fsync`），最慢。
- `O_DSYNC`：每次 `write` 返回前，**数据 + 必要元数据**落盘（等价于每次写后 `fdatasync`），比 `O_SYNC` 省。
- `sync_file_range`：**只发起回写**、不等待完成、也不刷设备缓存，**不提供持久性保证**。它的用途是"提前把数据推给设备、后面再 fsync 时更便宜"，误当持久化用会丢数据。
- `msync(MS_SYNC)`：mmap 映射区的"fsync"，把映射的脏页回写并等待，语义上最接近 `msync` 版本的 `fsync`；`MS_ASYNC` 只排队。

**面试点**：这四个 API 的差别本质是"**同步范围（数据/元数据）** × **是否等待设备**"。选错不会立刻报错，只会在掉电时暴露。

### 42. 原子替换一个配置文件/快照，完整步骤是什么？
**答**：

```c
fd = open("/dir/.tmp", O_WRONLY|O_CREAT|O_TRUNC, 0644);
write(fd, data, len);
fsync(fd);                     // 1. 数据+元数据落盘
close(fd);
rename("/dir/.tmp", "/dir/real");   // 2. 原子替换（同文件系统内）
dfd = open("/dir", O_RDONLY|O_DIRECTORY);
fsync(dfd);                    // 3. 目录项落盘 ← 最容易漏
close(dfd);
```

要点：① `rename` 在同一文件系统内是原子的，**跨文件系统会退化成"拷贝+删除"，不再原子**；② 第 3 步的目录 `fsync` 不能省，否则崩溃后可能丢失重命名（见第 40 题）；③ 若还要求"读者永远看到完整内容"，配合 `O_TMPFILE` + `linkat` 或先写临时名再 rename；④ 如果要保证**顺序可见性**，还可以用 `RENAME_EXCHANGE`/`renameat2`。

### 43. 组提交的 batch 大小和提交延迟怎么权衡？参数怎么定？`[工程题]`
**答**：设 batch 为 N、单次 fsync 成本为 T_fsync、数据量固定。吞吐 ≈ N·块大小 / (N·T_write + T_fsync)，N 越大越接近"纯写吞吐"，但**最坏提交延迟 ≈ N·T_write + T_fsync**（排队等一个批次）。定参方法是**从延迟 SLA 反推**：若要求 P99 提交延迟 ≤ 10ms，而实测 T_fsync = 1ms、单块写 20us，则 N ≲ 450。真实系统通常不写死 N，而是用**自适应策略**：时间窗口（`innodb_flush_log_at_timeout`、PostgreSQL `commit_delay` 微秒级等待）+ 并发度（`commit_siblings`：并发事务够多才等），或者干脆用"有事务在等就立刻刷、没事务就攒着"的事件驱动组提交。本仓库 `exp1` 的 `-fsync group` 就是最简单的固定 N=100 版本，可以拿它扫一遍 N=1/10/100/1000 画出"吞吐-延迟"曲线，作为组提交调参的入门实验。

### 44. 多个线程同时 `fsync` 同一个文件会怎样？
**答**：可以并发调用，但**收益极低且可能互相拖累**：`fsync` 的语义是"刷这个 fd 对应文件的**所有**脏页"（不是只刷某线程写的那部分），所以两个线程同时 fsync 大概率刷的是同一批页，第二个基本是白等；内核内部有 `inode->i_rwsem`/`mapping` 级别的同步与写回状态管理，一次 fsync 可能把别人刚写的页也刷掉，导致"重复劳动"。工程做法是**单点串行 + 组提交**（见第 43 题）：由一个后台提交线程或一个"第一个到达者负责刷、其余人等结果"的机制统一发 fsync，把 N 个 fsync 合并成 1 个。这也是为什么 `exp1` 的 `group` 模式比 `every` 快 30 倍——不是在优化 fsync 本身，而是在**减少 fsync 次数**。

### 45. 数据库 WAL 刷盘策略 always / every N / OS 缓冲，怎么选？`[工程题]`
**答**：三种对应不同"丢多少数据可接受"：

| 策略 | 语义 | 掉电丢失窗口 | 代表 |
|---|---|---|---|
| `always`（每次提交 fsync） | 提交即持久 | 0 | InnoDB `flush_log_at_trx_commit=1`、PG `synchronous_commit=on` |
| `every N`（组提交/时间窗口） | 批量刷 | ≤ 一个批次（ms 级） | InnoDB `=2`、PG `commit_delay` |
| `OS 缓冲`（不主动 fsync） | 交给内核回写 | 最长 `dirty_expire_centisecs`（默认 30s） | InnoDB `=0` |

选择的依据**不是性能，而是业务对 RPO 的要求**：金融账务用 `always`（本仓库 `every` 模式就是它的代价实证：0.4 MB/s）；日志类、可重放类用组提交；离线批处理才敢用 OS 缓冲。另外注意：`always` 也不能只靠数据库，**盘上的易失写缓存必须关掉（或盘支持断电保护 PLP）**，否则"数据库 fsync 了"但盘自己丢了缓存，仍然丢数据——这就是为什么服务器 SSD 有电容（Power Loss Protection）。

---

## D 组｜O_DIRECT 与自管缓存（第 46–58 题）

### 46. `O_DIRECT` 的对齐规矩有几条？`exp2` 的"阶段0"为什么会被 `EINVAL` 拒绝？
**答**：三条硬要求——**缓冲区起始地址**、**文件偏移**、**传输长度**都必须对齐到设备逻辑块大小（通常 512 或 4096；部分文件系统还要求对齐到自己的 fs block）。阶段0 的代码是：

```go
bad := make([]byte, 4096+1)   // 故意 +1，让指针错开
_, err = syscall.Pwrite(fd, bad, 0)
// → EINVAL (Invalid argument)
```

`make([]byte, 4097)` 会走到 Go 内存分配器的小对象路径，**起始地址通常只有 8 字节对齐**，不满足 4096 对齐，于是内核直接拒绝，连"尽力而为"的余地都没有。**工程要点**：不要靠"碰巧对齐"（Go 的小对象分配有时会给到页对齐地址，运气好阶段0 反而"成功"了）——正确做法是像 `allocAligned` 那样**显式对齐并断言校验**，项目里那段被注释掉的 `addr%4096 == 0` 校验应该保留在调试版本里。

### 47. 哪些文件系统/存储支持 `O_DIRECT`？不支持的时候怎么办？
**答**：主流本地文件系统都支持（ext4、xfs、btrfs 基本可用、f2fs）。**不支持的典型场景**：某些网络/分布式文件系统（NFS、FUSE、部分 CIFS 实现）、`/mnt` 下的 drvfs、tmpfs（无后端设备）、以及某些容器文件系统。不支持时的表现通常是 `open()` 直接返回 `EINVAL`，或者打开成功但行为退化。**处理办法**：① 探测能力——打开时若 `EINVAL` 就降级为 buffered（`exp2` 里的处理就是捕获 `EINVAL` 后打印"文件系统不支持 O_DIRECT"并跳过）：

```go
if cfg.direct && err != nil && strings.Contains(err.Error(), "EINVAL") {
    fmt.Printf("跳过: 文件系统不支持 O_DIRECT\n")
    continue
}
```

② 用 `open(O_DIRECT)` 的失败而不是 `statfs` 的 magic number 去判断（更可靠）；③ 上层把"是否 direct"做成可配置项，而不是硬编码——因为部署环境（本地盘/云盘/网络存储）差异很大。

### 48. `allocAligned` 为什么多分配一个 `blockSize`？有没有更干净的做法？
**答**：

```go
func allocAligned(n int) []byte {
    raw := make([]byte, n+blockSize)                    // 多留一个块
    addr := uintptr(unsafe.Pointer(&raw[0]))
    off := int((blockSize - addr%blockSize) % blockSize) // 向前对齐的偏移
    return raw[off : off+n]                              // 返回对齐的子切片
}
```

多分配一个 `blockSize` 是为了保证**无论起始地址落在页内哪个位置，都有足够空间向前挪到下一个对齐边界**（最坏情况偏移 `blockSize-1` 字节）。返回子切片而不是新分配，所以 GC 会跟踪整个底层数组，生命周期安全。**更干净的做法**：① C 里用 `posix_memalign`/`aligned_alloc`（但 `posix_memalign` **不保证**对齐 `O_DIRECT` 需要的设备 block size，只能保证你给的对齐值，所以要用 4096）；② 用 `mmap` 分配（页对齐，且可以用 `MADV_HUGEPAGE` 拿大页）；③ 高版本 Go 可以用 `sync.Pool` 复用对齐缓冲，避免每次实验都分配；④ 生产里最省事是**分配一次、全局复用**（`exp2` 的 `blockBuf` 就是这么做的）。

### 49. `O_DIRECT` 真的"没有缓存"吗？它绕过了谁、没绕过谁？
**答**：它绕过的是**内核页缓存（Page Cache）**，没有绕过的是**设备/存储控制器自己的缓存**（SSD 的 DRAM、RAID 卡的 cache、云盘的分布式缓存）。所以严格说 `O_DIRECT` 是"绕过内核缓存"，不是"无缓存"。它带来的直接收益是**少一次内存拷贝**：buffered 读是 `设备 → 页缓存（DMA）→ 用户缓冲（CPU 拷贝）` 两步，`O_DIRECT` 读是 `设备 → 用户缓冲（DMA）` 一步，前提是地址对齐（否则内核要引入 bounce buffer，把拷贝又还回来）。**代价**：失去预读、失去跨进程缓存共享、失去写合并，且必须自己处理对齐和部分读。所以"O_DIRECT 更快"是错的——它换的是**内存占用与延迟稳定性**，不是速度（见 D 阶段 16.4 MB/s）。

### 50. 既然 `O_DIRECT` 有这么多好处，为什么 D 阶段只有 16.4 MB/s，比 A 的 583 MB/s 慢 35 倍？
**答**：因为它们比的**不是同一件事**。A 的 583 MB/s 是"把 8MB 拷进内存"的速度（`write` 返回即完），数据还没落盘；D 的 16.4 MB/s 是"每一笔 4KB 都真的到达设备"的速度，**瓶颈已经在设备侧**（每次 pwrite 都是一个真实 I/O，可能还要 RMW 读-改-写）。这个 35 倍差距恰恰是本项目要传达的最重要的一课：**buffered I/O 的高吞吐是"内核帮你扛了持久化延迟"，不是"存储变快了"**。要公平比较必须把 A 也 fsync 一次（B 阶段），那时两者都是 0.4 MB/s 量级——**瓶颈一致时，O_DIRECT 不会让你更快，也不会让你更慢**。

### 51. 什么时候必须用 `O_DIRECT`？什么时候绝对不要用？`[工程题]`
**答**：

**必须用**（应用自己就是缓存管理者，不容许内核再来一份不可控的缓存）：
- 数据库/存储引擎（MySQL、PostgreSQL 的 `wal_sync_method`、RocksDB、ClickHouse、ScyllaDB）：自带 buffer pool，重复缓存既浪费内存又让延迟不可控；
- 大内存机器的重写入负载（第 24 题）：避免脏页堆积触发 direct reclaim 拖垮整机；
- 需要**可预测延迟**的写路径（避免被后台回写和回收插入抖动）；
- 性能测试要测"裸设备真实能力"时（fio `--direct=1`）。

**绝对不要用**：
- 小文件读写、元数据密集负载（`O_DIRECT` 对每个小 I/O 都是独立设备请求，写放大严重）；
- 顺序大文件读写（浪费预读，通常比 buffered 慢）；
- 不支持 `O_DIRECT` 的文件系统上强行打开；
- 应用没有自己的缓存机制时（等于把内核的优化全丢了，还多了对齐复杂度）。

### 52. 用了 `O_DIRECT` 之后还需要 `fsync` 吗？
**答**：**需要，而且是必须的**——两者正交（这是 `exp2` 程序末尾打印的结论）：

> O_DIRECT 解决的是"谁来缓存"的问题; fsync 解决的是"何时落盘"的问题, 两者是正交的

`O_DIRECT` 只保证数据不经过页缓存，**不保证它离开了设备的易失写缓存**。C 阶段（O_DIRECT + 每块 fsync）和 B 阶段（Buffered + 每块 fsync）耗时同量级（23.3s vs 21.3s），恰好证明：换掉缓存层并不会省掉落盘等待。**但有一个例外值得说**：如果设备支持并启用了 FUA/PLP，某些写路径可以在写命令上直接带 FUA 标志，使"写 + 落盘"一次完成，不必额外发 flush——这时 `O_DIRECT` 写 + 带 FUA 的写可以合并掉首尾的 fsync。要判断你的设备是否受益，用 `strace`/`blktrace` 看有没有独立的 flush 请求。

### 53. `O_DIRECT` 下 4KB 写有什么写放大风险？
**答**：SSD 的**写入粒度**通常远大于 4KB：NAND 一次 program 可能是 16KB/32KB 的页，擦除单位是 MB 级 block。如果应用以 4KB 粒度 `O_DIRECT` 随机写，主控可能需要 **读-改-写（RMW）**：读出整个 NAND page、改 4KB、再写回整页；更糟的是这会让 FTL 的映射表频繁更新，加剧 GC（垃圾回收）和写放大（WA）。表现为：测出的 4K 随机写 IOPS 远低于厂商标称，且写完一段后延迟抬升（后台 GC）。**缓解**：① 把随机写**攒成大批量顺序写**（LSM-tree 的 memtable flush、WAL 的批量追加）；② 用 `fallocate` 预分配避免元数据碎片；③ 对齐到 NAND page（尽量 ≥16KB）；④ 关注盘的 `WA`/`TBW` 指标，长期 4K 随机写会明显缩短寿命。**注意**：本仓库的 D 阶段是**顺序** pwrite 且每笔都 4KB，所以它测的是"小 I/O 无法合并"的代价，还没触发最坏的随机 RMW。

### 54. 一旦选择 `O_DIRECT` 自管缓存，应用需要自己实现哪些东西？`[工程题]`
**答**：这是"自管缓存"的真实成本清单：① **缓存淘汰策略**（LRU/CLOCK/2Q，InnoDB 的改进 LRU、RocksDB 的 block cache + LRU shard）；② **预读策略**（本来内核给的，现在要自己做：顺序探测 + 窗口放大）；③ **写合并/刷盘批量化**（对应本仓库的 group commit，避免第 33 题的 14.5ms 单价）；④ **对齐内存池**（`allocAligned` 的工程版：预分配 + 复用，避免每次 `make` 触发 GC）；⑤ **脏页管理与 checkpoint**（什么时候刷、刷多少、崩溃后怎么恢复）；⑥ **部分读/短读/`EINVAL` 的处理**（`O_DIRECT` 下 `pread` 可能短读）；⑦ **并发与锁**（多线程共享缓存的一致性）。**面试建议**：回答这题时把它当成"内核页缓存做过的所有事，你都要重做一遍并做得更好"——所以只有**比内核更懂自己访问模式**的存储引擎才值得付这个代价。

### 55. `O_DIRECT` 配合 `io_uring` 时，"注册缓冲区"能带来什么？
**答**：`io_uring` 的 `IORING_REGISTER_BUFFERS` + `IORING_OP_READ_FIXED`/`WRITE_FIXED` 把用户缓冲区**提前 pin 住**并建立持久映射。收益有两层：① 每个 I/O 不再需要 `get_user_pages`/`put_page`（页表遍历、跨页处理、引用计数），省掉可观的每请求 CPU 开销；② 配合 `O_DIRECT`，设备可以通过 DMA **直接把数据搬进用户内存**，整条路径**零拷贝**（没有页缓存中转、没有 bounce buffer）。这是现代存储引擎（如某个 SPDK/io_uring 混合方案）追求的终极数据通路。**代价**：缓冲区被 pin 住的内存不能换出、不能释放，需要固定大小的缓冲池，且必须满足 `O_DIRECT` 的对齐要求——本仓库的 `allocAligned` 正是为这类用法准备的基础设施。

### 56. `exp2` 注释说"D 阶段 476ms 是把 8MB 推到硬盘的 DRAM 缓存"——这个解释准确吗？怎么验证？`[工程题]`
**答**：**不准确，甚至方向反了**。8MB / 476ms = 16.4 MB/s，这个速率连 SATA 的 DRAM 缓存带宽都远达不到，更别说 NVMe over PCIe（GB/s 级）。16.4 MB/s 是典型的**介质写入速率量级**，说明这 476ms 主要花在**真实的 NAND program / 写放大**上，而不是"推到 DRAM 缓存"。另外还有一个被忽略的变量：本仓库要求在 **WSL2** 下运行，`/tmp` 实际位于 ext4 VHDX 之上、再经 virtio-blk 映射到 Windows 宿主，所以"盘"本身是虚拟化的——这里的绝对数字只能做**横向对比**，不能当设备真值。

**怎么验证**：① 用 `iostat -x 1` 看 `w_await`/`%util`，若设备侧本身耗时就在几百毫秒级，说明瓶颈在设备；② `blktrace`/`bpftrace` 看 bio 的延迟分布与合并情况；③ 换一个已知性能的真机 NVMe 重跑 D 阶段，若速度跳一个数量级，就证明 WSL 虚拟化才是主因；④ `fio --direct=1 --bs=4k --rw=write` 在同一路径跑，对比是否同量级。**面试价值**：这题考的是"**敢于质疑注释、用工具证伪**"的工程习惯——仓库里的注释也是人写的，数据和结论必须自己复核。

### 57. 数据库里 `O_DIRECT` 的作用范围：数据文件和 WAL 为什么处理不一样？`[工程题]`
**答**：**数据文件通常用 `O_DIRECT`**，因为引擎有自己的 buffer pool，重复缓存浪费内存；而**WAL 的选型更微妙**：InnoDB 有 `innodb_flush_method=O_DIRECT`（对数据文件）和 `O_DIRECT_NO_FSYNC`（对 redo/数据都 direct，且因为已经绕过页缓存而跳过部分 fsync）等取值。关键区别在于**访问模式**：数据文件是"大块随机读（走 buffer pool）+ 批量写"，`O_DIRECT` 收益明显；WAL 是**追加写**，天然可以合并成大 I/O，用 buffered + 组提交 fsync 往往效率更高（这正是仓库 B 阶段的模式：快写攒批 + 批量慢刷）。所以真实引擎常采用**混合策略**：数据文件 direct、日志文件 buffered + 批量 fsync，或者干脆数据也用 `O_DIRECT` 但要配合足够大的写批次。**面试点**：不要背"数据库都要用 O_DIRECT"，要说清"按文件角色分别决策"。

### 58. 在容器 / 云盘上使用 `O_DIRECT` 有哪些坑？`[工程题]`
**答**：① **支持性不确定**：容器里挂的卷可能是 overlayfs（`O_DIRECT` 支持有限）、网络存储（NFS/FUSE 常不支持）、或云厂商的块存储（支持但语义/性能不同），同一份代码在不同环境行为不一致；② **性能不再是本地盘**：云盘有配额 IOPS/带宽、网络抖动、多租户噪声，本仓库这类"本地盘量级"的结论直接搬过去会失真；③ **对齐要求可能不同**：某些云盘后端要求 512 或 4096 对齐，还有的会对非对齐请求做悲观处理；④ **缓存语义不同**：云盘的"写缓存"在远端，`fsync` 走的是网络往返，延迟量级完全不同；⑤ **cgroup I/O 限流**（`io.max`）会对 `O_DIRECT` 生效更直接，可能让你以为是设备慢。**应对**：启动时探测能力并降级、把 direct/buffered 做成配置、把"设备能力基线"纳入部署前检查（跑一个小 fio 基准），而不是假设所有环境一样。

---

## E 组｜mmap、缺页与随机 I/O（第 59–68 题）

### 59. mmap 随机读实验里 `majf=45628` 说明什么？
**答**：`majf` 是**主缺页（major page fault）次数**：进程访问的页不在内存中，必须发起**磁盘 I/O** 才能把它读进来，每次主缺页就是一次真实 I/O。45628 次主缺页对上约 20 秒、IOPS 2281 的量级（2281×20 ≈ 45620），说明**几乎每一次随机读都触发了一次主缺页和一次设备 I/O**——完全冷缓存、零命中。这是"mmap 随机读没有变快，只是把系统调用换成了缺页异常"的直接证据。反面对照：`minf`（次缺页）很少，说明没有额外的 TLB/COW 开销。

### 60. 既然 mmap 随机读的磁盘 I/O 一次没少，为什么还可能"CPU 占用更低"？
**答**：因为 mmap 把**一次 `read()` 系统调用（进入/退出内核 + 拷贝到用户缓冲）替换成了一次缺页异常**，数据页直接映射进用户地址空间，**省掉了"页缓存 → 用户缓冲"的那次 CPU 拷贝**（`copy_user_enhanced_fast_string`）。所以：磁盘 I/O 次数不变、延迟可能更差（缺页在访问时才发生，无法异步预取），但 CPU 侧的拷贝开销和系统调用开销下降。仓库文档把它写成"性能相近，但 CPU 占用可能略低"，是准确的——**mmap 优化的是 CPU 和内存带宽，不是 I/O 次数**。

### 61. mmap 4K 随机读为什么会出现 `P99=1.9ms`、`P999=10ms` 的尾延迟？来源可能有哪些？
**答**：尾延迟的来源是分层的：① **设备侧**：SSD 的 GC/磨损均衡会在后台抢占前台 I/O，产生毫秒级毛刺；② **内核侧**：缺页路径要拿 `mmap_lock`/页锁、分配页、等 I/O 完成，路径比 `pread` 长，还可能撞上内存回收；③ **调度**：随机 4KB 无法被 I/O 调度器合并，每次都是独立请求，队列积压时排队延迟放大；④ **虚拟化**（WSL2）：宿主的 I/O 调度、VHDX 层的合并/拆分、Windows 侧防病毒扫描都可能插入毫秒级抖动；⑤ **后台回写**：如果同时有写负载，回写会抢占读带宽。**工程要点**：`max=57819us`（57ms）和 `stdev=705` 说明**平均值毫无意义**，必须看 P99/P999/P9999——数据库/LSM 引擎的 SLA 从来都是按分位数定的。

### 62. 顺序读和随机读的吞吐为什么差一个数量级？预读在其中起什么作用？
**答**：顺序读时，① 预读把后续数据提前拉进页缓存，**一次 I/O 覆盖多个块**，有效块大小从 4KB 变成 128KB+；② 磁盘/SSD 对连续扇区可以做更大的传输、主控合并请求、减少寻址开销。随机读时这两个红利全没了：每次 4KB 都要独立寻址，HDD 上有寻道（ms 级，IOPS 只有一两百），SSD 上虽然没有机械寻道，但受限于 NAND 并行度和队列深度，4K 随机读的 IOPS 通常也只是顺序带宽除以 4K 的一小部分。仓库 `c/doc/RAEDME.md` 的预期观察写得很对："顺序读吞吐远高于随机读"。

### 63. mmap 写的数据什么时候落盘？需要 `msync` 吗？
**答**：mmap 写命中页缓存（shared file mapping），写操作**只修改内存中的页并置脏**，落盘时机和 buffered write 完全一样：`dirty_expire_centisecs` 到期、或脏页超过 `dirty_background_ratio`/`dirty_ratio`、或 `msync`/`munmap`（`munmap` 不保证写完，只是解映射，脏页仍由内核回写）。**要不要 `msync` 取决于语义需求**：只想"最终落盘"可以不调；需要"此刻已持久"就必须 `msync(MS_SYNC)`——它等价于对这段映射做 `fsync`。**注意**：fio 的 `ioengine=mmap` 之所以快（仓库里 9125 KiB/s 是 4K 随机读，不是写）是因为它把随机写也变成了内存操作 + 内核回写，**测出来的写性能是页缓存的性能**，不是设备的——这正是 `--direct=1` 存在的理由。

### 64. 文件被 `truncate` 或删除后，仍在 mmap 访问会发生什么？
**答**：**SIGBUS**（总线错误），而不是 SIGSEGV。原因是映射是按"文件当时的大小"建立的，映射区中超出当前文件末尾的那部分没有对应的页；访问它时缺页处理无法从文件读到数据，内核对映射的访问返回 `SIGBUS`。这是 mmap 一个非常危险的特性：**默认动作是终止进程，而且可以用 `sigaction` 捕获，但捕获后几乎无法安全恢复**（因为无法构造有效数据）。**工程教训**：用 mmap 做 `[工程题]`（如索引文件）时，必须① 约定文件只增不减、② 用文件锁/租约防止并发 truncate、③ 或在访问前用 `mincore`/`stat` 校验范围。相比之下 `pread` 越过文件末尾只会返回 0 或短读，安全得多——这也是很多存储引擎最终放弃 mmap 的现实原因之一。

### 65. mmap 适合哪类负载？为什么 RocksDB 曾用 mmap 又改回 `pread`？`[工程题]`
**答**：mmap 适合：① **只读、访问局部性好**的大文件（索引、词典、共享库、只读数据集）；② **多进程共享同一份数据**（天然零拷贝共享，页缓存就是共享点）；③ 随机访问、但**访问模式不需要预读控制**的场景。**不适合**：需要 `O_DIRECT`（mmap 不走 direct）、需要精确控制预读、需要异步 I/O、需要处理 I/O 错误（mmap 把 I/O 错误变成不可捕获的 `SIGBUS`，而 `pread` 返回 `EIO` 可以处理）、文件会被截断、以及需要把延迟控制在可预测范围内的场景。RocksDB 的文档明确建议**不要**用 mmap 做读（`mmap_read` 相关选项被标注为不推荐/已废弃），理由正是上面几条：错误不可处理（SIGBUS）、缺页延迟不可控且无法异步、无法与 `O_DIRECT` 配合、TLB/页表开销。**面试点**：这题的关键不是"mmap 好不好"，而是"**mmap 把 I/O 错误和延迟控制权从应用手里拿走了**"。

### 66. 用 mmap 做 4K 随机读，`IOPS=2281`、`BW=9125KiB/s`，这两个数怎么互相印证？
**答**：`IOPS × 块大小 = 带宽`：`2281 × 4KB = 9124 KB/s ≈ 9.1 MB/s`，与 fio 报的 9125 KiB/s 完全对上（KiB 的 1024 进制和 KB 的 1000 进制刚好差一点点）。**这两个数一起看能立刻判断瓶颈类型**：如果 IOPS 很高、带宽很低 → 瓶颈是**小 I/O 的处理能力（IOPS 受限）**，要提高块大小或加深队列；如果带宽高、IOPS 低 → 瓶颈是**带宽**（大块顺序传输）。结合平均延迟还能反推并发度（见第 90 题的 Little's Law）。**面试点**：永远不要只看一个指标，IOPS、带宽、延迟三者要一起读。

### 67. 大页（Huge Page）、`MAP_POPULATE`、`madvise` 在 mmap I/O 里各有什么用？
**答**：① **大页**：2MB 页代替 4KB 页，把页表项和 TLB miss 减少 512 倍——对随机访问大文件很有价值，但文件映射要拿到大页需要文件系统支持（如 tmpfs/shmem 支持 THP；ext4 的 DAX/大页支持有限），且要对齐到 2MB。② **`MAP_POPULATE`**：`mmap` 时**预缺页**，把整段映射的页一次性准备好，避免运行期陷入缺页路径（适合"马上就要全读"的场景，代价是启动时 I/O 集中爆发、且会占满内存）。③ **`madvise`**：`MADV_WILLNEED` 触发预读、`MADV_SEQUENTIAL` 让内核更激进地丢弃已用页、`MADV_RANDOM` 关闭预读、`MADV_DONTNEED` 丢弃已用页（注意 mmap 的 `MADV_DONTNEED` 对 MAP_SHARED 私有映射的语义与 fadvise 略有差异，写脏页处理要小心）。**一句话**：这三个都是"把内核缺页路径的开销提前或摊平"的手段，用错了会变成"启动时卡死"或"内存爆掉"。

### 68. 多进程/多线程共享同一份 mmap 需要注什么？
**答**：① **MAP_SHARED vs MAP_PRIVATE**：共享映射的写对其他进程可见并会回写文件；私有映射是 COW，写不回文件（常用于只读场景）。② **一致性没有自动保证**：共享映射下，进程 A 写了内存，进程 B 立刻读到新值（同一物理页），但**落盘**是异步的——所以跨进程的"写后读"可以一致，"写后崩溃恢复"不行。③ **锁**：mmap 不提供任何同步，需要自己用文件锁/信号量/原子操作。④ **截断**：任一进程 truncate 都会让其他进程的映射区变成 SIGBUS（第 64 题）。⑤ **msync 的粒度**：以页为单位，多进程对同一页并发 msync 属于浪费但无害。⑥ **fork**：子进程继承映射，页表复制但物理页共享，注意 COW 带来的额外内存。**工程实践**：共享 mmap 最适合"只读共享大文件"（词典、索引、只读快照），写共享要非常克制。

---

## F 组｜异步 I/O：io_uring / libaio / 并发模型（第 69–80 题）

### 69. `io_uring` 为什么能把 1024 次读压成 47 次系统调用？
**答**：因为 `io_uring` 把"**提交请求**"和"**收割完成**"都变成了**对共享内存环的操作**，系统调用只在"确实需要通知内核"时才发生。流程是：应用把 1024 个 SQE（提交队列条目）写进共享的 SQ ring，内核按批（本例队列深度 32）取出执行；完成结果写进共享的 CQ ring，应用从 ring 里直接读，**不需要为每次 I/O 陷入内核**。对比同步版 1026 次 `pread64`（每次一个系统调用 + 一次上下文切换），异步版只有 47 次 `io_uring_enter`——1024 次 I/O 的系统调用开销被摊薄了 20 倍，这就是总耗时从 0.038s 降到 0.014s 的主要来源。

### 70. SQ ring / CQ ring、SQE / CQE 是什么？为什么要用共享内存？
**答**：`io_uring` 由两个**内核与用户共享的环形队列**组成：提交队列 SQ（应用生产、内核消费）和完成队列 CQ（内核生产、应用消费），队列元素分别是 SQE（描述一个请求：操作码、fd、缓冲、偏移）和 CQE（描述结果：返回值、user_data）。用共享内存（`mmap` 映射的内核页）而不是普通系统调用的原因很简单：**系统调用本身有固定成本（进入/退出内核、参数拷贝、安全检查）**，把"投递请求"变成"写一个结构体到共享数组"，就把每次操作的边际成本从"一次系统调用"降到"几次内存写"。这正是它比 `libaio` 更彻底的地方——`libaio` 依然每次 `io_submit` 都要系统调用。

### 71. 队列深度 32 是怎么体现成 47 次 `io_uring_enter` 的？
**答**：提交阶段：填 SQE 的循环里，每填满 32 个（`io_uring_get_sqe` 返回 NULL）就 `io_uring_submit` 一次，1024/32 = 32 次；完成阶段：`io_uring_wait_cqe` 在没有就绪 CQE 时会**隐式提交并等待**，也会触发几次 `io_uring_enter`；两者相加再算上首尾，就是 47 次。**面试点**：这个数字不是"精确的 1024/32+1"，而是**反映"提交批次数 + 等待次数"的总和**，说明 `io_uring` 的调用次数由**队列深度和批处理策略**决定，而不是由 I/O 数量决定。调大 `QUEUE_DEPTH`（例如 256）会让 `io_uring_enter` 次数进一步下降，但会增加内存和 CQ 溢出风险。

### 72. `io_uring_get_sqe` 返回 NULL 时，项目里为什么是"submit 然后 continue"？
**答**：

```c
while (submitted < NUM_BLOCKS) {
    sqe = io_uring_get_sqe(&ring);
    if (!sqe) {
        io_uring_submit(&ring);   // SQ 满了：先把已有请求交给内核
        continue;                  // 然后重试获取空闲 SQE
    }
    ...
    submitted++;
}
```

NULL 表示**提交队列已满**（32 个槽位都被未提交的 SQE 占着）。此时必须先 `submit` 把队列推给内核、让内核消费掉槽位，才能继续填。**这种写法的问题**：① `continue` 是**忙等**（busy-loop），如果内核回收 SQ 槽位的速度慢，这个循环会空转烧 CPU；② 更干净的写法是把 `QUEUE_DEPTH` 设成不小于"想同时在飞的最大请求数"（比如 1024），或者用分批提交（每批 32 个 SQE 提交一次，而不是填满再提交），再配合 `io_uring_submit_and_wait` 控制节奏。工程上还要考虑**背压**：不能无限提交，否则 CQ 溢出会丢完成事件。

### 73. 完成循环里完全没检查 `cqe->res`，工程上有什么风险？`[工程题]`
**答**：代码是：

```c
while (completed < NUM_BLOCKS) {
    io_uring_wait_cqe(&ring, &cqe);
    // 处理完成，可获取 cqe->res 检查读取字节数   ← 只是注释，没真的检查
    io_uring_cqe_seen(&ring, cqe);
    completed++;
}
```

风险很大：`cqe->res` 是**每个请求的返回值**，可能是 `-EIO`（设备错误）、`-ENOSPC`、或**短读**（小于 4096）。不检查意味着：设备报错时程序认为"读成功了"，静默继续，最终拿到的是**未初始化或过期的缓冲区内容**——这是典型的数据正确性事故（读到脏数据还以为读到了）。对比 `sync_read.c` 不检查 `pread` 返回值、以及第 40 题的 fsyncgate，本质是同一类问题：**I/O 返回值即错误通道，吞掉它等于埋雷**。修法：`if (cqe->res != BLOCK_SIZE) { /* 记录 offset 与 errno，重试或失败 */ }`，并且用 `io_uring_sqe_set_data` 存的 user_data 来定位是哪个偏移失败了——注意项目里的 completion 循环**完全没用上 user_data**，白白浪费了精确定位失败请求的能力。

### 74. `io_uring_prep_readv` 只放 1 个 `iovec`，为什么不直接用 `io_uring_prep_read`？
**答**：功能上等价，这里用 `readv` 却没有多 iovec 属于**过度设计/预留扩展**：作者可能是想保留"一次提交读多个不连续段"的能力（`readv` 天然支持分散读，把多个块读进不同缓冲）。但既然每个 SQE 只放一个 `iovec`，用 `io_uring_prep_read` 更直接、少一层间接。**面试点**：① 这类"为了将来可能扩展而选择更复杂的 API"要谨慎，复杂度成本立刻发生、收益却可能永远不来；② 真正想批量读多块，正确做法是**多个 SQE**（每个 SQE 一个 offset）让内核并发执行，而不是**一个 SQE 里塞多个 iovec**（后者只是一次 I/O 内部的分段，仍然是单个 offset 语义）。这也是 `readv` 在 io_uring 场景里"看起来像批量，其实不是"的常见误解。

### 75. `io_uring` vs `libaio` vs `epoll + 线程池`，怎么选？`[工程题]`
**答**：

| 方案 | 机制 | 适用 | 局限 |
|---|---|---|---|
| `libaio`（AIO） | `io_submit` 系统调用提交 | 只对 `O_DIRECT` 真正异步；老系统/老代码兼容 | buffered I/O 下会退化阻塞；每次提交都要系统调用；API 难用；有短读/事件丢失的坑 |
| `io_uring` | SQ/CQ 共享环 | 新内核（≥5.10 体验好）上的通用异步（文件、socket、甚至 `fsync`） | 内核版本/容器 seccomp 限制；生态和调试工具还在成熟 |
| `epoll + 线程池` | 同步阻塞 I/O + 线程池模拟并发 | 兼容性最好、调试最简单、代码最好懂 | 每并发一个 I/O 一个线程，线程切换和内存成本高；无法单线程压满深队列 |

**选择依据**：① 内核版本够新（≥5.10，最好 ≥6.x）且要压榨高并发低延迟 → `io_uring`；② 传统数据库已深度绑定 buffered/`O_DIRECT` 混合路径、需要兼容老内核 → 线程池 + `pread`/`pwrite`（本仓库的 sync 版就是这个模型的原型）；③ 需要兼容最广的产品 → 做**可插拔的 I/O 后端**，运行时探测能力降级。**注意**：本仓库的实测也说明，**单线程小负载下 `io_uring` 优势不显著**（0.038s vs 0.014s），真正拉开差距的是高并发、深队列场景。

### 76. `SQPOLL`、`IOPOLL`、注册缓冲区、提供缓冲区分别解决什么问题？
**答**：

- **`IORING_SETUP_SQPOLL`**：让一个**内核线程轮询** SQ ring，应用连 `io_uring_enter` 都不用调了（提交零系统调用）。代价是内核线程持续占一个 CPU（可以设置空闲超时后睡眠）。适合超高 IOPS、CPU 富余的场景。
- **`IORING_SETUP_IOPOLL`**：用**轮询**代替中断来收割完成事件。省掉中断开销和上下文切换，代价是烧 CPU、且通常**只对 `O_DIRECT` 块设备 I/O 有效**（buffered I/O 不能用）。适合 NVMe 上追求极致 IOPS 的场景。
- **注册缓冲区（`REGISTER_BUFFERS`）**：预先 pin 住用户缓冲并建立长期映射，省掉每次 I/O 的 `get_user_pages`，配合 `O_DIRECT` 实现零拷贝（见第 55 题）。
- **提供缓冲区（`PROVIDE_BUFFERS`）**：预先提供一批缓冲给内核，读操作完成时内核**自己挑一个缓冲**并把它的编号放进 CQE。主要解决"**不知道数据会从哪个 fd 来、来多大**"的网络场景（如代理/网关），避免每请求分配缓冲。

**一句话**：这四个特性分别消除"提交系统调用"、"完成中断"、"页表 pin"、"缓冲分配"四类开销。

### 77. `io_uring` + `O_DIRECT` + 固定缓冲区，数据路径上有几次拷贝？
**答**：理想情况下**零次 CPU 拷贝**。路径是：设备通过 DMA 把数据写入内核为 `O_DIRECT` 建立的 bounce-free 映射，也就是应用**预先 pin 住的用户缓冲**；应用从该缓冲直接读。这其中没有"页缓存中转拷贝"、也没有"内核缓冲→用户缓冲拷贝"。对比 buffered 读的一次 CPU 拷贝（`copy_user_enhanced_fast_string`，就是 `perf` 里那个热点函数），这是存储引擎能拿到的极限。**前提条件**：① 缓冲地址/长度/偏移都满足 `O_DIRECT` 对齐；② 通过 `REGISTER_BUFFERS` 注册以使页被 pin；③ 设备支持散列表（SG）或 IOMMU 能直接映射；④ 否则内核会退化成"bounce buffer + 一次拷贝"。**验证方法**：`perf record -g` 看 `copy_user_*` 是否从热点里消失，或看 `perf stat` 的 CPU 使用率下降。

### 78. 异步 I/O 能提升"单线程 4KB 随机读"的 IOPS 吗？为什么？
**答**：**能，但前提是设备的队列深度确实被利用起来了**。同步 `pread` 的模型是：一次只能有一个 I/O 在飞，IOPS 硬上限 = `1 / 单次延迟`（Little's Law，见第 90 题）。如果设备单次 4K 随机读是 100us，同步单线程最多 1 万 IOPS——即使盘的队列深度有 128、能力是 100 万 IOPS。异步 I/O 让**一个线程同时挂起多个未完成请求**（本仓库 `QUEUE_DEPTH=32` 就是 32 个在飞），吞吐上限变成 `并发度 / 延迟`，能显著提升。**但要注意**：① 如果瓶颈不在"并发度"而在设备侧（比如设备本身只有 2281 IOPS 的能力，如仓库 mmap 实验所示），加深队列只会让**延迟线性上升而 IOPS 不变**；② buffered 读的 `io_uring` 有时会退化（内核可能同步完成），收益缩水；③ `O_DIRECT` 下异步才是"真异步"。**结论**：异步 I/O 是把"单线程单请求"变成"单线程深队列"的工具，能不能兑现取决于设备有没有可挖的并行度。

### 79. 同步 1024 次读 0.038s vs io_uring 1024 次读 0.014s，这个差距来自哪里？
**答**：来自**系统调用与调度开销**，而不是 I/O 本身：① 1026 次 `pread64` 意味着 1026 次用户态/内核态切换、参数校验、VFS 层遍历；io_uring 只有 47 次 `io_uring_enter`；② 同步版每次 `pread` 都可能触发进程睡眠/唤醒（等 I/O 完成），上下文切换成本高；异步版由内核在 ring 上批量完成，应用只在 ring 上取结果；③ 同步版的预读/缓存命中路径仍然要走完整系统调用。**重要限定**：这个实验用的是**页缓存已热的 4MB 小数据**，所以省下的几乎全是"每次系统调用的固定成本"（约 36us/次 × 1024 ≈ 37ms，正好对上 0.038s）；如果换成冷缓存或大文件，底层设备 I/O 时间会淹没这部分收益，差距会缩小。**面试点**：讲清"这个数字的上限是系统调用开销，不是 I/O 加速"。

### 80. 生产上用 `io_uring` 有哪些运维/安全坑？`[工程题]`
**答**：① **内核版本差异大**：老内核（5.10 之前）功能残缺、有已知 bug，6.x 才比较稳；同一份代码在不同内核上行为不同。② **seccomp/容器限制**：Docker 默认 seccomp profile 可能拦截 `io_uring_setup`/`io_uring_enter`，需要显式放行；Kubernetes Pod 安全策略也可能禁掉。③ **安全面**：`io_uring` 扩大了内核攻击面，Google 在 ChromeOS/Android 上直接禁用了它，部分发行版/加固内核配置会关闭；`SQPOLL` 让内核线程常驻轮询，可能被滥用。④ **可观测性差**：`strace` 对 io_uring 的可见性弱（只能看到 `io_uring_enter` 的次数，看不到里面的请求），排查要借助 `bpftrace`/`io_uring` 的 tracepoint/`ioutil` 之类工具。⑤ **资源限制**：`ulimit -l`（memlock）会影响注册缓冲区，`SQPOLL` 会多占一个核。**工程建议**：把 `io_uring` 作为**可探测、可降级**的可选后端，启动时做能力自检（`io_uring_setup` 探测 + 小负载冒烟），失败自动回退到 `pread`/`libaio`，而不是启动即崩。

---

## G 组｜块层、设备与观测工具（第 81–90 题）

### 81. 请把"一次 `pread` 到 SSD"的完整分层链路画出来。
**答**：仓库 `c/no_direct_read.c` 的注释和 `c/doc/RAEDME.md` 实验4 给的就是这张图：

```
用户态应用
   │  read/write/mmap
系统调用层        ksys_read() → vfs_read()
   │
VFS               struct file / dentry / inode / address_space
   │              file->f_op->read_iter() = generic_file_read_iter()
具体文件系统       ext4_file_read_iter()（inode、extent、目录、日志）
   │
Page Cache        address_space 管理（XArray）、find_get_page()
   │              page_cache_sync_readahead() 预读
通用块层           submit_bio() → blk_mq_submit_bio()
   │              bio → request_queue → I/O 调度器
设备驱动           nvme_queue_rq()（blk-mq 多队列、DMA）
   │
物理设备           SSD / HDD（NAND / 盘片）
```

**这张图的价值**：任何 I/O 问题都能先定位到"哪一层慢"——用户态算法？系统调用太碎？页缓存/回收？块层排队？设备熄火？——然后再选对应的工具（`perf`/`strace`/`iostat`/`blktrace`）。

### 82. `bio`、`request_queue`、`blk-mq`、I/O 调度器分别是什么？
**答**：① **`bio`** 是块层的 I/O 描述单元（"我要把哪些页/扇区写到哪个设备"），文件系统和页缓存回写都通过 `submit_bio` 投递；② **`request_queue`** 是每个块设备一个的请求队列，`bio` 会被合并/转换成 **request**（可合并的同向连续 I/O 合成一个请求）；③ **`blk-mq`（multi-queue）** 是现代块层的多队列架构：每个 CPU 有软件队列（`ctx`），映射到设备的硬件队列（`hctx`），减少锁竞争、充分利用 NVMe 的多队列能力，是 NVMe 高 IOPS 的关键；④ **I/O 调度器**决定队列里请求的派发顺序（`none`/`mq-deadline`/`kyber`/`bfq`）。

### 83. 现代 NVMe 还需要 I/O 调度器吗？怎么选？
**答**：多数情况下**不需要复杂的调度**，因为 NVMe 的并行度极高、延迟极低，调度带来的排序收益远小于其 CPU 开销，而且 SSD 没有机械寻道，"合并相邻请求"的价值也小。实践建议：① NVMe 上常用 **`none`**（直接派发，`multi-queue` 下的默认）；② 需要延迟公平或防止单个进程饿死其他进程时用 **`mq-deadline`**（保证请求不会无限等待）；③ **`bfq`** 是唯一做公平带宽分配的（面向桌面/交互场景，高吞吐场景开销明显）；④ **`kyber`** 按目标延迟做节流，适合需要同时约束读/写延迟的场景。**判断依据**：`/sys/block/<dev>/queue/scheduler` 看当前值；如果有"某类请求被饿死/延迟毛刺"再考虑换调度器，**不要默认改**。

### 84. `iostat` 的 `await`、`aqu-sz`、`%util` 怎么读？`%util=100%` 代表盘跑满了吗？
**答**：① `await`（`r_await`/`w_await`）= 平均 I/O 完成时间，含**排队时间 + 设备服务时间**，是和业务延迟最相关的指标；② `aqu-sz`（旧名 `avgqu-sz`）= 平均队列长度（在飞的请求数），配合 Little's Law 用来算并发度；③ `%util` = 设备**至少有一个未完成请求的时间占比**。**关键陷阱**：`%util` 是"忙不忙"而不是"满没满"——对**并行设备**（SSD/RAID/NVMe），只要有一个请求在飞就算"忙"，所以 `%util=100%` 时设备可能还有大量空闲并行度，远未饱和；对单队列机械盘，`%util≈100%` 才大致等于饱和。**正确读法**：结合 `r/s + await + aqu-sz`——`await` 开始显著上升、`aqu-sz` 持续大于设备并行度，才是真饱和。仓库 `解释.md` 里 sdd `util=90.05%` 只能说明"磁盘几乎没闲着"，不能直接推出"到极限了"。

### 85. fio 的常用参数怎么读？哪些最容易用错？
**答**：`--name`（job 名）、`--filename`（目标文件/设备）、`--rw=read|randread|write|randwrite|randrw`（访问模式）、`--bs=4k|128k`（块大小）、`--size=1G`（访问范围）、`--ioengine=sync|libaio|mmap|io_uring|psync`、`--direct=0|1`（是否绕过页缓存）、`--iodepth=32`（队列深度，**只对异步引擎有意义**）、`--numjobs=1`（并发进程数）、`--runtime=20 --time_based`（按时间跑满，否则读完就退出）、`--ramp_time`/`--warmup`（排除预热）、`--group_reporting`（合并输出）。

**最容易用错的**：① `--iodepth` 配 `sync`/`psync` 引擎**无效**（每个请求一个系统调用，深度恒为 1）；② 不加 `--time_based` 时 `--runtime` 可能不生效（提前读完就结束）；③ 不加 `--direct=1` 或 `--invalidate=1` 时，第二轮开始数据已在页缓存里，测的是内存；④ `--numjobs × --iodepth` 才是总并发度，漏看一个会把并发算错。

### 86. `c/doc/RAEDME.md` 里那条"典型裸盘随机读命令"其实测的不是裸盘，问题在哪？`[工程题]`
**答**：命令是：

```bash
fio --name=randread-test --filename=/mnt/testfile --rw=randread --bs=4k \
    --size=1G --ioengine=libaio --direct=1 --iodepth=32 --numjobs=1 --runtime=20 --time_based
```

两处问题：① **`--filename=/mnt/testfile` 是一个"文件系统上的文件"，不是裸设备**。测裸盘应该指向块设备本身（如 `--filename=/dev/nvme0n1`），此时 `--size` 的含义和风险都不同（**会破坏设备上原有数据**，必须用专门的测试盘或先备份）。测"文件上的 O_DIRECT"和测"裸设备的块层"是两件事：前者还要经过 ext4 的 extent 映射、可能受文件碎片影响；后者才是驱动+设备的最底层能力。② **路径在 `/mnt/` 下**，按 README 自己的警告，这多半是 drvfs/网络卷，`O_DIRECT` 语义本身就失真——用一条 README 明令禁止的路径做"标准写法"示例是自相矛盾的。

**正确写法**（在原生 Linux + 专用测试盘上）：

```bash
fio --name=raw-randread --filename=/dev/nvme0n1 --rw=randread --bs=4k \
    --ioengine=libaio --direct=1 --iodepth=32 --runtime=20 --time_based
```

**面试价值**：这题考"读文档时是否真的逐参数质疑"。同一条命令里同时藏了"不是裸盘"和"路径不合规"两个问题。

### 87. 为什么 4K 随机读和 128K 顺序读在 `iostat` 上的表现截然不同？
**答**：128K 顺序读：`rkB/s` 很高、`r/s` 相对低（每次请求搬得多）、`r_await` 可能偏高（单次传输时间长）但**吞吐高**、`aqu-sz` 通常较小；4K 随机读：`r/s` 极高、`rkB/s` 低、`r_await` 主要反映**排队和寻址开销**。核心原因是**两种瓶颈不同**：顺序读受**带宽**约束（设备能持续吐多少 MB/s），随机读受**IOPS/延迟**约束（每秒能处理多少个独立请求）。所以在 `iostat` 上看到"`r/s` 上万但 `rkB/s` 只有几十 MB/s"就是典型的 IOPS 瓶颈；反之"`rkB/s` 逼近设备带宽上限但 `r/s` 不高"就是带宽瓶颈。**这是选优化方向的第一步判断**。

### 88. `perf report` 里看到 `copy_user_enhanced_fast_string` 是热点，说明什么？
**答**：说明**CPU 时间大量花在用户态 ↔ 内核态的数据拷贝上**，即 `copy_from_user`/`copy_to_user`。这是 buffered I/O 的固有成本（页缓存 ↔ 用户缓冲那次拷贝），也是本仓库 `c/doc/RAEDME.md` 实验5 点名要观察的函数。**优化方向**：① 用 mmap 消除这次拷贝（数据页直接映射进用户空间）；② 用 `O_DIRECT` + 对齐缓冲省掉"页缓存中转"（但仍有 DMA，不是 CPU 拷贝）；③ 用 `io_uring` 注册缓冲区 + `O_DIRECT` 做到零拷贝；④ 增大块大小降低拷贝次数占比（拷贝总字节数不变，但每次调用的固定开销摊薄）。**对照**：如果热点在 `submit_bio`/`blk_mq_submit_bio`，说明瓶颈在块层排队/提交；如果在 `page_cache_sync_readahead`，说明预读在浪费带宽；如果在 `nvme_queue_rq`，说明在等设备——**读 `perf` 的关键是"热点函数落在哪一层"**。

### 89. `blktrace`/`bpftrace` 能补上 `iostat` 的哪些盲区？`[工程题]`
**答**：`iostat` 给的是**聚合后的结果**（每秒平均值），盲区包括：① **单个请求的延迟分布**（`iostat` 只有平均值，看不到 P99/毛刺）；② **请求被合并/拆分的过程**（一个 4K 逻辑写是否被合并、是否因未对齐被拆成多个物理 I/O）；③ **I/O 的来源进程**（是哪个进程/哪个 cgroup 在打盘）；④ **排队时间 vs 服务时间的拆分**（`await` 是两者之和，`blktrace` 的 Q→G→I→D→C 事件能把"排队等了多久、设备处理了多久"分开）；⑤ **事件顺序**（乱序、重排）。工具选择：`blktrace`/`btt` 做请求级时间线分析，`bpftrace` 挂 `block:block_rq_*` tracepoint 做自定义聚合（延迟直方图、按进程统计），`biosnoop`/`biostacks`（bcc）做快速定位。**典型用法**：`iostat` 发现 `await` 高但 `%util` 不高 → 用 `blktrace` 看是不是排队时间（`Q→G`）占了大部分，从而判断是队列积压还是设备慢。

### 90. 队列深度、延迟、IOPS 是什么关系？怎么用它定位问题？
**答**：**Little's Law**：`并发度 L = 吞吐 X × 延迟 W`，对存储就是：

```
在飞请求数 (queue depth) = IOPS × 平均延迟
→ IOPS = queue depth / 平均延迟
```

用仓库的真实数据验证：mmap 4K 随机读 `IOPS=2281`、`avg clat=435us`，则 `2281 × 435e-6 ≈ 0.99 ≈ 1`——**正好等于 1**，说明这个 fio job 全程只有 1 个请求在飞（`numjobs=1`、mmap 引擎、无 `iodepth`），是**彻底的同步串行**，设备并行度完全没被利用。

**怎么用它定位问题**：① 算出实际并发度。如果 ≈1，说明瓶颈是"应用没并发"，**该做的是加深队列/加异步**，而不是换更快的盘；② 如果并发度已经等于设备能力上限（查盘的 `nr_requests`/NVMe 队列深度），而 IOPS 还是不达标，说明**设备本身到顶了**，此时再加并发只会让延迟线性上升；③ 反过来，给定期望 IOPS 和实测延迟，就能**反推需要多少队列深度**（这正是第 43 题、第 85 题配参数时的算法依据）。**面试点**：很多人调优时盲目调大 `iodepth`/线程数，正确顺序是先用 Little's Law 判断瓶颈到底在"并发不足"还是"设备到顶"。

---

## H 组｜工程落地与生产实践（第 91–100 题）

### 91. 把这套实验的结论搬到一个真实存储引擎，写路径你会怎么设计？`[工程题]`
**答**：按"文件角色"分而治之，核心是**缓存归属 + 落盘时机**两个正交决策：

1. **WAL / redo 日志（追加写）**：buffered write + **组提交 fsync**（本仓库 `group` 模式）。理由是追加写天然可合并成大 I/O，用页缓存攒批能拿到高吞吐，再用一次 fsync 覆盖一批提交者。参数上暴露 `sync` 策略（每事务 / 每 N / 时间窗口），并对齐 `dirty_background_ratio` 做配套调优。这是 InnoDB/PG/RocksDB/etcd 的共同做法。
2. **数据文件 / SSTable**：`O_DIRECT`（本仓库 C/D 阶段）。因为引擎自带 block cache，再来一份页缓存是纯浪费，还会在大内存机器上引发 direct reclaim（第 24 题）。要求攒够批量（≥ NAND page）再写，避免 4K 随机 RMW 写放大（第 53 题）。
3. **读路径**：热数据走引擎自己的 block cache；大块顺序扫描用 `fadvise(DONTNEED)` 主动丢缓存，别污染；随机点查对 `O_DIRECT` 文件加 `FADV_RANDOM` 关预读。
4. **同步原语**：写临时文件 → `fsync(file)` → `rename` → `fsync(dir)`（第 42 题），并且**检查每个 fsync 的返回值**（第 40 题）。
5. **基础设施**：对齐内存池（复用、避免 GC/分配抖动）、可插拔 I/O 后端（`pread`/`io_uring`，启动探测降级）、把 direct/buffered 与 fsync 策略做成**可配置项**而不是硬编码。
6. **可观测**：把"提交延迟分位数"（P99/P999）、"fsync 次数/耗时占比"、"页缓存脏页量"、"IOPS/await/aqu-sz" 都做成指标，用本仓库的实验方法建立**性能基线**，回归时对比。

### 92. 线上出现"写延迟毛刺"（P99 突增但平均正常），你的排查顺序是什么？`[工程题]`
**答**：从上层往下逐层排除，每层都问"这一层能不能产生毫秒级抖动"：

1. **应用层**：GC/内存分配停顿、锁竞争、日志同步打印、批处理放大。用应用自身的延迟直方图 + `perf record -g` 看用户态热点。
2. **系统调用层**：是不是 fsync 次数突增（组提交退化成逐条提交）？用 `strace -c -T` 或 eBPF 统计 `fsync` 次数与耗时分布。
3. **页缓存/内存**：脏页是否逼近 `dirty_ratio` 触发同步回写和 direct reclaim（第 23/29 题）？看 `/proc/vmstat` 的 `nr_dirty`/`nr_writeback` 和 `pgscan_direct`，或 `cachestat`。
4. **块层**：队列积压、请求合并失效、调度器不合适的饥饿。`iostat -x 1` 看 `await`/`aqu-sz`，`blktrace`/`btt` 把排队时间与设备时间拆开（第 89 题）。
5. **设备层**：SSD 后台 GC、磨损、温度降频、写放大。看盘的 SMART、厂商遥测、`WA` 指标。
6. **虚拟化/宿主**：虚拟机/容器的 noisy neighbor、宿主 I/O 调度、网络存储抖动（第 58/94 题）。

**方法论**：并发度用 Little's Law 先判断是"并发不足"还是"设备到顶"（第 90 题）；用**延迟分位数**而不是平均值定位；每次都做**单变量隔离**（第 93 题），不要同时改三个参数。

### 93. 怎么压测才不骗自己？`[工程题]`
**答**：清单如下：

1. **冷热分离**：明确测"缓存命中"还是"真实设备"。要真实就用 `--direct=1`；要冷缓存就 `drop_caches` 或冷启动，并**每次运行前重置状态**。仓库里 A 阶段 583 MB/s 和 D 阶段 16.4 MB/s 的对比就是"没分离"和"分离"的差别。
2. **预热与稳态**：`--ramp_time` 丢弃预热期（SSD 首次写入可能是 SLC 缓存加速，稳态后掉速）。
3. **单变量**：一次只改一个参数（`ioengine`/`bs`/`iodepth`/`direct`），否则无法归因。本仓库的 A/B/C/D 正交设计就是范本。
4. **环境合规**：在原生 Linux 上跑，不在 `/mnt`（drvfs）、不在网络卷、不在有其它负载的机器上；记录内核版本、文件系统、挂载参数、盘型号。
5. **看分位数**：报 P50/P99/P999，不要只报平均。`stdev=705us` 配 `avg=435us` 的数据，平均值本身就不可信（第 61 题）。
6. **数据要可复现**：固定随机种子、固定 `size`/`bs`/`runtime`，把命令和完整输出（含 fio 的 job 描述）存档。本仓库把 `strace -c` 表贴在源码注释和 `strace.md` 里，就是"让结论可复核"的好习惯。
7. **对齐"业务负载画像"**：先搞清业务是 4K 随机读、还是 128K 顺序写、还是混合读写比例、读写比多少，再设计压测——**压测参数与业务不符，测出的数字再漂亮也没有决策价值**。
8. **结论要能证伪**：任何"某方案更快"的结论都要能被重复实验推翻，并对异常数据（如那 2 次多余的系统调用）追根究底。

### 94. 云盘/虚拟化环境延迟不稳定，怎么定位是盘的问题还是宿主的问题？`[工程题]`
**答**：用"**分层对照**"把变量隔离出来：① **同一台机器上换设备对照**：本地临时盘（`/tmp` 所在）vs 云数据盘，同样的 fio 命令跑一遍。本地盘稳、云盘抖 → 怀疑云盘链路；两个都抖 → 怀疑宿主/虚拟化层。② **换实例对照**：同规格换一个实例，若抖动消失，是 noisy neighbor / 宿主机问题。③ **看设备侧指标**：云厂商通常提供盘的 IOPS/带宽/延迟遥测；若服务端指标平稳而客户端抖动 → 问题在客户端的网络/虚拟化路径。④ **看客户端排队**：`iostat` 的 `aqu-sz` 是否积压、`blktrace` 里请求在设备上待了多久（D→C）；如果请求在**驱动/虚拟队列里就等了很久**（Q→D 很长），是客户端侧排队；如果在设备处理阶段长，是服务端。⑤ **网络存储还要看 MTU/重传/丢包**（`ss -ti`、`nstat`）。⑥ **cgroup 限流**：确认不是 `io.max` 在限你（第 95 题）。**工程做法**：把"延迟 SLA + 分位数"写进容量规划，对云盘做**持续基线监测**（而不是只在故障时测），抖动超过阈值就自动告警——因为云盘性能随邻居和宿主变化，一次性测得的数字会过期。

### 95. 容器里怎么给存储 I/O 做隔离？cgroup v1 和 v2 有什么区别？`[工程题]`
**答**：核心接口是 **cgroup v2 的 `io.max`（`rbps`/`wbps`/`riops`/`wiops`，按设备限流）、`io.weight`（相对权重，取代 v1 的 blkio.weight）、`io.latency`（按延迟目标保护关键负载）**。v1 对应的是 `blkio.throttle.read_bps_device` 等一整套 `throttle.*` 文件。

**关键坑**：① **限流对 buffered 写是"延迟生效"的**——因为数据先进页缓存，限流发生在回写阶段，所以容器里 `write()` 依然飞快，只是脏页堆积后被节流甚至触发回收，"限流了但应用没感觉到被限"是常见困惑；要严格限流和可预测延迟，配合 `O_DIRECT` 或直接限制 `wiops`/`wbps` 并观察回写队列。② **v1/v2 语义不同**，v1 的 blkio 对多队列设备的限流精度较差，v2 `io.max` 更可靠但要求内核和运行时支持。③ **`io.latency`** 需要先在设备上设置目标延迟（`io.latency` 用 cgroup 层级声明"我要多少延迟以内"），否则不会生效，容易配错。④ **同一块盘上多个容器的限流是各自独立的**，总盘带宽可能被多个容器之和打满，所以还需要节点级的总量控制（QoS/存储预留）。⑤ 部分运行时（docker `--device-read-bps` 等）已经封装了这些参数，但**不支持 v2 的全部能力**，需要核对实际落盘的 cgroup 文件。

### 96. 吞吐、IOPS、延迟三个指标，业务该盯哪个？SLA 怎么定？`[工程题]`
**答**：**先看业务的"访问模式"和"用户体验函数"，三者不是并列关系**：

- **延迟（尤其 P99/P999）**是唯一直接决定用户体验的指标。用户等一次查询，感知的是那一次请求的延迟，不是平均值。所以 SLA 必须按分位数定：`P99 提交延迟 < 10ms`、`P999 < 50ms` 之类。仓库 `解释.md` 特别强调"数据库、LSM 引擎最怕 P999 毛刺"，就是这个道理。
- **IOPS** 是"随机小 I/O 场景的能力上限"，**吞吐（带宽）**是"顺序大 I/O 场景的能力上限"。业务是 4K 随机点查就盯 IOPS，是大块扫描/备份/导入就盯带宽。
- **三者的换算关系**：`带宽 = IOPS × 块大小`，`IOPS = 并发度 / 延迟`（Little's Law）。定 SLA 的实用流程是：从业务 QPS 和每次请求的 I/O 次数，推出需要的 IOPS；再用允许的延迟反推需要的队列深度和实例数；最后用**基准测试验证这条曲线**（而不是相信厂商标称值——标称值通常是特定队列深度/块大小/顺序模式下的峰值）。

**常见误区**：① 用平均值做 SLA（会掩盖毛刺）；② 只看峰值 IOPS 不看对应延迟（峰值 IOPS 通常在延迟爆掉时才达到）；③ 把吞吐和 IOPS 混为一谈（同一个盘，4K 随机 IOPS 可能只有标称顺序带宽除以 4K 的十分之一）。

### 97. "数据没丢，但重启后丢了"——这类事故怎么复盘？`[工程题]`
**答**：这几乎必然是**持久化语义 bug**，而不是硬件故障（硬件故障通常是"全部/大范围丢"，语义 bug 是"丢掉最近一段时间/某个窗口"）。排查清单：

1. **写入路径是否检查了返回值**：`write`/`pwrite` 短写没重试、fsync 失败被吞掉（第 12/40/73 题，fsyncgate 就是内核层面"fsync 骗人"导致 PG 丢数据）。
2. **fsync 是否真的执行了**：用 `strace` 数 fsync 次数对不对（本仓库第 38 题就是"埋点与真实调用不一致"的小型演习）；确认没有走 `sync_file_range` 或 `O_DSYNC` 被误当成 `fsync`（第 41 题）。
3. **rename 之后有没有 fsync 目录**：只 fsync 文件不 fsync 目录，崩溃后重命名可能丢失（第 40/42 题）。
4. **是否依赖了页缓存的生命周期**：进程 `close` 不等于落盘（本仓库 `exp1 none` 模式 3.79s 跑完 78MB，`close` 什么都没保证）；`munmap` 也不保证写完成（第 63 题）。
5. **O_DIRECT 是否被误当作持久化**：O_DIRECT 只绕过内核缓存，不刷设备缓存（第 35/52 题）。
6. **组提交的窗口是否可接受**：如果用了批量提交（`flush_log_at_trx_commit=2`、时间窗口），"丢掉最后一个批次"是设计预期而非 bug——要确认业务是否接受了这个 RPO。
7. **设备侧**：消费级 SSD 无断电保护时，"盘自己丢缓存"也会造成丢数据；确认盘是否支持 PLP/FUA。

**复盘输出应该是**：一个能**复现**的测试（异常注入）+ 一个明确的语义修正（补齐哪次 fsync / 改哪个 flag）+ 一条**防止回归的测试用例**，而不是"以后小心点"。

### 98. 怎么验证"我的持久化真的生效了"？怎么安全地做掉电测试？`[工程题]`
**答**：核心是**故障注入**，绝不能在真机上拔电（会损坏文件系统/数据，且不可控、不可重复）。可用手段：

1. **`dm-flakey`**：device-mapper target，可以配置成"某些时间窗口内写入静默丢弃/报错"，模拟掉电丢缓存的写。挂在测试盘上跑你的持久化逻辑，重启后校验数据。
2. **`dm-dust`**：模拟坏块/写入失败，验证错误是否被正确上报（专治"fsync 失败被吞"）。
3. **`dm-log-writes` + 重放**：记录所有写并可在任意点截断重放，是崩溃一致性测试（CrashMonkey 类工具）的基础。
4. **`libeatmydata` / 把 fsync 变成 no-op**：快速验证"如果不 fsync 会丢什么"，用来确认你的持久化确实依赖了 fsync。
5. **顺序/边界测试**：在"文件 fsync 之后、rename 之前"、"rename 之后、目录 fsync 之前"等**每一个中间点**注入崩溃，验证重启后文件要么是新内容要么是旧内容，**不能是混合/损坏状态**（这就是"原子替换"的真正含义）。

**验收标准**：`fsync` 返回成功 ⇒ 掉电后数据一定在；崩溃后文件要么完整新版本要么完整旧版本。另外要**监控 fsync 的失败**并让上层感知（绝不能静默忽略 `EIO`）。

### 99. 把这套实验改造成可复现的 CI 基准测试，你会怎么做？`[工程题]`
**答**：关键难点是"**CI 环境的 I/O 特性不稳定**"，所以要把"正确性断言"和"性能基线"分开：

1. **正确性测试（可在 CI 里强断言）**：`O_DIRECT` 未对齐必须返回 `EINVAL`；`direct_read` 前后页缓存不增长（读 `/proc/meminfo` 的 `Cached` 差分做断言）；`io_uring` 版的 `io_uring_enter` 次数应显著小于同步版的 `pread` 次数；`exp1 group` 的 fsync 计数应与 strace 一致（顺便修掉第 38 题的 bug 并加回归用例）；`fsync` 后 `fadvise(DONTNEED)` 再读仍能拿到正确数据。这些是**确定的、与机器无关的**。
2. **性能基线（只做趋势检测，不做硬阈值）**：在固定的 runner 上跑固定命令，把结果存成 JSON 并**和该 runner 的历史分位数比较**（比如"允许 ±20% 波动，超出则告警"），而不是断言绝对数值。同时在输出里记录内核版本、文件系统、盘型号，便于判断"是环境变了还是代码退化了"。
3. **环境约束前置检查**：启动脚本先检查是否在 `/mnt`、文件系统类型、`O_DIRECT` 是否可用、`io_uring` 是否被 seccomp 拦截，不满足就**跳过并明确标注 skip**，而不是给出错误结论（第 47/80 题）。
4. **噪声控制**：固定 `runtime`、丢弃预热、串行执行（避免多个 job 抢同一块盘）、必要时用 `taskset` 绑核、记录当时的系统负载。
5. **产物可复核**：把完整的 strace/fio 输出归档成 artifact，配合本仓库现有的 `strace.md`/注释风格——**让每个数字都能追溯到生成它的命令**。

### 100. 如果让你扩展这个项目，下一步加什么实验？`[工程题]`
**答**：按"补全正交维度 + 补齐生产可观测性"来加，优先级排序：

1. **`fdatasync` vs `fsync` 的实测对比**：现在代码里只有 `f.Sync()`（fsync），`strace.md` 里 `fdatasync` 恒为 0。加一条路径能直接量化"元数据刷新的成本"，并对应第 34/41 题。
2. **组提交参数扫描**：把 `exp1` 的 `group` 的 N 做成参数，扫 N=1/10/100/1000/10000，画出"**吞吐-延迟曲线**"，直接把第 43 题的权衡变成一张图——这是本仓库最有生产价值的一个扩展。
3. **`io_uring` 的进阶对比**：`SQPOLL`、`IOPOLL`、`REGISTER_BUFFERS` 分别开关，量化各自的收益（第 55/76/77 题）；并加一条 `O_DIRECT + io_uring + 固定缓冲` 的零拷贝路径，用 `perf` 证明 `copy_user_*` 热点消失（第 88 题）。
4. **崩溃一致性实验**：用 `dm-flakey`/`dm-log-writes` 对一个"写临时文件 → fsync → rename → fsync 目录"的最小持久化实现做**故障注入**，验证每个中间点崩溃后的状态（第 42/98 题）。这是从"性能实验"跨到"正确性实验"的关键一步。
5. **观测工具链补齐**：把 `blktrace`/`btt`（排队 vs 服务时间拆分）、`bpftrace`（请求级延迟直方图）、`cachestat`（命中率）、`pidstat`（进程级 CPU/IO）都配上脚本，形成"每一层都有工具"的对照表（第 89 题）。
6. **云盘/容器对照组**：同一份负载在本地盘、云盘、cgroup 限流下各跑一遍，把第 58/94/95 题的结论数据化。
7. **修掉已知的小问题**：`exp1 group` 的计数 bug（第 38 题）、`sync_read.c` 不检查返回值（第 12 题）、`io_uring_read.c` 不检查 `cqe->res`（第 73 题）——把"实验代码"提升到"可当参考实现"的质量，本身就是很好的工程练习。

---

## 附：题目难度与考点速查

| 组 | 题号 | 主题 | 难度 |
|---|---|---|---|
| A | 1–15 | 系统调用、缓冲 I/O、对齐、环境 | ⭐ |
| B | 16–30 | 页缓存、脏页回写、内存压力与回收 | ⭐⭐ |
| C | 31–45 | fsync/fdatasync、组提交、原子替换、掉电语义 | ⭐⭐⭐ |
| D | 46–58 | O_DIRECT、自管缓存、写放大、云盘/容器 | ⭐⭐⭐ |
| E | 59–68 | mmap、缺页、尾延迟、共享映射 | ⭐⭐⭐ |
| F | 69–80 | io_uring、AIO、异步模型与并发度 | ⭐⭐⭐⭐ |
| G | 81–90 | 块层、调度器、iostat/fio/perf/blktrace、Little's Law | ⭐⭐⭐⭐ |
| H | 91–100 | 架构设计、事故复盘、压测方法学、CI、扩展 | ⭐⭐⭐⭐⭐ |

**高频追问 TOP 5**（面试官最爱往下钻的）：第 24 题（脏页怎么拖垮整机）、第 36 题（O_DIRECT 与 fsync 正交）、第 43 题（组提交权衡）、第 90 题（Little's Law 定位瓶颈）、第 92 题（毛刺分层排查）。
