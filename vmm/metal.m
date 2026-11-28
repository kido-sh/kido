/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#import "metal.h"

#if defined(__MAC_27_0)

#import "kmtl_proto.h"
#import "kmtl_shm.h"

#import <Metal/Metal.h>
#include <errno.h>
#include <math.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

enum {
  KMTL_MAX_CLIENTS = 256,
  KMTL_MAX_INFLIGHT_CMDS_PER_QUEUE = 256,
  KMTL_MAX_GLOBAL_INFLIGHT_CMDS = 1024,
  KMTL_MAX_ACTIVE_COMPILES = 64,
  KMTL_MAX_QUEUE_WAITERS = 64,
  KMTL_MAX_QUEUE_ERRORS = 64,
  KMTL_MAX_FUNC_CONST_INDEX = 65535,
};

typedef struct {
  uint64_t seq;
  uint64_t gpuStartNs;
  uint64_t gpuEndNs;
} kmtl_seq_timing_t;

API_AVAILABLE(macos(27.0))
@interface KidoMetalPendingElem : NSObject
@property (nonatomic, strong, nullable) VZVirtioQueueElement *elem;
@property (nonatomic) uint64_t generation;
@property (nonatomic) uint32_t clientId;
@property (nonatomic) uint64_t sessionToken;
@property (nonatomic) BOOL done;
@end

@implementation KidoMetalPendingElem
@end

@interface KidoMetalClientState : NSObject
@property (nonatomic) uint32_t clientId;
@property (nonatomic) uint64_t sessionToken;
@property (nonatomic) NSUInteger pendingBuffers;
@property (nonatomic) NSUInteger pendingLibraries;
@property (nonatomic) NSUInteger pendingFunctions;
@property (nonatomic) NSUInteger pendingPipelines;
@property (nonatomic, strong) dispatch_group_t pendingMapsGroup;
@end

@implementation KidoMetalClientState
- (instancetype)init {
  self = [super init];
  if (self) {
    _pendingMapsGroup = dispatch_group_create();
  }
  return self;
}
@end

@interface KidoMetalBufferSlot : NSObject
@property (nonatomic) uint64_t handle;
@property (nonatomic) uint32_t clientId;
@property (nonatomic) uint64_t shmOffset;
@property (nonatomic) uint64_t alignedLen;
@property (nonatomic) uint64_t requestedLen;
@property (nonatomic) void *hostPtr;
@property (nonatomic, strong, nullable) id<MTLBuffer> mtlBuffer;
@property (nonatomic) uint64_t inFlightRefs;
@property (nonatomic) BOOL destroyed;
@property (nonatomic) BOOL unmapping;
@property (nonatomic) BOOL quarantined;
@property (nonatomic, strong, nullable) NSMutableArray<dispatch_group_t> *releaseGroups;
@end

@implementation KidoMetalBufferSlot
@end

API_AVAILABLE(macos(27.0))
@interface KidoMetalQueueWaiter : NSObject
@property (nonatomic) uint64_t targetSeq;
@property (nonatomic, strong) KidoMetalPendingElem *pending;
@end

@implementation KidoMetalQueueWaiter
@end

API_AVAILABLE(macos(27.0))
@interface KidoMetalQueueState : NSObject {
@public
  kmtl_seq_timing_t timings[64];
}
@property (nonatomic, strong) id<MTLCommandQueue> queue;
@property (nonatomic) uint64_t submittedSeq;
@property (nonatomic) uint64_t completedSeq;
@property (nonatomic) uint64_t lastGpuStartNs;
@property (nonatomic) uint64_t lastGpuEndNs;
@property (nonatomic, strong) NSMutableSet<NSNumber *> *pendingSeqs;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, NSString *> *errors;
@property (nonatomic, strong) NSMutableArray<KidoMetalQueueWaiter *> *waiters;
@property (nonatomic, strong) NSMutableSet<KidoMetalPendingElem *> *commitWaiters;
@end

@implementation KidoMetalQueueState
- (instancetype)initWithQueue:(id<MTLCommandQueue>)queue {
  self = [super init];
  if (self) {
    _queue = queue;
    _pendingSeqs = [NSMutableSet set];
    _errors = [NSMutableDictionary dictionary];
    _waiters = [NSMutableArray array];
    _commitWaiters = [NSMutableSet set];
    memset(timings, 0, sizeof(timings));
  }
  return self;
}
@end

@interface KidoMetalObjEntry : NSObject
@property (nonatomic) uint64_t handle;
@property (nonatomic) uint32_t clientId;
@property (nonatomic, strong) id obj;
@end

@implementation KidoMetalObjEntry
@end

@interface KidoMetalDevice () {
  kmtl_shm_alloc_t _shmAlloc;
  BOOL _loggedDriverReady;
}
@property (nonatomic, strong) id<MTLDevice> gpu;
@property (nonatomic, strong) dispatch_queue_t deviceQueue;
@property (nonatomic, strong) dispatch_group_t inFlightGroup;
@property (nonatomic, strong, nullable) VZCustomVirtioDevice *virtioDevice;
@property (nonatomic, strong, nullable) VZVirtioSharedMemoryRegion *shmRegion;
@property (nonatomic) uint32_t hostPageSize;
@property (nonatomic) uint64_t memoryLimitBytes;
@property (nonatomic) uint64_t usedBytes;
@property (nonatomic) uint64_t nextHandle;
@property (nonatomic) uint64_t nextSessionToken;
@property (nonatomic) uint64_t deviceGeneration;
@property (nonatomic) NSUInteger globalInFlightCmds;
@property (nonatomic) NSUInteger activeCompiles;
@property (nonatomic) BOOL stopped;
@property (nonatomic) BOOL vmStopped;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalClientState *> *clients;
@property (nonatomic, strong) NSMutableSet<KidoMetalPendingElem *> *pendingElems;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalBufferSlot *> *buffers;
@property (nonatomic, strong) NSMutableSet<KidoMetalBufferSlot *> *drainingBuffers;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *libraries;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *functions;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *pipelines;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *queues;
@end

static uint64_t alignUpU64(uint64_t v, uint64_t align) {
  if (align <= 1) return v;
  uint64_t rem = v % align;
  if (rem == 0) return v;
  if (v > UINT64_MAX - (align - rem)) return 0;
  return v + (align - rem);
}

static uint64_t secondsToNsSafe(double t) {
  if (!isfinite(t) || t <= 0.0) return 0;
  double ns = t * 1e9;
  if (!isfinite(ns) || ns >= (double)UINT64_MAX) return UINT64_MAX;
  return (uint64_t)ns;
}

static uint64_t probeGPUFamilyMask(id<MTLDevice> gpu) {
  uint64_t m = 0;
  if ([gpu supportsFamily:MTLGPUFamilyApple1]) m |= KMTL_FAM_APPLE1;
  if ([gpu supportsFamily:MTLGPUFamilyApple2]) m |= KMTL_FAM_APPLE2;
  if ([gpu supportsFamily:MTLGPUFamilyApple3]) m |= KMTL_FAM_APPLE3;
  if ([gpu supportsFamily:MTLGPUFamilyApple4]) m |= KMTL_FAM_APPLE4;
  if ([gpu supportsFamily:MTLGPUFamilyApple5]) m |= KMTL_FAM_APPLE5;
  if ([gpu supportsFamily:MTLGPUFamilyApple6]) m |= KMTL_FAM_APPLE6;
  if ([gpu supportsFamily:MTLGPUFamilyApple7]) m |= KMTL_FAM_APPLE7;
  if ([gpu supportsFamily:MTLGPUFamilyApple8]) m |= KMTL_FAM_APPLE8;
  if ([gpu supportsFamily:MTLGPUFamilyApple9]) m |= KMTL_FAM_APPLE9;
#if defined(__MAC_26_0)
  if (@available(macOS 26.0, *)) {
    if ([gpu supportsFamily:MTLGPUFamilyApple10]) m |= KMTL_FAM_APPLE10;
  }
#endif
  if ([gpu supportsFamily:MTLGPUFamilyCommon1]) m |= KMTL_FAM_COMMON1;
  if ([gpu supportsFamily:MTLGPUFamilyCommon2]) m |= KMTL_FAM_COMMON2;
  if ([gpu supportsFamily:MTLGPUFamilyCommon3]) m |= KMTL_FAM_COMMON3;
  if ([gpu supportsFamily:MTLGPUFamilyMac2]) m |= KMTL_FAM_MAC2;
  if ([gpu supportsFamily:MTLGPUFamilyMetal3]) m |= KMTL_FAM_METAL3;
#if defined(__MAC_26_0)
  if (@available(macOS 26.0, *)) {
    if ([gpu supportsFamily:MTLGPUFamilyMetal4]) m |= KMTL_FAM_METAL4;
  }
#endif
  return m;
}

API_AVAILABLE(macos(27.0))
static uint32_t maxExtraBytesForElem(VZVirtioQueueElement *elem) {
  NSUInteger avail = elem.writeBuffersAvailableByteCount;
  if (avail <= sizeof(struct kmtl_resp_hdr)) return 0;
  NSUInteger maxExtra = avail - sizeof(struct kmtl_resp_hdr);
  if (maxExtra > KMTL_MAX_OUT_PAYLOAD) maxExtra = KMTL_MAX_OUT_PAYLOAD;
  return (uint32_t)maxExtra;
}

API_AVAILABLE(macos(27.0))
static void replyElemNow(VZVirtioQueueElement *elem, struct kmtl_resp_hdr resp, const void *extra, uint32_t extraLen) {
  NSUInteger avail = elem.writeBuffersAvailableByteCount;
  if (avail < sizeof(resp)) {
    [elem returnToQueue];
    return;
  }
  uint32_t maxExtra = maxExtraBytesForElem(elem);
  if (extraLen > maxExtra) extraLen = maxExtra;
  resp.extra_len = extraLen;
  NSError *err = nil;
  [elem writeBuffer:&resp exactLength:sizeof(resp) error:&err];
  if (!err && extra && extraLen > 0) {
    [elem writeBuffer:(void *)extra exactLength:extraLen error:&err];
  }
  [elem returnToQueue];
}

