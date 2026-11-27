/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "kmtl_shm.h"

#include <assert.h>
#include <stdio.h>

int main(void) {
  kmtl_shm_alloc_t a;
  assert(!kmtl_shm_init(&a, 0));
  assert(kmtl_shm_init(&a, 1024 * 1024));

  uint64_t o1 = 0, o2 = 0, o3 = 0;
  assert(!kmtl_shm_alloc(&a, 0, &o1));
  assert(!kmtl_shm_alloc(&a, 2 * 1024 * 1024, &o1));

  assert(kmtl_shm_alloc(&a, 256 * 1024, &o1) && o1 == 0);
  assert(kmtl_shm_alloc(&a, 256 * 1024, &o2) && o2 == 256 * 1024);
  assert(kmtl_shm_alloc(&a, 512 * 1024, &o3) && o3 == 512 * 1024);
  assert(a.count == 0);

  /* Double-free / out-of-bounds rejection */
  assert(!kmtl_shm_free(&a, 0, 0));
  assert(!kmtl_shm_free(&a, 1024 * 1024, 16384));
  assert(!kmtl_shm_free(&a, 1024 * 1024 - 4096, 8192));

  /* Free middle, reject overlap, then coalesce all three */
  assert(kmtl_shm_free(&a, o2, 256 * 1024));
  assert(!kmtl_shm_free(&a, o2, 256 * 1024));
  assert(!kmtl_shm_free(&a, o2 - 4096, 8192));
  assert(!kmtl_shm_free(&a, o2 + 256 * 1024 - 4096, 8192));

  assert(kmtl_shm_free(&a, o1, 256 * 1024));
  assert(a.count == 1 && a.spans[0].offset == 0 && a.spans[0].length == 512 * 1024);
  assert(kmtl_shm_free(&a, o3, 512 * 1024));
  assert(a.count == 1 && a.spans[0].offset == 0 && a.spans[0].length == 1024 * 1024);

  /* Fragment past initial 64-entry cap to exercise realloc growth and full coalescing */
  uint64_t offs[256];
  for (int i = 0; i < 256; i++) {
    assert(kmtl_shm_alloc(&a, 4096, &offs[i]));
  }
  for (int i = 0; i < 256; i += 2) {
    assert(kmtl_shm_free(&a, offs[i], 4096));
  }
  assert(a.count == 128);
  for (int i = 1; i < 256; i += 2) {
    assert(kmtl_shm_free(&a, offs[i], 4096));
  }
  assert(a.count == 1 && a.spans[0].offset == 0 && a.spans[0].length == 1024 * 1024);
  kmtl_shm_destroy(&a);

  /* Function constant allowlist */
  assert(kmtl_func_const_size(53) == 1);   /* Bool */
  assert(kmtl_func_const_size(3) == 4);    /* Float */
  assert(kmtl_func_const_size(5) == 16);   /* Float3 */
  assert(kmtl_func_const_size(88) == 32);  /* ULong4 */
  assert(kmtl_func_const_size(121) == 2);  /* BFloat */
  assert(kmtl_func_const_size(0) == 0);    /* None */
  assert(kmtl_func_const_size(1) == 0);    /* Struct */
  assert(kmtl_func_const_size(15) == 0);   /* Float4x4 rejected */
  assert(kmtl_func_const_size(9999) == 0);

  /* Language version allowlist */
  assert(kmtl_valid_language_version(0));
  assert(kmtl_valid_language_version((3u << 16) + 2u));
  assert(kmtl_valid_language_version((4u << 16) + 0u));
  assert(!kmtl_valid_language_version(0xDEADBEEFu));
  assert(!kmtl_valid_language_version((9u << 16) + 0u));

  printf("kmtl_shm_test: ok\n");
  return 0;
}
