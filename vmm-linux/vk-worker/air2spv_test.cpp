/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "air2spv.h"
#include "metallib.h"

#include <llvm/AsmParser/Parser.h>
#include <llvm/Bitcode/BitcodeWriter.h>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Module.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/Support/raw_ostream.h>

#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <string>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

namespace {

std::vector<uint8_t> assemble_ir_to_bitcode(const char *ir_text) {
  llvm::LLVMContext ctx;
  llvm::SMDiagnostic diag;
  auto mod = llvm::parseAssemblyString(ir_text, diag, ctx);
  if (!mod) {
    diag.print("air2spv_test", llvm::errs());
    std::abort();
  }
  std::string bc_str;
  llvm::raw_string_ostream os(bc_str);
  llvm::WriteBitcodeToFile(*mod, os);
  os.flush();
  std::vector<uint8_t> raw(bc_str.begin(), bc_str.end());
  /* WriteBitcodeToFile auto-wraps Darwin triples in 0x0B17C0DE; unwrap to raw BC\xC0\xDE here
     so build_metallib can explicitly test both wrapped and raw bitcode slices. */
  if (raw.size() >= 20 && raw[0] == 0xDE && raw[1] == 0xC0 && raw[2] == 0x17 && raw[3] == 0x0B) {
    uint32_t off = raw[8] | (raw[9] << 8) | (raw[10] << 16) | (raw[11] << 24);
    uint32_t sz = raw[12] | (raw[13] << 8) | (raw[14] << 16) | (raw[15] << 24);
    assert(off >= 20 && off + sz <= raw.size());
    return std::vector<uint8_t>(raw.begin() + off, raw.begin() + off + sz);
  }
  return raw;
}

void push_le16(std::vector<uint8_t> &v, uint16_t x) {
  v.push_back(static_cast<uint8_t>(x & 0xFF));
  v.push_back(static_cast<uint8_t>((x >> 8) & 0xFF));
}
void push_le32(std::vector<uint8_t> &v, uint32_t x) {
  for (int i = 0; i < 4; i++) v.push_back(static_cast<uint8_t>((x >> (i * 8)) & 0xFF));
}
void push_le64(std::vector<uint8_t> &v, uint64_t x) {
  for (int i = 0; i < 8; i++) v.push_back(static_cast<uint8_t>((x >> (i * 8)) & 0xFF));
}
void push_be32(std::vector<uint8_t> &v, uint32_t x) {
  for (int i = 3; i >= 0; i--) v.push_back(static_cast<uint8_t>((x >> (i * 8)) & 0xFF));
}
void push_str(std::vector<uint8_t> &v, const char *s, size_t n) {
  v.insert(v.end(), reinterpret_cast<const uint8_t *>(s),
           reinterpret_cast<const uint8_t *>(s) + n);
}

struct MtlbEntrySpec {
  std::string name;
  uint8_t fn_type;
  std::vector<uint8_t> bitcode;
  bool wrap_darwin_hdr;
};

std::vector<uint8_t> build_metallib(const std::vector<MtlbEntrySpec> &entries) {
  std::vector<uint8_t> fn_sec;
  std::vector<uint8_t> bc_sec;
  push_le32(fn_sec, static_cast<uint32_t>(entries.size()));

  for (const auto &e : entries) {
    uint64_t bc_rel = bc_sec.size();
    std::vector<uint8_t> bc_blob;
    if (e.wrap_darwin_hdr) {
      push_le32(bc_blob, 0x0B17C0DEu);
      push_le32(bc_blob, 0u);
      push_le32(bc_blob, 20u);
      push_le32(bc_blob, static_cast<uint32_t>(e.bitcode.size()));
      push_le32(bc_blob, 0xFFFFFFFFu);
    }
    bc_blob.insert(bc_blob.end(), e.bitcode.begin(), e.bitcode.end());
    while ((bc_blob.size() & 3u) != 0) bc_blob.push_back(0);
    bc_sec.insert(bc_sec.end(), bc_blob.begin(), bc_blob.end());

    std::vector<uint8_t> rec;
    push_le32(rec, 0); /* entry_sz placeholder */
    push_str(rec, "NAME", 4);
    push_le16(rec, static_cast<uint16_t>(e.name.size() + 1));
    push_str(rec, e.name.c_str(), e.name.size() + 1);
    push_str(rec, "TYPE", 4);
    push_le16(rec, 1);
    rec.push_back(e.fn_type);
    push_str(rec, "OFFT", 4);
    push_le16(rec, 24);
    push_le64(rec, 0);
    push_le64(rec, 0);
    push_le64(rec, bc_rel);
    push_str(rec, "MDSZ", 4);
    push_le16(rec, 8);
    push_le64(rec, bc_blob.size());
    push_str(rec, "ENDT", 4);
    uint32_t esz = static_cast<uint32_t>(rec.size());
    rec[0] = static_cast<uint8_t>(esz & 0xFF);
    rec[1] = static_cast<uint8_t>((esz >> 8) & 0xFF);
    rec[2] = static_cast<uint8_t>((esz >> 16) & 0xFF);
    rec[3] = static_cast<uint8_t>((esz >> 24) & 0xFF);
    fn_sec.insert(fn_sec.end(), rec.begin(), rec.end());
  }

  uint64_t fn_off = 88;
  uint64_t pub_off = fn_off + fn_sec.size();
  uint64_t priv_off = pub_off;
  uint64_t bc_off = priv_off;
  uint64_t total_sz = bc_off + bc_sec.size();

  std::vector<uint8_t> out;
  out.reserve(total_sz);
  push_str(out, "MTLB", 4);
  for (int i = 0; i < 12; i++) out.push_back(0);
  push_le64(out, total_sz);
  push_le64(out, fn_off);
  push_le64(out, fn_sec.size() - 4u); /* Apple MTLB stores entry bytes excluding 4B fn_count */
  push_le64(out, pub_off);
  push_le64(out, 0);
  push_le64(out, priv_off);
  push_le64(out, 0);
  push_le64(out, bc_off);
  push_le64(out, bc_sec.size());
  assert(out.size() == 88);
  out.insert(out.end(), fn_sec.begin(), fn_sec.end());
  out.insert(out.end(), bc_sec.begin(), bc_sec.end());
  return out;
}

std::vector<uint8_t> wrap_macho_fat(const std::vector<uint8_t> &mtlb) {
  std::vector<uint8_t> fat;
  push_be32(fat, 0xCAFEBABEu);
  push_be32(fat, 1u);
  push_be32(fat, 0x01000013u);
  push_be32(fat, 0u);
  push_be32(fat, 64u);
  push_be32(fat, static_cast<uint32_t>(mtlb.size()));
  push_be32(fat, 4u);
  while (fat.size() < 64) fat.push_back(0);
  fat.insert(fat.end(), mtlb.begin(), mtlb.end());
  return fat;
}

void validate_spirv_with_tool(const uint32_t *words, size_t bytes, const char *label) {
  assert(words != nullptr && bytes >= 20 && (bytes & 3) == 0);
  assert(words[0] == 0x07230203u);

  char path[] = "/tmp/air2spv-val-XXXXXX";
  int fd = mkstemp(path);
  assert(fd >= 0);
  ssize_t w = write(fd, words, bytes);
  close(fd);
  assert(w == static_cast<ssize_t>(bytes));

  pid_t pid = fork();
  assert(pid >= 0);
  if (pid == 0) {
    char *argv[] = {const_cast<char *>("spirv-val"), const_cast<char *>("--target-env"),
                    const_cast<char *>("vulkan1.3"), path, nullptr};
    execvp("spirv-val", argv);
    _exit(127);
  }
  int status = 0;
  waitpid(pid, &status, 0);
  unlink(path);
  if (WIFEXITED(status) && WEXITSTATUS(status) == 127) {
    std::fprintf(stderr, "air2spv_test: [note] spirv-val not installed, skipped external check for %s\n",
                 label);
    return;
  }
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
    std::fprintf(stderr, "air2spv_test: FAIL spirv-val rejected %s (exit=%d)\n", label,
                 WEXITSTATUS(status));
    std::abort();
  }
}