API_AVAILABLE(macos(27.0))
static void replyErrorNow(VZVirtioQueueElement *elem, int32_t status, NSString *_Nullable msg) {
  struct kmtl_resp_hdr resp = {0};
  resp.status = status;
  if (msg.length > 0) {
    NSData *d = [msg dataUsingEncoding:NSUTF8StringEncoding];
    uint32_t len = (uint32_t)MIN(d.length, (NSUInteger)65536);
    replyElemNow(elem, resp, d.bytes, len);
  } else {
    replyElemNow(elem, resp, NULL, 0);
  }
}

static NSData *_Nullable encodeFunctionNames(id<MTLLibrary> lib, uint32_t maxBytes) {
  if (maxBytes == 0) return nil;
  NSArray<NSString *> *names = lib.functionNames;
  if (names.count == 0) return nil;
  NSMutableData *buf = [NSMutableData dataWithCapacity:MIN(names.count * 32, (NSUInteger)maxBytes)];
  for (NSString *n in names) {
    NSData *d = [n dataUsingEncoding:NSUTF8StringEncoding];
    if (d.length == 0 || memchr(d.bytes, 0, d.length) != NULL) continue;
    if (d.length >= maxBytes || buf.length > (NSUInteger)maxBytes - d.length - 1) break;
    [buf appendData:d];
    uint8_t nul = 0;
    [buf appendBytes:&nul length:1];
  }
  return buf;
}

@implementation KidoMetalDevice

- (nullable instancetype)initWithMemoryLimitBytes:(uint64_t)limitBytes error:(NSError **)errorOut {
  self = [super init];
  if (!self) return nil;

  id<MTLDevice> gpu = MTLCreateSystemDefaultDevice();
  if (!gpu) {
    if (errorOut) {
      *errorOut = [NSError errorWithDomain:@"sh.kido.metal"
                                      code:1
                                  userInfo:@{NSLocalizedDescriptionKey : @"no Metal GPU device available on host"}];
    }
    return nil;
  }
  _gpu = gpu;
  _hostPageSize = (uint32_t)getpagesize();
  if (_hostPageSize < 4096) _hostPageSize = 16384;

  uint64_t recMax = (uint64_t)gpu.recommendedMaxWorkingSetSize;
  if (recMax == 0) recMax = 4ULL << 30;
  uint64_t defaultLimit = recMax / 2;
  if (defaultLimit > (8ULL << 30)) defaultLimit = 8ULL << 30;
  if (defaultLimit < (256ULL << 20)) defaultLimit = 256ULL << 20;

  uint64_t lim = limitBytes > 0 ? limitBytes : defaultLimit;
  if (lim > recMax) lim = recMax;
  if (lim < (64ULL << 20)) lim = 64ULL << 20;
  lim = (lim / _hostPageSize) * _hostPageSize;
  _memoryLimitBytes = lim;

  if (!kmtl_shm_init(&_shmAlloc, _memoryLimitBytes)) {
    if (errorOut) {
      *errorOut = [NSError errorWithDomain:@"sh.kido.metal"
                                      code:2
                                  userInfo:@{NSLocalizedDescriptionKey : @"failed to initialize SHM allocator"}];
    }
    return nil;
  }

  dispatch_queue_attr_t qattr =
      dispatch_queue_attr_make_with_qos_class(DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0);
  _deviceQueue = dispatch_queue_create("sh.kido.vmm.metal", qattr);
  _inFlightGroup = dispatch_group_create();
  _nextHandle = 1;
  _nextSessionToken = 1;
  _deviceGeneration = 1;
  _clients = [NSMutableDictionary dictionary];
  _pendingElems = [NSMutableSet set];
  _buffers = [NSMutableDictionary dictionary];
  _drainingBuffers = [NSMutableSet set];
  _libraries = [NSMutableDictionary dictionary];
  _functions = [NSMutableDictionary dictionary];
  _pipelines = [NSMutableDictionary dictionary];
  _queues = [NSMutableDictionary dictionary];
  return self;
}

- (void)dealloc {
  kmtl_shm_destroy(&_shmAlloc);
}

#pragma mark - Pending Element Tracking

- (KidoMetalPendingElem *)trackPendingElem:(VZVirtioQueueElement *)elem
                                  clientId:(uint32_t)clientId
                              sessionToken:(uint64_t)token {
  KidoMetalPendingElem *pe = [KidoMetalPendingElem new];
  pe.elem = elem;
  pe.generation = self.deviceGeneration;
  pe.clientId = clientId;
  pe.sessionToken = token;
  [self.pendingElems addObject:pe];
  return pe;
}

- (void)completePendingElem:(KidoMetalPendingElem *)pe
                       resp:(struct kmtl_resp_hdr)resp
                      extra:(const void *_Nullable)extra
                   extraLen:(uint32_t)extraLen {
  if (!pe || pe.done) return;
  pe.done = YES;
  [self.pendingElems removeObject:pe];
  VZVirtioQueueElement *elem = pe.elem;
  pe.elem = nil;
  if (!elem || pe.generation != self.deviceGeneration || self.vmStopped) return;
  replyElemNow(elem, resp, extra, extraLen);
}

- (void)failPendingElem:(KidoMetalPendingElem *)pe status:(int32_t)status message:(NSString *_Nullable)msg {
  if (!pe || pe.done) return;
  pe.done = YES;
  [self.pendingElems removeObject:pe];
  VZVirtioQueueElement *elem = pe.elem;
  pe.elem = nil;
  if (!elem || pe.generation != self.deviceGeneration || self.vmStopped) return;
  replyErrorNow(elem, status, msg);
}

- (void)failAllPendingElemsWithStatus:(int32_t)status message:(NSString *)msg returnToQueue:(BOOL)returnToQueue {
  NSArray<KidoMetalPendingElem *> *all = [self.pendingElems allObjects];
  [self.pendingElems removeAllObjects];
  for (KidoMetalPendingElem *pe in all) {
    if (pe.done) continue;
    pe.done = YES;
    VZVirtioQueueElement *elem = pe.elem;
    pe.elem = nil;
    if (elem && returnToQueue && !self.vmStopped) {
      replyErrorNow(elem, status, msg);
    }
  }
}

- (BOOL)isSessionValid:(uint32_t)clientId token:(uint64_t)token generation:(uint64_t)gen {
  if (self.stopped || self.vmStopped || gen != self.deviceGeneration) return NO;
  KidoMetalClientState *cs = self.clients[@(clientId)];
  return cs != nil && cs.sessionToken == token;
}

#pragma mark - Configuration

- (uint32_t)featureFlags {
  uint32_t f = KMTL_FEAT_NATIVE_METALLIB;
  if (_gpu.hasUnifiedMemory) f |= KMTL_FEAT_UNIFIED_MEMORY;
  if ([_gpu supportsFamily:MTLGPUFamilyApple7]) f |= KMTL_FEAT_SIMDGROUP_MATRIX;
  if ([_gpu supportsFamily:MTLGPUFamilyApple9] || [_gpu supportsFamily:MTLGPUFamilyMetal3]) f |= KMTL_FEAT_BFLOAT16;
  return f;
}

- (VZCustomVirtioDeviceConfiguration *)makeConfiguration {
  VZCustomVirtioDeviceConfiguration *cfg = [[VZCustomVirtioDeviceConfiguration alloc] init];
  cfg.deviceID = KMTL_VIRTIO_DEVICE_ID;
  cfg.PCIClassID = KMTL_PCI_CLASS_ID;
  cfg.PCISubclassID = KMTL_PCI_SUBCLASS_ID;
  cfg.virtioQueueCount = 2;

  struct kmtl_device_config devCfg = {0};
  devCfg.magic = KMTL_MAGIC;
  devCfg.version = KMTL_PROTO_VERSION;
  devCfg.host_page_size = _hostPageSize;
  devCfg.flags = [self featureFlags];
  devCfg.max_buffer_length = MIN((uint64_t)_gpu.maxBufferLength, _memoryLimitBytes);
  devCfg.recommended_max_working_set_size = _memoryLimitBytes;
  devCfg.shm_aperture_bytes = _memoryLimitBytes;
  MTLSize mt = _gpu.maxThreadsPerThreadgroup;
  devCfg.max_threads_per_threadgroup[0] = (uint32_t)mt.width;
  devCfg.max_threads_per_threadgroup[1] = (uint32_t)mt.height;
  devCfg.max_threads_per_threadgroup[2] = (uint32_t)mt.depth;
  devCfg.max_threadgroup_memory_length = (uint32_t)_gpu.maxThreadgroupMemoryLength;
  devCfg.registry_id = _gpu.registryID;

  NSData *cfgData = [NSData dataWithBytes:&devCfg length:sizeof(devCfg)];
  cfg.deviceSpecificConfiguration = [[VZVirtioDeviceSpecificConfiguration alloc] initWithConfigurationData:cfgData];

  VZVirtioSharedMemoryRegionConfiguration *shm =
      [[VZVirtioSharedMemoryRegionConfiguration alloc] initWithRegionID:KMTL_SHM_REGION_ID size:_memoryLimitBytes];
  cfg.sharedMemoryRegions = @[ shm ];
  cfg.provider = [[VZCustomVirtioDeviceDelegateProvider alloc] initWithDeviceQueue:_deviceQueue delegate:self];
  return cfg;
}

#pragma mark - VZCustomVirtioDeviceConfigurationDelegate

- (void)customVirtioConfiguration:(VZCustomVirtioDeviceConfiguration *)configuration
                  didCreateDevice:(VZCustomVirtioDevice *)device {
  self.virtioDevice = device;
  device.delegate = self;
  if (device.sharedMemoryRegions.count > 0) {
    self.shmRegion = device.sharedMemoryRegions[0];
  }
  kido_vmm_logf("virtio-metal ready: %s (shm %llu MiB)",
                _gpu.name.UTF8String, (unsigned long long)(_memoryLimitBytes >> 20));
}

#pragma mark - Resource Teardown

- (void)signalBufferReleaseGroups:(KidoMetalBufferSlot *)slot {
  if (slot.releaseGroups.count == 0) return;
  NSArray<dispatch_group_t> *groups = [slot.releaseGroups copy];
  [slot.releaseGroups removeAllObjects];
  for (dispatch_group_t g in groups) {
    dispatch_group_leave(g);
  }
}

- (void)attachReleaseGroup:(dispatch_group_t)group toBufferSlot:(KidoMetalBufferSlot *)slot {
  if (!group || !slot || slot.quarantined) return;
  if (!slot.releaseGroups) slot.releaseGroups = [NSMutableArray array];
  dispatch_group_enter(group);
  [slot.releaseGroups addObject:group];
}

