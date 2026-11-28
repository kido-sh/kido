/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#define _GNU_SOURCE

#include "../../vmm/kmtl_proto.h"
#include "../../vmm/kmtl_shm.h"
#include "msl2glsl.h"
#ifdef KIDO_HAVE_AIR2SPV
#include "air2spv.h"
#include "metallib.h"
#endif

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <linux/dma-buf.h>
#include <linux/udmabuf.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <vulkan/vulkan.h>

extern char **environ;

#define KMTL_FRAME_GUEST_RPC 0u
#define KMTL_FRAME_VMM_RESET 1u
#define KMTL_FRAME_GUEST_RPC_PREFLUSHED 2u
#define KMTL_FLUSH_HELPERS 3u
#define KMTL_FLUSH_PAR_MIN_BYTES (1u << 20)
#define KMTL_MAX_CLIENTS 256u
#define KMTL_MAX_PSO_SPECS 16u
#define KMTL_MAX_DEFERRED_PIPES 256u
#define KMTL_MAX_INFLIGHT_CMDS 4u
#define KMTL_MAX_TOUCHED_BUFS 256u
#define KMTL_MAX_USED_PSOS 256u
#define KMTL_CHUNK_MAX_DISPATCHES 256u
#define KMTL_CHUNK_MAX_DISPATCHES_SMALL 64u
#define KMTL_CHUNK_MAX_WORKGROUPS 262144ULL
#define KMTL_CHUNK_MAX_WORKGROUPS_SMALL 32768ULL
#define KMTL_CHUNK_MAX_DESC_SETS 512u
#define KMTL_INDIRECT_WG_ESTIMATE 1024ULL
#define KMTL_STAGING_BYTES (4u * 1024u * 1024u)
#define KMTL_STAGING_FALLBACK_BYTES (1u * 1024u * 1024u)
#define KMTL_DARWIN_PAGE_SIZE 16384u
#define KMTL_MAX_LIB_SOURCE_BYTES (4u * 1024u * 1024u)
#define KMTL_MAX_TOTAL_LIB_SOURCE_BYTES (32u * 1024u * 1024u)
#define KMTL_MAX_TOTAL_LIB_BINARY_BYTES (64u * 1024u * 1024u)
#define KMTL_MAX_TOTAL_FN_SPIRV_BYTES (64u * 1024u * 1024u)
#define KMTL_MAX_SHM_BYTES (128ULL << 30)
#define KMTL_COMPILE_TIMEOUT_MS 15000

typedef enum {
  KMTL_BUF_TIER_STAGING = 0,
  KMTL_BUF_TIER_DMABUF = 1,
  KMTL_BUF_TIER_HOST_PTR = 2,
} kmtl_buf_tier_t;

typedef struct {
  bool active;
  uint32_t client_id;
} vk_client_t;

typedef struct {
  bool active;
  bool pending_destroy;
  uint64_t handle;
  uint32_t client_id;
  uint64_t shm_offset;
  uint64_t shm_len;
  uint64_t aligned_len;
  uint64_t requested_len;
  bool is_dmabuf;
  bool is_host_ptr;
  bool flush_deferred;
  bool gpu_dirty;
  int dmabuf_fd;
  uint32_t inflight_refs;
  uint32_t inflight_write_refs;
  VkBuffer buffer;
  VkDeviceMemory memory;
  void *mapped;
} vk_buffer_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  bool is_binary;
  char *source;
  size_t source_len;
  uint8_t *binary;
  size_t binary_len;
  msl_macro_t *macros;
  char **macro_keys;
  char **macro_vals;
  size_t num_macros;
} vk_library_t;

typedef struct {
  uint32_t index;
  uint32_t data_type;
  uint32_t value_len;
  uint8_t value[64];
} vk_func_const_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  char name[128];
  uint32_t *spirv;
  size_t spirv_bytes;
  uint32_t binding_mask;
  uint32_t readonly_mask;
  vk_func_const_t *consts;
  uint32_t num_consts;
  bool is_binary;
} vk_function_t;

typedef struct {
  uint32_t tx, ty, tz;
  VkPipeline pipeline;
} vk_pso_spec_t;

typedef struct {
  bool active;
  bool pending_destroy;
  uint64_t handle;
  uint32_t client_id;
  uint32_t inflight_refs;
  VkShaderModule shader;
  VkDescriptorSetLayout ds_layout;
  VkPipelineLayout pipe_layout;
  uint32_t binding_mask;
  uint32_t readonly_mask;
  uint32_t max_tg;
  bool is_binary;
  vk_func_const_t *consts;
  uint32_t num_consts;
  vk_pso_spec_t specs[KMTL_MAX_PSO_SPECS];
  uint32_t num_specs;
} vk_pipeline_t;

typedef struct {
  VkCommandBuffer cmd_buf;
  VkFence fence;
  VkDescriptorPool desc_pool;
  VkQueryPool query_pool;
  uint64_t staging_base;

  bool in_flight;
  bool has_gpu_cmd;
  bool had_timestamps;
  uint64_t queue_handle;
  uint32_t client_id;
  uint64_t queue_seq;
  uint64_t submit_ns;

  size_t touched_idx[KMTL_MAX_TOUCHED_BUFS];
  bool touched_written[KMTL_MAX_TOUCHED_BUFS];
  size_t n_touched;

  size_t used_pso_idx[KMTL_MAX_USED_PSOS];
  size_t n_used_psos;

  VkPipeline deferred_pipes[KMTL_MAX_DEFERRED_PIPES];
  size_t n_deferred_pipes;
} vk_cmd_slot_t;

typedef struct {
  uint64_t seq;
  uint64_t gpu_start_ns;
  uint64_t gpu_end_ns;
} vk_seq_timing_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  uint64_t submitted_seq;
  uint64_t completed_seq;
  uint64_t last_start_ns;
  uint64_t last_end_ns;
  vk_seq_timing_t timings[64];
} vk_queue_t;

#if defined(__aarch64__)
typedef struct {
  pthread_mutex_t mu;
  pthread_cond_t cv_start;
  pthread_cond_t cv_done;
  pthread_t threads[KMTL_FLUSH_HELPERS];
  uint32_t n_threads;
  bool initialized;
  bool init_failed;
  bool stop;
  uint64_t gen;
  uint32_t done;
  uintptr_t start_addr;
  size_t total_lines;
  uint64_t line_size;
  bool invalidate;
} flush_pool_t;
#endif

typedef struct {
  int sock_fd;
  int shm_fd;
  int udmabuf_fd;
  uint8_t *shm_base;
  uint64_t shm_size;
  uint32_t host_page_size;
  kmtl_shm_alloc_t shm_alloc;
  uint64_t used_bytes;
  size_t total_lib_source_bytes;
  size_t total_lib_binary_bytes;
  size_t total_fn_spirv_bytes;
  uint64_t next_handle;
  char self_exe[4096];
  char glslang_path[4096];
#ifdef KIDO_HAVE_AIR2SPV
  int air_sb_fd;
  pid_t air_sb_pid;
#endif
#if defined(__aarch64__)
  flush_pool_t flush_pool;
#endif

  VkInstance instance;
  VkPhysicalDevice phys_dev;
  VkDevice device;
  uint32_t queue_family;
  VkQueue queue;
  VkCommandPool cmd_pool;
  vk_cmd_slot_t slots[KMTL_MAX_INFLIGHT_CMDS];
  uint32_t slot_head;
  uint32_t slot_tail;
  uint32_t slots_in_flight;
  uint32_t deferred_flush_count;
  uint32_t gpu_dirty_count;

  VkBuffer staging_buf;
  VkDeviceMemory staging_mem;
  uint8_t *staging_ptr;
  uint64_t slot_staging_bytes;
  uint64_t staging_align;

  VkPhysicalDeviceProperties props;
  VkPhysicalDeviceMemoryProperties mem_props;
  uint32_t subgroup_size;
  uint32_t subgroup_ops;
  uint32_t max_chunk_dispatches;
  uint64_t max_chunk_workgroups;
  uint64_t buf_extra_pad;
  uint64_t host_ptr_align;
  bool has_dmabuf_ext;
  bool has_host_ptr_ext;
  bool has_pci_bus_info;
  uint32_t pci_domain;
  uint32_t pci_bus;
  uint32_t pci_device;
  uint32_t pci_function;
  bool has_drm_props;
  int64_t drm_primary_major;
  int64_t drm_primary_minor;
  int64_t drm_render_major;
  int64_t drm_render_minor;
  bool has_timestamps;
  double timestamp_period_ns;
  uint64_t timestamp_mask;
  kmtl_buf_tier_t buf_tier;
  bool dma_coherent;
  bool vmm_dma_coherent;
  bool has_fp16;
  bool has_bf16;
  bool has_int16;
  bool has_int8;
  bool has_storage_16bit;
  bool has_storage_8bit;
  bool has_int64;
  bool logged_buf_mode;
  bool logged_dmabuf_fallback;
  PFN_vkGetMemoryFdPropertiesKHR pfn_get_mem_fd_props;
  PFN_vkGetMemoryHostPointerPropertiesEXT pfn_get_mem_host_ptr_props;

  vk_client_t clients[KMTL_MAX_CLIENTS];
  vk_buffer_t *buffers;
  size_t buffers_cap;
  vk_library_t *libraries;
  size_t libraries_cap;
  vk_function_t *functions;
  size_t functions_cap;
  vk_pipeline_t *pipelines;
  size_t pipelines_cap;
  vk_queue_t *queues;
  size_t queues_cap;
} worker_t;

static uint64_t monotonic_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

static uint64_t align_up(uint64_t v, uint64_t align) {
  if (align <= 1) return v;
  uint64_t r = v % align;
  if (r == 0) return v;
  if (v > UINT64_MAX - (align - r)) return 0;
  return v + (align - r);
}

static bool read_full(int fd, void *buf, size_t n) {
  uint8_t *p = (uint8_t *)buf;
  size_t done = 0;
  while (done < n) {
    ssize_t r = read(fd, p + done, n - done);
    if (r < 0) {
      if (errno == EINTR) continue;
      return false;
    }
    if (r == 0) return false;
    done += (size_t)r;
  }
  return true;
}

static bool write_full(int fd, const void *buf, size_t n) {
  const uint8_t *p = (const uint8_t *)buf;
  size_t done = 0;
  while (done < n) {
    ssize_t w = write(fd, p + done, n - done);
    if (w < 0) {
      if (errno == EINTR) continue;
      return false;
    }
    done += (size_t)w;
  }
  return true;
}

static bool send_resp(int fd, struct kmtl_resp_hdr resp, const void *extra, uint32_t extra_len, uint32_t out_cap) {
  if (extra_len > out_cap) extra_len = out_cap;
  if (extra_len > KMTL_MAX_OUT_PAYLOAD) extra_len = KMTL_MAX_OUT_PAYLOAD;
  resp.extra_len = extra ? extra_len : 0;
  if (!write_full(fd, &resp, sizeof(resp))) return false;
  if (resp.extra_len > 0 && !write_full(fd, extra, resp.extra_len)) return false;
  return true;
}

static bool send_err(int fd, int32_t status, const char *msg, uint32_t out_cap) {
  struct kmtl_resp_hdr resp;
  memset(&resp, 0, sizeof(resp));
  resp.status = status;
  uint32_t len = msg ? (uint32_t)strlen(msg) : 0;
  return send_resp(fd, resp, msg, len, out_cap);
}

static int find_memory_type(const VkPhysicalDeviceMemoryProperties *mp, uint32_t type_bits, VkMemoryPropertyFlags req) {
  for (uint32_t i = 0; i < mp->memoryTypeCount; i++) {
    if ((type_bits & (1u << i)) && (mp->memoryTypes[i].propertyFlags & req) == req) {
      return (int)i;
    }
  }
  return -1;
}

static bool try_alloc_dmabuf_vk_buffer(worker_t *w,
                                       uint64_t shm_offset,
                                       uint64_t buf_len,
                                       uint64_t shm_len,
                                       VkBuffer *out_buf,
                                       VkDeviceMemory *out_mem,
                                       int *out_dmabuf_fd);
static bool try_alloc_host_ptr_vk_buffer(worker_t *w,
                                         uint64_t shm_offset,
                                         uint64_t buf_len,
                                         uint64_t shm_len,
                                         VkBuffer *out_buf,
                                         VkDeviceMemory *out_mem);
static bool drain_all_inflight(worker_t *w);
static bool drain_client_inflight(worker_t *w, uint32_t cid);
static bool poll_retire_completed(worker_t *w);
static bool retire_up_to_queue_seq(worker_t *w, vk_queue_t *q, uint64_t target_seq);
static bool probe_gpu_dma_coherency(worker_t *w);
#if defined(__aarch64__)
static void arm64_clean_inv_dcache_poc(const void *ptr, size_t len);
#endif

static bool vk_init(worker_t *w) {
  VkApplicationInfo app = {
      .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
      .pApplicationName = "kido-vk-worker",
      .applicationVersion = VK_MAKE_VERSION(0, 1, 0),
      .pEngineName = "kido",
      .engineVersion = VK_MAKE_VERSION(0, 1, 0),
      .apiVersion = VK_API_VERSION_1_3,
  };
  VkInstanceCreateInfo ici = {
      .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
      .pApplicationInfo = &app,
  };
  VkResult vr = vkCreateInstance(&ici, NULL, &w->instance);
  if (vr != VK_SUCCESS) {
    fprintf(stderr, "kido-vk-worker: vkCreateInstance failed (%d)\n", vr);
    return false;
  }

  uint32_t dev_count = 0;
  vkEnumeratePhysicalDevices(w->instance, &dev_count, NULL);
  if (dev_count == 0) {
    fprintf(stderr, "kido-vk-worker: no Vulkan physical devices found\n");
    return false;
  }
  VkPhysicalDevice *devs = calloc(dev_count, sizeof(*devs));
  if (!devs) return false;
  vkEnumeratePhysicalDevices(w->instance, &dev_count, devs);

  int best_idx = -1;
  int best_score = -1;
  uint32_t best_qf = 0;
  uint32_t best_ts_bits = 0;
  for (uint32_t i = 0; i < dev_count; i++) {
    VkPhysicalDeviceProperties p;
    vkGetPhysicalDeviceProperties(devs[i], &p);
    uint32_t qcount = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, NULL);
    VkQueueFamilyProperties *qprops = calloc(qcount ? qcount : 1, sizeof(*qprops));
    if (!qprops) continue;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, qprops);
    int qf = -1;
    uint32_t qf_ts_bits = 0;
    for (uint32_t q = 0; q < qcount; q++) {
      if (qprops[q].queueFlags & VK_QUEUE_COMPUTE_BIT) {
        qf = (int)q;
        qf_ts_bits = qprops[q].timestampValidBits;
        break;
      }
    }
    free(qprops);
    if (qf < 0) continue;

    int score = 1;
    if (p.deviceType == VK_PHYSICAL_DEVICE_TYPE_DISCRETE_GPU) score = 100;
    else if (p.deviceType == VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU) score = 80;
    else if (p.deviceType == VK_PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU) score = 40;
    else if (p.deviceType == VK_PHYSICAL_DEVICE_TYPE_CPU) score = 5;
    if (score > best_score) {
      best_score = score;
      best_idx = (int)i;
      best_qf = (uint32_t)qf;
      best_ts_bits = qf_ts_bits;
    }
  }
  if (best_idx < 0) {
    free(devs);
    fprintf(stderr, "kido-vk-worker: no Vulkan compute-capable GPU found\n");
    return false;
  }
  w->phys_dev = devs[best_idx];
  w->queue_family = best_qf;
  free(devs);

  vkGetPhysicalDeviceProperties(w->phys_dev, &w->props);
  vkGetPhysicalDeviceMemoryProperties(w->phys_dev, &w->mem_props);

  if (best_ts_bits > 0 && best_ts_bits <= 64 &&
      w->props.limits.timestampPeriod > 0.0f && w->props.limits.timestampPeriod < 1e9f) {
    w->has_timestamps = true;
    w->timestamp_period_ns = (double)w->props.limits.timestampPeriod;
    w->timestamp_mask = (best_ts_bits == 64) ? ~0ULL : ((1ULL << best_ts_bits) - 1ULL);
  }

  uint32_t ext_count = 0;
  vkEnumerateDeviceExtensionProperties(w->phys_dev, NULL, &ext_count, NULL);
  VkExtensionProperties *exts = calloc(ext_count ? ext_count : 1, sizeof(*exts));
  bool has_ext_mem_fd = false;
  bool has_ext_dmabuf = false;
  bool has_ext_host_ptr = false;
  bool has_ext_pci = false;
  bool has_ext_drm = false;
  bool has_ext_bf16 = false;
  if (exts && ext_count > 0) {
    vkEnumerateDeviceExtensionProperties(w->phys_dev, NULL, &ext_count, exts);
    for (uint32_t i = 0; i < ext_count; i++) {
      if (strcmp(exts[i].extensionName, VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME) == 0) has_ext_mem_fd = true;
      if (strcmp(exts[i].extensionName, VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME) == 0) has_ext_dmabuf = true;
      if (strcmp(exts[i].extensionName, VK_EXT_EXTERNAL_MEMORY_HOST_EXTENSION_NAME) == 0) has_ext_host_ptr = true;
      if (strcmp(exts[i].extensionName, VK_EXT_PCI_BUS_INFO_EXTENSION_NAME) == 0) has_ext_pci = true;
      if (strcmp(exts[i].extensionName, VK_EXT_PHYSICAL_DEVICE_DRM_EXTENSION_NAME) == 0) has_ext_drm = true;
#ifdef VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME
      if (strcmp(exts[i].extensionName, VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME) == 0) has_ext_bf16 = true;
#endif
    }
  }
  free(exts);
  (void)has_ext_bf16;

  VkPhysicalDeviceSubgroupProperties sg_props = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SUBGROUP_PROPERTIES,
  };
  VkPhysicalDeviceExternalMemoryHostPropertiesEXT host_mem_props = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_MEMORY_HOST_PROPERTIES_EXT,
  };
  VkPhysicalDevicePCIBusInfoPropertiesEXT pci_props = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PCI_BUS_INFO_PROPERTIES_EXT,
  };
  VkPhysicalDeviceDrmPropertiesEXT drm_props = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRM_PROPERTIES_EXT,
  };
  void *props_chain = &sg_props;
  if (has_ext_host_ptr) {
    host_mem_props.pNext = props_chain;
    props_chain = &host_mem_props;
  }
  if (has_ext_pci) {
    pci_props.pNext = props_chain;
    props_chain = &pci_props;
  }
  if (has_ext_drm) {
    drm_props.pNext = props_chain;
    props_chain = &drm_props;
  }
  VkPhysicalDeviceProperties2 props2 = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
      .pNext = props_chain,
  };
  vkGetPhysicalDeviceProperties2(w->phys_dev, &props2);
  w->subgroup_size = sg_props.subgroupSize ? sg_props.subgroupSize : 16;
  w->subgroup_ops = 0;
  if (sg_props.supportedStages & VK_SHADER_STAGE_COMPUTE_BIT) {
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_BASIC_BIT) w->subgroup_ops |= (1u << 0);
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_VOTE_BIT) w->subgroup_ops |= (1u << 1);
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_ARITHMETIC_BIT) w->subgroup_ops |= (1u << 2);
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_BALLOT_BIT) w->subgroup_ops |= (1u << 3);
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_SHUFFLE_BIT) w->subgroup_ops |= (1u << 4);
    if (sg_props.supportedOperations & VK_SUBGROUP_FEATURE_SHUFFLE_RELATIVE_BIT) w->subgroup_ops |= (1u << 5);
  }
  if (has_ext_host_ptr) {
    w->host_ptr_align = host_mem_props.minImportedHostPointerAlignment;
  }
  if (has_ext_pci) {
    w->has_pci_bus_info = true;
    w->pci_domain = pci_props.pciDomain;
    w->pci_bus = pci_props.pciBus;
    w->pci_device = pci_props.pciDevice;
    w->pci_function = pci_props.pciFunction;
  }
  w->drm_primary_major = -1;
  w->drm_primary_minor = -1;
  w->drm_render_major = -1;
  w->drm_render_minor = -1;
  if (has_ext_drm) {
    w->has_drm_props = true;
    if (drm_props.hasPrimary) {
      w->drm_primary_major = drm_props.primaryMajor;
      w->drm_primary_minor = drm_props.primaryMinor;
    }
    if (drm_props.hasRender) {
      w->drm_render_major = drm_props.renderMajor;
      w->drm_render_minor = drm_props.renderMinor;
    }
  }

#ifdef VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME
  VkPhysicalDeviceShaderBfloat16FeaturesKHR bf16_avail = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_BFLOAT16_FEATURES_KHR,
  };
#endif
  VkPhysicalDeviceVulkan12Features feat12_avail = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES,
#ifdef VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME
      .pNext = has_ext_bf16 ? &bf16_avail : NULL,
#endif
  };
  VkPhysicalDeviceVulkan11Features feat11_avail = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES,
      .pNext = &feat12_avail,
  };
  VkPhysicalDeviceFeatures2 feat2_avail = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2,
      .pNext = &feat11_avail,
  };
  vkGetPhysicalDeviceFeatures2(w->phys_dev, &feat2_avail);

  const char *enabled_exts[8];
  uint32_t enabled_ext_count = 0;
  if (has_ext_mem_fd && has_ext_dmabuf) {
    enabled_exts[enabled_ext_count++] = VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME;
    enabled_exts[enabled_ext_count++] = VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME;
    w->has_dmabuf_ext = true;
  }
  if (has_ext_host_ptr) {
    enabled_exts[enabled_ext_count++] = VK_EXT_EXTERNAL_MEMORY_HOST_EXTENSION_NAME;
    w->has_host_ptr_ext = true;
  }

#ifdef VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME
  VkPhysicalDeviceShaderBfloat16FeaturesKHR bf16_en = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_BFLOAT16_FEATURES_KHR,
  };
  if (has_ext_bf16 && bf16_avail.shaderBFloat16Type) {
    enabled_exts[enabled_ext_count++] = VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME;
    bf16_en.shaderBFloat16Type = VK_TRUE;
    w->has_bf16 = true;
  }
#endif

  VkPhysicalDeviceVulkan12Features feat12_en = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES,
#ifdef VK_KHR_SHADER_BFLOAT16_EXTENSION_NAME
      .pNext = w->has_bf16 ? &bf16_en : NULL,
