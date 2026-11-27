/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "msl2glsl.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static const char *kTestMSL =
    "#include <metal_stdlib>\n"
    "using namespace metal;\n"
    "constant float SCALE [[function_constant(0)]];\n"
    "constant bool ADD_BIAS [[function_constant(1)]];\n"
    "struct Dims { uint rows; uint cols; };\n"
    "kernel void saxpy(\n"
    "    device const float *x [[buffer(0)]],\n"
    "    device float *y [[buffer(1)]],\n"
    "    constant float &alpha [[buffer(2)]],\n"
    "    uint2 gid [[thread_position_in_grid]]) {\n"
    "  uint id = gid.x;\n"
    "  float v = alpha * x[id] * SCALE;\n"
    "  if (ADD_BIAS) v += 1.0f;\n"
    "  y[id] = v;\n"
    "}\n"
    "kernel void rmsnorm_fp32(\n"
    "    device const float *x [[buffer(0)]],\n"
    "    device const float *w [[buffer(1)]],\n"
    "    device float *y [[buffer(2)]],\n"
    "    constant uint &cols [[buffer(3)]],\n"
    "    uint row [[threadgroup_position_in_grid]],\n"
    "    uint tid [[thread_index_in_threadgroup]],\n"
    "    uint tg_size [[threads_per_threadgroup]]) {\n"
    "  threadgroup float partial[256];\n"
    "  float sum = 0.0f;\n"
    "  uint base = row * cols;\n"
    "  for (uint i = tid; i < cols; i += tg_size) {\n"
    "    float v = x[base + i];\n"
    "    sum += v * v;\n"
    "  }\n"
    "  partial[tid] = sum;\n"
    "  threadgroup_barrier(mem_flags::mem_threadgroup);\n"
    "  float inv = rsqrt(partial[0] / float(cols) + 1e-6f);\n"
    "  for (uint i = tid; i < cols; i += tg_size) y[base + i] = x[base + i] * inv * w[i];\n"
    "}\n"
    "kernel void gemv_fp16(\n"
    "    device const half4 *W [[buffer(0)]],\n"
    "    device const half4 *x [[buffer(1)]],\n"
    "    device float *y [[buffer(2)]],\n"
    "    constant Dims &d [[buffer(3)]],\n"
    "    uint row [[threadgroup_position_in_grid]],\n"
    "    uint tid [[thread_index_in_threadgroup]],\n"
    "    uint tg_size [[threads_per_threadgroup]]) {\n"
    "  threadgroup float partial[256];\n"
    "  uint cols4 = d.cols >> 2;\n"
    "  float acc = 0.0f;\n"
    "  for (uint i = tid; i < cols4; i += tg_size) {\n"
    "    acc += dot(float4(W[row * cols4 + i]), float4(x[i]));\n"
    "  }\n"
    "  partial[tid] = acc;\n"
    "  threadgroup_barrier(mem_flags::mem_threadgroup);\n"
    "  if (tid == 0) y[row] = partial[0];\n"
    "}\n";

