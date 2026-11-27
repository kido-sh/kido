#include "notify.h"

#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

enum { HEADER = 20, LARGEST = HEADER + NOTIFY_MAX_PATH };

struct notifyq {
  int fd;
  bool broken;
  size_t cap;
  size_t len; // bytes queued in buf, which always starts with a record
  size_t off; // bytes of buf already written; only the first record can be partly written
  uint8_t *buf;
};

static uint32_t get32(const uint8_t *p) {
  return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24;
}

static void put32(uint8_t *p, uint32_t v) {
  p[0] = (uint8_t)v;
  p[1] = (uint8_t)(v >> 8);
  p[2] = (uint8_t)(v >> 16);
  p[3] = (uint8_t)(v >> 24);
}

notifyq *notifyq_new(int fd, size_t cap) {
  if (cap < 2 * LARGEST) cap = 2 * LARGEST;
  notifyq *q = calloc(1, sizeof(*q));
  if (!q) return NULL;
  q->buf = malloc(cap);
  if (!q->buf) {
    free(q);
    return NULL;
  }
  q->fd = fd;
  q->cap = cap;
  int fl = fcntl(fd, F_GETFL);
  if (fl < 0 || fcntl(fd, F_SETFL, fl | O_NONBLOCK) < 0) q->broken = true;
  return q;
}

void notifyq_free(notifyq *q) {
  if (!q) return;
  free(q->buf);
  free(q);
}

// compact drops the records written in full.
static void compact(notifyq *q) {
  size_t start = 0;
  while (start < q->len) {
    size_t end = start + 4 + get32(q->buf + start);
    if (end > q->off) break;
    start = end;
  }
  memmove(q->buf, q->buf + start, q->len - start);
  q->len -= start;
  q->off -= start;
}

static bool append(notifyq *q, uint32_t kind, uint32_t flags, uint64_t fileid, const char *path, size_t n) {
  if (q->len + HEADER + n > q->cap) compact(q);
  if (q->len + HEADER + n > q->cap) return false;
  uint8_t *p = q->buf + q->len;
  put32(p, (uint32_t)(HEADER - 4 + n));
  put32(p + 4, kind);
  put32(p + 8, flags);
  put32(p + 12, (uint32_t)fileid);
  put32(p + 16, (uint32_t)(fileid >> 32));
  memcpy(p + HEADER, path, n);
  q->len += HEADER + n;
  return true;
}

void notifyq_flush(notifyq *q) {
  if (q->broken) return;
  compact(q);
  q->len = q->off > 0 ? 4 + get32(q->buf) : 0;
  append(q, NOTIFY_FLUSH, 0, 0, "", 0);
}

void notifyq_event(notifyq *q, uint32_t flags, uint64_t fileid, const char *path, size_t n) {
  if (q->broken) return;
  if (n > NOTIFY_MAX_PATH || !append(q, NOTIFY_EVENT, flags, fileid, path, n)) notifyq_flush(q);
}

int notifyq_drain(notifyq *q) {
  if (q->broken) return -1;
  while (q->off < q->len) {
    ssize_t w = write(q->fd, q->buf + q->off, q->len - q->off);
    if (w > 0) {
      q->off += (size_t)w;
    } else if (w < 0 && errno == EINTR) {
      continue;
    } else if (w < 0 && errno == EAGAIN) {
      return 1;
    } else {
      q->broken = true;
      q->len = q->off = 0;
      return -1;
    }
  }
  q->len = q->off = 0;
  return 0;
}
