
```
// == 四个阶段: 谁快谁慢, 背后是什么 ==
// A. Buffered  无 fsync      8MB  耗时      13ms  吞吐   583.0 MB/s  fsync 0 次
// B. Buffered  每块 fsync    8MB  耗时   18.676s  吞吐     0.4 MB/s  fsync 2000 次
// C. O_DIRECT  每块 fsync    8MB  耗时   19.986s  吞吐     0.4 MB/s  fsync 2000 次
// D. O_DIRECT  无 fsync     8MB  耗时     476ms  吞吐    16.4 MB/s  fsync 0 次

```


```
strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=A

A. Buffered     无 fsync           8MB  耗时     375ms  吞吐    20.8 MB/s  fsync 0 次
A 最快 —— 数据只进了 pace cache 页缓存就返回, 内核稍后才回写, 此时断电/崩溃会丢
  结论: O_DIRECT 解决的是'谁来缓存'的问题; fsync 解决的是'何时落盘'的问题, 两者是正交的

% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 99.76    0.198010          99      2000           pwrite64
  0.17    0.000340          68         5           openat
  0.06    0.000129          43         3           write
------ ----------- ----------- --------- --------- ----------------
100.00    0.198479          98      2008           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=B

B. Buffered     每块 fsync          8MB  耗时   21.264s  吞吐     0.4 MB/s  fsync 2000 次
B 慢 —— 每块都等 page cache 真正落盘, 持久化是用吞吐换来的
  结论: O_DIRECT 解决的是'谁来缓存'的问题; fsync 解决的是'何时落盘'的问题, 两者是正交的
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 79.25    0.978841         489      2000           fsync
 20.70    0.255603         127      2000           pwrite64
  0.03    0.000341          68         5           openat
  0.02    0.000278          92         3           write
------ ----------- ----------- --------- --------- ----------------
100.00    1.235063         308      4008           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=C

C. O_DIRECT     每块 fsync          8MB  耗时   23.334s  吞吐     0.3 MB/s  fsync 2000 次
C 同 B 量级,但慢点 —— O_DIRECT 并不帮你省 fsync, 持久化还是得自己调用
  结论: O_DIRECT 解决的是'谁来缓存'的问题; fsync 解决的是'何时落盘'的问题, 两者是正交的
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 56.79    0.889727         444      2000           fsync
 43.20    0.676918         338      2000           pwrite64
  0.01    0.000178          35         5           openat
  0.00    0.000000           0         3           write
------ ----------- ----------- --------- --------- ----------------
100.00    1.566823         390      4008           total


strace -c -f -e trace=write,pwrite64,fsync,fdatasync,openat ./exp2 -stage=D

D. O_DIRECT     无 fsync           8MB  耗时    1.103s  吞吐     7.1 MB/s  fsync 0 次
D 比 A 慢 —— 因为没有页缓存兜底, 每次 Pwrite 都是真实磁盘 I/O
  结论: O_DIRECT 解决的是'谁来缓存'的问题; fsync 解决的是'何时落盘'的问题, 两者是正交的
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 99.89    0.430669         215      2000           pwrite64
  0.06    0.000250          83         3           write
  0.06    0.000240          48         5           openat
------ ----------- ----------- --------- --------- ----------------
100.00    0.431159         214      2008           total
```