# IOlab — Linux 存储 I/O 实验

一套动手实验，从**系统调用**到**页缓存 / O_DIRECT / fsync**，用 C 与 Go 两种语言对照观察 Linux 存储 I/O 栈的行为。每个实验都配合 `strace` / `fio` / `perf` 等工具验证结论。

> ⚠️ 所有实验**必须在 Linux（或 WSL2）下运行**：`strace`、`O_DIRECT`、`io_uring` 都是 Linux 特性。
> 不要放在 `/mnt/...`（Windows 盘符，drvfs）下跑，那里没有真实的文件系统语义；请把工程拷到 Linux 主目录（如 `~/`）再执行。

## 目录结构

```
.
├── c/                      # C 语言实验：同步读 / io_uring / Direct I/O
│   ├── src/
│   │   ├── sync_read.c        # 同步顺序读：1024 块 × 4KB，逐个 pread64
│   │   ├── io_uring_read.c    # 同一负载改用 io_uring 异步批量提交
│   │   ├── direct_read.c      # O_DIRECT 读（绕过页缓存，缓冲需 4K 对齐）
│   │   └── no_direct_read.c   # 普通缓冲读（走页缓存）
│   └── doc/                   # fio/iostat/mmap/perf 实验文档、截图与讲解
│
└── go/                      # Go 语言实验：fsync 节奏 × O_DIRECT
    ├── exp1_fsync/           # 实验1：fsync 不同调用节奏的代价（配合 strace）
    ├── exp2_odirect/         # 实验2：Buffered vs O_DIRECT × 有无 fsync
    └── doc/README.md          # Go 实验运行手册
```

## 环境要求

- Linux 内核 ≥ 5.10（io_uring / O_DIRECT / fadvise 支持）
- C 部分：`gcc`、`liburing-dev`（编译 io_uring 示例）、`strace`
- 观测工具：`fio`、`iostat`、`perf`
- Go 部分：`go`（实验1 依赖 `golang.org/x/sys`）

```bash
sudo apt update
sudo apt install -y gcc liburing-dev strace fio sysstat linux-tools-common
```

准备测试文件（C 实验用到，建议 1GB）：

```bash
# 随机数据：不可压缩，模拟真实业务脏数据
dd if=/dev/urandom of=/tmp/testfile bs=1M count=1024
```

---

## C 实验（`c/`）

### 1. 同步读 vs io_uring 读

两个程序读同一个 `/tmp/testfile` 的前 1024 个块（每块 4KB），一个用同步 `pread64`，一个用 liburing 批量提交 `readv`。

```bash
cd c/src

gcc -o sync_read sync_read.c            # 不依赖第三方库
gcc -o io_uring_read io_uring_read.c -luring

strace -c ./sync_read
strace -c ./io_uring_read
```

**看点**（程序源码注释里附有实测的 strace 汇总表）：

| 指标 | sync_read | io_uring_read |
|---|---|---|
| 提交 1024 次 4KB 读 | 1026 次 `pread64` | 47 次 `io_uring_enter` 批量完成 |
| strace 统计的总耗时 | ≈ 0.038 s | ≈ 0.014 s |

结论：`io_uring` 把大量系统调用合并成少数几次 `io_uring_enter`，减少了上下文切换；单线程小负载下差距未必显著，高并发 / 低延迟场景优势更明显。

### 2. Direct I/O：绕过页缓存

```bash
gcc -o direct_read direct_read.c
gcc -o no_direct_read no_direct_read.c

# 先清空缓存，观察两种读法对 Page Cache 的影响
echo 3 | sudo tee /proc/sys/vm/drop_caches
free -m
./direct_read        # O_DIRECT：页缓存几乎不增长
free -m
./no_direct_read     # 缓冲读：页缓存明显增长
free -m
```

**看点**：

- `direct_read.c` 用 `O_DIRECT | O_RDONLY` 打开文件，`pread` 前缓冲区必须 `posix_memalign` 按 4096 对齐，否则内核返回 `EINVAL`。
- O_DIRECT 省去了「页缓存 → 用户态」的双重拷贝，但也失去预读和缓存，适合数据库这类自管缓存的应用。
- `no_direct_read.c` 源码注释里画了完整的数据路径：`read → sys_read → VFS(struct file/dentry/inode/address_space) → Page Cache → bio → request_queue → I/O 调度器 → DMA → SSD`。

### 3. 更多实验（`c/doc/`）

[`c/doc/RAEDME.md`](c/doc/RAEDME.md) 里还有四个配套实验，含截图与逐行讲解：