- (void)maybeReleaseBufferSlot:(KidoMetalBufferSlot *)slot {
  if (!slot.destroyed) return;
  if (slot.quarantined) {
    [self.drainingBuffers removeObject:slot];
    [self signalBufferReleaseGroups:slot];
    return;
  }
  if (slot.inFlightRefs > 0 || slot.unmapping) {
    [self.drainingBuffers addObject:slot];
    return;
  }
  slot.unmapping = YES;
  [self.drainingBuffers addObject:slot];
  uint64_t off = slot.shmOffset;
  uint64_t len = slot.alignedLen;
  void *ptr = slot.hostPtr;
  slot.hostPtr = NULL;

  VZVirtioSharedMemoryRegion *shm = self.shmRegion;
  if (shm && !self.vmStopped) {
    [shm unmapMemoryAtOffset:off
                        size:len
           completionHandler:^(NSError *_Nullable err) {
             slot.mtlBuffer = nil;
             [self.drainingBuffers removeObject:slot];
             if (err) {
               /* Keep host mapping and SHM range quarantined so guest PTEs cannot alias freed memory. */
               slot.quarantined = YES;
               slot.hostPtr = ptr;
               kido_vmm_logf("virtio-metal: unmapMemory failed at 0x%llx (%llu B): %s",
                             (unsigned long long)off, (unsigned long long)len,
                             err.localizedDescription.UTF8String);
               [self signalBufferReleaseGroups:slot];
               return;
             }
             if (ptr) munmap(ptr, (size_t)len);
             if (self.usedBytes >= len) self.usedBytes -= len;
             else self.usedBytes = 0;
             kmtl_shm_free(&self->_shmAlloc, off, len);
             [self signalBufferReleaseGroups:slot];
           }];
  } else {
    slot.mtlBuffer = nil;
    [self.drainingBuffers removeObject:slot];
    if (ptr) munmap(ptr, (size_t)len);
    if (self.usedBytes >= len) self.usedBytes -= len;
    else self.usedBytes = 0;
    kmtl_shm_free(&self->_shmAlloc, off, len);
    [self signalBufferReleaseGroups:slot];
  }
}

- (void)failQueueWaiters:(KidoMetalQueueState *)qs status:(int32_t)status message:(NSString *)msg {
  if (!qs) return;
  if (qs.waiters.count > 0) {
    NSArray<KidoMetalQueueWaiter *> *list = [qs.waiters copy];
    [qs.waiters removeAllObjects];
    for (KidoMetalQueueWaiter *w in list) {
      [self failPendingElem:w.pending status:status message:msg];
    }
  }
  if (qs.commitWaiters.count > 0) {
    NSArray<KidoMetalPendingElem *> *clist = [qs.commitWaiters allObjects];
    [qs.commitWaiters removeAllObjects];
    for (KidoMetalPendingElem *pe in clist) {
      [self failPendingElem:pe status:status message:msg];
    }
  }
}

- (void)recordQueueError:(KidoMetalQueueState *)qs seq:(uint64_t)seq message:(NSString *)msg {
  if (qs.errors.count >= KMTL_MAX_QUEUE_ERRORS) {
    NSNumber *oldest = nil;
    for (NSNumber *k in qs.errors) {
      if (!oldest || k.unsignedLongLongValue < oldest.unsignedLongLongValue) oldest = k;
    }
    if (oldest) [qs.errors removeObjectForKey:oldest];
  }
  qs.errors[@(seq)] = msg;
}

- (void)replyQueueWaiterPending:(KidoMetalPendingElem *)pe
                          queue:(KidoMetalQueueState *)qs
                      targetSeq:(uint64_t)targetSeq {
  NSString *errMsg = qs.errors[@(targetSeq)];
  if (errMsg) {
    [self failPendingElem:pe status:KMTL_ERR_GPU message:errMsg];
    return;
  }
  struct kmtl_resp_hdr resp = {0};
  resp.status = KMTL_OK;
  uint64_t earliestErr = 0;
  for (NSNumber *k in qs.errors) {
    uint64_t s = k.unsignedLongLongValue;
    if (s > 0 && s <= targetSeq && (earliestErr == 0 || s < earliestErr)) {
      earliestErr = s;
    }
  }
  resp.handle = earliestErr;
  kmtl_seq_timing_t t = qs->timings[targetSeq & 63u];
  if (t.seq == targetSeq) {
    resp.arg0 = t.gpuStartNs;
    resp.arg1 = t.gpuEndNs;
  } else {
    resp.arg0 = qs.lastGpuStartNs;
    resp.arg1 = qs.lastGpuEndNs;
  }
  [self completePendingElem:pe resp:resp extra:NULL extraLen:0];
}

- (void)closeClient:(uint32_t)clientId group:(dispatch_group_t _Nullable)group {
  KidoMetalClientState *cs = self.clients[@(clientId)];
  [self.clients removeObjectForKey:@(clientId)];
  if (cs && group && cs.pendingMapsGroup &&
      dispatch_group_wait(cs.pendingMapsGroup, DISPATCH_TIME_NOW) != 0) {
    dispatch_group_enter(group);
    dispatch_group_notify(cs.pendingMapsGroup, self.deviceQueue, ^{
      dispatch_group_leave(group);
    });
  }

  NSMutableArray<NSNumber *> *dropQueues = [NSMutableArray array];
  for (NSNumber *k in self.queues) {
    KidoMetalObjEntry *e = self.queues[k];
    if (e.clientId == clientId) {
      [self failQueueWaiters:(KidoMetalQueueState *)e.obj status:KMTL_ERR_IO message:@"client closed"];
      [dropQueues addObject:k];
    }
  }
  [self.queues removeObjectsForKeys:dropQueues];

  NSArray<NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *> *tables =
      @[ self.pipelines, self.functions, self.libraries ];
  for (NSMutableDictionary<NSNumber *, KidoMetalObjEntry *> *t in tables) {
    NSMutableArray<NSNumber *> *drop = [NSMutableArray array];
    for (NSNumber *k in t) {
      if (t[k].clientId == clientId) [drop addObject:k];
    }
    [t removeObjectsForKeys:drop];
  }

  if (group) {
    for (KidoMetalBufferSlot *slot in [self.drainingBuffers allObjects]) {
      if (slot.clientId == clientId) {
        [self attachReleaseGroup:group toBufferSlot:slot];
      }
    }
  }

  NSMutableArray<NSNumber *> *dropBufs = [NSMutableArray array];
  for (NSNumber *k in self.buffers) {
    KidoMetalBufferSlot *slot = self.buffers[k];
    if (slot.clientId == clientId) {
      slot.destroyed = YES;
      [dropBufs addObject:k];
      [self attachReleaseGroup:group toBufferSlot:slot];
      [self maybeReleaseBufferSlot:slot];
    }
  }
  [self.buffers removeObjectsForKeys:dropBufs];
}

- (void)closeClient:(uint32_t)clientId {
  [self closeClient:clientId group:nil];
}

- (void)beginStopOnDeviceQueue {
  self.stopped = YES;
  self.vmStopped = YES;
  self.deviceGeneration++;
  [self failAllPendingElemsWithStatus:KMTL_ERR_IO message:@"device stopped" returnToQueue:NO];
  [self.clients removeAllObjects];
  for (NSNumber *k in self.queues) {
    KidoMetalQueueState *qs = (KidoMetalQueueState *)self.queues[k].obj;
    [qs.waiters removeAllObjects];
    [qs.commitWaiters removeAllObjects];
  }
  [self.queues removeAllObjects];
  [self.pipelines removeAllObjects];
  [self.functions removeAllObjects];
  [self.libraries removeAllObjects];
  for (NSNumber *k in self.buffers) {
    KidoMetalBufferSlot *slot = self.buffers[k];
    slot.destroyed = YES;
    if (slot.inFlightRefs == 0 && !slot.unmapping) {
      slot.unmapping = YES;
      slot.mtlBuffer = nil;
      if (slot.hostPtr) {
        munmap(slot.hostPtr, (size_t)slot.alignedLen);
        slot.hostPtr = NULL;
      }
      [self signalBufferReleaseGroups:slot];
    }
  }
  [self.buffers removeAllObjects];
  for (KidoMetalBufferSlot *slot in [self.drainingBuffers allObjects]) {
    if (slot.inFlightRefs == 0 && !slot.unmapping) {
      slot.unmapping = YES;
      slot.mtlBuffer = nil;
      if (slot.hostPtr) {
        munmap(slot.hostPtr, (size_t)slot.alignedLen);
        slot.hostPtr = NULL;
      }
      [self signalBufferReleaseGroups:slot];
      [self.drainingBuffers removeObject:slot];
    }
  }
}

