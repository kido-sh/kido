/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define AIR2SPV_SUBGROUP_BASIC (1u << 0)
#define AIR2SPV_SUBGROUP_VOTE (1u << 1)
#define AIR2SPV_SUBGROUP_ARITHMETIC (1u << 2)
#define AIR2SPV_SUBGROUP_BALLOT (1u << 3)
#define AIR2SPV_SUBGROUP_SHUFFLE (1u << 4)
#define AIR2SPV_SUBGROUP_SHUFFLE_RELATIVE (1u << 5)

typedef struct {
  bool has_float16;
  bool has_int16;
  bool has_int8;
  bool has_storage_16bit;
  bool has_storage_8bit;
  bool has_int64;
  uint32_t max_workgroup_memory;
  uint32_t subgroup_ops; /* Bitmask of AIR2SPV_SUBGROUP_* (matches VkSubgroupFeatureFlagBits). */
  uint32_t subgroup_size; /* Hardware Vulkan subgroupSize (e.g. 16 on Mali/V3D -> virtual 32-wide emulation). */
} air2spv_caps_t;

typedef struct {
  uint32_t *spirv;
  size_t spirv_bytes;
  uint32_t binding_mask;
  uint32_t readonly_mask;
} air2spv_result_t;

/*
 * Compile a single Apple AIR LLVM bitcode module directly into validated
 * Vulkan 1.3 SPIR-V words in memory.
 * On success returns true and sets out->spirv (caller must free(out->spirv)).
 * On failure returns false and writes a diagnostic into err_buf.
 */
bool air2spv_compile_bitcode(const uint8_t *bc,
                             size_t bc_len,
                             const char *kernel_name,
                             const air2spv_caps_t *caps,
                             air2spv_result_t *out,
                             char *err_buf,
                             size_t err_cap);

/*
 * Parse a Metal library archive (.metallib) and compile the named compute
 * kernel directly to Vulkan 1.3 SPIR-V in memory.
 */
bool air2spv_compile_metallib(const uint8_t *mlib,
                              size_t mlib_len,
                              const char *kernel_name,
                              const air2spv_caps_t *caps,
                              air2spv_result_t *out,
                              char *err_buf,
                              size_t err_cap);

#ifdef __cplusplus
}
#endif
