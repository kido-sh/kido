/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "kmtl_shm.h"

#include <stdlib.h>
#include <string.h>

bool kmtl_shm_init(kmtl_shm_alloc_t *a, uint64_t total_size) {
  if (!a || total_size == 0) return false;
  memset(a, 0, sizeof(*a));
  size_t cap = 64;
  kmtl_free_span_t *spans = calloc(cap, sizeof(*spans));
  if (!spans) return false;
  spans[0].offset = 0;
  spans[0].length = total_size;
  a->spans = spans;
  a->count = 1;
  a->cap = cap;
  a->total_size = total_size;
  return true;
}

void kmtl_shm_destroy(kmtl_shm_alloc_t *a) {
  if (!a) return;
  free(a->spans);
  memset(a, 0, sizeof(*a));
}

bool kmtl_shm_alloc(kmtl_shm_alloc_t *a, uint64_t len, uint64_t *out_offset) {
  if (!a || !a->spans || len == 0 || !out_offset) return false;
  for (size_t i = 0; i < a->count; i++) {
    if (a->spans[i].length >= len) {
      *out_offset = a->spans[i].offset;
      if (a->spans[i].length == len) {
        memmove(&a->spans[i], &a->spans[i + 1], (a->count - i - 1) * sizeof(kmtl_free_span_t));
        a->count--;
      } else {
        a->spans[i].offset += len;
        a->spans[i].length -= len;
      }
      return true;
    }
  }
  return false;
}

bool kmtl_shm_free(kmtl_shm_alloc_t *a, uint64_t offset, uint64_t len) {
  if (!a || !a->spans || len == 0) return false;
  if (offset > a->total_size || len > a->total_size - offset) return false;
  uint64_t end = offset + len;

  size_t pos = 0;
  while (pos < a->count && a->spans[pos].offset < offset) pos++;

  if (pos > 0) {
    uint64_t prev_end = a->spans[pos - 1].offset + a->spans[pos - 1].length;
    if (prev_end > offset) return false; /* Overlap / double free */
  }
  if (pos < a->count && end > a->spans[pos].offset) {
    return false; /* Overlap / double free */
  }

  bool merge_prev = (pos > 0 && a->spans[pos - 1].offset + a->spans[pos - 1].length == offset);
  bool merge_next = (pos < a->count && end == a->spans[pos].offset);

  if (merge_prev && merge_next) {
    a->spans[pos - 1].length += len + a->spans[pos].length;
    memmove(&a->spans[pos], &a->spans[pos + 1], (a->count - pos - 1) * sizeof(kmtl_free_span_t));
    a->count--;
    return true;
  }
  if (merge_prev) {
    a->spans[pos - 1].length += len;
    return true;
  }
  if (merge_next) {
    a->spans[pos].offset = offset;
    a->spans[pos].length += len;
    return true;
  }

  if (a->count == a->cap) {
    if (a->cap > SIZE_MAX / 2 / sizeof(kmtl_free_span_t)) return false;
    size_t new_cap = a->cap * 2;
    kmtl_free_span_t *grown = realloc(a->spans, new_cap * sizeof(kmtl_free_span_t));
    if (!grown) return false;
    a->spans = grown;
    a->cap = new_cap;
  }
  memmove(&a->spans[pos + 1], &a->spans[pos], (a->count - pos) * sizeof(kmtl_free_span_t));
  a->spans[pos].offset = offset;
  a->spans[pos].length = len;
  a->count++;
  return true;
}

uint32_t kmtl_func_const_size(uint32_t data_type) {
  switch (data_type) {
  case 53: case 45: case 49: return 1; /* Bool, Char, UChar */
  case 54: case 46: case 50: return 2; /* Bool2, Char2, UChar2 */
  case 55: case 47: case 51: return 4; /* Bool3, Char3, UChar3 */
  case 56: case 48: case 52: return 4; /* Bool4, Char4, UChar4 */

  case 37: case 41: case 16: case 121: return 2; /* Short, UShort, Half, BFloat */
  case 38: case 42: case 17: case 122: return 4; /* Short2, UShort2, Half2, BFloat2 */
  case 39: case 43: case 18: case 123: return 8; /* Short3, UShort3, Half3, BFloat3 */
  case 40: case 44: case 19: case 124: return 8; /* Short4, UShort4, Half4, BFloat4 */

  case 29: case 33: case 3: return 4;   /* Int, UInt, Float */
  case 30: case 34: case 4: return 8;   /* Int2, UInt2, Float2 */
  case 31: case 35: case 5: return 16;  /* Int3, UInt3, Float3 */
  case 32: case 36: case 6: return 16;  /* Int4, UInt4, Float4 */

  case 81: case 85: return 8;           /* Long, ULong */
  case 82: case 86: return 16;          /* Long2, ULong2 */
  case 83: case 87: return 32;          /* Long3, ULong3 */
  case 84: case 88: return 32;          /* Long4, ULong4 */

  default: return 0;
  }
}

bool kmtl_valid_language_version(uint32_t lv) {
  switch (lv) {
  case 0:
  case (1u << 16) + 0u:
  case (1u << 16) + 1u:
  case (1u << 16) + 2u:
  case (2u << 16) + 0u:
  case (2u << 16) + 1u:
  case (2u << 16) + 2u:
  case (2u << 16) + 3u:
  case (2u << 16) + 4u:
  case (3u << 16) + 0u:
  case (3u << 16) + 1u:
  case (3u << 16) + 2u:
  case (4u << 16) + 0u:
    return true;
  default:
    return false;
  }
}