#endif
      .shaderFloat16 = feat12_avail.shaderFloat16,
      .shaderInt8 = feat12_avail.shaderInt8,
      .storageBuffer8BitAccess = feat12_avail.storageBuffer8BitAccess,
  };
  VkPhysicalDeviceVulkan11Features feat11_en = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES,
      .pNext = &feat12_en,
      .storageBuffer16BitAccess = feat11_avail.storageBuffer16BitAccess,
      .uniformAndStorageBuffer16BitAccess = feat11_avail.uniformAndStorageBuffer16BitAccess,
  };
  VkPhysicalDeviceFeatures feat10_en = {
      .shaderInt16 = feat2_avail.features.shaderInt16,
      .shaderInt64 = feat2_avail.features.shaderInt64,
  };
  w->has_fp16 = feat12_avail.shaderFloat16 && feat11_avail.storageBuffer16BitAccess;
  w->has_int16 = feat2_avail.features.shaderInt16 != VK_FALSE;
  w->has_int8 = feat12_avail.shaderInt8 != VK_FALSE;
  w->has_storage_16bit = feat11_avail.storageBuffer16BitAccess != VK_FALSE;
  w->has_storage_8bit = feat12_avail.storageBuffer8BitAccess != VK_FALSE;
  w->has_int64 = feat2_avail.features.shaderInt64 != VK_FALSE;

  float prio = 1.0f;
  VkDeviceQueueCreateInfo qci = {
      .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
      .queueFamilyIndex = w->queue_family,
      .queueCount = 1,
      .pQueuePriorities = &prio,
  };
  VkDeviceCreateInfo dci = {
      .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
      .pNext = &feat11_en,
      .queueCreateInfoCount = 1,
      .pQueueCreateInfos = &qci,
      .enabledExtensionCount = enabled_ext_count,
      .ppEnabledExtensionNames = enabled_exts,
      .pEnabledFeatures = &feat10_en,
  };
  vr = vkCreateDevice(w->phys_dev, &dci, NULL, &w->device);
  if (vr != VK_SUCCESS) {
    fprintf(stderr, "kido-vk-worker: vkCreateDevice(%s) failed (%d)\n", w->props.deviceName, vr);
    return false;
  }
  vkGetDeviceQueue(w->device, w->queue_family, 0, &w->queue);

  if (w->has_dmabuf_ext) {
    w->pfn_get_mem_fd_props = (PFN_vkGetMemoryFdPropertiesKHR)vkGetDeviceProcAddr(
        w->device, "vkGetMemoryFdPropertiesKHR");
    if (!w->pfn_get_mem_fd_props) w->has_dmabuf_ext = false;
  }
  if (w->has_host_ptr_ext) {
    w->pfn_get_mem_host_ptr_props = (PFN_vkGetMemoryHostPointerPropertiesEXT)vkGetDeviceProcAddr(
        w->device, "vkGetMemoryHostPointerPropertiesEXT");
    if (!w->pfn_get_mem_host_ptr_props) w->has_host_ptr_ext = false;
  }

  VkCommandPoolCreateInfo cpci = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
      .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
      .queueFamilyIndex = w->queue_family,
  };
  if (vkCreateCommandPool(w->device, &cpci, NULL, &w->cmd_pool) != VK_SUCCESS) return false;

  VkCommandBuffer cmd_bufs[KMTL_MAX_INFLIGHT_CMDS] = {0};
  VkCommandBufferAllocateInfo cbai = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
      .commandPool = w->cmd_pool,
      .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
      .commandBufferCount = KMTL_MAX_INFLIGHT_CMDS,
  };
  if (vkAllocateCommandBuffers(w->device, &cbai, cmd_bufs) != VK_SUCCESS) return false;

  VkFenceCreateInfo fci = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
  VkDescriptorPoolSize pool_sz = {
      .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
      .descriptorCount = 8192,
  };
  VkDescriptorPoolCreateInfo dpci = {
      .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
      .maxSets = 1024,
      .poolSizeCount = 1,
      .pPoolSizes = &pool_sz,
  };
  VkQueryPoolCreateInfo qpci = {
      .sType = VK_STRUCTURE_TYPE_QUERY_POOL_CREATE_INFO,
      .queryType = VK_QUERY_TYPE_TIMESTAMP,
      .queryCount = 2,
  };

  for (uint32_t i = 0; i < KMTL_MAX_INFLIGHT_CMDS; i++) {
    w->slots[i].cmd_buf = cmd_bufs[i];
    if (vkCreateFence(w->device, &fci, NULL, &w->slots[i].fence) != VK_SUCCESS) return false;
    if (vkCreateDescriptorPool(w->device, &dpci, NULL, &w->slots[i].desc_pool) != VK_SUCCESS) return false;
    if (w->has_timestamps) {
      if (vkCreateQueryPool(w->device, &qpci, NULL, &w->slots[i].query_pool) != VK_SUCCESS) {
        w->has_timestamps = false;
        for (uint32_t j = 0; j < i; j++) {
          if (w->slots[j].query_pool) {
            vkDestroyQueryPool(w->device, w->slots[j].query_pool, NULL);
            w->slots[j].query_pool = VK_NULL_HANDLE;
          }
        }
      }
    }
  }

  w->staging_align = w->props.limits.minStorageBufferOffsetAlignment;
  if (w->staging_align < 16) w->staging_align = 16;

  uint64_t slot_staging = KMTL_STAGING_BYTES;
  for (int attempt = 0; attempt < 2; attempt++) {
    uint64_t total_staging = slot_staging * KMTL_MAX_INFLIGHT_CMDS;
    VkBufferCreateInfo bci = {
        .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
        .size = total_staging,
        .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT,
        .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
    };
    if (vkCreateBuffer(w->device, &bci, NULL, &w->staging_buf) == VK_SUCCESS) {
      VkMemoryRequirements mr;
      vkGetBufferMemoryRequirements(w->device, w->staging_buf, &mr);
      int mtype = find_memory_type(&w->mem_props, mr.memoryTypeBits,
                                   VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
      if (mtype >= 0) {
        VkMemoryAllocateInfo mai = {
            .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
            .allocationSize = mr.size,
            .memoryTypeIndex = (uint32_t)mtype,
        };
        if (vkAllocateMemory(w->device, &mai, NULL, &w->staging_mem) == VK_SUCCESS) {
          if (vkBindBufferMemory(w->device, w->staging_buf, w->staging_mem, 0) == VK_SUCCESS &&
              vkMapMemory(w->device, w->staging_mem, 0, total_staging, 0, (void **)&w->staging_ptr) == VK_SUCCESS) {
            w->slot_staging_bytes = slot_staging;
            for (uint32_t i = 0; i < KMTL_MAX_INFLIGHT_CMDS; i++) {
              w->slots[i].staging_base = (uint64_t)i * slot_staging;
            }
            break;
          }
          vkFreeMemory(w->device, w->staging_mem, NULL);
          w->staging_mem = VK_NULL_HANDLE;
        }
      }
      vkDestroyBuffer(w->device, w->staging_buf, NULL);
      w->staging_buf = VK_NULL_HANDLE;
    }
    slot_staging = KMTL_STAGING_FALLBACK_BYTES;
  }
  if (!w->staging_ptr) return false;

  w->udmabuf_fd = open("/dev/udmabuf", O_RDWR | O_CLOEXEC);
  if (w->has_dmabuf_ext || w->has_host_ptr_ext) {
    VkExternalMemoryHandleTypeFlags probe_types[2];
    size_t n_probe_types = 0;
    if (w->has_dmabuf_ext) probe_types[n_probe_types++] = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT;
    if (w->has_host_ptr_ext) probe_types[n_probe_types++] = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT;
    for (size_t pt = 0; pt < n_probe_types; pt++) {
      VkExternalMemoryBufferCreateInfo probe_ext = {
          .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
          .handleTypes = probe_types[pt],
      };
      VkBufferCreateInfo probe_bci = {
          .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
          .pNext = &probe_ext,
          .size = w->host_page_size,
          .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT |
                   VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT,
          .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
      };
      VkBuffer probe_buf = VK_NULL_HANDLE;
      if (vkCreateBuffer(w->device, &probe_bci, NULL, &probe_buf) == VK_SUCCESS) {
        VkMemoryRequirements mr;
        vkGetBufferMemoryRequirements(w->device, probe_buf, &mr);
        if (mr.size > w->host_page_size) {
          uint64_t pad = mr.size - w->host_page_size;
          if (pad > w->buf_extra_pad) w->buf_extra_pad = pad;
        }
        vkDestroyBuffer(w->device, probe_buf, NULL);
      }
    }
  }

  if (!probe_gpu_dma_coherency(w)) {
    fprintf(stderr, "kido-vk-worker: GPU coherency probe timed out; aborting init\n");
    return false;
  }

  /* Broadcom V3DV (vendorID 0x14e4) and small-workgroup GPUs hit per-CB memory or
     DRM job-timeout limits earlier than desktop/mid-range GPUs; split chunks more tightly. */
  bool small_gpu = (w->props.vendorID == 0x14e4u) ||
                   (w->props.limits.maxComputeWorkGroupInvocations <= 256u);
  w->max_chunk_dispatches = small_gpu ? KMTL_CHUNK_MAX_DISPATCHES_SMALL : KMTL_CHUNK_MAX_DISPATCHES;
  w->max_chunk_workgroups = small_gpu ? KMTL_CHUNK_MAX_WORKGROUPS_SMALL : KMTL_CHUNK_MAX_WORKGROUPS;

  const char *tier_name = (w->buf_tier == KMTL_BUF_TIER_DMABUF)     ? "dmabuf"
                          : (w->buf_tier == KMTL_BUF_TIER_HOST_PTR) ? "host_ptr"
                                                                    : "staging";
  fprintf(stderr,
          "kido-vk-worker: GPU=%s (subgroup=%u fp16=%d bf16=%d tier=%s dma_coherent=%d vmm_coherent=%d timestamps=%d slots=%u pad=%llu)\n",
          w->props.deviceName, w->subgroup_size, w->has_fp16 ? 1 : 0, w->has_bf16 ? 1 : 0, tier_name,
          w->dma_coherent ? 1 : 0, w->vmm_dma_coherent ? 1 : 0, w->has_timestamps ? 1 : 0,
          KMTL_MAX_INFLIGHT_CMDS, (unsigned long long)w->buf_extra_pad);
  return true;
}

static void vk_destroy_device_objects(worker_t *w) {
  if (w->device) {
    if (w->slots_in_flight > 0) drain_all_inflight(w);
    vkDeviceWaitIdle(w->device);
    if (w->staging_ptr && w->staging_mem) {
      vkUnmapMemory(w->device, w->staging_mem);
      w->staging_ptr = NULL;
    }
    if (w->staging_buf) vkDestroyBuffer(w->device, w->staging_buf, NULL);
    if (w->staging_mem) vkFreeMemory(w->device, w->staging_mem, NULL);
    for (uint32_t i = 0; i < KMTL_MAX_INFLIGHT_CMDS; i++) {
      if (w->slots[i].query_pool) vkDestroyQueryPool(w->device, w->slots[i].query_pool, NULL);
      if (w->slots[i].desc_pool) vkDestroyDescriptorPool(w->device, w->slots[i].desc_pool, NULL);
      if (w->slots[i].fence) vkDestroyFence(w->device, w->slots[i].fence, NULL);
    }
    if (w->cmd_pool) vkDestroyCommandPool(w->device, w->cmd_pool, NULL);
    vkDestroyDevice(w->device, NULL);
    w->device = VK_NULL_HANDLE;
  }
  if (w->instance) {
    vkDestroyInstance(w->instance, NULL);
    w->instance = VK_NULL_HANDLE;
  }
}

static bool has_client(worker_t *w, uint32_t cid) {
  if (cid == 0) return false;
  for (size_t i = 0; i < KMTL_MAX_CLIENTS; i++) {
    if (w->clients[i].active && w->clients[i].client_id == cid) return true;
  }
  return false;
}

static void destroy_buffer_slot_now(worker_t *w, vk_buffer_t *b) {
  if (!b->active) return;
  if (b->flush_deferred) {
    b->flush_deferred = false;
    if (w->deferred_flush_count > 0) w->deferred_flush_count--;
  }
  if (b->gpu_dirty) {
    b->gpu_dirty = false;
    if (w->gpu_dirty_count > 0) w->gpu_dirty_count--;
  }
  if (b->mapped && b->memory) {
    vkUnmapMemory(w->device, b->memory);
    b->mapped = NULL;
  }
  if (b->buffer) vkDestroyBuffer(w->device, b->buffer, NULL);
  if (b->memory) vkFreeMemory(w->device, b->memory, NULL);
  if (b->dmabuf_fd >= 0) close(b->dmabuf_fd);
  uint64_t freelen = b->shm_len ? b->shm_len : b->aligned_len;
  if (w->used_bytes >= freelen) w->used_bytes -= freelen;
  else w->used_bytes = 0;
  kmtl_shm_free(&w->shm_alloc, b->shm_offset, freelen);
  memset(b, 0, sizeof(*b));
  b->dmabuf_fd = -1;
}

static void destroy_buffer_slot(worker_t *w, vk_buffer_t *b) {
  if (!b->active) return;
  if (b->inflight_refs > 0) {
    /* Keep slot pinned and client_id charged to quota until in-flight GPU work retires. */
    b->pending_destroy = true;
    b->handle = 0;
    return;
  }
  destroy_buffer_slot_now(w, b);
}

static void destroy_library_slot(worker_t *w, vk_library_t *l) {
  if (!l->active) return;
  if (l->is_binary) {
    if (w->total_lib_binary_bytes >= l->binary_len) w->total_lib_binary_bytes -= l->binary_len;
    else w->total_lib_binary_bytes = 0;
    free(l->binary);
  } else {
    if (w->total_lib_source_bytes >= l->source_len) w->total_lib_source_bytes -= l->source_len;
    else w->total_lib_source_bytes = 0;
    free(l->source);
  }
  for (size_t i = 0; i < l->num_macros; i++) {
    free(l->macro_keys[i]);
    free(l->macro_vals[i]);
  }
  free(l->macro_keys);
  free(l->macro_vals);
  free(l->macros);
  memset(l, 0, sizeof(*l));
}

static void destroy_function_slot(worker_t *w, vk_function_t *f) {
  if (!f->active) return;
  if (w->total_fn_spirv_bytes >= f->spirv_bytes) w->total_fn_spirv_bytes -= f->spirv_bytes;
  else w->total_fn_spirv_bytes = 0;
  free(f->spirv);
  free(f->consts);
  memset(f, 0, sizeof(*f));
}

static void destroy_pipeline_slot_now(worker_t *w, vk_pipeline_t *p) {
  if (!p->active) return;
  for (uint32_t i = 0; i < p->num_specs; i++) {
    if (p->specs[i].pipeline) vkDestroyPipeline(w->device, p->specs[i].pipeline, NULL);
  }
  if (p->pipe_layout) vkDestroyPipelineLayout(w->device, p->pipe_layout, NULL);
  if (p->ds_layout) vkDestroyDescriptorSetLayout(w->device, p->ds_layout, NULL);
  if (p->shader) vkDestroyShaderModule(w->device, p->shader, NULL);
  free(p->consts);
  memset(p, 0, sizeof(*p));
}

static void destroy_pipeline_slot(worker_t *w, vk_pipeline_t *p) {
  if (!p->active) return;
  if (p->inflight_refs > 0) {
    /* Keep client_id charged to quota until in-flight GPU work retires. */
    p->pending_destroy = true;
    p->handle = 0;
    return;
  }
  destroy_pipeline_slot_now(w, p);
}

static bool close_client(worker_t *w, uint32_t cid, bool already_drained) {
  if (!already_drained) {
    if (!drain_client_inflight(w, cid)) return false;
  }
  for (size_t i = 0; i < KMTL_MAX_CLIENTS; i++) {
    if (w->clients[i].active && w->clients[i].client_id == cid) {
      w->clients[i].active = false;
      w->clients[i].client_id = 0;
    }
  }
  for (size_t i = 0; i < w->queues_cap; i++) {
    if (w->queues[i].active && w->queues[i].client_id == cid) memset(&w->queues[i], 0, sizeof(w->queues[i]));
  }
  for (size_t i = 0; i < w->pipelines_cap; i++) {
    if (w->pipelines[i].active && w->pipelines[i].client_id == cid) destroy_pipeline_slot(w, &w->pipelines[i]);
  }
  for (size_t i = 0; i < w->functions_cap; i++) {
    if (w->functions[i].active && w->functions[i].client_id == cid) destroy_function_slot(w, &w->functions[i]);
  }
  for (size_t i = 0; i < w->libraries_cap; i++) {
    if (w->libraries[i].active && w->libraries[i].client_id == cid) destroy_library_slot(w, &w->libraries[i]);
  }
  for (size_t i = 0; i < w->buffers_cap; i++) {
    if (w->buffers[i].active && w->buffers[i].client_id == cid) destroy_buffer_slot(w, &w->buffers[i]);
  }
  return true;
}

static bool reset_all(worker_t *w) {
  bool drained = drain_all_inflight(w);
  bool idle_ok = !w->device || (vkDeviceWaitIdle(w->device) == VK_SUCCESS);
  if (!drained && !idle_ok) {
    return false;
  }
  if (!drained && idle_ok) {
    /* GPU is confirmed idle after vkDeviceWaitIdle; safely reclaim orphaned slots. */
    for (uint32_t i = 0; i < KMTL_MAX_INFLIGHT_CMDS; i++) {
      vk_cmd_slot_t *s = &w->slots[i];
      for (size_t k = 0; k < s->n_deferred_pipes; k++) {
        vkDestroyPipeline(w->device, s->deferred_pipes[k], NULL);
      }
      s->n_deferred_pipes = 0;
      s->n_used_psos = 0;
      s->n_touched = 0;
      s->in_flight = false;
      s->has_gpu_cmd = false;
      s->had_timestamps = false;
    }
    w->slot_head = 0;
    w->slot_tail = 0;
    w->slots_in_flight = 0;
    w->deferred_flush_count = 0;
    w->gpu_dirty_count = 0;
  }
  for (size_t i = 0; i < KMTL_MAX_CLIENTS; i++) {
    if (w->clients[i].active) close_client(w, w->clients[i].client_id, true);
  }
  for (size_t i = 0; i < w->pipelines_cap; i++) {
    if (w->pipelines[i].active) {
      w->pipelines[i].inflight_refs = 0;
      destroy_pipeline_slot_now(w, &w->pipelines[i]);
    }
  }
  for (size_t i = 0; i < w->buffers_cap; i++) {
    if (w->buffers[i].active) {
      w->buffers[i].inflight_refs = 0;
      w->buffers[i].inflight_write_refs = 0;
      destroy_buffer_slot_now(w, &w->buffers[i]);
    }
  }
  return true;
}

static ptrdiff_t find_buffer_idx(worker_t *w, uint64_t h, uint32_t cid) {
  if (h == 0 || cid == 0) return -1;
  for (size_t i = 0; i < w->buffers_cap; i++) {
    if (w->buffers[i].active && !w->buffers[i].pending_destroy &&
        w->buffers[i].handle == h && w->buffers[i].client_id == cid) {
      return (ptrdiff_t)i;
    }
  }
  return -1;
}

static vk_buffer_t *find_buffer(worker_t *w, uint64_t h, uint32_t cid) {
  ptrdiff_t idx = find_buffer_idx(w, h, cid);
  return idx >= 0 ? &w->buffers[idx] : NULL;
}

static vk_library_t *find_library(worker_t *w, uint64_t h, uint32_t cid) {
  if (h == 0 || cid == 0) return NULL;
  for (size_t i = 0; i < w->libraries_cap; i++) {
    if (w->libraries[i].active && w->libraries[i].handle == h && w->libraries[i].client_id == cid)
      return &w->libraries[i];
  }
  return NULL;
}

static vk_function_t *find_function(worker_t *w, uint64_t h, uint32_t cid) {
  if (h == 0 || cid == 0) return NULL;
  for (size_t i = 0; i < w->functions_cap; i++) {
    if (w->functions[i].active && w->functions[i].handle == h && w->functions[i].client_id == cid)
      return &w->functions[i];
  }
  return NULL;
}

static ptrdiff_t find_pipeline_idx(worker_t *w, uint64_t h, uint32_t cid) {
  if (h == 0 || cid == 0) return -1;
  for (size_t i = 0; i < w->pipelines_cap; i++) {
    if (w->pipelines[i].active && !w->pipelines[i].pending_destroy &&
        w->pipelines[i].handle == h && w->pipelines[i].client_id == cid) {
      return (ptrdiff_t)i;
    }
  }
  return -1;
}

static vk_pipeline_t *find_pipeline(worker_t *w, uint64_t h, uint32_t cid) {
  ptrdiff_t idx = find_pipeline_idx(w, h, cid);
  return idx >= 0 ? &w->pipelines[idx] : NULL;
}

static vk_queue_t *find_queue(worker_t *w, uint64_t h, uint32_t cid) {
  if (h == 0 || cid == 0) return NULL;
  for (size_t i = 0; i < w->queues_cap; i++) {
    if (w->queues[i].active && w->queues[i].handle == h && w->queues[i].client_id == cid)
      return &w->queues[i];
  }
  return NULL;
}

#if !defined(__aarch64__)
static void dmabuf_sync(int fd, uint64_t flags) {
  if (fd < 0) return;
  struct dma_buf_sync s = {.flags = flags};
  while (ioctl(fd, DMA_BUF_IOCTL_SYNC, &s) < 0 && errno == EINTR) {}
}
#endif

static bool try_alloc_dmabuf_vk_buffer(worker_t *w,
                                       uint64_t shm_offset,
                                       uint64_t buf_len,
                                       uint64_t shm_len,
                                       VkBuffer *out_buf,
                                       VkDeviceMemory *out_mem,
                                       int *out_dmabuf_fd) {
  if (getenv("KIDO_VK_NO_DMABUF") != NULL) return false;
  if (w->udmabuf_fd < 0 || !w->has_dmabuf_ext || w->shm_fd < 0) return false;
  if (shm_len < buf_len) return false;
  struct udmabuf_create uc = {
      .memfd = (uint32_t)w->shm_fd,
      .flags = UDMABUF_FLAGS_CLOEXEC,
      .offset = shm_offset,
      .size = shm_len,
  };
  int dfd = ioctl(w->udmabuf_fd, UDMABUF_CREATE, &uc);
  if (dfd < 0) {
    if (!w->logged_dmabuf_fallback) {
      w->logged_dmabuf_fallback = true;
      fprintf(stderr,
              "kido-vk-worker: WARNING: UDMABUF_CREATE failed for %llu MiB buffer (%s); "
              "falling back to staging copies (check /sys/module/udmabuf/parameters/size_limit_mb)\n",
              (unsigned long long)(shm_len >> 20), strerror(errno));
    }
    return false;
  }

  VkExternalMemoryBufferCreateInfo ext_bci = {
      .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
      .handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
  };
  VkBufferCreateInfo bci = {
      .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
      .pNext = &ext_bci,
      .size = buf_len,
      .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT |
               VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT,
      .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
  };
  VkBuffer buf = VK_NULL_HANDLE;
  if (vkCreateBuffer(w->device, &bci, NULL, &buf) != VK_SUCCESS) {
    close(dfd);
    return false;
  }

  VkMemoryRequirements mr;
  vkGetBufferMemoryRequirements(w->device, buf, &mr);
  VkMemoryFdPropertiesKHR fd_props = {.sType = VK_STRUCTURE_TYPE_MEMORY_FD_PROPERTIES_KHR};
  if (w->pfn_get_mem_fd_props(w->device, VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT, dfd, &fd_props) !=
      VK_SUCCESS) {
    vkDestroyBuffer(w->device, buf, NULL);
    close(dfd);
    return false;
  }

  uint32_t valid_bits = mr.memoryTypeBits & fd_props.memoryTypeBits;
  int mtype = find_memory_type(&w->mem_props, valid_bits, VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT);
  if (mtype < 0) mtype = find_memory_type(&w->mem_props, valid_bits, 0);
  if (mtype < 0 || mr.size > shm_len) {
    vkDestroyBuffer(w->device, buf, NULL);
    close(dfd);
    return false;
  }

  int import_fd = fcntl(dfd, F_DUPFD_CLOEXEC, 0);
  if (import_fd < 0) {
    vkDestroyBuffer(w->device, buf, NULL);
    close(dfd);
    return false;
  }

  VkMemoryDedicatedAllocateInfo ded = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,
      .buffer = buf,
  };
  VkImportMemoryFdInfoKHR imp = {
      .sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR,
      .pNext = &ded,
      .handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
      .fd = import_fd,
  };
  VkMemoryAllocateInfo mai = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
      .pNext = &imp,
      .allocationSize = shm_len,
      .memoryTypeIndex = (uint32_t)mtype,
  };
  VkDeviceMemory mem = VK_NULL_HANDLE;
  if (vkAllocateMemory(w->device, &mai, NULL, &mem) != VK_SUCCESS) {
    close(import_fd);
    vkDestroyBuffer(w->device, buf, NULL);
    close(dfd);
    return false;
  }
  if (vkBindBufferMemory(w->device, buf, mem, 0) != VK_SUCCESS) {
    vkFreeMemory(w->device, mem, NULL);
    vkDestroyBuffer(w->device, buf, NULL);
    close(dfd);
    return false;
  }
