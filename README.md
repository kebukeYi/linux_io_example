## 实验环境准备
一台 Linux 机器（虚拟机即可），建议内核版本 ≥ 5.10

安装必要工具：
```
sudo apt update
sudo apt install -y fio iotop sysstat strace liburing-dev build-essential linux-tools-common util-linux
```
准备一个测试文件（例如 1GB 大小）：
```
# 1G随机数据，CPU开销大，数据不可压缩，适合模拟真实业务脏数据
dd if=/dev/urandom of=/tmp/testfile bs=1M count=1024

# 1G全零，速度飞快，数据高度可压缩，适合单纯做磁盘吞吐压力
dd if=/dev/zero of=/tmp/testfile bs=1M count=1024

```
## 实验 1：用 fio + iostat 观察不同 I/O 模式的性能
目标：直观感受顺序读、随机读、mmap 读写下的吞吐量、IOPS、延迟和磁盘利用率差异。

步骤
1.打开两个终端：一个运行 iostat -x 1 持续监控磁盘；另一个执行 fio 命令。

2.执行顺序读测试：
```
fio --name=seq-read --filename=/tmp/testfile --rw=read --bs=128k --size=1G --ioengine=sync --direct=0 --numjobs=1 --runtime=20 --time_based
```

3.执行随机读测试：
```
fio --name=rand-read --filename=/tmp/testfile --rw=randread --bs=4k --size=1G --ioengine=sync --direct=0 --numjobs=1 --runtime=20 --time_based
```

4.执行mmap 随机读测试（使用 ioengine=mmap）：
```
fio --name=mmap-randread --filename=/tmp/testfile --rw=randread --bs=4k --size=1G --ioengine=mmap --numjobs=1 --runtime=20 --time_based
```

5.执行mmap 随机写测试（注意会修改文件内容，可先复制一份）：
```
cp /tmp/testfile /tmp/testfile_mmap
fio --name=mmap-randwrite --filename=/tmp/testfile_mmap --rw=randwrite --bs=4k --size=1G --ioengine=mmap --numjobs=1 --runtime=20 --time_based
```
6.典型裸盘随机读命令，业务SSD测试标准写法,Linux 原生异步 IO，常搭配`--direct=1`绕过 page cache，直接访问块设备，测裸盘真实性能
```
fio --name=randread-test --filename=/mnt/testfile --rw=randread --bs=4k --size=1G --ioengine=libaio --direct=1 --iodepth=32 --numjobs=1 --runtime=20 --time_based
```

### 预期观察
- 顺序读吞吐量远高于随机读（因为预读和连续扇区）。
- 随机读 IOPS 很高但吞吐量低（大量小 I/O）。
- mmap 随机读与普通随机读性能相近，但 CPU 占用可能略低。
- iostat 中 %util 在顺序读时可能接近 100%，随机读时则可能较低但 await 上升。

### 分析要点
- 结合 iostat 的 r/s、rkB/s、await、%util 理解不同模式对磁盘的压力。
- 对比 mmap 与普通 read 的 CPU 使用率（可用 pidstat -p <pid> 1 观察）。

---
## 实验 2：io_uring 简单示例，对比同步 read


### 目标：理解 io_uring 如何减少系统调用开销，提高异步 I/O 性能。

步骤
1.编写一个简单的 C 程序 io_uring_read.c，使用 liburing 提交批量读请求：

2.编译运行：
```
gcc -o io_uring_read io_uring_read.c -luring
time ./io_uring_read
```

3.对比同步读程序 sync_read.c：
```
gcc -o sync_read sync_read.c
time ./sync_read
```

### 预期观察
- io_uring 版本可能略快（尤其在深度大时），但单线程下差距不一定显著，但可以观察到系统调用次数减少（用 strace -c 验证）。

- 用 strace -c ./io_uring_read 和 strace -c ./sync_read 对比系统调用次数。

### 分析要点
- io_uring 只提交一次系统调用（io_uring_enter）就处理了大量 I/O，减少了上下文切换。

- 在高并发、低延迟场景下优势更明显。


# 实验 3：Direct I/O 路径追踪
### 目标：观察 O_DIRECT 如何绕过 Page Cache，理解其行为差异。

