/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
  uint64_t offset;
  uint64_t length;
} kmtl_free_span_t;

typedef struct {
  kmtl_free_span_t *spans;
  size_t count;
  size_t cap;
  uint64_t total_size;
} kmtl_shm_alloc_t;

bool kmtl_shm_init(kmtl_shm_alloc_t *a, uint64_t total_size);
void kmtl_shm_destroy(kmtl_shm_alloc_t *a);
bool kmtl_shm_alloc(kmtl_shm_alloc_t *a, uint64_t len, uint64_t *out_offset);
bool kmtl_shm_free(kmtl_shm_alloc_t *a, uint64_t offset, uint64_t len);

/* Returns exact byte size expected by MTLFunctionConstantValues, or 0 if unsupported. */
uint32_t kmtl_func_const_size(uint32_t data_type);

/* Returns true if language_version is 0 (default) or a valid Metal language version enum. */
bool kmtl_valid_language_version(uint32_t lv);
