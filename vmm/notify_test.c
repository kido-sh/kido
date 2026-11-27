// Tests for notify.c: whatever the queue writes must parse as whole records, in order, with
// a flush wherever records were dropped. Build and run with make test-vmm.
#include "notify.h"

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int failures;

#define CHECK(cond, ...)                                                                                     \
  do {                                                                                                       \
    if (!(cond)) {                                                                                           \
      fprintf(stderr, "%s:%d: ", __FILE__, __LINE__);                                                        \
      fprintf(stderr, __VA_ARGS__);                                                                          \
      fprintf(stderr, "\n");                                                                                 \
      failures++;                                                                                            \
    }                                                                                                        \
  } while (0)

typedef struct {
  unsigned char *b;
  size_t len, cap;
} stream;

static void slurp(int fd, stream *s) {
  for (;;) {
    if (s->cap - s->len < 65536) {
      s->cap = s->cap * 2 + 65536;
      s->b = realloc(s->b, s->cap);
    }
    ssize_t n = read(fd, s->b + s->len, s->cap - s->len);
    if (n <= 0) return;
    s->len += (size_t)n;
  }
}

static uint32_t get32(const unsigned char *p) {
  return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24;
}

typedef struct {
  int flush;
  uint32_t flags;
  uint64_t fileid;
  char path[NOTIFY_MAX_PATH + 1];
} record;

// parse splits s into records; it fails the test and returns -1 on anything malformed.
static int parse(const stream *s, record *out, int max) {
  size_t i = 0;
  int n = 0;
  while (i < s->len) {
    if (s->len - i < 20 || n == max) {
      CHECK(0, "a record is cut at byte %zu of %zu", i, s->len);
      return -1;
    }
    uint32_t len = get32(s->b + i), kind = get32(s->b + i + 4);
    if (len < 16 || len > 16 + NOTIFY_MAX_PATH || s->len - i - 4 < len ||
        (kind != NOTIFY_EVENT && kind != NOTIFY_FLUSH) || (kind == NOTIFY_FLUSH && len != 16)) {
      CHECK(0, "malformed record at byte %zu: length %u kind %u", i, len, kind);
      return -1;
    }
    out[n].flush = kind == NOTIFY_FLUSH;
    out[n].flags = get32(s->b + i + 8);
    out[n].fileid = get32(s->b + i + 12) | (uint64_t)get32(s->b + i + 16) << 32;
    memcpy(out[n].path, s->b + i + 20, len - 16);
    out[n].path[len - 16] = 0;
    n++;
    i += 4 + len;
  }
  return n;
}

static void mkpipe(int fds[2]) {
  if (pipe(fds) != 0) {
    perror("pipe");
    exit(1);
  }
  fcntl(fds[0], F_SETFL, fcntl(fds[0], F_GETFL) | O_NONBLOCK);
}

static void path_of(char *buf, int i, size_t len) {
  memset(buf, 'a' + i % 26, len);
  snprintf(buf, len, "/r/%06d/", i);
  buf[strlen(buf)] = 'x';
  buf[len] = 0;
}

static void test_in_order(void) {
  int fds[2];
  mkpipe(fds);
  notifyq *q = notifyq_new(fds[1], 0);
  char p[64];
  for (int i = 0; i < 50; i++) {
    path_of(p, i, 20);
    notifyq_event(q, (uint32_t)i, 0x100000007ULL * (uint64_t)i, p, strlen(p));
  }
  CHECK(notifyq_drain(q) == 0, "drain of a small queue did not finish");
  stream s = {0};
  slurp(fds[0], &s);
  static record r[64];
  int n = parse(&s, r, 64);
  CHECK(n == 50, "got %d records, want 50", n);
  for (int i = 0; i < n && i < 50; i++) {
    path_of(p, i, 20);
    CHECK(!r[i].flush && r[i].flags == (uint32_t)i && r[i].fileid == 0x100000007ULL * (uint64_t)i && !strcmp(r[i].path, p),
          "record %d differs", i);
  }
  notifyq_free(q);
  close(fds[0]);
  close(fds[1]);
  free(s.b);
}