1. **fio + iostat 观察不同 I/O 模式**：顺序读、随机读、mmap 随机读写、libaio + `--direct=1` 裸盘随机读（[典型结果讲解](c/doc/解释.md)）。
2. **io_uring 简单示例**：即上面的 `io_uring_read.c` / `sync_read.c` 对比。
3. **Direct I/O 路径追踪**：即上面的 `direct_read.c` / `no_direct_read.c` 对比。
4. **I/O 栈分层图 + perf 热点分析**：从应用到硬件画分层图；用 `perf trace` / `perf record -g` 找 `submit_bio`、`ext4_file_read_iter`、`page_cache_sync_readahead` 等热点函数。

---

## Go 实验（`go/`）

两个配套实验的详细操作手册见 [`go/doc/README.md`](go/doc/README.md)。二者都把结果打印到终端，配合 `strace -c` 观察系统调用。

### 实验 1：fsync 三种节奏的代价（`go/exp1_fsync`）

写 20000 块 × 4KB（约 78MB），用不同节奏调用 `fsync`，配合 strace 统计 fsync 的**次数**与**耗时占比**。刷完盘后用 `FADV_DONTNEED` 通知内核丢弃这段缓存，避免脏页堆积触发内核强制回收。

```bash
cd go/exp1_fsync
go build -o exp1 .
# 依次跑 4 种节奏：
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync none
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync once
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync group
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync every
```

`-blocks` 可调数据量（默认 20000 块），嫌慢可加 `-blocks 5000`。

典型结果（不同机器量级有差异，见源码头注释与 `strace.md`）：

| 模式 | fsync 次数 | 现象 |
|---|---|---|
| `none` | 0 | 最快（≈ 20 MB/s 量级），数据只进页缓存，断电会丢 |
| `once` | 1 | 略慢，全部写完刷一次盘 |
| `group` | 200（每 100 块一次） | 组提交，比 once 慢一点 |
| `every` | 20000 | 慢约一个数量级（≈ 0.4 MB/s，3 分钟+），「持久化是用吞吐换来的」 |

看点：strace 表里 `fsync` 列的次数应与程序打印一致；`every` 下 fsync 的耗时占比冲到最高。`group` 就是数据库 WAL「组提交（batched commit）」的原型。

### 实验 2：O_DIRECT 对齐规矩与四种写法（`go/exp2_odirect`）

同一个写负载，遍历「Buffered / O_DIRECT × 每块 fsync / 无 fsync」四种组合（阶段 A/B/C/D）：

```bash
cd go/exp2_odirect
go build -o exp2 .
./exp2
# 或用 -stage=A,B 单独跑某个阶段（对照 strace）：
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=D
```

参考结果（单次运行，见 `odirect_linux.go` 头注释）：

| 阶段 | 组合 | 8MB 耗时 | 吞吐 | 说明 |
|---|---|---|---|---|
| A | Buffered，无 fsync | 13 ms | 583 MB/s | 最快，数据只进页缓存（≈ 内存拷贝速度） |
| B | Buffered，每块 fsync | 18.7 s | 0.4 MB/s | 每块都等真正落盘 |
| C | O_DIRECT，每块 fsync | 20.0 s | 0.4 MB/s | 不比 B 快 —— O_DIRECT 并不帮你省 fsync |
| D | O_DIRECT，无 fsync | 476 ms | 16.4 MB/s | 每次 Pwrite 都是真实磁盘 I/O，没有页缓存兜底 |

看点：缓冲区必须对齐（`allocAligned` 保证首地址 4K 对齐；源码里有被注释掉的「阶段0」演示不对齐会被内核以 `EINVAL` 拒绝）。

**核心结论**（程序末尾会打印）：
> O_DIRECT 解决的是「谁来缓存」的问题；fsync 解决的是「何时落盘」的问题，两者是正交的。

以及为什么生产环境（如数据库，内存 128GB、数据 1TB）要用 O_DIRECT：不加的话海量脏数据会撑爆 Page Cache，内核频繁触发内存回收（Reclaim），造成数秒的整机卡顿（Thrashing）；O_DIRECT 单次写入虽慢，但延迟稳定、不拖垮整个 OS。

---

## 进阶阅读

- 一次读完 1GB 文件后如何清缓存：主动 `fadvise(DONTNEED)`（约 1~5 ms） vs 内核 `kswapd` 异步回收 vs `direct reclaim` 同步回收（应用完全卡死）——对比表见 [`go/exp1_fsync/strace.md`](go/exp1_fsync/strace.md)。
- 内核脏页阈值调优参考（`vm.dirty_background_ratio` 等）见 [`go/doc/README.md`](go/doc/README.md)。

## License

见 [LICENSE](LICENSE)。
