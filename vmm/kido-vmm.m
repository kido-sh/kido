// kido-vmm runs one kido container VM under Virtualization.framework, with public API only.
// It exits when the guest powers off. SIGTERM or SIGINT stops the VM at once; SIGUSR1 pauses it and
// SIGUSR2 resumes it (--state-file then reads "paused" or "running").
// --forward tcp:HOSTIP:HOSTPORT:GUESTIP:GUESTPORT stands in for QEMU's hostfwd: VZ's NAT has no
// port forwarding, so the helper proxies each TCP connection, or each UDP client with udp:, to the
// guest's address.
// --network FILE attaches the VM to a vmnet network (macOS 26+) that `kido-vmm hold-network` owns.
// --notify-fd N with --watch DIR ... writes a record for each FSEvents event under the DIRs to the
// pipe at descriptor N, for kido's notify relay (notify.h).
#import <CoreServices/CoreServices.h>
#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#import <vmnet/vmnet.h>
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <ifaddrs.h>
#include <net/if.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <string.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>
#include <xpc/xpc.h>

#include "notify.h"

static NSDate *gStarted;
static NSString *gStateFile;
static atomic_long gLogLeft = 8 << 20;
static atomic_bool gLogFull;

static void logf_(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static void logf_(const char *fmt, ...) {
  if (atomic_load(&gLogFull)) return;
  char buf[1024];
  va_list ap;
  va_start(ap, fmt);
  int n = vsnprintf(buf, sizeof(buf), fmt, ap);
  va_end(ap);
  long cost = (n < 0 ? 0 : MIN(n, (int)sizeof(buf))) + 24;
  if (atomic_fetch_sub(&gLogLeft, cost) < cost) {
    if (!atomic_exchange(&gLogFull, true)) fprintf(stderr, "kido-vmm: log limit reached\n");
    return;
  }
  fprintf(stderr, "kido-vmm %7.3f: %s\n", -gStarted.timeIntervalSinceNow, buf);
}

static const char *stateName(VZVirtualMachineState s) {
  switch (s) {
  case VZVirtualMachineStateStopped: return "stopped";
  case VZVirtualMachineStateRunning: return "running";
  case VZVirtualMachineStatePaused: return "paused";
  case VZVirtualMachineStateError: return "error";
  case VZVirtualMachineStateStarting: return "starting";
  case VZVirtualMachineStatePausing: return "pausing";
  case VZVirtualMachineStateResuming: return "resuming";
  case VZVirtualMachineStateStopping: return "stopping";
  case VZVirtualMachineStateSaving: return "saving";
  case VZVirtualMachineStateRestoring: return "restoring";
  }
  return "unknown";
}

static void writeState(VZVirtualMachineState s) {
  if (!gStateFile) return;
  char tmp[PATH_MAX], line[32];
  snprintf(tmp, sizeof(tmp), "%s.tmp", gStateFile.fileSystemRepresentation);
  int fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
  if (fd < 0) return;
  int n = snprintf(line, sizeof(line), "%s\n", stateName(s));
  bool ok = write(fd, line, (size_t)n) == n;
  close(fd);
  if (ok) rename(tmp, gStateFile.fileSystemRepresentation);
}

// MARK: TCP forwarding

// maxConns caps the connections one forward relays at a time; each costs a thread and two buffers.
enum { maxConns = 256 };

typedef struct {
  struct sockaddr_in host, guest;
  int fd;
  bool udp;
  atomic_int conns;
} forward_t;

typedef struct {
  int from, to;
  int failed;
} pump_t;

// bridgeIndex finds the host interface whose IPv4 subnet holds the guest (VZ's NAT bridge), so a
// connection is never routed out of the default interface before the bridge exists.
static unsigned bridgeIndex(struct in_addr guest) {
  struct ifaddrs *list, *i;
  unsigned idx = 0;
  if (getifaddrs(&list) != 0) return 0;
  for (i = list; i && !idx; i = i->ifa_next) {
    if (!i->ifa_addr || !i->ifa_netmask || i->ifa_addr->sa_family != AF_INET || !(i->ifa_flags & IFF_UP)) continue;
    in_addr_t a = ((struct sockaddr_in *)i->ifa_addr)->sin_addr.s_addr;
    in_addr_t m = ((struct sockaddr_in *)i->ifa_netmask)->sin_addr.s_addr;
    if (a != guest.s_addr && m != 0 && (a & m) == (guest.s_addr & m) && strncmp(i->ifa_name, "bridge", 6) == 0)
      idx = if_nametoindex(i->ifa_name);
  }
  freeifaddrs(list);
  return idx;
}

static void noDelay(int fd) {
  int one = 1;
  setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
  setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof(one));
}

static int dialGuest(const struct sockaddr_in *sa, int timeoutMS) {
  unsigned idx = bridgeIndex(sa->sin_addr);
  if (!idx) {
    errno = ENETUNREACH;
    return -1;
  }
  int s = socket(AF_INET, SOCK_STREAM, 0);
  if (s < 0) return -1;
  fcntl(s, F_SETFD, FD_CLOEXEC);
  if (setsockopt(s, IPPROTO_IP, IP_BOUND_IF, &idx, sizeof(idx)) != 0) goto fail;
  int fl = fcntl(s, F_GETFL);
  fcntl(s, F_SETFL, fl | O_NONBLOCK);
  if (connect(s, (const struct sockaddr *)sa, sizeof(*sa)) != 0) {
    if (errno != EINPROGRESS) goto fail;
    struct pollfd p = {.fd = s, .events = POLLOUT};
    if (poll(&p, 1, timeoutMS) != 1) {
      errno = ETIMEDOUT;
      goto fail;
    }
    int err = 0;
    socklen_t len = sizeof(err);
    getsockopt(s, SOL_SOCKET, SO_ERROR, &err, &len);
    if (err) {
      errno = err;
      goto fail;
    }
  }
  fcntl(s, F_SETFL, fl);
  return s;
fail:;
  int saved = errno;
  close(s);
  errno = saved;
  return -1;
}