static void test_overflow_flushes(void) {
  int fds[2];
  mkpipe(fds);
  notifyq *q = notifyq_new(fds[1], 0);
  char p[2001];
  for (int i = 0; i < 20; i++) {
    path_of(p, i, 2000);
    notifyq_event(q, 1, 9, p, 2000);
  }
  path_of(p, 99, 30);
  notifyq_event(q, 7, 9, p, 30);
  while (notifyq_drain(q) == 1) {
  }
  stream s = {0};
  slurp(fds[0], &s);
  static record r[64];
  int n = parse(&s, r, 64);
  CHECK(n >= 2, "got %d records", n);
  int flushes = 0;
  for (int i = 0; i < n; i++) flushes += r[i].flush;
  CHECK(flushes >= 1, "an overflowing queue sent no flush");
  CHECK(n >= 1 && !r[n - 1].flush && !strcmp(r[n - 1].path, p) && r[n - 1].flags == 7,
        "the event after the overflow is not last");
  notifyq_free(q);
  close(fds[0]);
  close(fds[1]);
  free(s.b);
}

// test_partial_writes keeps the pipe full so that writes come back short, flushes while a record is
// half written, and checks that the stream still parses.
static void test_partial_writes(void) {
  int fds[2];
  mkpipe(fds);
  notifyq *q = notifyq_new(fds[1], 1 << 20);
  char p[1501];
  stream s = {0};
  int sent = 0, flushedAt = -1, partial = 0;
  for (int round = 0; round < 40; round++) {
    for (int i = 0; i < 40; i++, sent++) {
      path_of(p, sent, 1500);
      notifyq_event(q, 1, 9, p, 1500);
    }
    int d = notifyq_drain(q);
    CHECK(d >= 0, "drain failed with the reader open");
    if (d == 1) partial++;
    if (round == 20 && d == 1) {
      notifyq_flush(q);
      flushedAt = sent;
    }
    char buf[700];
    ssize_t n = read(fds[0], buf, sizeof(buf));
    if (n > 0) {
      if (s.cap - s.len < (size_t)n + 65536) {
        s.cap = s.cap * 2 + 65536 + (size_t)n;
        s.b = realloc(s.b, s.cap);
      }
      memcpy(s.b + s.len, buf, (size_t)n);
      s.len += (size_t)n;
    }
  }
  for (;;) {
    int d = notifyq_drain(q);
    slurp(fds[0], &s);
    if (d == 0) break;
  }
  slurp(fds[0], &s);
  CHECK(partial > 0, "the pipe never filled; the test proves nothing");
  CHECK(flushedAt >= 0, "never flushed with a write in progress");
  static record r[4096];
  int n = parse(&s, r, 4096);
  CHECK(n > 0, "no records parsed");
  int last = -1, flushes = 0;
  for (int i = 0; i < n; i++) {
    if (r[i].flush) {
      flushes++;
      continue;
    }
    int idx = atoi(r[i].path + 3);
    CHECK(idx > last, "record %d came after %d", idx, last);
    last = idx;
    path_of(p, idx, 1500);
    CHECK(!strcmp(r[i].path, p), "record %d has a damaged path", idx);
  }
  CHECK(flushes >= 1, "no flush in the stream");
  notifyq_free(q);
  close(fds[0]);
  close(fds[1]);
  free(s.b);
}

static void test_reader_gone(void) {
  int fds[2];
  mkpipe(fds);
  notifyq *q = notifyq_new(fds[1], 0);
  close(fds[0]);
  notifyq_event(q, 1, 9, "/r/a", 4);
  CHECK(notifyq_drain(q) == -1, "drain with no reader did not fail");
  notifyq_event(q, 1, 9, "/r/b", 4);
  CHECK(notifyq_drain(q) == -1, "a broken queue took more events");
  notifyq_free(q);
  close(fds[1]);
}

static void test_long_path_flushes(void) {
  int fds[2];
  mkpipe(fds);
  notifyq *q = notifyq_new(fds[1], 0);
  static char p[NOTIFY_MAX_PATH + 2];
  memset(p, 'x', sizeof(p) - 1);
  notifyq_event(q, 1, 9, p, NOTIFY_MAX_PATH + 1);
  CHECK(notifyq_drain(q) == 0, "drain");
  stream s = {0};
  slurp(fds[0], &s);
  static record r[4];
  int n = parse(&s, r, 4);
  CHECK(n == 1 && r[0].flush, "a path past NOTIFY_MAX_PATH did not become a flush");
  notifyq_free(q);
  close(fds[0]);
  close(fds[1]);
  free(s.b);
}

int main(void) {
  signal(SIGPIPE, SIG_IGN);
  test_in_order();
  test_overflow_flushes();
  test_partial_writes();
  test_reader_gone();
  test_long_path_flushes();
  if (failures) {
    fprintf(stderr, "notify_test: %d failures\n", failures);
    return 1;
  }
  printf("notify_test: ok\n");
  return 0;
}