- (void)shutdown {
  dispatch_sync(_deviceQueue, ^{
    [self beginStopOnDeviceQueue];
  });
  dispatch_group_wait(_inFlightGroup, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
  dispatch_sync(_deviceQueue, ^{
    for (NSNumber *k in self.buffers) {
      KidoMetalBufferSlot *slot = self.buffers[k];
      slot.destroyed = YES;
      if (slot.inFlightRefs == 0 && !slot.unmapping) {
        slot.unmapping = YES;
        slot.mtlBuffer = nil;
        if (slot.hostPtr) {
          munmap(slot.hostPtr, (size_t)slot.alignedLen);
          slot.hostPtr = NULL;
        }
        [self signalBufferReleaseGroups:slot];
      }
    }
    [self.buffers removeAllObjects];
    for (KidoMetalBufferSlot *slot in [self.drainingBuffers allObjects]) {
      if (slot.inFlightRefs == 0 && !slot.unmapping) {
        slot.unmapping = YES;
        slot.mtlBuffer = nil;
        if (slot.hostPtr) {
          munmap(slot.hostPtr, (size_t)slot.alignedLen);
          slot.hostPtr = NULL;
        }
      }
      [self signalBufferReleaseGroups:slot];
    }
    [self.drainingBuffers removeAllObjects];
  });
}

#pragma mark - VZCustomVirtioDeviceDelegate

- (void)customVirtioDeviceDidAcceptDriverOk:(VZCustomVirtioDevice *)device {
  if (!self.vmStopped) self.stopped = NO;
  if (!_loggedDriverReady) {
    _loggedDriverReady = YES;
    kido_vmm_logf("virtio-metal guest driver ready");
  }
}

- (void)customVirtioDeviceWillReset:(VZCustomVirtioDevice *)device {
  [self failAllPendingElemsWithStatus:KMTL_ERR_IO message:@"device reset" returnToQueue:YES];
  self.deviceGeneration++;

  NSArray<NSNumber *> *allClients = [self.clients allKeys];
  for (NSNumber *cid in allClients) {
    [self closeClient:cid.unsignedIntValue];
  }
  for (NSNumber *k in self.queues) {
    [self failQueueWaiters:(KidoMetalQueueState *)self.queues[k].obj status:KMTL_ERR_IO message:@"device reset"];
  }
  [self.queues removeAllObjects];
  [self.pipelines removeAllObjects];
  [self.functions removeAllObjects];
  [self.libraries removeAllObjects];
  NSArray<NSNumber *> *remBufs = [self.buffers allKeys];
  for (NSNumber *k in remBufs) {
    KidoMetalBufferSlot *slot = self.buffers[k];
    slot.destroyed = YES;
    [self maybeReleaseBufferSlot:slot];
  }
  [self.buffers removeAllObjects];
  if (!self.vmStopped) self.stopped = NO;
}

- (void)customVirtioDeviceWillStop:(VZCustomVirtioDevice *)device {
  [self beginStopOnDeviceQueue];
}

- (void)customVirtioDevice:(VZCustomVirtioDevice *)device didReceiveNotificationForQueue:(VZVirtioQueue *)queue {
  if (queue.queueIndex != 0) {
    VZVirtioQueueElement *e;
    while ((e = [queue nextElement]) != nil) [e returnToQueue];
    return;
  }
  VZVirtioQueueElement *elem;
  while ((elem = [queue nextElement]) != nil) {
    [self handleElement:elem];
  }
}

#pragma mark - RPC Dispatch

- (NSUInteger)countForClient:(uint32_t)clientId inTable:(NSDictionary<NSNumber *, KidoMetalObjEntry *> *)table {
  NSUInteger n = 0;
  for (NSNumber *k in table) {
    if (table[k].clientId == clientId) n++;
  }
  return n;
}

- (NSUInteger)bufferCountForClient:(uint32_t)clientId {
  NSUInteger n = 0;
  for (NSNumber *k in self.buffers) {
    if (self.buffers[k].clientId == clientId) n++;
  }
  return n;
}

- (void)handleElement:(VZVirtioQueueElement *)elem {
  if (self.stopped || self.vmStopped) {
    replyErrorNow(elem, KMTL_ERR_IO, @"device stopped");
    return;
  }
  struct kmtl_req_hdr req = {0};
  if (elem.readBuffersAvailableByteCount < sizeof(req)) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated request header");
    return;
  }
  NSError *err = nil;
  if (![elem readBytesIntoBuffer:&req exactLength:sizeof(req) error:&err]) {
    replyErrorNow(elem, KMTL_ERR_IO, err.localizedDescription);
    return;
  }
  if (req.magic != KMTL_MAGIC || req.version != KMTL_PROTO_VERSION) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"bad magic or protocol version");
    return;
  }

  switch (req.opcode) {
  case KMTL_OP_GET_DEVICE_INFO: {
    struct kmtl_device_info info = {0};
    strlcpy(info.name, _gpu.name.UTF8String ?: "Apple Silicon GPU", sizeof(info.name));
    info.registry_id = _gpu.registryID;
    info.max_buffer_length = MIN((uint64_t)_gpu.maxBufferLength, _memoryLimitBytes);
    info.recommended_max_working_set_size = _memoryLimitBytes;
    info.shm_aperture_bytes = _memoryLimitBytes;
    info.gpu_family_mask = probeGPUFamilyMask(_gpu);
    MTLSize mt = _gpu.maxThreadsPerThreadgroup;
    info.max_threads_per_threadgroup[0] = (uint32_t)mt.width;
    info.max_threads_per_threadgroup[1] = (uint32_t)mt.height;
    info.max_threads_per_threadgroup[2] = (uint32_t)mt.depth;
    info.max_threadgroup_memory_length = (uint32_t)_gpu.maxThreadgroupMemoryLength;
    info.host_page_size = _hostPageSize;
    info.flags = [self featureFlags];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, &info, (uint32_t)sizeof(info));
    return;
  }

  case KMTL_OP_CLIENT_OPEN: {
    if (req.client_id == 0) {
      replyErrorNow(elem, KMTL_ERR_INVAL, @"client_id must be non-zero");
      return;
    }
    if (!self.clients[@(req.client_id)] && self.clients.count >= KMTL_MAX_CLIENTS) {
      replyErrorNow(elem, KMTL_ERR_NOMEM, @"client limit reached");
      return;
    }
    if (self.clients[@(req.client_id)]) {
      [self closeClient:req.client_id];
    }
    KidoMetalClientState *cs = [KidoMetalClientState new];
    cs.clientId = req.client_id;
    cs.sessionToken = self.nextSessionToken++;
    self.clients[@(req.client_id)] = cs;
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_CLIENT_CLOSE: {
    dispatch_group_t closeGroup = dispatch_group_create();
    KidoMetalClientState *cs = self.clients[@(req.client_id)];
    uint64_t sessionToken = cs ? cs.sessionToken : 0;
    [self closeClient:req.client_id group:closeGroup];
    if (dispatch_group_wait(closeGroup, DISPATCH_TIME_NOW) == 0) {
      struct kmtl_resp_hdr resp = {0};
      resp.status = KMTL_OK;
      replyElemNow(elem, resp, NULL, 0);
    } else {
      KidoMetalPendingElem *pe =
          [self trackPendingElem:elem clientId:req.client_id sessionToken:sessionToken];
      dispatch_group_notify(closeGroup, self.deviceQueue, ^{
        struct kmtl_resp_hdr resp = {0};
        resp.status = KMTL_OK;
        [self completePendingElem:pe resp:resp extra:NULL extraLen:0];
      });
    }
    return;
  }

  case KMTL_OP_CREATE_BUFFER: {
    [self handleCreateBuffer:req element:elem];
    return;
  }

  case KMTL_OP_DESTROY_BUFFER: {
    KidoMetalBufferSlot *slot = self.buffers[@(req.handle)];
    if (!slot || slot.clientId != req.client_id) {
      replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown buffer handle");
      return;
    }
    [self.buffers removeObjectForKey:@(req.handle)];
    slot.destroyed = YES;
    [self maybeReleaseBufferSlot:slot];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_CREATE_LIBRARY_SOURCE: {
    [self handleCreateLibrarySource:req element:elem];
    return;
  }

  case KMTL_OP_CREATE_LIBRARY_DATA: {
    [self handleCreateLibraryData:req element:elem];
    return;
  }

  case KMTL_OP_DESTROY_LIBRARY: {
    KidoMetalObjEntry *e = self.libraries[@(req.handle)];
    if (!e || e.clientId != req.client_id) {
      replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown library handle");
      return;
    }
    [self.libraries removeObjectForKey:@(req.handle)];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_CREATE_FUNCTION: {
    [self handleCreateFunction:req element:elem];
    return;
  }

  case KMTL_OP_DESTROY_FUNCTION: {
    KidoMetalObjEntry *e = self.functions[@(req.handle)];
    if (!e || e.clientId != req.client_id) {
      replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown function handle");
      return;
    }
    [self.functions removeObjectForKey:@(req.handle)];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_CREATE_COMPUTE_PIPELINE: {
    [self handleCreateComputePipeline:req element:elem];
    return;
  }

  case KMTL_OP_DESTROY_PIPELINE: {
    KidoMetalObjEntry *e = self.pipelines[@(req.handle)];
    if (!e || e.clientId != req.client_id) {
      replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown pipeline handle");
      return;
    }
    [self.pipelines removeObjectForKey:@(req.handle)];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_CREATE_QUEUE: {
    if (!self.clients[@(req.client_id)]) {
      replyErrorNow(elem, KMTL_ERR_PERM, @"unknown client");
      return;
    }
    if ([self countForClient:req.client_id inTable:self.queues] >= KMTL_MAX_QUEUES_PER_CLIENT) {
      replyErrorNow(elem, KMTL_ERR_NOMEM, @"queue limit reached");
      return;
    }
    id<MTLCommandQueue> q = [_gpu newCommandQueue];
    if (!q) {
      replyErrorNow(elem, KMTL_ERR_NOMEM, @"newCommandQueue failed");
      return;
    }
    uint64_t h = self.nextHandle++;
    KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
    entry.handle = h;
    entry.clientId = req.client_id;
    entry.obj = [[KidoMetalQueueState alloc] initWithQueue:q];
    self.queues[@(h)] = entry;
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = h;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_DESTROY_QUEUE: {
    KidoMetalObjEntry *e = self.queues[@(req.handle)];
    if (!e || e.clientId != req.client_id) {
      replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown queue handle");
      return;
    }
    [self failQueueWaiters:(KidoMetalQueueState *)e.obj status:KMTL_ERR_IO message:@"queue destroyed"];
    [self.queues removeObjectForKey:@(req.handle)];
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  case KMTL_OP_COMMIT_COMMAND_BUFFER: {
    [self handleCommitCommandBuffer:req element:elem];
    return;
  }

  case KMTL_OP_WAIT_QUEUE: {
    [self handleWaitQueue:req element:elem];
    return;
  }

  default:
    replyErrorNow(elem, KMTL_ERR_UNSUPP, @"unsupported opcode");
    return;
  }
}

#pragma mark - Buffer Allocation (Zero-Copy Unified Memory)

- (void)handleCreateBuffer:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  if (!cs) {
    replyErrorNow(elem, KMTL_ERR_PERM, @"unknown client");
    return;
  }
  if (!self.shmRegion) {
    replyErrorNow(elem, KMTL_ERR_IO, @"shared memory region unavailable");
    return;
  }
  if ([self bufferCountForClient:req.client_id] + cs.pendingBuffers >= KMTL_MAX_BUFFERS_PER_CLIENT) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"buffer handle limit reached");
    return;
  }
  uint64_t reqLen = req.arg0;
  if (reqLen == 0) reqLen = 1;
  uint64_t alignedLen = alignUpU64(reqLen, _hostPageSize);
  if (alignedLen == 0 || alignedLen > _memoryLimitBytes || alignedLen > (uint64_t)_gpu.maxBufferLength) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"buffer exceeds GPU memory limit");
    return;
  }
  if (self.usedBytes > _memoryLimitBytes - alignedLen) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"GPU memory quota exhausted");
    return;
  }

  uint64_t shmOffset = 0;
  if (!kmtl_shm_alloc(&_shmAlloc, alignedLen, &shmOffset)) {
    replyErrorNow(elem, KMTL_ERR_NOSPC, @"GPU shared memory aperture fragmented or full");
    return;
  }

  void *ptr = mmap(NULL, (size_t)alignedLen, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
  if (ptr == MAP_FAILED) {
    kmtl_shm_free(&_shmAlloc, shmOffset, alignedLen);
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"host mmap failed");
    return;
  }

  id<MTLBuffer> mb = [_gpu newBufferWithBytesNoCopy:ptr
                                             length:(NSUInteger)alignedLen
                                            options:MTLResourceStorageModeShared
                                        deallocator:nil];
  if (!mb) {
    munmap(ptr, (size_t)alignedLen);
    kmtl_shm_free(&_shmAlloc, shmOffset, alignedLen);
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"newBufferWithBytesNoCopy failed");
    return;
  }

  uint64_t handle = self.nextHandle++;
  if (handle > 0xFFFFFFFFULL) {
    mb = nil;
    munmap(ptr, (size_t)alignedLen);
    kmtl_shm_free(&_shmAlloc, shmOffset, alignedLen);
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"buffer handle space exhausted");
    return;
  }

  self.usedBytes += alignedLen;
  cs.pendingBuffers++;
  dispatch_group_t mapsGroup = cs.pendingMapsGroup;
  dispatch_group_enter(mapsGroup);
  uint64_t sessionToken = cs.sessionToken;
  uint64_t gen = self.deviceGeneration;
  KidoMetalPendingElem *pe = [self trackPendingElem:elem clientId:req.client_id sessionToken:sessionToken];

  __block id<MTLBuffer> mbBlock = mb;
  mb = nil;
  [self.shmRegion mapMemory:ptr
                   atOffset:shmOffset
                       size:alignedLen
          completionHandler:^(NSError *_Nullable mapErr) {
            if (cs.pendingBuffers > 0) cs.pendingBuffers--;
            BOOL sessionOk = [self isSessionValid:req.client_id token:sessionToken generation:gen];
            if (mapErr || !sessionOk) {
              mbBlock = nil;
              if (!mapErr && self.shmRegion && !self.vmStopped) {
                [self.shmRegion unmapMemoryAtOffset:shmOffset
                                               size:alignedLen
                                  completionHandler:^(NSError *_Nullable uerr) {
                                    if (uerr) {
                                      kido_vmm_logf("virtio-metal: rollback unmap failed at 0x%llx: %s",
                                                    (unsigned long long)shmOffset,
                                                    uerr.localizedDescription.UTF8String);
                                      dispatch_group_leave(mapsGroup);
                                      return;
                                    }
                                    munmap(ptr, (size_t)alignedLen);
                                    if (self.usedBytes >= alignedLen) self.usedBytes -= alignedLen;
                                    else self.usedBytes = 0;
                                    kmtl_shm_free(&self->_shmAlloc, shmOffset, alignedLen);
                                    dispatch_group_leave(mapsGroup);
                                  }];
              } else {
                munmap(ptr, (size_t)alignedLen);
                if (self.usedBytes >= alignedLen) self.usedBytes -= alignedLen;
                else self.usedBytes = 0;
                kmtl_shm_free(&self->_shmAlloc, shmOffset, alignedLen);
                dispatch_group_leave(mapsGroup);
              }
              [self failPendingElem:pe
                             status:KMTL_ERR_IO
                            message:mapErr ? mapErr.localizedDescription : @"client closed during buffer map"];
              return;
            }
            KidoMetalBufferSlot *slot = [KidoMetalBufferSlot new];
            slot.handle = handle;
            slot.clientId = req.client_id;
            slot.shmOffset = shmOffset;
            slot.alignedLen = alignedLen;
            slot.requestedLen = reqLen;
            slot.hostPtr = ptr;
            slot.mtlBuffer = mbBlock;
            mbBlock = nil;
            self.buffers[@(handle)] = slot;
            dispatch_group_leave(mapsGroup);

            struct kmtl_resp_hdr resp = {0};
            resp.status = KMTL_OK;
            resp.handle = handle;
            resp.arg0 = shmOffset;
            resp.arg1 = alignedLen;
            [self completePendingElem:pe resp:resp extra:NULL extraLen:0];
          }];
}

