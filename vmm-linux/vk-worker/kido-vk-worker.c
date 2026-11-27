/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#define _GNU_SOURCE

#include "../../vmm/kmtl_proto.h"
#include "../../vmm/kmtl_shm.h"
#include "msl2glsl.h"

#include <errno.h>
#include <fcntl.h>
#include <linux/dma-buf.h>
#include <linux/udmabuf.h>
#include <poll.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <vulkan/vulkan.h>

#define KMTL_FRAME_GUEST_RPC 0u
#define KMTL_FRAME_VMM_RESET 1u
#define KMTL_MAX_CLIENTS 256u
#define KMTL_MAX_PSO_SPECS 16u
#define KMTL_MAX_DEFERRED_PIPES 256u
#define KMTL_STAGING_BYTES (4u * 1024u * 1024u)
#define KMTL_DARWIN_PAGE_SIZE 16384u
#define KMTL_MAX_LIB_SOURCE_BYTES (4u * 1024u * 1024u)
#define KMTL_MAX_TOTAL_LIB_SOURCE_BYTES (32u * 1024u * 1024u)
#define KMTL_MAX_SHM_BYTES (128ULL << 30)
#define KMTL_COMPILE_TIMEOUT_MS 15000

typedef struct {
  bool active;
  uint32_t client_id;
} vk_client_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  uint64_t shm_offset;
  uint64_t aligned_len;
  uint64_t requested_len;
  bool is_dmabuf;
  int dmabuf_fd;
  VkBuffer buffer;
  VkDeviceMemory memory;
  void *mapped;
} vk_buffer_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  char *source;
  size_t source_len;
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
} vk_function_t;

typedef struct {
  uint32_t tx, ty, tz;
  VkPipeline pipeline;
} vk_pso_spec_t;

typedef struct {
  bool active;
  uint64_t handle;
  uint32_t client_id;
  VkShaderModule shader;
  VkDescriptorSetLayout ds_layout;
  VkPipelineLayout pipe_layout;
  uint32_t binding_mask;
  uint32_t readonly_mask;
  uint32_t max_tg;
  vk_func_const_t *consts;
  uint32_t num_consts;
  vk_pso_spec_t specs[KMTL_MAX_PSO_SPECS];
  uint32_t num_specs;
} vk_pipeline_t;

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
  uint64_t next_handle;

  VkInstance instance;
  VkPhysicalDevice phys_dev;
  VkDevice device;
  uint32_t queue_family;
  VkQueue queue;
  VkCommandPool cmd_pool;
  VkCommandBuffer cmd_buf;
  VkFence fence;
  VkDescriptorPool desc_pool;

  VkBuffer staging_buf;
  VkDeviceMemory staging_mem;
  uint8_t *staging_ptr;
  uint64_t staging_align;

  VkPhysicalDeviceProperties props;
  VkPhysicalDeviceMemoryProperties mem_props;
  uint32_t subgroup_size;
  bool has_dmabuf_ext;
  bool has_fp16;
  bool logged_buf_mode;
  PFN_vkGetMemoryFdPropertiesKHR pfn_get_mem_fd_props;

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
  for (uint32_t i = 0; i < dev_count; i++) {
    VkPhysicalDeviceProperties p;
    vkGetPhysicalDeviceProperties(devs[i], &p);
    uint32_t qcount = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, NULL);
    VkQueueFamilyProperties *qprops = calloc(qcount ? qcount : 1, sizeof(*qprops));
    if (!qprops) continue;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, qprops);
    int qf = -1;
    for (uint32_t q = 0; q < qcount; q++) {
      if (qprops[q].queueFlags & VK_QUEUE_COMPUTE_BIT) {
        qf = (int)q;
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

  VkPhysicalDeviceSubgroupProperties sg_props = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SUBGROUP_PROPERTIES,
  };
  VkPhysicalDeviceProperties2 props2 = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
      .pNext = &sg_props,
  };
  vkGetPhysicalDeviceProperties2(w->phys_dev, &props2);
  w->subgroup_size = sg_props.subgroupSize ? sg_props.subgroupSize : 16;

  VkPhysicalDeviceVulkan12Features feat12_avail = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES,
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

  uint32_t ext_count = 0;
  vkEnumerateDeviceExtensionProperties(w->phys_dev, NULL, &ext_count, NULL);
  VkExtensionProperties *exts = calloc(ext_count ? ext_count : 1, sizeof(*exts));
  bool has_ext_mem_fd = false;
  bool has_ext_dmabuf = false;
  if (exts && ext_count > 0) {
    vkEnumerateDeviceExtensionProperties(w->phys_dev, NULL, &ext_count, exts);
    for (uint32_t i = 0; i < ext_count; i++) {
      if (strcmp(exts[i].extensionName, VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME) == 0) has_ext_mem_fd = true;
      if (strcmp(exts[i].extensionName, VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME) == 0) has_ext_dmabuf = true;
    }
  }
  free(exts);

  const char *enabled_exts[4];
  uint32_t enabled_ext_count = 0;
  if (has_ext_mem_fd && has_ext_dmabuf) {
    enabled_exts[enabled_ext_count++] = VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME;
    enabled_exts[enabled_ext_count++] = VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME;
    w->has_dmabuf_ext = true;
  }

  VkPhysicalDeviceVulkan12Features feat12_en = {
      .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES,
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

  VkCommandPoolCreateInfo cpci = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
      .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
      .queueFamilyIndex = w->queue_family,
  };
  if (vkCreateCommandPool(w->device, &cpci, NULL, &w->cmd_pool) != VK_SUCCESS) return false;

  VkCommandBufferAllocateInfo cbai = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
      .commandPool = w->cmd_pool,
      .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
      .commandBufferCount = 1,
  };
  if (vkAllocateCommandBuffers(w->device, &cbai, &w->cmd_buf) != VK_SUCCESS) return false;

  VkFenceCreateInfo fci = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
  if (vkCreateFence(w->device, &fci, NULL, &w->fence) != VK_SUCCESS) return false;

  VkDescriptorPoolSize pool_sz = {
      .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
      .descriptorCount = 16384,
  };
  VkDescriptorPoolCreateInfo dpci = {
      .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
      .maxSets = 2048,
      .poolSizeCount = 1,
      .pPoolSizes = &pool_sz,
  };
  if (vkCreateDescriptorPool(w->device, &dpci, NULL, &w->desc_pool) != VK_SUCCESS) return false;

  w->staging_align = w->props.limits.minStorageBufferOffsetAlignment;
  if (w->staging_align < 16) w->staging_align = 16;

  VkBufferCreateInfo bci = {
      .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
      .size = KMTL_STAGING_BYTES,
      .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT,
      .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
  };
  if (vkCreateBuffer(w->device, &bci, NULL, &w->staging_buf) != VK_SUCCESS) return false;
  VkMemoryRequirements mr;
  vkGetBufferMemoryRequirements(w->device, w->staging_buf, &mr);
  int mtype = find_memory_type(&w->mem_props, mr.memoryTypeBits,
                               VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
  if (mtype < 0) return false;
  VkMemoryAllocateInfo mai = {
      .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
      .allocationSize = mr.size,
      .memoryTypeIndex = (uint32_t)mtype,
  };
  if (vkAllocateMemory(w->device, &mai, NULL, &w->staging_mem) != VK_SUCCESS) return false;
  if (vkBindBufferMemory(w->device, w->staging_buf, w->staging_mem, 0) != VK_SUCCESS) return false;
  if (vkMapMemory(w->device, w->staging_mem, 0, KMTL_STAGING_BYTES, 0, (void **)&w->staging_ptr) != VK_SUCCESS)
    return false;

  w->udmabuf_fd = open("/dev/udmabuf", O_RDWR | O_CLOEXEC);
  fprintf(stderr, "kido-vk-worker: GPU=%s (subgroup=%u fp16=%d dmabuf=%d udmabuf=%d)\n",
          w->props.deviceName, w->subgroup_size, w->has_fp16 ? 1 : 0,
          w->has_dmabuf_ext ? 1 : 0, w->udmabuf_fd >= 0 ? 1 : 0);
  return true;
}