#if !defined(__aarch64__)
  dmabuf_sync(dfd, DMA_BUF_SYNC_START | DMA_BUF_SYNC_RW);
#endif
  *out_buf = buf;
  *out_mem = mem;
  *out_dmabuf_fd = dfd;
  return true;
}

static uint64_t host_ptr_eff_align(const worker_t *w) {
  uint64_t a = w->host_page_size;
  if (w->host_ptr_align > a) {
    /* Only accept valid power-of-two alignments up to 64 KiB. */
    if ((w->host_ptr_align & (w->host_ptr_align - 1u)) != 0 || w->host_ptr_align > 65536u) {
      return 0;
    }
    a = w->host_ptr_align;
  }
  return a;
}

static bool try_alloc_host_ptr_vk_buffer(worker_t *w,
                                         uint64_t shm_offset,
                                         uint64_t buf_len,
                                         uint64_t shm_len,
                                         VkBuffer *out_buf,
                                         VkDeviceMemory *out_mem) {
  if (getenv("KIDO_VK_NO_HOST_PTR") != NULL) return false;
  if (!w->has_host_ptr_ext || !w->pfn_get_mem_host_ptr_props || !w->shm_base) return false;
  if (shm_len < buf_len || shm_offset > w->shm_size || shm_len > w->shm_size - shm_offset) return false;

  uint64_t align = host_ptr_eff_align(w);
  if (align == 0 || (shm_offset % align) != 0 || (shm_len % align) != 0 ||
      (((uintptr_t)w->shm_base) % align) != 0) {
    return false;
  }

  VkExternalMemoryBufferCreateInfo ext_bci = {
      .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
      .handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT,
  };
  VkBufferCreateInfo bci = {
      .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
      .pNext = &ext_bci,
      .size = buf_len,
      .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT |
               VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT,
      .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
  };
  VkBuffer buf = VK_NULL_HANDLE;
  if (vkCreateBuffer(w->device, &bci, NULL, &buf) != VK_SUCCESS) return false;

  VkMemoryRequirements mr;
  vkGetBufferMemoryRequirements(w->device, buf, &mr);
  if (mr.size > shm_len) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }

  void *host_ptr = w->shm_base + shm_offset;
  VkMemoryHostPointerPropertiesEXT hp_props = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_HOST_POINTER_PROPERTIES_EXT,
  };
  if (w->pfn_get_mem_host_ptr_props(w->device, VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT,
                                    host_ptr, &hp_props) != VK_SUCCESS) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }

  /* Require HOST_COHERENT so GPU-side visibility is guaranteed across submit/fence boundaries. */
  uint32_t valid_bits = mr.memoryTypeBits & hp_props.memoryTypeBits;
  int mtype = find_memory_type(&w->mem_props, valid_bits,
                               VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
  if (mtype < 0) mtype = find_memory_type(&w->mem_props, valid_bits, VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
  if (mtype < 0) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }

  VkImportMemoryHostPointerInfoEXT imp = {
      .sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT,
      .handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT,
      .pHostPointer = host_ptr,
  };
  VkMemoryAllocateInfo mai = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
      .pNext = &imp,
      .allocationSize = shm_len,
      .memoryTypeIndex = (uint32_t)mtype,
  };
  VkDeviceMemory mem = VK_NULL_HANDLE;
  if (vkAllocateMemory(w->device, &mai, NULL, &mem) != VK_SUCCESS) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  if (vkBindBufferMemory(w->device, buf, mem, 0) != VK_SUCCESS) {
    vkFreeMemory(w->device, mem, NULL);
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  *out_buf = buf;
  *out_mem = mem;
  return true;
}

#if defined(__aarch64__)
static bool path_exists(const char *p) {
  struct stat st;
  return stat(p, &st) == 0;
}

static bool check_of_node_dma_coherent(const char *dev_path, bool *out_found_of_node) {
  *out_found_of_node = false;
  char sys_cur[PATH_MAX];
  char of_link[PATH_MAX];
  char cur[PATH_MAX];
  if (!realpath(dev_path, sys_cur)) return false;

  /* Check device of_node directly, then walk up parent sysfs devices (e.g. PCIe host bridge). */
  bool resolved_of = false;
  for (int up = 0; up < 8; up++) {
    if (snprintf(of_link, sizeof(of_link), "%s/of_node", sys_cur) < (int)sizeof(of_link) &&
        realpath(of_link, cur) != NULL) {
      resolved_of = true;
      break;
    }
    if (strcmp(sys_cur, "/sys/devices") == 0 || strcmp(sys_cur, "/sys") == 0 || strcmp(sys_cur, "/") == 0) break;
    char *slash = strrchr(sys_cur, '/');
    if (!slash || slash == sys_cur) break;
    *slash = '\0';
  }
  if (!resolved_of) return false;

  *out_found_of_node = true;
  for (int depth = 0; depth < 16; depth++) {
    char prop[PATH_MAX];
    if (snprintf(prop, sizeof(prop), "%s/dma-noncoherent", cur) < (int)sizeof(prop) && path_exists(prop)) {
      return false;
    }
    if (snprintf(prop, sizeof(prop), "%s/dma-coherent", cur) < (int)sizeof(prop) && path_exists(prop)) {
      return true;
    }
    if (strcmp(cur, "/sys/firmware/devicetree/base") == 0 || strcmp(cur, "/") == 0) break;
    char *slash = strrchr(cur, '/');
    if (!slash || slash == cur) break;
    *slash = '\0';
  }
  return false;
}

static bool detect_fw_dma_coherent(const worker_t *w) {
  char dev_path[PATH_MAX];
  dev_path[0] = '\0';
  if (w->has_pci_bus_info) {
    snprintf(dev_path, sizeof(dev_path), "/sys/bus/pci/devices/%04x:%02x:%02x.%u",
             w->pci_domain, w->pci_bus, w->pci_device, w->pci_function);
  } else if (w->has_drm_props && w->drm_render_major >= 0) {
    snprintf(dev_path, sizeof(dev_path), "/sys/dev/char/%lld:%lld/device",
             (long long)w->drm_render_major, (long long)w->drm_render_minor);
  } else if (w->has_drm_props && w->drm_primary_major >= 0) {
    snprintf(dev_path, sizeof(dev_path), "/sys/dev/char/%lld:%lld/device",
             (long long)w->drm_primary_major, (long long)w->drm_primary_minor);
  }
  if (dev_path[0] != '\0') {
    bool found_of = false;
    bool dt_coh = check_of_node_dma_coherent(dev_path, &found_of);
    if (found_of) return dt_coh;
  }
  /* ARM64 ACPI server/workstation without DT (e.g. Grace Blackwell/Hopper, Graviton, Ampere):
     candidate for empirical L1-hot hardware coherency verification below. */
  if (!path_exists("/sys/firmware/devicetree/base") && path_exists("/sys/firmware/acpi")) {
    return true;
  }
  return false;
}
#endif

static bool probe_gpu_dma_coherency(worker_t *w) {
  w->buf_tier = KMTL_BUF_TIER_STAGING;
  w->dma_coherent = false;
  w->vmm_dma_coherent = true;
  if (!w->shm_base || w->shm_size < 65536u) return true;

  const uint64_t probe_bytes = 65536u;
  uint64_t probe_shm_dmabuf = align_up(probe_bytes + w->buf_extra_pad, w->host_page_size);
  if (probe_shm_dmabuf == 0 || probe_shm_dmabuf > w->shm_size) probe_shm_dmabuf = probe_bytes;

  uint64_t hp_align = host_ptr_eff_align(w);
  uint64_t probe_shm_hp = hp_align ? align_up(probe_bytes + w->buf_extra_pad, hp_align) : 0;
  if (probe_shm_hp > w->shm_size) probe_shm_hp = 0;

  uint64_t probe_shm = probe_shm_dmabuf;
  VkBuffer buf = VK_NULL_HANDLE;
  VkDeviceMemory mem = VK_NULL_HANDLE;
  int dfd = -1;
  if (try_alloc_dmabuf_vk_buffer(w, 0, probe_bytes, probe_shm_dmabuf, &buf, &mem, &dfd)) {
    w->buf_tier = KMTL_BUF_TIER_DMABUF;
    probe_shm = probe_shm_dmabuf;
    w->vmm_dma_coherent = false;
  } else if (probe_shm_hp >= probe_bytes &&
             try_alloc_host_ptr_vk_buffer(w, 0, probe_bytes, probe_shm_hp, &buf, &mem)) {
    w->buf_tier = KMTL_BUF_TIER_HOST_PTR;
    probe_shm = probe_shm_hp;
    w->vmm_dma_coherent = false;
  } else {
    /* Staging fallback uses CPU memcpy between shm_base and Vulkan HOST_COHERENT memory,
       so guest-to-host CPU visibility is always hardware-coherent without VMM PoC flushes. */
    w->buf_tier = KMTL_BUF_TIER_STAGING;
    w->dma_coherent = false;
    w->vmm_dma_coherent = true;
    return true;
  }

  const char *env = getenv("KIDO_VK_DMA_COHERENT");
  if (env && strcmp(env, "0") == 0) {
    goto cleanup;
  }

  bool fw_coh = false;
#if defined(__aarch64__)
  fw_coh = detect_fw_dma_coherent(w);
#else
  fw_coh = true;
#endif
  if (!fw_coh && !(env && strcmp(env, "1") == 0)) {
    goto cleanup;
  }

  /* Live 4-round L1-hot GPU coherency self-test: ZERO dc civac, only Outer Shareable dmb osh.
     src (32 KiB) and dst (32 KiB) both stay dirty+hot in L1 D-cache; a non-coherent GPU
     reads stale DRAM for src and CPU reads stale sentinel from L1 for dst, failing immediately. */
  bool all_ok = true;
  bool gpu_wedged = false;
  volatile uint32_t *src = (volatile uint32_t *)(w->shm_base);
  volatile uint32_t *dst = (volatile uint32_t *)(w->shm_base + 32768u);
  const size_t nwords = 32768u / sizeof(uint32_t);
  VkCommandBuffer cb = w->slots[0].cmd_buf;
  VkFence fence = w->slots[0].fence;

  for (uint32_t r = 0; r < 4 && all_ok; r++) {
    uint32_t sentinel = 0xDEADBEE0u ^ (r * 0x11111111u);
    uint32_t seed = 0xA5A55A50u ^ (r * 0x01010101u);
    for (size_t i = 0; i < nwords; i++) {
      dst[i] = sentinel;
      src[i] = seed ^ ((uint32_t)i * 0x9E3779B1u);
    }
#if defined(__aarch64__)
    __asm__ volatile("dmb osh" ::: "memory");
#else
    __atomic_thread_fence(__ATOMIC_SEQ_CST);
#endif
    vkResetCommandBuffer(cb, 0);
    VkCommandBufferBeginInfo cbbi = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
        .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
    };
    if (vkBeginCommandBuffer(cb, &cbbi) != VK_SUCCESS) { all_ok = false; break; }
    VkBufferCopy region = {.srcOffset = 0, .dstOffset = 32768u, .size = 32768u};
    vkCmdCopyBuffer(cb, buf, buf, 1, &region);
    if (vkEndCommandBuffer(cb) != VK_SUCCESS) { all_ok = false; break; }
    vkResetFences(w->device, 1, &fence);
    VkSubmitInfo si = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1,
        .pCommandBuffers = &cb,
    };
    if (vkQueueSubmit(w->queue, 1, &si, fence) != VK_SUCCESS) {
      all_ok = false;
      break;
    }
    if (vkWaitForFences(w->device, 1, &fence, VK_TRUE, 5000000000ULL) != VK_SUCCESS) {
      if (vkDeviceWaitIdle(w->device) != VK_SUCCESS) {
        /* GPU may still be referencing buf/mem and slots[0].cmd_buf; abort init safely. */
        gpu_wedged = true;
      }
      all_ok = false;
      break;
    }
#if defined(__aarch64__)
    __asm__ volatile("dmb osh" ::: "memory");
#else
    __atomic_thread_fence(__ATOMIC_SEQ_CST);
#endif
    for (size_t i = 0; i < nwords; i++) {
      uint32_t want = seed ^ ((uint32_t)i * 0x9E3779B1u);
      if (dst[i] != want) {
        all_ok = false;
        break;
      }
    }
  }

  if (gpu_wedged) {
    return false;
  }
  vkResetCommandBuffer(cb, 0);

  if (all_ok) {
    w->dma_coherent = true;
    w->vmm_dma_coherent = true;
  } else {
    fprintf(stderr, "kido-vk-worker: live hardware coherency probe failed; using explicit PoC cache sync\n");
    w->dma_coherent = false;
    w->vmm_dma_coherent = false;
  }

cleanup:
#if !defined(__aarch64__)
  /* On non-ARM64 hosts there is no explicit host_ptr PoC flush; fall back to staging if non-coherent. */
  if (w->buf_tier == KMTL_BUF_TIER_HOST_PTR && !w->dma_coherent) {
    w->buf_tier = KMTL_BUF_TIER_STAGING;
    w->vmm_dma_coherent = true;
  }
#endif
  vkDestroyBuffer(w->device, buf, NULL);
  vkFreeMemory(w->device, mem, NULL);
  if (dfd >= 0) close(dfd);
  memset(w->shm_base, 0, (size_t)probe_shm);
#if defined(__aarch64__)
  if (!w->dma_coherent) arm64_clean_inv_dcache_poc(w->shm_base, (size_t)probe_shm);
#endif
  return true;
}

static bool alloc_host_visible_vk_buffer(worker_t *w,
                                         uint64_t aligned_len,
                                         VkBuffer *out_buf,
                                         VkDeviceMemory *out_mem,
                                         void **out_mapped) {
  VkBufferCreateInfo bci = {
      .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
      .size = aligned_len,
      .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT |
               VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT,
      .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
  };
  VkBuffer buf = VK_NULL_HANDLE;
  if (vkCreateBuffer(w->device, &bci, NULL, &buf) != VK_SUCCESS) return false;
  VkMemoryRequirements mr;
  vkGetBufferMemoryRequirements(w->device, buf, &mr);
  int mtype = find_memory_type(&w->mem_props, mr.memoryTypeBits,
                               VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
  if (mtype < 0) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  VkMemoryAllocateInfo mai = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
      .allocationSize = mr.size,
      .memoryTypeIndex = (uint32_t)mtype,
  };
  VkDeviceMemory mem = VK_NULL_HANDLE;
  if (vkAllocateMemory(w->device, &mai, NULL, &mem) != VK_SUCCESS) {
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  if (vkBindBufferMemory(w->device, buf, mem, 0) != VK_SUCCESS) {
    vkFreeMemory(w->device, mem, NULL);
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  void *ptr = NULL;
  if (vkMapMemory(w->device, mem, 0, aligned_len, 0, &ptr) != VK_SUCCESS) {
    vkFreeMemory(w->device, mem, NULL);
    vkDestroyBuffer(w->device, buf, NULL);
    return false;
  }
  *out_buf = buf;
  *out_mem = mem;
  *out_mapped = ptr;
  return true;
}

static void resolve_executable_in_path(const char *name, char *out, size_t out_cap) {
  out[0] = '\0';
  const char *path_env = getenv("PATH");
  if (!path_env || !*path_env) path_env = "/usr/local/bin:/usr/bin:/bin";
  const char *p = path_env;
  while (*p) {
    const char *colon = strchr(p, ':');
    size_t dlen = colon ? (size_t)(colon - p) : strlen(p);
    if (dlen > 0 && dlen + 1 + strlen(name) + 1 <= out_cap) {
      memcpy(out, p, dlen);
      out[dlen] = '/';
      strcpy(out + dlen + 1, name);
      if (access(out, X_OK) == 0) return;
    }
    if (!colon) break;
    p = colon + 1;
  }
  out[0] = '\0';
}

static void close_fds_from(int min_fd) {
#ifdef SYS_close_range
  if (syscall(SYS_close_range, (unsigned int)min_fd, ~0U, 0U) == 0) return;
#endif
  long max_fd = sysconf(_SC_OPEN_MAX);
  if (max_fd < 0 || max_fd > 4096) max_fd = 4096;
  for (int fd = min_fd; fd < (int)max_fd; fd++) {
    close(fd);
  }
}

#ifdef KIDO_HAVE_AIR2SPV
static void resolve_self_exe(char *out, size_t out_cap) {
  out[0] = '\0';
  ssize_t n = readlink("/proc/self/exe", out, out_cap - 1);
  if (n > 0 && (size_t)n < out_cap) {
    out[n] = '\0';
  } else {
    out[0] = '\0';
  }
}

typedef struct {
  air2spv_caps_t caps;
  uint32_t mlib_len;
  uint32_t name_len;
} air_sb_req_t;

typedef struct {
  int32_t ok;
  uint32_t binding_mask;
  uint32_t readonly_mask;
  uint32_t payload_len;
} air_sb_resp_t;

static void air_sandbox_kill(worker_t *w) {
  if (w->air_sb_fd >= 0) {
    close(w->air_sb_fd);
    w->air_sb_fd = -1;
  }
  if (w->air_sb_pid > 0) {
    kill(w->air_sb_pid, SIGKILL);
    int st = 0;
    while (waitpid(w->air_sb_pid, &st, 0) < 0 && errno == EINTR) {}
    w->air_sb_pid = -1;
  }
}

static bool sb_send_all_deadline(int fd, const void *buf, size_t len, uint64_t deadline) {
  const uint8_t *p = (const uint8_t *)buf;
  size_t done = 0;
  while (done < len) {
    uint64_t now = monotonic_ns();
    if (now >= deadline) return false;
    int rem_ms = (int)((deadline - now) / 1000000ULL);
    if (rem_ms < 1) rem_ms = 1;
    struct pollfd pfd = {.fd = fd, .events = POLLOUT};
    int pr = poll(&pfd, 1, rem_ms);
    if (pr < 0) {
      if (errno == EINTR) continue;
      return false;
    }
    if (pr == 0) return false;
    ssize_t n = send(fd, p + done, len - done, MSG_NOSIGNAL | MSG_DONTWAIT);
    if (n < 0) {
      if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) continue;
      return false;
    }
    if (n == 0) return false;
    done += (size_t)n;
  }
  return true;
}

static bool sb_recv_all_deadline(int fd, void *buf, size_t len, uint64_t deadline) {
  uint8_t *p = (uint8_t *)buf;
  size_t done = 0;
  while (done < len) {
    uint64_t now = monotonic_ns();
    if (now >= deadline) return false;
    int rem_ms = (int)((deadline - now) / 1000000ULL);
    if (rem_ms < 1) rem_ms = 1;
    struct pollfd pfd = {.fd = fd, .events = POLLIN};
    int pr = poll(&pfd, 1, rem_ms);
    if (pr < 0) {
      if (errno == EINTR) continue;
      return false;
    }
    if (pr == 0) return false;
    ssize_t n = recv(fd, p + done, len - done, MSG_DONTWAIT);
    if (n < 0) {
      if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) continue;
      return false;
    }
    if (n == 0) return false;
    done += (size_t)n;
  }
  return true;
}

static bool air_sandbox_ensure_started(worker_t *w, char *err_buf, size_t err_cap) {
  if (w->air_sb_fd >= 0 && w->air_sb_pid > 0) return true;
  if (w->self_exe[0] == '\0') {
    snprintf(err_buf, err_cap, "unable to resolve self executable for AIR sandbox");
    return false;
  }
  int sv[2];
  if (socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sv) != 0) {
    snprintf(err_buf, err_cap, "socketpair for AIR sandbox failed");
    return false;
  }
  pid_t parent_pid = getpid();
  pid_t pid = fork();
  if (pid < 0) {
    close(sv[0]);
    close(sv[1]);
    snprintf(err_buf, err_cap, "fork for AIR sandbox failed");
    return false;
  }
  if (pid == 0) {
    close(sv[0]);
    prctl(PR_SET_PDEATHSIG, SIGKILL, 0, 0, 0);
    if (getppid() != parent_pid) _exit(1);
    prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
    int devnull = open("/dev/null", O_RDONLY);
    if (devnull >= 0) {
      dup2(devnull, STDIN_FILENO);
      if (devnull > 2 && devnull != sv[1]) close(devnull);
    }
    /* Pin sandbox IPC socket to FD 3 and close all higher FDs (GPU/DRM/SHM/control). */
    if (sv[1] != 3) {
      if (dup2(sv[1], 3) < 0) _exit(126);
      close(sv[1]);
    }
    if (fcntl(3, F_SETFD, 0) < 0) _exit(126);
    close_fds_from(4);
    struct rlimit rl_as = {1024ULL << 20, 1024ULL << 20};
    struct rlimit rl_fsize = {0, 0};
    setrlimit(RLIMIT_AS, &rl_as);
    setrlimit(RLIMIT_FSIZE, &rl_fsize);
    char *argv[] = {w->self_exe, "--air-sandbox", "--sandbox-fd", "3", NULL};
    execve(w->self_exe, argv, environ);
    _exit(127);
  }
  close(sv[1]);
  w->air_sb_fd = sv[0];
  w->air_sb_pid = pid;
  return true;
}

static bool compile_metallib_sandboxed(worker_t *w,
                                       const uint8_t *mlib,
                                       size_t mlib_len,
                                       const char *fn_name,
                                       const air2spv_caps_t *caps,
                                       air2spv_result_t *out,
                                       char *err_buf,
                                       size_t err_cap) {
  memset(out, 0, sizeof(*out));
  size_t name_len = strlen(fn_name);
  if (mlib_len == 0 || mlib_len > METALLIB_MAX_BYTES || name_len == 0 || name_len > 127u) {
    snprintf(err_buf, err_cap, "invalid metallib compile request");
    return false;
  }
  if (!air_sandbox_ensure_started(w, err_buf, err_cap)) return false;

  air_sb_req_t req;
  memset(&req, 0, sizeof(req));
  req.caps = *caps;
  req.mlib_len = (uint32_t)mlib_len;
  req.name_len = (uint32_t)name_len;

  uint64_t deadline = monotonic_ns() + (uint64_t)KMTL_COMPILE_TIMEOUT_MS * 1000000ULL;
  if (!sb_send_all_deadline(w->air_sb_fd, &req, sizeof(req), deadline) ||
      !sb_send_all_deadline(w->air_sb_fd, mlib, mlib_len, deadline) ||
      !sb_send_all_deadline(w->air_sb_fd, fn_name, name_len, deadline)) {
    air_sandbox_kill(w);
    snprintf(err_buf, err_cap, "AIR compiler sandbox timed out or crashed while sending input");
    return false;
  }

  air_sb_resp_t resp;
  memset(&resp, 0, sizeof(resp));
  if (!sb_recv_all_deadline(w->air_sb_fd, &resp, sizeof(resp), deadline)) {
    air_sandbox_kill(w);
    snprintf(err_buf, err_cap, "AIR compiler sandbox crashed, exceeded memory limit, or timed out");
    return false;
  }

  if (!resp.ok) {
    size_t elen = resp.payload_len;
    if (elen > 4096u) {
      air_sandbox_kill(w);
      snprintf(err_buf, err_cap, "AIR compiler sandbox protocol error");
      return false;
    }
    char tmp[4097];
    if (elen > 0 && !sb_recv_all_deadline(w->air_sb_fd, tmp, elen, deadline)) {
      air_sandbox_kill(w);
      snprintf(err_buf, err_cap, "AIR compiler sandbox truncated error");
      return false;
    }
    tmp[elen] = '\0';
    snprintf(err_buf, err_cap, "%s", elen > 0 ? tmp : "AIR compilation failed");
    return false;
  }

  if (resp.payload_len < 20u || (resp.payload_len & 3u) != 0 || resp.payload_len > (8u << 20)) {
    air_sandbox_kill(w);
    snprintf(err_buf, err_cap, "AIR compiler sandbox returned invalid SPIR-V size");
    return false;
  }
  uint32_t *words = malloc(resp.payload_len);
  if (!words) {
    air_sandbox_kill(w);
    snprintf(err_buf, err_cap, "host OOM receiving SPIR-V from AIR sandbox");
    return false;
  }
  if (!sb_recv_all_deadline(w->air_sb_fd, words, resp.payload_len, deadline)) {
    free(words);
    air_sandbox_kill(w);
    snprintf(err_buf, err_cap, "AIR compiler sandbox truncated SPIR-V payload");
    return false;
  }
  out->spirv = words;
  out->spirv_bytes = resp.payload_len;
  out->binding_mask = resp.binding_mask;
  out->readonly_mask = resp.readonly_mask;
  return true;
}