static void *pump(void *arg) {
  pump_t *p = arg;
  enum { size = 256 << 10 };
  char *buf = malloc(size);
  for (;;) {
    ssize_t n = read(p->from, buf, size);
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) {
      p->failed = n < 0;
      break;
    }
    for (ssize_t off = 0; off < n;) {
      ssize_t w = write(p->to, buf + off, (size_t)(n - off));
      if (w < 0 && errno == EINTR) continue;
      if (w <= 0) {
        p->failed = 1;
        goto out;
      }
      off += w;
    }
  }
out:
  free(buf);
  if (p->failed) {
    shutdown(p->from, SHUT_RDWR);
    shutdown(p->to, SHUT_RDWR);
  } else {
    shutdown(p->to, SHUT_WR);
  }
  return NULL;
}

typedef struct {
  forward_t *f;
  int client;
} conn_t;

static void *relay(void *arg) {
  conn_t *c = arg;
  NSDate *dialStart = [NSDate date];
  int g = dialGuest(&c->f->guest, 2000);
  if (g < 0) {
    static atomic_int failures;
    if (errno != ENETUNREACH && atomic_fetch_add(&failures, 1) < 20)
      logf_("dial guest port %d: %s after %.0f ms", ntohs(c->f->guest.sin_port), strerror(errno),
            -dialStart.timeIntervalSinceNow * 1000);
    close(c->client);
    free(c);
    return NULL;
  }
  static atomic_flag reached = ATOMIC_FLAG_INIT;
  if (!atomic_flag_test_and_set(&reached)) logf_("first connection to the guest");
  noDelay(c->client);
  noDelay(g);
  pump_t up = {.from = c->client, .to = g}, down = {.from = g, .to = c->client};
  pthread_t t;
  if (pthread_create(&t, NULL, pump, &up) == 0) {
    pump(&down);
    pthread_join(t, NULL);
  }
  close(g);
  close(c->client);
  free(c);
  return NULL;
}

static void *serve(void *arg) {
  forward_t *f = ((conn_t *)arg)->f;
  relay(arg);
  atomic_fetch_sub(&f->conns, 1);
  return NULL;
}

// MARK: UDP forwarding

// Each client address gets its own socket connected to the guest, so replies go back to the right
// client. A session idle for udpIdle seconds is closed, and a full table drops its oldest session.
enum { maxSessions = 256, udpIdle = 90, udpBuf = 65536 };

typedef struct {
  struct sockaddr_in client;
  int fd;
  time_t last;
} session_t;

static int dialGuestUDP(const struct sockaddr_in *sa) {
  unsigned idx = bridgeIndex(sa->sin_addr);
  if (!idx) {
    errno = ENETUNREACH;
    return -1;
  }
  int s = socket(AF_INET, SOCK_DGRAM, 0);
  if (s < 0) return -1;
  if (setsockopt(s, IPPROTO_IP, IP_BOUND_IF, &idx, sizeof(idx)) != 0 ||
      connect(s, (const struct sockaddr *)sa, sizeof(*sa)) != 0) {
    int saved = errno;
    close(s);
    errno = saved;
    return -1;
  }
  fcntl(s, F_SETFD, FD_CLOEXEC);
  fcntl(s, F_SETFL, fcntl(s, F_GETFL) | O_NONBLOCK);
  return s;
}

static void *udpLoop(void *arg) {
  forward_t *f = arg;
  session_t sessions[maxSessions];
  struct pollfd fds[maxSessions + 1];
  char *buf = malloc(udpBuf);
  int n = 0;
  for (;;) {
    fds[0] = (struct pollfd){.fd = f->fd, .events = POLLIN};
    for (int i = 0; i < n; i++) fds[i + 1] = (struct pollfd){.fd = sessions[i].fd, .events = POLLIN};
    int ready = poll(fds, (nfds_t)n + 1, 1000);
    time_t now = time(NULL);
    for (int i = 0; ready > 0 && i < n; i++) {
      if (!fds[i + 1].revents) continue;
      ssize_t m = recv(sessions[i].fd, buf, udpBuf, 0);
      if (m < 0) continue;
      sendto(f->fd, buf, (size_t)m, 0, (struct sockaddr *)&sessions[i].client, sizeof(sessions[i].client));
      sessions[i].last = now;
    }
    if (ready > 0 && (fds[0].revents & POLLIN)) {
      struct sockaddr_in from;
      socklen_t len = sizeof(from);
      ssize_t m = recvfrom(f->fd, buf, udpBuf, 0, (struct sockaddr *)&from, &len);
      int i = 0;
      while (m >= 0 && i < n &&
             (sessions[i].client.sin_addr.s_addr != from.sin_addr.s_addr || sessions[i].client.sin_port != from.sin_port))
        i++;
      if (m >= 0 && i == n) {
        if (n == maxSessions) {
          int oldest = 0;
          for (int j = 1; j < n; j++)
            if (sessions[j].last < sessions[oldest].last) oldest = j;
          close(sessions[oldest].fd);
          sessions[oldest] = sessions[--n];
          i = n;
        }
        int g = dialGuestUDP(&f->guest);
        if (g < 0) {
          static atomic_int failures;
          if (errno != ENETUNREACH && atomic_fetch_add(&failures, 1) < 20)
            logf_("udp to guest port %d: %s", ntohs(f->guest.sin_port), strerror(errno));
          m = -1;
        } else {
          sessions[n++] = (session_t){.client = from, .fd = g};
        }
      }
      if (m >= 0) {
        send(sessions[i].fd, buf, (size_t)m, 0);
        sessions[i].last = now;
      }
    }
    for (int i = 0; i < n;) {
      if (now - sessions[i].last > udpIdle) {
        close(sessions[i].fd);
        sessions[i] = sessions[--n];
      } else {
        i++;
      }
    }
  }
  return NULL;
}

