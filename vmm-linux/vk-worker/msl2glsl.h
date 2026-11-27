/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
  const char *key;
  size_t key_len;
  const char *val;
  size_t val_len;
} msl_macro_t;

typedef struct {
  char *glsl;
  size_t glsl_len;
  uint32_t binding_mask;
  uint32_t readonly_mask;
} msl_translate_result_t;

/*
 * Scan MSL source for `kernel void <name>(` declarations and encode NUL-separated
 * names into out_buf (matching encodeFunctionNames in vmm/metal.m).
 */
size_t msl_extract_kernel_names(const char *src, size_t src_len, char *out_buf, size_t out_cap);

/*
 * Translate a single named compute kernel from an MSL library into a GLSL 450
 * compute shader using specialization constants 1000/1001/1002 for local_size.
 * On success returns true and sets out->glsl (caller must free(out->glsl)).
 * On failure returns false and writes a diagnostic into err_buf.
 */
bool msl_translate_kernel(const char *src,
                          size_t src_len,
                          const msl_macro_t *macros,
                          size_t num_macros,
                          const char *kernel_name,
                          msl_translate_result_t *out,
                          char *err_buf,
                          size_t err_cap);