步骤
1.编写一个小程序 direct_read.c，使用 O_DIRECT 读取文件：
2.编译并运行，同时用 strace 观察系统调用和内核行为：
```
gcc -o direct_read direct_read.c
strace -e trace=pread64,read,openat ./direct_read
```
3.对比非 O_DIRECT 版本（去掉 O_DIRECT），同样用 strace 观察。

4.用 free -m 观察 Page Cache 占用变化：
```
# 先清空缓存
echo 3 | sudo tee /proc/sys/vm/drop_caches
free -m
./direct_read
free -m
```
5.再运行非 O_DIRECT 版本，对比 Page Cache 增长。

### 预期观察
- O_DIRECT 读取后，Page Cache 没有增长（或增长很少）。
- strace 显示 O_DIRECT 的 pread64 调用与普通 pread64 相同，但底层 I/O 路径不同。
- 普通 read 后 Page Cache 明显增加。

### 分析要点
- Direct I/O 避免了双重拷贝，但需要用户空间缓冲区对齐，且不能利用 Page Cache 的预读和缓存，适合数据库等应用自行管理缓存。


# 实验 4：绘制完整 I/O 栈分层图
### 目标：将所学知识结构化，画一张从应用到硬件的 I/O 栈图，并标注各层关键结构和数据流。

### 步骤
1.使用 draw.io、Visio 或纸笔绘制如下层次：

```
用户态应用
   │  (read/write/mmap)
   ▼
系统调用层 (sys_read, sys_write, sys_mmap)
   │
   ▼
VFS (虚拟文件系统层)
   │  - struct file, dentry, inode, address_space
   ▼
具体文件系统 (ext4/xfs/...)
   │  - 管理 inode、extent、目录、日志
   ▼
Page Cache (页缓存)
   │  - address_space 管理基树/ XArray
   │  - 脏页回写
   ▼
通用块层 (Block Layer)
   │  - bio 请求、request_queue、I/O 调度器
   ▼
设备驱动 (NVMe/SCSI)
   │  - 多队列 (blk-mq)、DMA
   ▼
物理设备 (磁盘/SSD)
```
2.在每条路径旁边标注典型函数或数据结构，例如：

系统调用 → ksys_read() → vfs_read()

VFS → file->f_op->read_iter() → generic_file_read_iter()

Page Cache → find_get_page() / add_to_page_cache_lru()

块层 → submit_bio() → blk_mq_submit_bio()

驱动 → nvme_queue_rq()

3.特别画出两条路径：

缓冲 I/O（Buffered I/O）：经过 Page Cache，有预读、回写。

直接 I/O（Direct I/O）：绕过 Page Cache，直接到块层。

#### 分析要点
这张图将成为你日后分析 I/O 问题的“地图”，可以快速定位问题发生在哪一层。


# 实验 5：用 perf 分析 I/O 性能
目标：学会使用 perf 工具追踪 I/O 相关系统调用和内核函数热点。

步骤
5.1 使用 perf trace 观察系统调用耗时
```
# 追踪一个简单文件读程序的系统调用
perf trace -e read,pread64,openat ./sync_read
```
观察每个系统调用的耗时（以毫秒为单位），对比 mmap 或 io_uring 程序的系统调用。

5.2 使用 perf record + perf report 分析内核热点
 运行一个 I/O 密集型负载，比如 fio 随机读：
```
fio --name=rand-read --filename=/tmp/testfile --rw=randread --bs=4k --size=1G --ioengine=sync --direct=0 --numjobs=1 --runtime=30 --time_based &
FIO_PID=$!
```

采集 perf 数据：
```
sudo perf record -g -p $FIO_PID -- sleep 20
```
生成报告：
```
sudo perf report
```
在 perf report 中，浏览函数调用图（按 + 展开），寻找与 I/O 相关的函数，例如：

blk_mq_submit_bio

submit_bio

ext4_file_read_iter

generic_file_read_iter

page_cache_sync_readahead

find_get_page

copy_user_enhanced_fast_string（拷贝开销）

### 预期观察
- 随机读负载下，CPU 主要消耗在块层提交、中断处理、拷贝用户数据等函数。

- 使用 perf report -g graph 可看到完整调用链，理解从 VFS 到驱动的路径。

### 分析要点
- 通过热点函数判断性能瓶颈在 CPU 拷贝、锁竞争还是磁盘 I/O 等待。

- 对比使用 mmap 或 io_uring 时，热点函数有何不同（例如拷贝函数减少）。