static void vk_destroy_device_objects(worker_t *w) {
  if (w->device) {
    vkDeviceWaitIdle(w->device);
    if (w->staging_ptr && w->staging_mem) {
      vkUnmapMemory(w->device, w->staging_mem);
      w->staging_ptr = NULL;
    }
    if (w->staging_buf) vkDestroyBuffer(w->device, w->staging_buf, NULL);
    if (w->staging_mem) vkFreeMemory(w->device, w->staging_mem, NULL);
    if (w->desc_pool) vkDestroyDescriptorPool(w->device, w->desc_pool, NULL);
    if (w->fence) vkDestroyFence(w->device, w->fence, NULL);
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

static void destroy_buffer_slot(worker_t *w, vk_buffer_t *b) {
  if (!b->active) return;
  if (b->mapped && b->memory) {
    vkUnmapMemory(w->device, b->memory);
    b->mapped = NULL;
  }
  if (b->buffer) vkDestroyBuffer(w->device, b->buffer, NULL);
  if (b->memory) vkFreeMemory(w->device, b->memory, NULL);
  if (b->dmabuf_fd >= 0) close(b->dmabuf_fd);
  if (w->used_bytes >= b->aligned_len) w->used_bytes -= b->aligned_len;
  else w->used_bytes = 0;
  kmtl_shm_free(&w->shm_alloc, b->shm_offset, b->aligned_len);
  memset(b, 0, sizeof(*b));
  b->dmabuf_fd = -1;
}

static void destroy_library_slot(worker_t *w, vk_library_t *l) {
  if (!l->active) return;
  if (w->total_lib_source_bytes >= l->source_len) w->total_lib_source_bytes -= l->source_len;
  else w->total_lib_source_bytes = 0;
  free(l->source);
  for (size_t i = 0; i < l->num_macros; i++) {
    free(l->macro_keys[i]);
    free(l->macro_vals[i]);
  }
  free(l->macro_keys);
  free(l->macro_vals);
  free(l->macros);
  memset(l, 0, sizeof(*l));
}

static void destroy_function_slot(vk_function_t *f) {
  if (!f->active) return;
  free(f->spirv);
  free(f->consts);
  memset(f, 0, sizeof(*f));
}

static void destroy_pipeline_slot(worker_t *w, vk_pipeline_t *p) {
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

static void close_client(worker_t *w, uint32_t cid) {
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
    if (w->functions[i].active && w->functions[i].client_id == cid) destroy_function_slot(&w->functions[i]);
  }
  for (size_t i = 0; i < w->libraries_cap; i++) {
    if (w->libraries[i].active && w->libraries[i].client_id == cid) destroy_library_slot(w, &w->libraries[i]);
  }
  for (size_t i = 0; i < w->buffers_cap; i++) {
    if (w->buffers[i].active && w->buffers[i].client_id == cid) destroy_buffer_slot(w, &w->buffers[i]);
  }
}

static void reset_all(worker_t *w) {
  if (w->device) vkDeviceWaitIdle(w->device);
  for (size_t i = 0; i < KMTL_MAX_CLIENTS; i++) {
    if (w->clients[i].active) close_client(w, w->clients[i].client_id);
  }
}

static vk_buffer_t *find_buffer(worker_t *w, uint64_t h, uint32_t cid) {
  for (size_t i = 0; i < w->buffers_cap; i++) {
    if (w->buffers[i].active && w->buffers[i].handle == h && w->buffers[i].client_id == cid)
      return &w->buffers[i];
  }
  return NULL;
}

static vk_library_t *find_library(worker_t *w, uint64_t h, uint32_t cid) {
  for (size_t i = 0; i < w->libraries_cap; i++) {
    if (w->libraries[i].active && w->libraries[i].handle == h && w->libraries[i].client_id == cid)
      return &w->libraries[i];
  }
  return NULL;
}

static vk_function_t *find_function(worker_t *w, uint64_t h, uint32_t cid) {
  for (size_t i = 0; i < w->functions_cap; i++) {
    if (w->functions[i].active && w->functions[i].handle == h && w->functions[i].client_id == cid)
      return &w->functions[i];
  }
  return NULL;
}

static vk_pipeline_t *find_pipeline(worker_t *w, uint64_t h, uint32_t cid) {
  for (size_t i = 0; i < w->pipelines_cap; i++) {
    if (w->pipelines[i].active && w->pipelines[i].handle == h && w->pipelines[i].client_id == cid)
      return &w->pipelines[i];
  }
  return NULL;
}

static vk_queue_t *find_queue(worker_t *w, uint64_t h, uint32_t cid) {
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
                                       uint64_t aligned_len,
                                       VkBuffer *out_buf,
                                       VkDeviceMemory *out_mem,
                                       int *out_dmabuf_fd) {
  if (getenv("KIDO_VK_NO_DMABUF") != NULL) return false;
  if (w->udmabuf_fd < 0 || !w->has_dmabuf_ext || w->shm_fd < 0) return false;
  struct udmabuf_create uc = {
      .memfd = (uint32_t)w->shm_fd,
      .flags = UDMABUF_FLAGS_CLOEXEC,
      .offset = shm_offset,
      .size = aligned_len,
  };
  int dfd = ioctl(w->udmabuf_fd, UDMABUF_CREATE, &uc);
  if (dfd < 0) return false;

  VkExternalMemoryBufferCreateInfo ext_bci = {
      .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
      .handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
  };
  VkBufferCreateInfo bci = {
      .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
      .pNext = &ext_bci,
      .size = aligned_len,
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
  if (mtype < 0 || mr.size > aligned_len) {
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
      .allocationSize = aligned_len,
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

static bool compile_glsl_to_spirv(const char *glsl,
                                  size_t glsl_len,
                                  uint32_t **out_words,
                                  size_t *out_bytes,
                                  char *err_buf,
                                  size_t err_cap) {
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
    int devnull = open("/dev/null", O_RDONLY);
    if (devnull >= 0) {
      dup2(devnull, STDIN_FILENO);
      if (devnull > 2) close(devnull);
    }
    dup2(pfd[1], STDOUT_FILENO);
    dup2(pfd[1], STDERR_FILENO);
    close(pfd[1]);
    struct rlimit rl_cpu = {15, 15};
    struct rlimit rl_as = {512ULL << 20, 512ULL << 20};
    struct rlimit rl_fsize = {32ULL << 20, 32ULL << 20};
    setrlimit(RLIMIT_CPU, &rl_cpu);
    setrlimit(RLIMIT_AS, &rl_as);
    setrlimit(RLIMIT_FSIZE, &rl_fsize);
    char *argv[] = {"glslangValidator", "-V", "--target-env", "vulkan1.3", "-S", "comp", "-o", spv_path, comp_path,
                    NULL};
    execvp("glslangValidator", argv);
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
    entries[d].constantID = 1000u + d;
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
  uint64_t line = arm64_dcache_line_size();
  uintptr_t cur = (uintptr_t)ptr & ~(uintptr_t)(line - 1);
  uintptr_t end = (uintptr_t)ptr + len;
  for (; cur < end; cur += line) {
    __asm__ volatile("dc cvac, %0" : : "r"(cur) : "memory");
  }
  __asm__ volatile("dsb sy" : : : "memory");
}

static void arm64_clean_inv_dcache_poc(const void *ptr, size_t len) {
  uint64_t line = arm64_dcache_line_size();
  uintptr_t cur = (uintptr_t)ptr & ~(uintptr_t)(line - 1);
  uintptr_t end = (uintptr_t)ptr + len;
  for (; cur < end; cur += line) {
    __asm__ volatile("dc civac, %0" : : "r"(cur) : "memory");
  }
  __asm__ volatile("dsb sy" : : : "memory");
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

static bool handle_commit(worker_t *w,
                          const struct kmtl_req_hdr *req,
                          const uint8_t *stream,
                          uint32_t stream_len,
                          uint32_t out_cap) {
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

  vkResetDescriptorPool(w->device, w->desc_pool, 0);
  vkResetCommandBuffer(w->cmd_buf, 0);
  VkCommandBufferBeginInfo cbbi = {
      .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
      .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
  };
  if (vkBeginCommandBuffer(w->cmd_buf, &cbbi) != VK_SUCCESS) {
    return send_err(w->sock_fd, KMTL_ERR_GPU, "vkBeginCommandBuffer failed", out_cap);
  }

  vk_buffer_t *touched[256];
  bool touched_written[256];
  size_t n_touched = 0;
  VkPipeline deferred_pipes[KMTL_MAX_DEFERRED_PIPES];
  size_t n_deferred_pipes = 0;

  bool bad = false;
  const char *bad_reason = "invalid command stream";
  #define TOUCH_BUF(B, WR)                                                  \
    do {                                                                    \
      bool seen_ = false;                                                   \
      for (size_t ti_ = 0; ti_ < n_touched; ti_++) {                        \
        if (touched[ti_] == (B)) {                                          \
          if (WR) touched_written[ti_] = true;                              \
          seen_ = true;                                                     \
          break;                                                            \
        }                                                                   \
      }                                                                     \
      if (!seen_) {                                                         \
        if (n_touched >= 256) {                                             \
          bad = true;                                                       \
          bad_reason = "too many distinct buffers in command buffer";       \
        } else {                                                            \
          touched[n_touched] = (B);                                         \
          touched_written[n_touched] = (WR);                                \
          n_touched++;                                                      \
        }                                                                   \
      }                                                                     \
    } while (0)

  bool in_comp = false, in_blit = false;
  vk_pipeline_t *cur_pso = NULL;
  vk_buffer_t *bound_buf[KMTL_MAX_BUFFER_SLOTS] = {NULL};
  uint64_t bound_off[KMTL_MAX_BUFFER_SLOTS] = {0};
  bool bound_is_bytes[KMTL_MAX_BUFFER_SLOTS] = {false};
  uint64_t bound_bytes_off[KMTL_MAX_BUFFER_SLOTS] = {0};
  uint32_t bound_bytes_len[KMTL_MAX_BUFFER_SLOTS] = {0};
  uint32_t tg_mem_lengths[KMTL_MAX_BUFFER_SLOTS] = {0};
  uint64_t staging_cursor = 0;

  size_t pos = 0;
  bool any_gpu_cmd = false;

  while (pos < stream_len && !bad) {
    if (stream_len - pos < sizeof(struct kmtl_cmd_hdr)) {
      bad = true;
      break;
    }
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
      cur_pso = NULL;
      memset(bound_buf, 0, sizeof(bound_buf));
      memset(bound_off, 0, sizeof(bound_off));
      memset(bound_is_bytes, 0, sizeof(bound_is_bytes));
      memset(tg_mem_lengths, 0, sizeof(tg_mem_lengths));
      if (any_gpu_cmd) emit_full_barrier(w->cmd_buf);
      break;

    case KMTL_CMD_BEGIN_BLIT:
      if (in_comp || in_blit) { bad = true; break; }
      in_blit = true;
      if (any_gpu_cmd) emit_full_barrier(w->cmd_buf);
      break;

    case KMTL_CMD_END_ENCODER:
      in_comp = false;
      in_blit = false;
      cur_pso = NULL;
      break;

    case KMTL_CMD_SET_PIPELINE: {
      if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_pipeline)) { bad = true; break; }
      struct kmtl_cmd_set_pipeline c;
      memcpy(&c, rec, sizeof(c));
      vk_pipeline_t *p = find_pipeline(w, c.pso_handle, req->client_id);
      if (!p) { bad = true; bad_reason = "unknown pipeline handle"; break; }
      cur_pso = p;
      break;
    }

    case KMTL_CMD_SET_BUFFER: {
      if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_buffer)) { bad = true; break; }
      struct kmtl_cmd_set_buffer c;
      memcpy(&c, rec, sizeof(c));
      if (c.index >= KMTL_MAX_BUFFER_SLOTS) { bad = true; break; }
      bound_is_bytes[c.index] = false;
      if (c.buf_handle == 0) {
        bound_buf[c.index] = NULL;
        bound_off[c.index] = 0;
      } else {
        vk_buffer_t *b = find_buffer(w, c.buf_handle, req->client_id);
        if (!b || c.offset >= b->aligned_len || (c.offset % w->staging_align) != 0) {
          bad = true;
          bad_reason = "invalid or unaligned buffer offset";
          break;
        }
        bound_buf[c.index] = b;
        bound_off[c.index] = c.offset;
      }
      break;
    }

    case KMTL_CMD_SET_BUFFER_OFFSET: {
      if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_set_buffer_offset)) { bad = true; break; }
      struct kmtl_cmd_set_buffer_offset c;
      memcpy(&c, rec, sizeof(c));
      if (c.index >= KMTL_MAX_BUFFER_SLOTS || !bound_buf[c.index] ||
          c.offset >= bound_buf[c.index]->aligned_len || (c.offset % w->staging_align) != 0) {
        bad = true;
        bad_reason = "invalid or unaligned buffer offset";
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
      uint64_t off = align_up(staging_cursor, w->staging_align);
      uint64_t end = off + c.length;
      if (end > KMTL_STAGING_BYTES) {
        bad = true;
        bad_reason = "staging buffer exhausted for setBytes";
        break;
      }
      memcpy(w->staging_ptr + off, rec + sizeof(c), c.length);
      staging_cursor = end;
      bound_buf[c.index] = NULL;
      bound_is_bytes[c.index] = true;
      bound_bytes_off[c.index] = off;
      bound_bytes_len[c.index] = c.length;
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
      if (total_tg > w->props.limits.maxComputeSharedMemorySize) {
        bad = true;
        break;
      }
      tg_mem_lengths[c.index] = c.length;
      break;
    }

    case KMTL_CMD_DISPATCH_THREADGROUPS:
    case KMTL_CMD_DISPATCH_THREADS: {
      if (!in_comp || !cur_pso || hdr.size < sizeof(struct kmtl_cmd_dispatch)) { bad = true; break; }
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
      VkPipeline pipe = get_or_create_vk_pipeline(w, cur_pso, (uint32_t)c.tx, (uint32_t)c.ty, (uint32_t)c.tz,
                                                  deferred_pipes, &n_deferred_pipes);
      if (!pipe) {
        bad = true;
        bad_reason = "vkCreateComputePipelines failed";
        break;
      }

      if (cur_pso->binding_mask != 0) {
        VkDescriptorSetAllocateInfo dsai = {
            .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
            .descriptorPool = w->desc_pool,
            .descriptorSetCount = 1,
            .pSetLayouts = &cur_pso->ds_layout,
        };
        VkDescriptorSet dset = VK_NULL_HANDLE;
        if (vkAllocateDescriptorSets(w->device, &dsai, &dset) != VK_SUCCESS) {
          bad = true;
          bad_reason = "vkAllocateDescriptorSets failed";
          break;
        }
        VkDescriptorBufferInfo dbis[KMTL_MAX_BUFFER_SLOTS];
        VkWriteDescriptorSet writes[KMTL_MAX_BUFFER_SLOTS];
        uint32_t nw = 0;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) {
            dbis[nw].buffer = w->staging_buf;
            dbis[nw].offset = bound_bytes_off[s];
            dbis[nw].range = bound_bytes_len[s];
          } else if (bound_buf[s]) {
            bool is_writable = (cur_pso->readonly_mask & (1u << s)) == 0;
            TOUCH_BUF(bound_buf[s], is_writable);
            if (bad) break;
            dbis[nw].buffer = bound_buf[s]->buffer;
            dbis[nw].offset = bound_off[s];
            dbis[nw].range = VK_WHOLE_SIZE;
          } else {
            bad = true;
            bad_reason = "missing buffer binding for compute kernel";
            break;
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
        if (bad) break;
        vkUpdateDescriptorSets(w->device, nw, writes, 0, NULL);
        vkCmdBindDescriptorSets(w->cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, cur_pso->pipe_layout, 0, 1, &dset, 0, NULL);
      }

      vkCmdBindPipeline(w->cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, pipe);
      vkCmdDispatch(w->cmd_buf, (uint32_t)ngx, (uint32_t)ngy, (uint32_t)ngz);
      emit_full_barrier(w->cmd_buf);
      any_gpu_cmd = true;
      break;
    }

    case KMTL_CMD_DISPATCH_INDIRECT: {
      if (!in_comp || !cur_pso || hdr.size < sizeof(struct kmtl_cmd_dispatch_indirect)) { bad = true; break; }
      struct kmtl_cmd_dispatch_indirect c;
      memcpy(&c, rec, sizeof(c));
      vk_buffer_t *ib = find_buffer(w, c.buf_handle, req->client_id);
      if (!ib || (c.offset & 3ULL) != 0 || c.offset > ib->aligned_len || ib->aligned_len - c.offset < 12 ||
          !validate_tg_dims(w, cur_pso, c.tx, c.ty, c.tz)) {
        bad = true;
        break;
      }
      TOUCH_BUF(ib, false);
      if (bad) break;
      VkPipeline pipe = get_or_create_vk_pipeline(w, cur_pso, (uint32_t)c.tx, (uint32_t)c.ty, (uint32_t)c.tz,
                                                  deferred_pipes, &n_deferred_pipes);
      if (!pipe) { bad = true; break; }
      if (cur_pso->binding_mask != 0) {
        VkDescriptorSetAllocateInfo dsai = {
            .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
            .descriptorPool = w->desc_pool,
            .descriptorSetCount = 1,
            .pSetLayouts = &cur_pso->ds_layout,
        };
        VkDescriptorSet dset = VK_NULL_HANDLE;
        if (vkAllocateDescriptorSets(w->device, &dsai, &dset) != VK_SUCCESS) { bad = true; break; }
        VkDescriptorBufferInfo dbis[KMTL_MAX_BUFFER_SLOTS];
        VkWriteDescriptorSet writes[KMTL_MAX_BUFFER_SLOTS];
        uint32_t nw = 0;
        for (uint32_t s = 0; s < KMTL_MAX_BUFFER_SLOTS; s++) {
          if (!(cur_pso->binding_mask & (1u << s))) continue;
          if (bound_is_bytes[s]) {
            dbis[nw] = (VkDescriptorBufferInfo){w->staging_buf, bound_bytes_off[s], bound_bytes_len[s]};
          } else if (bound_buf[s]) {
            bool is_writable = (cur_pso->readonly_mask & (1u << s)) == 0;
            TOUCH_BUF(bound_buf[s], is_writable);
            if (bad) break;
            dbis[nw] = (VkDescriptorBufferInfo){bound_buf[s]->buffer, bound_off[s], VK_WHOLE_SIZE};
          } else {
            bad = true;
            break;
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
        if (bad) break;
        vkUpdateDescriptorSets(w->device, nw, writes, 0, NULL);
        vkCmdBindDescriptorSets(w->cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, cur_pso->pipe_layout, 0, 1, &dset, 0, NULL);
      }
      vkCmdBindPipeline(w->cmd_buf, VK_PIPELINE_BIND_POINT_COMPUTE, pipe);
      vkCmdDispatchIndirect(w->cmd_buf, ib->buffer, c.offset);
      emit_full_barrier(w->cmd_buf);
      any_gpu_cmd = true;
      break;
    }

    case KMTL_CMD_MEMORY_BARRIER: {
      if (!in_comp || hdr.size < sizeof(struct kmtl_cmd_barrier)) { bad = true; break; }
      struct kmtl_cmd_barrier c;
      memcpy(&c, rec, sizeof(c));
      if (c.scope == 0 || (c.scope & ~3u) != 0) { bad = true; break; }
      emit_full_barrier(w->cmd_buf);
      break;
    }

    case KMTL_CMD_BLIT_COPY_BUFFER: {
      if (!in_blit || hdr.size < sizeof(struct kmtl_cmd_blit_copy)) { bad = true; break; }
      struct kmtl_cmd_blit_copy c;
      memcpy(&c, rec, sizeof(c));
      vk_buffer_t *src = find_buffer(w, c.src_handle, req->client_id);
      vk_buffer_t *dst = find_buffer(w, c.dst_handle, req->client_id);
      if (!src || !dst || c.src_offset > src->aligned_len || c.dst_offset > dst->aligned_len ||
          c.size > src->aligned_len - c.src_offset || c.size > dst->aligned_len - c.dst_offset) {
        bad = true;
        break;
      }
      if (c.size > 0) {
        TOUCH_BUF(src, false);
        TOUCH_BUF(dst, true);
        if (bad) break;
        VkBufferCopy region = {.srcOffset = c.src_offset, .dstOffset = c.dst_offset, .size = c.size};
        vkCmdCopyBuffer(w->cmd_buf, src->buffer, dst->buffer, 1, &region);
        emit_full_barrier(w->cmd_buf);
        any_gpu_cmd = true;
      }
      break;
    }

    case KMTL_CMD_BLIT_FILL_BUFFER: {
      if (!in_blit || hdr.size < sizeof(struct kmtl_cmd_blit_fill)) { bad = true; break; }
      struct kmtl_cmd_blit_fill c;
      memcpy(&c, rec, sizeof(c));
      vk_buffer_t *dst = find_buffer(w, c.dst_handle, req->client_id);
      if (!dst || c.offset > dst->aligned_len || c.size > dst->aligned_len - c.offset) {
        bad = true;
        break;
      }
      if (c.size > 0) {
        TOUCH_BUF(dst, true);
        if (bad) break;
        if ((c.offset & 3ULL) == 0 && (c.size & 3ULL) == 0) {
          uint32_t v32 = (uint32_t)c.value * 0x01010101u;
          vkCmdFillBuffer(w->cmd_buf, dst->buffer, c.offset, c.size, v32);
          emit_full_barrier(w->cmd_buf);
          any_gpu_cmd = true;
        } else {
          uint64_t remaining = c.size;
          uint64_t dst_off = c.offset;
          while (remaining > 0) {
            uint64_t off = align_up(staging_cursor, 4);
            if (off >= KMTL_STAGING_BYTES) { bad = true; bad_reason = "staging full for unaligned fill"; break; }
            uint64_t chunk = KMTL_STAGING_BYTES - off;
            if (chunk > remaining) chunk = remaining;
            memset(w->staging_ptr + off, c.value, (size_t)chunk);
            VkBufferCopy r = {.srcOffset = off, .dstOffset = dst_off, .size = chunk};
            vkCmdCopyBuffer(w->cmd_buf, w->staging_buf, dst->buffer, 1, &r);
            staging_cursor = off + chunk;
            dst_off += chunk;
            remaining -= chunk;
            any_gpu_cmd = true;
          }
          if (!bad) emit_full_barrier(w->cmd_buf);
        }
      }
      break;
    }

    default:
      bad = true;
      break;
    }
  }
  #undef TOUCH_BUF

  if (vkEndCommandBuffer(w->cmd_buf) != VK_SUCCESS) {
    bad = true;
    bad_reason = "vkEndCommandBuffer failed";
  }
  if (bad) {
    vkResetCommandBuffer(w->cmd_buf, 0);
    for (size_t i = 0; i < n_deferred_pipes; i++) {
      vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
    }
    return send_err(w->sock_fd, KMTL_ERR_INVAL, bad_reason, out_cap);
  }

  /* Pre-submit CPU->GPU coherency sync over full aligned_len */
  for (size_t i = 0; i < n_touched; i++) {
    vk_buffer_t *b = touched[i];
    if (b->is_dmabuf) {
#if defined(__aarch64__)
      if (w->shm_base) {
        if (touched_written[i]) {
          arm64_clean_inv_dcache_poc(w->shm_base + b->shm_offset, (size_t)b->aligned_len);
        } else {
          arm64_clean_dcache_poc(w->shm_base + b->shm_offset, (size_t)b->aligned_len);
        }
      }
#else
      dmabuf_sync(b->dmabuf_fd, DMA_BUF_SYNC_END | DMA_BUF_SYNC_RW);
#endif
    } else if (b->mapped && w->shm_base) {
      memcpy(b->mapped, w->shm_base + b->shm_offset, (size_t)b->aligned_len);
    }
  }

  uint64_t t0 = monotonic_ns();
  if (any_gpu_cmd) {
    vkResetFences(w->device, 1, &w->fence);
    VkSubmitInfo si = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1,
        .pCommandBuffers = &w->cmd_buf,
    };
    VkResult vr = vkQueueSubmit(w->queue, 1, &si, w->fence);
    if (vr != VK_SUCCESS) {
      for (size_t i = 0; i < n_deferred_pipes; i++) {
        vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
      }
      send_err(w->sock_fd, KMTL_ERR_GPU, "vkQueueSubmit failed", out_cap);
      return false;
    }
    vr = vkWaitForFences(w->device, 1, &w->fence, VK_TRUE, 30000000000ULL);
    if (vr != VK_SUCCESS) {
      /* Never reuse or destroy Vulkan objects while GPU execution may still be in flight. */
      send_err(w->sock_fd, KMTL_ERR_GPU, "vkWaitForFences failed or timed out", out_cap);
      return false;
    }
  }
  uint64_t t1 = monotonic_ns();

  for (size_t i = 0; i < n_deferred_pipes; i++) {
    vkDestroyPipeline(w->device, deferred_pipes[i], NULL);
  }

  /* Post-submit GPU->CPU coherency sync over full aligned_len */
  for (size_t i = 0; i < n_touched; i++) {
    vk_buffer_t *b = touched[i];
    if (b->is_dmabuf) {
#if defined(__aarch64__)
      if (touched_written[i] && w->shm_base) {
        arm64_clean_inv_dcache_poc(w->shm_base + b->shm_offset, (size_t)b->aligned_len);
      }
#else
      dmabuf_sync(b->dmabuf_fd, DMA_BUF_SYNC_START | DMA_BUF_SYNC_RW);
#endif
    } else if (touched_written[i] && b->mapped && w->shm_base) {
      memcpy(w->shm_base + b->shm_offset, b->mapped, (size_t)b->aligned_len);
    }
  }

  uint64_t seq = ++q->submitted_seq;
  q->completed_seq = seq;
  q->last_start_ns = t0;
  q->last_end_ns = t1;
  q->timings[seq & 63u].seq = seq;
  q->timings[seq & 63u].gpu_start_ns = t0;
  q->timings[seq & 63u].gpu_end_ns = t1;

  struct kmtl_resp_hdr resp;
  memset(&resp, 0, sizeof(resp));
  resp.status = KMTL_OK;
  if (req->arg0 & KMTL_COMMIT_WAIT_COMPLETED) {
    resp.arg0 = t0;
    resp.arg1 = t1;
  } else {
    resp.arg0 = seq;
  }
  return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
}

static bool handle_rpc(worker_t *w,
                       uint8_t frame_kind,
                       const struct kmtl_req_hdr *req,
                       const uint8_t *in_payload,
                       uint32_t in_len,
                       uint32_t out_cap) {
  if (frame_kind == KMTL_FRAME_VMM_RESET) {
    reset_all(w);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }
  if (frame_kind != KMTL_FRAME_GUEST_RPC) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "unknown frame kind", out_cap);
  }
  if (req->magic != KMTL_MAGIC || req->version != KMTL_PROTO_VERSION) {
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "bad magic or protocol version", out_cap);
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
    info.gpu_family_mask = KMTL_FAM_COMMON1 | KMTL_FAM_COMMON2 | KMTL_FAM_COMMON3 | KMTL_FAM_METAL3;
    info.max_threads_per_threadgroup[0] = w->props.limits.maxComputeWorkGroupSize[0];
    info.max_threads_per_threadgroup[1] = w->props.limits.maxComputeWorkGroupSize[1];
    info.max_threads_per_threadgroup[2] = w->props.limits.maxComputeWorkGroupSize[2];
    info.max_threadgroup_memory_length = w->props.limits.maxComputeSharedMemorySize;
    info.host_page_size = w->host_page_size;
    info.flags = KMTL_FEAT_UNIFIED_MEMORY;
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, &info, (uint32_t)sizeof(info), out_cap);
  }

  case KMTL_OP_CLIENT_OPEN: {
    if (req->client_id == 0) return send_err(w->sock_fd, KMTL_ERR_INVAL, "client_id must be non-zero", out_cap);
    if (has_client(w, req->client_id)) close_client(w, req->client_id);
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
    close_client(w, req->client_id);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_BUFFER: {
    if (!has_client(w, req->client_id)) return send_err(w->sock_fd, KMTL_ERR_PERM, "unknown client", out_cap);
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
    if (w->used_bytes > w->shm_size - aligned_len) {
      return send_err(w->sock_fd, KMTL_ERR_NOMEM, "GPU memory quota exhausted", out_cap);
    }
    uint64_t shm_off = 0;
    if (!kmtl_shm_alloc(&w->shm_alloc, aligned_len, &shm_off)) {
      return send_err(w->sock_fd, KMTL_ERR_NOSPC, "GPU SHM aperture full or fragmented", out_cap);
    }

    if (w->shm_base) memset(w->shm_base + shm_off, 0, (size_t)aligned_len);

    VkBuffer buf = VK_NULL_HANDLE;
    VkDeviceMemory mem = VK_NULL_HANDLE;
    void *mapped = NULL;
    int dmabuf_fd = -1;
    bool is_dmabuf = try_alloc_dmabuf_vk_buffer(w, shm_off, aligned_len, &buf, &mem, &dmabuf_fd);
    if (!is_dmabuf) {
      if (!alloc_host_visible_vk_buffer(w, aligned_len, &buf, &mem, &mapped)) {
        kmtl_shm_free(&w->shm_alloc, shm_off, aligned_len);
        return send_err(w->sock_fd, KMTL_ERR_NOMEM, "Vulkan buffer allocation failed", out_cap);
      }
    }
    if (!w->logged_buf_mode) {
      w->logged_buf_mode = true;
      fprintf(stderr, "kido-vk-worker: buffer mode = %s\n",
              is_dmabuf ? "ZERO-COPY udmabuf + VK_EXT_external_memory_dma_buf"
                        : "HOST_VISIBLE | HOST_COHERENT sync");
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
        kmtl_shm_free(&w->shm_alloc, shm_off, aligned_len);
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
    slot->handle = handle;
    slot->client_id = req->client_id;
    slot->shm_offset = shm_off;
    slot->aligned_len = aligned_len;
    slot->requested_len = req_len;
    slot->is_dmabuf = is_dmabuf;
    slot->dmabuf_fd = dmabuf_fd;
    slot->buffer = buf;
    slot->memory = mem;
    slot->mapped = mapped;
    w->used_bytes += aligned_len;

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

    char names_buf[8192];
    size_t names_len = msl_extract_kernel_names(src, co.source_len, names_buf, sizeof(names_buf));

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    return send_resp(w->sock_fd, resp, names_buf, (uint32_t)names_len, out_cap);

  bad_macros:
    free(src);
    if (mkeys) { for (uint32_t i = 0; i < co.num_macros; i++) free(mkeys[i]); }
    if (mvals) { for (uint32_t i = 0; i < co.num_macros; i++) free(mvals[i]); }
    free(mkeys); free(mvals); free(macros);
    return send_err(w->sock_fd, KMTL_ERR_INVAL, "malformed compile macros", out_cap);
  }

  case KMTL_OP_CREATE_LIBRARY_DATA:
    return send_err(w->sock_fd, KMTL_ERR_UNSUPP, "Vulkan backend requires MSL source (newLibraryWithSource:)", out_cap);

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
            fc.index > 65535u || (fc.index >= 1000u && fc.index <= 1002u)) {
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

    msl_translate_result_t tr;
    char err[1024] = {0};
    if (!msl_translate_kernel(lib->source, lib->source_len, lib->macros, lib->num_macros, fn_name, &tr, err,
                              sizeof(err))) {
      free(consts);
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, err, out_cap);
    }

    uint32_t *spirv = NULL;
    size_t spirv_bytes = 0;
    if (!compile_glsl_to_spirv(tr.glsl, tr.glsl_len, &spirv, &spirv_bytes, err, sizeof(err))) {
      fprintf(stderr, "kido-vk-worker: GLSL compile error for %s: %.512s\n", fn_name, err);
      if (getenv("KIDO_VK_DEBUG_SHADER") != NULL) {
        fprintf(stderr, "--- GLSL ---\n%.4096s\n", tr.glsl);
      }
      free(tr.glsl);
      free(consts);
      return send_err(w->sock_fd, KMTL_ERR_COMPILE, err, out_cap);
    }
    free(tr.glsl);

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
    fn->binding_mask = tr.binding_mask;
    fn->readonly_mask = tr.readonly_mask;
    fn->consts = consts;
    fn->num_consts = fr.num_constants;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_DESTROY_FUNCTION: {
    vk_function_t *f = find_function(w, req->handle, req->client_id);
    if (!f) return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown function handle", out_cap);
    destroy_function_slot(f);
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_CREATE_COMPUTE_PIPELINE: {
    vk_function_t *fn = find_function(w, req->handle, req->client_id);
    if (!has_client(w, req->client_id) || !fn) {
      return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown function handle", out_cap);
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

    VkPipelineLayoutCreateInfo plci = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
        .setLayoutCount = 1,
        .pSetLayouts = &dsl,
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
    p->handle = handle;
    p->client_id = req->client_id;
    p->shader = shader;
    p->ds_layout = dsl;
    p->pipe_layout = pl;
    p->binding_mask = fn->binding_mask;
    p->readonly_mask = fn->readonly_mask;
    p->max_tg = max_tg;
    p->consts = consts_copy;
    p->num_consts = fn->num_consts;
    p->num_specs = 0;

    struct kmtl_resp_hdr resp = {0};
    resp.status = KMTL_OK;
    resp.handle = handle;
    resp.arg0 = ((uint64_t)max_tg & 0xFFFFFFFFULL) | (((uint64_t)w->subgroup_size & 0xFFFFFFFFULL) << 32);
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
    memset(q, 0, sizeof(*q));
    struct kmtl_resp_hdr resp = {0};
    return send_resp(w->sock_fd, resp, NULL, 0, out_cap);
  }

  case KMTL_OP_COMMIT_COMMAND_BUFFER:
    return handle_commit(w, req, in_payload, in_len, out_cap);

  case KMTL_OP_WAIT_QUEUE: {
    vk_queue_t *q = find_queue(w, req->handle, req->client_id);
    if (!has_client(w, req->client_id) || !q) {
      return send_err(w->sock_fd, KMTL_ERR_NOENT, "unknown queue handle", out_cap);
    }
    uint64_t target = req->arg0 ? req->arg0 : q->submitted_seq;
    if (target > q->submitted_seq) {
      return send_err(w->sock_fd, KMTL_ERR_INVAL, "wait sequence exceeds submitted command buffers", out_cap);
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
  w.shm_base = mmap(NULL, w.shm_size, PROT_READ | PROT_WRITE, MAP_SHARED, w.shm_fd, 0);
  if (w.shm_base == MAP_FAILED) {
    perror("mmap");
    return 1;
  }
  if (!vk_init(&w)) return 1;

  VkBuffer buf = VK_NULL_HANDLE;
  VkDeviceMemory mem = VK_NULL_HANDLE;
  int dfd = -1;
  bool dmabuf_ok = try_alloc_dmabuf_vk_buffer(&w, 0, 65536, &buf, &mem, &dfd);
  printf("selftest: GPU=%s dmabuf_zero_copy=%s\n", w.props.deviceName, dmabuf_ok ? "YES" : "NO (fallback)");
  if (dmabuf_ok) {
    vkDestroyBuffer(w.device, buf, NULL);
    vkFreeMemory(w.device, mem, NULL);
    close(dfd);
  }

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
  if (!compile_glsl_to_spirv(tr.glsl, tr.glsl_len, &spv, &spv_len, err, sizeof(err))) {
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

  /* Ensure sock_fd and shm_fd are never inherited by glslangValidator child processes. */
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
  w.shm_size = shm_bytes;
  w.host_page_size = (uint32_t)getpagesize();
  if (w.host_page_size < KMTL_DARWIN_PAGE_SIZE) w.host_page_size = KMTL_DARWIN_PAGE_SIZE;
  w.next_handle = 1;

  w.shm_base = mmap(NULL, (size_t)shm_bytes, PROT_READ | PROT_WRITE, MAP_SHARED, shm_fd, 0);
  if (w.shm_base == MAP_FAILED) {
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

  reset_all(&w);
  vk_destroy_device_objects(&w);
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
