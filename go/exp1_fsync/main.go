//go:build linux

// 实验1：fsync 三种节奏的代价 —— 配合 strace 观察系统调用
//
//	-fsync none    全程不调用 fsync（数据只进页缓存，close 不保证落盘）
//	-fsync once    全部写完调一次 fsync
//	-fsync group   每100次写,调一次 fsync（组提交）
//	-fsync every   每写一块就 fsync（逐条持久化）
//
// Linux/WSL 下配合 strace 使用（三选一）:
//  go build -o exp1 main.go

//  1.80秒
//	strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync none

//  1.96秒
//	strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync once
// 
//  2.21秒
//	strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync group

//  3.5分钟
//	strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat,close ./exp1 -fsync every
//
//  对比三张 strace 汇总表：fsync 调用次数、耗时占比，以及程序自己打印的吞吐差异。
//  write: 将 4KB 数据从用户态拷贝到内核 Page Cache（页缓存）的时间 平均每次 75微秒;
//  fsync: 将 78MB 的脏数据（20,000 × 4KB）从内核 Page Cache 强制刷到物理磁盘所消耗的物理时间, 平均每次 14.5毫秒;
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"golang.org/x/sys/unix"
)

func main() {
	mode := flag.String("fsync", "none", "none |once | group | every")
	blocks := flag.Int64("blocks", 20000, "写入块数（每块 4KB, 默认共 20000次写入, 共计78MB)")
	dir := flag.String("dir", "", "输出目录（默认当前目录）")
	flag.Parse()

	if *dir == "" {
		*dir, _ = os.Getwd()
	}

	if runtime.GOOS == "linux" && strings.HasPrefix(*dir, "/mnt/") {
		fmt.Println("!! 警告: 当前在 /mnt/...(Windows 盘符)下运行, drvfs 的磁盘语义与真实文件系统不同")
		fmt.Println("!!        建议把本工程拷到 Linux 主目录 ~/ 下再跑: cp -r /mnt/c/.../IOlab ~/")
	}

	path := filepath.Join(*dir, "exp1.data")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("打开文件失败:", err)
		return
	}
	var sem int64
	sem = 4096
	buf := make([]byte, sem)
	for i := range buf {
		buf[i] = byte(i % 251) // 填点非零数据, 防止内核偷懒压缩;
	}

	fsyncCount := 0
	groupCount:=0
	start := time.Now()
	var offset int64
	var i int64
	for i = 0; i < *blocks; i++ {
		if _, err := f.Write(buf); err != nil {
			fmt.Println("写入失败:", err)
			f.Close()
			return
		}
		offset = int64(i * sem)
		if *mode=="group" && i % 100 ==0{
			EvictCache(f,offset,sem,true)
			groupCount++
		}

		if *mode == "every" {
		EvictCache(f,offset,sem,true)
			fsyncCount++
		}
	}

	if *mode == "once" {
		EvictCache(f,offset,sem,true)
		fsyncCount = 1
	}

	elapsed := time.Since(start)
	f.Close()
	os.Remove(path)

	mb := float64(*blocks) * 4096 / 1024 / 1024

	fmt.Printf("fsync模式=%-5s 数据=%4.0fMB  耗时=%8s  吞吐=%7.1f MB/s  fsync调用=%d次\n",
		*mode, mb, elapsed.Round(time.Millisecond), mb/elapsed.Seconds(), fsyncCount)

	fmt.Println("（上面这行是程序自己统计的; 对比下面: strace 表格里的 fsync 列值应该和它一致）")
}

// 避免大量脏页撑爆内存导致卡顿”。写一批、刷一批、丢一批。
// 内存里的脏页永远不会堆积，内核自然就不需要触发紧急回收（Reclaim）
func EvictCache(f *os.File, offset, length int64,advise bool) error {
    // 1. 强制将数据从 Page Cache 同步到磁盘
    if err := f.Sync(); err != nil {
		fmt.Println("fsync 失败:", err)
		f.Close()
        return err
    }
	if advise {
    // 2. 建议内核释放这段已经落盘的缓存
    // 此处直接使用 unix.FADV_DONTNEED
    return unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED)
	}
	return nil
}