static int run_air_sandbox(int fd) {
  if (fd < 0) return 2;
  for (;;) {
    air_sb_req_t req;
    if (!read_full(fd, &req, sizeof(req))) break;
    if (req.mlib_len == 0 || req.mlib_len > METALLIB_MAX_BYTES || req.name_len == 0 || req.name_len > 127u) {
      break;
    }
    uint8_t *mlib = malloc(req.mlib_len);
    char fn_name[128];
    if (!mlib) break;
    if (!read_full(fd, mlib, req.mlib_len) || !read_full(fd, fn_name, req.name_len)) {
      free(mlib);
      break;
    }
    fn_name[req.name_len] = '\0';

    air2spv_result_t ar = {0};
    char err[1024] = {0};
    bool ok = air2spv_compile_metallib(mlib, req.mlib_len, fn_name, &req.caps, &ar, err, sizeof(err));
    free(mlib);

    air_sb_resp_t resp;
    memset(&resp, 0, sizeof(resp));
    if (ok && ar.spirv_bytes <= (8u << 20)) {
      resp.ok = 1;
      resp.binding_mask = ar.binding_mask;
      resp.readonly_mask = ar.readonly_mask;
      resp.payload_len = (uint32_t)ar.spirv_bytes;
      bool wok = write_full(fd, &resp, sizeof(resp)) &&
                 (ar.spirv_bytes == 0 || write_full(fd, ar.spirv, ar.spirv_bytes));
      free(ar.spirv);
      if (!wok) break;
    } else {
      if (ok) {
        free(ar.spirv);
        snprintf(err, sizeof(err), "AIR SPIR-V output exceeded 8 MiB limit");
      }
      size_t elen = strlen(err);
      resp.ok = 0;
      resp.payload_len = (uint32_t)elen;
      if (!write_full(fd, &resp, sizeof(resp)) || (elen > 0 && !write_full(fd, err, elen))) break;
    }
  }
  close(fd);
  return 0;
}
#endif

static bool compile_glsl_to_spirv(const char *glslang_path,
                                  const char *glsl,
                                  size_t glsl_len,
                                  uint32_t **out_words,
                                  size_t *out_bytes,
                                  char *err_buf,
                                  size_t err_cap) {
  if (!glslang_path || glslang_path[0] == '\0') {
    snprintf(err_buf, err_cap, "glslangValidator not found in PATH");
    return false;
  }
  char tmp_dir[] = "/tmp/kido-vk-XXXXXX";
  if (!mkdtemp(tmp_dir)) {
    snprintf(err_buf, err_cap, "mkdtemp failed: %s", strerror(errno));
    return false;
  }

  char comp_path[128];
  char spv_path[128];
  snprintf(comp_path, sizeof(comp_path), "%s/in.comp", tmp_dir);
  snprintf(spv_path, sizeof(spv_path), "%s/out.spv", tmp_dir);

  int cfd = open(comp_path, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
  if (cfd < 0) {
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "open(in.comp) failed: %s", strerror(errno));
    return false;
  }
  bool wok = write_full(cfd, glsl, glsl_len);
  close(cfd);
  if (!wok) {
    unlink(comp_path);
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "failed writing GLSL temp file");
    return false;
  }

  int pfd[2];
  if (pipe2(pfd, O_CLOEXEC) != 0) {
    unlink(comp_path);
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "pipe2 failed");
    return false;
  }

  pid_t parent_pid = getpid();
  pid_t pid = fork();
  if (pid < 0) {
    close(pfd[0]);
    close(pfd[1]);
    unlink(comp_path);
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "fork(glslangValidator) failed");
    return false;
  }
  if (pid == 0) {
    close(pfd[0]);
    prctl(PR_SET_PDEATHSIG, SIGKILL, 0, 0, 0);
    if (getppid() != parent_pid) _exit(1);
    prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
    int devnull = open("/dev/null", O_RDONLY);
    if (devnull >= 0) {
      dup2(devnull, STDIN_FILENO);
      if (devnull > 2 && devnull != pfd[1]) close(devnull);
    }
    dup2(pfd[1], STDOUT_FILENO);
    dup2(pfd[1], STDERR_FILENO);
    if (pfd[1] > 2) close(pfd[1]);
    close_fds_from(3);
    struct rlimit rl_cpu = {15, 15};
    struct rlimit rl_as = {512ULL << 20, 512ULL << 20};
    struct rlimit rl_fsize = {32ULL << 20, 32ULL << 20};
    setrlimit(RLIMIT_CPU, &rl_cpu);
    setrlimit(RLIMIT_AS, &rl_as);
    setrlimit(RLIMIT_FSIZE, &rl_fsize);
    char *argv[] = {(char *)glslang_path, "-V", "--target-env", "vulkan1.3", "-S", "comp", "-o", spv_path, comp_path,
                    NULL};
    execve(glslang_path, argv, environ);
    _exit(127);
  }

  close(pfd[1]);
  char log_buf[2048];
  size_t log_len = 0;
  uint64_t deadline = monotonic_ns() + (uint64_t)KMTL_COMPILE_TIMEOUT_MS * 1000000ULL;
  bool timed_out = false;
  for (;;) {
    uint64_t now = monotonic_ns();
    if (now >= deadline) {
      timed_out = true;
      break;
    }
    int rem_ms = (int)((deadline - now) / 1000000ULL);
    if (rem_ms < 1) rem_ms = 1;
    struct pollfd p = {.fd = pfd[0], .events = POLLIN};
    int pr = poll(&p, 1, rem_ms);
    if (pr < 0) {
      if (errno == EINTR) continue;
      break;
    }
    if (pr == 0) {
      timed_out = true;
      break;
    }
    char chunk[512];
    ssize_t r = read(pfd[0], chunk, sizeof(chunk));
    if (r < 0 && errno == EINTR) continue;
    if (r <= 0) break;
    size_t avail = sizeof(log_buf) - 1 - log_len;
    size_t copy = (size_t)r < avail ? (size_t)r : avail;
    if (copy > 0) {
      memcpy(log_buf + log_len, chunk, copy);
      log_len += copy;
    }
  }
  log_buf[log_len] = '\0';
  close(pfd[0]);

  if (timed_out) {
    kill(pid, SIGKILL);
  }
  int status = 0;
  while (waitpid(pid, &status, 0) < 0 && errno == EINTR) {}
  unlink(comp_path);

  if (timed_out) {
    unlink(spv_path);
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "glslangValidator timed out");
    return false;
  }
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
    unlink(spv_path);
    rmdir(tmp_dir);
    snprintf(err_buf, err_cap, "glslangValidator failed: %.512s", log_len ? log_buf : "unknown error");
    return false;
  }

  int sfd = open(spv_path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
  unlink(spv_path);
  rmdir(tmp_dir);
  if (sfd < 0) {
    snprintf(err_buf, err_cap, "failed to open compiled SPIR-V");
    return false;
  }
  struct stat st;
  if (fstat(sfd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size <= 0 ||
      (st.st_size & 3) != 0 || st.st_size > (16 << 20)) {
    close(sfd);
    snprintf(err_buf, err_cap, "invalid SPIR-V output size");
    return false;
  }
  uint32_t *words = malloc((size_t)st.st_size);
  if (!words || !read_full(sfd, words, (size_t)st.st_size)) {
    free(words);
    close(sfd);
    snprintf(err_buf, err_cap, "failed reading SPIR-V bytes");
    return false;
  }
  close(sfd);
  *out_words = words;
  *out_bytes = (size_t)st.st_size;
  return true;
}

static VkPipeline get_or_create_vk_pipeline(worker_t *w,
                                            vk_pipeline_t *p,
                                            uint32_t tx,
                                            uint32_t ty,
                                            uint32_t tz,
                                            VkPipeline *deferred,
                                            size_t *num_deferred) {
  for (uint32_t i = 0; i < p->num_specs; i++) {
    if (p->specs[i].tx == tx && p->specs[i].ty == ty && p->specs[i].tz == tz) {
      return p->specs[i].pipeline;
    }
  }
  if (p->num_specs >= KMTL_MAX_PSO_SPECS && *num_deferred >= KMTL_MAX_DEFERRED_PIPES) {
    return VK_NULL_HANDLE;
  }

  uint32_t n_entries = 3 + p->num_consts;
  VkSpecializationMapEntry *entries = calloc(n_entries, sizeof(*entries));
  size_t data_cap = 12 + (size_t)p->num_consts * 64;
  uint8_t *sdata = calloc(1, data_cap);
  if (!entries || !sdata) {
    free(entries);
    free(sdata);
    return VK_NULL_HANDLE;
  }

  uint32_t off = 0;
  uint32_t dims[3] = {tx, ty, tz};
  for (uint32_t d = 0; d < 3; d++) {
    entries[d].constantID = 65536u + d;
    entries[d].offset = off;
    entries[d].size = 4;
    memcpy(sdata + off, &dims[d], 4);
    off += 4;
  }
  for (uint32_t i = 0; i < p->num_consts; i++) {
    entries[3 + i].constantID = p->consts[i].index;
    entries[3 + i].offset = off;
    if (p->consts[i].data_type == 53u) {
      VkBool32 bv = p->consts[i].value[0] ? VK_TRUE : VK_FALSE;
      entries[3 + i].size = 4;
      memcpy(sdata + off, &bv, 4);
      off += 4;
    } else {
      entries[3 + i].size = p->consts[i].value_len;
      memcpy(sdata + off, p->consts[i].value, p->consts[i].value_len);
      off += p->consts[i].value_len;
    }
  }

  VkSpecializationInfo sinfo = {
      .mapEntryCount = n_entries,
      .pMapEntries = entries,
      .dataSize = off,
      .pData = sdata,
  };
  VkComputePipelineCreateInfo cpci = {
      .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
      .stage = {
          .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
          .stage = VK_SHADER_STAGE_COMPUTE_BIT,
          .module = p->shader,
          .pName = "main",
          .pSpecializationInfo = &sinfo,
      },
      .layout = p->pipe_layout,
  };
  VkPipeline pipe = VK_NULL_HANDLE;
  VkResult vr = vkCreateComputePipelines(w->device, VK_NULL_HANDLE, 1, &cpci, NULL, &pipe);
  free(entries);
  free(sdata);
  if (vr != VK_SUCCESS) return VK_NULL_HANDLE;

  if (p->num_specs < KMTL_MAX_PSO_SPECS) {
    p->specs[p->num_specs].tx = tx;
    p->specs[p->num_specs].ty = ty;
    p->specs[p->num_specs].tz = tz;
    p->specs[p->num_specs].pipeline = pipe;
    p->num_specs++;
  } else {
    /* Defer destroying evicted pipeline until after vkWaitForFences so in-flight cmd_buf never UAFs. */
    deferred[(*num_deferred)++] = p->specs[0].pipeline;
    memmove(&p->specs[0], &p->specs[1], (KMTL_MAX_PSO_SPECS - 1) * sizeof(vk_pso_spec_t));
    p->specs[KMTL_MAX_PSO_SPECS - 1].tx = tx;
    p->specs[KMTL_MAX_PSO_SPECS - 1].ty = ty;
    p->specs[KMTL_MAX_PSO_SPECS - 1].tz = tz;
    p->specs[KMTL_MAX_PSO_SPECS - 1].pipeline = pipe;
  }
  return pipe;
}

static void emit_full_barrier(VkCommandBuffer cb) {
  VkMemoryBarrier mb = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
      .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT | VK_ACCESS_TRANSFER_WRITE_BIT,
      .dstAccessMask = VK_ACCESS_SHADER_READ_BIT | VK_ACCESS_SHADER_WRITE_BIT |
                       VK_ACCESS_TRANSFER_READ_BIT | VK_ACCESS_TRANSFER_WRITE_BIT |
                       VK_ACCESS_INDIRECT_COMMAND_READ_BIT,
  };
  vkCmdPipelineBarrier(cb,
                       VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT | VK_PIPELINE_STAGE_TRANSFER_BIT,
                       VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT | VK_PIPELINE_STAGE_TRANSFER_BIT |
                           VK_PIPELINE_STAGE_DRAW_INDIRECT_BIT,
                       0, 1, &mb, 0, NULL, 0, NULL);
}

#if defined(__aarch64__)
static uint64_t arm64_dcache_line_size(void) {
  static uint64_t line = 0;
  if (line == 0) {
    uint64_t ctr = 0;
    __asm__ volatile("mrs %0, ctr_el0" : "=r"(ctr));
    uint64_t dmin = (ctr >> 16) & 0xfu;
    line = 4ULL << dmin;
    if (line < 16 || line > 256) line = 64;
  }
  return line;
}

static void arm64_clean_dcache_poc(const void *ptr, size_t len) {
  if (len == 0) return;
  uint64_t line = arm64_dcache_line_size();
  uintptr_t cur = (uintptr_t)ptr & ~(uintptr_t)(line - 1);
  uintptr_t end = (uintptr_t)ptr + len;
  for (; cur < end; cur += line) {
    __asm__ volatile("dc cvac, %0" : : "r"(cur) : "memory");
  }
  __asm__ volatile("dsb sy" : : : "memory");
}

static void arm64_clean_inv_dcache_poc(const void *ptr, size_t len) {
  if (len == 0) return;
  uint64_t line = arm64_dcache_line_size();
  uintptr_t cur = (uintptr_t)ptr & ~(uintptr_t)(line - 1);
  uintptr_t end = (uintptr_t)ptr + len;
  for (; cur < end; cur += line) {
    __asm__ volatile("dc civac, %0" : : "r"(cur) : "memory");
  }
  __asm__ volatile("dsb sy" : : : "memory");
}

typedef struct {
  flush_pool_t *pool;
  uint32_t idx;
} flush_helper_arg_t;

static void arm64_flush_lines(uintptr_t start_addr, size_t first_line, size_t end_line, uint64_t line, bool inv) {
  uintptr_t cur = start_addr + first_line * (uintptr_t)line;
  uintptr_t end = start_addr + end_line * (uintptr_t)line;
  if (inv) {
    for (; cur < end; cur += line) {
      __asm__ volatile("dc civac, %0" : : "r"(cur) : "memory");
    }
  } else {
    for (; cur < end; cur += line) {
      __asm__ volatile("dc cvac, %0" : : "r"(cur) : "memory");
    }
  }
  __asm__ volatile("dsb sy" : : : "memory");
}

static void *flush_helper_main(void *opaque) {
  flush_helper_arg_t *arg = (flush_helper_arg_t *)opaque;
  flush_pool_t *p = arg->pool;
  uint32_t my_idx = arg->idx;
  free(arg);

  uint64_t seen_gen = 0;
  for (;;) {
    pthread_mutex_lock(&p->mu);
    while (!p->stop && p->gen == seen_gen) {
      pthread_cond_wait(&p->cv_start, &p->mu);
    }
    if (p->stop) {
      pthread_mutex_unlock(&p->mu);
      return NULL;
    }
    seen_gen = p->gen;
    uintptr_t start_addr = p->start_addr;
    size_t total_lines = p->total_lines;
    uint64_t line_size = p->line_size;
    bool inv = p->invalidate;
    uint32_t chunks = p->n_threads + 1u;
    pthread_mutex_unlock(&p->mu);

    size_t l0 = (total_lines * (size_t)(my_idx + 1u)) / (size_t)chunks;
    size_t l1 = (total_lines * (size_t)(my_idx + 2u)) / (size_t)chunks;
    if (l1 > l0) {
      arm64_flush_lines(start_addr, l0, l1, line_size, inv);
    }

    pthread_mutex_lock(&p->mu);
    p->done++;
    if (p->done == p->n_threads) {
      pthread_cond_signal(&p->cv_done);
    }
    pthread_mutex_unlock(&p->mu);
  }
}

static void flush_pool_ensure_init(worker_t *w) {
  flush_pool_t *p = &w->flush_pool;
  if (p->initialized || p->init_failed) return;
  if (pthread_mutex_init(&p->mu, NULL) != 0) {
    p->init_failed = true;
    return;
  }
  if (pthread_cond_init(&p->cv_start, NULL) != 0) {
    pthread_mutex_destroy(&p->mu);
    p->init_failed = true;
    return;
  }
  if (pthread_cond_init(&p->cv_done, NULL) != 0) {
    pthread_cond_destroy(&p->cv_start);
    pthread_mutex_destroy(&p->mu);
    p->init_failed = true;
    return;
  }
  p->initialized = true;
  long ncpu = sysconf(_SC_NPROCESSORS_ONLN);
  uint32_t want = (ncpu >= 4) ? KMTL_FLUSH_HELPERS : (ncpu > 1 ? (uint32_t)(ncpu - 1) : 0u);
  for (uint32_t i = 0; i < want; i++) {
    flush_helper_arg_t *arg = malloc(sizeof(*arg));
    if (!arg) break;
    arg->pool = p;
    arg->idx = i;
    if (pthread_create(&p->threads[p->n_threads], NULL, flush_helper_main, arg) != 0) {
      free(arg);
      break;
    }
    p->n_threads++;
  }
}

static void flush_pool_destroy(worker_t *w) {
  flush_pool_t *p = &w->flush_pool;
  if (!p->initialized) return;
  if (p->n_threads > 0) {
    pthread_mutex_lock(&p->mu);
    p->stop = true;
    pthread_cond_broadcast(&p->cv_start);
    pthread_mutex_unlock(&p->mu);
    for (uint32_t i = 0; i < p->n_threads; i++) {
      pthread_join(p->threads[i], NULL);
    }
    p->n_threads = 0;
  }
  pthread_cond_destroy(&p->cv_done);
  pthread_cond_destroy(&p->cv_start);
  pthread_mutex_destroy(&p->mu);
  p->stop = false;
  p->initialized = false;
}

static void arm64_flush_poc_parallel(worker_t *w, const void *ptr, size_t len, bool invalidate) {
  if (len == 0) return;
  if (len < KMTL_FLUSH_PAR_MIN_BYTES) {
    if (invalidate) arm64_clean_inv_dcache_poc(ptr, len);
    else arm64_clean_dcache_poc(ptr, len);
    return;
  }
  flush_pool_ensure_init(w);
  flush_pool_t *p = &w->flush_pool;
  if (p->n_threads == 0) {
    if (invalidate) arm64_clean_inv_dcache_poc(ptr, len);
    else arm64_clean_dcache_poc(ptr, len);
    return;
  }
  uint64_t line = arm64_dcache_line_size();
  uintptr_t start = (uintptr_t)ptr & ~(uintptr_t)(line - 1);
  uintptr_t end = ((uintptr_t)ptr + len + (uintptr_t)(line - 1)) & ~(uintptr_t)(line - 1);
  if (end <= start) return;
  size_t total_lines = (size_t)((end - start) / line);
  uint32_t chunks = p->n_threads + 1u;

  pthread_mutex_lock(&p->mu);
  p->start_addr = start;
  p->total_lines = total_lines;
  p->line_size = line;
  p->invalidate = invalidate;
  p->done = 0;
  p->gen++;
  pthread_cond_broadcast(&p->cv_start);
  pthread_mutex_unlock(&p->mu);

  size_t l1 = total_lines / (size_t)chunks;
  if (l1 > 0) {
    arm64_flush_lines(start, 0, l1, line, invalidate);
  }

  pthread_mutex_lock(&p->mu);
  while (p->done < p->n_threads) {
    pthread_cond_wait(&p->cv_done, &p->mu);
  }
  pthread_mutex_unlock(&p->mu);
}
#endif

static size_t count_client_buffers(worker_t *w, uint32_t cid) {
  size_t n = 0;
  for (size_t i = 0; i < w->buffers_cap; i++)
    if (w->buffers[i].active && w->buffers[i].client_id == cid) n++;
  return n;
}

static size_t count_client_libraries(worker_t *w, uint32_t cid) {
  size_t n = 0;
  for (size_t i = 0; i < w->libraries_cap; i++)
    if (w->libraries[i].active && w->libraries[i].client_id == cid) n++;
  return n;
}

static size_t count_client_functions(worker_t *w, uint32_t cid) {
  size_t n = 0;
  for (size_t i = 0; i < w->functions_cap; i++)
    if (w->functions[i].active && w->functions[i].client_id == cid) n++;
  return n;
}

static size_t count_client_pipelines(worker_t *w, uint32_t cid) {
  size_t n = 0;
  for (size_t i = 0; i < w->pipelines_cap; i++)
    if (w->pipelines[i].active && w->pipelines[i].client_id == cid) n++;
  return n;
}

static size_t count_client_queues(worker_t *w, uint32_t cid) {
  size_t n = 0;
  for (size_t i = 0; i < w->queues_cap; i++)
    if (w->queues[i].active && w->queues[i].client_id == cid) n++;
  return n;
}

static bool validate_tg_dims(const worker_t *w, const vk_pipeline_t *pso, uint64_t tx, uint64_t ty, uint64_t tz) {
  if (tx == 0 || ty == 0 || tz == 0) return false;
  if (tx > w->props.limits.maxComputeWorkGroupSize[0] ||
      ty > w->props.limits.maxComputeWorkGroupSize[1] ||
      tz > w->props.limits.maxComputeWorkGroupSize[2]) {
    return false;
  }
  uint64_t max_inv = pso->max_tg ? pso->max_tg : w->props.limits.maxComputeWorkGroupInvocations;
  uint64_t xy = tx * ty;
  if (xy > max_inv || tz > max_inv / xy) return false;
  return true;
}

