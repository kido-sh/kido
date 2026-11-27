/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once

#include <stdint.h>

#define KMTL_VIRTIO_DEVICE_ID 0x50u
#define KMTL_PCI_VENDOR_ID 0x1af4u
#define KMTL_PCI_DEVICE_ID (0x1040u + KMTL_VIRTIO_DEVICE_ID)
#define KMTL_PCI_CLASS_ID 0x03u
#define KMTL_PCI_SUBCLASS_ID 0x02u

#define KMTL_MAGIC 0x4B4D544Cu
#define KMTL_PROTO_VERSION 1u
#define KMTL_SHM_REGION_ID 0u

#define KMTL_MAX_IN_PAYLOAD (64u * 1024u * 1024u)
#define KMTL_MAX_CMD_PAYLOAD (16u * 1024u * 1024u)
#define KMTL_MAX_OUT_PAYLOAD (1024u * 1024u)
#define KMTL_MAX_SET_BYTES 4096u
#define KMTL_MAX_BUFFER_SLOTS 31u

#define KMTL_MAX_BUFFERS_PER_CLIENT 4096u
#define KMTL_MAX_LIBS_PER_CLIENT 256u
#define KMTL_MAX_FNS_PER_CLIENT 2048u
#define KMTL_MAX_PSOS_PER_CLIENT 2048u
#define KMTL_MAX_QUEUES_PER_CLIENT 64u

#define KMTL_FEAT_UNIFIED_MEMORY (1u << 0)
#define KMTL_FEAT_NATIVE_METALLIB (1u << 1)
#define KMTL_FEAT_BFLOAT16 (1u << 2)
#define KMTL_FEAT_SIMDGROUP_MATRIX (1u << 3)

#define KMTL_COMMIT_WAIT_COMPLETED (1ULL << 0)
#define KMTL_COMMIT_WAIT_SCHEDULED (1ULL << 1)

/* GPU family bitmask matching Apple MTLGPUFamily enum slots */
#define KMTL_FAM_APPLE1 (1ULL << 0)
#define KMTL_FAM_APPLE2 (1ULL << 1)
#define KMTL_FAM_APPLE3 (1ULL << 2)
#define KMTL_FAM_APPLE4 (1ULL << 3)
#define KMTL_FAM_APPLE5 (1ULL << 4)
#define KMTL_FAM_APPLE6 (1ULL << 5)
#define KMTL_FAM_APPLE7 (1ULL << 6)
#define KMTL_FAM_APPLE8 (1ULL << 7)
#define KMTL_FAM_APPLE9 (1ULL << 8)
#define KMTL_FAM_APPLE10 (1ULL << 9)
#define KMTL_FAM_COMMON1 (1ULL << 16)
#define KMTL_FAM_COMMON2 (1ULL << 17)
#define KMTL_FAM_COMMON3 (1ULL << 18)
#define KMTL_FAM_MAC2 (1ULL << 20)
#define KMTL_FAM_METAL3 (1ULL << 24)
#define KMTL_FAM_METAL4 (1ULL << 25)

enum kmtl_opcode {
  KMTL_OP_GET_DEVICE_INFO = 1,
  KMTL_OP_CLIENT_OPEN = 2,
  KMTL_OP_CLIENT_CLOSE = 3,

  KMTL_OP_CREATE_BUFFER = 10,
  KMTL_OP_DESTROY_BUFFER = 11,

  KMTL_OP_CREATE_LIBRARY_SOURCE = 20,
  KMTL_OP_CREATE_LIBRARY_DATA = 21,
  KMTL_OP_DESTROY_LIBRARY = 22,
  KMTL_OP_CREATE_FUNCTION = 23,
  KMTL_OP_DESTROY_FUNCTION = 24,
  KMTL_OP_CREATE_COMPUTE_PIPELINE = 25,
  KMTL_OP_DESTROY_PIPELINE = 26,

  KMTL_OP_CREATE_QUEUE = 30,
  KMTL_OP_DESTROY_QUEUE = 31,

  KMTL_OP_COMMIT_COMMAND_BUFFER = 40,
  KMTL_OP_WAIT_QUEUE = 41,
};

enum kmtl_status {
  KMTL_OK = 0,
  KMTL_ERR_INVAL = -22,
  KMTL_ERR_NOMEM = -12,
  KMTL_ERR_NOENT = -2,
  KMTL_ERR_PERM = -1,
  KMTL_ERR_NOSPC = -28,
  KMTL_ERR_IO = -5,
  KMTL_ERR_COMPILE = -100,
  KMTL_ERR_GPU = -101,
  KMTL_ERR_UNSUPP = -95,
};

enum kmtl_cmd_op {
  KMTL_CMD_BEGIN_COMPUTE = 1,
  KMTL_CMD_END_ENCODER = 2,
  KMTL_CMD_SET_PIPELINE = 3,
  KMTL_CMD_SET_BUFFER = 4,
  KMTL_CMD_SET_BUFFER_OFFSET = 5,
  KMTL_CMD_SET_BYTES = 6,
  KMTL_CMD_SET_TG_MEM = 7,
  KMTL_CMD_DISPATCH_THREADGROUPS = 8,
  KMTL_CMD_DISPATCH_THREADS = 9,
  KMTL_CMD_DISPATCH_INDIRECT = 10,
  KMTL_CMD_MEMORY_BARRIER = 11,

