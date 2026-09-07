#include <stdio.h>
#include <fcntl.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#include <liburing.h>
#define BLOCK_SIZE 4096   // 请确保定义
#define QUEUE_DEPTH 32
#define NUM_BLOCKS 1024

int main() {
    struct io_uring ring;
    struct io_uring_sqe *sqe;
    struct io_uring_cqe *cqe;
    int fd = open("/tmp/testfile", O_RDONLY);
    if (fd < 0) { perror("open"); exit(1); }

    char *buf;
    if (posix_memalign((void **)&buf, 4096, BLOCK_SIZE * NUM_BLOCKS) != 0) {
        perror("posix_memalign"); exit(1);
    }

    if (io_uring_queue_init(QUEUE_DEPTH, &ring, 0) != 0) {
        fprintf(stderr, "io_uring_queue_init failed\n"); exit(1);
    }

    struct iovec iovs[NUM_BLOCKS];
    int submitted = 0;
    while (submitted < NUM_BLOCKS) {
        sqe = io_uring_get_sqe(&ring);
        if (!sqe) {
            io_uring_submit(&ring);
            continue;
        }
        iovs[submitted].iov_base = buf + submitted * BLOCK_SIZE;
        iovs[submitted].iov_len = BLOCK_SIZE;
        io_uring_prep_readv(sqe, fd, &iovs[submitted], 1, submitted * BLOCK_SIZE);
        io_uring_sqe_set_data(sqe, (void *)(long)submitted);
        submitted++;
    }
    io_uring_submit(&ring);

    int completed = 0;
    while (completed < NUM_BLOCKS) {
        int ret = io_uring_wait_cqe(&ring, &cqe);
        if (ret < 0) { fprintf(stderr, "wait_cqe: %s\n", strerror(-ret)); exit(1); }
        // 处理完成，可获取 cqe->res 检查读取字节数
        io_uring_cqe_seen(&ring, cqe);
        completed++;
    }

    io_uring_queue_exit(&ring);
    close(fd);
    free(buf);
    return 0;
}

/*
strace -c ./io_uring_read

% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 67.56    0.009463         201        47           io_uring_enter
 21.44    0.003003         200        15           mmap
  3.33    0.000466         116         4           munmap
  1.57    0.000220          55         4           openat
  1.31    0.000183          36         5           close
  0.95    0.000133          33         4           mprotect
  0.80    0.000112          37         3           fstat
  0.64    0.000090          45         2           read
  0.57    0.000080          80         1           io_uring_setup
  0.40    0.000056          28         2           pread64
  0.29    0.000041          41         1           getrandom
  0.26    0.000037          37         1           prlimit64
  0.21    0.000030          30         1         1 access
  0.16    0.000023          23         1           arch_prctl
  0.16    0.000023          23         1           set_tid_address
  0.16    0.000023          23         1           set_robust_list
  0.16    0.000023          23         1           rseq
  0.00    0.000000           0         1           brk
  0.00    0.000000           0         1           execve
------ ----------- ----------- --------- --------- ----------------
100.00    0.014006         145        96         1 total
*/