static bool retire_oldest_slot(worker_t *w, bool *out_had_deferred_flush) {
  if (w->slots_in_flight == 0) return true;
  vk_cmd_slot_t *s = &w->slots[w->slot_head];

  if (s->has_gpu_cmd) {
    VkResult vr = vkWaitForFences(w->device, 1, &s->fence, VK_TRUE, 30000000000ULL);
    if (vr != VK_SUCCESS) {
      /* Never reuse or destroy Vulkan objects while GPU execution may still be in flight. */
      return false;
    }
  }

  uint64_t now = monotonic_ns();
  uint64_t t0 = s->submit_ns;
  uint64_t t1 = now;
  if (s->has_gpu_cmd && s->had_timestamps && s->query_pool) {
    uint64_t ts[2] = {0, 0};
    VkResult qr = vkGetQueryPoolResults(w->device, s->query_pool, 0, 2, sizeof(ts), ts, sizeof(uint64_t),
                                        VK_QUERY_RESULT_64_BIT | VK_QUERY_RESULT_WAIT_BIT);
    if (qr == VK_SUCCESS) {
      uint64_t ticks = (ts[1] - ts[0]) & w->timestamp_mask;
      double dur_d = (double)ticks * w->timestamp_period_ns;
      uint64_t wall_ns = (now >= s->submit_ns) ? (now - s->submit_ns) : 0;
      if (dur_d >= 1.0 && dur_d <= (double)wall_ns + 1e9) {
        uint64_t dur_ns = (uint64_t)dur_d;
        t1 = now;
        t0 = (now >= dur_ns) ? (now - dur_ns) : 0;
      }
    }
  }

  for (size_t i = 0; i < s->n_deferred_pipes; i++) {
    vkDestroyPipeline(w->device, s->deferred_pipes[i], NULL);
  }
  s->n_deferred_pipes = 0;

  for (size_t i = 0; i < s->n_used_psos; i++) {
    size_t pidx = s->used_pso_idx[i];
    if (pidx < w->pipelines_cap) {
      vk_pipeline_t *p = &w->pipelines[pidx];
      if (p->inflight_refs > 0) p->inflight_refs--;
      if (p->inflight_refs == 0 && p->pending_destroy) {
        destroy_pipeline_slot_now(w, p);
      }
    }
  }
  s->n_used_psos = 0;

  /* Post-submit GPU->CPU coherency sync and deferred buffer teardown. */
  for (size_t i = 0; i < s->n_touched; i++) {
    size_t bidx = s->touched_idx[i];
    if (bidx >= w->buffers_cap) continue;
    vk_buffer_t *b = &w->buffers[bidx];
    bool written = s->touched_written[i];
    if (b->inflight_refs > 0) b->inflight_refs--;
    if (written && b->inflight_write_refs > 0) b->inflight_write_refs--;

    if (written && !b->pending_destroy) {
      if (b->inflight_write_refs > 0) {
        /* A later already-submitted command buffer is also writing this buffer; defer sync. */
        if (!b->flush_deferred) {
          b->flush_deferred = true;
          w->deferred_flush_count++;
        }
        if (out_had_deferred_flush) *out_had_deferred_flush = true;
      } else {
        if (b->flush_deferred) {
          b->flush_deferred = false;
          if (w->deferred_flush_count > 0) w->deferred_flush_count--;
        }
        if (b->is_dmabuf || b->is_host_ptr) {
          if (!w->dma_coherent) {
#if defined(__aarch64__)
            /* Defer GPU->CPU PoC invalidation until client sync point (WAIT_COMPLETED / WAIT_QUEUE). */
            if (!b->gpu_dirty) {
              b->gpu_dirty = true;
              w->gpu_dirty_count++;
            }
#endif
          }
        } else if (b->mapped && w->shm_base) {
          memcpy(w->shm_base + b->shm_offset, b->mapped, (size_t)b->aligned_len);
        }
      }
    }
#if !defined(__aarch64__)
    /* Keep DMA_BUF_IOCTL_SYNC balanced for both read-only and read-write dmabufs. */
    if (!b->pending_destroy && b->is_dmabuf && !w->dma_coherent && b->inflight_refs == 0) {
      dmabuf_sync(b->dmabuf_fd, DMA_BUF_SYNC_START | DMA_BUF_SYNC_RW);
    }
#endif

    if (b->inflight_refs == 0 && b->pending_destroy) {
      destroy_buffer_slot_now(w, b);
    }
  }
  s->n_touched = 0;

  vk_queue_t *q = find_queue(w, s->queue_handle, s->client_id);
  if (q) {
    if (s->queue_seq > q->completed_seq) q->completed_seq = s->queue_seq;
    q->last_start_ns = t0;
    q->last_end_ns = t1;
    q->timings[s->queue_seq & 63u].seq = s->queue_seq;
    q->timings[s->queue_seq & 63u].gpu_start_ns = t0;
    q->timings[s->queue_seq & 63u].gpu_end_ns = t1;
  }

  s->in_flight = false;
  s->has_gpu_cmd = false;
  s->had_timestamps = false;
  w->slot_head = (w->slot_head + 1u) % KMTL_MAX_INFLIGHT_CMDS;
  w->slots_in_flight--;
  if (w->slots_in_flight == 0) w->deferred_flush_count = 0;
  return true;
}

static bool poll_retire_completed(worker_t *w) {
  while (w->slots_in_flight > 0) {
    vk_cmd_slot_t *s = &w->slots[w->slot_head];
    if (!s->has_gpu_cmd || vkGetFenceStatus(w->device, s->fence) == VK_SUCCESS) {
      if (!retire_oldest_slot(w, NULL)) return false;
    } else {
      break;
    }
  }
  return true;
}

static bool drain_all_inflight(worker_t *w) {
  while (w->slots_in_flight > 0) {
    if (!retire_oldest_slot(w, NULL)) return false;
  }
  return true;
}

static bool drain_client_inflight(worker_t *w, uint32_t cid) {
  /* Retire FIFO slots up to the last in-flight slot belonging to cid. */
  uint32_t last_rel = 0;
  bool found = false;
  for (uint32_t k = 0; k < w->slots_in_flight; k++) {
    uint32_t idx = (w->slot_head + k) % KMTL_MAX_INFLIGHT_CMDS;
    if (w->slots[idx].client_id == cid) {
      last_rel = k + 1u;
      found = true;
    }
  }
  if (!found) {
    if (w->deferred_flush_count > 0 && w->slots_in_flight > 0) return drain_all_inflight(w);
    return true;
  }
  bool had_def = false;
  for (uint32_t k = 0; k < last_rel; k++) {
    if (!retire_oldest_slot(w, &had_def)) return false;
  }
  if ((had_def || w->deferred_flush_count > 0) && w->slots_in_flight > 0) return drain_all_inflight(w);
  return true;
}

static bool retire_up_to_queue_seq(worker_t *w, vk_queue_t *q, uint64_t target_seq) {
  bool had_deferred_flush = false;
  while (w->slots_in_flight > 0 && q->completed_seq < target_seq) {
    bool slot_def = false;
    if (!retire_oldest_slot(w, &slot_def)) return false;
    if (slot_def) had_deferred_flush = true;
  }
  if ((had_deferred_flush || w->deferred_flush_count > 0) && w->slots_in_flight > 0) {
    /* Ensure any buffer also written by a subsequent pipelined CB is fully flushed before return. */
    return drain_all_inflight(w);
  }
  return true;
}

static bool flush_gpu_dirty_buffers(worker_t *w, uint32_t cid) {
#if defined(__aarch64__)
  if (w->dma_coherent || w->gpu_dirty_count == 0 || !w->shm_base) return true;
  /* If any gpu_dirty buffer for cid also has a newer in-flight GPU write, drain first to avoid racing DMA. */
  bool need_drain = false;
  for (size_t i = 0; i < w->buffers_cap; i++) {
    vk_buffer_t *b = &w->buffers[i];
    if (b->active && b->gpu_dirty && b->inflight_write_refs > 0 && (cid == 0 || b->client_id == cid)) {
      need_drain = true;
      break;
    }
  }
  if (need_drain && !drain_all_inflight(w)) return false;
  for (size_t i = 0; i < w->buffers_cap; i++) {
    vk_buffer_t *b = &w->buffers[i];
    if (!b->active || !b->gpu_dirty) continue;
    if (cid != 0 && b->client_id != cid) continue;
    b->gpu_dirty = false;
    if (w->gpu_dirty_count > 0) w->gpu_dirty_count--;
    if (!b->pending_destroy && (b->is_dmabuf || b->is_host_ptr)) {
      arm64_flush_poc_parallel(w, w->shm_base + b->shm_offset, (size_t)b->aligned_len, true);
    }
  }
#else
  (void)w;
  (void)cid;
#endif
  return true;
}

static bool repack_active_set_bytes(worker_t *w,
                                    vk_cmd_slot_t *slot,
                                    const bool *bound_is_bytes,
                                    const uint8_t **bound_bytes_src,
                                    uint64_t *bound_bytes_off,
                                    const uint32_t *bound_bytes_len,
                                    uint64_t *staging_cursor) {
  *staging_cursor = 0;
  for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
    if (!bound_is_bytes[s]) continue;
    uint64_t off = align_up(*staging_cursor, w->staging_align);
    uint64_t end = off + bound_bytes_len[s];
    if (end > w->slot_staging_bytes) return false;
    memcpy(w->staging_ptr + slot->staging_base + off, bound_bytes_src[s], bound_bytes_len[s]);
    *staging_cursor = end;
    bound_bytes_off[s] = slot->staging_base + off;
  }
  return true;
}

static bool flush_commit_chunk(worker_t *w,
                               vk_cmd_slot_t *slot,
                               VkPipeline *deferred_pipes,
                               size_t *n_deferred_pipes,
                               bool *bound_is_bytes,
                               const uint8_t **bound_bytes_src,
                               uint64_t *bound_bytes_off,
                               const uint32_t *bound_bytes_len,
                               uint64_t *staging_cursor,
                               uint32_t *chunk_dispatches,
                               uint32_t *chunk_desc_sets,
                               uint64_t *chunk_workgroups,
                               bool *any_gpu_cmd,
                               bool *did_intermediate_flush,
                               uint64_t *first_submit_ns,
                               bool *use_ts,
                               bool *submitted_pending) {
  VkCommandBuffer cmd_buf = slot->cmd_buf;
  if (vkEndCommandBuffer(cmd_buf) != VK_SUCCESS) return false;
  if (!*did_intermediate_flush) *first_submit_ns = monotonic_ns();
  vkResetFences(w->device, 1, &slot->fence);
  VkSubmitInfo si = {
      .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
      .commandBufferCount = 1,
      .pCommandBuffers = &cmd_buf,
  };
  if (vkQueueSubmit(w->queue, 1, &si, slot->fence) != VK_SUCCESS) return false;
  *submitted_pending = true;
  if (vkWaitForFences(w->device, 1, &slot->fence, VK_TRUE, 30000000000ULL) != VK_SUCCESS) return false;
  *submitted_pending = false;

  /* Retire any earlier FIFO slots that completed ahead of this chunk before freeing deferred pipes. */
  if (w->slots_in_flight > 0 && !poll_retire_completed(w)) return false;

  if (w->slots_in_flight == 0 && *n_deferred_pipes > 0) {
    for (size_t i = 0; i < *n_deferred_pipes; i++) {
      vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
    }
    *n_deferred_pipes = 0;
  }

  vkResetDescriptorPool(w->device, slot->desc_pool, 0);
  vkResetCommandBuffer(cmd_buf, 0);
  VkCommandBufferBeginInfo cbbi = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
      .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
  };
  if (vkBeginCommandBuffer(cmd_buf, &cbbi) != VK_SUCCESS) return false;

  *chunk_dispatches = 0;
  *chunk_desc_sets = 0;
  *chunk_workgroups = 0;
  *any_gpu_cmd = false;
  *did_intermediate_flush = true;
  *use_ts = false;

  return repack_active_set_bytes(w, slot, bound_is_bytes, bound_bytes_src, bound_bytes_off,
                                 bound_bytes_len, staging_cursor);
}