void test_metallib_parser_negative() {
  char err[256];
  metallib_index_t idx{};

  /* Empty / tiny / bad magic */
  assert(!metallib_parse(nullptr, 0, &idx, err, sizeof(err)));
  uint8_t tiny[16] = {'M', 'T', 'L', 'B'};
  assert(!metallib_parse(tiny, sizeof(tiny), &idx, err, sizeof(err)));
  uint8_t bad[128] = {'N', 'O', 'P', 'E'};
  assert(!metallib_parse(bad, sizeof(bad), &idx, err, sizeof(err)));

  /* Crafted FAT header with huge nfat */
  std::vector<uint8_t> bad_fat;
  push_be32(bad_fat, 0xCAFEBABEu);
  push_be32(bad_fat, 999999u);
  bad_fat.resize(128, 0);
  assert(!metallib_parse(bad_fat.data(), bad_fat.size(), &idx, err, sizeof(err)));

  /* Crafted MTLB with out-of-bounds declared size & fn_off overflow */
  std::vector<uint8_t> bad_mtlb(128, 0);
  std::memcpy(bad_mtlb.data(), "MTLB", 4);
  uint64_t huge = 0xFFFFFFFFFFFFULL;
  std::memcpy(bad_mtlb.data() + 16, &huge, 8);
  assert(!metallib_parse(bad_mtlb.data(), bad_mtlb.size(), &idx, err, sizeof(err)));

  /* Crafted valid-header MTLB with truncated function table and invalid bitcode magic */
  auto bc = std::vector<uint8_t>{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0};
  auto bad_bc_mlib = build_metallib({{"bad_fn", METALLIB_FN_COMPUTE, bc, false}});
  assert(!metallib_parse(bad_bc_mlib.data(), bad_bc_mlib.size(), &idx, err, sizeof(err)));
}

void test_air_kernels_and_spirv_val() {
  /* 1. fc_saxpy: FunctionConstants (float SpecId 0, bool SpecId 1) + buffers + GlobalInvocationId */
  const char *ir_fc_saxpy = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i8:8:8-i16:16:16-i32:32:32-i64:64:64-f32:32:32"
target triple = "air64_v29-apple-macosx27.2.0"

@kScale = external addrspace(2) constant float
@kAddBias = external addrspace(2) constant i1

define void @fc_saxpy(ptr addrspace(1) readonly %a, ptr addrspace(1) readonly %b,
                      ptr addrspace(1) writeonly %out, ptr addrspace(2) readonly %alpha_p,
                      i32 %gid) {
entry:
  %idx64 = zext i32 %gid to i64
  %pa = getelementptr inbounds float, ptr addrspace(1) %a, i64 %idx64
  %pb = getelementptr inbounds float, ptr addrspace(1) %b, i64 %idx64
  %po = getelementptr inbounds float, ptr addrspace(1) %out, i64 %idx64
  %va = load float, ptr addrspace(1) %pa, align 4
  %vb = load float, ptr addrspace(1) %pb, align 4
  %alpha = load float, ptr addrspace(2) %alpha_p, align 4
  %sc = load float, ptr addrspace(2) @kScale, align 4
  %ab = load i1, ptr addrspace(2) @kAddBias, align 1
  %m = fmul float %alpha, %va
  %s = fadd float %m, %vb
  %r0 = fmul float %s, %sc
  %r1 = fadd float %r0, 1.500000e+00
  %r = select i1 %ab, float %r1, float %r0
  store float %r, ptr addrspace(1) %po, align 4
  ret void
}

!air.kernel = !{!0}
!air.function_constants = !{!10, !11}
!0 = !{ptr @fc_saxpy, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6, !7}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.buffer", !"air.location_index", i32 3, i32 1, !"air.read", !"air.address_space", i32 2}
!7 = !{i32 4, !"air.thread_position_in_grid"}
!10 = !{ptr addrspace(2) @kScale, !"air.location_index", i32 0}
!11 = !{ptr addrspace(2) @kAddBias, !"air.location_index", i32 1}
)IR";

  /* 2. rmsnorm_fp32: static threadgroup memory, loops, if inside loop header, air.wg.barrier, air.fast_rsqrt */
  const char *ir_rmsnorm = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i8:8:8-i16:16:16-i32:32:32-i64:64:64-f32:32:32"
target triple = "air64_v29-apple-macosx27.2.0"

@partial = internal addrspace(3) global [256 x float] undef, align 4

declare void @air.wg.barrier(i32, i32, i32)
declare float @air.fast_rsqrt.f32(float)