#pragma mark - Shaders & Pipelines

- (void)handleCreateLibrarySource:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  if (!cs) {
    replyErrorNow(elem, KMTL_ERR_PERM, @"unknown client");
    return;
  }
  if ([self countForClient:req.client_id inTable:self.libraries] + cs.pendingLibraries >= KMTL_MAX_LIBS_PER_CLIENT) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"library limit reached");
    return;
  }
  if (self.activeCompiles >= KMTL_MAX_ACTIVE_COMPILES) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"too many concurrent shader compilations");
    return;
  }
  NSUInteger avail = elem.readBuffersAvailableByteCount;
  if (avail < sizeof(struct kmtl_compile_opts) || avail > KMTL_MAX_IN_PAYLOAD) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"invalid library source payload size");
    return;
  }
  NSError *err = nil;
  NSData *payload = [elem readBytesWithExactLength:avail error:&err];
  if (!payload) {
    replyErrorNow(elem, KMTL_ERR_IO, err.localizedDescription);
    return;
  }
  const uint8_t *bytes = payload.bytes;
  struct kmtl_compile_opts co;
  memcpy(&co, bytes, sizeof(co));
  size_t pos = sizeof(co);
  if (co.source_len > payload.length - pos || co.num_macros > 1024 ||
      !kmtl_valid_language_version(co.language_version)) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"malformed compile options");
    return;
  }
  NSString *src = [[NSString alloc] initWithBytes:bytes + pos length:co.source_len encoding:NSUTF8StringEncoding];
  if (!src) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"shader source is not valid UTF-8");
    return;
  }
  pos += co.source_len;

  MTLCompileOptions *opts = [MTLCompileOptions new];
  if (@available(macOS 15.0, *)) {
    opts.mathMode = co.fast_math ? MTLMathModeFast : MTLMathModeSafe;
  }
  if (co.language_version != 0) {
    opts.languageVersion = (MTLLanguageVersion)co.language_version;
  }
  if (co.num_macros > 0) {
    NSMutableDictionary<NSString *, NSObject *> *macros = [NSMutableDictionary dictionaryWithCapacity:co.num_macros];
    for (uint32_t i = 0; i < co.num_macros; i++) {
      if (pos + sizeof(struct kmtl_macro_hdr) > payload.length) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated macro header");
        return;
      }
      struct kmtl_macro_hdr mh;
      memcpy(&mh, bytes + pos, sizeof(mh));
      pos += sizeof(mh);
      if (mh.key_len == 0 || mh.key_len > payload.length - pos) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"invalid macro key");
        return;
      }
      if (memchr(bytes + pos, 0, mh.key_len) != NULL) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"NUL byte in macro key");
        return;
      }
      NSString *k = [[NSString alloc] initWithBytes:bytes + pos length:mh.key_len encoding:NSUTF8StringEncoding];
      pos += mh.key_len;
      if (mh.val_len > payload.length - pos) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated macro value");
        return;
      }
      if (mh.val_len > 0 && memchr(bytes + pos, 0, mh.val_len) != NULL) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"NUL byte in macro value");
        return;
      }
      NSString *v = [[NSString alloc] initWithBytes:bytes + pos length:mh.val_len encoding:NSUTF8StringEncoding];
      pos += mh.val_len;
      if (!k || !v) {
        replyErrorNow(elem, KMTL_ERR_INVAL, @"macro is not valid UTF-8");
        return;
      }
      macros[k] = v;
    }
    opts.preprocessorMacros = macros;
  }

  uint32_t cid = req.client_id;
  uint64_t sessionToken = cs.sessionToken;
  uint64_t gen = self.deviceGeneration;
  uint32_t maxExtra = maxExtraBytesForElem(elem);
  cs.pendingLibraries++;
  self.activeCompiles++;
  KidoMetalPendingElem *pe = [self trackPendingElem:elem clientId:cid sessionToken:sessionToken];

  @try {
    [_gpu newLibraryWithSource:src
                       options:opts
             completionHandler:^(id<MTLLibrary> _Nullable lib, NSError *_Nullable compErr) {
               dispatch_async(self.deviceQueue, ^{
                 if (cs.pendingLibraries > 0) cs.pendingLibraries--;
                 if (self.activeCompiles > 0) self.activeCompiles--;
                 if (![self isSessionValid:cid token:sessionToken generation:gen]) {
                   [self failPendingElem:pe status:KMTL_ERR_IO message:@"client closed"];
                   return;
                 }
                 if (!lib) {
                   [self failPendingElem:pe
                                  status:KMTL_ERR_COMPILE
                                 message:compErr.localizedDescription ?: @"shader compile failed"];
                   return;
                 }
                 uint64_t h = self.nextHandle++;
                 KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
                 entry.handle = h;
                 entry.clientId = cid;
                 entry.obj = lib;
                 self.libraries[@(h)] = entry;
                 struct kmtl_resp_hdr resp = {0};
                 resp.status = KMTL_OK;
                 resp.handle = h;
                 NSData *fnData = encodeFunctionNames(lib, maxExtra);
                 [self completePendingElem:pe resp:resp extra:fnData.bytes extraLen:(uint32_t)fnData.length];
               });
             }];
  } @catch (NSException *ex) {
    if (cs.pendingLibraries > 0) cs.pendingLibraries--;
    if (self.activeCompiles > 0) self.activeCompiles--;
    [self failPendingElem:pe status:KMTL_ERR_INVAL message:ex.reason ?: @"invalid compile options"];
  }
}

