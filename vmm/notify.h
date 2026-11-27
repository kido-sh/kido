// Records kido-vmm writes for the notify relay, one per FSEvents event or flush: a u32 length of
// what follows it, a u32 kind, the u32 FSEvents flags, the u64 file ID (inode, 0 if unknown) and the
// path bytes, little-endian.
//
// The queue holds whole records and writes them to a non-blocking pipe. A record the pipe takes
// only in part keeps its rest for the next write, so the stream never loses its framing. When the
// queue is full, the records not yet started give way to one flush record.
#ifndef KIDO_VMM_NOTIFY_H
#define KIDO_VMM_NOTIFY_H

#include <stddef.h>
#include <stdint.h>

enum { NOTIFY_EVENT = 1, NOTIFY_FLUSH = 2, NOTIFY_MAX_PATH = 4096 };

typedef struct notifyq notifyq;

// notifyq_new makes fd non-blocking and queues up to cap bytes, at least two of the largest records.
notifyq *notifyq_new(int fd, size_t cap);
void notifyq_free(notifyq *q);

void notifyq_event(notifyq *q, uint32_t flags, uint64_t fileid, const char *path, size_t len);
void notifyq_flush(notifyq *q);

// notifyq_drain writes what the pipe takes: 1 when bytes remain, 0 when the queue is empty, -1 once
// the reader is gone.
int notifyq_drain(notifyq *q);

#endif