  KMTL_CMD_BEGIN_BLIT = 20,
  KMTL_CMD_BLIT_COPY_BUFFER = 21,
  KMTL_CMD_BLIT_FILL_BUFFER = 22,
};

/* VirtIO device-specific PCI configuration space (64 bytes). */
struct kmtl_device_config {
  uint32_t magic;
  uint32_t version;
  uint32_t host_page_size;
  uint32_t flags;
  uint64_t max_buffer_length;
  uint64_t recommended_max_working_set_size;
  uint64_t shm_aperture_bytes;
  uint32_t max_threads_per_threadgroup[3];
  uint32_t max_threadgroup_memory_length;
  uint64_t registry_id;
};

/* Request header placed at the start of every VQ0 readable chain (40 bytes). */
struct kmtl_req_hdr {
  uint32_t magic;
  uint32_t version;
  uint32_t opcode;
  uint32_t client_id;
  uint64_t handle;
  uint64_t arg0;
  uint64_t arg1;
};

/* Response header placed at the start of every VQ0 writable chain (32 bytes). */
struct kmtl_resp_hdr {
  int32_t status;
  uint32_t extra_len;
  uint64_t handle;
  uint64_t arg0;
  uint64_t arg1;
};

/* Returned as extra payload for KMTL_OP_GET_DEVICE_INFO (256 bytes). */
struct kmtl_device_info {
  char name[128];
  uint64_t registry_id;
  uint64_t max_buffer_length;
  uint64_t recommended_max_working_set_size;
  uint64_t shm_aperture_bytes;
  uint64_t gpu_family_mask;
  uint32_t max_threads_per_threadgroup[3];
  uint32_t max_threadgroup_memory_length;
  uint32_t host_page_size;
  uint32_t flags;
  uint32_t reserved[16];
};

/* Compile options prefix for KMTL_OP_CREATE_LIBRARY_SOURCE (24 bytes). */
struct kmtl_compile_opts {
  uint32_t fast_math;
  uint32_t language_version;
  uint32_t source_len;
  uint32_t num_macros;
  uint32_t reserved[2];
};

/* Macro entry after source bytes in KMTL_OP_CREATE_LIBRARY_SOURCE: key_len, val_len, key, val. */
struct kmtl_macro_hdr {
  uint32_t key_len;
  uint32_t val_len;
};

/* Header for KMTL_OP_CREATE_FUNCTION payload (16 bytes). */
struct kmtl_function_req {
  uint32_t name_len;
  uint32_t num_constants;
  uint32_t reserved[2];
};

/* Serialized MTLFunctionConstantValues entry (16 bytes + value_len bytes padded to 8B). */
struct kmtl_func_const {
  uint32_t index;
  uint32_t data_type;
  uint32_t value_len;
  uint32_t reserved;
};

/* Every command inside KMTL_OP_COMMIT_COMMAND_BUFFER starts with this 8-byte header. */
struct kmtl_cmd_hdr {
  uint16_t op;
  uint16_t flags;
  uint32_t size;
};

struct kmtl_cmd_set_pipeline {
  struct kmtl_cmd_hdr hdr;
  uint64_t pso_handle;
};

struct kmtl_cmd_set_buffer {
  struct kmtl_cmd_hdr hdr;
  uint32_t index;
  uint32_t reserved;
  uint64_t buf_handle;
  uint64_t offset;
};

struct kmtl_cmd_set_buffer_offset {
  struct kmtl_cmd_hdr hdr;
  uint32_t index;
  uint32_t reserved;
  uint64_t offset;
};

struct kmtl_cmd_set_bytes {
  struct kmtl_cmd_hdr hdr;
  uint32_t index;
  uint32_t length;
};

struct kmtl_cmd_set_tg_mem {
  struct kmtl_cmd_hdr hdr;
  uint32_t index;
  uint32_t length;
};

struct kmtl_cmd_dispatch {
  struct kmtl_cmd_hdr hdr;
  uint64_t gx, gy, gz;
  uint64_t tx, ty, tz;
};

struct kmtl_cmd_dispatch_indirect {
  struct kmtl_cmd_hdr hdr;
  uint64_t buf_handle;
  uint64_t offset;
  uint64_t tx, ty, tz;
};

struct kmtl_cmd_barrier {
  struct kmtl_cmd_hdr hdr;
  uint32_t scope;
  uint32_t reserved;
};

struct kmtl_cmd_blit_copy {
  struct kmtl_cmd_hdr hdr;
  uint64_t src_handle;
  uint64_t src_offset;
  uint64_t dst_handle;
  uint64_t dst_offset;
  uint64_t size;
};

struct kmtl_cmd_blit_fill {
  struct kmtl_cmd_hdr hdr;
  uint64_t dst_handle;
  uint64_t offset;
  uint64_t size;
  uint8_t value;
  uint8_t pad[7];
};

/* IOUserClient externalMethod selector 0 input structure (64 bytes). */
#define KMTL_UC_SELECTOR_RPC 0u

struct kmtl_uc_call {
  struct kmtl_req_hdr req;
  uint64_t in_ptr;
  uint32_t in_len;
  uint32_t out_cap;
  uint64_t out_ptr;
};
