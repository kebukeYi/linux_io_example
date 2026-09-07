//go:build linux

// 实验2：O_DIRECT —— 绕过页缓存, 自己管缓存之前, 先看清它的规则和代价
//
// go build -o exp2
// ./exp2 
// ./exp2 -stage=A

// strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=A
// strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=B
// strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=C
// strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=D
//
// 四个阶段:
//   A 缓冲写(Buffered), 无 fsync     —— 数据只进页缓存, 内核稍后回写
//   B 缓冲写(Buffered), 每块 fsync    —— 每次写都要等真正落盘
//   C O_DIRECT,        每块 fsync    —— 绕过页缓存 + 每块强制落盘
//   D O_DIRECT,        无 fsync      —— 绕过页缓存, 没有回写缓冲可依赖

// == 四个阶段: 谁快谁慢, 背后是什么 ==
// A. Buffered     无 fsync           8MB  耗时      13ms  吞吐   583.0 MB/s  fsync 0 次
// B. Buffered     每块 fsync         8MB  耗时   18.676s  吞吐     0.4 MB/s  fsync 2000 次
// C. O_DIRECT     每块 fsync         8MB  耗时   19.986s  吞吐     0.4 MB/s  fsync 2000 次
// D. O_DIRECT     无 fsync           8MB  耗时     476ms  吞吐    16.4 MB/s  fsync 0 次
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const blockSize = 4096

// allocAligned 分配首地址按 blockSize 对齐的内存。
// O_DIRECT 的硬规矩之一: 缓冲区地址、文件偏移、长度都须对齐(通常4096)。
func allocAligned(n int) []byte {
	raw := make([]byte, n+blockSize)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int((blockSize - addr%blockSize) % blockSize)
	return raw[off : off+n]
}

func openForWrite(path string, direct bool) (int, error) {
	flags := syscall.O_CREAT | syscall.O_WRONLY | syscall.O_TRUNC
	if direct {
		flags |= syscall.O_DIRECT
	}
	return syscall.Open(path, flags, 0644)
}

// runPhase 返回 (耗时, fsync次数, 错误)
func runPhase(path string, blocks int, direct, syncEvery bool) (time.Duration, int, error) {
	fd, err := openForWrite(path, direct)
	if err != nil {
		return 0, 0, fmt.Errorf("open(direct=%v) 失败: %w", direct, err)
	}
	defer syscall.Close(fd)

	fsyncCount := 0
	start := time.Now()
	for i := 0; i < blocks; i++ {
		off := int64(i) * blockSize
		// 1. 写入到 page cache, 随机刷盘;
		// 2. 直接写入到到 磁盘中, 必须刷盘;
		if _, err := syscall.Pwrite(fd, blockBuf[off:off+blockSize], off); err != nil {
			return time.Since(start), fsyncCount, fmt.Errorf("pwrite 失败: %w", err)
		}
		if syncEvery {
			if err := syscall.Fsync(fd); err != nil {
				return time.Since(start), fsyncCount, fmt.Errorf("fsync 失败: %w", err)
			}
			fsyncCount++
		}
	}
	return time.Since(start), fsyncCount, nil
}

func report(label string, d time.Duration, blocks, fsyncCount int, err error) {
	mb := float64(blocks) * blockSize / 1024 / 1024
	if err != nil {
		fmt.Printf("%-30s 失败: %v\n", label, err)
		return
	}
	fmt.Printf("%-30s %4.0fMB  耗时 %9s  吞吐 %7.1f MB/s  fsync %d 次\n",
		label, mb, d.Round(time.Millisecond), mb/d.Seconds(), fsyncCount)
}

var blockBuf []byte

