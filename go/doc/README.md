# IOlab — 存储 I/O 双实验

两个配套实验，配合上一篇讲解使用。**都必须在 Linux/WSL2 里跑**（strace 和 O_DIRECT 是 Linux 特性，Windows 原生不支持）。

## 准备工作（一次）

```bash
# 确认依赖（本机 WSL 已有 go1.27 和 strace）
go version
which strace
```

## 实验1：fsync 三种节奏 + strace 透视

```bash
cd /exp1_fsync
go build -o exp1_advise .

# 修改 EvictCache(f,offset,sem,true) -> EvictCache(f,offset,sem,false)
go build -o exp1_no_advise .

strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1_advise -fsync none
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1_advise -fsync once
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1_advise -fsync group
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1_advise -fsync every
```

看什么：
1. 四张表里 `fsync` 这一行的 **calls**：0 → 1 → 20000
2. `fsync` 的耗时占比在 `every` 模式下冲到最高
3. 程序自己打印的吞吐：`every` 比 `none` 慢一个数量级——这就是"持久性是买来的"

`-blocks` 参数可调数据量（默认 20000 块 = 80MB），嫌慢可以 `-blocks 5000`。

## 实验2：O_DIRECT 的对齐规矩与四种写法的代价

```bash
cd /exp2_odirect
go build -o exp2 .
./exp2

strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=A
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=B
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=C
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=D

```

会依次演示：
- **阶段0**：故意用不对齐的缓冲区写 O_DIRECT 文件 → 内核直接 `EINVAL` 拒绝（对齐是硬规矩）
- **A/B/C/D 四阶段对比**：Buffered vs O_DIRECT × 有无 fsync，输出吞吐表

看什么：A 最快（只进页缓存）、B/C 慢（每块等落盘）、D 介于中间（没有页缓存兜底，每次 Pwrite 都是真 I/O）。结论在程序末尾有打印。

`-blocks` 可调数据量（默认 2000 块 = 8MB）。

## 进阶作业（可选）

1. `strace -c` 跑一次实验2的 C 阶段，对比 D 阶段，观察 O_DIRECT 下 pwrite 的调用形态
2. 把实验1的 `every` 改成每 100 块 fsync 一次，观察吞吐如何随批次大小变化——这就是"组提交(batched commit)"的原型，数据库 WAL 刷盘就是这么设计的


# 关于Sync, 用户层优化: 落盘后，告诉内核这 4KB 数据不要再缓存了，直接丢弃
```
//go:build linux

import (
    "os"
    "golang.org/x/sys/unix"
)

func EvictCache(f *os.File, offset, length int64) error {
    // 1. 强制将数据从 Page Cache 同步到磁盘
    if err := f.Sync(); err != nil {
        return err
    }

    // 2. 建议内核释放这段已经落盘的缓存
    // 此处直接使用 unix.FADV_DONTNEED
    return unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED)
}

```

```
# 调整内核参数（sysctl）：把“恐慌阈值”降低，把“预警阈值”大幅提前
## 尽早刷盘（1% 内存脏页就启动后台线程）
vm.dirty_background_ratio = 1
## 允许突发积累到 20% 内存（给爆发空间）
vm.dirty_ratio = 20
## 脏页最多活 5 秒，加速淘汰
vm.dirty_expire_centisecs = 500
```