- (void)handleCreateLibraryData:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  if (!cs) {
    replyErrorNow(elem, KMTL_ERR_PERM, @"unknown client");
    return;
  }
  if ([self countForClient:req.client_id inTable:self.libraries] + cs.pendingLibraries >= KMTL_MAX_LIBS_PER_CLIENT) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"library limit reached");
    return;
  }
  NSUInteger avail = elem.readBuffersAvailableByteCount;
  if (avail == 0 || avail > KMTL_MAX_IN_PAYLOAD) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"invalid metallib size");
    return;
  }
  NSError *err = nil;
  NSData *payload = [elem readBytesWithExactLength:avail error:&err];
  if (!payload) {
    replyErrorNow(elem, KMTL_ERR_IO, err.localizedDescription);
    return;
  }
  id<MTLLibrary> lib = nil;
  @try {
    dispatch_data_t dd = dispatch_data_create(payload.bytes, payload.length, NULL, DISPATCH_DATA_DESTRUCTOR_DEFAULT);
    lib = [_gpu newLibraryWithData:dd error:&err];
  } @catch (NSException *ex) {
    replyErrorNow(elem, KMTL_ERR_COMPILE, ex.reason ?: @"newLibraryWithData threw exception");
    return;
  }
  if (!lib) {
    replyErrorNow(elem, KMTL_ERR_COMPILE, err.localizedDescription ?: @"newLibraryWithData failed");
    return;
  }
  uint64_t h = self.nextHandle++;
  KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
  entry.handle = h;
  entry.clientId = req.client_id;
  entry.obj = lib;
  self.libraries[@(h)] = entry;
  struct kmtl_resp_hdr resp = {0};
  resp.status = KMTL_OK;
  resp.handle = h;
  NSData *fnData = encodeFunctionNames(lib, maxExtraBytesForElem(elem));
  replyElemNow(elem, resp, fnData.bytes, (uint32_t)fnData.length);
}

- (void)handleCreateFunction:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  KidoMetalObjEntry *libEntry = self.libraries[@(req.handle)];
  if (!cs || !libEntry || libEntry.clientId != req.client_id) {
    replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown library handle");
    return;
  }
  if ([self countForClient:req.client_id inTable:self.functions] + cs.pendingFunctions >= KMTL_MAX_FNS_PER_CLIENT) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"function limit reached");
    return;
  }
  NSUInteger avail = elem.readBuffersAvailableByteCount;
  if (avail < sizeof(struct kmtl_function_req) || avail > (1ULL << 20)) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"invalid function request size");
    return;
  }
  NSError *err = nil;
  NSData *payload = [elem readBytesWithExactLength:avail error:&err];
  if (!payload) {
    replyErrorNow(elem, KMTL_ERR_IO, err.localizedDescription);
    return;
  }
  const uint8_t *bytes = payload.bytes;
  struct kmtl_function_req fr;
  memcpy(&fr, bytes, sizeof(fr));
  size_t pos = sizeof(fr);
  if (fr.name_len == 0 || fr.name_len > payload.length - pos || fr.num_constants > 256) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"malformed function request");
    return;
  }
  if (memchr(bytes + pos, 0, fr.name_len) != NULL) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"NUL byte in function name");
    return;
  }
  NSString *fnName = [[NSString alloc] initWithBytes:bytes + pos length:fr.name_len encoding:NSUTF8StringEncoding];
  if (!fnName) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"function name is not valid UTF-8");
    return;
  }
  pos += fr.name_len;
  uint64_t alignedPos = alignUpU64((uint64_t)pos, 8);
  if (alignedPos == 0 || alignedPos > payload.length) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated function request");
    return;
  }
  pos = (size_t)alignedPos;

  id<MTLLibrary> lib = (id<MTLLibrary>)libEntry.obj;
  if (fr.num_constants == 0) {
    id<MTLFunction> fn = nil;
    @try {
      fn = [lib newFunctionWithName:fnName];
    } @catch (NSException *ex) {
      replyErrorNow(elem, KMTL_ERR_INVAL, ex.reason ?: @"newFunctionWithName failed");
      return;
    }
    if (!fn) {
      replyErrorNow(elem, KMTL_ERR_NOENT, [NSString stringWithFormat:@"function '%@' not found", fnName]);
      return;
    }
    uint64_t h = self.nextHandle++;
    KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
    entry.handle = h;
    entry.clientId = req.client_id;
    entry.obj = fn;
    self.functions[@(h)] = entry;
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = h;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }

  if (self.activeCompiles >= KMTL_MAX_ACTIVE_COMPILES) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"too many concurrent compilations");
    return;
  }

  MTLFunctionConstantValues *vals = [MTLFunctionConstantValues new];
  for (uint32_t i = 0; i < fr.num_constants; i++) {
    if (pos + sizeof(struct kmtl_func_const) > payload.length) {
      replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated function constant");
      return;
    }
    struct kmtl_func_const fc;
    memcpy(&fc, bytes + pos, sizeof(fc));
    pos += sizeof(fc);
    uint32_t expectedLen = kmtl_func_const_size(fc.data_type);
    if (expectedLen == 0 || expectedLen > 64 || fc.index > KMTL_MAX_FUNC_CONST_INDEX ||
        fc.value_len < expectedLen || fc.value_len > 256 || fc.value_len > payload.length - pos) {
      replyErrorNow(elem, KMTL_ERR_INVAL, @"invalid function constant type or length");
      return;
    }
    uint64_t paddedVal = alignUpU64(fc.value_len, 8);
    if (paddedVal == 0 || paddedVal > payload.length - pos) {
      replyErrorNow(elem, KMTL_ERR_INVAL, @"truncated function constant padding");
      return;
    }
    _Alignas(16) uint8_t valBuf[64] = {0};
    memcpy(valBuf, bytes + pos, expectedLen);
    @try {
      [vals setConstantValue:valBuf type:(MTLDataType)fc.data_type atIndex:(NSUInteger)fc.index];
    } @catch (NSException *ex) {
      replyErrorNow(elem, KMTL_ERR_INVAL, ex.reason ?: @"invalid function constant");
      return;
    }
    pos += (size_t)paddedVal;
  }

  uint32_t cid = req.client_id;
  uint64_t sessionToken = cs.sessionToken;
  uint64_t gen = self.deviceGeneration;
  cs.pendingFunctions++;
  self.activeCompiles++;
  KidoMetalPendingElem *pe = [self trackPendingElem:elem clientId:cid sessionToken:sessionToken];

  @try {
    [lib newFunctionWithName:fnName
              constantValues:vals
           completionHandler:^(id<MTLFunction> _Nullable fn, NSError *_Nullable fnErr) {
             dispatch_async(self.deviceQueue, ^{
               if (cs.pendingFunctions > 0) cs.pendingFunctions--;
               if (self.activeCompiles > 0) self.activeCompiles--;
               if (![self isSessionValid:cid token:sessionToken generation:gen]) {
                 [self failPendingElem:pe status:KMTL_ERR_IO message:@"client closed"];
                 return;
               }
               if (!fn) {
                 [self failPendingElem:pe
                                status:KMTL_ERR_COMPILE
                               message:fnErr.localizedDescription ?: @"newFunctionWithName:constantValues failed"];
                 return;
               }
               uint64_t h = self.nextHandle++;
               KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
               entry.handle = h;
               entry.clientId = cid;
               entry.obj = fn;
               self.functions[@(h)] = entry;
               struct kmtl_resp_hdr resp = {0};
               resp.status = KMTL_OK;
               resp.handle = h;
               [self completePendingElem:pe resp:resp extra:NULL extraLen:0];
             });
           }];
  } @catch (NSException *ex) {
    if (cs.pendingFunctions > 0) cs.pendingFunctions--;
    if (self.activeCompiles > 0) self.activeCompiles--;
    [self failPendingElem:pe status:KMTL_ERR_INVAL message:ex.reason ?: @"function specialization threw exception"];
  }
}

- (void)handleCreateComputePipeline:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  KidoMetalObjEntry *fnEntry = self.functions[@(req.handle)];
  if (!cs || !fnEntry || fnEntry.clientId != req.client_id) {
    replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown function handle");
    return;
  }
  if ([self countForClient:req.client_id inTable:self.pipelines] + cs.pendingPipelines >= KMTL_MAX_PSOS_PER_CLIENT) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"pipeline limit reached");
    return;
  }
  if (self.activeCompiles >= KMTL_MAX_ACTIVE_COMPILES) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"too many concurrent compilations");
    return;
  }
  id<MTLFunction> fn = (id<MTLFunction>)fnEntry.obj;
  if (fn.functionType != MTLFunctionTypeKernel) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"function is not a compute kernel");
    return;
  }
  NSUInteger maxTG = _gpu.maxThreadsPerThreadgroup.width ?: 1024;
  if (req.arg0 > maxTG) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"maxTotalThreadsPerThreadgroup exceeds device limit");
    return;
  }

  MTLComputePipelineDescriptor *desc = [MTLComputePipelineDescriptor new];
  desc.computeFunction = fn;
  if (req.arg0 > 0) desc.maxTotalThreadsPerThreadgroup = (NSUInteger)req.arg0;
  if (req.arg1 & 1ULL) desc.threadGroupSizeIsMultipleOfThreadExecutionWidth = YES;

  uint32_t cid = req.client_id;
  uint64_t sessionToken = cs.sessionToken;
  uint64_t gen = self.deviceGeneration;
  cs.pendingPipelines++;
  self.activeCompiles++;
  KidoMetalPendingElem *pe = [self trackPendingElem:elem clientId:cid sessionToken:sessionToken];

  @try {
    [_gpu newComputePipelineStateWithDescriptor:desc
                                        options:MTLPipelineOptionNone
                              completionHandler:^(id<MTLComputePipelineState> _Nullable pso,
                                                  MTLComputePipelineReflection *_Nullable refl,
                                                  NSError *_Nullable psoErr) {
                                (void)refl;
                                dispatch_async(self.deviceQueue, ^{
                                  if (cs.pendingPipelines > 0) cs.pendingPipelines--;
                                  if (self.activeCompiles > 0) self.activeCompiles--;
                                  if (![self isSessionValid:cid token:sessionToken generation:gen]) {
                                    [self failPendingElem:pe status:KMTL_ERR_IO message:@"client closed"];
                                    return;
                                  }
                                  if (!pso) {
                                    [self failPendingElem:pe
                                                   status:KMTL_ERR_COMPILE
                                                  message:psoErr.localizedDescription ?: @"pipeline failed"];
                                    return;
                                  }
                                  uint64_t h = self.nextHandle++;
                                  KidoMetalObjEntry *entry = [KidoMetalObjEntry new];
                                  entry.handle = h;
                                  entry.clientId = cid;
                                  entry.obj = pso;
                                  self.pipelines[@(h)] = entry;

                                  struct kmtl_resp_hdr resp = {0};
                                  resp.status = KMTL_OK;
                                  resp.handle = h;
                                  resp.arg0 = ((uint64_t)pso.maxTotalThreadsPerThreadgroup & 0xFFFFFFFFULL) |
                                              (((uint64_t)pso.threadExecutionWidth & 0xFFFFFFFFULL) << 32);
                                  resp.arg1 = (uint64_t)pso.staticThreadgroupMemoryLength;
                                  [self completePendingElem:pe resp:resp extra:NULL extraLen:0];
                                });
                              }];
  } @catch (NSException *ex) {
    if (cs.pendingPipelines > 0) cs.pendingPipelines--;
    if (self.activeCompiles > 0) self.activeCompiles--;
    [self failPendingElem:pe status:KMTL_ERR_INVAL message:ex.reason ?: @"pipeline descriptor invalid"];
  }
}