define void @rmsnorm_fp32(ptr addrspace(1) readonly %x, ptr addrspace(1) readonly %w,
                          ptr addrspace(1) writeonly %y, ptr addrspace(2) readonly %cols_p,
                          i32 %row, i32 %tid, i32 %tg_size) {
entry:
  %cols = load i32, ptr addrspace(2) %cols_p, align 4
  %base = mul i32 %row, %cols
  br label %sum_hdr

sum_hdr:
  %i = phi i32 [ %tid, %entry ], [ %i_next, %sum_body ]
  %acc = phi float [ 0.000000e+00, %entry ], [ %acc_next, %sum_body ]
  %cmp0 = icmp ult i32 %i, %cols
  br i1 %cmp0, label %sum_body, label %sum_done

sum_body:
  %off0 = add i32 %base, %i
  %off0_64 = zext i32 %off0 to i64
  %px = getelementptr inbounds float, ptr addrspace(1) %x, i64 %off0_64
  %vx = load float, ptr addrspace(1) %px, align 4
  %sq = fmul float %vx, %vx
  %acc_next = fadd float %acc, %sq
  %i_next = add i32 %i, %tg_size
  br label %sum_hdr

sum_done:
  %tid64 = zext i32 %tid to i64
  %pt = getelementptr inbounds [256 x float], ptr addrspace(3) @partial, i64 0, i64 %tid64
  store float %acc, ptr addrspace(3) %pt, align 4
  call void @air.wg.barrier(i32 2, i32 5, i32 1)
  %s0 = lshr i32 %tg_size, 1
  br label %red_hdr

red_hdr:
  %s = phi i32 [ %s0, %sum_done ], [ %s_next, %red_latch ]
  %scmp = icmp ne i32 %s, 0
  br i1 %scmp, label %red_check, label %red_done

red_check:
  %tcmp = icmp ult i32 %tid, %s
  br i1 %tcmp, label %red_add, label %red_latch

red_add:
  %ts = add i32 %tid, %s
  %ts64 = zext i32 %ts to i64
  %p1 = getelementptr inbounds [256 x float], ptr addrspace(3) @partial, i64 0, i64 %tid64
  %p2 = getelementptr inbounds [256 x float], ptr addrspace(3) @partial, i64 0, i64 %ts64
  %v1 = load float, ptr addrspace(3) %p1, align 4
  %v2 = load float, ptr addrspace(3) %p2, align 4
  %vs = fadd float %v1, %v2
  store float %vs, ptr addrspace(3) %p1, align 4
  br label %red_latch

red_latch:
  call void @air.wg.barrier(i32 2, i32 5, i32 1)
  %s_next = lshr i32 %s, 1
  br label %red_hdr

red_done:
  %tot = load float, ptr addrspace(3) @partial, align 4
  %cols_f = uitofp i32 %cols to float
  %mean = fdiv float %tot, %cols_f
  %meps = fadd float %mean, 0x3EB0C6F7A0000000
  %inv_rms = call float @air.fast_rsqrt.f32(float %meps)
  br label %out_hdr

out_hdr:
  %j = phi i32 [ %tid, %red_done ], [ %j_next, %out_body ]
  %ocmp = icmp ult i32 %j, %cols
  br i1 %ocmp, label %out_body, label %exit

out_body:
  %off1 = add i32 %base, %j
  %off1_64 = zext i32 %off1 to i64
  %j64 = zext i32 %j to i64
  %px1 = getelementptr inbounds float, ptr addrspace(1) %x, i64 %off1_64
  %pw1 = getelementptr inbounds float, ptr addrspace(1) %w, i64 %j64
  %py1 = getelementptr inbounds float, ptr addrspace(1) %y, i64 %off1_64
  %xv = load float, ptr addrspace(1) %px1, align 4
  %wv = load float, ptr addrspace(1) %pw1, align 4
  %t0 = fmul float %xv, %inv_rms
  %t1 = fmul float %t0, %wv
  store float %t1, ptr addrspace(1) %py1, align 4
  %j_next = add i32 %j, %tg_size
  br label %out_hdr

exit:
  ret void
}

!air.kernel = !{!0}
!0 = !{ptr @rmsnorm_fp32, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6, !7, !8, !9}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.buffer", !"air.location_index", i32 3, i32 1, !"air.read", !"air.address_space", i32 2}
!7 = !{i32 4, !"air.threadgroup_position_in_grid"}
!8 = !{i32 5, !"air.thread_index_in_threadgroup"}
!9 = !{i32 6, !"air.threads_per_threadgroup"}
)IR";

  /* 3. fp16_vec4_dot + simd_sum + swiglu (tests half4, fpext, dot, exp, subgroup ops) */
  const char *ir_fp16_simd = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i8:8:8-i16:16:16-i32:32:32-i64:64:64-f32:32:32"
target triple = "air64_v29-apple-macosx27.2.0"

declare float @air.dot.v4f32(<4 x float>, <4 x float>)
declare float @air.simd_sum.f32(float)
declare float @air.fast_exp.f32(float)

define void @fp16_simd(ptr addrspace(1) readonly %w, ptr addrspace(1) readonly %x,
                       ptr addrspace(1) writeonly %y, i32 %gid) {
entry:
  %g64 = zext i32 %gid to i64
  %pw = getelementptr inbounds <4 x half>, ptr addrspace(1) %w, i64 %g64
  %px = getelementptr inbounds <4 x half>, ptr addrspace(1) %x, i64 %g64
  %hw = load <4 x half>, ptr addrspace(1) %pw, align 8
  %hx = load <4 x half>, ptr addrspace(1) %px, align 8
  %fw = fpext <4 x half> %hw to <4 x float>
  %fx = fpext <4 x half> %hx to <4 x float>
  %d = call float @air.dot.v4f32(<4 x float> %fw, <4 x float> %fx)
  %s = call float @air.simd_sum.f32(float %d)
  %ng = fneg float %s
  %ex = call float @air.fast_exp.f32(float %ng)
  %den = fadd float 1.000000e+00, %ex
  %silu = fdiv float %s, %den
  %py = getelementptr inbounds float, ptr addrspace(1) %y, i64 %g64
  store float %silu, ptr addrspace(1) %py, align 4
  ret void
}

!air.kernel = !{!0}
!0 = !{ptr @fp16_simd, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.thread_position_in_grid"}
)IR";

  /* 4. Hostile negative bitcode cases: out-of-range binding slot, bad i24 type, recursive call */
  const char *ir_bad_slot = R"IR(
