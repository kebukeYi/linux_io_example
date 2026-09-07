
#define _GNU_SOURCE
#include <stdio.h>
#include <fcntl.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>
#include <malloc.h>

#define BLOCK_SIZE 4096

int main() {
    int fd = open("/tmp/testfile", O_RDONLY | O_DIRECT);
    if (fd < 0) { perror("open"); exit(1); }

    char *buf;
    posix_memalign((void **)&buf, 4096, BLOCK_SIZE);  // 必须对齐
    if (!buf) { perror("posix_memalign"); exit(1); }

    ssize_t ret = pread(fd, buf, BLOCK_SIZE, 0);
    if (ret < 0) { perror("pread"); exit(1); }

    close(fd);
    free(buf);
    return 0;
}