#pragma mark - Batched Command Buffer Execution

static BOOL validateThreadgroupDims(id<MTLDevice> gpu,
                                    id<MTLComputePipelineState> pso,
                                    uint64_t tx,
                                    uint64_t ty,
                                    uint64_t tz) {
  MTLSize mt = gpu.maxThreadsPerThreadgroup;
  if (tx == 0 || ty == 0 || tz == 0) return NO;
  if (tx > mt.width || ty > mt.height || tz > mt.depth) return NO;
  uint64_t xy = tx * ty;
  uint64_t maxTotal = (uint64_t)pso.maxTotalThreadsPerThreadgroup;
  if (maxTotal == 0) maxTotal = 1024;
  if (xy > maxTotal || tz > maxTotal / xy) return NO;
  return YES;
}

- (void)handleCommitCommandBuffer:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  KidoMetalObjEntry *qEntry = self.queues[@(req.handle)];
  if (!cs || !qEntry || qEntry.clientId != req.client_id) {
    replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown queue handle");
    return;
  }
  if ((req.arg0 & ~(KMTL_COMMIT_WAIT_COMPLETED | KMTL_COMMIT_WAIT_SCHEDULED)) != 0) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"unsupported commit flags");
    return;
  }
  KidoMetalQueueState *qs = (KidoMetalQueueState *)qEntry.obj;
  if (qs.pendingSeqs.count >= KMTL_MAX_INFLIGHT_CMDS_PER_QUEUE ||
      self.globalInFlightCmds >= KMTL_MAX_GLOBAL_INFLIGHT_CMDS) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"too many in-flight command buffers");
    return;
  }
  NSUInteger avail = elem.readBuffersAvailableByteCount;
  if (avail > KMTL_MAX_CMD_PAYLOAD) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"command stream exceeds limit");
    return;
  }
  NSError *err = nil;
  NSData *stream = avail > 0 ? [elem readBytesWithExactLength:avail error:&err] : [NSData data];
  if (!stream) {
    replyErrorNow(elem, KMTL_ERR_IO, err.localizedDescription);
    return;
  }

  id<MTLCommandQueue> queue = qs.queue;
  id<MTLCommandBuffer> cb = [queue commandBuffer];
  if (!cb) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"commandBuffer allocation failed");
    return;
  }

  NSMutableSet<KidoMetalBufferSlot *> *pinnedBuffers = [NSMutableSet set];
  KidoMetalBufferSlot *boundBuffers[KMTL_MAX_BUFFER_SLOTS] = {nil};
  uint32_t tgMemLengths[KMTL_MAX_BUFFER_SLOTS] = {0};
  id<MTLComputeCommandEncoder> compEnc = nil;
  id<MTLBlitCommandEncoder> blitEnc = nil;
  id<MTLComputePipelineState> currentPSO = nil;

  const uint8_t *bytes = stream.bytes;
  size_t len = stream.length;
  size_t pos = 0;
  BOOL bad = NO;
  NSString *badReason = @"invalid command record or buffer bounds";

  @try {
    while (pos < len && !bad) {
      if (len - pos < sizeof(struct kmtl_cmd_hdr)) {
        bad = YES;
        break;
      }
      struct kmtl_cmd_hdr hdr;
      memcpy(&hdr, bytes + pos, sizeof(hdr));
      if (hdr.size < sizeof(hdr) || (hdr.size & 7u) != 0 || hdr.size > len - pos) {
        bad = YES;
        break;
      }

      const uint8_t *rec = bytes + pos;
      pos += hdr.size;

      switch (hdr.op) {
      case KMTL_CMD_BEGIN_COMPUTE: {
        if (compEnc || blitEnc) {
          bad = YES;
          break;
        }
        MTLDispatchType dtype = (hdr.flags == (uint16_t)MTLDispatchTypeConcurrent)
                                    ? MTLDispatchTypeConcurrent
                                    : MTLDispatchTypeSerial;
        compEnc = [cb computeCommandEncoderWithDispatchType:dtype];
        currentPSO = nil;
        for (uint32_t i = 0; i < KMTL_MAX_BUFFER_SLOTS; i++) {
          boundBuffers[i] = nil;
          tgMemLengths[i] = 0;
        }
        if (!compEnc) {
          bad = YES;
          badReason = @"computeCommandEncoder failed";
          break;
        }
        break;
      }

      case KMTL_CMD_BEGIN_BLIT: {
        if (compEnc || blitEnc) {
          bad = YES;
          break;
        }
        blitEnc = [cb blitCommandEncoder];
        if (!blitEnc) {
          bad = YES;
          badReason = @"blitCommandEncoder failed";
          break;
        }
        break;
      }

      case KMTL_CMD_END_ENCODER: {
        if (compEnc) {
          [compEnc endEncoding];
          compEnc = nil;
          currentPSO = nil;
          for (uint32_t i = 0; i < KMTL_MAX_BUFFER_SLOTS; i++) {
            boundBuffers[i] = nil;
            tgMemLengths[i] = 0;
          }
        } else if (blitEnc) {
          [blitEnc endEncoding];
          blitEnc = nil;
        }
        break;
      }

      case KMTL_CMD_SET_PIPELINE: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_set_pipeline)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_set_pipeline c;
        memcpy(&c, rec, sizeof(c));
        KidoMetalObjEntry *pe = self.pipelines[@(c.pso_handle)];
        if (!pe || pe.clientId != req.client_id) {
          bad = YES;
          break;
        }
        currentPSO = (id<MTLComputePipelineState>)pe.obj;
        [compEnc setComputePipelineState:currentPSO];
        break;
      }

      case KMTL_CMD_SET_BUFFER: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_set_buffer)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_set_buffer c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS) {
          bad = YES;
          break;
        }
        if (c.buf_handle == 0) {
          boundBuffers[c.index] = nil;
          [compEnc setBuffer:nil offset:0 atIndex:c.index];
        } else {
          KidoMetalBufferSlot *slot = self.buffers[@(c.buf_handle)];
          if (!slot || slot.clientId != req.client_id || slot.destroyed || c.offset > slot.alignedLen) {
            bad = YES;
            break;
          }
          boundBuffers[c.index] = slot;
          [compEnc setBuffer:slot.mtlBuffer offset:(NSUInteger)c.offset atIndex:c.index];
          [pinnedBuffers addObject:slot];
        }
        break;
      }

      case KMTL_CMD_SET_BUFFER_OFFSET: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_set_buffer_offset)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_set_buffer_offset c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS) {
          bad = YES;
          break;
        }
        KidoMetalBufferSlot *bound = boundBuffers[c.index];
        if (!bound || bound.destroyed || c.offset > bound.alignedLen) {
          bad = YES;
          break;
        }
        [compEnc setBufferOffset:(NSUInteger)c.offset atIndex:c.index];
        break;
      }

      case KMTL_CMD_SET_BYTES: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_set_bytes)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_set_bytes c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS || c.length == 0 || c.length > KMTL_MAX_SET_BYTES ||
            sizeof(c) + c.length > hdr.size) {
          bad = YES;
          break;
        }
        boundBuffers[c.index] = nil;
        [compEnc setBytes:rec + sizeof(c) length:c.length atIndex:c.index];
        break;
      }

      case KMTL_CMD_SET_TG_MEM: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_set_tg_mem)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_set_tg_mem c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS || (c.length & 15u) != 0 ||
            c.length > _gpu.maxThreadgroupMemoryLength) {
          bad = YES;
          break;
        }
        uint64_t totalTg = 0;
        for (uint32_t i = 0; i < KMTL_MAX_BUFFER_SLOTS; i++) {
          totalTg += (i == c.index) ? c.length : tgMemLengths[i];
        }
        if (totalTg > _gpu.maxThreadgroupMemoryLength) {
          bad = YES;
          break;
        }
        tgMemLengths[c.index] = c.length;
        [compEnc setThreadgroupMemoryLength:c.length atIndex:c.index];
        break;
      }

      case KMTL_CMD_DISPATCH_THREADGROUPS: {
        if (!compEnc || !currentPSO || hdr.size < sizeof(struct kmtl_cmd_dispatch)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_dispatch c;
        memcpy(&c, rec, sizeof(c));
        if (c.gx == 0 || c.gy == 0 || c.gz == 0 ||
            c.gx > UINT32_MAX || c.gy > UINT32_MAX || c.gz > UINT32_MAX ||
            !validateThreadgroupDims(_gpu, currentPSO, c.tx, c.ty, c.tz)) {
          bad = YES;
          break;
        }
        [compEnc dispatchThreadgroups:MTLSizeMake((NSUInteger)c.gx, (NSUInteger)c.gy, (NSUInteger)c.gz)
                threadsPerThreadgroup:MTLSizeMake((NSUInteger)c.tx, (NSUInteger)c.ty, (NSUInteger)c.tz)];
        break;
      }

      case KMTL_CMD_DISPATCH_THREADS: {
        if (!compEnc || !currentPSO || hdr.size < sizeof(struct kmtl_cmd_dispatch)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_dispatch c;
        memcpy(&c, rec, sizeof(c));
        if (c.gx == 0 || c.gy == 0 || c.gz == 0 ||
            c.gx > UINT32_MAX || c.gy > UINT32_MAX || c.gz > UINT32_MAX ||
            !validateThreadgroupDims(_gpu, currentPSO, c.tx, c.ty, c.tz)) {
          bad = YES;
          break;
        }
        [compEnc dispatchThreads:MTLSizeMake((NSUInteger)c.gx, (NSUInteger)c.gy, (NSUInteger)c.gz)
           threadsPerThreadgroup:MTLSizeMake((NSUInteger)c.tx, (NSUInteger)c.ty, (NSUInteger)c.tz)];
        break;
      }

      case KMTL_CMD_DISPATCH_INDIRECT: {
        if (!compEnc || !currentPSO || hdr.size < sizeof(struct kmtl_cmd_dispatch_indirect)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_dispatch_indirect c;
        memcpy(&c, rec, sizeof(c));
        KidoMetalBufferSlot *slot = self.buffers[@(c.buf_handle)];
        if (!slot || slot.clientId != req.client_id || slot.destroyed || (c.offset & 3ULL) != 0 ||
            c.offset > slot.alignedLen || slot.alignedLen - c.offset < 12 ||
            !validateThreadgroupDims(_gpu, currentPSO, c.tx, c.ty, c.tz)) {
          bad = YES;
          break;
        }
        [compEnc dispatchThreadgroupsWithIndirectBuffer:slot.mtlBuffer
                                   indirectBufferOffset:(NSUInteger)c.offset
                                  threadsPerThreadgroup:MTLSizeMake((NSUInteger)c.tx, (NSUInteger)c.ty, (NSUInteger)c.tz)];
        [pinnedBuffers addObject:slot];
        break;
      }

      case KMTL_CMD_MEMORY_BARRIER: {
        if (!compEnc || hdr.size < sizeof(struct kmtl_cmd_barrier)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_barrier c;
        memcpy(&c, rec, sizeof(c));
        uint32_t allowed = (uint32_t)(MTLBarrierScopeBuffers | MTLBarrierScopeTextures);
        if (c.scope == 0 || (c.scope & ~allowed) != 0) {
          bad = YES;
          break;
        }
        [compEnc memoryBarrierWithScope:(MTLBarrierScope)c.scope];
        break;
      }

      case KMTL_CMD_BLIT_COPY_BUFFER: {
        if (!blitEnc || hdr.size < sizeof(struct kmtl_cmd_blit_copy)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_blit_copy c;
        memcpy(&c, rec, sizeof(c));
        KidoMetalBufferSlot *src = self.buffers[@(c.src_handle)];
        KidoMetalBufferSlot *dst = self.buffers[@(c.dst_handle)];
        if (!src || !dst || src.clientId != req.client_id || dst.clientId != req.client_id ||
            src.destroyed || dst.destroyed || c.src_offset > src.alignedLen || c.dst_offset > dst.alignedLen ||
            c.size > src.alignedLen - c.src_offset || c.size > dst.alignedLen - c.dst_offset) {
          bad = YES;
          break;
        }
        if (c.size > 0) {
          [blitEnc copyFromBuffer:src.mtlBuffer
                     sourceOffset:(NSUInteger)c.src_offset
                         toBuffer:dst.mtlBuffer
                destinationOffset:(NSUInteger)c.dst_offset
                             size:(NSUInteger)c.size];
          [pinnedBuffers addObject:src];
          [pinnedBuffers addObject:dst];
        }
        break;
      }

      case KMTL_CMD_BLIT_FILL_BUFFER: {
        if (!blitEnc || hdr.size < sizeof(struct kmtl_cmd_blit_fill)) {
          bad = YES;
          break;
        }
        struct kmtl_cmd_blit_fill c;
        memcpy(&c, rec, sizeof(c));
        KidoMetalBufferSlot *dst = self.buffers[@(c.dst_handle)];
        if (!dst || dst.clientId != req.client_id || dst.destroyed || c.offset > dst.alignedLen ||
            c.size > dst.alignedLen - c.offset) {
          bad = YES;
          break;
        }
        if (c.size > 0) {
          [blitEnc fillBuffer:dst.mtlBuffer range:NSMakeRange((NSUInteger)c.offset, (NSUInteger)c.size) value:c.value];
          [pinnedBuffers addObject:dst];
        }
        break;
      }

      default:
        bad = YES;
        break;
      }
    }

    if (compEnc) [compEnc endEncoding];
    if (blitEnc) [blitEnc endEncoding];
  } @catch (NSException *ex) {
    if (compEnc) @try { [compEnc endEncoding]; } @catch (NSException *_) {}
    if (blitEnc) @try { [blitEnc endEncoding]; } @catch (NSException *_) {}
    bad = YES;
    badReason = ex.reason ?: @"Metal encoder rejected command stream";
  }

  if (bad) {
    replyErrorNow(elem, KMTL_ERR_INVAL, badReason);
    return;
  }

  // Pin every unique referenced buffer across GPU execution to prevent use-after-unmap.
  for (KidoMetalBufferSlot *slot in pinnedBuffers) slot.inFlightRefs++;
  self.globalInFlightCmds++;
  dispatch_group_enter(_inFlightGroup);

  uint64_t seq = ++qs.submittedSeq;
  [qs.pendingSeqs addObject:@(seq)];
  BOOL syncCompleted = (req.arg0 & KMTL_COMMIT_WAIT_COMPLETED) != 0;
  BOOL syncScheduled = !syncCompleted && ((req.arg0 & KMTL_COMMIT_WAIT_SCHEDULED) != 0);
  KidoMetalPendingElem *syncPE = nil;
  if (syncCompleted || syncScheduled) {
    syncPE = [self trackPendingElem:elem clientId:req.client_id sessionToken:cs.sessionToken];
    [qs.commitWaiters addObject:syncPE];
  }

  if (syncScheduled && syncPE) {
    [cb addScheduledHandler:^(id<MTLCommandBuffer> sched) {
      (void)sched;
      dispatch_async(self.deviceQueue, ^{
        [qs.commitWaiters removeObject:syncPE];
        struct kmtl_resp_hdr resp = {0};
        resp.status = KMTL_OK;
        resp.arg0 = seq;
        [self completePendingElem:syncPE resp:resp extra:NULL extraLen:0];
      });
    }];
  }

  [cb addCompletedHandler:^(id<MTLCommandBuffer> done) {
    dispatch_async(self.deviceQueue, ^{
      for (KidoMetalBufferSlot *slot in pinnedBuffers) {
        if (slot.inFlightRefs > 0) slot.inFlightRefs--;
        [self maybeReleaseBufferSlot:slot];
      }
      if (self.globalInFlightCmds > 0) self.globalInFlightCmds--;

      uint64_t startNs = secondsToNsSafe(done.GPUStartTime);
      uint64_t endNs = secondsToNsSafe(done.GPUEndTime);
      qs->timings[seq & 63u].seq = seq;
      qs->timings[seq & 63u].gpuStartNs = startNs;
      qs->timings[seq & 63u].gpuEndNs = endNs;
      qs.lastGpuStartNs = startNs;
      qs.lastGpuEndNs = endNs;

      BOOL ok = (done.status == MTLCommandBufferStatusCompleted);
      NSString *errMsg = ok ? nil : (done.error.localizedDescription ?: @"GPU command buffer failed");
      if (!ok && !syncCompleted) {
        [self recordQueueError:qs seq:seq message:errMsg];
      }

      [qs.pendingSeqs removeObject:@(seq)];
      while (qs.completedSeq < qs.submittedSeq && ![qs.pendingSeqs containsObject:@(qs.completedSeq + 1)]) {
        qs.completedSeq++;
      }

      if (syncCompleted && syncPE) {
        [qs.commitWaiters removeObject:syncPE];
        if (ok) {
          struct kmtl_resp_hdr resp = {0};
          resp.status = KMTL_OK;
          resp.arg0 = startNs;
          resp.arg1 = endNs;
          [self completePendingElem:syncPE resp:resp extra:NULL extraLen:0];
        } else {
          [self failPendingElem:syncPE status:KMTL_ERR_GPU message:errMsg];
        }
      }

      if (qs.waiters.count > 0) {
        NSMutableArray<KidoMetalQueueWaiter *> *stillWaiting = nil;
        for (KidoMetalQueueWaiter *w in qs.waiters) {
          if (w.targetSeq <= qs.completedSeq) {
            [self replyQueueWaiterPending:w.pending queue:qs targetSeq:w.targetSeq];
          } else {
            if (!stillWaiting) stillWaiting = [NSMutableArray array];
            [stillWaiting addObject:w];
          }
        }
        if (stillWaiting) {
          [qs.waiters setArray:stillWaiting];
        } else {
          [qs.waiters removeAllObjects];
        }
      }

      dispatch_group_leave(self.inFlightGroup);
    });
  }];
  [cb commit];

  if (!syncCompleted && !syncScheduled) {
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.arg0 = seq;
    replyElemNow(elem, resp, NULL, 0);
  }
}

- (void)handleWaitQueue:(struct kmtl_req_hdr)req element:(VZVirtioQueueElement *)elem {
  KidoMetalClientState *cs = self.clients[@(req.client_id)];
  KidoMetalObjEntry *qEntry = self.queues[@(req.handle)];
  if (!cs || !qEntry || qEntry.clientId != req.client_id) {
    replyErrorNow(elem, KMTL_ERR_NOENT, @"unknown queue handle");
    return;
  }
  KidoMetalQueueState *qs = (KidoMetalQueueState *)qEntry.obj;
  uint64_t targetSeq = req.arg0 == 0 ? qs.submittedSeq : req.arg0;
  if (targetSeq == 0) {
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    replyElemNow(elem, resp, NULL, 0);
    return;
  }
  if (targetSeq > qs.submittedSeq) {
    replyErrorNow(elem, KMTL_ERR_INVAL, @"wait sequence exceeds submitted command buffers");
    return;
  }
  if (targetSeq <= qs.completedSeq) {
    KidoMetalPendingElem *pe = [self trackPendingElem:elem clientId:req.client_id sessionToken:cs.sessionToken];
    [self replyQueueWaiterPending:pe queue:qs targetSeq:targetSeq];
    return;
  }
  if (qs.waiters.count >= KMTL_MAX_QUEUE_WAITERS) {
    replyErrorNow(elem, KMTL_ERR_NOMEM, @"too many concurrent queue waiters");
    return;
  }
  KidoMetalQueueWaiter *w = [KidoMetalQueueWaiter new];
  w.targetSeq = targetSeq;
  w.pending = [self trackPendingElem:elem clientId:req.client_id sessionToken:cs.sessionToken];
  [qs.waiters addObject:w];
}

@end

#endif