func main() {
	blocks := flag.Int("blocks", 2000, "每阶段写入块数(每块4KB, 默认共8MB)")
	dir := flag.String("dir", "", "输出目录(默认当前目录)")
	stageFlag := flag.String("stage", "", "指定要运行的阶段，多个用逗号分隔，如 A,B; 不指定则全部运行")
	flag.Parse()

	wd, _ := os.Getwd()
	if *dir == "" {
		*dir = wd
	}

	if strings.HasPrefix(*dir, "/mnt/") {
		fmt.Println("!! 警告: 在 /mnt/...(Windows盘符)下, drvfs 不支持 O_DIRECT 或语义失真")
		fmt.Println("!!        请把本工程拷到 Linux 主目录 ~/ 下再跑: cp -r /mnt/c/.../IOlab ~/")
	}

	// 1.对齐分配空间
	blockBuf = allocAligned(*blocks * blockSize) // 全局块缓冲, 保证对齐
	// addr := uintptr(unsafe.Pointer(&blockBuf[0]))
	// fmt.Printf("缓冲对齐校验: 起始地址 0x%x, addr%%4096 = %d (应为 0)\n", addr, addr%blockSize)
	// if addr%blockSize != 0 {
	// 	fmt.Println("对齐失败, 退出")
	// 	return
	// }

	for i := range blockBuf {
		blockBuf[i] = byte(i % 251)
	}

	path := filepath.Join(*dir, "exp2.data")

	// ---- 阶段0: 故意不对齐, 看内核怎么拒绝(运行一次即可) ----
	// fmt.Println("\n== 阶段0: 用非对齐缓冲区写 O_DIRECT 文件 ==")
	// fd, err := openForWrite(path, true)
	// if err != nil {
	// 	fmt.Printf("open(O_DIRECT) 失败: %v\n", err)
	// 	fmt.Println("→ 说明当前文件系统不支持 O_DIRECT(或路径不对)。请在 Linux 原生目录(如 ~/)运行。")
	// 	return
	// }
	// bad := make([]byte, 4096+1) // 故意 +1, 让指针错开
	// _, err = syscall.Pwrite(fd, bad, 0)
	// switch {
	// case err == syscall.EINVAL:
	// 	fmt.Println("  写入被内核拒绝: EINVAL (Invalid argument)")
	// 	fmt.Println("  原因: 缓冲区起始地址没有对齐到块大小 —— O_DIRECT 的硬规矩第一条")
	// case err != nil:
	// 	fmt.Println("  其它错误:", err)
	// default:
	// 	fmt.Println("  居然成功了? 该文件系统未强制对齐要求")
	// }
	// syscall.Close(fd)
	// os.Remove(path)

	// ---- 四个阶段对比 ----
	// 1. 定义所有阶段及其属性（使用 map 便于查找）
	stageMap := map[string]struct {
	    direct bool
	    sync   bool
		desc string
	}{
	    "A": {false, false," 最快 —— 数据只进了 pace cache 页缓存就返回, 内核稍后才回写, 此时断电/崩溃会丢"},
	    "B": {false, true," 慢 —— 每块都等 page cache 真正落盘, 持久化是用吞吐换来的"},
	    "C": {true, true," 同 B 量级,但慢点 —— O_DIRECT 并不帮你省 fsync, 持久化还是得自己调用"},
	    "D": {true, false," 比 A 慢 —— 因为没有页缓存兜底, 每次 Pwrite 都是真实磁盘 I/O"},
	}

	// 2. 确定运行顺序（保证输出稳定，与原来一致）
	order := []string{"A", "B", "C", "D"}

	// 3. 根据 -stage 参数筛选要运行的阶段
	var runList []string
	if *stageFlag == "" {
	    runList = order // 全部运行
	} else {
	    parts := strings.Split(*stageFlag, ",")
	    seen := make(map[string]bool)
	    for _, p := range parts {
	        p = strings.TrimSpace(p)
	        if p == "" {
	            continue
	        }
	        if _, ok := stageMap[p]; !ok {
	            fmt.Printf("警告: 未知阶段 %s, 跳过\n", p)
	            continue
	        }
	        if !seen[p] {
	            seen[p] = true
	            runList = append(runList, p)
	        }
	    }
	    if len(runList) == 0 {
	        fmt.Println("没有有效的阶段可运行，退出")
	        return
	    }
	}

	descs:=make([]string,0,len(runList))
	// 4. 遍历运行列表
	for _, name := range runList {
	    cfg := stageMap[name]
	    os.Remove(path) // 每次用新文件
	    d, n, err := runPhase(path, *blocks, cfg.direct, cfg.sync)
	    if cfg.direct && err != nil && strings.Contains(err.Error(), "EINVAL") {
	        fmt.Printf("%-30s 跳过: 文件系统不支持 O_DIRECT\n", name+". "+labelFromCfg(cfg))
	        continue
	    }
	    label := name + ". " + labelFromCfg(cfg)
	    report(label, d, *blocks, n, err)
		desc:=name+cfg.desc
		descs=append(descs,desc)
	}

	for _,desc:= range descs{
		fmt.Println(desc)
	}

	os.Remove(path)

	// A: 583 MB/s 这个速度，基本就是你的内存带宽（或 CPU 拷贝数据到 Page Cache 的速度）。
	// 含义：数据只是从用户态拷贝到了内核态的页缓存（Page Cache）里，write 系统调用就立即返回了。
	// 根本没有碰磁盘。如果此时断电，这 8MB 数据会彻底丢失。

	// B 是“快写（内存） + 慢刷（批量 fsync）”; C 是“慢写（直写磁盘） + 快刷（元数据 fsync）”

	// C 缺少“批量合并”的优势且小包 I/O 效率低,大概率被拆成 2000 个独立的 4KB 磁盘写命令, 所以 C 确实比 B 慢
	// 但如果把数据量放大到 10GB，B 的 Page Cache 会被脏数据占满，触发内核的“强制回写（Writeback）”机制，导致应用进程被阻塞。
	// 而 C 由于不占用 Page Cache，不会拖慢整个系统的内存回收，此时 C 的吞吐会更稳定（但绝对速度依然未必比 B 快）。
	// 在生产环境（如数据库，内存 128GB，数据 1TB）中，如果不加 O_DIRECT，Page Cache 会被巨大的脏数据撑爆，
	// 导致内核频繁触发内存回收（Reclaim），这会阻塞整个系统的 write 调用，造成高达数秒的“系统卡顿（Thrashing）”。
	// 而 O_DIRECT 绕过了 Page Cache，虽然单次写入慢，但延迟稳定、不会拖垮整个 OS。

	// D: O_DIRECT 只是绕过了操作系统的页缓存，但数据依然会被写入磁盘自带的易失性写缓存（DRAM 缓存）。
	// 476ms 的时间，正是把 8MB 数据通过 PCIe 总线推到硬盘缓存（而不是 NAND 闪存颗粒/盘片）的时间。
	// 此时如果断电，靠硬盘电容保住的那点缓存数据依然会丢。
	//fmt.Println("\n读法: ")
	//fmt.Println("  A 最快 —— 数据只进了 pace cache 页缓存就返回, 内核稍后才回写, 此时断电/崩溃会丢")
	//fmt.Println("  B 慢 —— 每块都等 page cache 真正落盘, 持久化是用吞吐换来的")
	//fmt.Println("  C 同 B 量级,但慢点 —— O_DIRECT 并不帮你省 fsync, 持久化还是得自己调用")
	//fmt.Println("  D 比 A 慢 —— 因为没有页缓存兜底, 每次 Pwrite 都是真实磁盘 I/O")
	fmt.Println("  结论: O_DIRECT 解决的是'谁来缓存'的问题; fsync 解决的是'何时落盘'的问题, 两者是正交的")
}

func labelFromCfg(cfg struct{direct bool; sync bool; desc string}) string {
    if cfg.direct {
        if cfg.sync {
            return "O_DIRECT     每块 fsync"
        }
        return "O_DIRECT     无 fsync"
    } else {
        if cfg.sync {
            return "Buffered     每块 fsync"
        }
        return "Buffered     无 fsync"
    }
}