```
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp1 -fsync=none

fsync模式=none  数据=  78MB  耗时=  3.788s  吞吐=   20.6 MB/s  fsync调用=0次
（上面这行是程序自己统计的; 对比下面: strace 表格里的 fsync 列值应该和它一致）
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 99.99    1.958193          97     20002           write
  0.01    0.000268          53         5           openat
------ ----------- ----------- --------- --------- ----------------
100.00    1.958461          97     20007           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp1 -fsync=once

fsync模式=once  数据=  78MB  耗时=  4.181s  吞吐=   18.7 MB/s  fsync调用=1次
（上面这行是程序自己统计的; 对比下面: strace 表格里的 fsync 列值应该和它一致）
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 99.17    2.065638         103     20002           write
  0.82    0.017130       17130         1           fsync
  0.01    0.000209          41         5           openat
------ ----------- ----------- --------- --------- ----------------
100.00    2.082977         104     20008           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp1 -fsync=group

fsync模式=group 数据=  78MB  耗时=  5.826s  吞吐=   13.4 MB/s  fsync调用=0次
（上面这行是程序自己统计的; 对比下面: strace 表格里的 fsync 列值应该和它一致）
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 90.43    1.852560          92     20002           write
  9.55    0.195567         977       200           fsync
  0.02    0.000497          99         5           openat
------ ----------- ----------- --------- --------- ----------------
100.00    2.048624         101     20207           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp1 -fsync=every

fsync模式=every 数据=  78MB  耗时=3m31.184s  吞吐=    0.4 MB/s  fsync调用=20000次
（上面这行是程序自己统计的; 对比下面: strace 表格里的 fsync 列值应该和它一致）
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 79.86   10.478409         523     20000           fsync
 20.12    2.639778         131     20002           write
  0.02    0.002432         486         5           openat
------ ----------- ----------- --------- --------- ----------------
100.00   13.120619         327     40007           total

```

```
假设要清理掉 1GB 的一次性文件缓存：

操作方式	主要耗时点	预估耗时（现代 NVMe + 高端 CPU）	是否会卡住应用？
主动 fadvise	解除页表映射 + TLB 刷新（纯 CPU）	约 1 ~ 5 毫秒	仅清理线程，短暂暂停
被动 kswapd（异步回收）	扫描 LRU + 锁开销	约 50 ~ 200 毫秒（若脏页多则更久）	不卡应用，但 CPU 飙升，拖慢整体
被动 direct reclaim（同步回收）	扫描 LRU + 强制回写脏页 + 等待磁盘	约 500 毫秒 ~ 数秒（取决于脏页比例）	应用完全卡死（你遇到的状况）

```




