static bool handle_commit(worker_t *w,
                          const struct kmtl_req_hdr *req,
                          const uint8_t *stream,
                          uint32_t stream_len,
                          uint32_t out_cap,
                          bool host_preflushed) {
  if (!has_client(w, req->client_id)) {
    return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
  }
  vk_queue_t *q = find_queue(w, req->handle, req->client_id);
  if (!q) {
    return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown queue handle", out_cap);
  }
  if ((req->arg0 & ~(KMTL_COMMIT_WAIT_COMPLETED | KMTL_COMMIT_WAIT_SCHEDULED)) != 0) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "unsupported commit flags", out_cap);
  }
  if (stream_len > KMTL_MAX_CMD_PAYLOAD) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "command stream exceeds limit", out_cap);
  }

  /* Opportunistically retire completed slots, and apply bounded backpressure when ring is full. */
  if (!poll_retire_completed(w)) {
    send_err(w->sock_fd, KMTL_ERR_GPU, "previous GPU command buffer failed or timed out", out_cap);
    return false;
  }
  if (w->slots_in_flight >= KMTL_MAX_INFLIGHT_CMDS) {
    if (!retire_oldest_slot(w, NULL)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "previous GPU command buffer failed or timed out", out_cap);
      return false;
    }
  }

  size_t touched_idx[KMTL_MAX_TOUCHED_BUFS];
  bool touched_written[KMTL_MAX_TOUCHED_BUFS];
  size_t n_touched = 0;
  size_t used_pso_idx[KMTL_MAX_USED_PSOS];
  size_t n_used_psos = 0;

  bool bad = false;
  const char *bad_reason = "invalid command stream";
  #define TOUCH_BUF_IDX(BIDX, WR)                                           \
    do {                                                                    \
      bool seen_ = false;                                                   \
      for (size_t ti_ = 0; ti_ < n_touched; ti_++) {                        \
        if (touched_idx[ti_] == (BIDX)) {                                   \
          if (WR) touched_written[ti_] = true;                              \
          seen_ = true;                                                     \
          break;                                                            \
        }                                                                   \
      }                                                                     \
      if (!seen_) {                                                         \
        if (n_touched >= KMTL_MAX_TOUCHED_BUFS) {                           \
          bad = true;                                                       \
          bad_reason = "too many distinct buffers in command buffer";       \
        } else {                                                            \
          touched_idx[n_touched] = (BIDX);                                  \
          touched_written[n_touched] = (WR);                                \
          n_touched++;                                                      \
        }                                                                   \
      }                                                                     \
    } while (0)

  #define TOUCH_PSO_IDX(PIDX)                                               \
    do {                                                                    \
      bool seen_ = false;                                                   \
      for (size_t pi_ = 0; pi_ < n_used_psos; pi_++) {                      \
        if (used_pso_idx[pi_] == (PIDX)) { seen_ = true; break; }           \
      }                                                                     \
      if (!seen_) {                                                         \
        if (n_used_psos >= KMTL_MAX_USED_PSOS) {                            \
          bad = true;                                                       \
          bad_reason = "too many distinct pipelines in command buffer";     \
        } else {                                                            \
          used_pso_idx[n_used_psos++] = (PIDX);                             \
        }                                                                   \
      }                                                                     \
    } while (0)

  /* Phase 1: Validate full stream and collect touched buffers/pipelines with zero GPU side effects. */
  {
    bool in_comp = false, in_blit = false;
    ptrdiff_t cur_pso_idx = -1;
    ptrdiff_t bound_buf_idx[KMTL_MAX_BUFFER_SLOTS];
    for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) bound_buf_idx[s] = -1;
    uint64_t bound_off[KMTL_MAX_BUFFER_SLOTS] = {0};
    bool bound_is_bytes[KMTL_MAX_BUFFER_SLOTS] = {false};
    uint32_t bound_bytes_len[KMTL_MAX_BUFFER_SLOTS] = {0};
    uint32_t tg_mem_lengths[KMTL_MAX_BUFFER_SLOTS] = {0};
    size_t pos = 0;

    while (pos < stream_len && !bad) {
      if (stream_len - pos < sizeof(struct kmtl_cmd_hdr)) { bad = true; break; }
      struct kmtl_cmd_hdr hdr;
      memcpy(&hdr, stream + pos, sizeof(hdr));
      if (hdr.size < sizeof(hdr) || (hdr.size & 7u) != 0 || hdr.size > stream_len - pos) {
        bad = true;
        break;
      }
      const uint8_t *rec = stream + pos;
      pos += hdr.size;

      switch (hdr.op) {
      case KMTL_CMD_BEGIN_COMPUTE:
        if (in_comp || in_blit) { bad = true; break; }
        in_comp = true;
        cur_pso_idx = -1;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) bound_buf_idx[s] = -1;
        memset(bound_off, 0, sizeof(bound_off));
        memset(bound_is_bytes, 0, sizeof(bound_is_bytes));
        memset(bound_bytes_len, 0, sizeof(bound_bytes_len));
        memset(tg_mem_lengths, 0, sizeof(tg_mem_lengths));
        break;

      case KMTL_CMD_BEGIN_BLIT:
        if (in_comp || in_blit) { bad = true; break; }
        in_blit = true;
        break;

      case KMTL_CMD_END_ENCODER:
        in_comp = false;
        in_blit = false;
        cur_pso_idx = -1;
        break;

      case KMTL_CMD_SET_PIPELINE: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_pipeline)) { bad = true; break; }
        struct kmtl_cmd_set_pipeline c;
        memcpy(&c, rec, sizeof(c));
        ptrdiff_t pidx = find_pipeline_idx(w, c.pso_handle, req->client_id);
        if (pidx < 0) { bad = true; bad_reason = "unknown pipeline handle"; break; }
        cur_pso_idx = pidx;
        break;
      }

      case KMTL_CMD_SET_BUFFER: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_buffer)) { bad = true; break; }
        struct kmtl_cmd_set_buffer c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS) { bad = true; break; }
        bound_is_bytes[c.index] = false;
        bound_bytes_len[c.index] = 0;
        if (c.buf_handle == 0) {
          bound_buf_idx[c.index] = -1;
          bound_off[c.index] = 0;
        } else {
          ptrdiff_t bidx = find_buffer_idx(w, c.buf_handle, req->client_id);
          if (bidx < 0 || c.offset >= w->buffers[bidx].aligned_len) {
            bad = true;
            bad_reason = "invalid buffer offset";
            break;
          }
          bound_buf_idx[c.index] = bidx;
          bound_off[c.index] = c.offset;
        }
        break;
      }

      case KMTL_CMD_SET_BUFFER_OFFSET: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_buffer_offset)) { bad = true; break; }
        struct kmtl_cmd_set_buffer_offset c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS || bound_buf_idx[c.index] < 0 ||
            c.offset >= w->buffers[bound_buf_idx[c.index]].aligned_len) {
          bad = true;
          bad_reason = "invalid buffer offset";
          break;
        }
        bound_off[c.index] = c.offset;
        break;
      }

      case KMTL_CMD_SET_BYTES: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_bytes)) { bad = true; break; }
        struct kmtl_cmd_set_bytes c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS || c.length == 0 || c.length > KMTL_MAX_SET_BYTES ||
            sizeof(c) + c.length > hdr.size) {
          bad = true;
          break;
        }
        bound_buf_idx[c.index] = -1;
        bound_is_bytes[c.index] = true;
        bound_bytes_len[c.index] = c.length;
        uint64_t active_stage = 0;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!bound_is_bytes[s]) continue;
          active_stage = align_up(active_stage, w->staging_align) + bound_bytes_len[s];
        }
        if (active_stage > w->slot_staging_bytes) {
          bad = true;
          bad_reason = "staging buffer exhausted for setBytes";
        }
        break;
      }

      case KMTL_CMD_SET_TG_MEM: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_tg_mem)) { bad = true; break; }
        struct kmtl_cmd_set_tg_mem c;
        memcpy(&c, rec, sizeof(c));
        if (c.index >= KMTL_MAX_BUFFER_SLOTS || (c.length & 15u) != 0 ||
            c.length > w->props.limits.maxComputeSharedMemorySize) {
          bad = true;
          break;
        }
        uint64_t total_tg = 0;
        for (uint32_t ti = 0; ti < KMTL_MAX_BUFFER_SLOTS; ti++) {
          total_tg += (ti == c.index) ? c.length : tg_mem_lengths[ti];
        }
        if (total_tg > w->props.limits.maxComputeSharedMemorySize) { bad = true; break; }
        tg_mem_lengths[c.index] = c.length;
        break;
      }

      case KMTL_CMD_DISPATCH_THREADGROUPS:
      case KMTL_CMD_DISPATCH_THREADS: {
        if (!in_comp || cur_pso_idx < 0 || hdr.size < sizeof(struct kmtl_cmd_dispatch)) { bad = true; break; }
        vk_pipeline_t *cur_pso = &w->pipelines[cur_pso_idx];
        struct kmtl_cmd_dispatch c;
        memcpy(&c, rec, sizeof(c));
        if (c.gx == 0 || c.gy == 0 || c.gz == 0 ||
            c.gx > UINT32_MAX || c.gy > UINT32_MAX || c.gz > UINT32_MAX ||
            !validate_tg_dims(w, cur_pso, c.tx, c.ty, c.tz)) {
          bad = true;
          bad_reason = "invalid dispatch dimensions";
          break;
        }
        uint64_t ngx = c.gx, ngy = c.gy, ngz = c.gz;
        if (hdr.op == KMTL_CMD_DISPATCH_THREADS) {
          ngx = (c.gx + c.tx - 1) / c.tx;
          ngy = (c.gy + c.ty - 1) / c.ty;
          ngz = (c.gz + c.tz - 1) / c.tz;
        }
        if (ngx > w->props.limits.maxComputeWorkGroupCount[0] ||
            ngy > w->props.limits.maxComputeWorkGroupCount[1] ||
            ngz > w->props.limits.maxComputeWorkGroupCount[2]) {
          bad = true;
          bad_reason = "dispatch workgroup count exceeds device limits";
          break;
        }
        TOUCH_PSO_IDX((size_t)cur_pso_idx);
        if (bad) break;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) continue;
          if (bound_buf_idx[s] >= 0) {
            if ((!cur_pso->is_binary || s >= 16u) && (bound_off[s] % w->staging_align) != 0) {
              bad = true;
              bad_reason = "unaligned buffer offset for non-AIR pipeline";
              break;
            }
            bool is_writable = (cur_pso->readonly_mask & (1u << s)) == 0;
            TOUCH_BUF_IDX((size_t)bound_buf_idx[s], is_writable);
            if (bad) break;
          } else {
            bad = true;
            bad_reason = "missing buffer binding for compute kernel";
            break;
          }
        }
        break;
      }

      case KMTL_CMD_DISPATCH_INDIRECT: {
        if (!in_comp || cur_pso_idx < 0 || hdr.size < sizeof(struct kmtl_cmd_dispatch_indirect)) { bad = true; break; }
        vk_pipeline_t *cur_pso = &w->pipelines[cur_pso_idx];
        struct kmtl_cmd_dispatch_indirect c;
        memcpy(&c, rec, sizeof(c));
        ptrdiff_t ibidx = find_buffer_idx(w, c.buf_handle, req->client_id);
        if (ibidx < 0 || (c.offset & 3ULL) != 0 || c.offset > w->buffers[ibidx].aligned_len ||
            w->buffers[ibidx].aligned_len - c.offset < 12 ||
            !validate_tg_dims(w, cur_pso, c.tx, c.ty, c.tz)) {
          bad = true;
          break;
        }
        TOUCH_BUF_IDX((size_t)ibidx, false);
        TOUCH_PSO_IDX((size_t)cur_pso_idx);
        if (bad) break;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) continue;
          if (bound_buf_idx[s] >= 0) {
            if ((!cur_pso->is_binary || s >= 16u) && (bound_off[s] % w->staging_align) != 0) {
              bad = true;
              bad_reason = "unaligned buffer offset for non-AIR pipeline";
              break;
            }
            bool is_writable = (cur_pso->readonly_mask & (1u << s)) == 0;
            TOUCH_BUF_IDX((size_t)bound_buf_idx[s], is_writable);
            if (bad) break;
          } else {
            bad = true;
            bad_reason = "missing buffer binding for compute kernel";
            break;
          }
        }
        break;
      }

      case KMTL_CMD_MEMORY_BARRIER: {
        if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_barrier)) { bad = true; break; }
        struct kmtl_cmd_barrier c;
        memcpy(&c, rec, sizeof(c));
        if (c.scope == 0 || (c.scope & ~3u) != 0) { bad = true; break; }
        break;
      }

      case KMTL_CMD_BLIT_COPY_BUFFER: {
        if (!in_blit || hdr.size < sizeof(struct kmtl_cmd_blit_copy)) { bad = true; break; }
        struct kmtl_cmd_blit_copy c;
        memcpy(&c, rec, sizeof(c));
        ptrdiff_t sidx = find_buffer_idx(w, c.src_handle, req->client_id);
        ptrdiff_t didx = find_buffer_idx(w, c.dst_handle, req->client_id);
        if (sidx < 0 || didx < 0 ||
            c.src_offset > w->buffers[sidx].aligned_len || c.dst_offset > w->buffers[didx].aligned_len ||
            c.size > w->buffers[sidx].aligned_len - c.src_offset ||
            c.size > w->buffers[didx].aligned_len - c.dst_offset) {
          bad = true;
          break;
        }
        if (c.size > 0) {
          TOUCH_BUF_IDX((size_t)sidx, false);
          TOUCH_BUF_IDX((size_t)didx, true);
        }
        break;
      }

      case KMTL_CMD_BLIT_FILL_BUFFER: {
        if (!in_blit || hdr.size < sizeof(struct kmtl_cmd_blit_fill)) { bad = true; break; }
        struct kmtl_cmd_blit_fill c;
        memcpy(&c, rec, sizeof(c));
        ptrdiff_t didx = find_buffer_idx(w, c.dst_handle, req->client_id);
        if (didx < 0 || c.offset > w->buffers[didx].aligned_len || c.size > w->buffers[didx].aligned_len - c.offset) {
          bad = true;
          break;
        }
        if (c.size > 0) {
          TOUCH_BUF_IDX((size_t)didx, true);
        }
        break;
      }

      default:
        bad = true;
        break;
      }
    }
  }
  #undef TOUCH_BUF_IDX
  #undef TOUCH_PSO_IDX

  if (bad) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, bad_reason, out_cap);
  }

  /* Phase 2: Pre-submit CPU->GPU coherency sync once over all touched buffers. */
  for (size_t i = 0; i < n_touched; i++) {
    vk_buffer_t *b = &w->buffers[touched_idx[i]];
    if (b->is_dmabuf || b->is_host_ptr) {
      if (!w->dma_coherent && !host_preflushed) {
#if defined(__aarch64__)
        if (b->inflight_write_refs > 0) {
          /* Do not race CPU cache clean/invalidate against an in-flight GPU DMA write. */
          if (!drain_all_inflight(w)) {
            send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
            return false;
          }
          b = &w->buffers[touched_idx[i]];
        }
        if (w->shm_base) {
          arm64_flush_poc_parallel(w, w->shm_base + b->shm_offset, (size_t)b->aligned_len, touched_written[i]);
          if (touched_written[i] && b->gpu_dirty) {
            b->gpu_dirty = false;
            if (w->gpu_dirty_count > 0) w->gpu_dirty_count--;
          }
        }
#else
        if (b->is_dmabuf) {
          if (b->inflight_refs > 0) {
            if (!drain_all_inflight(w)) {
              send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
              return false;
            }
            b = &w->buffers[touched_idx[i]];
          }
          dmabuf_sync(b->dmabuf_fd, DMA_BUF_SYNC_END | DMA_BUF_SYNC_RW);
        }
#endif
      }
    } else if (b->mapped && w->shm_base) {
      /* Staging memcpy must not race with an earlier in-flight GPU slot touching this buffer. */
      if (b->inflight_refs > 0) {
        if (!drain_all_inflight(w)) {
          send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
          return false;
        }
        /* Re-fetch pointer after drain (w->buffers does not realloc during drain). */
        b = &w->buffers[touched_idx[i]];
      }
      memcpy(b->mapped, w->shm_base + b->shm_offset, (size_t)b->aligned_len);
    }
  }

  /* Phase 3: Record Vulkan command buffer(s), transparently splitting oversized CBs into chunks. */
  vk_cmd_slot_t *slot = &w->slots[w->slot_tail];
  VkCommandBuffer cmd_buf = slot->cmd_buf;
  VkDescriptorPool desc_pool = slot->desc_pool;
  uint64_t staging_base = slot->staging_base;

  vkResetDescriptorPool(w->device, desc_pool, 0);
  vkResetCommandBuffer(cmd_buf, 0);
  VkCommandBufferBeginInfo cbbi = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
      .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
  };
  if (vkBeginCommandBuffer(cmd_buf, &cbbi) != VK_SUCCESS) {
    return send_err(w->sock_fd, KMTL_ERR_GPU, "vkBeginCommandBuffer failed", out_cap);
  }

  bool use_ts = w->has_timestamps && slot->query_pool != VK_NULL_HANDLE;
  if (use_ts) {
    vkCmdResetQueryPool(cmd_buf, slot->query_pool, 0, 2);
    vkCmdWriteTimestamp(cmd_buf, VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT, slot->query_pool, 0);
  }

  VkPipeline deferred_pipes[KMTL_MAX_DEFERRED_PIPES];
  size_t n_deferred_pipes = 0;

  ptrdiff_t cur_pso_idx = -1;
  bool in_comp_concurrent = false;
  ptrdiff_t bound_buf_idx[KMTL_MAX_BUFFER_SLOTS];
  for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) bound_buf_idx[s] = -1;
  uint64_t bound_off[KMTL_MAX_BUFFER_SLOTS] = {0};
  bool bound_is_bytes[KMTL_MAX_BUFFER_SLOTS] = {false};
  const uint8_t *bound_bytes_src[KMTL_MAX_BUFFER_SLOTS] = {NULL};
  uint64_t bound_bytes_off[KMTL_MAX_BUFFER_SLOTS] = {0};
  uint32_t bound_bytes_len[KMTL_MAX_BUFFER_SLOTS] = {0};
  uint64_t staging_cursor = 0;
  uint32_t chunk_dispatches = 0;
  uint32_t chunk_desc_sets = 0;
  uint64_t chunk_workgroups = 0;
  bool any_gpu_cmd = false;
  bool did_intermediate_flush = false;
  bool submitted_pending = false;
  uint64_t first_submit_ns = 0;
  bool gpu_fatal = false;

  size_t pos = 0;
  while (pos < stream_len && !bad) {
    struct kmtl_cmd_hdr hdr;
    memcpy(&hdr, stream + pos, sizeof(hdr));
    const uint8_t *rec = stream + pos;
    pos += hdr.size;

    switch (hdr.op) {
    case KMTL_CMD_BEGIN_COMPUTE:
      in_comp_concurrent = (hdr.flags == 1u);
      cur_pso_idx = -1;
      for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) bound_buf_idx[s] = -1;
      memset(bound_off, 0, sizeof(bound_off));
      memset(bound_is_bytes, 0, sizeof(bound_is_bytes));
      memset(bound_bytes_src, 0, sizeof(bound_bytes_src));
      memset(bound_bytes_len, 0, sizeof(bound_bytes_len));
      if (any_gpu_cmd || w->slots_in_flight > 0) emit_full_barrier(cmd_buf);
      break;

    case KMTL_CMD_BEGIN_BLIT:
      if (any_gpu_cmd || w->slots_in_flight > 0) emit_full_barrier(cmd_buf);
      break;

    case KMTL_CMD_END_ENCODER:
      if (in_comp_concurrent && any_gpu_cmd) emit_full_barrier(cmd_buf);
      in_comp_concurrent = false;
      cur_pso_idx = -1;
      memset(bound_is_bytes, 0, sizeof(bound_is_bytes));
      memset(bound_bytes_src, 0, sizeof(bound_bytes_src));
      memset(bound_bytes_len, 0, sizeof(bound_bytes_len));
      break;

    case KMTL_CMD_SET_PIPELINE: {
      struct kmtl_cmd_set_pipeline c;
      memcpy(&c, rec, sizeof(c));
      cur_pso_idx = find_pipeline_idx(w, c.pso_handle, req->client_id);
      break;
    }

    case KMTL_CMD_SET_BUFFER: {
      struct kmtl_cmd_set_buffer c;
      memcpy(&c, rec, sizeof(c));
      bound_is_bytes[c.index] = false;
      bound_bytes_src[c.index] = NULL;
      bound_bytes_len[c.index] = 0;
      if (c.buf_handle == 0) {
        bound_buf_idx[c.index] = -1;
        bound_off[c.index] = 0;
      } else {
        bound_buf_idx[c.index] = find_buffer_idx(w, c.buf_handle, req->client_id);
        bound_off[c.index] = c.offset;
      }
      break;
    }

    case KMTL_CMD_SET_BUFFER_OFFSET: {
      struct kmtl_cmd_set_buffer_offset c;
      memcpy(&c, rec, sizeof(c));
      bound_off[c.index] = c.offset;
      break;
    }

    case KMTL_CMD_SET_BYTES: {
      struct kmtl_cmd_set_bytes c;
      memcpy(&c, rec, sizeof(c));
      uint64_t off = align_up(staging_cursor, w->staging_align);
      if (off + c.length > w->slot_staging_bytes) {
        bound_is_bytes[c.index] = false;
        bound_bytes_src[c.index] = NULL;
        bound_bytes_len[c.index] = 0;
        if (!any_gpu_cmd) {
          if (!repack_active_set_bytes(w, slot, bound_is_bytes, bound_bytes_src, bound_bytes_off,
                                       bound_bytes_len, &staging_cursor)) {
            bad = true;
            gpu_fatal = did_intermediate_flush;
            bad_reason = "staging buffer exhausted for setBytes";
            break;
          }
        } else if (!flush_commit_chunk(w, slot, deferred_pipes, &n_deferred_pipes,
                                       bound_is_bytes, bound_bytes_src, bound_bytes_off, bound_bytes_len,
                                       &staging_cursor, &chunk_dispatches, &chunk_desc_sets, &chunk_workgroups,
                                       &any_gpu_cmd, &did_intermediate_flush, &first_submit_ns, &use_ts,
                                       &submitted_pending)) {
          bad = true;
          gpu_fatal = true;
          bad_reason = "intermediate command buffer chunk flush failed";
          break;
        }
        off = align_up(staging_cursor, w->staging_align);
        if (off + c.length > w->slot_staging_bytes) {
          bad = true;
          gpu_fatal = did_intermediate_flush;
          bad_reason = "staging buffer exhausted for setBytes";
          break;
        }
      }
      memcpy(w->staging_ptr + staging_base + off, rec + sizeof(c), c.length);
      staging_cursor = off + c.length;
      bound_buf_idx[c.index] = -1;
      bound_is_bytes[c.index] = true;
      bound_bytes_src[c.index] = rec + sizeof(c);
      bound_bytes_off[c.index] = staging_base + off;
      bound_bytes_len[c.index] = c.length;
      break;
    }

    case KMTL_CMD_SET_TG_MEM:
      break;

    case KMTL_CMD_DISPATCH_THREADGROUPS:
    case KMTL_CMD_DISPATCH_THREADS: {
      vk_pipeline_t *cur_pso = &w->pipelines[cur_pso_idx];
      struct kmtl_cmd_dispatch c;
      memcpy(&c, rec, sizeof(c));
      uint64_t ngx = c.gx, ngy = c.gy, ngz = c.gz;
      if (hdr.op == KMTL_CMD_DISPATCH_THREADS) {
        ngx = (c.gx + c.tx - 1) / c.tx;
        ngy = (c.gy + c.ty - 1) / c.ty;
        ngz = (c.gz + c.tz - 1) / c.tz;
      }
      uint64_t disp_wgs = ngx;
      if (ngy > 0 && disp_wgs <= UINT64_MAX / ngy) disp_wgs *= ngy;
      else disp_wgs = UINT64_MAX;
      if (ngz > 0 && disp_wgs <= UINT64_MAX / ngz) disp_wgs *= ngz;
      else disp_wgs = UINT64_MAX;
      if (any_gpu_cmd &&
          (chunk_dispatches >= w->max_chunk_dispatches ||
           chunk_desc_sets >= KMTL_CHUNK_MAX_DESC_SETS ||
           chunk_workgroups >= w->max_chunk_workgroups ||
           (disp_wgs <= w->max_chunk_workgroups && chunk_workgroups > w->max_chunk_workgroups - disp_wgs))) {
        if (!flush_commit_chunk(w, slot, deferred_pipes, &n_deferred_pipes,
                                bound_is_bytes, bound_bytes_src, bound_bytes_off, bound_bytes_len,
                                &staging_cursor, &chunk_dispatches, &chunk_desc_sets, &chunk_workgroups,
                                &any_gpu_cmd, &did_intermediate_flush, &first_submit_ns, &use_ts,
                                &submitted_pending)) {
          bad = true;
          gpu_fatal = true;
          bad_reason = "intermediate command buffer chunk flush failed";
          break;
        }
      }
      VkPipeline pipe = get_or_create_vk_pipeline(w, cur_pso, (uint32_t)c.tx, (uint32_t)c.ty, (uint32_t)c.tz,
                                                  deferred_pipes, &n_deferred_pipes);
      if (!pipe) {
        bad = true;
        gpu_fatal = did_intermediate_flush;
        bad_reason = "vkCreateComputePipelines failed";
        break;
      }

      if (cur_pso->binding_mask != 0) {
        VkDescriptorSetAllocateInfo dsai = {
            .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
            .descriptorPool = desc_pool,
            .descriptorSetCount = 1,
            .pSetLayouts = &cur_pso->ds_layout,
        };
        VkDescriptorSet dset = VK_NULL_HANDLE;
        if (vkAllocateDescriptorSets(w->device, &dsai, &dset) != VK_SUCCESS) {
          bad = true;
          gpu_fatal = did_intermediate_flush;
          bad_reason = "vkAllocateDescriptorSets failed";
          break;
        }
        chunk_desc_sets++;
        VkDescriptorBufferInfo dbis[KMTL_MAX_BUFFER_SLOTS];
        VkWriteDescriptorSet writes[KMTL_MAX_BUFFER_SLOTS];
        uint32_t nw = 0;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) {
            dbis[nw].buffer = w->staging_buf;
            dbis[nw].offset = bound_bytes_off[s];
            dbis[nw].range = bound_bytes_len[s];
          } else {
            uint64_t desc_off = bound_off[s];
            if (cur_pso->is_binary && s < 16u && w->staging_align > 1u) {
              desc_off = bound_off[s] - (bound_off[s] % w->staging_align);
            }
            dbis[nw].buffer = w->buffers[bound_buf_idx[s]].buffer;
            dbis[nw].offset = desc_off;
            dbis[nw].range = VK_WHOLE_SIZE;
          }
          writes[nw] = (VkWriteDescriptorSet){
              .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
              .dstSet = dset,
              .dstBinding = s,
              .descriptorCount = 1,
              .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
              .pBufferInfo = &dbis[nw],
          };
          nw++;
        }
        vkUpdateDescriptorSets(w->device, nw, writes, 0, NULL);
        vkCmdBindDescriptorSets(cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, cur_pso->pipe_layout, 0, 1, &dset, 0, NULL);
      }

      {
        uint32_t ptr_pc[32] = {0};
        for (uint32_t s = 0; s < 16u; s++) {
          if (bound_is_bytes[s]) {
            ptr_pc[s] = 0x80000000u | s;
            ptr_pc[16u + s] = 0u;
          } else if (bound_buf_idx[s] >= 0) {
            ptr_pc[s] = (uint32_t)w->buffers[bound_buf_idx[s]].handle;
            ptr_pc[16u + s] = (uint32_t)bound_off[s];
          }
        }
        vkCmdPushConstants(cmd_buf, cur_pso->pipe_layout, VK_SHADER_STAGE_COMPUTE_BIT, 0, sizeof(ptr_pc), ptr_pc);
      }
      vkCmdBindPipeline(cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, pipe);
      vkCmdDispatch(cmd_buf, (uint32_t)ngx, (uint32_t)ngy, (uint32_t)ngz);
      if (!in_comp_concurrent) emit_full_barrier(cmd_buf);
      chunk_dispatches++;
      if (disp_wgs <= UINT64_MAX - chunk_workgroups) chunk_workgroups += disp_wgs;
      else chunk_workgroups = UINT64_MAX;
      any_gpu_cmd = true;
      break;
    }

    case KMTL_CMD_DISPATCH_INDIRECT: {
      if (any_gpu_cmd &&
          (chunk_dispatches >= w->max_chunk_dispatches ||
           chunk_desc_sets >= KMTL_CHUNK_MAX_DESC_SETS ||
           chunk_workgroups >= w->max_chunk_workgroups)) {
        if (!flush_commit_chunk(w, slot, deferred_pipes, &n_deferred_pipes,
                                bound_is_bytes, bound_bytes_src, bound_bytes_off, bound_bytes_len,
                                &staging_cursor, &chunk_dispatches, &chunk_desc_sets, &chunk_workgroups,
                                &any_gpu_cmd, &did_intermediate_flush, &first_submit_ns, &use_ts,
                                &submitted_pending)) {
          bad = true;
          gpu_fatal = true;
          bad_reason = "intermediate command buffer chunk flush failed";
          break;
        }
      }
      vk_pipeline_t *cur_pso = &w->pipelines[cur_pso_idx];
      struct kmtl_cmd_dispatch_indirect c;
      memcpy(&c, rec, sizeof(c));
      ptrdiff_t ibidx = find_buffer_idx(w, c.buf_handle, req->client_id);
      VkPipeline pipe = get_or_create_vk_pipeline(w, cur_pso, (uint32_t)c.tx, (uint32_t)c.ty, (uint32_t)c.tz,
                                                  deferred_pipes, &n_deferred_pipes);
      if (!pipe) {
        bad = true;
        gpu_fatal = did_intermediate_flush;
        bad_reason = "vkCreateComputePipelines failed";
        break;
      }
      if (cur_pso->binding_mask != 0) {
        VkDescriptorSetAllocateInfo dsai = {
            .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
            .descriptorPool = desc_pool,
            .descriptorSetCount = 1,
            .pSetLayouts = &cur_pso->ds_layout,
        };
        VkDescriptorSet dset = VK_NULL_HANDLE;
        if (vkAllocateDescriptorSets(w->device, &dsai, &dset) != VK_SUCCESS) {
          bad = true;
          gpu_fatal = did_intermediate_flush;
          bad_reason = "vkAllocateDescriptorSets failed";
          break;
        }
        chunk_desc_sets++;
        VkDescriptorBufferInfo dbis[KMTL_MAX_BUFFER_SLOTS];
        VkWriteDescriptorSet writes[KMTL_MAX_BUFFER_SLOTS];
        uint32_t nw = 0;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) {
            dbis[nw] = (VkDescriptorBufferInfo){w->staging_buf, bound_bytes_off[s], bound_bytes_len[s]};
          } else {
            uint64_t desc_off = bound_off[s];
            if (cur_pso->is_binary && s < 16u && w->staging_align > 1u) {
              desc_off = bound_off[s] - (bound_off[s] % w->staging_align);
            }
            dbis[nw] = (VkDescriptorBufferInfo){w->buffers[bound_buf_idx[s]].buffer, desc_off, VK_WHOLE_SIZE};
          }
          writes[nw] = (VkWriteDescriptorSet){
              .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
              .dstSet = dset,
              .dstBinding = s,
              .descriptorCount = 1,
              .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
              .pBufferInfo = &dbis[nw],
          };
          nw++;
        }
        vkUpdateDescriptorSets(w->device, nw, writes, 0, NULL);
        vkCmdBindDescriptorSets(cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, cur_pso->pipe_layout, 0, 1, &dset, 0, NULL);
      }
      {
        uint32_t ptr_pc[32] = {0};
        for (uint32_t s = 0; s < 16u; s++) {
          if (bound_is_bytes[s]) {
            ptr_pc[s] = 0x80000000u | s;
            ptr_pc[16u + s] = 0u;
          } else if (bound_buf_idx[s] >= 0) {
            ptr_pc[s] = (uint32_t)w->buffers[bound_buf_idx[s]].handle;
            ptr_pc[16u + s] = (uint32_t)bound_off[s];
          }
        }
        vkCmdPushConstants(cmd_buf, cur_pso->pipe_layout, VK_SHADER_STAGE_COMPUTE_BIT, 0, sizeof(ptr_pc), ptr_pc);
      }
      vkCmdBindPipeline(cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, pipe);
      vkCmdDispatchIndirect(cmd_buf, w->buffers[ibidx].buffer, c.offset);
      if (!in_comp_concurrent) emit_full_barrier(cmd_buf);
      chunk_dispatches++;
      if (KMTL_INDIRECT_WG_ESTIMATE <= UINT64_MAX - chunk_workgroups) {
        chunk_workgroups += KMTL_INDIRECT_WG_ESTIMATE;
      } else {
        chunk_workgroups = UINT64_MAX;
      }
      any_gpu_cmd = true;
      break;
    }

    case KMTL_CMD_MEMORY_BARRIER:
      emit_full_barrier(cmd_buf);
      break;

    case KMTL_CMD_BLIT_COPY_BUFFER: {
      struct kmtl_cmd_blit_copy c;
      memcpy(&c, rec, sizeof(c));
      if (c.size > 0) {
        ptrdiff_t sidx = find_buffer_idx(w, c.src_handle, req->client_id);
        ptrdiff_t didx = find_buffer_idx(w, c.dst_handle, req->client_id);
        VkBufferCopy region = {.srcOffset = c.src_offset, .dstOffset = c.dst_offset, .size = c.size};
        vkCmdCopyBuffer(cmd_buf, w->buffers[sidx].buffer, w->buffers[didx].buffer, 1, &region);
        emit_full_barrier(cmd_buf);
        any_gpu_cmd = true;
      }
      break;
    }

    case KMTL_CMD_BLIT_FILL_BUFFER: {
      struct kmtl_cmd_blit_fill c;
      memcpy(&c, rec, sizeof(c));
      if (c.size > 0) {
        ptrdiff_t didx = find_buffer_idx(w, c.dst_handle, req->client_id);
        if ((c.offset & 3ULL) == 0 && (c.size & 3ULL) == 0) {
          uint32_t v32 = (uint32_t)c.value * 0x01010101u;
          vkCmdFillBuffer(cmd_buf, w->buffers[didx].buffer, c.offset, c.size, v32);
          emit_full_barrier(cmd_buf);
          any_gpu_cmd = true;
        } else {
          uint64_t remaining = c.size;
          uint64_t dst_off = c.offset;
          while (remaining > 0) {
            uint64_t off = align_up(staging_cursor, 4);
            if (off >= w->slot_staging_bytes) {
              if (!any_gpu_cmd) {
                staging_cursor = 0;
              } else if (!flush_commit_chunk(w, slot, deferred_pipes, &n_deferred_pipes,
                                             bound_is_bytes, bound_bytes_src, bound_bytes_off, bound_bytes_len,
                                             &staging_cursor, &chunk_dispatches, &chunk_desc_sets, &chunk_workgroups,
                                             &any_gpu_cmd, &did_intermediate_flush, &first_submit_ns, &use_ts,
                                             &submitted_pending)) {
                bad = true;
                gpu_fatal = true;
                bad_reason = "intermediate command buffer chunk flush failed";
                break;
              }
              off = align_up(staging_cursor, 4);
              if (off >= w->slot_staging_bytes) {
                bad = true;
                gpu_fatal = did_intermediate_flush;
                bad_reason = "staging full for unaligned fill";
                break;
              }
            }
            uint64_t chunk = w->slot_staging_bytes - off;
            if (chunk > remaining) chunk = remaining;
            memset(w->staging_ptr + staging_base + off, c.value, (size_t)chunk);
            VkBufferCopy r = {.srcOffset = staging_base + off, .dstOffset = dst_off, .size = chunk};
            vkCmdCopyBuffer(cmd_buf, w->staging_buf, w->buffers[didx].buffer, 1, &r);
            staging_cursor = off + chunk;
            dst_off += chunk;
            remaining -= chunk;
            any_gpu_cmd = true;
          }
          if (!bad) emit_full_barrier(cmd_buf);
        }
      }
      break;
    }

    default:
      break;
    }
  }

  if (use_ts && any_gpu_cmd && !bad) {
    vkCmdWriteTimestamp(cmd_buf, VK_PIPELINE_STAGE_BOTTOM_OF_PIPE_BIT, slot->query_pool, 1);
  }
  if (!bad && vkEndCommandBuffer(cmd_buf) != VK_SUCCESS) {
    bad = true;
    gpu_fatal = did_intermediate_flush;
    bad_reason = "vkEndCommandBuffer failed";
  }
  if (bad) {
    if (submitted_pending) {
      /* Never reset a pending VkCommandBuffer or destroy pipelines while GPU execution may be active. */
      send_err(w->sock_fd, KMTL_ERR_GPU, bad_reason, out_cap);
      return false;
    }
    vkResetCommandBuffer(cmd_buf, 0);
    if (n_deferred_pipes > 0 && w->slots_in_flight > 0) {
      if (!drain_all_inflight(w)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
    }
    for (size_t i = 0; i < n_deferred_pipes; i++) {
      vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
    }
    if (gpu_fatal) {
      send_err(w->sock_fd, KMTL_ERR_GPU, bad_reason, out_cap);
      return false;
    }
    return send_err(w->sock_fd, KMTL_ERR_INVAL, bad_reason, out_cap);
  }

  uint64_t submit_ns = did_intermediate_flush ? first_submit_ns : monotonic_ns();
  if (any_gpu_cmd) {
    vkResetFences(w->device, 1, &slot->fence);
    VkSubmitInfo si = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1,
        .pCommandBuffers = &cmd_buf,
    };
    VkResult vr = vkQueueSubmit(w->queue, 1, &si, slot->fence);
    if (vr != VK_SUCCESS) {
      bool safe_destroy = (n_deferred_pipes == 0 || w->slots_in_flight == 0 || drain_all_inflight(w));
      if (safe_destroy) {
        for (size_t i = 0; i < n_deferred_pipes; i++) {
          vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
        }
      }
      send_err(w->sock_fd, KMTL_ERR_GPU, "vkQueueSubmit failed", out_cap);
      return false;
    }
  }

  uint64_t seq = ++q->submitted_seq;
  if (!any_gpu_cmd && !did_intermediate_flush && w->slots_in_flight == 0) {
    for (size_t i = 0; i < n_deferred_pipes; i++) {
      vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
    }
    q->completed_seq = seq;
    q->last_start_ns = submit_ns;
    q->last_end_ns = submit_ns;
    q->timings[seq & 63u] = (vk_seq_timing_t){seq, submit_ns, submit_ns};
  } else {
    /* Pin all referenced buffers & pipelines until slot retires. */
    slot->in_flight = true;
    slot->has_gpu_cmd = any_gpu_cmd;
    slot->had_timestamps = use_ts && any_gpu_cmd && !did_intermediate_flush;
    slot->queue_handle = q->handle;
    slot->client_id = req->client_id;
    slot->queue_seq = seq;
    slot->submit_ns = submit_ns;
    slot->n_touched = n_touched;
    for (size_t i = 0; i < n_touched; i++) {
      slot->touched_idx[i] = touched_idx[i];
      slot->touched_written[i] = touched_written[i];
      w->buffers[touched_idx[i]].inflight_refs++;
      if (touched_written[i]) w->buffers[touched_idx[i]].inflight_write_refs++;
    }
    slot->n_used_psos = n_used_psos;
    for (size_t i = 0; i < n_used_psos; i++) {
      slot->used_pso_idx[i] = used_pso_idx[i];
      w->pipelines[used_pso_idx[i]].inflight_refs++;
    }
    slot->n_deferred_pipes = n_deferred_pipes;
    for (size_t i = 0; i < n_deferred_pipes; i++) {
      slot->deferred_pipes[i] = deferred_pipes[i];
    }
    w->slot_tail = (w->slot_tail + 1u) % KMTL_MAX_INFLIGHT_CMDS;
    w->slots_in_flight++;
  }

  if (req->arg0 & KMTL_COMMIT_WAIT_COMPLETED) {
    if (!retire_up_to_queue_seq(w, q, seq) || !flush_gpu_dirty_buffers(w, req->client_id)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "vkWaitForFences failed or timed out", out_cap);
      return false;
    }
    vk_seq_timing_t t = q->timings[seq & 63u];
    struct kmtl_resp_hdr resp;
    memset(&resp, 0, sizeof(resp));
    resp.status = KMTL_OK;
    resp.arg0 = (t.seq == seq) ? t.gpu_start_ns : q->last_start_ns;
    resp.arg1 = (t.seq == seq) ? t.gpu_end_ns : q->last_end_ns;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  /* Pipelined async commit: return sequence immediately while GPU executes in parallel! */
  struct kmtl_resp_hdr resp;
  memset(&resp, 0, sizeof(resp));
  resp.status = KMTL_OK;
  resp.arg0 = seq;
  return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
}