int main(void) {
  char names[512];
  size_t nlen = msl_extract_kernel_names(kTestMSL, strlen(kTestMSL), names, sizeof(names));
  assert(nlen > 0);
  assert(strcmp(names, "saxpy") == 0);

  msl_translate_result_t res;
  char err[256];

  assert(msl_translate_kernel(kTestMSL, strlen(kTestMSL), NULL, 0, "saxpy", &res, err, sizeof(err)));
  assert(res.binding_mask == 0x7u);
  assert(res.readonly_mask == 0x5u);
  assert(strstr(res.glsl, "gid.x") != NULL);
  assert(strstr(res.glsl, "_kmtl_buf0.data[id]") != NULL);
  assert(strstr(res.glsl, "layout(constant_id = 0) const float SCALE") != NULL);
  assert(strstr(res.glsl, "layout(constant_id = 1) const bool ADD_BIAS") != NULL);
  free(res.glsl);

  /* Reserved GLSL keywords escaped safely */
  const char *kw_msl =
      "#include <metal_stdlib>\nusing namespace metal;\n"
      "kernel void kw_test(device const float *input [[buffer(0)]], device float *output [[buffer(1)]],\n"
      "                    uint flat [[thread_position_in_grid]]) {\n"
      "  float out = input[flat] * 2.0f;\n"
      "  output[flat] = out;\n"
      "}\n";
  assert(msl_translate_kernel(kw_msl, strlen(kw_msl), NULL, 0, "kw_test", &res, err, sizeof(err)));
  assert(strstr(res.glsl, "uint _msl_flat = gl_GlobalInvocationID.x;") != NULL);
  assert(strstr(res.glsl, "float _msl_out = _kmtl_buf0.data[_msl_flat]") != NULL);
  free(res.glsl);

  assert(msl_translate_kernel(kTestMSL, strlen(kTestMSL), NULL, 0, "rmsnorm_fp32", &res, err, sizeof(err)));
  assert(res.binding_mask == 0xfu);
  assert(strstr(res.glsl, "shared  float partial[256];") != NULL);
  assert(strstr(res.glsl, "barrier();") != NULL);
  assert(strstr(res.glsl, "inversesqrt(") != NULL);
  free(res.glsl);

  assert(msl_translate_kernel(kTestMSL, strlen(kTestMSL), NULL, 0, "gemv_fp16", &res, err, sizeof(err)));
  assert(res.binding_mask == 0xfu);
  assert(strstr(res.glsl, "GL_EXT_shader_explicit_arithmetic_types_float16") != NULL);
  assert(strstr(res.glsl, "f16vec4 data[]") != NULL);
  assert(strstr(res.glsl, "vec4(_kmtl_buf0.data[") != NULL);
  free(res.glsl);

  /* Security regression: reject #extension, #include "/etc/passwd", bare CR, \f/\v, multi-backslash splice */
  const char *bad_shaders[] = {
      "#extension GL_GOOGLE_include_directive : enable\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "  #include \"/etc/passwd\"\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "#\\\ninclude \"/etc/passwd\"\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "kernel void k(uint i [[thread_position_in_grid]]) {\n#include \"/etc/passwd\"\n}\n",
      "constant uint X [[function_constant(1000)]];\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "kernel void k(device float *a [[buffer(abc)]]) {}\n",
      "int x;\r#extension GL_ARB_shading_language_include : enable\r#include \"/etc/passwd\"\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "\f#extension GL_ARB_shading_language_include : enable\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "\v#include \"/etc/passwd\"\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
      "int GL_GOO\\\\\n\nGLE_include_directive;\nkernel void k(uint i [[thread_position_in_grid]]) {}\n",
  };
  for (size_t i = 0; i < sizeof(bad_shaders) / sizeof(bad_shaders[0]); i++) {
    assert(!msl_translate_kernel(bad_shaders[i], strlen(bad_shaders[i]), NULL, 0, "k", &res, err, sizeof(err)));
  }

  /* Security regression: reject bad macros */
  msl_macro_t bad_macro = {.key = "FOO BAR", .key_len = 7, .val = "1", .val_len = 1};
  assert(!msl_translate_kernel(kTestMSL, strlen(kTestMSL), &bad_macro, 1, "saxpy", &res, err, sizeof(err)));
  msl_macro_t bad_macro2 = {.key = "FOO", .key_len = 3, .val = "1\\", .val_len = 2};
  assert(!msl_translate_kernel(kTestMSL, strlen(kTestMSL), &bad_macro2, 1, "saxpy", &res, err, sizeof(err)));

  /* Comment-only & unterminated block comment edge cases */
  const char *only_comment = "/* only comment */";
  assert(msl_extract_kernel_names(only_comment, strlen(only_comment), names, sizeof(names)) == 0);
  const char *unterm = "/* unterminated";
  assert(msl_extract_kernel_names(unterm, strlen(unterm), names, sizeof(names)) == 0);

  /* DoS regression: repeated `threadgroup ` with no semicolon must remain linear */
  size_t reps = 5000;
  size_t dlen = 128 + reps * 12;
  char *dos = malloc(dlen);
  assert(dos != NULL);
  size_t pos = (size_t)snprintf(dos, dlen, "kernel void k(uint i [[thread_position_in_grid]]) {");
  for (size_t r = 0; r < reps; r++) {
    memcpy(dos + pos, "threadgroup ", 12);
    pos += 12;
  }
  dos[pos++] = '}';
  dos[pos] = '\0';
  assert(msl_translate_kernel(dos, pos, NULL, 0, "k", &res, err, sizeof(err)));
  assert(res.glsl_len < pos * 2);
  free(res.glsl);
  free(dos);

  printf("msl2glsl_test: all checks passed\n");
  return 0;
}