define void @bad_slot(ptr addrspace(1) %p) {
  store i32 1, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_slot, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 99, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR";

  const char *ir_bad_i24 = R"IR(
define void @bad_i24(ptr addrspace(1) %p) {
  %x = add i24 1, 2
  %y = zext i24 %x to i32
  store i32 %y, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_i24, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR";

  auto bc_saxpy = assemble_ir_to_bitcode(ir_fc_saxpy);
  auto bc_rms = assemble_ir_to_bitcode(ir_rmsnorm);
  auto bc_fp16 = assemble_ir_to_bitcode(ir_fp16_simd);
  auto bc_bad_slot = assemble_ir_to_bitcode(ir_bad_slot);
  auto bc_bad_i24 = assemble_ir_to_bitcode(ir_bad_i24);

  auto mlib = build_metallib({
      {"fc_saxpy", METALLIB_FN_COMPUTE, bc_saxpy, true},
      {"rmsnorm_fp32", METALLIB_FN_COMPUTE, bc_rms, true},
      {"fp16_simd", METALLIB_FN_COMPUTE, bc_fp16, false},
      {"dummy_vert", METALLIB_FN_VERTEX, bc_saxpy, true},
  });
  auto fat_mlib = wrap_macho_fat(mlib);

#define CHECK_OK(cond, msg)                                                                   \
  do {                                                                                        \
    if (!(cond)) {                                                                            \
      std::fprintf(stderr, "air2spv_test:%d: FAIL (%s): %s\n", __LINE__, #cond, (msg));       \
      std::abort();                                                                           \
    }                                                                                         \
  } while (0)

  char err[512] = {0};
  metallib_index_t idx{};
  CHECK_OK(metallib_parse(fat_mlib.data(), fat_mlib.size(), &idx, err, sizeof(err)), err);
  assert(idx.count == 4);
  char names[256] = {0};
  size_t nlen = metallib_encode_compute_names(&idx, names, sizeof(names));
  assert(nlen > 0);
  assert(metallib_find_compute(&idx, "fc_saxpy") != nullptr);
  assert(metallib_find_compute(&idx, "dummy_vert") == nullptr);
  metallib_index_free(&idx);

  /* Test with has_int64 = false (matches V3D 7.1) to prove zero Int64 leakage from 64-bit GEPs. */
  const uint32_t all_sg = AIR2SPV_SUBGROUP_BASIC | AIR2SPV_SUBGROUP_VOTE |
                          AIR2SPV_SUBGROUP_ARITHMETIC | AIR2SPV_SUBGROUP_BALLOT |
                          AIR2SPV_SUBGROUP_SHUFFLE | AIR2SPV_SUBGROUP_SHUFFLE_RELATIVE;
  air2spv_caps_t caps = {true, true, true, true, true, false, 32768, all_sg, 32};
  air2spv_result_t res{};

  CHECK_OK(air2spv_compile_metallib(fat_mlib.data(), fat_mlib.size(), "fc_saxpy", &caps, &res, err,
                                    sizeof(err)), err);
  assert(res.binding_mask == 0x0Fu);
  assert(res.readonly_mask == 0x0Bu);
  validate_spirv_with_tool(res.spirv, res.spirv_bytes, "fc_saxpy");
  std::free(res.spirv);

  /* Also verify zero-Int64 profile (Broadcom V3D 7.1 has shaderInt64 = false). */
  air2spv_caps_t caps_no64 = {true, true, true, true, true, false, 32768, all_sg, 32};
  CHECK_OK(air2spv_compile_metallib(mlib.data(), mlib.size(), "rmsnorm_fp32", &caps_no64, &res,
                                    err, sizeof(err)), err);
  assert(res.binding_mask == 0x0Fu);
  assert(res.readonly_mask == 0x0Bu);
  validate_spirv_with_tool(res.spirv, res.spirv_bytes, "rmsnorm_fp32_no64");
  std::free(res.spirv);

  CHECK_OK(air2spv_compile_metallib(mlib.data(), mlib.size(), "fp16_simd", &caps, &res, err,
                                    sizeof(err)), err);
  assert(res.binding_mask == 0x07u);
  assert(res.readonly_mask == 0x03u);
  validate_spirv_with_tool(res.spirv, res.spirv_bytes, "fp16_simd");
  std::free(res.spirv);

  /* Capability gating: fp16_simd must fail cleanly when subgroup_ops lacks arithmetic. */
  air2spv_caps_t caps_nosg = {true, true, true, true, true, false, 32768, 0, 32};
  assert(!air2spv_compile_metallib(mlib.data(), mlib.size(), "fp16_simd", &caps_nosg, &res, err,
                                   sizeof(err)));

  /* Valid edge cases that must compile and pass spirv-val:
     (a) ordered i1 icmp + i1 uitofp/sitofp + fptoui to i1 + int-to-int same-width air.convert,
     (b) struct with half fields stored to SSBO,
     (c) diamond select pointer DAG (40 deep -> must finish instantly, not 2^40),
     (d) duplicate-predecessor PHI from `br i1 %c, label %m, label %m`. */
  const char *ir_valid_edges = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i8:8:8-i16:16:16-i32:32:32-i64:64:64-f32:32:32"
target triple = "air64_v29-apple-macosx27.2.0"

%pair_h = type { half, half }
declare i32 @air.convert.u.i32.u.i32(i32)

define void @valid_edges(ptr addrspace(1) %out_f, ptr addrspace(1) %out_h, i32 %gid) {
entry:
  %b0 = icmp eq i32 %gid, 0
  %b1 = icmp ne i32 %gid, 1
  %bu = icmp ult i1 %b0, %b1
  %bs = icmp slt i1 %b0, %b1
  %fu = uitofp i1 %bu to float
  %fs = sitofp i1 %bs to float
  %sum = fadd float %fu, %fs
  %bf = fptoui float %sum to i1
  %same = call i32 @air.convert.u.i32.u.i32(i32 %gid)
  %sum2 = select i1 %bf, float %sum, float 2.000000e+00
  %p = getelementptr inbounds float, ptr addrspace(1) %out_f, i32 %same
  store float %sum2, ptr addrspace(1) %p, align 4
  %h0 = fptrunc float %sum2 to half
  %s0 = insertvalue %pair_h undef, half %h0, 0
  %s1 = insertvalue %pair_h %s0, half %h0, 1
  %ph = getelementptr inbounds %pair_h, ptr addrspace(1) %out_h, i32 %gid
  store %pair_h %s1, ptr addrspace(1) %ph, align 2
  br i1 %b0, label %dup, label %dup

dup:
  %dphi = phi i32 [ %gid, %entry ], [ %gid, %entry ]
  %p2 = getelementptr inbounds float, ptr addrspace(1) %out_f, i32 %dphi
  store float %sum, ptr addrspace(1) %p2, align 4
  ret void
}

!air.kernel = !{!0}
!0 = !{ptr @valid_edges, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
  auto bc_edges = assemble_ir_to_bitcode(ir_valid_edges);
  CHECK_OK(air2spv_compile_bitcode(bc_edges.data(), bc_edges.size(), "valid_edges", &caps, &res,
                                   err, sizeof(err)), err);
  validate_spirv_with_tool(res.spirv, res.spirv_bytes, "valid_edges");
  std::free(res.spirv);

  /* 40-deep select pointer diamond DAG must complete in milliseconds via 3-state memoization. */
  {
    std::string ir =
        "target datalayout = \"e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32\"\n"
        "define void @sel_dag(ptr addrspace(1) %p, i32 %gid) {\n"
        "entry:\n"
        "  %c = icmp eq i32 %gid, 0\n"
        "  %s0 = getelementptr inbounds float, ptr addrspace(1) %p, i32 0\n";
    for (int i = 1; i <= 40; i++) {
      ir += "  %s" + std::to_string(i) + " = select i1 %c, ptr addrspace(1) %s" +
            std::to_string(i - 1) + ", ptr addrspace(1) %s" + std::to_string(i - 1) + "\n";
    }
    ir += "  store float 1.000000e+00, ptr addrspace(1) %s40, align 4\n"
          "  ret void\n}\n"
          "!air.kernel = !{!0}\n!0 = !{ptr @sel_dag, !1, !2}\n!1 = !{}\n!2 = !{!3, !4}\n"
          "!3 = !{i32 0, !\"air.buffer\", !\"air.location_index\", i32 0, i32 1, !\"air.read_write\", !\"air.address_space\", i32 1}\n"
          "!4 = !{i32 1, !\"air.thread_position_in_grid\"}\n";
    auto bc_dag = assemble_ir_to_bitcode(ir.c_str());
    CHECK_OK(air2spv_compile_bitcode(bc_dag.data(), bc_dag.size(), "sel_dag", &caps, &res, err,
                                     sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "sel_dag");
    std::free(res.spirv);
  }

  /* Nested if (c1 && c2) sharing outer exit + unstructured acyclic cross-branch CFG + dynamic GEP pointer PHI. */
  {
    const char *ir_nested_if = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @nested_if(ptr addrspace(1) %out, i32 %gid) {
entry:
  %c1 = icmp ult i32 %gid, 100
  br i1 %c1, label %a, label %exit
a:
  %c2 = icmp ugt i32 %gid, 10
  br i1 %c2, label %body, label %exit
body:
  %p = getelementptr inbounds float, ptr addrspace(1) %out, i32 %gid
  store float 4.200000e+01, ptr addrspace(1) %p, align 4
  br label %exit
exit:
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @nested_if, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR";
    auto bc_nif = assemble_ir_to_bitcode(ir_nested_if);
    CHECK_OK(air2spv_compile_bitcode(bc_nif.data(), bc_nif.size(), "nested_if", &caps, &res, err,
                                     sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "nested_if");
    std::free(res.spirv);

    const char *ir_cross_cfg = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @cross_cfg(ptr addrspace(1) %out, i32 %gid) {
entry:
  %c0 = icmp eq i32 %gid, 0
  br i1 %c0, label %a, label %b
a:
  %c1 = icmp ult i32 %gid, 4
  %pa = getelementptr inbounds float, ptr addrspace(1) %out, i32 %gid
  br i1 %c1, label %c, label %d
b:
  %pb = getelementptr inbounds float, ptr addrspace(1) %out, i32 %gid
  br label %c
c:
  %pc = phi ptr addrspace(1) [ %pa, %a ], [ %pb, %b ]
  store float 1.000000e+00, ptr addrspace(1) %pc, align 4
  br label %exit
d:
  store float 2.000000e+00, ptr addrspace(1) %pa, align 4
  br label %exit
exit:
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @cross_cfg, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR";
    auto bc_ccfg = assemble_ir_to_bitcode(ir_cross_cfg);
    CHECK_OK(air2spv_compile_bitcode(bc_ccfg.data(), bc_ccfg.size(), "cross_cfg", &caps, &res, err,
                                     sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "cross_cfg");
    std::free(res.spirv);

    /* Post-inline readonly inference: binding 0 tagged air.read_write but never stored -> readonly bit set. */
    const char *ir_infer_ro = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @infer_ro(ptr addrspace(1) %weights, ptr addrspace(1) %out, i32 %gid) {
entry:
  %wp = getelementptr inbounds float, ptr addrspace(1) %weights, i32 %gid
  %w = load float, ptr addrspace(1) %wp, align 4
  %op = getelementptr inbounds float, ptr addrspace(1) %out, i32 %gid
  store float %w, ptr addrspace(1) %op, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @infer_ro, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
    auto bc_ro = assemble_ir_to_bitcode(ir_infer_ro);
    CHECK_OK(air2spv_compile_bitcode(bc_ro.data(), bc_ro.size(), "infer_ro", &caps, &res, err,
                                     sizeof(err)), err);
    assert(res.binding_mask == 0x3u);
    assert(res.readonly_mask == 0x1u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "infer_ro");
    std::free(res.spirv);

    /* Two-buffer pointer select (gated_delta_net pattern) + downstream OpPhi in successor block. */
    const char *ir_dual_sel = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @dual_buf_select(ptr addrspace(1) %src0, ptr addrspace(1) %src1, ptr addrspace(1) %dst0, ptr addrspace(1) %dst1, i32 %gid) {
entry:
  %c = icmp eq i32 %gid, 0
  %sp = select i1 %c, ptr addrspace(1) %src0, ptr addrspace(1) %src1
  %sp_off = getelementptr inbounds float, ptr addrspace(1) %sp, i32 %gid
  %v = load float, ptr addrspace(1) %sp_off, align 4
  %dp = select i1 %c, ptr addrspace(1) %dst0, ptr addrspace(1) %dst1
  %dp_off = getelementptr inbounds float, ptr addrspace(1) %dp, i32 %gid
  store float %v, ptr addrspace(1) %dp_off, align 4
  br label %done
done:
  %p = phi float [ %v, %entry ]
  store float %p, ptr addrspace(1) %dst0, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @dual_buf_select, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6, !7}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.buffer", !"air.location_index", i32 3, i32 1, !"air.read_write", !"air.address_space", i32 1}
!7 = !{i32 4, !"air.thread_position_in_grid"}
)IR";
    auto bc_ds = assemble_ir_to_bitcode(ir_dual_sel);
    CHECK_OK(air2spv_compile_bitcode(bc_ds.data(), bc_ds.size(), "dual_buf_select", &caps, &res,
                                     err, sizeof(err)), err);
    assert(res.binding_mask == 0xFu);
    assert(res.readonly_mask == 0x3u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "dual_buf_select");
    std::free(res.spirv);

    /* Two-buffer pointer select inside a loop header (exact gated_delta_net pattern). */
    const char *ir_dual_loop = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @dual_buf_loop_hdr(ptr addrspace(1) %dst0, ptr addrspace(1) %dst1, i32 %gid) {
entry:
  %c = icmp eq i32 %gid, 0
  %dp = select i1 %c, ptr addrspace(1) %dst0, ptr addrspace(1) %dst1
  br label %loop
loop:
  %i = phi i32 [ 0, %entry ], [ %ni, %loop ]
  %p = getelementptr inbounds float, ptr addrspace(1) %dp, i32 %i
  store float 1.000000e+00, ptr addrspace(1) %p, align 4
  %ni = add i32 %i, 1
  %more = icmp ult i32 %ni, 4
  br i1 %more, label %loop, label %exit
exit:
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @dual_buf_loop_hdr, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
    auto bc_dl = assemble_ir_to_bitcode(ir_dual_loop);
    CHECK_OK(air2spv_compile_bitcode(bc_dl.data(), bc_dl.size(), "dual_buf_loop_hdr", &caps, &res,
                                     err, sizeof(err)), err);
    assert(res.binding_mask == 0x3u);
    assert(res.readonly_mask == 0x0u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "dual_buf_loop_hdr");
    std::free(res.spirv);

    /* Software bfloat16 load/fpext/arithmetic/fptrunc/store without VK_KHR_shader_bfloat16. */
    const char *ir_bf16 = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i16:16:16-i32:32:32-f32:32:32"
define void @bf16_roundtrip(ptr addrspace(1) %in, ptr addrspace(1) %out, i32 %gid) {
entry:
  %ip = getelementptr inbounds bfloat, ptr addrspace(1) %in, i32 %gid
  %b = load bfloat, ptr addrspace(1) %ip, align 2
  %f = fpext bfloat %b to float
  %f2 = fadd float %f, 1.500000e+00
  %b2 = fptrunc float %f2 to bfloat
  %bn = fneg bfloat %b2
  %op = getelementptr inbounds bfloat, ptr addrspace(1) %out, i32 %gid
  store bfloat %bn, ptr addrspace(1) %op, align 2
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bf16_roundtrip, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
    auto bc_bf = assemble_ir_to_bitcode(ir_bf16);
    CHECK_OK(air2spv_compile_bitcode(bc_bf.data(), bc_bf.size(), "bf16_roundtrip", &caps, &res,
                                     err, sizeof(err)), err);
    assert(res.binding_mask == 0x3u);
    assert(res.readonly_mask == 0x1u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "bf16_roundtrip");
    std::free(res.spirv);

    /* Two-buffer pointer PHI (exact kernel_concat_f32 pattern). */
    const char *ir_dual_phi = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @dual_buf_phi(ptr addrspace(1) %a, ptr addrspace(1) %b, ptr addrspace(1) %dst, i32 %gid) {
entry:
  %c = icmp ult i32 %gid, 4
  br i1 %c, label %lhs, label %rhs
lhs:
  %pa = getelementptr inbounds float, ptr addrspace(1) %a, i32 %gid
  br label %merge
rhs:
  %idx = sub i32 %gid, 4
  %pb = getelementptr inbounds float, ptr addrspace(1) %b, i32 %idx
  br label %merge
merge:
  %p = phi ptr addrspace(1) [ %pa, %lhs ], [ %pb, %rhs ]
  %v = load float, ptr addrspace(1) %p, align 4
  %dp = getelementptr inbounds float, ptr addrspace(1) %dst, i32 %gid
  store float %v, ptr addrspace(1) %dp, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @dual_buf_phi, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.thread_position_in_grid"}
)IR";
    auto bc_dp = assemble_ir_to_bitcode(ir_dual_phi);
    CHECK_OK(air2spv_compile_bitcode(bc_dp.data(), bc_dp.size(), "dual_buf_phi", &caps, &res,
                                     err, sizeof(err)), err);
    assert(res.binding_mask == 0x7u);
    assert(res.readonly_mask == 0x3u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "dual_buf_phi");
    std::free(res.spirv);

    /* ptrtoint alignment check (exact concat_copy pattern). */
    const char *ir_p2i = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-i64:64:64-f32:32:32"
define void @ptrtoint_align(ptr addrspace(1) %src, ptr addrspace(1) %dst, i32 %gid) {
entry:
  %a = ptrtoint ptr addrspace(1) %src to i64
  %b = ptrtoint ptr addrspace(1) %dst to i64
  %x = xor i64 %a, %b
  %m = and i64 %x, 15
  %ok = icmp eq i64 %m, 0
  %v = select i1 %ok, float 1.000000e+00, float 2.000000e+00
  %dp = getelementptr inbounds float, ptr addrspace(1) %dst, i32 %gid
  store float %v, ptr addrspace(1) %dp, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @ptrtoint_align, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
    auto bc_p2i = assemble_ir_to_bitcode(ir_p2i);
    air2spv_caps_t caps_sub = caps;
    caps_sub.ssbo_offset_align = 64;
    CHECK_OK(air2spv_compile_bitcode(bc_p2i.data(), bc_p2i.size(), "ptrtoint_align", &caps_sub, &res,
                                     err, sizeof(err)), err);
    assert(res.binding_mask == 0x3u);
    assert(res.readonly_mask == 0x1u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "ptrtoint_align");
    std::free(res.spirv);

    /* Magic 64-bit unsigned division (tid / 3) must validate under both has_int64=false (UMulExtended) and true. */
    const char *ir_magic_div = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-i64:64:64"
define void @magic_div_u32(ptr addrspace(1) %dst, i32 %gid) {
entry:
  %a = zext i32 %gid to i64
  %m = mul nuw i64 %a, 2863311531
  %q = lshr i64 %m, 33
  %r = trunc i64 %q to i32
  %dp = getelementptr inbounds i32, ptr addrspace(1) %dst, i32 %gid
  store i32 %r, ptr addrspace(1) %dp, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @magic_div_u32, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR";
    auto bc_mdiv = assemble_ir_to_bitcode(ir_magic_div);
    CHECK_OK(air2spv_compile_bitcode(bc_mdiv.data(), bc_mdiv.size(), "magic_div_u32", &caps, &res,
                                     err, sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "magic_div_u32_no64");
    std::free(res.spirv);
    air2spv_caps_t caps64 = caps;
    caps64.has_int64 = true;
    CHECK_OK(air2spv_compile_bitcode(bc_mdiv.data(), bc_mdiv.size(), "magic_div_u32", &caps64, &res,
                                     err, sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "magic_div_u32_i64");
    std::free(res.spirv);

    /* shl i64 32 + ashr i64 32 (32-bit sign-extension idiom in kernel_cpy_f32_f32) with zero Int64/shift UB. */
    const char *ir_sext32 = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-i64:64:64"
define void @sext32_idiom(ptr addrspace(1) %dst, i32 %gid) {
entry:
  %a = sext i32 %gid to i64
  %s = shl i64 %a, 32
  %e = ashr exact i64 %s, 32
  %dp = getelementptr inbounds i32, ptr addrspace(1) %dst, i64 %e
  store i32 %gid, ptr addrspace(1) %dp, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @sext32_idiom, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR";
    auto bc_sx = assemble_ir_to_bitcode(ir_sext32);
    CHECK_OK(air2spv_compile_bitcode(bc_sx.data(), bc_sx.size(), "sext32_idiom", &caps, &res,
                                     err, sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "sext32_idiom");
    std::free(res.spirv);

    /* Unmasked ptrtoint comparison must NOT narrow (discarding hi32) on !has_int64 GPUs. */
    const char *ir_p2i_cmp = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-i64:64:64"
define void @ptrtoint_cmp(ptr addrspace(1) %a, ptr addrspace(1) %b, ptr addrspace(1) %dst, i32 %gid) {
entry:
  %ia = ptrtoint ptr addrspace(1) %a to i64
  %ib = ptrtoint ptr addrspace(1) %b to i64
  %eq = icmp eq i64 %ia, %ib
  %v = zext i1 %eq to i32
  %dp = getelementptr inbounds i32, ptr addrspace(1) %dst, i32 %gid
  store i32 %v, ptr addrspace(1) %dp, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @ptrtoint_cmp, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.thread_position_in_grid"}
)IR";
    auto bc_p2i_cmp = assemble_ir_to_bitcode(ir_p2i_cmp);
    assert(!air2spv_compile_bitcode(bc_p2i_cmp.data(), bc_p2i_cmp.size(), "ptrtoint_cmp", &caps,
                                    &res, err, sizeof(err)));
    CHECK_OK(air2spv_compile_bitcode(bc_p2i_cmp.data(), bc_p2i_cmp.size(), "ptrtoint_cmp", &caps64,
                                     &res, err, sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "ptrtoint_cmp_i64");
    std::free(res.spirv);

    /* Software BF16 math + dot + 32-lane emulated subgroup reduce/scan/shuffle/barrier on subgroup_size=16. */
    const char *ir_bf16_sg = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i16:16:16-i32:32:32-f32:32:32"
declare bfloat @air.fast_exp.bf16(bfloat)
declare bfloat @air.fma.bf16(bfloat, bfloat, bfloat)
declare bfloat @air.dot.v2bf16(<2 x bfloat>, <2 x bfloat>)
declare bfloat @air.simd_sum.bf16(bfloat)
declare float @air.simd_prefix_exclusive_sum.f32(float)
declare float @air.simd_shuffle_down.f32(float, i16)
declare void @air.simdgroup.barrier(i32, i32)
define void @bf16_sg16_ops(ptr addrspace(1) %in, ptr addrspace(1) %out, i32 %gid) {
entry:
  %ip = getelementptr inbounds <2 x bfloat>, ptr addrspace(1) %in, i32 %gid
  %v2 = load <2 x bfloat>, ptr addrspace(1) %ip, align 4
  %d = call bfloat @air.dot.v2bf16(<2 x bfloat> %v2, <2 x bfloat> %v2)
  %e = call bfloat @air.fast_exp.bf16(bfloat %d)
  %fm = call bfloat @air.fma.bf16(bfloat %e, bfloat %d, bfloat %e)
  %sm = call bfloat @air.simd_sum.bf16(bfloat %fm)
  call void @air.simdgroup.barrier(i32 2, i32 0)
  %sf = fpext bfloat %sm to float
  %ps = call float @air.simd_prefix_exclusive_sum.f32(float %sf)
  %sh = call float @air.simd_shuffle_down.f32(float %ps, i16 16)
  %res = fptrunc float %sh to bfloat
  %op = getelementptr inbounds bfloat, ptr addrspace(1) %out, i32 %gid
  store bfloat %res, ptr addrspace(1) %op, align 2
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bf16_sg16_ops, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read_write", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.thread_position_in_grid"}
)IR";
    auto bc_bfsg = assemble_ir_to_bitcode(ir_bf16_sg);
    air2spv_caps_t caps16 = caps;
    caps16.subgroup_size = 16;
    CHECK_OK(air2spv_compile_bitcode(bc_bfsg.data(), bc_bfsg.size(), "bf16_sg16_ops", &caps16, &res,
                                     err, sizeof(err)), err);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "bf16_sg16_ops");
    std::free(res.spirv);

    /* Loop-carried dual-root pointer PHI initialized from a two-buffer select (canonical root order). */
    const char *ir_sel_phi = R"IR(
target datalayout = "e-p:64:64:64-i1:8:8-i32:32:32-f32:32:32"
define void @dual_select_loop_phi(ptr addrspace(1) %a, ptr addrspace(1) %b, ptr addrspace(1) %dst, i32 %gid) {
entry:
  %c = icmp eq i32 %gid, 0
  %p0 = select i1 %c, ptr addrspace(1) %b, ptr addrspace(1) %a
  br label %loop
loop:
  %i = phi i32 [ 0, %entry ], [ %ni, %loop ]
  %p = phi ptr addrspace(1) [ %p0, %entry ], [ %np, %loop ]
  %v = load float, ptr addrspace(1) %p, align 4
  %dp = getelementptr inbounds float, ptr addrspace(1) %dst, i32 %i
  store float %v, ptr addrspace(1) %dp, align 4
  %np = getelementptr inbounds float, ptr addrspace(1) %p, i32 1
  %ni = add i32 %i, 1
  %more = icmp ult i32 %ni, 4
  br i1 %more, label %loop, label %exit
exit:
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @dual_select_loop_phi, !1, !2}
!1 = !{}
!2 = !{!3, !4, !5, !6}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.buffer", !"air.location_index", i32 1, i32 1, !"air.read", !"air.address_space", i32 1}
!5 = !{i32 2, !"air.buffer", !"air.location_index", i32 2, i32 1, !"air.read_write", !"air.address_space", i32 1}
!6 = !{i32 3, !"air.thread_position_in_grid"}
)IR";
    auto bc_sphi = assemble_ir_to_bitcode(ir_sel_phi);
    CHECK_OK(air2spv_compile_bitcode(bc_sphi.data(), bc_sphi.size(), "dual_select_loop_phi", &caps,
                                     &res, err, sizeof(err)), err);
    assert(res.binding_mask == 0x7u);
    assert(res.readonly_mask == 0x3u);
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, "dual_select_loop_phi");
    std::free(res.spirv);
  }
#undef CHECK_OK

  /* Negative compiler checks must fail cleanly without crashing, asserting, or hanging. */
  assert(!air2spv_compile_bitcode(bc_bad_slot.data(), bc_bad_slot.size(), "bad_slot", &caps, &res,
                                  err, sizeof(err)));
  assert(!air2spv_compile_bitcode(bc_bad_i24.data(), bc_bad_i24.size(), "bad_i24", &caps, &res,
                                  err, sizeof(err)));
  uint8_t garbage[64] = {0x42, 0x43, 0xC0, 0xDE, 1, 2, 3, 4};
  assert(!air2spv_compile_bitcode(garbage, sizeof(garbage), "x", &caps, &res, err, sizeof(err)));

  /* Hostile adversarial bitcode suite covering T2-2..T2-6. */
  const char *hostile_cases[][2] = {
      /* i128 constant (must not assert in getZExtValue or shift UB in get_const_int) */
      {"bad_i128", R"IR(
define void @bad_i128(ptr addrspace(1) %p) {
  %x = add i128 0, 18446744073709551616
  %t = trunc i128 %x to i32
  store i32 %t, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_i128, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Missing intrinsic operand (must not OOB read getArgOperand) */
      {"bad_ctpop_args", R"IR(
declare i32 @llvm.ctpop.i32()
define void @bad_ctpop_args(ptr addrspace(1) %p) {
  %v = call i32 @llvm.ctpop.i32()
  store i32 %v, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_ctpop_args, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Void-returning math intrinsic */
      {"bad_void_sin", R"IR(
declare void @air.sin.f32(float)
define void @bad_void_sin(ptr addrspace(1) %p) {
  call void @air.sin.f32(float 1.000000e+00)
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_void_sin, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Boolean arithmetic (i1 add) */
      {"bad_i1_add", R"IR(
define void @bad_i1_add(ptr addrspace(1) %p, i32 %gid) {
  %c = icmp eq i32 %gid, 0
  %b = add i1 %c, true
  %z = zext i1 %b to i32
  store i32 %z, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_i1_add, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR"},
      /* Out-of-bounds constant extractelement */
      {"bad_extract_oob", R"IR(
define void @bad_extract_oob(ptr addrspace(1) %p) {
  %v = extractelement <2 x float> <float 1.0, float 2.0>, i32 9
  store float %v, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_extract_oob, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Duplicate SpecId in !air.function_constants */
      {"bad_dup_specid", R"IR(
@fc0 = external addrspace(2) constant float
@fc1 = external addrspace(2) constant float
define void @bad_dup_specid(ptr addrspace(1) %p) {
  %a = load float, ptr addrspace(2) @fc0, align 4
  %b = load float, ptr addrspace(2) @fc1, align 4
  %s = fadd float %a, %b
  store float %s, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!air.function_constants = !{!10, !11}
!0 = !{ptr @bad_dup_specid, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!10 = !{ptr addrspace(2) @fc0, !"air.location_index", i32 5}
!11 = !{ptr addrspace(2) @fc1, !"air.location_index", i32 5}
)IR"},
      /* Mutual recursion inline bomb (A <-> B) */
      {"bad_mutual_rec", R"IR(
define internal void @helper_a(ptr addrspace(1) %p) {
  call void @helper_b(ptr addrspace(1) %p)
  ret void
}
define internal void @helper_b(ptr addrspace(1) %p) {
  call void @helper_a(ptr addrspace(1) %p)
  ret void
}
define void @bad_mutual_rec(ptr addrspace(1) %p) {
  call void @helper_a(ptr addrspace(1) %p)
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_mutual_rec, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Exponential nested struct load bomb (4^8 = 65536 scalars > budget 256) */
      {"bad_struct_bomb", R"IR(
%S0 = type { float, float, float, float }
%S1 = type { %S0, %S0, %S0, %S0 }
%S2 = type { %S1, %S1, %S1, %S1 }
%S3 = type { %S2, %S2, %S2, %S2 }
%S4 = type { %S3, %S3, %S3, %S3 }
define void @bad_struct_bomb(ptr addrspace(1) %p) {
  %v = load %S4, ptr addrspace(1) %p, align 4
  store %S4 %v, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_struct_bomb, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Non-integer builtin type (<3 x float>) */
      {"bad_builtin_fp", R"IR(
define void @bad_builtin_fp(ptr addrspace(1) %p, <3 x float> %gid) {
  %x = extractelement <3 x float> %gid, i32 0
  store float %x, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_builtin_fp, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR"},
      /* Empty-struct nested array store bomb (must hit scalar_budget instantly) */
      {"bad_empty_agg", R"IR(
%E = type {}
define void @bad_empty_agg(ptr addrspace(1) %p) {
  store [64 x [64 x [64 x %E]]] zeroinitializer, ptr addrspace(1) %p, align 1
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_empty_agg, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
)IR"},
      /* Struct passed to subgroup shuffle (illegal in SPIR-V) */
      {"bad_shuffle_struct", R"IR(
%Pair = type { float, float }
declare %Pair @air.simd_shuffle.s(%Pair, i32)
define void @bad_shuffle_struct(ptr addrspace(1) %p, i32 %gid) {
  %v = load %Pair, ptr addrspace(1) %p, align 4
  %s = call %Pair @air.simd_shuffle.s(%Pair %v, i32 %gid)
  store %Pair %s, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!0 = !{ptr @bad_shuffle_struct, !1, !2}
!1 = !{}
!2 = !{!3, !4}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!4 = !{i32 1, !"air.thread_position_in_grid"}
)IR"},
      /* Mismatched load type from function constant global */
      {"bad_fc_type_mismatch", R"IR(
@fc0 = external addrspace(2) constant i32
define void @bad_fc_type_mismatch(ptr addrspace(1) %p) {
  %v = load float, ptr addrspace(2) @fc0, align 4
  store float %v, ptr addrspace(1) %p, align 4
  ret void
}
!air.kernel = !{!0}
!air.function_constants = !{!10}
!0 = !{ptr @bad_fc_type_mismatch, !1, !2}
!1 = !{}
!2 = !{!3}
!3 = !{i32 0, !"air.buffer", !"air.location_index", i32 0, i32 1, !"air.read_write", !"air.address_space", i32 1}
!10 = !{ptr addrspace(2) @fc0, !"air.location_index", i32 0}
)IR"},
  };
  for (const auto &tc : hostile_cases) {
    auto hbc = assemble_ir_to_bitcode(tc[1]);
    if (air2spv_compile_bitcode(hbc.data(), hbc.size(), tc[0], &caps, &res, err, sizeof(err))) {
      std::fprintf(stderr, "air2spv_test: FAIL hostile case %s unexpectedly compiled!\n", tc[0]);
      std::free(res.spirv);
      std::abort();
    }
  }
}

void test_external_metallib_file(const char *path) {
  std::ifstream f(path, std::ios::binary);
  assert(f.good());
  std::vector<uint8_t> data((std::istreambuf_iterator<char>(f)), std::istreambuf_iterator<char>());
  char err[512] = {0};
  metallib_index_t idx{};
  if (!metallib_parse(data.data(), data.size(), &idx, err, sizeof(err))) {
    std::fprintf(stderr, "air2spv_test: FAIL metallib_parse(%s): %s\n", path, err);
    std::abort();
  }
  /* Test with has_int64 = false (matches V3D 7.1) to prove zero Int64 leakage from 64-bit GEPs. */
  const uint32_t all_sg = AIR2SPV_SUBGROUP_BASIC | AIR2SPV_SUBGROUP_VOTE |
                          AIR2SPV_SUBGROUP_ARITHMETIC | AIR2SPV_SUBGROUP_BALLOT |
                          AIR2SPV_SUBGROUP_SHUFFLE | AIR2SPV_SUBGROUP_SHUFFLE_RELATIVE;
  air2spv_caps_t caps = {true, true, true, true, true, false, 32768, all_sg, 32};
  size_t compiled = 0;
  for (size_t i = 0; i < idx.count; i++) {
    if (idx.funcs[i].fn_type != METALLIB_FN_COMPUTE) continue;
    air2spv_result_t res{};
    if (!air2spv_compile_bitcode(idx.funcs[i].bitcode, idx.funcs[i].bitcode_len, idx.funcs[i].name,
                                 &caps, &res, err, sizeof(err))) {
      std::fprintf(stderr, "air2spv_test: FAIL compiling %s::%s: %s\n", path, idx.funcs[i].name,
                   err);
      std::abort();
    }
    validate_spirv_with_tool(res.spirv, res.spirv_bytes, idx.funcs[i].name);
    std::printf("  [OK] %s::%s -> %zu B SPIR-V (bindings=0x%x ro=0x%x)\n", path, idx.funcs[i].name,
                res.spirv_bytes, res.binding_mask, res.readonly_mask);
    std::free(res.spirv);
    compiled++;
  }
  metallib_index_free(&idx);
  assert(compiled > 0);
}

} // namespace

int main(int argc, char **argv) {
  test_metallib_parser_negative();
  test_air_kernels_and_spirv_val();
  for (int i = 1; i < argc; i++) {
    test_external_metallib_file(argv[i]);
  }
  std::printf("air2spv_test: all checks passed\n");
  return 0;
}