static bool handle_rpc(worker_t *w,
                       uint8_t frame_kind,
                       const struct kmtl_req_hdr *req,
                       const uint8_t *in_payload,
                       uint32_t in_len,
                       uint32_t out_cap) {
  if (frame_kind == KMTL_FRAME_VMM_RESET) {
    if (!reset_all(w)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "GPU reset failed", out_cap);
      return false;
    }
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }
  if (frame_kind != KMTL_FRAME_GUEST_RPC && frame_kind != KMTL_FRAME_GUEST_RPC_PREFLUSHED) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "unknown frame kind", out_cap);
  }
  if (req->magic != KMTL_MAGIC || req->version != KMTL_PROTO_VERSION) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "bad magic or protocol version", out_cap);
  }
  if (frame_kind == KMTL_FRAME_GUEST_RPC_PREFLUSHED && req->opcode != KMTL_OP_COMMIT_COMMAND_BUFFER) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "preflushed frame only valid for commit", out_cap);
  }

  switch (req->opcode) {
  case KMTL_OP_GET_DEVICE_INFO: {
    struct kmtl_device_info info;
    memset(&info, 0, sizeof(info));
    snprintf(info.name, sizeof(info.name), "%.116s (Vulkan)", w->props.deviceName);
    info.registry_id = ((uint64_t)w->props.vendorID << 32) | (uint64_t)w->props.deviceID;
    uint64_t max_buf = w->props.limits.maxStorageBufferRange;
    if (max_buf == 0 || max_buf > w->shm_size) max_buf = w->shm_size;
    info.max_buffer_length = max_buf;
    info.recommended_max_working_set_size = w->shm_size;
    info.shm_aperture_bytes = w->shm_size;
    uint32_t sg_req = (1u << 0) | (1u << 2) | (1u << 4); /* basic + arithmetic + shuffle */
    info.gpu_family_mask = KMTL_FAM_COMMON1 | KMTL_FAM_COMMON2 | KMTL_FAM_COMMON3;
    if ((w->subgroup_ops & sg_req) == sg_req) {
      info.gpu_family_mask |= KMTL_FAM_METAL3;
    }
    info.max_threads_per_threadgroup[0] = w->props.limits.maxComputeWorkGroupSize[0];
    info.max_threads_per_threadgroup[1] = w->props.limits.maxComputeWorkGroupSize[1];
    info.max_threads_per_threadgroup[2] = w->props.limits.maxComputeWorkGroupSize[2];
    info.max_threadgroup_memory_length = w->props.limits.maxComputeSharedMemorySize;
    info.host_page_size = w->host_page_size;
    info.flags = KMTL_FEAT_UNIFIED_MEMORY
#ifdef KIDO_HAVE_AIR2SPV
                 | KMTL_FEAT_NATIVE_METALLIB
#endif
                 | (w->has_bf16 ? KMTL_FEAT_BFLOAT16 : 0u)
                 | (w->vmm_dma_coherent ? KMTL_FEAT_DMA_COHERENT : 0u);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, &info, (uint32_t)sizeof(info), out_cap);
  }

  case KMTL_OP_CLIENT_OPEN: {
    if (req->client_id == 0) return send_err(w->sock_fd, KMTL_ERR_INVAL, "client_id must be non-zero", out_cap);
    if (has_client(w, req->client_id)) {
      if (!close_client(w, req->client_id, false)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
    }
    for (size_t i = 0; i < KMTL_MAX_CLIENTS; i++) {
      if (!w->clients[i].active) {
        w->clients[i].active = true;
        w->clients[i].client_id = req->client_id;
        struct kmtl_resp_hdr resp = {0};
        return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
      }
    }
    return send_err(w->sock_fd, KMTL_ERR_NOMEM, "client limit reached", out_cap);
  }

  case KMTL_OP_CLIENT_CLOSE: {
    if (!close_client(w, req->client_id, false)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
      return false;
    }
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_BUFFER: {
    if (!has_client(w, req->client_id)) return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
    if (!poll_retire_completed(w)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
      return false;
    }
    if (count_client_buffers(w, req->client_id) >= KMTL_MAX_BUFFERS_PER_CLIENT && w->slots_in_flight > 0) {
      if (!drain_all_inflight(w)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
    }
    if (count_client_buffers(w, req->client_id) >= KMTL_MAX_BUFFERS_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "buffer handle limit reached", out_cap);
    }
    if (w->next_handle > 0xFFFFFFFFULL) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "buffer handle space exhausted", out_cap);
    }
    uint64_t req_len = req->arg0 ? req->arg0 : 1;
    uint64_t aligned_len = align_up(req_len, w->host_page_size);
    if (aligned_len == 0 || aligned_len > w->shm_size || aligned_len > w->props.limits.maxStorageBufferRange) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "buffer exceeds GPU memory limit", out_cap);
    }
    uint64_t buf_align = w->host_page_size;
    if (w->buf_tier == KMTL_BUF_TIER_HOST_PTR) {
      uint64_t hp_a = host_ptr_eff_align(w);
      if (hp_a > buf_align) buf_align = hp_a;
    }
    uint64_t shm_len = align_up(aligned_len, buf_align);
    if (shm_len == 0 || shm_len > w->shm_size) shm_len = aligned_len;
    bool can_zero_copy = (w->buf_tier != KMTL_BUF_TIER_STAGING);
    if (w->buf_extra_pad > 0 && can_zero_copy && w->buf_extra_pad < w->shm_size &&
        aligned_len <= w->shm_size - w->buf_extra_pad) {
      uint64_t padded = align_up(aligned_len + w->buf_extra_pad, buf_align);
      if (padded != 0 && padded <= w->shm_size) {
        shm_len = padded;
      }
    }
    if (w->used_bytes > w->shm_size - shm_len && w->slots_in_flight > 0) {
      if (!drain_all_inflight(w)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
    }
    if (w->used_bytes > w->shm_size - shm_len) {
      if (shm_len > aligned_len && w->used_bytes <= w->shm_size - aligned_len) {
        shm_len = aligned_len;
      } else {
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "GPU memory quota exhausted", out_cap);
      }
    }
    uint64_t shm_off = 0;
    bool alloc_ok = kmtl_shm_alloc(&w->shm_alloc, shm_len, &shm_off);
    if (!alloc_ok && w->slots_in_flight > 0) {
      /* Reclaim SHM pinned by pending-destroy buffers from in-flight command buffers. */
      if (!drain_all_inflight(w)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
      alloc_ok = kmtl_shm_alloc(&w->shm_alloc, shm_len, &shm_off);
    }
    if (!alloc_ok) {
      if (shm_len > aligned_len && w->used_bytes <= w->shm_size - aligned_len &&
          kmtl_shm_alloc(&w->shm_alloc, aligned_len, &shm_off)) {
        shm_len = aligned_len;
      } else {
        return send_err(w->sock_fd, KMTL_ERR_NOSPC, "GPU SHM aperture full or fragmented", out_cap);
      }
    }

    if (w->shm_base) memset(w->shm_base + shm_off, 0, (size_t)shm_len);

    VkBuffer buf = VK_NULL_HANDLE;
    VkDeviceMemory mem = VK_NULL_HANDLE;
    void *mapped = NULL;
    int dmabuf_fd = -1;
    /* Strictly honor the probed w->buf_tier; never upgrade to an unprobed zero-copy path. */
    bool is_dmabuf = false;
    bool is_host_ptr = false;
    if (w->buf_tier == KMTL_BUF_TIER_DMABUF) {
      is_dmabuf = try_alloc_dmabuf_vk_buffer(w, shm_off, aligned_len, shm_len, &buf, &mem, &dmabuf_fd);
    } else if (w->buf_tier == KMTL_BUF_TIER_HOST_PTR) {
      is_host_ptr = try_alloc_host_ptr_vk_buffer(w, shm_off, aligned_len, shm_len, &buf, &mem);
    }
    if (!is_dmabuf && !is_host_ptr) {
      if (!alloc_host_visible_vk_buffer(w, aligned_len, &buf, &mem, &mapped)) {
        kmtl_shm_free(&w->shm_alloc, shm_off, shm_len);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "Vulkan buffer allocation failed", out_cap);
      }
    }
#if defined(__aarch64__)
    else if (!w->dma_coherent && w->shm_base) {
      /* Host memset(0) does not mark KVM Stage-2 dirty pages; publish zeroed pages to PoC once. */
      arm64_flush_poc_parallel(w, w->shm_base + shm_off, (size_t)shm_len, true);
    }
