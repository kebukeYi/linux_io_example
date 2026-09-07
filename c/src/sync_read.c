#include <stdio.h>
#include <fcntl.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>

#define BLOCK_SIZE 4096
#define NUM_BLOCKS 1024

int main() {
    int fd = open("/tmp/testfile", O_RDONLY);
    char *buf;
    posix_memalign((void **)&buf, 4096, BLOCK_SIZE * NUM_BLOCKS);
    for (int i = 0; i < NUM_BLOCKS; i++) {
        pread(fd, buf + i * BLOCK_SIZE, BLOCK_SIZE, i * BLOCK_SIZE);
    }
    close(fd);
    free(buf);
    return 0;
}

/*
strace -c ./sync_read
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 99.24    0.037317          36      1026           pread64
  0.69    0.000260         130         2           munmap
  0.07    0.000026           8         3           close
  0.00    0.000000           0         1           read
  0.00    0.000000           0         2           fstat
  0.00    0.000000           0         9           mmap
  0.00    0.000000           0         3           mprotect
  0.00    0.000000           0         1           brk
  0.00    0.000000           0         1         1 access
  0.00    0.000000           0         1           execve
  0.00    0.000000           0         1           arch_prctl
  0.00    0.000000           0         1           set_tid_address
  0.00    0.000000           0         3           openat
  0.00    0.000000           0         1           set_robust_list
  0.00    0.000000           0         1           prlimit64
  0.00    0.000000           0         1           getrandom
  0.00    0.000000           0         1           rseq
------ ----------- ----------- --------- --------- ----------------
100.00    0.037603          35      1058         1 total
*/