// MARK: console

// The console goes through a pipe to a thread that keeps FILE and FILE.1 at most consoleMax bytes
// each, so a guest that writes forever cannot fill the host disk.
typedef struct {
  int in;
  char path[PATH_MAX];
  long long max;
} console_t;

static void *consoleLoop(void *arg) {
  console_t *c = arg;
  char buf[16384], old[PATH_MAX + 2];
  snprintf(old, sizeof(old), "%s.1", c->path);
  int out = open(c->path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0600);
  long long size = out >= 0 ? lseek(out, 0, SEEK_END) : 0;
  for (;;) {
    ssize_t n = read(c->in, buf, sizeof(buf));
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) break;
    if (size + n > c->max) {
      if (out >= 0) close(out);
      rename(c->path, old);
      out = open(c->path, O_WRONLY | O_APPEND | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
      size = 0;
    }
    if (out >= 0 && write(out, buf, (size_t)n) == n) size += n;
  }
  return NULL;
}

static void *acceptLoop(void *arg) {
  forward_t *f = arg;
  for (;;) {
    int fd = accept(f->fd, NULL, NULL);
    if (fd < 0) {
      if (errno != EINTR && errno != ECONNABORTED) usleep(10000);
      continue;
    }
    fcntl(fd, F_SETFD, FD_CLOEXEC);
    if (atomic_fetch_add(&f->conns, 1) >= maxConns) {
      atomic_fetch_sub(&f->conns, 1);
      static atomic_int refused;
      if (atomic_fetch_add(&refused, 1) < 20)
        logf_("refusing a connection to guest port %d: %d open", ntohs(f->guest.sin_port), maxConns);
      close(fd);
      continue;
    }
    conn_t *c = malloc(sizeof(*c));
    c->f = f;
    c->client = fd;
    pthread_t t;
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    if (pthread_create(&t, &attr, serve, c) != 0) {
      atomic_fetch_sub(&f->conns, 1);
      close(fd);
      free(c);
    }
    pthread_attr_destroy(&attr);
  }
  return NULL;
}

static int parseAddr(const char *ip, const char *port, struct sockaddr_in *sa) {
  char *end;
  long p = strtol(port, &end, 10);
  memset(sa, 0, sizeof(*sa));
  sa->sin_len = sizeof(*sa);
  sa->sin_family = AF_INET;
  sa->sin_port = htons((uint16_t)p);
  return *end == 0 && p > 0 && p < 65536 && inet_pton(AF_INET, ip, &sa->sin_addr) == 1 ? 0 : -1;
}

// parseForward reads tcp:HOSTIP:HOSTPORT:GUESTIP:GUESTPORT, or the same with udp.
static forward_t *parseForward(const char *spec) {
  char *dup = strdup(spec), *parts[5], *save = NULL;
  int n = 0;
  for (char *tok = strtok_r(dup, ":", &save); tok && n < 5; tok = strtok_r(NULL, ":", &save)) parts[n++] = tok;
  forward_t *f = calloc(1, sizeof(*f));
  if (n != 5 || strtok_r(NULL, ":", &save) || (strcmp(parts[0], "tcp") != 0 && strcmp(parts[0], "udp") != 0) ||
      parseAddr(parts[1], parts[2], &f->host) != 0 || parseAddr(parts[3], parts[4], &f->guest) != 0) {
    free(f);
    f = NULL;
  } else {
    f->udp = strcmp(parts[0], "udp") == 0;
  }
  free(dup);
  return f;
}

