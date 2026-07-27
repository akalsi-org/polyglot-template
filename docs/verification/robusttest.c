// robusttest.c - empirical verification of robust-futex behavior for the
// tmp-mpsc.md liveness design. Two roles:
//
//   holder <mode>   creates a memfd, maps it, initializes a
//                   PTHREAD_PROCESS_SHARED|PTHREAD_MUTEX_ROBUST mutex in it,
//                   locks it per <mode>, prints "READY <pid> <memfd>".
//   checker <pid> <fd> [--recover]
//                   attaches to the holder's memfd via /proc/<pid>/fd/<fd>
//                   (unrelated-process rendezvous; no fork relationship),
//                   trylocks, reports errno by name.
//
// Modes:
//   hold         main thread locks, sleeps 600        (T1 baseline / T2 SIGKILL)
//   threadexit   spawned thread locks, pthread_exit(); process keeps running (T3a)
//   threadcancel spawned thread locks, sleeps; main pthread_cancel()s it;
//                process keeps running                 (T3b)
//   munmap       main locks, munmaps region, _exit(0)  (T5)
//   exitclean    main locks, _exit(0) while mapped     (T5 control)
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

typedef struct {
  pthread_mutex_t mu;
} shared_t;

static shared_t *map_fd(int fd) {
  void *p = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (p == MAP_FAILED) {
    perror("mmap");
    exit(2);
  }
  return (shared_t *)p;
}

static const char *errname(int e) {
  switch (e) {
    case 0:
      return "OK";
    case EBUSY:
      return "EBUSY";
    case EOWNERDEAD:
      return "EOWNERDEAD";
    case ENOTRECOVERABLE:
      return "ENOTRECOVERABLE";
    default:
      return strerror(e);
  }
}

static shared_t *g_sh;

static void *thread_lock_and_exit(void *arg) {
  (void)arg;
  int r = pthread_mutex_lock(&g_sh->mu);
  printf("THREAD_LOCKED %s\n", errname(r));
  fflush(stdout);
  pthread_exit(NULL);  // thread dies holding the robust mutex; process lives
}

static void *thread_lock_and_sleep(void *arg) {
  (void)arg;
  int r = pthread_mutex_lock(&g_sh->mu);
  printf("THREAD_LOCKED %s\n", errname(r));
  fflush(stdout);
  for (;;) sleep(1);  // cancellation point
  return NULL;
}

static int run_holder(const char *mode) {
  int fd = memfd_create("robusttest", 0);
  if (fd < 0) {
    perror("memfd_create");
    return 2;
  }
  if (ftruncate(fd, 4096) != 0) {
    perror("ftruncate");
    return 2;
  }
  g_sh = map_fd(fd);

  pthread_mutexattr_t a;
  pthread_mutexattr_init(&a);
  pthread_mutexattr_setpshared(&a, PTHREAD_PROCESS_SHARED);
  pthread_mutexattr_setrobust(&a, PTHREAD_MUTEX_ROBUST);
  if (pthread_mutex_init(&g_sh->mu, &a) != 0) {
    perror("mutex_init");
    return 2;
  }

  printf("READY %d %d\n", getpid(), fd);
  fflush(stdout);

  if (strcmp(mode, "hold") == 0) {
    int r = pthread_mutex_lock(&g_sh->mu);
    printf("LOCKED %s\n", errname(r));
    fflush(stdout);
    sleep(600);
  } else if (strcmp(mode, "threadexit") == 0) {
    pthread_t t;
    pthread_create(&t, NULL, thread_lock_and_exit, NULL);
    pthread_join(t, NULL);
    printf("THREAD_GONE_PROCESS_ALIVE\n");
    fflush(stdout);
    sleep(600);  // process stays alive; only the locking thread exited
  } else if (strcmp(mode, "threadcancel") == 0) {
    pthread_t t;
    pthread_create(&t, NULL, thread_lock_and_sleep, NULL);
    sleep(1);  // let it take the lock
    pthread_cancel(t);
    pthread_join(t, NULL);
    printf("THREAD_CANCELED_PROCESS_ALIVE\n");
    fflush(stdout);
    sleep(600);
  } else if (strcmp(mode, "munmap") == 0) {
    sleep(2);  // give the orchestrator time to dup our memfd via /proc
    int r = pthread_mutex_lock(&g_sh->mu);
    printf("LOCKED %s\n", errname(r));
    fflush(stdout);
    munmap(g_sh, 4096);
    printf("UNMAPPED_EXITING\n");
    fflush(stdout);
    _exit(0);  // exits holding the lock, region unmapped
  } else if (strcmp(mode, "exitclean") == 0) {
    sleep(2);  // give the orchestrator time to dup our memfd via /proc
    int r = pthread_mutex_lock(&g_sh->mu);
    printf("LOCKED_EXITING %s\n", errname(r));
    fflush(stdout);
    _exit(0);  // exits holding the lock, region still mapped
  } else if (strcmp(mode, "oomhold") == 0) {
    int r = pthread_mutex_lock(&g_sh->mu);
    printf("LOCKED %s\n", errname(r));
    fflush(stdout);
    sleep(2);  // let the orchestrator dup our memfd via /proc
    // allocate and touch memory until the cgroup OOM killer takes us
    for (;;) {
      char *p = malloc(16 << 20);
      if (!p) break;
      memset(p, 0xa5, 16 << 20);
    }
    printf("OOM_NOT_TRIGGERED\n");
    fflush(stdout);  // should be unreachable
    sleep(600);
  } else {
    fprintf(stderr, "unknown mode %s\n", mode);
    return 2;
  }
  return 0;
}

static int run_checker(int pid, int fdnum, int recover) {
  char path[64];
  snprintf(path, sizeof path, "/proc/%d/fd/%d", pid, fdnum);
  int fd = open(path, O_RDWR);
  if (fd < 0) {
    perror(path);
    return 2;
  }
  shared_t *sh = map_fd(fd);

  int r = pthread_mutex_trylock(&sh->mu);
  printf("TRYLOCK %s\n", errname(r));
  if (r == 0) pthread_mutex_unlock(&sh->mu);
  if (r == EOWNERDEAD && recover) {
    int c = pthread_mutex_consistent(&sh->mu);
    int u = pthread_mutex_unlock(&sh->mu);
    int r2 = pthread_mutex_trylock(&sh->mu);
    int u2 = (r2 == 0) ? pthread_mutex_unlock(&sh->mu) : -1;
    printf("RECOVER consistent=%s unlock=%s retrylock=%s reunlock=%s\n", errname(c), errname(u),
           errname(r2), u2 == 0 ? "OK" : "FAIL");
  }
  fflush(stdout);
  return 0;
}

int main(int argc, char **argv) {
  if (argc >= 2 && strcmp(argv[1], "holder") == 0 && argc == 3) return run_holder(argv[2]);
  if (argc >= 4 && strcmp(argv[1], "checker") == 0)
    return run_checker(atoi(argv[2]), atoi(argv[3]), argc > 4 && strcmp(argv[4], "--recover") == 0);
  fprintf(stderr, "usage: %s holder <mode> | checker <pid> <fd> [--recover]\n", argv[0]);
  return 2;
}
