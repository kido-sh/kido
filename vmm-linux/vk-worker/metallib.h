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

#define METALLIB_MAX_BYTES (32u * 1024u * 1024u)
#define METALLIB_MAX_FUNCS 4096u
#define METALLIB_MAX_BITCODE_BYTES (16u * 1024u * 1024u)
#define METALLIB_MAX_NAME_LEN 127u

#define METALLIB_FN_VERTEX 0u
#define METALLIB_FN_FRAGMENT 1u
#define METALLIB_FN_COMPUTE 2u

typedef struct {
  char name[128];
  uint8_t fn_type;
  const uint8_t *bitcode; /* Non-owning pointer into the parsed metallib buffer. */
  size_t bitcode_len;
} metallib_func_t;

typedef struct {
  metallib_func_t *funcs;
  size_t count;
} metallib_index_t;

/*
 * Parse a Metal library archive (MTLB container or Mach-O universal/FAT wrapper
 * containing an MTLB slice) and extract validated LLVM bitcode slices.
 * Pointers in out->funcs[i].bitcode alias the caller's `data` buffer.
 * Caller must release with metallib_index_free(out).
 */
bool metallib_parse(const uint8_t *data,
                    size_t len,
                    metallib_index_t *out,
                    char *err_buf,
                    size_t err_cap);

void metallib_index_free(metallib_index_t *idx);

/*
 * Encode NUL-separated compute kernel names into out_buf (same wire format as
 * msl_extract_kernel_names). When out_buf is NULL, returns the exact byte
 * length required to hold all compute kernel names.
 */
size_t metallib_encode_compute_names(const metallib_index_t *idx, char *out_buf, size_t out_cap);

/* Find a compute kernel entry by exact name, or NULL if absent. */
const metallib_func_t *metallib_find_compute(const metallib_index_t *idx, const char *name);

#ifdef __cplusplus
}
#endif