#endif
    if (!w->logged_buf_mode) {
      w->logged_buf_mode = true;
      fprintf(stderr, "kido-vk-worker: buffer mode = %s (dma_coherent=%d)\n",
              is_dmabuf      ? "ZERO-COPY udmabuf + VK_EXT_external_memory_dma_buf"
              : is_host_ptr  ? "ZERO-COPY VK_EXT_external_memory_host"
                             : "HOST_VISIBLE | HOST_COHERENT sync",
              w->dma_coherent ? 1 : 0);
    }

    size_t idx = w->buffers_cap;
    for (size_t i = 0; i < w->buffers_cap; i++) {
      if (!w->buffers[i].active) { idx = i; break; }
    }
    if (idx == w->buffers_cap) {
      size_t ncap = w->buffers_cap ? w->buffers_cap * 2 : 64;
      vk_buffer_t *nb = realloc(w->buffers, ncap * sizeof(*nb));
      if (!nb) {
        if (mapped) vkUnmapMemory(w->device, mem);
        vkDestroyBuffer(w->device, buf, NULL);
        vkFreeMemory(w->device, mem, NULL);
        if (dmabuf_fd >= 0) close(dmabuf_fd);
        kmtl_shm_free(&w->shm_alloc, shm_off, shm_len);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      memset(nb + w->buffers_cap, 0, (ncap - w->buffers_cap) * sizeof(*nb));
      for (size_t k = w->buffers_cap; k < ncap; k++) nb[k].dmabuf_fd = -1;
      w->buffers = nb;
      w->buffers_cap = ncap;
    }

    uint64_t handle = w->next_handle++;
    vk_buffer_t *slot = &w->buffers[idx];
    slot->active = true;
    slot->pending_destroy = false;
    slot->handle = handle;
    slot->client_id = req->client_id;
    slot->shm_offset = shm_off;
    slot->shm_len = shm_len;
    slot->aligned_len = aligned_len;
    slot->requested_len = req_len;
    slot->is_dmabuf = is_dmabuf;
    slot->is_host_ptr = is_host_ptr;
    slot->dmabuf_fd = dmabuf_fd;
    slot->inflight_refs = 0;
    slot->inflight_write_refs = 0;
    slot->buffer = buf;
    slot->memory = mem;
    slot->mapped = mapped;
    w->used_bytes += shm_len;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    resp.arg0 = shm_off;
    resp.arg1 = aligned_len;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_DESTROY_BUFFER: {
    vk_buffer_t *b = find_buffer(w, req->handle, req->client_id);
    if (!b) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown buffer handle", out_cap);
    destroy_buffer_slot(w, b);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_LIBRARY_SOURCE: {
    if (!has_client(w, req->client_id)) return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
    if (count_client_libraries(w, req->client_id) >= KMTL_MAX_LIBS_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "library limit reached", out_cap);
    }
    if (in_len < sizeof(struct kmtl_compile_opts)) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "truncated compile options", out_cap);
    }
    struct kmtl_compile_opts co;
    memcpy(&co, in_payload, sizeof(co));
    size_t pos = sizeof(co);
    if (co.source_len == 0 || co.source_len > in_len - pos || co.source_len > KMTL_MAX_LIB_SOURCE_BYTES ||
        co.num_macros > 1024 || !kmtl_valid_language_version(co.language_version)) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "malformed or oversized compile options", out_cap);
    }
    if (w->total_lib_source_bytes > KMTL_MAX_TOTAL_LIB_SOURCE_BYTES - co.source_len) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "shader library source memory quota exhausted", out_cap);
    }
    if (memchr(in_payload + pos, 0, co.source_len) != NULL) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "embedded NUL byte in MSL source", out_cap);
    }

    char *src = malloc(co.source_len + 1);
    if (!src) return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
    memcpy(src, in_payload + pos, co.source_len);
    src[co.source_len] = '\0';
    pos += co.source_len;

    msl_macro_t *macros = NULL;
    char **mkeys = NULL;
    char **mvals = NULL;
    if (co.num_macros > 0) {
      macros = calloc(co.num_macros, sizeof(*macros));
      mkeys = calloc(co.num_macros, sizeof(*mkeys));
      mvals = calloc(co.num_macros, sizeof(*mvals));
      if (!macros || !mkeys || !mvals) {
        free(src); free(macros); free(mkeys); free(mvals);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      for (uint32_t i = 0; i < co.num_macros; i++) {
        if (pos + sizeof(struct kmtl_macro_hdr) > in_len) goto bad_macros;
        struct kmtl_macro_hdr mh;
        memcpy(&mh, in_payload + pos, sizeof(mh));
        pos += sizeof(mh);
        if (mh.key_len == 0 || mh.key_len > 128 || mh.key_len > in_len - pos) goto bad_macros;
        if (memchr(in_payload + pos, 0, mh.key_len) || memchr(in_payload + pos, '\n', mh.key_len) ||
            memchr(in_payload + pos, '\r', mh.key_len))
          goto bad_macros;
        mkeys[i] = malloc(mh.key_len + 1);
        if (!mkeys[i]) goto bad_macros;
        memcpy(mkeys[i], in_payload + pos, mh.key_len);
        mkeys[i][mh.key_len] = '\0';
        pos += mh.key_len;
        if (mh.val_len > 4096 || mh.val_len > in_len - pos) goto bad_macros;
        if (mh.val_len > 0 && (memchr(in_payload + pos, 0, mh.val_len) ||
                               memchr(in_payload + pos, '\n', mh.val_len) ||
                               memchr(in_payload + pos, '\r', mh.val_len)))
          goto bad_macros;
        mvals[i] = malloc(mh.val_len + 1);
        if (!mvals[i]) goto bad_macros;
        if (mh.val_len > 0) memcpy(mvals[i], in_payload + pos, mh.val_len);
        mvals[i][mh.val_len] = '\0';
        pos += mh.val_len;
        macros[i].key = mkeys[i];
        macros[i].key_len = mh.key_len;
        macros[i].val = mvals[i];
        macros[i].val_len = mh.val_len;
      }
    }

    size_t idx = w->libraries_cap;
    for (size_t i = 0; i < w->libraries_cap; i++) {
      if (!w->libraries[i].active) { idx = i; break; }
    }
    if (idx == w->libraries_cap) {
      size_t ncap = w->libraries_cap ? w->libraries_cap * 2 : 32;
      vk_library_t *nl = realloc(w->libraries, ncap * sizeof(*nl));
      if (!nl) goto bad_macros;
      memset(nl + w->libraries_cap, 0, (ncap - w->libraries_cap) * sizeof(*nl));
      w->libraries = nl;
      w->libraries_cap = ncap;
    }

    uint64_t handle = w->next_handle++;
    vk_library_t *lib = &w->libraries[idx];
    lib->active = true;
    lib->handle = handle;
    lib->client_id = req->client_id;
    lib->source = src;
    lib->source_len = co.source_len;
    lib->macros = macros;
    lib->macro_keys = mkeys;
    lib->macro_vals = mvals;
    lib->num_macros = co.num_macros;
    w->total_lib_source_bytes += co.source_len;

    size_t max_names = (out_cap < KMTL_MAX_OUT_PAYLOAD) ? (size_t)out_cap : (size_t)KMTL_MAX_OUT_PAYLOAD;
    size_t need_names = msl_extract_kernel_names(src, co.source_len, NULL, 0);
    size_t names_cap = (need_names < max_names) ? need_names : max_names;
    char *names_buf = names_cap > 0 ? malloc(names_cap) : NULL;
    size_t names_len = names_buf ? msl_extract_kernel_names(src, co.source_len, names_buf, names_cap) : 0;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    bool ok = send_resp(w->sock_fd, resp, names_buf, (uint32_t)names_len, out_cap);
    free(names_buf);
    return ok;

  bad_macros:
    free(src);
    if (mkeys) { for (uint32_t i = 0; i < co.num_macros; i++) free(mkeys[i]); }
    if (mvals) { for (uint32_t i = 0; i < co.num_macros; i++) free(mvals[i]); }
    free(mkeys); free(mvals); free(macros);
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "malformed compile macros", out_cap);
  }

  case KMTL_OP_CREATE_LIBRARY_DATA: {
#ifdef KIDO_HAVE_AIR2SPV
    if (!has_client(w, req->client_id)) return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
    if (count_client_libraries(w, req->client_id) >= KMTL_MAX_LIBS_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "library limit reached", out_cap);
    }
    if (!in_payload || in_len == 0 || in_len > METALLIB_MAX_BYTES) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "metallib payload empty or oversized", out_cap);
    }
    if (w->total_lib_binary_bytes > KMTL_MAX_TOTAL_LIB_BINARY_BYTES - in_len) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "binary library memory quota exhausted", out_cap);
    }
    metallib_index_t midx = {0};
    char perr[512] = {0};
    if (!metallib_parse(in_payload, in_len, &midx, perr, sizeof(perr))) {
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, perr, out_cap);
    }

    size_t max_names = (out_cap < KMTL_MAX_OUT_PAYLOAD) ? (size_t)out_cap : (size_t)KMTL_MAX_OUT_PAYLOAD;
    size_t need_names = metallib_encode_compute_names(&midx, NULL, 0);
    size_t names_cap = (need_names < max_names) ? need_names : max_names;
    char *names_buf = names_cap > 0 ? malloc(names_cap) : NULL;
    size_t names_len = names_buf ? metallib_encode_compute_names(&midx, names_buf, names_cap) : 0;
    metallib_index_free(&midx);

    uint8_t *bin_copy = malloc(in_len);
    if (!bin_copy) {
      free(names_buf);
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
    }
    memcpy(bin_copy, in_payload, in_len);

    size_t idx = w->libraries_cap;
    for (size_t i = 0; i < w->libraries_cap; i++) {
      if (!w->libraries[i].active) { idx = i; break; }
    }
    if (idx == w->libraries_cap) {
      size_t ncap = w->libraries_cap ? w->libraries_cap * 2 : 32;
      vk_library_t *nl = realloc(w->libraries, ncap * sizeof(*nl));
      if (!nl) {
        free(names_buf);
        free(bin_copy);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      memset(nl + w->libraries_cap, 0, (ncap - w->libraries_cap) * sizeof(*nl));
      w->libraries = nl;
      w->libraries_cap = ncap;
    }

    uint64_t handle = w->next_handle++;
    vk_library_t *lib = &w->libraries[idx];
    memset(lib, 0, sizeof(*lib));
    lib->active = true;
    lib->handle = handle;
    lib->client_id = req->client_id;
    lib->is_binary = true;
    lib->binary = bin_copy;
    lib->binary_len = in_len;
    w->total_lib_binary_bytes += in_len;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    bool ok = send_resp(w->sock_fd, resp, names_buf, (uint32_t)names_len, out_cap);
    free(names_buf);
    return ok;
#else
    return send_err(w->sock_fd, KMTL_ERR_UNSUPP, "Vulkan backend built without AIR metallib support", out_cap);
#endif
  }

  case KMTL_OP_DESTROY_LIBRARY: {
    vk_library_t *l = find_library(w, req->handle, req->client_id);
    if (!l) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown library handle", out_cap);
    destroy_library_slot(w, l);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_FUNCTION: {
    vk_library_t *lib = find_library(w, req->handle, req->client_id);
    if (!has_client(w, req->client_id) || !lib) {
      return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown library handle", out_cap);
    }
    if (count_client_functions(w, req->client_id) >= KMTL_MAX_FNS_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "function limit reached", out_cap);
    }
    if (in_len < sizeof(struct kmtl_function_req)) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "truncated function request", out_cap);
    }
    struct kmtl_function_req fr;
    memcpy(&fr, in_payload, sizeof(fr));
    size_t pos = sizeof(fr);
    if (fr.name_len == 0 || fr.name_len >= 128 || fr.name_len > in_len - pos || fr.num_constants > 256 ||
        memchr(in_payload + pos, 0, fr.name_len) != NULL) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "malformed function request", out_cap);
    }
    char fn_name[128];
    memcpy(fn_name, in_payload + pos, fr.name_len);
    fn_name[fr.name_len] = '\0';
    pos = (size_t)align_up(pos + fr.name_len, 8);
    if (pos > in_len) return send_err(w->sock_fd, KMTL_ERR_INVAL, "truncated function request", out_cap);

    vk_func_const_t *consts = NULL;
    if (fr.num_constants > 0) {
      consts = calloc(fr.num_constants, sizeof(*consts));
      if (!consts) return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      for (uint32_t i = 0; i < fr.num_constants; i++) {
        if (pos + sizeof(struct kmtl_func_const) > in_len) {
          free(consts);
          return send_err(w->sock_fd, KMTL_ERR_INVAL, "truncated function constant", out_cap);
        }
        struct kmtl_func_const fc;
        memcpy(&fc, in_payload + pos, sizeof(fc));
        pos += sizeof(fc);
        uint32_t exp_len = kmtl_func_const_size(fc.data_type);
        uint64_t pad_len = align_up(fc.value_len, 8);
        if (exp_len == 0 || exp_len > 64 || fc.value_len < exp_len || pad_len > in_len - pos ||
            fc.index > 65535u) {
          free(consts);
          return send_err(w->sock_fd, KMTL_ERR_INVAL, "invalid function constant", out_cap);
        }
        for (uint32_t j = 0; j < i; j++) {
          if (consts[j].index == fc.index) {
            free(consts);
            return send_err(w->sock_fd, KMTL_ERR_INVAL, "duplicate function constant index", out_cap);
          }
        }
        consts[i].index = fc.index;
        consts[i].data_type = fc.data_type;
        consts[i].value_len = exp_len;
        memcpy(consts[i].value, in_payload + pos, exp_len);
        pos += (size_t)pad_len;
      }
    }

    uint32_t *spirv = NULL;
    size_t spirv_bytes = 0;
    uint32_t binding_mask = 0;
    uint32_t readonly_mask = 0;
    char err[1024] = {0};

    if (lib->is_binary) {
#ifdef KIDO_HAVE_AIR2SPV
      air2spv_caps_t caps = {
          .has_float16 = w->has_fp16,
          .has_int16 = w->has_int16,
          .has_int8 = w->has_int8,
          .has_storage_16bit = w->has_storage_16bit,
          .has_storage_8bit = w->has_storage_8bit,
          .has_int64 = w->has_int64,
          .max_workgroup_memory = w->props.limits.maxComputeSharedMemorySize,
          .subgroup_ops = w->subgroup_ops,
          .subgroup_size = w->subgroup_size,
          .ssbo_offset_align = (uint32_t)w->staging_align,
      };
      air2spv_result_t ar = {0};
      if (!compile_metallib_sandboxed(w, lib->binary, lib->binary_len, fn_name, &caps, &ar, err, sizeof(err))) {
        fprintf(stderr, "kido-vk-worker: AIR compile error for %s: %.512s\n", fn_name, err);
        free(consts);
        return send_err(w->sock_fd, KMTL_ERR_COMPILE, err, out_cap);
      }
      spirv = ar.spirv;
      spirv_bytes = ar.spirv_bytes;
      binding_mask = ar.binding_mask;
      readonly_mask = ar.readonly_mask;
#else
      free(consts);
      return send_err(w->sock_fd, KMTL_ERR_UNSUPP, "binary metallib not enabled", out_cap);
#endif
    } else {
      msl_translate_result_t tr;
      if (!msl_translate_kernel(lib->source, lib->source_len, lib->macros, lib->num_macros, fn_name, &tr, err,
                                sizeof(err))) {
        free(consts);
        return send_err(w->sock_fd, KMTL_ERR_COMPILE, err, out_cap);
      }
      if (!compile_glsl_to_spirv(w->glslang_path, tr.glsl, tr.glsl_len, &spirv, &spirv_bytes, err, sizeof(err))) {
        fprintf(stderr, "kido-vk-worker: GLSL compile error for %s: %.512s\n", fn_name, err);
        if (getenv("KIDO_VK_DEBUG_SHADER") != NULL) {
          fprintf(stderr, "--- GLSL ---\n%.4096s\n", tr.glsl);
        }
        free(tr.glsl);
        free(consts);
        return send_err(w->sock_fd, KMTL_ERR_COMPILE, err, out_cap);
      }
      free(tr.glsl);
      binding_mask = tr.binding_mask;
      readonly_mask = tr.readonly_mask;
    }

    if (spirv_bytes > KMTL_MAX_TOTAL_FN_SPIRV_BYTES ||
        w->total_fn_spirv_bytes > KMTL_MAX_TOTAL_FN_SPIRV_BYTES - spirv_bytes) {
      free(spirv);
      free(consts);
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "compiled function SPIR-V memory quota exhausted", out_cap);
    }

    size_t idx = w->functions_cap;
    for (size_t i = 0; i < w->functions_cap; i++) {
      if (!w->functions[i].active) { idx = i; break; }
    }
    if (idx == w->functions_cap) {
      size_t ncap = w->functions_cap ? w->functions_cap * 2 : 64;
      vk_function_t *nf = realloc(w->functions, ncap * sizeof(*nf));
      if (!nf) {
        free(spirv);
        free(consts);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      memset(nf + w->functions_cap, 0, (ncap - w->functions_cap) * sizeof(*nf));
      w->functions = nf;
      w->functions_cap = ncap;
    }

    uint64_t handle = w->next_handle++;
    vk_function_t *fn = &w->functions[idx];
    fn->active = true;
    fn->handle = handle;
    fn->client_id = req->client_id;
    snprintf(fn->name, sizeof(fn->name), "%s", fn_name);
    fn->spirv = spirv;
    fn->spirv_bytes = spirv_bytes;
    fn->binding_mask = binding_mask;
    fn->readonly_mask = readonly_mask;
    fn->consts = consts;
    fn->num_consts = fr.num_constants;
    fn->is_binary = lib->is_binary;
    w->total_fn_spirv_bytes += spirv_bytes;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_DESTROY_FUNCTION: {
    vk_function_t *f = find_function(w, req->handle, req->client_id);
    if (!f) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown function handle", out_cap);
    destroy_function_slot(w, f);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_COMPUTE_PIPELINE: {
    vk_function_t *fn = find_function(w, req->handle, req->client_id);
    if (!has_client(w, req->client_id) || !fn) {
      return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown function handle", out_cap);
    }
    if (!poll_retire_completed(w)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
      return false;
    }
    if (count_client_pipelines(w, req->client_id) >= KMTL_MAX_PSOS_PER_CLIENT && w->slots_in_flight > 0) {
      if (!drain_all_inflight(w)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "in-flight GPU command buffer failed", out_cap);
        return false;
      }
    }
    if (count_client_pipelines(w, req->client_id) >= KMTL_MAX_PSOS_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "pipeline limit reached", out_cap);
    }
    uint32_t dev_max_tg = w->props.limits.maxComputeWorkGroupInvocations;
    if (req->arg0 > dev_max_tg) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "maxTotalThreadsPerThreadgroup exceeds device limit", out_cap);
    }
    uint32_t max_tg = req->arg0 ? (uint32_t)req->arg0 : dev_max_tg;

    VkShaderModuleCreateInfo smci = {
        .sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO,
        .codeSize = fn->spirv_bytes,
        .pCode = fn->spirv,
    };
    VkShaderModule shader = VK_NULL_HANDLE;
    if (vkCreateShaderModule(w->device, &smci, NULL, &shader) != VK_SUCCESS) {
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, "vkCreateShaderModule failed", out_cap);
    }

    VkDescriptorSetLayoutBinding bindings[KMTL_MAX_BUFFER_SLOTS];
    uint32_t nb = 0;
    for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
      if (!(fn->binding_mask & (1u << s))) continue;
      bindings[nb++] = (VkDescriptorSetLayoutBinding){
          .binding = s,
          .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
          .descriptorCount = 1,
          .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
      };
    }
    VkDescriptorSetLayoutCreateInfo dslci = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
        .bindingCount = nb,
        .pBindings = bindings,
    };
    VkDescriptorSetLayout dsl = VK_NULL_HANDLE;
    if (vkCreateDescriptorSetLayout(w->device, &dslci, NULL, &dsl) != VK_SUCCESS) {
      vkDestroyShaderModule(w->device, shader, NULL);
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, "vkCreateDescriptorSetLayout failed", out_cap);
    }

    VkPushConstantRange pcr = {
        .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
        .offset = 0,
        .size = 128,
    };
    VkPipelineLayoutCreateInfo plci = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
        .setLayoutCount = 1,
        .pSetLayouts = &dsl,
        .pushConstantRangeCount = 1,
        .pPushConstantRanges = &pcr,
    };
    VkPipelineLayout pl = VK_NULL_HANDLE;
    if (vkCreatePipelineLayout(w->device, &plci, NULL, &pl) != VK_SUCCESS) {
      vkDestroyDescriptorSetLayout(w->device, dsl, NULL);
      vkDestroyShaderModule(w->device, shader, NULL);
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, "vkCreatePipelineLayout failed", out_cap);
    }

    vk_func_const_t *consts_copy = NULL;
    if (fn->num_consts > 0) {
      consts_copy = malloc(fn->num_consts * sizeof(*consts_copy));
      if (!consts_copy) {
        vkDestroyPipelineLayout(w->device, pl, NULL);
        vkDestroyDescriptorSetLayout(w->device, dsl, NULL);
        vkDestroyShaderModule(w->device, shader, NULL);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      memcpy(consts_copy, fn->consts, fn->num_consts * sizeof(*consts_copy));
    }

    size_t idx = w->pipelines_cap;
    for (size_t i = 0; i < w->pipelines_cap; i++) {
      if (!w->pipelines[i].active) { idx = i; break; }
    }
    if (idx == w->pipelines_cap) {
      size_t ncap = w->pipelines_cap ? w->pipelines_cap * 2 : 64;
      vk_pipeline_t *np = realloc(w->pipelines, ncap * sizeof(*np));
      if (!np) {
        free(consts_copy);
        vkDestroyPipelineLayout(w->device, pl, NULL);
        vkDestroyDescriptorSetLayout(w->device, dsl, NULL);
        vkDestroyShaderModule(w->device, shader, NULL);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      }
      memset(np + w->pipelines_cap, 0, (ncap - w->pipelines_cap) * sizeof(*np));
      w->pipelines = np;
      w->pipelines_cap = ncap;
    }

    uint64_t handle = w->next_handle++;
    vk_pipeline_t *p = &w->pipelines[idx];
    p->active = true;
    p->pending_destroy = false;
    p->inflight_refs = 0;
    p->handle = handle;
    p->client_id = req->client_id;
    p->shader = shader;
    p->ds_layout = dsl;
    p->pipe_layout = pl;
    p->binding_mask = fn->binding_mask;
    p->readonly_mask = fn->readonly_mask;
    p->max_tg = max_tg;
    p->is_binary = fn->is_binary;
    p->consts = consts_copy;
    p->num_consts = fn->num_consts;
    p->num_specs = 0;

    uint32_t eff_sg = (fn->is_binary && w->subgroup_size == 16u) ? 32u : w->subgroup_size;
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    resp.arg0 = ((uint64_t)max_tg & 0xFFFFFFFFULL) | (((uint64_t)eff_sg & 0xFFFFFFFFULL) << 32);
    resp.arg1 = 0;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_DESTROY_PIPELINE: {
    vk_pipeline_t *p = find_pipeline(w, req->handle, req->client_id);
    if (!p) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown pipeline handle", out_cap);
    destroy_pipeline_slot(w, p);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_QUEUE: {
    if (!has_client(w, req->client_id)) return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
    if (count_client_queues(w, req->client_id) >= KMTL_MAX_QUEUES_PER_CLIENT) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "queue limit reached", out_cap);
    }
    size_t idx = w->queues_cap;
    for (size_t i = 0; i < w->queues_cap; i++) {
      if (!w->queues[i].active) { idx = i; break; }
    }
    if (idx == w->queues_cap) {
      size_t ncap = w->queues_cap ? w->queues_cap * 2 : 32;
      vk_queue_t *nq = realloc(w->queues, ncap * sizeof(*nq));
      if (!nq) return send_err(w->sock_fd, KMTL_ERR_NOMEM, "host OOM", out_cap);
      memset(nq + w->queues_cap, 0, (ncap - w->queues_cap) * sizeof(*nq));
      w->queues = nq;
      w->queues_cap = ncap;
    }
    uint64_t handle = w->next_handle++;
    vk_queue_t *q = &w->queues[idx];
    memset(q, 0, sizeof(*q));
    q->active = true;
    q->handle = handle;
    q->client_id = req->client_id;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_DESTROY_QUEUE: {
    vk_queue_t *q = find_queue(w, req->handle, req->client_id);
    if (!q) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown queue handle", out_cap);
    if (q->completed_seq < q->submitted_seq || (w->deferred_flush_count > 0 && w->slots_in_flight > 0)) {
      if (!retire_up_to_queue_seq(w, q, q->submitted_seq)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "GPU command buffer failed while destroying queue", out_cap);
        return false;
      }
    }
    memset(q, 0, sizeof(*q));
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_COMMIT_COMMAND_BUFFER:
    return handle_commit(w, req, in_payload, in_len, out_cap, frame_kind == KMTL_FRAME_GUEST_RPC_PREFLUSHED);

  case KMTL_OP_WAIT_QUEUE: {
    vk_queue_t *q = find_queue(w, req->handle, req->client_id);
    if (!has_client(w, req->client_id) || !q) {
      return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown queue handle", out_cap);
    }
    uint64_t target = req->arg0 ? req->arg0 : q->submitted_seq;
    if (target > q->submitted_seq) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "wait sequence exceeds submitted command buffers", out_cap);
    }
    if ((target > 0 && q->completed_seq < target) || (w->deferred_flush_count > 0 && w->slots_in_flight > 0)) {
      if (!retire_up_to_queue_seq(w, q, target)) {
        send_err(w->sock_fd, KMTL_ERR_GPU, "vkWaitForFences failed or timed out", out_cap);
        return false;
      }
    }
    if (!flush_gpu_dirty_buffers(w, req->client_id)) {
      send_err(w->sock_fd, KMTL_ERR_GPU, "vkWaitForFences failed or timed out", out_cap);
      return false;
    }
    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    vk_seq_timing_t t = q->timings[target & 63u];
    if (target > 0 && t.seq == target) {
      resp.arg0 = t.gpu_start_ns;
      resp.arg1 = t.gpu_end_ns;
    } else {
      resp.arg0 = q->last_start_ns;
      resp.arg1 = q->last_end_ns;
    }
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  default:
    return send_err(w->sock_fd, KMTL_ERR_UNSUPP, "unsupported opcode", out_cap);
  }
}

static uint8_t *mmap_aligned_shm(int fd, uint64_t bytes, uint64_t align) {
  if (align > 4096u && (align & (align - 1u)) == 0 && bytes <= SIZE_MAX - align) {
    size_t reserve = (size_t)bytes + (size_t)align;
    void *raw = mmap(NULL, reserve, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (raw != MAP_FAILED) {
      uintptr_t base = ((uintptr_t)raw + (uintptr_t)(align - 1u)) & ~(uintptr_t)(align - 1u);
      void *mapped = mmap((void *)base, (size_t)bytes, PROT_READ | PROT_WRITE,
                          MAP_SHARED | MAP_FIXED, fd, 0);
      if (mapped != MAP_FAILED) {
        size_t head = (size_t)(base - (uintptr_t)raw);
        if (head > 0) munmap(raw, head);
        size_t tail = reserve - head - (size_t)bytes;
        if (tail > 0) munmap((uint8_t *)mapped + bytes, tail);
        return (uint8_t *)mapped;
      }
      munmap(raw, reserve);
    }
  }
  void *p = mmap(NULL, (size_t)bytes, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  return (p == MAP_FAILED) ? NULL : (uint8_t *)p;
}

static int run_selftest(void) {
  worker_t w;
  memset(&w, 0, sizeof(w));
  w.sock_fd = -1;
  w.udmabuf_fd = -1;
  w.host_page_size = KMTL_DARWIN_PAGE_SIZE;
  w.shm_size = 16u * 1024u * 1024u;
  w.shm_fd = memfd_create("kido-vk-selftest", MFD_CLOEXEC | MFD_ALLOW_SEALING);
  if (w.shm_fd < 0 || ftruncate(w.shm_fd, (off_t)w.shm_size) != 0) {
    perror("memfd_create");
    return 1;
  }
  fcntl(w.shm_fd, F_ADD_SEALS, F_SEAL_SHRINK);
  w.shm_base = mmap_aligned_shm(w.shm_fd, w.shm_size, 65536u);
  if (!w.shm_base) {
    perror("mmap");
    return 1;
  }
  if (!vk_init(&w)) return 1;

  const char *zc_str = (w.buf_tier == KMTL_BUF_TIER_DMABUF)     ? "YES (dmabuf)"
                       : (w.buf_tier == KMTL_BUF_TIER_HOST_PTR) ? "YES (host_ptr)"
                                                                : "NO (staging)";
  printf("selftest: GPU=%s zero_copy=%s dma_coherent=%s timestamps=%s inflight_slots=%u\n",
         w.props.deviceName, zc_str,
         w.dma_coherent ? "YES" : "NO",
         w.has_timestamps ? "YES" : "NO",
         KMTL_MAX_INFLIGHT_CMDS);

  const char *msl =
      "#include <metal_stdlib>\n"
      "using namespace metal;\n"
      "kernel void vadd(device const float *a [[buffer(0)]], device const float *b [[buffer(1)]],\n"
      "                 device float *c [[buffer(2)]], uint id [[thread_position_in_grid]]) {\n"
      "  c[id] = a[id] + b[id];\n"
      "}\n";
  msl_translate_result_t tr;
  char err[512];
  if (!msl_translate_kernel(msl, strlen(msl), NULL, 0, "vadd", &tr, err, sizeof(err))) {
    fprintf(stderr, "selftest translate failed: %s\n", err);
    return 1;
  }
  uint32_t *spv = NULL;
  size_t spv_len = 0;
  resolve_executable_in_path("glslangValidator", w.glslang_path, sizeof(w.glslang_path));
  if (!compile_glsl_to_spirv(w.glslang_path, tr.glsl, tr.glsl_len, &spv, &spv_len, err, sizeof(err))) {
    fprintf(stderr, "selftest spirv failed: %s\n", err);
    return 1;
  }
  printf("selftest: MSL -> GLSL -> SPIR-V compiled (%zu bytes)\n", spv_len);
  free(spv);
  free(tr.glsl);
  vk_destroy_device_objects(&w);
  munmap(w.shm_base, w.shm_size);
  close(w.shm_fd);
  if (w.udmabuf_fd >= 0) close(w.udmabuf_fd);
  return 0;
}

int main(int argc, char **argv) {
  signal(SIGPIPE, SIG_IGN);
#ifdef KIDO_HAVE_AIR2SPV
  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "--air-sandbox") == 0) {
      int sb_fd = -1;
      for (int j = 1; j < argc; j++) {
        if (strcmp(argv[j], "--sandbox-fd") == 0 && j + 1 < argc) sb_fd = atoi(argv[++j]);
      }
      return run_air_sandbox(sb_fd);
    }
  }
#endif

  int sock_fd = -1;
  int shm_fd = -1;
  uint64_t shm_bytes = 0;
  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "--selftest") == 0) return run_selftest();
    if (strcmp(argv[i], "--sock-fd") == 0 && i + 1 < argc) sock_fd = atoi(argv[++i]);
    else if (strcmp(argv[i], "--shm-fd") == 0 && i + 1 < argc) shm_fd = atoi(argv[++i]);
    else if (strcmp(argv[i], "--shm-bytes") == 0 && i + 1 < argc) shm_bytes = strtoull(argv[++i], NULL, 10);
  }
  if (sock_fd < 0 || shm_fd < 0 || shm_bytes < (64ULL << 20) ||
      shm_bytes > KMTL_MAX_SHM_BYTES || (shm_bytes & (KMTL_DARWIN_PAGE_SIZE - 1)) != 0) {
    fprintf(stderr, "usage: kido-vk-worker --sock-fd FD --shm-fd FD --shm-bytes N | --selftest\n");
    return 2;
  }

  /* Ensure sock_fd and shm_fd are never inherited by glslangValidator or AIR sandbox children. */
  fcntl(sock_fd, F_SETFD, FD_CLOEXEC);
  fcntl(shm_fd, F_SETFD, FD_CLOEXEC);

  struct stat shm_st;
  if (fstat(shm_fd, &shm_st) != 0 || shm_st.st_size < 0 || (uint64_t)shm_st.st_size < shm_bytes) {
    fprintf(stderr, "kido-vk-worker: shm fd size mismatch\n");
    return 1;
  }

  worker_t w;
  memset(&w, 0, sizeof(w));
  w.sock_fd = sock_fd;
  w.shm_fd = shm_fd;
  w.udmabuf_fd = -1;
#ifdef KIDO_HAVE_AIR2SPV
  w.air_sb_fd = -1;
  w.air_sb_pid = -1;
#endif
  w.shm_size = shm_bytes;
  w.host_page_size = (uint32_t)getpagesize();
  if (w.host_page_size < KMTL_DARWIN_PAGE_SIZE) w.host_page_size = KMTL_DARWIN_PAGE_SIZE;
  w.next_handle = 1;
#ifdef KIDO_HAVE_AIR2SPV
  resolve_self_exe(w.self_exe, sizeof(w.self_exe));
#endif
  resolve_executable_in_path("glslangValidator", w.glslang_path, sizeof(w.glslang_path));

  w.shm_base = mmap_aligned_shm(shm_fd, shm_bytes, 65536u);
  if (!w.shm_base) {
    fprintf(stderr, "kido-vk-worker: mmap shm failed: %s\n", strerror(errno));
    return 1;
  }
  if (!kmtl_shm_init(&w.shm_alloc, shm_bytes)) {
    fprintf(stderr, "kido-vk-worker: shm allocator init failed\n");
    return 1;
  }
  if (!vk_init(&w)) {
    return 1;
  }

  for (;;) {
    uint8_t frame_hdr[1 + sizeof(struct kmtl_req_hdr) + 8];
    if (!read_full(sock_fd, frame_hdr, sizeof(frame_hdr))) break;
    uint8_t frame_kind = frame_hdr[0];
    struct kmtl_req_hdr req;
    uint32_t in_len = 0, out_cap = 0;
    memcpy(&req, frame_hdr + 1, sizeof(req));
    memcpy(&in_len, frame_hdr + 1 + sizeof(req), 4);
    memcpy(&out_cap, frame_hdr + 1 + sizeof(req) + 4, 4);
    if (in_len > KMTL_MAX_IN_PAYLOAD) {
      fprintf(stderr, "kido-vk-worker: oversized in_len %u\n", in_len);
      break;
    }
    uint8_t *in_buf = NULL;
    if (in_len > 0) {
      in_buf = malloc(in_len);
      if (!in_buf || !read_full(sock_fd, in_buf, in_len)) {
        free(in_buf);
        break;
      }
    }
    bool ok = handle_rpc(&w, frame_kind, &req, in_buf, in_len, out_cap);
    free(in_buf);
    if (!ok) break;
  }

  bool clean_reset = reset_all(&w);
#ifdef KIDO_HAVE_AIR2SPV
  air_sandbox_kill(&w);
#endif
#if defined(__aarch64__)
  flush_pool_destroy(&w);
#endif
  if (clean_reset) {
    vk_destroy_device_objects(&w);
  }
  free(w.buffers);
  free(w.libraries);
  free(w.functions);
  free(w.pipelines);
  free(w.queues);
  kmtl_shm_destroy(&w.shm_alloc);
  munmap(w.shm_base, (size_t)w.shm_size);
  if (w.udmabuf_fd >= 0) close(w.udmabuf_fd);
  close(shm_fd);
  close(sock_fd);
  return 0;
}