static int listenOn(forward_t *f) {
  int one = 1;
  pthread_t t;
  if (f->udp) {
    f->fd = socket(AF_INET, SOCK_DGRAM, 0);
    if (f->fd < 0) return -1;
    fcntl(f->fd, F_SETFD, FD_CLOEXEC);
    fcntl(f->fd, F_SETFL, fcntl(f->fd, F_GETFL) | O_NONBLOCK);
    if (bind(f->fd, (struct sockaddr *)&f->host, sizeof(f->host)) != 0) return -1;
    return pthread_create(&t, NULL, udpLoop, f);
  }
  f->fd = socket(AF_INET, SOCK_STREAM, 0);
  if (f->fd < 0) return -1;
  setsockopt(f->fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
  fcntl(f->fd, F_SETFD, FD_CLOEXEC);
  if (bind(f->fd, (struct sockaddr *)&f->host, sizeof(f->host)) != 0 || listen(f->fd, 128) != 0) return -1;
  return pthread_create(&t, NULL, acceptLoop, f);
}

// MARK: vmnet networks

// A vmnet network lives only as long as the process that created it: when that process exits, every
// VM attached to it loses the network, and a VM process that loads it afterwards gets none. So
// `kido-vmm hold-network` creates the network, writes its serialization to a file for the VM
// processes (one data value, rebuilt with xpc_dictionary_set_data) and exits once no VM process holds
// a shared flock on the users file.
static const char *const serializationKey = "networkSerialization";
static char gHeldPath[PATH_MAX];
static struct stat gHeld;

static void usage(void) __attribute__((noreturn));

// parseSubnet reads A.B.C.D/N (N from 16 to 29) into the host's address, the subnet's first (vmnet
// takes the host address, not the network's), and the mask.
static bool parseSubnet(const char *s, struct in_addr *host, struct in_addr *mask) {
  char addr[INET_ADDRSTRLEN], *end;
  const char *slash = strchr(s, '/');
  if (!slash || (size_t)(slash - s) >= sizeof(addr)) return false;
  memcpy(addr, s, (size_t)(slash - s));
  addr[slash - s] = 0;
  long bits = strtol(slash + 1, &end, 10);
  struct in_addr net;
  if (*end || bits < 16 || bits > 29 || inet_pton(AF_INET, addr, &net) != 1) return false;
  mask->s_addr = htonl(~0u << (32 - bits));
  if (net.s_addr & ~mask->s_addr) return false;
  host->s_addr = htonl(ntohl(net.s_addr) + 1);
  return true;
}

// unlinkHeld removes the serialization file unless a later holder has replaced it.
static void unlinkHeld(void) {
  struct stat st;
  if (stat(gHeldPath, &st) == 0 && st.st_dev == gHeld.st_dev && st.st_ino == gHeld.st_ino) unlink(gHeldPath);
}

static void holderExit(int sig) {
  unlinkHeld();
  _exit(0);
}

// exitBusy says vmnet refused the subnet: kido waits until macOS can have released it and retries.
enum { exitBusy = 75 };

// report ends the holder's report to kido: "ok", "busy MSG" or "err MSG".
static int report(FILE *r, int status, const char *fmt, ...) __attribute__((format(printf, 3, 4)));
static int report(FILE *r, int status, const char *fmt, ...) {
  char msg[512];
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(msg, sizeof(msg), fmt, ap);
  va_end(ap);
  if (status) logf_("%s", msg);
  if (r) {
    fprintf(r, "%s%s\n", status == 0 ? "" : status == exitBusy ? "busy " : "err ", msg);
    fclose(r);
  }
  return status;
}

API_AVAILABLE(macos(26.0))
static int holdNetwork(const char *subnet, bool hostOnly, const char *path, const char *users, FILE *r) {
  struct in_addr host, mask;
  if (!parseSubnet(subnet, &host, &mask)) return report(r, 2, "bad subnet %s: want A.B.C.D/16 to /29", subnet);
  vmnet_return_t st = 0;
  vmnet_network_configuration_ref config =
      vmnet_network_configuration_create(hostOnly ? VMNET_HOST_MODE : VMNET_SHARED_MODE, &st);
  if (!config) return report(r, 1, "vmnet configuration: status %u", st);
  if ((st = vmnet_network_configuration_set_ipv4_subnet(config, &host, &mask)) != VMNET_SUCCESS)
    return report(r, 1, "vmnet subnet %s: status %u", subnet, st);
  vmnet_network_configuration_disable_dhcp(config);
  if (hostOnly) vmnet_network_configuration_disable_dns_proxy(config);
  // A previous holder that has just exited may still be releasing the subnet, so retry briefly.
  // One killed while VMs were attached leaves it reserved until InternetSharing exits, a minute
  // after its last client. This process would be such a client, so kido waits for that, not it.
  vmnet_network_ref network = NULL;
  for (int i = 0; i < 5 && !(network = vmnet_network_create(config, &st)); i++) usleep(500000);
  if (!network)
    return report(r, st == VMNET_FAILURE || st == VMNET_SHARING_SERVICE_BUSY ? exitBusy : 1, "vmnet refused %s (status %u)",
                  subnet, st);
  xpc_object_t x = vmnet_network_copy_serialization(network, &st);
  size_t len = 0;
  const void *bytes = x && xpc_dictionary_get_count(x) == 1 ? xpc_dictionary_get_data(x, serializationKey, &len) : NULL;
  if (!bytes) return report(r, 1, "unknown vmnet serialization format (status %u)", st);
  char tmp[PATH_MAX];
  snprintf(tmp, sizeof(tmp), "%s.tmp", path);
  int fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
  bool ok = fd >= 0 && write(fd, bytes, len) == (ssize_t)len;
  if (fd >= 0) close(fd);
  if (!ok || rename(tmp, path) != 0 || stat(path, &gHeld) != 0) return report(r, 1, "write %s: %s", path, strerror(errno));
  strlcpy(gHeldPath, path, sizeof(gHeldPath));
  signal(SIGTERM, holderExit);
  signal(SIGINT, holderExit);
  int lock = open(users, O_RDWR | O_CREAT | O_CLOEXEC, 0600);
  if (lock < 0) {
    unlinkHeld();
    return report(r, 1, "open %s: %s", users, strerror(errno));
  }
  logf_("holding %s%s", subnet, hostOnly ? " (host-only)" : "");
  report(r, 0, "ok");
  // Every VM process holds a shared lock; give the last one time to detach before the network goes.
  for (;;) {
    if (flock(lock, LOCK_EX) != 0) {
      if (errno == EINTR) continue;
      break;
    }
    flock(lock, LOCK_UN);
    sleep(2);
    if (flock(lock, LOCK_EX | LOCK_NB) == 0) break;
  }
  unlinkHeld();
  logf_("no VM uses the network any more");
  return 0;
}

static int holdNetworkMain(int argc, const char **argv) {
  const char *subnet = NULL, *path = NULL, *users = NULL;
  bool hostOnly = false;
  for (int i = 0; i < argc; i++) {
    if (!strcmp(argv[i], "--host-only")) hostOnly = true;
    else if (i + 1 < argc && !strcmp(argv[i], "--subnet")) subnet = argv[++i];
    else if (i + 1 < argc && !strcmp(argv[i], "--network")) path = argv[++i];
    else if (i + 1 < argc && !strcmp(argv[i], "--users")) users = argv[++i];
    else usage();
  }
  if (!subnet || !path || !users || strlen(path) >= sizeof(gHeldPath)) usage();
  FILE *r = fcntl(3, F_GETFD) != -1 ? fdopen(3, "w") : NULL;
  if (@available(macOS 26, *)) return holdNetwork(subnet, hostOnly, path, users, r);
  return report(r, 2, "vmnet networks need macOS 26 or later");
}

API_AVAILABLE(macos(26.0))
static VZNetworkDeviceAttachment *vmnetAttachment(NSString *path) {
  NSError *error = nil;
  NSData *d = [NSData dataWithContentsOfFile:path options:0 error:&error];
  if (!d) {
    fprintf(stderr, "kido-vmm: network %s: %s\n", path.UTF8String, error.localizedDescription.UTF8String);
    return nil;
  }
  xpc_object_t dict = xpc_dictionary_create(NULL, NULL, 0);
  xpc_dictionary_set_data(dict, serializationKey, d.bytes, d.length);
  vmnet_return_t st = 0;
  vmnet_network_ref network = vmnet_network_create_with_serialization(dict, &st);
  if (!network) {
    fprintf(stderr, "kido-vmm: network %s: vmnet status %u\n", path.UTF8String, st);
    return nil;
  }
  return [[VZVmnetNetworkDeviceAttachment alloc] initWithNetwork:network];
}

// MARK: notifications

// Everything here runs on gNotifyQueue, never on the main queue that runs the VM and the signals.
// The write source owns the pipe and the queue: its cancel handler closes and frees them.
static dispatch_queue_t gNotifyQueue;
static notifyq *gNotify;
static dispatch_source_t gNotifyWrite;
static bool gNotifyArmed, gNotifyStopping;
static FSEventStreamRef gNotifyStream;
static char gNotifyPath[NOTIFY_MAX_PATH];

enum { notifyMaxPerCallback = 4096 };

// notifyStop never runs inside the stream's callback, which must return before the stream goes.
static void notifyStop(void) {
  if (gNotifyStream) {
    FSEventStreamStop(gNotifyStream);
    FSEventStreamInvalidate(gNotifyStream);
    FSEventStreamRelease(gNotifyStream);
    gNotifyStream = NULL;
  }
  if (gNotifyWrite) {
    dispatch_source_cancel(gNotifyWrite);
    if (!gNotifyArmed) dispatch_resume(gNotifyWrite);
    gNotifyWrite = NULL;
  }
}

static void notifyPump(void) {
  if (!gNotifyWrite || gNotifyStopping) return;
  int r = notifyq_drain(gNotify);
  if (r < 0) {
    logf_("notifications stopped: the relay is gone");
    gNotifyStopping = true;
    if (gNotifyArmed) {
      dispatch_suspend(gNotifyWrite);
      gNotifyArmed = false;
    }
    dispatch_async(gNotifyQueue, ^{
      notifyStop();
    });
  } else if (r == 1 && !gNotifyArmed) {
    dispatch_resume(gNotifyWrite);
    gNotifyArmed = true;
  } else if (r == 0 && gNotifyArmed) {
    dispatch_suspend(gNotifyWrite);
    gNotifyArmed = false;
  }
}

// notifyEvents queues each event's path as the file system gives it; anything it cannot take as it
// is becomes a flush, which costs the guest its cache, never a wrong answer.
static void notifyEvents(ConstFSEventStreamRef stream, void *info, size_t n, void *paths,
                         const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
  if (!gNotifyWrite || gNotifyStopping) return;
  CFTypeRef events = paths;
  if (n > notifyMaxPerCallback || !events || CFGetTypeID(events) != CFArrayGetTypeID() ||
      (size_t)CFArrayGetCount(events) < n) {
    notifyq_flush(gNotify);
  } else {
    for (size_t i = 0; i < n; i++) {
      CFTypeRef ev = CFArrayGetValueAtIndex(events, (CFIndex)i);
      bool dict = ev && CFGetTypeID(ev) == CFDictionaryGetTypeID();
      CFTypeRef path = dict ? CFDictionaryGetValue(ev, kFSEventStreamEventExtendedDataPathKey) : NULL;
      CFTypeRef idref = dict ? CFDictionaryGetValue(ev, kFSEventStreamEventExtendedFileIDKey) : NULL;
      SInt64 fileid = 0;
      if (idref && CFGetTypeID(idref) == CFNumberGetTypeID()) CFNumberGetValue(idref, kCFNumberSInt64Type, &fileid);
      // The bytes as stored, not the decomposed form CFStringGetFileSystemRepresentation gives: the guest
      // knows each name as the host's server stores it.
      CFIndex len = path && CFGetTypeID(path) == CFStringGetTypeID() ? CFStringGetLength(path) : -1, used = 0;
      if (len < 0 || CFStringGetBytes(path, CFRangeMake(0, len), kCFStringEncodingUTF8, 0, false, (UInt8 *)gNotifyPath,
                                      sizeof(gNotifyPath), &used) != len) {
        notifyq_flush(gNotify);
        continue;
      }
      notifyq_event(gNotify, (uint32_t)flags[i], (uint64_t)fileid, gNotifyPath, (size_t)used);
    }
  }
  notifyPump();
}

// notifyStart takes fd: on failure too, it is closed by the time this returns or soon after.
static bool notifyStart(int fd, NSArray<NSString *> *roots) {
  gNotifyQueue = dispatch_queue_create("kido-vmm.notify", DISPATCH_QUEUE_SERIAL);
  gNotify = fcntl(fd, F_SETFD, FD_CLOEXEC) == 0 ? notifyq_new(fd, 1 << 20) : NULL;
  FSEventStreamContext ctx = {0};
  gNotifyStream = gNotify ? FSEventStreamCreate(NULL, notifyEvents, &ctx, (__bridge CFArrayRef)roots,
                                                kFSEventStreamEventIdSinceNow, 0.01,
                                                kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer |
                                                    kFSEventStreamCreateFlagUseCFTypes |
                                                    kFSEventStreamCreateFlagUseExtendedData |
                                                    kFSEventStreamCreateFlagWatchRoot)
                          : NULL;
  gNotifyWrite = gNotifyStream ? dispatch_source_create(DISPATCH_SOURCE_TYPE_WRITE, (uintptr_t)fd, 0, gNotifyQueue) : NULL;
  if (!gNotifyWrite) {
    if (gNotifyStream) FSEventStreamRelease(gNotifyStream);
    gNotifyStream = NULL;
    notifyq_free(gNotify);
    gNotify = NULL;
    close(fd);
    return false;
  }
  dispatch_source_set_event_handler(gNotifyWrite, ^{
    notifyPump();
  });
  dispatch_source_set_cancel_handler(gNotifyWrite, ^{
    notifyq_free(gNotify);
    gNotify = NULL;
    close(fd);
  });
  FSEventStreamSetDispatchQueue(gNotifyStream, gNotifyQueue);
  if (FSEventStreamStart(gNotifyStream)) return true;
  dispatch_async(gNotifyQueue, ^{
    notifyStop();
  });
  return false;
}

// MARK: VM

@interface Watcher : NSObject <VZVirtualMachineDelegate>
@end

@implementation Watcher
- (void)guestDidStopVirtualMachine:(VZVirtualMachine *)vm {
  logf_("guest powered off");
  writeState(VZVirtualMachineStateStopped);
  exit(0);
}
- (void)virtualMachine:(VZVirtualMachine *)vm didStopWithError:(NSError *)error {
  logf_("stopped with error: %s", error.localizedDescription.UTF8String);
  writeState(VZVirtualMachineStateError);
  exit(3);
}
- (void)virtualMachine:(VZVirtualMachine *)vm
             networkDevice:(VZNetworkDevice *)networkDevice
    attachmentWasDisconnectedWithError:(NSError *)error {
  logf_("network attachment disconnected: %s", error.localizedDescription.UTF8String);
}
- (void)observeValueForKeyPath:(NSString *)keyPath ofObject:(id)object change:(NSDictionary *)change context:(void *)context {
  VZVirtualMachineState s = ((VZVirtualMachine *)object).state;
  logf_("state %s", stateName(s));
  writeState(s);
}
@end

static void usage(void) {
  fprintf(stderr, "usage: kido-vmm --kernel IMAGE --initrd CPIO --disk RAW --cmdline-file FILE [--cpus N] [--memory MB]\n"
                  "               [--mac MAC] [--console FILE [--console-max BYTES]] [--state-file FILE] [--disk-sync full|fsync|none]\n"
                  "               [--network FILE] [--forward tcp|udp:HOSTIP:HOSTPORT:GUESTIP:GUESTPORT ...]\n"
                  "               [--extra-disk RAW ...] [--extra-disk-ro RAW ...]\n"
                  "               [--share NAME=DIR ...] [--share-ro NAME=DIR ...] [--notify-fd N --watch DIR ...]\n"
                  "       kido-vmm hold-network --subnet A.B.C.D/N [--host-only] --network FILE --users FILE\n");
  exit(2);
}

static void hardStop(VZVirtualMachine *vm, const char *why) {
  logf_("%s: stopping (state %s)", why, stateName(vm.state));
  if (!vm.canStop) exit(0);
  [vm stopWithCompletionHandler:^(NSError *err) {
    if (err) logf_("stop: %s", err.localizedDescription.UTF8String);
    writeState(VZVirtualMachineStateStopped);
    exit(err ? 4 : 0);
  }];
}

static dispatch_source_t onSignal(int sig, void (^handler)(void)) {
  signal(sig, SIG_IGN);
  dispatch_source_t s = dispatch_source_create(DISPATCH_SOURCE_TYPE_SIGNAL, (uintptr_t)sig, 0, dispatch_get_main_queue());
  dispatch_source_set_event_handler(s, handler);
  dispatch_resume(s);
  return s;
}

int main(int argc, const char **argv) {
  @autoreleasepool {
    gStarted = [NSDate date];
    signal(SIGPIPE, SIG_IGN);
    if (argc > 1 && !strcmp(argv[1], "hold-network")) return holdNetworkMain(argc - 2, argv + 2);
    NSString *kernel, *initrd, *disk, *cmdlineFile, *mac, *console, *network, *sync = @"full";
    NSInteger cpus = 2, memoryMB = 1024;
    long long consoleMax = 2 << 20;
    NSMutableArray<NSValue *> *forwards = [NSMutableArray array];
    NSMutableArray<NSString *> *extraDisks = [NSMutableArray array];
    NSMutableArray<NSNumber *> *extraReadOnly = [NSMutableArray array];
    NSMutableDictionary<NSString *, VZSharedDirectory *> *shares = [NSMutableDictionary dictionary];
    NSMutableArray<NSString *> *watch = [NSMutableArray array];
    int notifyFD = -1;
    for (int i = 1; i < argc; i++) {
      const char *a = argv[i];
      if (i + 1 >= argc) usage();
      NSString *v = @(argv[++i]);
      if (!strcmp(a, "--kernel")) kernel = v;
      else if (!strcmp(a, "--initrd")) initrd = v;
      else if (!strcmp(a, "--disk")) disk = v;
      else if (!strcmp(a, "--cmdline-file")) cmdlineFile = v;
      else if (!strcmp(a, "--cpus")) cpus = v.integerValue;
      else if (!strcmp(a, "--memory")) memoryMB = v.integerValue;
      else if (!strcmp(a, "--mac")) mac = v;
      else if (!strcmp(a, "--console")) console = v;
      else if (!strcmp(a, "--console-max")) consoleMax = v.longLongValue;
      else if (!strcmp(a, "--state-file")) gStateFile = v;
      else if (!strcmp(a, "--disk-sync")) sync = v;
      else if (!strcmp(a, "--network")) network = v;
      else if (!strcmp(a, "--extra-disk") || !strcmp(a, "--extra-disk-ro")) {
        [extraDisks addObject:v];
        [extraReadOnly addObject:@(strcmp(a, "--extra-disk-ro") == 0)];
      } else if (!strcmp(a, "--share") || !strcmp(a, "--share-ro")) {
        NSRange eq = [v rangeOfString:@"="];
        NSString *name = eq.location == NSNotFound ? @"" : [v substringToIndex:eq.location];
        NSString *dir = eq.location == NSNotFound ? @"" : [v substringFromIndex:eq.location + 1];
        NSError *bad = nil;
        BOOL isDir = NO;
        NSString *why = nil;
        if (![VZMultipleDirectoryShare validateName:name error:&bad]) why = bad.localizedDescription ?: @"invalid name";
        else if (shares[name]) why = @"the name is given twice";
        else if (![dir hasPrefix:@"/"]) why = @"DIR is not an absolute path";
        else if (![NSFileManager.defaultManager fileExistsAtPath:dir isDirectory:&isDir] || !isDir)
          why = @"DIR is not a directory";
        if (why) {
          fprintf(stderr, "kido-vmm: bad %s %s: %s\n", a, v.UTF8String, why.UTF8String);
          return 2;
        }
        shares[name] = [[VZSharedDirectory alloc] initWithURL:[NSURL fileURLWithPath:dir]
                                                     readOnly:strcmp(a, "--share-ro") == 0];
      } else if (!strcmp(a, "--notify-fd")) {
        notifyFD = v.intValue;
        struct stat st;
        if (notifyFD < 3 || fstat(notifyFD, &st) != 0 || !S_ISFIFO(st.st_mode)) {
          fprintf(stderr, "kido-vmm: bad --notify-fd %s: not a pipe\n", v.UTF8String);
          return 2;
        }
      } else if (!strcmp(a, "--watch")) {
        if (![v hasPrefix:@"/"]) {
          fprintf(stderr, "kido-vmm: bad --watch %s: not an absolute path\n", v.UTF8String);
          return 2;
        }
        [watch addObject:v];
      } else if (!strcmp(a, "--forward")) {
        forward_t *f = parseForward(v.UTF8String);
        if (!f) {
          fprintf(stderr, "kido-vmm: bad --forward %s\n", v.UTF8String);
          return 2;
        }
        [forwards addObject:[NSValue valueWithPointer:f]];
      } else usage();
    }
    if (!kernel || !initrd || !disk || !cmdlineFile) usage();
    if (notifyFD >= 0 && !watch.count) close(notifyFD);
    else if (notifyFD >= 0 && !notifyStart(notifyFD, watch)) logf_("notifications off");

    NSError *error = nil;
    NSString *cmdline = [NSString stringWithContentsOfFile:cmdlineFile encoding:NSUTF8StringEncoding error:&error];
    if (!cmdline) {
      fprintf(stderr, "kido-vmm: %s\n", error.localizedDescription.UTF8String);
      return 1;
    }
    unlink(cmdlineFile.fileSystemRepresentation);

    VZLinuxBootLoader *boot = [[VZLinuxBootLoader alloc] initWithKernelURL:[NSURL fileURLWithPath:kernel]];
    boot.initialRamdiskURL = [NSURL fileURLWithPath:initrd];
    boot.commandLine = [cmdline stringByTrimmingCharactersInSet:NSCharacterSet.whitespaceAndNewlineCharacterSet];

    VZVirtualMachineConfiguration *config = [VZVirtualMachineConfiguration new];
    config.platform = [[VZGenericPlatformConfiguration alloc] init];
    config.bootLoader = boot;
    config.CPUCount = (NSUInteger)cpus;
    config.memorySize = (unsigned long long)memoryMB << 20;
    config.entropyDevices = @[ [[VZVirtioEntropyDeviceConfiguration alloc] init] ];

    if (console) {
      int p[2];
      console_t *c = calloc(1, sizeof(*c));
      pthread_t t;
      if (consoleMax < 4096 || strlcpy(c->path, console.fileSystemRepresentation, sizeof(c->path)) >= sizeof(c->path) ||
          pipe(p) != 0) {
        fprintf(stderr, "kido-vmm: console %s: %s\n", console.UTF8String, strerror(errno));
        return 1;
      }
      fcntl(p[0], F_SETFD, FD_CLOEXEC);
      fcntl(p[1], F_SETFD, FD_CLOEXEC);
      c->in = p[0];
      c->max = consoleMax;
      pthread_create(&t, NULL, consoleLoop, c);
      int fd = p[1];
      VZVirtioConsoleDeviceSerialPortConfiguration *port = [[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
      port.attachment = [[VZFileHandleSerialPortAttachment alloc]
          initWithFileHandleForReading:nil
                  fileHandleForWriting:[[NSFileHandle alloc] initWithFileDescriptor:fd closeOnDealloc:YES]];
      config.serialPorts = @[ port ];
    }

    VZDiskImageSynchronizationMode syncMode = VZDiskImageSynchronizationModeFull;
    if ([sync isEqualToString:@"fsync"]) syncMode = VZDiskImageSynchronizationModeFsync;
    else if ([sync isEqualToString:@"none"]) syncMode = VZDiskImageSynchronizationModeNone;
    else if (![sync isEqualToString:@"full"]) usage();
    VZDiskImageStorageDeviceAttachment *attachment =
        [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:[NSURL fileURLWithPath:disk]
                                                       readOnly:NO
                                                    cachingMode:VZDiskImageCachingModeAutomatic
                                            synchronizationMode:syncMode
                                                          error:&error];
    if (!attachment) {
      fprintf(stderr, "kido-vmm: disk: %s\n", error.localizedDescription.UTF8String);
      return 1;
    }
    NSMutableArray<VZStorageDeviceConfiguration *> *storage =
        [NSMutableArray arrayWithObject:[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:attachment]];
    for (NSUInteger i = 0; i < extraDisks.count; i++) {
      VZDiskImageStorageDeviceAttachment *extra =
          [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:[NSURL fileURLWithPath:extraDisks[i]]
                                                         readOnly:extraReadOnly[i].boolValue
                                                      cachingMode:VZDiskImageCachingModeAutomatic
                                              synchronizationMode:syncMode
                                                            error:&error];
      if (!extra) {
        fprintf(stderr, "kido-vmm: disk %s: %s\n", extraDisks[i].UTF8String, error.localizedDescription.UTF8String);
        return 1;
      }
      [storage addObject:[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:extra]];
    }
    config.storageDevices = storage;

    VZVirtioNetworkDeviceConfiguration *net = [[VZVirtioNetworkDeviceConfiguration alloc] init];
    if (!network) {
      net.attachment = [[VZNATNetworkDeviceAttachment alloc] init];
    } else if (@available(macOS 26, *)) {
      if (!(net.attachment = vmnetAttachment(network))) return 1;
    } else {
      fprintf(stderr, "kido-vmm: --network needs macOS 26 or later\n");
      return 2;
    }
    if (mac) {
      VZMACAddress *m = [[VZMACAddress alloc] initWithString:mac];
      if (!m) {
        fprintf(stderr, "kido-vmm: bad MAC %s\n", mac.UTF8String);
        return 2;
      }
      net.MACAddress = m;
    }
    config.networkDevices = @[ net ];

    if (shares.count) {
      VZVirtioFileSystemDeviceConfiguration *fs = [[VZVirtioFileSystemDeviceConfiguration alloc] initWithTag:@"kido"];
      fs.share = [[VZMultipleDirectoryShare alloc] initWithDirectories:shares];
      config.directorySharingDevices = @[ fs ];
    }

    if (![config validateWithError:&error]) {
      fprintf(stderr, "kido-vmm: invalid configuration: %s\n", error.localizedDescription.UTF8String);
      return 1;
    }
    for (NSValue *v in forwards) {
      forward_t *f = v.pointerValue;
      char ip[INET_ADDRSTRLEN];
      inet_ntop(AF_INET, &f->host.sin_addr, ip, sizeof(ip));
      if (listenOn(f) != 0) {
        fprintf(stderr, "kido-vmm: listen %s:%d: %s\n", ip, ntohs(f->host.sin_port), strerror(errno));
        return 1;
      }
    }

    VZVirtualMachine *vm = [[VZVirtualMachine alloc] initWithConfiguration:config];
    Watcher *watcher = [Watcher new];
    vm.delegate = watcher;
    [vm addObserver:watcher forKeyPath:@"state" options:NSKeyValueObservingOptionNew context:NULL];

    dispatch_source_t term = onSignal(SIGTERM, ^{ hardStop(vm, "SIGTERM"); });
    dispatch_source_t intr = onSignal(SIGINT, ^{ hardStop(vm, "SIGINT"); });
    dispatch_source_t usr1 = onSignal(SIGUSR1, ^{
      if (!vm.canPause) return;
      [vm pauseWithCompletionHandler:^(NSError *err) {
        if (err) logf_("pause: %s", err.localizedDescription.UTF8String);
      }];
    });
    dispatch_source_t usr2 = onSignal(SIGUSR2, ^{
      if (!vm.canResume) return;
      [vm resumeWithCompletionHandler:^(NSError *err) {
        if (err) logf_("resume: %s", err.localizedDescription.UTF8String);
      }];
    });
    (void)term, (void)intr, (void)usr1, (void)usr2;

    logf_("cpus %ld, memory %ld MB, MAC %s, %s, %lu forwards, %lu shares", (long)cpus, (long)memoryMB,
          net.MACAddress.string.UTF8String, network ? "vmnet network" : "NAT", (unsigned long)forwards.count,
          (unsigned long)shares.count);
    [vm startWithCompletionHandler:^(NSError *err) {
      if (err) {
        logf_("start failed: %s", err.localizedDescription.UTF8String);
        writeState(VZVirtualMachineStateError);
        exit(1);
      }
      logf_("started");
    }];
    dispatch_main();
  }
}
