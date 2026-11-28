/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "air2spv.h"
#include "metallib.h"

#include <llvm/Analysis/LoopInfo.h>
#include <llvm/Analysis/PostDominators.h>
#include <llvm/Bitcode/BitcodeReader.h>
#include <llvm/IR/Constants.h>
#include <llvm/IR/DataLayout.h>
#include <llvm/IR/Dominators.h>
#include <llvm/IR/Function.h>
#include <llvm/IR/GlobalVariable.h>
#include <llvm/IR/Instructions.h>
#include <llvm/IR/Intrinsics.h>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Metadata.h>
#include <llvm/IR/Module.h>
#include <llvm/IR/Operator.h>
#include <llvm/IR/PassManager.h>
#include <llvm/IR/Verifier.h>
#include <llvm/Passes/PassBuilder.h>
#include <llvm/Support/MemoryBuffer.h>
#include <llvm/Transforms/Scalar/StructurizeCFG.h>
#include <llvm/Transforms/Utils/BasicBlockUtils.h>
#include <llvm/Transforms/Utils/Cloning.h>
#include <llvm/Transforms/Utils/LoopSimplify.h>
#include <llvm/Transforms/Utils/LowerSwitch.h>
#include <llvm/Transforms/Utils/Mem2Reg.h>
#include <llvm/Transforms/Utils/UnifyFunctionExitNodes.h>

#include <spirv/unified1/GLSL.std.450.h>
#include <spirv/unified1/spirv.hpp>

#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

namespace {

constexpr uint32_t kSpvVersion16 = 0x00010600u;
constexpr uint32_t kMaxBufferSlots = 31u;
constexpr size_t kMaxBlocks = 4096u;
constexpr size_t kMaxInsts = 65536u;
constexpr size_t kMaxSpirvWords = 2u * 1024u * 1024u;
constexpr uint32_t kMaxWorkgroupBytes = 65536u;
constexpr uint32_t kMaxAllocaBytes = 65536u;
constexpr uint32_t kDefaultDynWgBytes = 16384u;
constexpr int kMaxInlineDepth = 16;
constexpr size_t kMaxStructMembers = 256u;
constexpr int kMaxPtrDepth = 64;
constexpr int kMaxScalarOpsPerMem = 256;
constexpr size_t kMaxSwitchCasesSingle = 256u;
constexpr size_t kMaxSwitchCasesTotal = 1024u;
constexpr size_t kMaxReachableHelpers = 256u;

void set_err(char *err_buf, size_t err_cap, const std::string &msg) {
  if (err_buf && err_cap > 0) {
    std::snprintf(err_buf, err_cap, "%s", msg.c_str());
  }
}

bool try_get_u64(const llvm::ConstantInt *ci, uint64_t &out) {
  if (!ci || ci->getBitWidth() == 0 || ci->getBitWidth() > 64) return false;
  out = ci->getZExtValue();
  return true;
}

bool try_get_s64(const llvm::ConstantInt *ci, int64_t &out) {
  if (!ci || ci->getBitWidth() == 0 || ci->getBitWidth() > 64) return false;
  out = ci->getSExtValue();
  return true;
}

bool try_get_u32(const llvm::ConstantInt *ci, uint32_t &out) {
  uint64_t v = 0;
  if (!try_get_u64(ci, v) || v > UINT32_MAX) return false;
  out = static_cast<uint32_t>(v);
  return true;
}

bool try_type_alloc_bytes(const llvm::DataLayout &dl, llvm::Type *ty, uint64_t &out) {
  if (!ty || !ty->isSized()) return false;
  if (auto *st = llvm::dyn_cast<llvm::StructType>(ty)) {
    if (st->isOpaque()) return false;
  }
  llvm::TypeSize ts = dl.getTypeAllocSize(ty);
  if (ts.isScalable()) return false;
  out = ts.getFixedValue();
  return true;
}

enum class ElemKind : uint8_t {
  U8,
  U16,
  F16,
  U32,
  F32,
};

uint32_t elem_size_bytes(ElemKind k) {
  switch (k) {
  case ElemKind::U8: return 1;
  case ElemKind::U16:
  case ElemKind::F16: return 2;
  case ElemKind::U32:
  case ElemKind::F32: return 4;
  }
  return 4;
}

enum class BuiltinKind : uint8_t {
  None,
  GlobalInvocationId,
  LocalInvocationId,
  WorkgroupId,
  WorkgroupSize,
  NumWorkgroups,
  GridSize,
  LocalInvocationIndex,
  SubgroupLocalInvocationId,
  SubgroupSize,
  SubgroupId,
  NumSubgroups,
};

enum class PtrKind : uint8_t {
  Invalid,
  Buffer,
  Workgroup,
  Function,
  FuncConst,
};

struct PtrInfo {
  PtrKind kind = PtrKind::Invalid;
  uint32_t base_id = 0;       /* buffer slot (0..30), wg array index, alloca index, or fc spec_id */
  uint32_t byte_off_id = 0;   /* SPIR-V u32 SSA ID for byte offset from base */
  uint32_t idx_id = 0;        /* Optional deferred scaled element index (0 = none) */
  uint32_t idx_scale = 0;     /* Byte stride for idx_id */
  uint32_t sel_cond_id = 0;   /* Optional SPIR-V bool SSA ID for two-root pointer select */
  PtrKind alt_kind = PtrKind::Invalid;
  uint32_t alt_base_id = 0;
  uint32_t alt_byte_off_id = 0;

  bool is_dual() const { return sel_cond_id != 0; }
  PtrInfo primary_arm() const {
    return PtrInfo{kind, base_id, byte_off_id, idx_id, idx_scale, 0, PtrKind::Invalid, 0, 0};
  }
  PtrInfo alt_arm() const {
    return PtrInfo{alt_kind, alt_base_id, alt_byte_off_id, idx_id, idx_scale, 0, PtrKind::Invalid, 0, 0};
  }
};

struct KernelArgMeta {
  bool is_buffer = false;
  uint32_t slot = 0;
  uint32_t addr_space = 0;
  bool is_readonly = false;
  bool is_writable = false;
  BuiltinKind builtin = BuiltinKind::None;
};

struct BufferBinding {
  bool used = false;
  bool readonly = false;
  bool has_store = false;
  bool has_atomic = false;
  bool all_half = true;
  bool all_f32 = true;
  bool saw_access = false;
  uint32_t min_store_bytes = 4;
  uint32_t min_access_bytes = 4;
  ElemKind elem = ElemKind::U32;
  uint32_t var_id = 0;
  uint32_t ptr_elem_ty = 0;
};

struct MemArrayInfo {
  uint32_t var_id = 0;
  uint32_t ptr_elem_ty = 0;
  ElemKind elem = ElemKind::U32;
  uint32_t total_bytes = 0;
  spv::StorageClass storage = spv::StorageClassWorkgroup;
};

struct FuncConstInfo {
  uint32_t spec_id = 0;
  llvm::Type *type = nullptr;
  uint32_t spv_id = 0;
  bool is_air_bool = false;
};

struct SpvEmitter {
  uint32_t next_id = 1;
  std::set<spv::Capability> caps;
  std::vector<uint32_t> annotations;
  std::vector<uint32_t> types_consts_vars;
  std::vector<uint32_t> interface_vars;
  std::vector<uint32_t> entry_vars;

  uint32_t glsl_ext_id = 0;
  uint32_t main_fn_id = 0;
  uint32_t spec_lx = 0;
  uint32_t spec_ly = 0;
  uint32_t spec_lz = 0;
  uint32_t wg_size_v3 = 0;
  uint32_t pc_var_id = 0;
  uint32_t pc_ptr_u32_ty = 0;
  uint32_t sg32_f32_var = 0;
  uint32_t sg32_f32_ptr_ty = 0;
  uint32_t sg32_u32_var = 0;
  uint32_t sg32_u32_ptr_ty = 0;
  uint32_t sg32_shuf_f32_var = 0;
  uint32_t sg32_shuf_f32_ptr_ty = 0;
  uint32_t sg32_shuf_u32_var = 0;
  uint32_t sg32_shuf_u32_ptr_ty = 0;

  /* Type deduplication */
  uint32_t ty_void = 0;
  uint32_t ty_bool = 0;
  uint32_t ty_fn_void = 0;
  std::map<uint32_t, uint32_t> ty_int;
  std::map<uint32_t, uint32_t> ty_float;
  std::map<std::pair<uint32_t, uint32_t>, uint32_t> ty_vec;
  std::map<std::pair<uint32_t, uint32_t>, uint32_t> ty_ptr; /* (storage, pointee) */
  std::map<std::pair<uint32_t, uint32_t>, uint32_t> ty_arr; /* (elem_ty, count) */
  std::map<uint32_t, uint32_t> ty_rt_arr;
  std::map<std::vector<uint32_t>, uint32_t> ty_struct;
  std::map<std::pair<uint32_t, uint64_t>, uint32_t> const_int;
  std::map<std::pair<uint32_t, uint64_t>, uint32_t> const_fp;
  uint32_t const_true = 0;
  uint32_t const_false = 0;
  std::map<std::pair<uint32_t, std::vector<uint32_t>>, uint32_t> const_comp;
  std::map<uint32_t, uint32_t> undef_map;
  std::map<uint32_t, uint32_t> null_map;
  std::unordered_map<const llvm::Type *, uint32_t> ty_memo;
  std::unordered_map<const llvm::Constant *, uint32_t> const_memo;
  size_t total_emitted_words = 0;

  air2spv_caps_t hw_caps{};
  std::string error;

  bool fail(const std::string &msg) {
    if (error.empty()) error = msg;
    return false;
  }

  uint32_t alloc_id() { return next_id++; }

  bool check_op_budget(const std::vector<uint32_t> &out, size_t nargs) {
    if (!error.empty()) return false;
    if (nargs >= 0xFFFFu) {
      fail("SPIR-V instruction exceeds 16-bit word count limit");
      return false;
    }
    size_t wc = 1u + nargs;
    if (wc > kMaxSpirvWords || total_emitted_words > kMaxSpirvWords - wc ||
        out.size() > kMaxSpirvWords - wc) {
      fail("SPIR-V output exceeds word budget");
      return false;
    }
    total_emitted_words += wc;
    return true;
  }

  void emit_op(std::vector<uint32_t> &out, spv::Op op, std::initializer_list<uint32_t> args) {
    if (!check_op_budget(out, args.size())) return;
    uint32_t wc = static_cast<uint32_t>(1u + args.size());
    out.push_back((wc << 16) | static_cast<uint32_t>(op));
    for (uint32_t a : args) out.push_back(a);
  }

  void emit_op_vec(std::vector<uint32_t> &out, spv::Op op, const std::vector<uint32_t> &args) {
    if (!check_op_budget(out, args.size())) return;
    uint32_t wc = static_cast<uint32_t>(1u + args.size());
    out.push_back((wc << 16) | static_cast<uint32_t>(op));
    out.insert(out.end(), args.begin(), args.end());
  }

  static void append_string(std::vector<uint32_t> &out, const char *s) {
    size_t len = std::strlen(s) + 1u;
    size_t words = (len + 3u) / 4u;
    for (size_t w = 0; w < words; w++) {
      uint32_t v = 0;
      for (size_t b = 0; b < 4; b++) {
        size_t idx = w * 4u + b;
        if (idx < len) v |= static_cast<uint32_t>(static_cast<uint8_t>(s[idx])) << (b * 8u);
      }
      out.push_back(v);
    }
  }

  void decorate(uint32_t target, spv::Decoration dec, std::initializer_list<uint32_t> lits = {}) {
    std::vector<uint32_t> args = {target, static_cast<uint32_t>(dec)};
    for (uint32_t x : lits) args.push_back(x);
    emit_op_vec(annotations, spv::OpDecorate, args);
  }

  void member_decorate(uint32_t struct_ty,
                       uint32_t member,
                       spv::Decoration dec,
                       std::initializer_list<uint32_t> lits = {}) {
    std::vector<uint32_t> args = {struct_ty, member, static_cast<uint32_t>(dec)};
    for (uint32_t x : lits) args.push_back(x);
    emit_op_vec(annotations, spv::OpMemberDecorate, args);
  }

  uint32_t get_void() {
    if (!ty_void) {
      ty_void = alloc_id();
      emit_op(types_consts_vars, spv::OpTypeVoid, {ty_void});
    }
    return ty_void;
  }

  uint32_t get_bool() {
    if (!ty_bool) {
      ty_bool = alloc_id();
      emit_op(types_consts_vars, spv::OpTypeBool, {ty_bool});
    }
    return ty_bool;
  }

  uint32_t get_fn_void() {
    if (!ty_fn_void) {
      uint32_t v = get_void();
      ty_fn_void = alloc_id();
      emit_op(types_consts_vars, spv::OpTypeFunction, {ty_fn_void, v});
    }
    return ty_fn_void;
  }

  uint32_t get_int(uint32_t bits) {
    auto it = ty_int.find(bits);
    if (it != ty_int.end()) return it->second;
    if (bits == 8) {
      if (!hw_caps.has_int8) {
        fail("shader requires Int8 capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityInt8);
    } else if (bits == 16) {
      if (!hw_caps.has_int16) {
        fail("shader requires Int16 capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityInt16);
    } else if (bits == 64) {
      if (!hw_caps.has_int64) {
        fail("shader requires Int64 capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityInt64);
    } else if (bits != 32) {
      fail("unsupported integer bit width in AIR");
      return 0;
    }
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeInt, {id, bits, 0u});
    ty_int[bits] = id;
    return id;
  }

  uint32_t get_float(uint32_t bits) {
    auto it = ty_float.find(bits);
    if (it != ty_float.end()) return it->second;
    if (bits == 16) {
      if (!hw_caps.has_float16) {
        fail("shader requires Float16 capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityFloat16);
    } else if (bits != 32) {
      fail("unsupported floating-point bit width in AIR");
      return 0;
    }
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeFloat, {id, bits});
    ty_float[bits] = id;
    return id;
  }

  uint32_t get_vec(uint32_t elem_ty, uint32_t count) {
    if (!elem_ty) return 0;
    auto key = std::make_pair(elem_ty, count);
    auto it = ty_vec.find(key);
    if (it != ty_vec.end()) return it->second;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeVector, {id, elem_ty, count});
    ty_vec[key] = id;
    return id;
  }

  uint32_t get_ptr(spv::StorageClass sc, uint32_t pointee) {
    if (!pointee) return 0;
    auto key = std::make_pair(static_cast<uint32_t>(sc), pointee);
    auto it = ty_ptr.find(key);
    if (it != ty_ptr.end()) return it->second;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypePointer, {id, static_cast<uint32_t>(sc), pointee});
    ty_ptr[key] = id;
    return id;
  }

  uint32_t get_const_u32(uint32_t v) { return get_const_int(32, v); }

  uint32_t get_const_int(uint32_t bits, uint64_t val) {
    uint32_t ty = get_int(bits);
    if (!ty) return 0;
    uint64_t mask = (bits == 64) ? ~0ULL : ((1ULL << bits) - 1ULL);
    val &= mask;
    auto key = std::make_pair(bits, val);
    auto it = const_int.find(key);
    if (it != const_int.end()) return it->second;
    uint32_t id = alloc_id();
    if (bits <= 32) {
      emit_op(types_consts_vars, spv::OpConstant, {ty, id, static_cast<uint32_t>(val)});
    } else {
      emit_op(types_consts_vars, spv::OpConstant,
              {ty, id, static_cast<uint32_t>(val & 0xFFFFFFFFu),
               static_cast<uint32_t>(val >> 32)});
    }
    const_int[key] = id;
    return id;
  }

  uint32_t get_const_bool(bool v) {
    if (v) {
      if (!const_true) {
        uint32_t ty = get_bool();
        const_true = alloc_id();
        emit_op(types_consts_vars, spv::OpConstantTrue, {ty, const_true});
      }
      return const_true;
    } else {
      if (!const_false) {
        uint32_t ty = get_bool();
        const_false = alloc_id();
        emit_op(types_consts_vars, spv::OpConstantFalse, {ty, const_false});
      }
      return const_false;
    }
  }

  uint32_t get_const_fp_bits(uint32_t bits, uint64_t raw_bits) {
    uint64_t mask = (bits == 16) ? 0xFFFFULL : 0xFFFFFFFFULL;
    raw_bits &= mask;
    auto key = std::make_pair(bits, raw_bits);
    auto it = const_fp.find(key);
    if (it != const_fp.end()) return it->second;
    uint32_t ty = get_float(bits);
    if (!ty) return 0;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpConstant, {ty, id, static_cast<uint32_t>(raw_bits)});
    const_fp[key] = id;
    return id;
  }

  uint32_t get_const_f32(float f) {
    uint32_t u = 0;
    std::memcpy(&u, &f, 4);
    return get_const_fp_bits(32, u);
  }

  uint32_t get_array(uint32_t elem_ty, uint32_t count) {
    if (!elem_ty) return 0;
    auto key = std::make_pair(elem_ty, count);
    auto it = ty_arr.find(key);
    if (it != ty_arr.end()) return it->second;
    uint32_t len_c = get_const_u32(count);
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeArray, {id, elem_ty, len_c});
    ty_arr[key] = id;
    return id;
  }

  uint32_t get_runtime_array(uint32_t elem_ty, uint32_t stride) {
    if (!elem_ty) return 0;
    auto it = ty_rt_arr.find(elem_ty);
    if (it != ty_rt_arr.end()) return it->second;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeRuntimeArray, {id, elem_ty});
    decorate(id, spv::DecorationArrayStride, {stride});
    ty_rt_arr[elem_ty] = id;
    return id;
  }

  uint32_t get_elem_spv_type(ElemKind k) {
    switch (k) {
    case ElemKind::U8: return get_int(8);
    case ElemKind::U16: return get_int(16);
    case ElemKind::F16: return get_float(16);
    case ElemKind::U32: return get_int(32);
    case ElemKind::F32: return get_float(32);
    }
    return get_int(32);
  }

  uint32_t get_undef(uint32_t ty) {
    if (!ty) return 0;
    auto it = undef_map.find(ty);
    if (it != undef_map.end()) return it->second;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpUndef, {ty, id});
    undef_map[ty] = id;
    return id;
  }

  uint32_t get_null(uint32_t ty) {
    if (!ty) return 0;
    auto it = null_map.find(ty);
    if (it != null_map.end()) return it->second;
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpConstantNull, {ty, id});
    null_map[ty] = id;
    return id;
  }

  uint32_t map_llvm_type(llvm::Type *ty, int depth = 0) {
    if (!ty || depth > 16) {
      fail("type nesting too deep or null");
      return 0;
    }
    if (auto mit = ty_memo.find(ty); mit != ty_memo.end()) return mit->second;
    uint32_t result = 0;
    if (ty->isVoidTy()) result = get_void();
    else if (ty->isIntegerTy(1)) result = get_bool();
    else if (ty->isIntegerTy()) result = get_int(ty->getIntegerBitWidth());
    else if (ty->isBFloatTy()) result = get_int(16);
    else if (ty->isHalfTy()) result = get_float(16);
    else if (ty->isFloatTy()) result = get_float(32);
    else if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ty)) {
      uint32_t n = vt->getNumElements();
      if (n < 2 || n > 4) {
        fail("unsupported vector width (only 2/3/4 supported)");
        return 0;
      }
      result = get_vec(map_llvm_type(vt->getElementType(), depth + 1), n);
    } else if (auto *at = llvm::dyn_cast<llvm::ArrayType>(ty)) {
      uint64_t n = at->getNumElements();
      if (n == 0 || n > 4096) {
        fail("unsupported aggregate array size");
        return 0;
      }
      result = get_array(map_llvm_type(at->getElementType(), depth + 1), static_cast<uint32_t>(n));
    } else if (auto *st = llvm::dyn_cast<llvm::StructType>(ty)) {
      if (st->isOpaque()) {
        fail("opaque struct type not supported in AIR");
        return 0;
      }
      if (st->getNumElements() > kMaxStructMembers) {
        fail("struct member count exceeds limit");
        return 0;
      }
      std::vector<uint32_t> members;
      members.reserve(st->getNumElements());
      for (unsigned i = 0; i < st->getNumElements(); i++) {
        uint32_t m = map_llvm_type(st->getElementType(i), depth + 1);
        if (!m) return 0;
        members.push_back(m);
      }
      auto it = ty_struct.find(members);
      if (it != ty_struct.end()) {
        result = it->second;
      } else {
        uint32_t id = alloc_id();
        std::vector<uint32_t> args;
        args.push_back(id);
        args.insert(args.end(), members.begin(), members.end());
        emit_op_vec(types_consts_vars, spv::OpTypeStruct, args);
        if (!error.empty()) return 0;
        ty_struct[members] = id;
        result = id;
      }
    } else {
      fail("unsupported LLVM type in AIR shader");
      return 0;
    }
    if (result) ty_memo[ty] = result;
    return result;
  }

  uint32_t emit_constant(llvm::Constant *c, int depth = 0) {
    if (!error.empty()) return 0;
    if (!c || depth > 16) {
      fail("constant nesting too deep");
      return 0;
    }
    if (auto mit = const_memo.find(c); mit != const_memo.end()) return mit->second;
    llvm::Type *ty = c->getType();
    if (llvm::isa<llvm::UndefValue>(c) || llvm::isa<llvm::PoisonValue>(c)) {
      uint32_t id = get_undef(map_llvm_type(ty));
      if (id) const_memo[c] = id;
      return id;
    }
    if (llvm::isa<llvm::ConstantAggregateZero>(c)) {
      uint32_t id = get_null(map_llvm_type(ty));
      if (id) const_memo[c] = id;
      return id;
    }
    if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(c)) {
      if (ci->getBitWidth() == 1) {
        uint32_t id = get_const_bool(!ci->isZero());
        const_memo[c] = id;
        return id;
      }
      uint64_t val = 0;
      if (!try_get_u64(ci, val)) {
        fail("unsupported integer constant bit width in AIR");
        return 0;
      }
      uint32_t id = get_const_int(ci->getBitWidth(), val);
      if (id) const_memo[c] = id;
      return id;
    }
    if (auto *cf = llvm::dyn_cast<llvm::ConstantFP>(c)) {
      llvm::APInt bits = cf->getValueAPF().bitcastToAPInt();
      if (ty->isBFloatTy()) {
        uint32_t id = get_const_int(16, bits.getZExtValue());
        if (id) const_memo[c] = id;
        return id;
      }
      if (ty->isHalfTy()) {
        uint32_t id = get_const_fp_bits(16, bits.getZExtValue());
        if (id) const_memo[c] = id;
        return id;
      }
      if (ty->isFloatTy()) {
        uint32_t id = get_const_fp_bits(32, bits.getZExtValue());
        if (id) const_memo[c] = id;
        return id;
      }
      fail("unsupported floating-point constant width");
      return 0;
    }
    if (auto *cdv = llvm::dyn_cast<llvm::ConstantDataSequential>(c)) {
      uint32_t spv_ty = map_llvm_type(ty);
      if (!spv_ty) return 0;
      std::vector<uint32_t> elems;
      unsigned n = cdv->getNumElements();
      elems.reserve(n);
      for (unsigned i = 0; i < n; i++) {
        uint32_t e = emit_constant(cdv->getElementAsConstant(i), depth + 1);
        if (!e) return 0;
        elems.push_back(e);
      }
      auto key = std::make_pair(spv_ty, elems);
      auto it = const_comp.find(key);
      if (it != const_comp.end()) {
        const_memo[c] = it->second;
        return it->second;
      }
      uint32_t id = alloc_id();
      std::vector<uint32_t> args = {spv_ty, id};
      args.insert(args.end(), elems.begin(), elems.end());
      emit_op_vec(types_consts_vars, spv::OpConstantComposite, args);
      if (!error.empty()) return 0;
      const_comp[key] = id;
      const_memo[c] = id;
      return id;
    }
    if (auto *ca = llvm::dyn_cast<llvm::ConstantAggregate>(c)) {
      uint32_t spv_ty = map_llvm_type(ty);
      if (!spv_ty) return 0;
      std::vector<uint32_t> elems;
      unsigned n = ca->getNumOperands();
      elems.reserve(n);
      for (unsigned i = 0; i < n; i++) {
        uint32_t e = emit_constant(ca->getOperand(i), depth + 1);
        if (!e) return 0;
        elems.push_back(e);
      }
      auto key = std::make_pair(spv_ty, elems);
      auto it = const_comp.find(key);
      if (it != const_comp.end()) {
        const_memo[c] = it->second;
        return it->second;
      }
      uint32_t id = alloc_id();
      std::vector<uint32_t> args = {spv_ty, id};
      args.insert(args.end(), elems.begin(), elems.end());
      emit_op_vec(types_consts_vars, spv::OpConstantComposite, args);
      if (!error.empty()) return 0;
      const_comp[key] = id;
      const_memo[c] = id;
      return id;
    }
    fail("unsupported LLVM constant expression in AIR");
    return 0;
  }

  uint32_t make_builtin_var(spv::BuiltIn bi, uint32_t val_ty) {
    uint32_t ptr_ty = get_ptr(spv::StorageClassInput, val_ty);
    uint32_t id = alloc_id();
    emit_op(types_consts_vars, spv::OpVariable,
            {ptr_ty, id, static_cast<uint32_t>(spv::StorageClassInput)});
    decorate(id, spv::DecorationBuiltIn, {static_cast<uint32_t>(bi)});
    interface_vars.push_back(id);
    return id;
  }

  uint32_t make_ssbo_var(uint32_t slot, ElemKind elem, bool readonly, uint32_t *out_ptr_elem_ty) {
    if (elem == ElemKind::U8) {
      if (!hw_caps.has_storage_8bit) {
        fail("shader requires StorageBuffer8BitAccess capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityStorageBuffer8BitAccess);
    } else if (elem == ElemKind::U16 || elem == ElemKind::F16) {
      if (!hw_caps.has_storage_16bit) {
        fail("shader requires StorageBuffer16BitAccess capability unsupported by GPU");
        return 0;
      }
      caps.insert(spv::CapabilityStorageBuffer16BitAccess);
    }
    uint32_t ety = get_elem_spv_type(elem);
    if (!ety) return 0;
    uint32_t rta = get_runtime_array(ety, elem_size_bytes(elem));
    uint32_t sty = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeStruct, {sty, rta});
    decorate(sty, spv::DecorationBlock);
    member_decorate(sty, 0, spv::DecorationOffset, {0u});

    uint32_t ptr_struct = get_ptr(spv::StorageClassStorageBuffer, sty);
    uint32_t var_id = alloc_id();
    emit_op(types_consts_vars, spv::OpVariable,
            {ptr_struct, var_id, static_cast<uint32_t>(spv::StorageClassStorageBuffer)});
    decorate(var_id, spv::DecorationDescriptorSet, {0u});
    decorate(var_id, spv::DecorationBinding, {slot});
    if (readonly) decorate(var_id, spv::DecorationNonWritable);
    interface_vars.push_back(var_id);
    *out_ptr_elem_ty = get_ptr(spv::StorageClassStorageBuffer, ety);
    return var_id;
  }

  uint32_t make_mem_array_var(spv::StorageClass sc,
                              ElemKind elem,
                              uint32_t total_bytes,
                              uint32_t *out_ptr_elem_ty) {
    uint32_t esz = elem_size_bytes(elem);
    uint32_t count = (total_bytes + esz - 1u) / esz;
    if (count == 0) count = 1;
    uint32_t ety = get_elem_spv_type(elem);
    if (!ety) return 0;
    uint32_t arr_ty = get_array(ety, count);
    uint32_t ptr_arr = get_ptr(sc, arr_ty);
    uint32_t var_id = alloc_id();
    if (sc == spv::StorageClassFunction) {
      emit_op(entry_vars, spv::OpVariable, {ptr_arr, var_id, static_cast<uint32_t>(sc)});
    } else {
      emit_op(types_consts_vars, spv::OpVariable, {ptr_arr, var_id, static_cast<uint32_t>(sc)});
      interface_vars.push_back(var_id);
    }
    *out_ptr_elem_ty = get_ptr(sc, ety);
    return var_id;
  }

  uint32_t get_ptr_pc_var() {
    if (pc_var_id) return pc_var_id;
    uint32_t u32 = get_int(32);
    uint32_t c16 = get_const_u32(16u);
    uint32_t arr_ty = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeArray, {arr_ty, u32, c16});
    decorate(arr_ty, spv::DecorationArrayStride, {4u});
    uint32_t st_ty = alloc_id();
    emit_op(types_consts_vars, spv::OpTypeStruct, {st_ty, arr_ty, arr_ty});
    decorate(st_ty, spv::DecorationBlock, {});
    member_decorate(st_ty, 0u, spv::DecorationOffset, {0u});
    member_decorate(st_ty, 1u, spv::DecorationOffset, {64u});
    uint32_t ptr_st = get_ptr(spv::StorageClassPushConstant, st_ty);
    pc_ptr_u32_ty = get_ptr(spv::StorageClassPushConstant, u32);
    pc_var_id = alloc_id();
    emit_op(types_consts_vars, spv::OpVariable,
            {ptr_st, pc_var_id, static_cast<uint32_t>(spv::StorageClassPushConstant)});
    interface_vars.push_back(pc_var_id);
    return pc_var_id;
  }

  uint32_t get_sg32_scratch_u32(uint32_t &wg_bytes_used, uint32_t max_wg, uint32_t *out_elem_ptr_ty) {
    if (!sg32_u32_var) {
      const uint32_t need = 64u * 4u;
      if (need > max_wg || wg_bytes_used > max_wg - need) {
        fail("simdgroup emulation scratch exceeds device threadgroup memory limit");
        return 0;
      }
      wg_bytes_used += need;
      uint32_t u32 = get_int(32);
      uint32_t arr_ty = get_array(u32, 64u);
      uint32_t ptr_arr = get_ptr(spv::StorageClassWorkgroup, arr_ty);
      sg32_u32_ptr_ty = get_ptr(spv::StorageClassWorkgroup, u32);
      sg32_u32_var = alloc_id();
      emit_op(types_consts_vars, spv::OpVariable,
              {ptr_arr, sg32_u32_var, static_cast<uint32_t>(spv::StorageClassWorkgroup)});
      interface_vars.push_back(sg32_u32_var);
    }
    *out_elem_ptr_ty = sg32_u32_ptr_ty;
    return sg32_u32_var;
  }

  uint32_t get_sg32_shuf_scratch(bool is_fp, uint32_t &wg_bytes_used, uint32_t max_wg,
                                 uint32_t *out_elem_ptr_ty) {
    const uint32_t need = 1024u * 4u;
    if (is_fp) {
      if (!sg32_shuf_f32_var) {
        if (need > max_wg || wg_bytes_used > max_wg - need) {
          fail("simdgroup shuffle scratch exceeds device threadgroup memory limit");
          return 0;
        }
        wg_bytes_used += need;
        uint32_t f32 = get_float(32);
        uint32_t arr_ty = get_array(f32, 1024u);
        uint32_t ptr_arr = get_ptr(spv::StorageClassWorkgroup, arr_ty);
        sg32_shuf_f32_ptr_ty = get_ptr(spv::StorageClassWorkgroup, f32);
        sg32_shuf_f32_var = alloc_id();
        emit_op(types_consts_vars, spv::OpVariable,
                {ptr_arr, sg32_shuf_f32_var, static_cast<uint32_t>(spv::StorageClassWorkgroup)});
        interface_vars.push_back(sg32_shuf_f32_var);
      }
      *out_elem_ptr_ty = sg32_shuf_f32_ptr_ty;
      return sg32_shuf_f32_var;
    } else {
      if (!sg32_shuf_u32_var) {
        if (need > max_wg || wg_bytes_used > max_wg - need) {
          fail("simdgroup shuffle scratch exceeds device threadgroup memory limit");
          return 0;
        }
        wg_bytes_used += need;
        uint32_t u32 = get_int(32);
        uint32_t arr_ty = get_array(u32, 1024u);
        uint32_t ptr_arr = get_ptr(spv::StorageClassWorkgroup, arr_ty);
        sg32_shuf_u32_ptr_ty = get_ptr(spv::StorageClassWorkgroup, u32);
        sg32_shuf_u32_var = alloc_id();
        emit_op(types_consts_vars, spv::OpVariable,
                {ptr_arr, sg32_shuf_u32_var, static_cast<uint32_t>(spv::StorageClassWorkgroup)});
        interface_vars.push_back(sg32_shuf_u32_var);
      }
      *out_elem_ptr_ty = sg32_shuf_u32_ptr_ty;
      return sg32_shuf_u32_var;
    }
  }
};

struct MergeInfo {
  bool is_loop = false;
  llvm::BasicBlock *merge_bb = nullptr;
  llvm::BasicBlock *continue_bb = nullptr;
};

struct Compiler {
  llvm::Module &mod;
  llvm::Function &fn;
  const llvm::DataLayout &dl;
  SpvEmitter spv;

  std::vector<KernelArgMeta> arg_meta;
  BufferBinding buffers[kMaxBufferSlots];
  std::vector<MemArrayInfo> wg_arrays;
  std::vector<MemArrayInfo> fn_arrays;
  std::unordered_map<const llvm::GlobalVariable *, uint32_t> wg_gv_map;
  std::unordered_map<const llvm::Argument *, uint32_t> wg_arg_map;
  std::unordered_map<const llvm::AllocaInst *, uint32_t> alloca_map;
  std::unordered_map<uint32_t, std::pair<PtrKind, uint32_t>> ptr_alloca_roots;
  std::unordered_map<const llvm::GlobalVariable *, FuncConstInfo> fc_map;
  std::unordered_map<BuiltinKind, uint32_t> builtin_vars;

  std::unordered_map<const llvm::BasicBlock *, uint32_t> bb_label;
  std::unordered_map<const llvm::BasicBlock *, uint32_t> bb_exit_label;
  std::unordered_map<const llvm::BasicBlock *, MergeInfo> merges;
  std::unordered_set<const llvm::BasicBlock *> continue_latches;
  const llvm::BasicBlock *cur_bb = nullptr;
  uint32_t cur_exit_label = 0;

  std::unordered_map<const llvm::Value *, uint32_t> val_map;
  std::unordered_map<const llvm::Value *, PtrInfo> ptr_map;
  std::unordered_map<const llvm::Value *, uint32_t> ptr_null_map;
  std::unordered_map<const llvm::PHINode *, uint32_t> phi_ids;
  std::unordered_map<const llvm::PHINode *, PtrInfo> ptr_phi_info;
  std::unordered_map<const llvm::PHINode *, uint32_t> ptr_phi_null_ids;
  std::unordered_set<const llvm::PHINode *> narrowed_i64_phis;
  std::unordered_set<const llvm::Value *> narrowed_i64_set;
  std::unordered_map<const llvm::Value *, uint32_t> narrowed_i64_vals;

  enum class TraceState : uint8_t { Visiting, Failed, Resolved };
  enum class TraceRes : uint8_t { Failed, Cycle, Resolved };
  struct TraceEntry {
    TraceState state = TraceState::Visiting;
    PtrKind kind = PtrKind::Invalid;
    uint32_t base = 0;
  };
  using TraceCache = std::unordered_map<const llvm::Value *, TraceEntry>;

  std::vector<uint32_t> cur_body;
  uint32_t buf_sub_off_ids[16] = {0};
  uint32_t wg_bytes_used = 0;
  uint32_t alloca_bytes_used = 0;

  Compiler(llvm::Module &m, llvm::Function &f, const air2spv_caps_t &c)
      : mod(m), fn(f), dl(m.getDataLayout()) {
    spv.hw_caps = c;
    spv.caps.insert(spv::CapabilityShader);
    spv.glsl_ext_id = spv.alloc_id();
    spv.main_fn_id = spv.alloc_id();

    /* LocalSizeId specialization constants: SpecId 65536, 65537, 65538 (outside Metal 0..65535 FC range). */
    uint32_t u32 = spv.get_int(32);
    spv.spec_lx = spv.alloc_id();
    spv.spec_ly = spv.alloc_id();
    spv.spec_lz = spv.alloc_id();
    spv.emit_op(spv.types_consts_vars, spv::OpSpecConstant, {u32, spv.spec_lx, 1u});
    spv.emit_op(spv.types_consts_vars, spv::OpSpecConstant, {u32, spv.spec_ly, 1u});
    spv.emit_op(spv.types_consts_vars, spv::OpSpecConstant, {u32, spv.spec_lz, 1u});
    spv.decorate(spv.spec_lx, spv::DecorationSpecId, {65536u});
    spv.decorate(spv.spec_ly, spv::DecorationSpecId, {65537u});
    spv.decorate(spv.spec_lz, spv::DecorationSpecId, {65538u});
    uint32_t v3u = spv.get_vec(u32, 3);
    spv.wg_size_v3 = spv.alloc_id();
    spv.emit_op(spv.types_consts_vars, spv::OpSpecConstantComposite,
                {v3u, spv.wg_size_v3, spv.spec_lx, spv.spec_ly, spv.spec_lz});
  }

  static BuiltinKind parse_builtin_tag(llvm::StringRef s) {
    if (s == "air.thread_position_in_grid") return BuiltinKind::GlobalInvocationId;
    if (s == "air.thread_position_in_threadgroup") return BuiltinKind::LocalInvocationId;
    if (s == "air.threadgroup_position_in_grid") return BuiltinKind::WorkgroupId;
    if (s == "air.threads_per_threadgroup") return BuiltinKind::WorkgroupSize;
    if (s == "air.threadgroups_per_grid") return BuiltinKind::NumWorkgroups;
    if (s == "air.threads_per_grid") return BuiltinKind::GridSize;
    if (s == "air.thread_index_in_threadgroup") return BuiltinKind::LocalInvocationIndex;
    if (s == "air.thread_index_in_simdgroup") return BuiltinKind::SubgroupLocalInvocationId;
    if (s == "air.threads_per_simdgroup") return BuiltinKind::SubgroupSize;
    if (s == "air.simdgroup_index_in_threadgroup") return BuiltinKind::SubgroupId;
    if (s == "air.simdgroups_per_threadgroup") return BuiltinKind::NumSubgroups;
    return BuiltinKind::None;
  }

  bool parse_air_metadata(llvm::MDNode *kernel_md) {
    arg_meta.resize(fn.arg_size());
    std::vector<bool> seen_arg(fn.arg_size(), false);
    if (kernel_md && kernel_md->getNumOperands() >= 3) {
      if (auto *inputs = llvm::dyn_cast_or_null<llvm::MDNode>(kernel_md->getOperand(2).get())) {
        for (unsigned i = 0; i < inputs->getNumOperands(); i++) {
          auto *entry = llvm::dyn_cast_or_null<llvm::MDNode>(inputs->getOperand(i).get());
          if (!entry || entry->getNumOperands() < 2) continue;
          auto *idx_md = llvm::mdconst::dyn_extract_or_null<llvm::ConstantInt>(entry->getOperand(0).get());
          if (!idx_md) continue;
          uint32_t aidx = 0;
          if (!try_get_u32(idx_md, aidx) || aidx >= fn.arg_size()) {
            return spv.fail("air.kernel arg index out of bounds");
          }
          if (seen_arg[aidx]) {
            return spv.fail("duplicate air.kernel argument metadata");
          }
          seen_arg[aidx] = true;
          auto *tag_md = llvm::dyn_cast_or_null<llvm::MDString>(entry->getOperand(1).get());
          if (!tag_md) continue;
          llvm::StringRef tag = tag_md->getString();
          KernelArgMeta &am = arg_meta[aidx];
          if (tag == "air.buffer") {
            if (!fn.getArg(aidx)->getType()->isPointerTy()) {
              return spv.fail("air.buffer metadata attached to non-pointer argument");
            }
            am.is_buffer = true;
            for (unsigned j = 2; j < entry->getNumOperands(); j++) {
              auto *s = llvm::dyn_cast_or_null<llvm::MDString>(entry->getOperand(j).get());
              if (!s) continue;
              llvm::StringRef key = s->getString();
              if (key == "air.location_index" && j + 1 < entry->getNumOperands()) {
                auto *c = llvm::mdconst::dyn_extract_or_null<llvm::ConstantInt>(
                    entry->getOperand(j + 1).get());
                if (!c || !try_get_u32(c, am.slot)) {
                  return spv.fail("invalid air.location_index metadata");
                }
              } else if (key == "air.address_space" && j + 1 < entry->getNumOperands()) {
                auto *c = llvm::mdconst::dyn_extract_or_null<llvm::ConstantInt>(
                    entry->getOperand(j + 1).get());
                if (!c || !try_get_u32(c, am.addr_space)) {
                  return spv.fail("invalid air.address_space metadata");
                }
              } else if (key == "air.read") {
                am.is_readonly = true;
              } else if (key == "air.read_write" || key == "air.write") {
                am.is_writable = true;
              }
            }
            if (am.addr_space == 0 && fn.getArg(aidx)->getType()->isPointerTy()) {
              am.addr_space = fn.getArg(aidx)->getType()->getPointerAddressSpace();
            }
          } else {
            BuiltinKind bk = parse_builtin_tag(tag);
            if (bk == BuiltinKind::None) {
              return spv.fail("unsupported air.kernel argument metadata: " + tag.str());
            }
            am.builtin = bk;
          }
        }
      }
    }

    /* Parse !air.function_constants if present.
     * Supports both synthetic key-value form:
     *   !{ptr @gv, !"air.location_index", i32 N}
     * and Apple AIR positional form emitted by Apple's Metal compiler:
     *   !{ptr @gv.MTL_FC_INIT_N_*, !"type_str", !"name_str", i32 N, i1 req}
     */
    std::unordered_set<uint32_t> used_spec_ids;
    if (auto *fc_nmd = mod.getNamedMetadata("air.function_constants")) {
      for (unsigned i = 0; i < fc_nmd->getNumOperands(); i++) {
        auto *node = fc_nmd->getOperand(i);
        if (!node || node->getNumOperands() < 3) continue;
        auto *gv = llvm::mdconst::dyn_extract_or_null<llvm::GlobalVariable>(node->getOperand(0).get());
        if (!gv) continue;
        if (fc_map.count(gv)) {
          return spv.fail("duplicate air.function_constants global");
        }
        uint32_t spec_id = UINT32_MAX;
        bool is_air_bool = false;
        for (unsigned j = 1; j + 1 < node->getNumOperands(); j++) {
          if (auto *s = llvm::dyn_cast_or_null<llvm::MDString>(node->getOperand(j).get())) {
            if (s->getString() == "air.location_index") {
              auto *c = llvm::mdconst::dyn_extract_or_null<llvm::ConstantInt>(
                  node->getOperand(j + 1).get());
              if (!c || !try_get_u32(c, spec_id)) {
                return spv.fail("invalid air.function_constants location_index");
              }
            }
          }
        }
        if (spec_id == UINT32_MAX && node->getNumOperands() >= 4) {
          auto *ty_md = llvm::dyn_cast_or_null<llvm::MDString>(node->getOperand(1).get());
          auto *nm_md = llvm::dyn_cast_or_null<llvm::MDString>(node->getOperand(2).get());
          auto *id_c = llvm::mdconst::dyn_extract_or_null<llvm::ConstantInt>(node->getOperand(3).get());
          if (ty_md && nm_md && id_c && try_get_u32(id_c, spec_id)) {
            if (ty_md->getString() == "bool") is_air_bool = true;
          }
        }
        if (spec_id == UINT32_MAX || spec_id > 65535u) {
          return spv.fail("invalid air.function_constants location_index");
        }
        if (!used_spec_ids.insert(spec_id).second) {
          return spv.fail("duplicate air.function_constants SpecId");
        }
        llvm::Type *vty = gv->getValueType();
        uint32_t sid = spv.alloc_id();
        if (vty->isIntegerTy(1) || (is_air_bool && vty->isIntegerTy(8))) {
          uint32_t bty = spv.get_bool();
          if (!bty) return false;
          spv.emit_op(spv.types_consts_vars, spv::OpSpecConstantFalse, {bty, sid});
          is_air_bool = vty->isIntegerTy(8);
        } else if (vty->isIntegerTy() || vty->isFloatTy() || vty->isHalfTy() || vty->isBFloatTy()) {
          uint32_t spv_ty = spv.map_llvm_type(vty);
          if (!spv_ty) return false;
          if (vty->isIntegerTy(64)) {
            spv.emit_op(spv.types_consts_vars, spv::OpSpecConstant, {spv_ty, sid, 0u, 0u});
          } else {
            spv.emit_op(spv.types_consts_vars, spv::OpSpecConstant, {spv_ty, sid, 0u});
          }
          is_air_bool = false;
        } else {
          return spv.fail("unsupported function constant type in AIR");
        }
        spv.decorate(sid, spv::DecorationSpecId, {spec_id});
        if (!spv.error.empty()) return false;
        fc_map[gv] = FuncConstInfo{spec_id, vty, sid, is_air_bool};
      }
      /* Map internal constant globals populated from air.fc_initializer in air.static_init ctors. */
      for (llvm::Function &sf : mod) {
        if (sf.isDeclaration()) continue;
        if (sf.getSection() != "air.static_init" && !sf.getName().starts_with("_GLOBAL__sub_I_")) continue;
        for (llvm::BasicBlock &sbb : sf) {
          for (llvm::Instruction &si_raw : sbb) {
            auto *si = llvm::dyn_cast<llvm::StoreInst>(&si_raw);
            if (!si) continue;
            auto *dst_gv = llvm::dyn_cast<llvm::GlobalVariable>(si->getPointerOperand()->stripPointerCasts());
            auto *li = llvm::dyn_cast<llvm::LoadInst>(si->getValueOperand());
            if (!dst_gv || !li) continue;
            auto *src_gv = llvm::dyn_cast<llvm::GlobalVariable>(li->getPointerOperand()->stripPointerCasts());
            if (!src_gv) continue;
            auto fit = fc_map.find(src_gv);
            if (fit != fc_map.end() && !fc_map.count(dst_gv) &&
                dst_gv->getValueType() == src_gv->getValueType()) {
              fc_map[dst_gv] = fit->second;
            }
          }
        }
      }
    }
    return true;
  }

  /* Trace pointer provenance to a root using 3-state memoization + step cap (O(V) on DAGs). */
  TraceRes trace_ptr_root_impl(const llvm::Value *v,
                               PtrKind &out_kind,
                               uint32_t &out_base,
                               TraceCache &cache,
                               int depth,
                               size_t &steps) const {
    if (!v || depth > 256 || ++steps > 4096) return TraceRes::Failed;
    if (llvm::isa<llvm::UndefValue>(v) || llvm::isa<llvm::PoisonValue>(v)) {
      return TraceRes::Failed;
    }
    if (auto it = cache.find(v); it != cache.end()) {
      if (it->second.state == TraceState::Visiting) return TraceRes::Cycle;
      if (it->second.state == TraceState::Failed) return TraceRes::Failed;
      out_kind = it->second.kind;
      out_base = it->second.base;
      return TraceRes::Resolved;
    }

    auto finish = [&](TraceRes r, PtrKind k, uint32_t b) -> TraceRes {
      if (r == TraceRes::Resolved) {
        cache[v] = TraceEntry{TraceState::Resolved, k, b};
        out_kind = k;
        out_base = b;
      } else if (r == TraceRes::Failed) {
        cache[v] = TraceEntry{TraceState::Failed, PtrKind::Invalid, 0};
      } else {
        cache.erase(v);
      }
      return r;
    };

    cache[v] = TraceEntry{TraceState::Visiting, PtrKind::Invalid, 0};

    if (auto *arg = llvm::dyn_cast<llvm::Argument>(v)) {
      unsigned idx = arg->getArgNo();
      if (idx >= arg_meta.size() || !arg_meta[idx].is_buffer) {
        return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      }
      const KernelArgMeta &am = arg_meta[idx];
      if (am.addr_space == 1 || am.addr_space == 2) {
        return finish(TraceRes::Resolved, PtrKind::Buffer, am.slot);
      }
      if (am.addr_space == 3) {
        auto it = wg_arg_map.find(arg);
        if (it == wg_arg_map.end()) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
        return finish(TraceRes::Resolved, PtrKind::Workgroup, it->second);
      }
      return finish(TraceRes::Failed, PtrKind::Invalid, 0);
    }
    if (auto *gv = llvm::dyn_cast<llvm::GlobalVariable>(v)) {
      if (auto it = fc_map.find(gv); it != fc_map.end()) {
        return finish(TraceRes::Resolved, PtrKind::FuncConst, it->second.spec_id);
      }
      if (auto it = wg_gv_map.find(gv); it != wg_gv_map.end()) {
        return finish(TraceRes::Resolved, PtrKind::Workgroup, it->second);
      }
      return finish(TraceRes::Failed, PtrKind::Invalid, 0);
    }
    if (auto *ai = llvm::dyn_cast<llvm::AllocaInst>(v)) {
      auto it = alloca_map.find(ai);
      if (it == alloca_map.end()) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      return finish(TraceRes::Resolved, PtrKind::Function, it->second);
    }
    if (auto *gep = llvm::dyn_cast<llvm::GEPOperator>(v)) {
      PtrKind k = PtrKind::Invalid;
      uint32_t b = 0;
      TraceRes r = trace_ptr_root_impl(gep->getPointerOperand(), k, b, cache, depth + 1, steps);
      return finish(r, k, b);
    }
    if (auto *bc = llvm::dyn_cast<llvm::BitCastOperator>(v)) {
      PtrKind k = PtrKind::Invalid;
      uint32_t b = 0;
      TraceRes r = trace_ptr_root_impl(bc->getOperand(0), k, b, cache, depth + 1, steps);
      return finish(r, k, b);
    }
    if (auto *asc = llvm::dyn_cast<llvm::AddrSpaceCastOperator>(v)) {
      PtrKind k = PtrKind::Invalid;
      uint32_t b = 0;
      TraceRes r = trace_ptr_root_impl(asc->getOperand(0), k, b, cache, depth + 1, steps);
      return finish(r, k, b);
    }
    if (auto *fi = llvm::dyn_cast<llvm::FreezeInst>(v)) {
      PtrKind k = PtrKind::Invalid;
      uint32_t b = 0;
      TraceRes r = trace_ptr_root_impl(fi->getOperand(0), k, b, cache, depth + 1, steps);
      return finish(r, k, b);
    }
    if (auto *phi = llvm::dyn_cast<llvm::PHINode>(v)) {
      PtrKind found_k = PtrKind::Invalid;
      uint32_t found_b = 0;
      bool saw_cycle = false;
      for (unsigned i = 0; i < phi->getNumIncomingValues(); i++) {
        llvm::Value *inc = phi->getIncomingValue(i);
        if (llvm::isa<llvm::UndefValue>(inc) || llvm::isa<llvm::PoisonValue>(inc) ||
            llvm::isa<llvm::ConstantPointerNull>(inc)) {
          continue;
        }
        PtrKind k = PtrKind::Invalid;
        uint32_t b = 0;
        TraceRes r = trace_ptr_root_impl(inc, k, b, cache, depth + 1, steps);
        if (r == TraceRes::Failed) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
        if (r == TraceRes::Cycle) {
          saw_cycle = true;
          continue;
        }
        if (r == TraceRes::Resolved) {
          if (found_k == PtrKind::Invalid) {
            found_k = k;
            found_b = b;
          } else if (found_k != k || found_b != b) {
            return finish(TraceRes::Failed, PtrKind::Invalid, 0);
          }
        }
      }
      if (found_k != PtrKind::Invalid) {
        if (saw_cycle && depth > 0) {
          cache.erase(v);
          out_kind = found_k;
          out_base = found_b;
          return TraceRes::Resolved;
        }
        return finish(TraceRes::Resolved, found_k, found_b);
      }
      if (saw_cycle) return finish(TraceRes::Cycle, PtrKind::Invalid, 0);
      return finish(TraceRes::Failed, PtrKind::Invalid, 0);
    }
    if (auto *li = llvm::dyn_cast<llvm::LoadInst>(v)) {
      if (li->getType()->isPointerTy()) {
        PtrKind pk = PtrKind::Invalid;
        uint32_t pb = 0;
        TraceRes r = trace_ptr_root_impl(li->getPointerOperand(), pk, pb, cache, depth + 1, steps);
        if (r == TraceRes::Resolved && pk == PtrKind::Function) {
          auto it = ptr_alloca_roots.find(pb);
          if (it != ptr_alloca_roots.end()) {
            return finish(TraceRes::Resolved, it->second.first, it->second.second);
          }
        }
      }
      return finish(TraceRes::Failed, PtrKind::Invalid, 0);
    }
    if (auto *sel = llvm::dyn_cast<llvm::SelectInst>(v)) {
      auto is_nullish = [](const llvm::Value *x) {
        return llvm::isa<llvm::UndefValue>(x) || llvm::isa<llvm::PoisonValue>(x) ||
               llvm::isa<llvm::ConstantPointerNull>(x);
      };
      const llvm::Value *tv = sel->getTrueValue();
      const llvm::Value *fv = sel->getFalseValue();
      bool tn = is_nullish(tv);
      bool fn_null = is_nullish(fv);
      if (tn && fn_null) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      PtrKind k1 = PtrKind::Invalid, k2 = PtrKind::Invalid;
      uint32_t b1 = 0, b2 = 0;
      TraceRes r1 = tn ? TraceRes::Cycle : trace_ptr_root_impl(tv, k1, b1, cache, depth + 1, steps);
      if (!tn && r1 == TraceRes::Failed) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      TraceRes r2 = fn_null ? TraceRes::Cycle : trace_ptr_root_impl(fv, k2, b2, cache, depth + 1, steps);
      if (!fn_null && r2 == TraceRes::Failed) return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      if (!tn && !fn_null && r1 == TraceRes::Resolved && r2 == TraceRes::Resolved) {
        if (k1 == k2 && b1 == b2) return finish(TraceRes::Resolved, k1, b1);
        return finish(TraceRes::Failed, PtrKind::Invalid, 0);
      }
      if (!tn && r1 == TraceRes::Resolved) {
        if (fn_null) return finish(TraceRes::Resolved, k1, b1);
        cache.erase(v);
        out_kind = k1;
        out_base = b1;
        return TraceRes::Resolved;
      }
      if (!fn_null && r2 == TraceRes::Resolved) {
        if (tn) return finish(TraceRes::Resolved, k2, b2);
        cache.erase(v);
        out_kind = k2;
        out_base = b2;
        return TraceRes::Resolved;
      }
      return finish(TraceRes::Cycle, PtrKind::Invalid, 0);
    }
    return finish(TraceRes::Failed, PtrKind::Invalid, 0);
  }

  bool trace_ptr_root(const llvm::Value *v,
                      PtrKind &out_kind,
                      uint32_t &out_base,
                      TraceCache &cache) const {
    size_t steps = 0;
    return trace_ptr_root_impl(v, out_kind, out_base, cache, 0, steps) == TraceRes::Resolved;
  }

  /* Collect all memory roots reachable from a pointer expression, including two-root selects. */
  bool collect_ptr_roots(const llvm::Value *v,
                         std::vector<std::pair<PtrKind, uint32_t>> &out) const {
    out.clear();
    std::unordered_set<const llvm::Value *> visited;
    std::vector<const llvm::Value *> work;
    work.push_back(v);
    size_t steps = 0;
    auto is_nullish = [](const llvm::Value *x) {
      return llvm::isa<llvm::ConstantPointerNull>(x) || llvm::isa<llvm::UndefValue>(x) ||
             llvm::isa<llvm::PoisonValue>(x);
    };
    auto add_root = [&](PtrKind k, uint32_t b) {
      for (const auto &e : out) {
        if (e.first == k && e.second == b) return;
      }
      out.push_back({k, b});
    };
    while (!work.empty()) {
      const llvm::Value *cur = work.back();
      work.pop_back();
      if (!cur || ++steps > 4096) return false;
      if (is_nullish(cur)) continue;
      if (!visited.insert(cur).second) continue;

      if (auto *arg = llvm::dyn_cast<llvm::Argument>(cur)) {
        unsigned idx = arg->getArgNo();
        if (idx >= arg_meta.size() || !arg_meta[idx].is_buffer) return false;
        const KernelArgMeta &am = arg_meta[idx];
        if (am.addr_space == 1 || am.addr_space == 2) {
          add_root(PtrKind::Buffer, am.slot);
          continue;
        }
        if (am.addr_space == 3) {
          auto it = wg_arg_map.find(arg);
          if (it == wg_arg_map.end()) return false;
          add_root(PtrKind::Workgroup, it->second);
          continue;
        }
        return false;
      }
      if (auto *gv = llvm::dyn_cast<llvm::GlobalVariable>(cur)) {
        if (auto it = fc_map.find(gv); it != fc_map.end()) {
          add_root(PtrKind::FuncConst, it->second.spec_id);
          continue;
        }
        if (auto it = wg_gv_map.find(gv); it != wg_gv_map.end()) {
          add_root(PtrKind::Workgroup, it->second);
          continue;
        }
        return false;
      }
      if (auto *ai = llvm::dyn_cast<llvm::AllocaInst>(cur)) {
        auto it = alloca_map.find(ai);
        if (it == alloca_map.end()) return false;
        add_root(PtrKind::Function, it->second);
        continue;
      }
      if (auto *gep = llvm::dyn_cast<llvm::GEPOperator>(cur)) {
        work.push_back(gep->getPointerOperand());
        continue;
      }
      if (auto *bc = llvm::dyn_cast<llvm::BitCastOperator>(cur)) {
        work.push_back(bc->getOperand(0));
        continue;
      }
      if (auto *asc = llvm::dyn_cast<llvm::AddrSpaceCastOperator>(cur)) {
        work.push_back(asc->getOperand(0));
        continue;
      }
      if (auto *fi = llvm::dyn_cast<llvm::FreezeInst>(cur)) {
        work.push_back(fi->getOperand(0));
        continue;
      }
      if (auto *phi = llvm::dyn_cast<llvm::PHINode>(cur)) {
        for (unsigned i = 0; i < phi->getNumIncomingValues(); i++) {
          work.push_back(phi->getIncomingValue(i));
        }
        continue;
      }
      if (auto *sel = llvm::dyn_cast<llvm::SelectInst>(cur)) {
        work.push_back(sel->getFalseValue());
        work.push_back(sel->getTrueValue());
        continue;
      }
      if (auto *li = llvm::dyn_cast<llvm::LoadInst>(cur)) {
        if (li->getType()->isPointerTy()) {
          TraceCache tmp_cache;
          PtrKind pk = PtrKind::Invalid;
          uint32_t pb = 0;
          if (trace_ptr_root(li->getPointerOperand(), pk, pb, tmp_cache) &&
              pk == PtrKind::Function) {
            auto it = ptr_alloca_roots.find(pb);
            if (it != ptr_alloca_roots.end()) {
              add_root(it->second.first, it->second.second);
              continue;
            }
          }
        }
        return false;
      }
      return false;
    }
    return !out.empty();
  }

  void record_access_type_impl(BufferBinding &bb, llvm::Type *ty, bool is_store, bool is_atomic,
                               std::unordered_set<const llvm::Type *> &seen, int depth) {
    if (!ty || depth > 16) {
      bb.all_half = false;
      bb.all_f32 = false;
      return;
    }
    if (!seen.insert(ty).second) return;
    if (auto *st = llvm::dyn_cast<llvm::StructType>(ty)) {
      bb.saw_access = true;
      if (is_store) bb.has_store = true;
      if (is_atomic) bb.has_atomic = true;
      if (st->isOpaque() || st->getNumElements() > kMaxStructMembers) {
        bb.all_half = false;
        bb.all_f32 = false;
        return;
      }
      for (unsigned i = 0; i < st->getNumElements(); i++) {
        record_access_type_impl(bb, st->getElementType(i), is_store, is_atomic, seen, depth + 1);
      }
      return;
    }
    if (auto *at = llvm::dyn_cast<llvm::ArrayType>(ty)) {
      record_access_type_impl(bb, at->getElementType(), is_store, is_atomic, seen, depth + 1);
      return;
    }
    bb.saw_access = true;
    if (is_store) bb.has_store = true;
    if (is_atomic) bb.has_atomic = true;
    llvm::Type *scalar = ty->getScalarType();
    if (scalar->isHalfTy()) {
      bb.all_f32 = false;
      bb.min_access_bytes = std::min(bb.min_access_bytes, 2u);
      if (is_store) bb.min_store_bytes = std::min(bb.min_store_bytes, 2u);
    } else if (scalar->isBFloatTy()) {
      bb.all_half = false;
      bb.all_f32 = false;
      bb.min_access_bytes = std::min(bb.min_access_bytes, 2u);
      if (is_store) bb.min_store_bytes = std::min(bb.min_store_bytes, 2u);
    } else if (scalar->isFloatTy()) {
      bb.all_half = false;
      bb.min_access_bytes = std::min(bb.min_access_bytes, 4u);
      if (is_store) bb.min_store_bytes = std::min(bb.min_store_bytes, 4u);
    } else if (scalar->isIntegerTy()) {
      bb.all_half = false;
      bb.all_f32 = false;
      uint32_t b = std::max(1u, scalar->getIntegerBitWidth() / 8u);
      bb.min_access_bytes = std::min(bb.min_access_bytes, b);
      if (is_store) bb.min_store_bytes = std::min(bb.min_store_bytes, b);
    } else {
      bb.all_half = false;
      bb.all_f32 = false;
    }
  }

  void record_access_type(BufferBinding &bb, llvm::Type *ty, bool is_store, bool is_atomic) {
    std::unordered_set<const llvm::Type *> seen;
    record_access_type_impl(bb, ty, is_store, is_atomic, seen, 0);
  }

  bool setup_memory_objects() {
    uint32_t max_wg = spv.hw_caps.max_workgroup_memory
                          ? std::min(spv.hw_caps.max_workgroup_memory, kMaxWorkgroupBytes)
                          : kMaxWorkgroupBytes;

    /* 1. Register kernel buffer arguments (device, constant, dynamic threadgroup). */
    for (unsigned i = 0; i < fn.arg_size(); i++) {
      const KernelArgMeta &am = arg_meta[i];
      if (!am.is_buffer) continue;
      if (am.addr_space == 1 || am.addr_space == 2) {
        if (am.slot >= kMaxBufferSlots) return spv.fail("buffer binding slot >= 31");
        if (buffers[am.slot].used) return spv.fail("duplicate buffer binding slot");
        buffers[am.slot].used = true;
        buffers[am.slot].readonly = (am.is_readonly && !am.is_writable) ||
                                    fn.getArg(i)->onlyReadsMemory();
      } else if (am.addr_space == 3) {
        uint32_t sz = std::min(kDefaultDynWgBytes, max_wg);
        if (sz > max_wg - wg_bytes_used) return spv.fail("threadgroup memory exceeds device limit");
        uint32_t idx = static_cast<uint32_t>(wg_arrays.size());
        MemArrayInfo info;
        info.total_bytes = sz;
        info.storage = spv::StorageClassWorkgroup;
        wg_arrays.push_back(info);
        wg_arg_map[fn.getArg(i)] = idx;
        wg_bytes_used += sz;
      } else {
        return spv.fail("unsupported buffer address space in AIR");
      }
    }

    /* 2. Register static workgroup (addrspace 3) GlobalVariables referenced by fn. */
    for (llvm::GlobalVariable &gv : mod.globals()) {
      if (fc_map.count(&gv)) continue;
      if (gv.getAddressSpace() == 3) {
        bool used = false;
        for (const llvm::User *u : gv.users()) {
          if (auto *inst = llvm::dyn_cast<llvm::Instruction>(u)) {
            if (inst->getFunction() == &fn) {
              used = true;
              break;
            }
          } else {
            used = true;
            break;
          }
        }
        if (!used) continue;
        uint64_t sz = 0;
        if (!try_type_alloc_bytes(dl, gv.getValueType(), sz) || sz == 0 || sz > max_wg ||
            sz > max_wg - wg_bytes_used) {
          return spv.fail("static threadgroup variable invalid or exceeds device limit");
        }
        uint32_t idx = static_cast<uint32_t>(wg_arrays.size());
        MemArrayInfo info;
        info.total_bytes = static_cast<uint32_t>(sz);
        info.storage = spv::StorageClassWorkgroup;
        wg_arrays.push_back(info);
        wg_gv_map[&gv] = idx;
        wg_bytes_used += static_cast<uint32_t>(sz);
      }
    }

    /* 3. Register remaining stack allocas (addrspace 0) after mem2reg. */
    for (llvm::BasicBlock &bb : fn) {
      for (llvm::Instruction &inst : bb) {
        if (auto *ai = llvm::dyn_cast<llvm::AllocaInst>(&inst)) {
          if (!ai->isStaticAlloca()) return spv.fail("dynamic alloca not supported");
          auto *arr_sz = llvm::dyn_cast<llvm::ConstantInt>(ai->getArraySize());
          if (!arr_sz) return spv.fail("non-constant alloca array size");
          uint64_t elem_sz = 0;
          uint64_t cnt = 0;
          if (!try_type_alloc_bytes(dl, ai->getAllocatedType(), elem_sz) ||
              !try_get_u64(arr_sz, cnt) || cnt == 0 || elem_sz == 0 ||
              cnt > kMaxAllocaBytes / elem_sz) {
            return spv.fail("alloca size invalid or exceeds limit");
          }
          uint64_t total = elem_sz * cnt;
          if (total > kMaxAllocaBytes - alloca_bytes_used) {
            return spv.fail("total function alloca memory exceeds limit");
          }
          uint32_t idx = static_cast<uint32_t>(fn_arrays.size());
          MemArrayInfo info;
          info.total_bytes = static_cast<uint32_t>(total);
          info.storage = spv::StorageClassFunction;
          fn_arrays.push_back(info);
          alloca_map[ai] = idx;
          alloca_bytes_used += static_cast<uint32_t>(total);
        }
      }
    }

    /* 3b. Identify single-root pointer allocas (e.g. alloca [N x ptr addrspace(1/3)]). */
    {
      std::unordered_map<uint32_t, std::pair<PtrKind, uint32_t>> cand;
      std::unordered_set<uint32_t> poisoned;
      bool changed = true;
      while (changed) {
        changed = false;
        TraceCache pt_cache;
        for (llvm::BasicBlock &bb : fn) {
          for (llvm::Instruction &inst : bb) {
            if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&inst)) {
              llvm::Function *callee = ci->getCalledFunction();
              if (callee) {
                llvm::StringRef cname = callee->getName();
                if ((cname.starts_with("llvm.memcpy.") || cname.starts_with("llvm.memmove.") ||
                     cname.starts_with("llvm.memset.")) && ci->arg_size() >= 1) {
                  PtrKind dst_k = PtrKind::Invalid;
                  uint32_t dst_b = 0;
                  if (trace_ptr_root(ci->getArgOperand(0), dst_k, dst_b, pt_cache) &&
                      dst_k == PtrKind::Function && !poisoned.count(dst_b)) {
                    poisoned.insert(dst_b);
                    cand.erase(dst_b);
                    ptr_alloca_roots.erase(dst_b);
                    changed = true;
                  }
                }
              }
              continue;
            }
            auto *si = llvm::dyn_cast<llvm::StoreInst>(&inst);
            if (!si || !si->getValueOperand()->getType()->isPointerTy()) continue;
            PtrKind dst_k = PtrKind::Invalid;
            uint32_t dst_b = 0;
            if (!trace_ptr_root(si->getPointerOperand(), dst_k, dst_b, pt_cache)) continue;
            if (dst_k != PtrKind::Function || poisoned.count(dst_b)) continue;
            llvm::Value *sv = si->getValueOperand();
            if (llvm::isa<llvm::ConstantPointerNull>(sv) || llvm::isa<llvm::UndefValue>(sv) ||
                llvm::isa<llvm::PoisonValue>(sv)) {
              continue;
            }
            PtrKind val_k = PtrKind::Invalid;
            uint32_t val_b = 0;
            if (!trace_ptr_root(sv, val_k, val_b, pt_cache)) {
              poisoned.insert(dst_b);
              cand.erase(dst_b);
              ptr_alloca_roots.erase(dst_b);
              continue;
            }
            if (val_k != PtrKind::Buffer && val_k != PtrKind::Workgroup && val_k != PtrKind::Function) {
              poisoned.insert(dst_b);
              cand.erase(dst_b);
              ptr_alloca_roots.erase(dst_b);
              continue;
            }
            auto it = cand.find(dst_b);
            if (it == cand.end()) {
              cand[dst_b] = {val_k, val_b};
              ptr_alloca_roots[dst_b] = {val_k, val_b};
              changed = true;
            } else if (it->second.first != val_k || it->second.second != val_b) {
              poisoned.insert(dst_b);
              cand.erase(dst_b);
              ptr_alloca_roots.erase(dst_b);
              changed = true;
            }
          }
        }
      }
    }

    /* 4. Pre-scan all memory ops to select optimal race-free backing element types and readonly masks. */
    std::vector<BufferBinding> wg_stats(wg_arrays.size());
    std::vector<BufferBinding> fn_stats(fn_arrays.size());
    std::vector<std::pair<PtrKind, uint32_t>> roots;
    auto record_ptr_access = [&](const llvm::Value *ptr, llvm::Type *acc_ty, bool is_store,
                                 bool is_atomic) {
      if (!ptr || !acc_ty) return;
      if (acc_ty->isPointerTy()) acc_ty = llvm::Type::getInt32Ty(mod.getContext());
      if (!collect_ptr_roots(ptr, roots)) {
        if (is_store) {
          /* Conservatively mark all used buffers writable if any store target is unresolvable. */
          for (uint32_t s = 0; s < kMaxBufferSlots; s++) {
            if (buffers[s].used) buffers[s].has_store = true;
          }
        }
        return;
      }
      for (const auto &rb : roots) {
        if (rb.first == PtrKind::Buffer && rb.second < kMaxBufferSlots) {
          record_access_type(buffers[rb.second], acc_ty, is_store, is_atomic);
        } else if (rb.first == PtrKind::Workgroup && rb.second < wg_stats.size()) {
          record_access_type(wg_stats[rb.second], acc_ty, is_store, is_atomic);
        } else if (rb.first == PtrKind::Function && rb.second < fn_stats.size()) {
          record_access_type(fn_stats[rb.second], acc_ty, is_store, is_atomic);
        }
      }
    };
    llvm::Type *i8_ty = llvm::Type::getInt8Ty(mod.getContext());
    llvm::Type *i32_ty = llvm::Type::getInt32Ty(mod.getContext());
    for (llvm::BasicBlock &bb : fn) {
      for (llvm::Instruction &inst : bb) {
        if (auto *li = llvm::dyn_cast<llvm::LoadInst>(&inst)) {
          record_ptr_access(li->getPointerOperand(), li->getType(), false, false);
        } else if (auto *si = llvm::dyn_cast<llvm::StoreInst>(&inst)) {
          record_ptr_access(si->getPointerOperand(), si->getValueOperand()->getType(), true, false);
        } else if (auto *rmw = llvm::dyn_cast<llvm::AtomicRMWInst>(&inst)) {
          record_ptr_access(rmw->getPointerOperand(), rmw->getValOperand()->getType(), true, true);
        } else if (auto *cx = llvm::dyn_cast<llvm::AtomicCmpXchgInst>(&inst)) {
          record_ptr_access(cx->getPointerOperand(), cx->getCompareOperand()->getType(), true, true);
        } else if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&inst)) {
          llvm::Function *callee = ci->getCalledFunction();
          if (!callee) continue;
          llvm::StringRef cname = callee->getName();
          if (cname.starts_with("llvm.memset.") && ci->arg_size() >= 3) {
            auto *len_c = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(2));
            uint64_t nbytes = 0;
            bool byte_gran = !len_c || !try_get_u64(len_c, nbytes) || (nbytes & 3u) != 0 ||
                             !llvm::isa<llvm::ConstantInt>(ci->getArgOperand(1));
            record_ptr_access(ci->getArgOperand(0), byte_gran ? i8_ty : i32_ty, true, false);
          } else if ((cname.starts_with("llvm.memcpy.") || cname.starts_with("llvm.memmove.")) &&
                     ci->arg_size() >= 3) {
            auto *len_c = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(2));
            uint64_t nbytes = 0;
            bool byte_gran = !len_c || !try_get_u64(len_c, nbytes) || (nbytes & 3u) != 0;
            llvm::Type *mty = byte_gran ? i8_ty : i32_ty;
            record_ptr_access(ci->getArgOperand(0), mty, true, false);
            record_ptr_access(ci->getArgOperand(1), mty, false, false);
          }
        }
      }
    }

    auto pick_elem = [&](const BufferBinding &st, bool is_ssbo) -> ElemKind {
      if (!st.saw_access) return ElemKind::U32;
      if (st.has_atomic) return ElemKind::U32;
      if (st.all_half && spv.hw_caps.has_float16 && (!is_ssbo || spv.hw_caps.has_storage_16bit)) {
        return ElemKind::F16;
      }
      if (st.all_f32) return ElemKind::F32;
      if (st.min_store_bytes == 1 && spv.hw_caps.has_int8 &&
          (!is_ssbo || spv.hw_caps.has_storage_8bit)) {
        return ElemKind::U8;
      }
      if (st.min_store_bytes == 2 && spv.hw_caps.has_int16 &&
          (!is_ssbo || spv.hw_caps.has_storage_16bit)) {
        return ElemKind::U16;
      }
      return ElemKind::U32;
    };

    for (uint32_t s = 0; s < kMaxBufferSlots; s++) {
      if (!buffers[s].used) continue;
      /* Infer NonWritable directly from post-inlining stores/atomics/mem-intrinsics. */
      buffers[s].readonly = !buffers[s].has_store;
      buffers[s].elem = pick_elem(buffers[s], true);
      buffers[s].var_id = spv.make_ssbo_var(s, buffers[s].elem, buffers[s].readonly,
                                            &buffers[s].ptr_elem_ty);
      if (!buffers[s].var_id) return false;
    }
    for (size_t i = 0; i < wg_arrays.size(); i++) {
      wg_arrays[i].elem = pick_elem(wg_stats[i], false);
      wg_arrays[i].var_id = spv.make_mem_array_var(
          spv::StorageClassWorkgroup, wg_arrays[i].elem, wg_arrays[i].total_bytes,
          &wg_arrays[i].ptr_elem_ty);
      if (!wg_arrays[i].var_id) return false;
    }
    for (size_t i = 0; i < fn_arrays.size(); i++) {
      fn_arrays[i].elem = pick_elem(fn_stats[i], false);
      fn_arrays[i].var_id = spv.make_mem_array_var(
          spv::StorageClassFunction, fn_arrays[i].elem, fn_arrays[i].total_bytes,
          &fn_arrays[i].ptr_elem_ty);
      if (!fn_arrays[i].var_id) return false;
    }
    return true;
  }

  uint32_t get_or_create_builtin_var(BuiltinKind bk) {
    auto it = builtin_vars.find(bk);
    if (it != builtin_vars.end()) return it->second;
    uint32_t u32 = spv.get_int(32);
    uint32_t v3u = spv.get_vec(u32, 3);
    uint32_t id = 0;
    switch (bk) {
    case BuiltinKind::GlobalInvocationId:
      id = spv.make_builtin_var(spv::BuiltInGlobalInvocationId, v3u);
      break;
    case BuiltinKind::LocalInvocationId:
      id = spv.make_builtin_var(spv::BuiltInLocalInvocationId, v3u);
      break;
    case BuiltinKind::WorkgroupId:
      id = spv.make_builtin_var(spv::BuiltInWorkgroupId, v3u);
      break;
    case BuiltinKind::NumWorkgroups:
      id = spv.make_builtin_var(spv::BuiltInNumWorkgroups, v3u);
      break;
    case BuiltinKind::LocalInvocationIndex:
      id = spv.make_builtin_var(spv::BuiltInLocalInvocationIndex, u32);
      break;
    case BuiltinKind::SubgroupLocalInvocationId:
      if ((spv.hw_caps.subgroup_ops & AIR2SPV_SUBGROUP_BASIC) == 0) {
        spv.fail("shader requires subgroup basic capability unsupported by GPU");
        return 0;
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      id = spv.make_builtin_var(spv::BuiltInSubgroupLocalInvocationId, u32);
      break;
    case BuiltinKind::SubgroupSize:
      if ((spv.hw_caps.subgroup_ops & AIR2SPV_SUBGROUP_BASIC) == 0) {
        spv.fail("shader requires subgroup basic capability unsupported by GPU");
        return 0;
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      id = spv.make_builtin_var(spv::BuiltInSubgroupSize, u32);
      break;
    case BuiltinKind::SubgroupId:
      if ((spv.hw_caps.subgroup_ops & AIR2SPV_SUBGROUP_BASIC) == 0) {
        spv.fail("shader requires subgroup basic capability unsupported by GPU");
        return 0;
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      id = spv.make_builtin_var(spv::BuiltInSubgroupId, u32);
      break;
    case BuiltinKind::NumSubgroups:
      if ((spv.hw_caps.subgroup_ops & AIR2SPV_SUBGROUP_BASIC) == 0) {
        spv.fail("shader requires subgroup basic capability unsupported by GPU");
        return 0;
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      id = spv.make_builtin_var(spv::BuiltInNumSubgroups, u32);
      break;
    default: return 0;
    }
    builtin_vars[bk] = id;
    return id;
  }

  uint32_t load_builtin_for_arg(BuiltinKind bk, llvm::Type *arg_ty) {
    uint32_t u32 = spv.get_int(32);
    uint32_t v3u = spv.get_vec(u32, 3);
    uint32_t raw_id = 0;
    bool is_vec3 = false;

    switch (bk) {
    case BuiltinKind::GlobalInvocationId:
    case BuiltinKind::LocalInvocationId:
    case BuiltinKind::WorkgroupId:
    case BuiltinKind::NumWorkgroups: {
      uint32_t var = get_or_create_builtin_var(bk);
      if (!var) return 0;
      raw_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {v3u, raw_id, var});
      is_vec3 = true;
      break;
    }
    case BuiltinKind::WorkgroupSize:
      raw_id = spv.wg_size_v3;
      is_vec3 = true;
      break;
    case BuiltinKind::GridSize: {
      uint32_t nwg_var = get_or_create_builtin_var(BuiltinKind::NumWorkgroups);
      if (!nwg_var) return 0;
      uint32_t nwg = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {v3u, nwg, nwg_var});
      raw_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpIMul, {v3u, raw_id, nwg, spv.wg_size_v3});
      is_vec3 = true;
      break;
    }
    case BuiltinKind::LocalInvocationIndex: {
      uint32_t var = get_or_create_builtin_var(bk);
      if (!var) return 0;
      raw_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {u32, raw_id, var});
      is_vec3 = false;
      break;
    }
    case BuiltinKind::SubgroupLocalInvocationId:
    case BuiltinKind::SubgroupSize:
    case BuiltinKind::SubgroupId:
    case BuiltinKind::NumSubgroups: {
      if (spv.hw_caps.subgroup_size == 16u) {
        /* Emulate Apple Silicon 32-lane SIMD groups on 16-lane Vulkan hardware (Mali/V3D). */
        if (bk == BuiltinKind::SubgroupSize) {
          raw_id = spv.get_const_u32(32u);
        } else if (bk == BuiltinKind::NumSubgroups) {
          uint32_t lx_ly = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpIMul, {u32, lx_ly, spv.spec_lx, spv.spec_ly});
          uint32_t total = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpIMul, {u32, total, lx_ly, spv.spec_lz});
          uint32_t plus31 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpIAdd, {u32, plus31, total, spv.get_const_u32(31u)});
          raw_id = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, raw_id, plus31, spv.get_const_u32(5u)});
        } else {
          uint32_t lvar = get_or_create_builtin_var(BuiltinKind::LocalInvocationIndex);
          if (!lvar) return 0;
          uint32_t lidx = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpLoad, {u32, lidx, lvar});
          raw_id = spv.alloc_id();
          if (bk == BuiltinKind::SubgroupLocalInvocationId) {
            spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, raw_id, lidx, spv.get_const_u32(31u)});
          } else {
            spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, raw_id, lidx, spv.get_const_u32(5u)});
          }
        }
        is_vec3 = false;
        break;
      }
      uint32_t var = get_or_create_builtin_var(bk);
      if (!var) return 0;
      raw_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {u32, raw_id, var});
      is_vec3 = false;
      break;
    }
    default: return 0;
    }

    uint32_t sized_id = raw_id;
    uint32_t want_elems = 1;
    uint32_t want_bits = 32;
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(arg_ty)) {
      llvm::Type *et = vt->getElementType();
      if (!et->isIntegerTy() || et->isIntegerTy(1)) {
        spv.fail("unsupported builtin vector element type");
        return 0;
      }
      want_elems = vt->getNumElements();
      want_bits = et->getIntegerBitWidth();
    } else if (arg_ty->isIntegerTy() && !arg_ty->isIntegerTy(1)) {
      want_elems = 1;
      want_bits = arg_ty->getIntegerBitWidth();
    } else {
      spv.fail("unsupported builtin argument type");
      return 0;
    }

    if (is_vec3 && (want_elems < 1 || want_elems > 3)) {
      spv.fail("unsupported vec3 builtin lane count");
      return 0;
    }
    if (!is_vec3 && want_elems != 1) {
      spv.fail("scalar builtin cannot have vector type");
      return 0;
    }

    if (is_vec3 && want_elems == 1) {
      sized_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpCompositeExtract, {u32, sized_id, raw_id, 0u});
    } else if (is_vec3 && want_elems == 2) {
      uint32_t v2u = spv.get_vec(u32, 2);
      sized_id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpVectorShuffle, {v2u, sized_id, raw_id, raw_id, 0u, 1u});
    }

    if (want_bits != 32) {
      uint32_t dst_ty = spv.map_llvm_type(arg_ty);
      if (!dst_ty) return 0;
      uint32_t conv = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpUConvert, {dst_ty, conv, sized_id});
      sized_id = conv;
    }
    return sized_id;
  }

  static bool is_zero_ext_u32(const llvm::Value *v) {
    if (!v) return false;
    if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(v)) {
      return ci->getBitWidth() <= 64 && ci->getZExtValue() <= 0xFFFFFFFFULL;
    }
    if (auto *z = llvm::dyn_cast<llvm::ZExtInst>(v)) {
      return z->getOperand(0)->getType()->isIntegerTy() &&
             z->getOperand(0)->getType()->getIntegerBitWidth() <= 32;
    }
    if (auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(v)) {
      if (bo->getOpcode() == llvm::Instruction::And) {
        if (auto *c = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1))) {
          return c->getBitWidth() <= 64 && c->getZExtValue() <= 0xFFFFFFFFULL;
        }
        if (auto *c = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(0))) {
          return c->getBitWidth() <= 64 && c->getZExtValue() <= 0xFFFFFFFFULL;
        }
      }
    }
    return false;
  }

  static llvm::Value *match_sext32_in_i64(const llvm::Instruction *inst) {
    auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(inst);
    if (!bo || bo->getOpcode() != llvm::Instruction::AShr) return nullptr;
    auto *c2 = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
    uint64_t sh2 = 0;
    if (!c2 || !try_get_u64(c2, sh2) || sh2 != 32u) return nullptr;
    auto *shl = llvm::dyn_cast<llvm::BinaryOperator>(bo->getOperand(0));
    if (!shl || shl->getOpcode() != llvm::Instruction::Shl) return nullptr;
    auto *c1 = llvm::dyn_cast<llvm::ConstantInt>(shl->getOperand(1));
    uint64_t sh1 = 0;
    if (!c1 || !try_get_u64(c1, sh1) || sh1 != 32u) return nullptr;
    return shl->getOperand(0);
  }

  static bool match_magic_div_lshr(const llvm::Instruction *inst,
                                   llvm::Value *&mul_a,
                                   llvm::Value *&mul_b,
                                   uint32_t &shift_k) {
    auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(inst);
    if (!bo || bo->getOpcode() != llvm::Instruction::LShr) return false;
    auto *shc = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
    uint64_t shv = 0;
    if (!shc || !try_get_u64(shc, shv) || shv < 32u || shv >= 64u) return false;
    auto *mul = llvm::dyn_cast<llvm::BinaryOperator>(bo->getOperand(0));
    if (!mul || mul->getOpcode() != llvm::Instruction::Mul) return false;
    if (!is_zero_ext_u32(mul->getOperand(0)) || !is_zero_ext_u32(mul->getOperand(1))) return false;
    mul_a = mul->getOperand(0);
    mul_b = mul->getOperand(1);
    shift_k = static_cast<uint32_t>(shv);
    return true;
  }

  /* Precompute the globally consistent greatest fixed point of i64 index-only instructions
     so narrowed i64 values never leak a u32 SSA ID into an un-narrowed i64 instruction. */
  void compute_narrowed_i64_set(const std::vector<llvm::BasicBlock *> &order) {
    narrowed_i64_set.clear();
    std::unordered_set<const llvm::BasicBlock *> reachable(order.begin(), order.end());
    std::vector<const llvm::Instruction *> cands;
    std::unordered_set<const llvm::Value *> ptr_hi_tainted;
    for (llvm::BasicBlock *bb : order) {
      for (llvm::Instruction &inst : *bb) {
        if (!inst.getType()->isIntegerTy(64) || inst.use_empty()) continue;
        bool ok = llvm::isa<llvm::ZExtInst>(&inst) || llvm::isa<llvm::SExtInst>(&inst) ||
                  llvm::isa<llvm::PHINode>(&inst) || llvm::isa<llvm::SelectInst>(&inst);
        if (!spv.hw_caps.has_int64 && llvm::isa<llvm::PtrToIntInst>(&inst)) {
          ok = true;
          ptr_hi_tainted.insert(&inst);
        }
        if (!spv.hw_caps.has_int64 && llvm::isa<llvm::LoadInst>(&inst)) {
          auto *li = llvm::cast<llvm::LoadInst>(&inst);
          auto *gv = llvm::dyn_cast<llvm::GlobalVariable>(li->getPointerOperand()->stripPointerCasts());
          if (!gv || !fc_map.count(gv)) {
            ok = true;
          }
        }
        if (auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(&inst)) {
          switch (bo->getOpcode()) {
          case llvm::Instruction::Add:
          case llvm::Instruction::Sub:
          case llvm::Instruction::Mul:
          case llvm::Instruction::Or:
          case llvm::Instruction::And:
            ok = true;
            break;
          case llvm::Instruction::Shl: {
            auto *shc = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
            uint64_t shv = 0;
            if (shc && try_get_u64(shc, shv)) {
              if (shv < 32u) {
                ok = true;
              } else if (shv == 32u) {
                bool only_sext32 = true;
                for (const llvm::User *u : bo->users()) {
                  auto *ui = llvm::dyn_cast<llvm::Instruction>(u);
                  if (!ui || !reachable.count(ui->getParent())) continue;
                  if (!match_sext32_in_i64(ui)) { only_sext32 = false; break; }
                }
                if (only_sext32) ok = true;
              }
            }
            break;
          }
          case llvm::Instruction::LShr: {
            auto *shc = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
            uint64_t shv = 0;
            llvm::Value *ma = nullptr, *mb = nullptr;
            uint32_t mk = 0;
            if (!spv.hw_caps.has_int64 && match_magic_div_lshr(bo, ma, mb, mk)) {
              ok = true;
            } else if (shc && try_get_u64(shc, shv) && shv < 32u &&
                       (is_zero_ext_u32(bo->getOperand(0)) ||
                        (!spv.hw_caps.has_int64 && llvm::isa<llvm::LoadInst>(bo->getOperand(0))))) {
              ok = true;
            }
            break;
          }
          case llvm::Instruction::AShr: {
            auto *shc = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
            uint64_t shv = 0;
            if (match_sext32_in_i64(bo)) {
              ok = true;
            } else if (!spv.hw_caps.has_int64 && shc && try_get_u64(shc, shv) && shv < 32u &&
                       (llvm::isa<llvm::SExtInst>(bo->getOperand(0)) ||
                        llvm::isa<llvm::LoadInst>(bo->getOperand(0)))) {
              ok = true;
            }
            break;
          }
          case llvm::Instruction::UDiv:
          case llvm::Instruction::SDiv:
          case llvm::Instruction::URem:
          case llvm::Instruction::SRem:
          case llvm::Instruction::Xor:
            if (!spv.hw_caps.has_int64) ok = true;
            break;
          default: break;
          }
        }
        if (ok) {
          narrowed_i64_set.insert(&inst);
          cands.push_back(&inst);
        }
      }
    }

    /* Propagate ptrtoint upper-32-bit taint; only And with <= 0xFFFFFFFF clears upper bits. */
    if (!ptr_hi_tainted.empty()) {
      bool tchanged = true;
      while (tchanged) {
        tchanged = false;
        for (const llvm::Instruction *inst : cands) {
          if (ptr_hi_tainted.count(inst)) continue;
          if (auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(inst)) {
            if (bo->getOpcode() == llvm::Instruction::And) {
              auto *c0 = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(0));
              auto *c1 = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
              if ((c0 && c0->getBitWidth() <= 64 && c0->getZExtValue() <= 0xFFFFFFFFULL) ||
                  (c1 && c1->getBitWidth() <= 64 && c1->getZExtValue() <= 0xFFFFFFFFULL)) {
                continue;
              }
            }
          }
          for (const llvm::Value *op : inst->operands()) {
            if (ptr_hi_tainted.count(op)) {
              ptr_hi_tainted.insert(inst);
              tchanged = true;
              break;
            }
          }
        }
      }
    }

    bool changed = true;
    while (changed) {
      changed = false;
      for (const llvm::Instruction *inst : cands) {
        if (!narrowed_i64_set.count(inst)) continue;
        bool keep = true;
        if (auto *phi = llvm::dyn_cast<llvm::PHINode>(inst)) {
          for (unsigned k = 0; k < phi->getNumIncomingValues(); k++) {
            if (!reachable.count(phi->getIncomingBlock(k))) continue;
            const llvm::Value *inc = phi->getIncomingValue(k);
            if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(inc)) {
              if (ci->getBitWidth() <= 64) continue;
              keep = false;
              break;
            }
            if (llvm::isa<llvm::UndefValue>(inc) || llvm::isa<llvm::PoisonValue>(inc)) continue;
            if (narrowed_i64_set.count(inc)) continue;
            keep = false;
            break;
          }
        }
        if (keep) {
          for (const llvm::Use &u : inst->uses()) {
            const llvm::User *usr = u.getUser();
            if (auto *ui = llvm::dyn_cast<llvm::Instruction>(usr)) {
              if (!reachable.count(ui->getParent())) continue;
            }
            if (llvm::isa<llvm::GetElementPtrInst>(usr)) {
              if (u.getOperandNo() == 0) { keep = false; break; }
            } else if (llvm::isa<llvm::ExtractElementInst>(usr)) {
              if (u.getOperandNo() != 1) { keep = false; break; }
            } else if (llvm::isa<llvm::InsertElementInst>(usr)) {
              if (u.getOperandNo() != 2) { keep = false; break; }
            } else if (auto *tr = llvm::dyn_cast<llvm::TruncInst>(usr)) {
              if (tr->getType()->getIntegerBitWidth() > 32) { keep = false; break; }
            } else if (auto *ic = llvm::dyn_cast<llvm::ICmpInst>(usr)) {
              if (spv.hw_caps.has_int64) { keep = false; break; }
              if (ptr_hi_tainted.count(ic->getOperand(0)) ||
                  ptr_hi_tainted.count(ic->getOperand(1))) {
                keep = false;
                break;
              }
              auto ok_op = [&](const llvm::Value *v) {
                return narrowed_i64_set.count(v) || llvm::isa<llvm::ConstantInt>(v) ||
                       llvm::isa<llvm::UndefValue>(v) || llvm::isa<llvm::PoisonValue>(v);
              };
              if (!ok_op(ic->getOperand(0)) || !ok_op(ic->getOperand(1))) { keep = false; break; }
            } else if (narrowed_i64_set.count(usr)) {
              continue;
            } else {
              keep = false;
              break;
            }
          }
        }
        if (!keep) {
          narrowed_i64_set.erase(inst);
          changed = true;
        }
      }
    }
  }

  bool is_i64_index_only(const llvm::Value *v) const {
    return narrowed_i64_set.count(v) != 0;
  }

  /* Materialize a single i64 index-only instruction in-place as u32 (O(1), dominance-safe). */
  uint32_t emit_buf_pc_base_off(uint32_t slot) {
    uint32_t raw = emit_pc_load(1u, slot);
    if (slot < 16u && buf_sub_off_ids[slot] != 0) {
      uint32_t mask = ~(spv.hw_caps.ssbo_offset_align - 1u);
      uint32_t aligned = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitwiseAnd,
                  {spv.get_int(32), aligned, raw, spv.get_const_u32(mask)});
      return aligned;
    }
    return raw;
  }

  bool emit_ptr_to_u32_pair(llvm::Value *src_v, uint32_t &off32, uint32_t &hi32) {
    uint32_t u32 = spv.get_int(32);
    off32 = spv.get_const_u32(0u);
    hi32 = spv.get_const_u32(0u);
    if (llvm::isa<llvm::ConstantPointerNull>(src_v) ||
        llvm::isa<llvm::UndefValue>(src_v) || llvm::isa<llvm::PoisonValue>(src_v)) {
      return true;
    }
    PtrInfo p = resolve_ptr(src_v);
    if (p.kind == PtrKind::Invalid || p.is_dual()) {
      return spv.fail("unsupported ptrtoint pointer operand");
    }
    off32 = materialize_byte_off(p);
    if (p.kind == PtrKind::Buffer && p.base_id < 16u) {
      hi32 = emit_pc_load(0u, p.base_id);
      off32 = emit_u32_add(emit_buf_pc_base_off(p.base_id), off32);
    } else if (p.kind == PtrKind::Workgroup) {
      hi32 = spv.get_const_u32(0x40000000u | p.base_id);
    } else if (p.kind == PtrKind::Function) {
      hi32 = spv.get_const_u32(0x20000000u | p.base_id);
    }
    uint32_t is_null = get_ptr_is_null(src_v);
    if (is_null != spv.get_const_bool(false)) {
      uint32_t z32 = spv.get_const_u32(0u);
      uint32_t sel_lo = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelect, {u32, sel_lo, is_null, z32, off32});
      uint32_t sel_hi = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelect, {u32, sel_hi, is_null, z32, hi32});
      off32 = sel_lo;
      hi32 = sel_hi;
    }
    return spv.error.empty();
  }

  uint32_t materialize_narrowed_i64(llvm::Instruction *inst) {
    if (auto *p2i = llvm::dyn_cast<llvm::PtrToIntInst>(inst)) {
      uint32_t off32 = 0, hi32 = 0;
      if (!emit_ptr_to_u32_pair(p2i->getOperand(0), off32, hi32)) return 0;
      return off32;
    }
    if (auto *li = llvm::dyn_cast<llvm::LoadInst>(inst)) {
      PtrInfo ptr = resolve_ptr(li->getPointerOperand());
      if (ptr.kind == PtrKind::Invalid || ptr.kind == PtrKind::FuncConst) {
        spv.fail("invalid narrowed i64 load");
        return 0;
      }
      return emit_mem_load_ptr(ptr, llvm::Type::getInt32Ty(mod.getContext()));
    }
    if (auto *z = llvm::dyn_cast<llvm::ZExtInst>(inst)) {
      return get_index_u32(z->getOperand(0));
    }
    if (auto *s = llvm::dyn_cast<llvm::SExtInst>(inst)) {
      llvm::Value *src = s->getOperand(0);
      if (!src->getType()->isIntegerTy()) {
        spv.fail("non-integer sext source in index");
        return 0;
      }
      if (src->getType()->isIntegerTy(32)) return get_val(src);
      uint32_t sv = get_val(src);
      if (!sv) return 0;
      uint32_t u32 = spv.get_int(32);
      uint32_t r = spv.alloc_id();
      if (src->getType()->isIntegerTy(1)) {
        spv.emit_op(cur_body, spv::OpSelect,
                    {u32, r, sv, spv.get_const_u32(0xFFFFFFFFu), spv.get_const_u32(0)});
      } else {
        spv.emit_op(cur_body, spv::OpSConvert, {u32, r, sv});
      }
      return r;
    }
    if (auto *sel = llvm::dyn_cast<llvm::SelectInst>(inst)) {
      uint32_t c = get_val(sel->getCondition());
      uint32_t tv = get_index_u32(sel->getTrueValue());
      uint32_t fv = get_index_u32(sel->getFalseValue());
      if (!c || !tv || !fv) return 0;
      uint32_t r = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelect, {spv.get_int(32), r, c, tv, fv});
      return r;
    }
    if (auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(inst)) {
      if (llvm::Value *inner = match_sext32_in_i64(bo)) {
        return get_index_u32(inner);
      }
      if (bo->getOpcode() == llvm::Instruction::Shl) {
        auto *shc = llvm::dyn_cast<llvm::ConstantInt>(bo->getOperand(1));
        uint64_t shv = 0;
        if (shc && try_get_u64(shc, shv) && shv >= 32u) {
          return spv.get_const_u32(0u);
        }
      }
      llvm::Value *ma = nullptr, *mb = nullptr;
      uint32_t mk = 0;
      if (match_magic_div_lshr(bo, ma, mb, mk)) {
        uint32_t a32 = get_index_u32(ma);
        uint32_t b32 = get_index_u32(mb);
        if (!a32 || !b32) return 0;
        uint32_t u32 = spv.get_int(32);
        uint32_t pair_ty = spv.alloc_id();
        spv.emit_op(spv.types_consts_vars, spv::OpTypeStruct, {pair_ty, u32, u32});
        uint32_t ext = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUMulExtended, {pair_ty, ext, a32, b32});
        uint32_t hi = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpCompositeExtract, {u32, hi, ext, 1u});
        if (mk == 32u) return hi;
        uint32_t r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpShiftRightLogical,
                    {u32, r, hi, spv.get_const_u32(mk - 32u)});
        return r;
      }
      uint32_t a = get_index_u32(bo->getOperand(0));
      uint32_t b = get_index_u32(bo->getOperand(1));
      if (!a || !b) return 0;
      spv::Op op = spv::OpNop;
      switch (bo->getOpcode()) {
      case llvm::Instruction::Add: op = spv::OpIAdd; break;
      case llvm::Instruction::Sub: op = spv::OpISub; break;
      case llvm::Instruction::Mul: op = spv::OpIMul; break;
      case llvm::Instruction::Shl: op = spv::OpShiftLeftLogical; break;
      case llvm::Instruction::LShr: op = spv::OpShiftRightLogical; break;
      case llvm::Instruction::AShr: op = spv::OpShiftRightArithmetic; break;
      case llvm::Instruction::UDiv: op = spv::OpUDiv; break;
      case llvm::Instruction::SDiv: op = spv::OpSDiv; break;
      case llvm::Instruction::URem: op = spv::OpUMod; break;
      case llvm::Instruction::SRem: op = spv::OpSRem; break;
      case llvm::Instruction::Or: op = spv::OpBitwiseOr; break;
      case llvm::Instruction::And: op = spv::OpBitwiseAnd; break;
      case llvm::Instruction::Xor: op = spv::OpBitwiseXor; break;
      default: break;
      }
      if (op == spv::OpNop) {
        spv.fail("unsupported narrowed i64 binary operator");
        return 0;
      }
      uint32_t r = spv.alloc_id();
      spv.emit_op(cur_body, op, {spv.get_int(32), r, a, b});
      return r;
    }
    spv.fail("unsupported narrowed i64 instruction");
    return 0;
  }

  /* Obtain a u32 SSA value for GEP/vector/subgroup index expressions (O(1), non-recursive). */
  uint32_t get_index_u32(llvm::Value *v) {
    if (!v || !v->getType()->isIntegerTy()) {
      spv.fail("non-integer index expression");
      return 0;
    }
    if (auto it = narrowed_i64_vals.find(v); it != narrowed_i64_vals.end()) {
      return it->second;
    }
    if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(v)) {
      uint64_t val = 0;
      if (!try_get_u64(ci, val)) {
        spv.fail("unsupported integer index width");
        return 0;
      }
      return spv.get_const_u32(static_cast<uint32_t>(val));
    }
    if (llvm::isa<llvm::UndefValue>(v) || llvm::isa<llvm::PoisonValue>(v)) {
      return spv.get_undef(spv.get_int(32));
    }
    if (auto *z = llvm::dyn_cast<llvm::ZExtInst>(v)) {
      if (z->getOperand(0)->getType()->isIntegerTy(32)) return get_val(z->getOperand(0));
    }
    if (auto *s = llvm::dyn_cast<llvm::SExtInst>(v)) {
      if (s->getOperand(0)->getType()->isIntegerTy(32)) return get_val(s->getOperand(0));
    }
    uint32_t raw = get_val(v);
    if (!raw) return 0;
    unsigned bw = v->getType()->getIntegerBitWidth();
    if (bw == 32) return raw;
    uint32_t u32 = spv.get_int(32);
    uint32_t res = spv.alloc_id();
    if (bw == 1) {
      spv.emit_op(cur_body, spv::OpSelect,
                  {u32, res, raw, spv.get_const_u32(1), spv.get_const_u32(0)});
    } else {
      spv.emit_op(cur_body, spv::OpUConvert, {u32, res, raw});
    }
    return res;
  }

  uint32_t emit_u32_add(uint32_t a, uint32_t b) {
    uint32_t z = spv.get_const_u32(0);
    if (a == z) return b;
    if (b == z) return a;
    uint32_t r = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpIAdd, {spv.get_int(32), r, a, b});
    return r;
  }

  uint32_t emit_u32_mul_const(uint32_t a, uint32_t c) {
    if (c == 0) return spv.get_const_u32(0);
    if (c == 1) return a;
    if ((c & (c - 1u)) == 0) {
      uint32_t shift = 0;
      while ((1u << shift) < c) shift++;
      uint32_t r = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpShiftLeftLogical,
                  {spv.get_int(32), r, a, spv.get_const_u32(shift)});
      return r;
    }
    uint32_t r = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpIMul, {spv.get_int(32), r, a, spv.get_const_u32(c)});
    return r;
  }

  uint32_t emit_u32_shr_const(uint32_t a, uint32_t shift) {
    if (shift == 0) return a;
    if (a == spv.get_const_u32(0)) return a;
    uint32_t r = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpShiftRightLogical,
                {spv.get_int(32), r, a, spv.get_const_u32(shift)});
    return r;
  }

  uint32_t materialize_byte_off(const PtrInfo &p) {
    if (p.idx_id == 0 || p.idx_scale == 0) return p.byte_off_id;
    return emit_u32_add(p.byte_off_id, emit_u32_mul_const(p.idx_id, p.idx_scale));
  }

  uint32_t materialize_elem_idx(const PtrInfo &p, uint32_t shift) {
    uint32_t esz = 1u << shift;
    uint32_t z = spv.get_const_u32(0);
    if (p.byte_off_id == z) {
      if (p.idx_id == 0) return z;
      if (p.idx_scale == esz) return p.idx_id;
      if (p.idx_scale != 0 && (p.idx_scale & (esz - 1u)) == 0) {
        return emit_u32_mul_const(p.idx_id, p.idx_scale >> shift);
      }
    }
    return emit_u32_shr_const(materialize_byte_off(p), shift);
  }

  PtrInfo resolve_ptr(llvm::Value *v, int depth = 0) {
    if (!v || depth > kMaxPtrDepth) {
      spv.fail("pointer expression too deep");
      return {};
    }
    bool cacheable = llvm::isa<llvm::Argument>(v) || llvm::isa<llvm::GlobalVariable>(v) ||
                     llvm::isa<llvm::Instruction>(v);
    if (cacheable) {
      auto it = ptr_map.find(v);
      if (it != ptr_map.end()) return it->second;
    }

    if (auto *arg = llvm::dyn_cast<llvm::Argument>(v)) {
      unsigned idx = arg->getArgNo();
      if (idx < arg_meta.size() && arg_meta[idx].is_buffer) {
        PtrInfo p;
        p.byte_off_id = spv.get_const_u32(0);
        if (arg_meta[idx].addr_space == 1 || arg_meta[idx].addr_space == 2) {
          p.kind = PtrKind::Buffer;
          p.base_id = arg_meta[idx].slot;
          if (p.base_id < 16u && buf_sub_off_ids[p.base_id] != 0) {
            p.byte_off_id = buf_sub_off_ids[p.base_id];
          }
        } else if (arg_meta[idx].addr_space == 3) {
          auto wit = wg_arg_map.find(arg);
          if (wit == wg_arg_map.end()) {
            spv.fail("unregistered threadgroup argument");
            return {};
          }
          p.kind = PtrKind::Workgroup;
          p.base_id = wit->second;
        }
        ptr_map[v] = p;
        return p;
      }
      spv.fail("unknown pointer argument in AIR");
      return {};
    }
    if (auto *gv = llvm::dyn_cast<llvm::GlobalVariable>(v)) {
      if (auto fit = fc_map.find(gv); fit != fc_map.end()) {
        PtrInfo p{PtrKind::FuncConst, fit->second.spec_id, spv.get_const_u32(0), 0, 0};
        ptr_map[v] = p;
        return p;
      }
      if (auto wit = wg_gv_map.find(gv); wit != wg_gv_map.end()) {
        PtrInfo p{PtrKind::Workgroup, wit->second, spv.get_const_u32(0), 0, 0};
        ptr_map[v] = p;
        return p;
      }
      spv.fail("unsupported global variable pointer: " + gv->getName().str());
      return {};
    }
    if (auto *ai = llvm::dyn_cast<llvm::AllocaInst>(v)) {
      auto ait = alloca_map.find(ai);
      if (ait == alloca_map.end()) {
        spv.fail("untracked alloca pointer");
        return {};
      }
      PtrInfo p{PtrKind::Function, ait->second, spv.get_const_u32(0), 0, 0};
      ptr_map[v] = p;
      return p;
    }
    if (auto *bc = llvm::dyn_cast<llvm::BitCastOperator>(v)) {
      PtrInfo p = resolve_ptr(bc->getOperand(0), depth + 1);
      if (cacheable && p.kind != PtrKind::Invalid) ptr_map[v] = p;
      return p;
    }
    if (auto *asc = llvm::dyn_cast<llvm::AddrSpaceCastOperator>(v)) {
      PtrInfo p = resolve_ptr(asc->getOperand(0), depth + 1);
      if (cacheable && p.kind != PtrKind::Invalid) ptr_map[v] = p;
      return p;
    }
    if (auto *fi = llvm::dyn_cast<llvm::FreezeInst>(v)) {
      PtrInfo p = resolve_ptr(fi->getOperand(0), depth + 1);
      if (cacheable && p.kind != PtrKind::Invalid) ptr_map[v] = p;
      return p;
    }
    if (auto *gep = llvm::dyn_cast<llvm::GEPOperator>(v)) {
      PtrInfo base = resolve_ptr(gep->getPointerOperand(), depth + 1);
      if (base.kind == PtrKind::Invalid) return {};
      uint32_t z = spv.get_const_u32(0);
      uint32_t off = base.is_dual() ? z : ((base.idx_id == 0) ? base.byte_off_id : materialize_byte_off(base));
      uint32_t dyn_idx = 0;
      uint32_t dyn_scale = 0;
      llvm::Type *cur_ty = gep->getSourceElementType();
      uint64_t const_bytes = 0;

      auto add_dyn = [&](uint32_t id32, uint32_t stride) {
        if (stride == 0) return;
        if (off == z && dyn_idx == 0) {
          dyn_idx = id32;
          dyn_scale = stride;
        } else {
          if (dyn_idx != 0) {
            off = emit_u32_add(off, emit_u32_mul_const(dyn_idx, dyn_scale));
            dyn_idx = 0;
            dyn_scale = 0;
          }
          off = emit_u32_add(off, emit_u32_mul_const(id32, stride));
        }
      };

      for (unsigned i = 1; i < gep->getNumOperands(); i++) {
        llvm::Value *idx_v = gep->getOperand(i);
        if (i == 1) {
          uint64_t stride = 0;
          if (!try_type_alloc_bytes(dl, cur_ty, stride) || stride > UINT32_MAX) {
            spv.fail("invalid or oversized GEP element stride");
            return {};
          }
          if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(idx_v)) {
            int64_t sv = 0;
            if (!try_get_s64(ci, sv)) {
              spv.fail("unsupported GEP constant index width");
              return {};
            }
            const_bytes += static_cast<uint64_t>(sv) * stride;
          } else {
            uint32_t id32 = get_index_u32(idx_v);
            if (!id32) return {};
            add_dyn(id32, static_cast<uint32_t>(stride));
          }
        } else if (auto *st = llvm::dyn_cast<llvm::StructType>(cur_ty)) {
          if (st->isOpaque()) {
            spv.fail("opaque struct in GEP");
            return {};
          }
          auto *ci = llvm::dyn_cast<llvm::ConstantInt>(idx_v);
          uint32_t field = 0;
          if (!ci || !try_get_u32(ci, field) || field >= st->getNumElements()) {
            spv.fail("invalid struct GEP field index");
            return {};
          }
          uint64_t foff = dl.getStructLayout(st)->getElementOffset(field);
          if (foff > UINT32_MAX) {
            spv.fail("struct field offset exceeds 32-bit range");
            return {};
          }
          const_bytes += foff;
          cur_ty = st->getElementType(field);
        } else if (auto *at = llvm::dyn_cast<llvm::ArrayType>(cur_ty)) {
          cur_ty = at->getElementType();
          uint64_t stride = 0;
          if (!try_type_alloc_bytes(dl, cur_ty, stride) || stride > UINT32_MAX) {
            spv.fail("invalid or oversized array GEP stride");
            return {};
          }
          if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(idx_v)) {
            int64_t sv = 0;
            if (!try_get_s64(ci, sv)) {
              spv.fail("unsupported array GEP index width");
              return {};
            }
            const_bytes += static_cast<uint64_t>(sv) * stride;
          } else {
            uint32_t id32 = get_index_u32(idx_v);
            if (!id32) return {};
            add_dyn(id32, static_cast<uint32_t>(stride));
          }
        } else if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(cur_ty)) {
          cur_ty = vt->getElementType();
          uint64_t stride = 0;
          if (!try_type_alloc_bytes(dl, cur_ty, stride) || stride > UINT32_MAX) {
            spv.fail("invalid or oversized vector GEP stride");
            return {};
          }
          if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(idx_v)) {
            int64_t sv = 0;
            if (!try_get_s64(ci, sv)) {
              spv.fail("unsupported vector GEP index width");
              return {};
            }
            const_bytes += static_cast<uint64_t>(sv) * stride;
          } else {
            uint32_t id32 = get_index_u32(idx_v);
            if (!id32) return {};
            add_dyn(id32, static_cast<uint32_t>(stride));
          }
        } else {
          spv.fail("unsupported GEP indexed type");
          return {};
        }
      }
      if (const_bytes != 0) {
        if (dyn_idx != 0) {
          off = emit_u32_add(off, emit_u32_mul_const(dyn_idx, dyn_scale));
          dyn_idx = 0;
          dyn_scale = 0;
        }
        off = emit_u32_add(off, spv.get_const_u32(static_cast<uint32_t>(const_bytes)));
      }
      if (base.is_dual()) {
        if (dyn_idx != 0) {
          off = emit_u32_add(off, emit_u32_mul_const(dyn_idx, dyn_scale));
          dyn_idx = 0;
          dyn_scale = 0;
        }
        uint32_t p_off = (off == z) ? base.byte_off_id : emit_u32_add(base.byte_off_id, off);
        uint32_t a_off = (off == z) ? base.alt_byte_off_id : emit_u32_add(base.alt_byte_off_id, off);
        PtrInfo res{base.kind, base.base_id, p_off, 0, 0,
                    base.sel_cond_id, base.alt_kind, base.alt_base_id, a_off};
        if (cacheable) ptr_map[v] = res;
        return res;
      }
      /* Materialize deferred scaled index eagerly if fed to a pointer PHI (directly or via cast/freeze). */
      if (dyn_idx != 0 && llvm::isa<llvm::Instruction>(v)) {
        std::vector<const llvm::User *> ustack(v->user_begin(), v->user_end());
        std::unordered_set<const llvm::User *> useen;
        bool feeds_phi = false;
        while (!ustack.empty() && useen.size() < 64u) {
          const llvm::User *u = ustack.back();
          ustack.pop_back();
          if (!useen.insert(u).second) continue;
          if (llvm::isa<llvm::PHINode>(u)) {
            feeds_phi = true;
            break;
          }
          if (llvm::isa<llvm::BitCastOperator>(u) || llvm::isa<llvm::AddrSpaceCastOperator>(u) ||
              llvm::isa<llvm::FreezeInst>(u)) {
            for (const llvm::User *nu : u->users()) ustack.push_back(nu);
          }
        }
        if (feeds_phi) {
          off = emit_u32_add(off, emit_u32_mul_const(dyn_idx, dyn_scale));
          dyn_idx = 0;
          dyn_scale = 0;
        }
      }
      PtrInfo res{base.kind, base.base_id, off, dyn_idx, dyn_scale};
      if (cacheable) ptr_map[v] = res;
      return res;
    }
    if (auto *li = llvm::dyn_cast<llvm::LoadInst>(v)) {
      if (li->getType()->isPointerTy()) {
        PtrInfo slot_ptr = resolve_ptr(li->getPointerOperand(), depth + 1);
        if (slot_ptr.kind == PtrKind::Function) {
          auto rit = ptr_alloca_roots.find(slot_ptr.base_id);
          if (rit != ptr_alloca_roots.end()) {
            uint32_t off = emit_mem_load_ptr(slot_ptr, llvm::Type::getInt32Ty(mod.getContext()));
            if (!off) return {};
            PtrInfo res{rit->second.first, rit->second.second, off, 0, 0};
            if (cacheable) ptr_map[v] = res;
            return res;
          }
        }
      }
      spv.fail("unresolvable pointer load in AIR");
      return {};
    }
    if (auto *sel = llvm::dyn_cast<llvm::SelectInst>(v)) {
      auto is_nullish = [](const llvm::Value *x) {
        return llvm::isa<llvm::UndefValue>(x) || llvm::isa<llvm::PoisonValue>(x) ||
               llvm::isa<llvm::ConstantPointerNull>(x);
      };
      auto is_mem_kind = [](PtrKind k) {
        return k == PtrKind::Buffer || k == PtrKind::Workgroup || k == PtrKind::Function;
      };
      bool tn = is_nullish(sel->getTrueValue());
      bool fn_null = is_nullish(sel->getFalseValue());
      if (tn && fn_null) {
        spv.fail("select between null pointers not supported");
        return {};
      }
      PtrInfo t = tn ? PtrInfo{} : resolve_ptr(sel->getTrueValue(), depth + 1);
      PtrInfo f = fn_null ? PtrInfo{} : resolve_ptr(sel->getFalseValue(), depth + 1);
      if (tn) {
        if (f.kind == PtrKind::Invalid || f.is_dual()) {
          spv.fail("nested multi-buffer pointer select not supported");
          return {};
        }
        t = PtrInfo{f.kind, f.base_id, spv.get_const_u32(0), 0, 0};
      } else if (fn_null) {
        if (t.kind == PtrKind::Invalid || t.is_dual()) {
          spv.fail("nested multi-buffer pointer select not supported");
          return {};
        }
        f = PtrInfo{t.kind, t.base_id, spv.get_const_u32(0), 0, 0};
      } else if (t.kind == PtrKind::Invalid || f.kind == PtrKind::Invalid) {
        return {};
      }
      uint32_t cond = get_val(sel->getCondition());
      if (!cond) return {};
      PtrInfo res;
      if (!t.is_dual() && !f.is_dual() && t.kind == f.kind && t.base_id == f.base_id) {
        uint32_t toff = materialize_byte_off(t);
        uint32_t foff = materialize_byte_off(f);
        uint32_t off = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {spv.get_int(32), off, cond, toff, foff});
        res = PtrInfo{t.kind, t.base_id, off, 0, 0};
      } else if (!t.is_dual() && !f.is_dual() && is_mem_kind(t.kind) && is_mem_kind(f.kind)) {
        uint32_t toff = materialize_byte_off(t);
        uint32_t foff = materialize_byte_off(f);
        auto root_key = [](PtrKind k, uint32_t b) {
          return std::make_pair(static_cast<uint8_t>(k), b);
        };
        if (root_key(f.kind, f.base_id) < root_key(t.kind, t.base_id)) {
          uint32_t not_c = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpLogicalNot, {spv.get_bool(), not_c, cond});
          res = PtrInfo{f.kind, f.base_id, foff, 0, 0, not_c, t.kind, t.base_id, toff};
        } else {
          res = PtrInfo{t.kind, t.base_id, toff, 0, 0, cond, f.kind, f.base_id, foff};
        }
      } else {
        spv.fail("select between incompatible pointers not supported");
        return {};
      }
      if (cacheable) {
        ptr_map[v] = res;
        uint32_t tnull = get_ptr_is_null(sel->getTrueValue());
        uint32_t fnull = get_ptr_is_null(sel->getFalseValue());
        if (tnull != spv.get_const_bool(false) || fnull != spv.get_const_bool(false)) {
          uint32_t nid = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpSelect, {spv.get_bool(), nid, cond, tnull, fnull});
          ptr_null_map[v] = nid;
        }
      }
      return res;
    }
    spv.fail("unresolvable pointer expression in AIR");
    return {};
  }

  uint32_t get_ptr_is_null(llvm::Value *v, int depth = 0) {
    if (!v || depth > kMaxPtrDepth) return spv.get_const_bool(false);
    if (llvm::isa<llvm::ConstantPointerNull>(v)) return spv.get_const_bool(true);
    if (llvm::isa<llvm::UndefValue>(v) || llvm::isa<llvm::PoisonValue>(v)) {
      return spv.get_undef(spv.get_bool());
    }
    if (auto it = ptr_null_map.find(v); it != ptr_null_map.end()) return it->second;
    if (auto *bc = llvm::dyn_cast<llvm::BitCastOperator>(v)) {
      return get_ptr_is_null(bc->getOperand(0), depth + 1);
    }
    if (auto *asc = llvm::dyn_cast<llvm::AddrSpaceCastOperator>(v)) {
      return get_ptr_is_null(asc->getOperand(0), depth + 1);
    }
    if (auto *gep = llvm::dyn_cast<llvm::GEPOperator>(v)) {
      return get_ptr_is_null(gep->getPointerOperand(), depth + 1);
    }
    if (auto *fi = llvm::dyn_cast<llvm::FreezeInst>(v)) {
      return get_ptr_is_null(fi->getOperand(0), depth + 1);
    }
    return spv.get_const_bool(false);
  }

  uint32_t emit_pc_load(uint32_t field, uint32_t slot) {
    uint32_t pc_var = spv.get_ptr_pc_var();
    uint32_t ptr_id = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpAccessChain,
                {spv.pc_ptr_u32_ty, ptr_id, pc_var, spv.get_const_u32(field),
                 spv.get_const_u32(slot)});
    uint32_t val_id = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpLoad, {spv.get_int(32), val_id, ptr_id});
    return val_id;
  }

  bool get_backing_info(const PtrInfo &ptr, uint32_t &var_id, uint32_t &ptr_elem_ty, ElemKind &elem,
                        bool &is_ssbo) {
    if (ptr.kind == PtrKind::Buffer) {
      if (ptr.base_id >= kMaxBufferSlots || !buffers[ptr.base_id].used ||
          !buffers[ptr.base_id].var_id) {
        return spv.fail("invalid buffer binding reference");
      }
      const BufferBinding &b = buffers[ptr.base_id];
      var_id = b.var_id;
      ptr_elem_ty = b.ptr_elem_ty;
      elem = b.elem;
      is_ssbo = true;
      return true;
    }
    if (ptr.kind == PtrKind::Workgroup) {
      if (ptr.base_id >= wg_arrays.size() || !wg_arrays[ptr.base_id].var_id) {
        return spv.fail("invalid workgroup memory reference");
      }
      const MemArrayInfo &w = wg_arrays[ptr.base_id];
      var_id = w.var_id;
      ptr_elem_ty = w.ptr_elem_ty;
      elem = w.elem;
      is_ssbo = false;
      return true;
    }
    if (ptr.kind == PtrKind::Function) {
      if (ptr.base_id >= fn_arrays.size() || !fn_arrays[ptr.base_id].var_id) {
        return spv.fail("invalid function alloca reference");
      }
      const MemArrayInfo &f = fn_arrays[ptr.base_id];
      var_id = f.var_id;
      ptr_elem_ty = f.ptr_elem_ty;
      elem = f.elem;
      is_ssbo = false;
      return true;
    }
    return spv.fail("invalid memory backing kind");
  }

  uint32_t emit_elem_ptr(const PtrInfo &ptr, uint32_t elem_idx_id) {
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(ptr, var_id, ptr_elem_ty, elem, is_ssbo)) return 0;
    uint32_t ep = spv.alloc_id();
    if (is_ssbo) {
      spv.emit_op(cur_body, spv::OpAccessChain,
                  {ptr_elem_ty, ep, var_id, spv.get_const_u32(0), elem_idx_id});
    } else {
      spv.emit_op(cur_body, spv::OpAccessChain, {ptr_elem_ty, ep, var_id, elem_idx_id});
    }
    return ep;
  }

  uint32_t emit_raw_elem_load(const PtrInfo &ptr, uint32_t elem_idx_id) {
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(ptr, var_id, ptr_elem_ty, elem, is_ssbo)) return 0;
    uint32_t ety = spv.get_elem_spv_type(elem);
    if (!ety) return 0;
    uint32_t ep = emit_elem_ptr(ptr, elem_idx_id);
    if (!ep) return 0;
    uint32_t val = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpLoad, {ety, val, ep});
    return val;
  }

  bool emit_raw_elem_store(const PtrInfo &ptr, uint32_t elem_idx_id, uint32_t val_id) {
    uint32_t ep = emit_elem_ptr(ptr, elem_idx_id);
    if (!ep) return false;
    spv.emit_op(cur_body, spv::OpStore, {ep, val_id});
    return spv.error.empty();
  }

  uint32_t emit_load_scalar_at(const PtrInfo &base, uint32_t byte_off, llvm::Type *ty) {
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(base, var_id, ptr_elem_ty, elem, is_ssbo)) return 0;

    uint32_t u32 = spv.get_int(32);
    uint32_t f32 = spv.get_float(32);

    if (ty->isHalfTy()) {
      uint32_t f16 = spv.get_float(16);
      if (!f16) return 0;
      if (elem == ElemKind::F16) {
        return emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 1));
      }
      if (elem == ElemKind::U16) {
        uint32_t raw = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 1));
        uint32_t r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {f16, r, raw});
        return r;
      }
      if (elem == ElemKind::U32 || elem == ElemKind::F32) {
        uint32_t w = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 2));
        if (elem == ElemKind::F32) {
          uint32_t wu = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u32, wu, w});
          w = wu;
        }
        uint32_t bit_off = spv.alloc_id();
        uint32_t byte_mod = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, byte_mod, byte_off, spv.get_const_u32(2)});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32, bit_off, byte_mod, spv.get_const_u32(3)});
        uint32_t bits16 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitFieldUExtract, {u32, bits16, w, bit_off, spv.get_const_u32(16)});
        uint32_t v2f = spv.get_vec(f32, 2);
        uint32_t unp = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpExtInst,
                    {v2f, unp, spv.glsl_ext_id, GLSLstd450UnpackHalf2x16, bits16});
        uint32_t flt = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpCompositeExtract, {f32, flt, unp, 0u});
        uint32_t h = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFConvert, {f16, h, flt});
        return h;
      }
      if (elem == ElemKind::U8) {
        uint32_t u16 = spv.get_int(16);
        if (!u16) return 0;
        uint32_t b0 = emit_raw_elem_load(base, byte_off);
        uint32_t b1 = emit_raw_elem_load(base, emit_u32_add(byte_off, spv.get_const_u32(1)));
        uint32_t lo = spv.alloc_id(), hi = spv.alloc_id(), hish = spv.alloc_id();
        uint32_t u16v = spv.alloc_id(), h = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u16, lo, b0});
        spv.emit_op(cur_body, spv::OpUConvert, {u16, hi, b1});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u16, hish, hi, spv.get_const_int(16, 8)});
        spv.emit_op(cur_body, spv::OpBitwiseOr, {u16, u16v, lo, hish});
        spv.emit_op(cur_body, spv::OpBitcast, {f16, h, u16v});
        return h;
      }
    }

    if (ty->isFloatTy() || ty->isIntegerTy(32)) {
      uint32_t dst_ty = ty->isFloatTy() ? f32 : u32;
      if ((ty->isFloatTy() && elem == ElemKind::F32) ||
          (ty->isIntegerTy(32) && elem == ElemKind::U32)) {
        return emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 2));
      }
      if (elem == ElemKind::U32 || elem == ElemKind::F32) {
        uint32_t raw = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 2));
        uint32_t r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {dst_ty, r, raw});
        return r;
      }
      if (elem == ElemKind::U16 || elem == ElemKind::F16) {
        uint32_t idx0 = emit_u32_shr_const(byte_off, 1);
        uint32_t idx1 = emit_u32_add(idx0, spv.get_const_u32(1));
        uint32_t lo = emit_raw_elem_load(base, idx0);
        uint32_t hi = emit_raw_elem_load(base, idx1);
        if (elem == ElemKind::F16) {
          uint32_t u16 = spv.get_int(16);
          if (!u16) return 0;
          uint32_t lo_u = spv.alloc_id(), hi_u = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u16, lo_u, lo});
          spv.emit_op(cur_body, spv::OpBitcast, {u16, hi_u, hi});
          lo = lo_u;
          hi = hi_u;
        }
        uint32_t lo32 = spv.alloc_id(), hi32 = spv.alloc_id(), hish = spv.alloc_id(), comb = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u32, lo32, lo});
        spv.emit_op(cur_body, spv::OpUConvert, {u32, hi32, hi});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32, hish, hi32, spv.get_const_u32(16)});
        spv.emit_op(cur_body, spv::OpBitwiseOr, {u32, comb, lo32, hish});
        if (ty->isFloatTy()) {
          uint32_t rf = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {f32, rf, comb});
          return rf;
        }
        return comb;
      }
      if (elem == ElemKind::U8) {
        uint32_t comb = spv.get_const_u32(0);
        for (uint32_t b = 0; b < 4; b++) {
          uint32_t bv = emit_raw_elem_load(base, emit_u32_add(byte_off, spv.get_const_u32(b)));
          uint32_t b32 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpUConvert, {u32, b32, bv});
          if (b > 0) {
            uint32_t sh = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32, sh, b32, spv.get_const_u32(b * 8u)});
            b32 = sh;
          }
          uint32_t next = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseOr, {u32, next, comb, b32});
          comb = next;
        }
        if (ty->isFloatTy()) {
          uint32_t rf = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {f32, rf, comb});
          return rf;
        }
        return comb;
      }
    }

    if (ty->isIntegerTy(16) || ty->isBFloatTy()) {
      uint32_t u16 = spv.get_int(16);
      if (!u16) return 0;
      if (elem == ElemKind::U16) return emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 1));
      if (elem == ElemKind::F16) {
        uint32_t raw = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 1));
        uint32_t r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {u16, r, raw});
        return r;
      }
      if (elem == ElemKind::U32 || elem == ElemKind::F32) {
        uint32_t w = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 2));
        if (elem == ElemKind::F32) {
          uint32_t wu = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u32, wu, w});
          w = wu;
        }
        uint32_t bm = spv.alloc_id(), bo = spv.alloc_id(), ex = spv.alloc_id(), r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, bm, byte_off, spv.get_const_u32(2)});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32, bo, bm, spv.get_const_u32(3)});
        spv.emit_op(cur_body, spv::OpBitFieldUExtract, {u32, ex, w, bo, spv.get_const_u32(16)});
        spv.emit_op(cur_body, spv::OpUConvert, {u16, r, ex});
        return r;
      }
      if (elem == ElemKind::U8) {
        uint32_t b0 = emit_raw_elem_load(base, byte_off);
        uint32_t b1 = emit_raw_elem_load(base, emit_u32_add(byte_off, spv.get_const_u32(1)));
        uint32_t lo = spv.alloc_id(), hi = spv.alloc_id(), hish = spv.alloc_id(), r = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u16, lo, b0});
        spv.emit_op(cur_body, spv::OpUConvert, {u16, hi, b1});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u16, hish, hi, spv.get_const_int(16, 8)});
        spv.emit_op(cur_body, spv::OpBitwiseOr, {u16, r, lo, hish});
        return r;
      }
    }

    if (ty->isIntegerTy(8) || ty->isIntegerTy(1)) {
      uint32_t b32 = 0;
      if (elem == ElemKind::U8) {
        uint32_t raw = emit_raw_elem_load(base, byte_off);
        if (ty->isIntegerTy(8)) return raw;
        b32 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u32, b32, raw});
      } else if (elem == ElemKind::U32) {
        uint32_t w = emit_raw_elem_load(base, emit_u32_shr_const(byte_off, 2));
        uint32_t bm = spv.alloc_id(), bo = spv.alloc_id();
        b32 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, bm, byte_off, spv.get_const_u32(3)});
        spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32, bo, bm, spv.get_const_u32(3)});
        spv.emit_op(cur_body, spv::OpBitFieldUExtract, {u32, b32, w, bo, spv.get_const_u32(8)});
      } else {
        spv.fail("unsupported 8-bit load backing combination");
        return 0;
      }
      if (ty->isIntegerTy(1)) {
        uint32_t res = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpINotEqual, {spv.get_bool(), res, b32, spv.get_const_u32(0)});
        return res;
      }
      uint32_t u8 = spv.get_int(8);
      if (!u8) return 0;
      uint32_t res = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpUConvert, {u8, res, b32});
      return res;
    }

    if (ty->isIntegerTy(64)) {
      uint32_t u64 = spv.get_int(64);
      if (!u64) return 0;
      uint32_t lo = emit_load_scalar_at(base, byte_off, llvm::Type::getInt32Ty(mod.getContext()));
      uint32_t hi = emit_load_scalar_at(base, emit_u32_add(byte_off, spv.get_const_u32(4)),
                                        llvm::Type::getInt32Ty(mod.getContext()));
      if (!lo || !hi) return 0;
      uint32_t lo64 = spv.alloc_id(), hi64 = spv.alloc_id(), hish = spv.alloc_id(), r = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpUConvert, {u64, lo64, lo});
      spv.emit_op(cur_body, spv::OpUConvert, {u64, hi64, hi});
      spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u64, hish, hi64, spv.get_const_int(64, 32)});
      spv.emit_op(cur_body, spv::OpBitwiseOr, {u64, r, lo64, hish});
      return r;
    }

    spv.fail("unsupported scalar load type/backing combination");
    return 0;
  }

  uint32_t emit_mem_load_ptr(const PtrInfo &base, llvm::Type *ty) {
    if (base.is_dual()) {
      if (cur_bb && merges.count(cur_bb) && merges[cur_bb].is_loop) {
        spv.fail("two-buffer pointer select load inside loop header not supported");
        return 0;
      }
      uint32_t res_ty = spv.map_llvm_type(ty);
      if (!res_ty) return 0;
      uint32_t then_lbl = spv.alloc_id();
      uint32_t else_lbl = spv.alloc_id();
      uint32_t merge_lbl = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelectionMerge, {merge_lbl, 0u});
      spv.emit_op(cur_body, spv::OpBranchConditional, {base.sel_cond_id, then_lbl, else_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {then_lbl});
      cur_exit_label = then_lbl;
      uint32_t v_true = emit_mem_load_ptr(base.primary_arm(), ty);
      if (!v_true) return 0;
      uint32_t then_exit = cur_exit_label;
      spv.emit_op(cur_body, spv::OpBranch, {merge_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {else_lbl});
      cur_exit_label = else_lbl;
      uint32_t v_false = emit_mem_load_ptr(base.alt_arm(), ty);
      if (!v_false) return 0;
      uint32_t else_exit = cur_exit_label;
      spv.emit_op(cur_body, spv::OpBranch, {merge_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {merge_lbl});
      cur_exit_label = merge_lbl;
      uint32_t res = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpPhi, {res_ty, res, v_true, then_exit, v_false, else_exit});
      return spv.error.empty() ? res : 0u;
    }
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(base, var_id, ptr_elem_ty, elem, is_ssbo)) return 0;

    /* Fast direct element-indexed path: zero shift-left + shift-right overhead! */
    if ((ty->isFloatTy() && elem == ElemKind::F32) ||
        (ty->isIntegerTy(32) && elem == ElemKind::U32)) {
      return emit_raw_elem_load(base, materialize_elem_idx(base, 2));
    }
    if ((ty->isHalfTy() && elem == ElemKind::F16) ||
        ((ty->isIntegerTy(16) || ty->isBFloatTy()) && elem == ElemKind::U16)) {
      return emit_raw_elem_load(base, materialize_elem_idx(base, 1));
    }
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ty)) {
      uint32_t vty = spv.map_llvm_type(ty);
      if (!vty) return 0;
      llvm::Type *ety = vt->getElementType();
      unsigned n = vt->getNumElements();
      bool match_f32 = ety->isFloatTy() && elem == ElemKind::F32;
      bool match_u32 = ety->isIntegerTy(32) && elem == ElemKind::U32;
      bool match_f16 = ety->isHalfTy() && elem == ElemKind::F16;
      bool match_u16 = (ety->isIntegerTy(16) || ety->isBFloatTy()) && elem == ElemKind::U16;
      if (match_f32 || match_u32 || match_f16 || match_u16) {
        uint32_t shift = (match_f32 || match_u32) ? 2u : 1u;
        uint32_t idx0 = materialize_elem_idx(base, shift);
        std::vector<uint32_t> elems;
        elems.reserve(n);
        for (unsigned i = 0; i < n; i++) {
          uint32_t idx_i = (i == 0) ? idx0 : emit_u32_add(idx0, spv.get_const_u32(i));
          uint32_t ev = emit_raw_elem_load(base, idx_i);
          if (!ev) return 0;
          elems.push_back(ev);
        }
        uint32_t res = spv.alloc_id();
        std::vector<uint32_t> args = {vty, res};
        args.insert(args.end(), elems.begin(), elems.end());
        spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
        return spv.error.empty() ? res : 0u;
      }
    }
    int budget = kMaxScalarOpsPerMem;
    return emit_mem_load(base, materialize_byte_off(base), ty, budget, 0);
  }

  bool emit_mem_store_ptr(const PtrInfo &base, llvm::Type *ty, uint32_t val_id) {
    if (base.is_dual()) {
      if (cur_bb && merges.count(cur_bb) && merges[cur_bb].is_loop) {
        return spv.fail("two-buffer pointer select store inside loop header not supported");
      }
      uint32_t then_lbl = spv.alloc_id();
      uint32_t else_lbl = spv.alloc_id();
      uint32_t merge_lbl = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelectionMerge, {merge_lbl, 0u});
      spv.emit_op(cur_body, spv::OpBranchConditional, {base.sel_cond_id, then_lbl, else_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {then_lbl});
      cur_exit_label = then_lbl;
      if (!emit_mem_store_ptr(base.primary_arm(), ty, val_id)) return false;
      spv.emit_op(cur_body, spv::OpBranch, {merge_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {else_lbl});
      cur_exit_label = else_lbl;
      if (!emit_mem_store_ptr(base.alt_arm(), ty, val_id)) return false;
      spv.emit_op(cur_body, spv::OpBranch, {merge_lbl});
      spv.emit_op(cur_body, spv::OpLabel, {merge_lbl});
      cur_exit_label = merge_lbl;
      return spv.error.empty();
    }
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(base, var_id, ptr_elem_ty, elem, is_ssbo)) return false;

    if ((ty->isFloatTy() && elem == ElemKind::F32) ||
        (ty->isIntegerTy(32) && elem == ElemKind::U32)) {
      return emit_raw_elem_store(base, materialize_elem_idx(base, 2), val_id);
    }
    if ((ty->isHalfTy() && elem == ElemKind::F16) ||
        ((ty->isIntegerTy(16) || ty->isBFloatTy()) && elem == ElemKind::U16)) {
      return emit_raw_elem_store(base, materialize_elem_idx(base, 1), val_id);
    }
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ty)) {
      if (!spv.map_llvm_type(ty)) return false;
      llvm::Type *ety = vt->getElementType();
      unsigned n = vt->getNumElements();
      bool match_f32 = ety->isFloatTy() && elem == ElemKind::F32;
      bool match_u32 = ety->isIntegerTy(32) && elem == ElemKind::U32;
      bool match_f16 = ety->isHalfTy() && elem == ElemKind::F16;
      bool match_u16 = (ety->isIntegerTy(16) || ety->isBFloatTy()) && elem == ElemKind::U16;
      if (match_f32 || match_u32 || match_f16 || match_u16) {
        uint32_t shift = (match_f32 || match_u32) ? 2u : 1u;
        uint32_t spv_ety = spv.map_llvm_type(ety);
        if (!spv_ety) return false;
        uint32_t idx0 = materialize_elem_idx(base, shift);
        for (unsigned i = 0; i < n; i++) {
          uint32_t e = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpCompositeExtract, {spv_ety, e, val_id, i});
          uint32_t idx_i = (i == 0) ? idx0 : emit_u32_add(idx0, spv.get_const_u32(i));
          if (!emit_raw_elem_store(base, idx_i, e)) return false;
        }
        return true;
      }
    }
    int budget = kMaxScalarOpsPerMem;
    return emit_mem_store(base, materialize_byte_off(base), ty, val_id, budget, 0);
  }

  uint32_t emit_mem_load(const PtrInfo &base, uint32_t byte_off, llvm::Type *ty,
                         int &scalar_budget, int depth) {
    if (!spv.error.empty()) return 0;
    if (!ty || depth > 16) {
      spv.fail("aggregate load nesting too deep");
      return 0;
    }
    if (--scalar_budget < 0) {
      spv.fail("aggregate load exceeds scalar op budget");
      return 0;
    }
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ty)) {
      uint32_t vty = spv.map_llvm_type(ty);
      if (!vty) return 0;
      unsigned n = vt->getNumElements();
      llvm::Type *ety = vt->getElementType();
      uint64_t esz64 = 0;
      if (!try_type_alloc_bytes(dl, ety, esz64) || esz64 > UINT32_MAX) {
        spv.fail("invalid vector element size in load");
        return 0;
      }
      uint32_t esz = static_cast<uint32_t>(esz64);
      std::vector<uint32_t> elems;
      elems.reserve(n);
      for (unsigned i = 0; i < n; i++) {
        if (--scalar_budget < 0) {
          spv.fail("aggregate load exceeds scalar op budget");
          return 0;
        }
        uint32_t off_i = i == 0 ? byte_off : emit_u32_add(byte_off, spv.get_const_u32(i * esz));
        uint32_t e = emit_load_scalar_at(base, off_i, ety);
        if (!e) return 0;
        elems.push_back(e);
      }
      uint32_t res = spv.alloc_id();
      std::vector<uint32_t> args = {vty, res};
      args.insert(args.end(), elems.begin(), elems.end());
      spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
      return spv.error.empty() ? res : 0u;
    }
    if (auto *st = llvm::dyn_cast<llvm::StructType>(ty)) {
      if (st->isOpaque() || st->getNumElements() > kMaxStructMembers) {
        spv.fail("unsupported struct type in load");
        return 0;
      }
      const llvm::StructLayout *sl = dl.getStructLayout(st);
      std::vector<uint32_t> fields;
      fields.reserve(st->getNumElements());
      for (unsigned i = 0; i < st->getNumElements(); i++) {
        uint64_t off64 = sl->getElementOffset(i);
        if (off64 > UINT32_MAX) {
          spv.fail("struct field offset exceeds 32-bit range");
          return 0;
        }
        uint32_t foff = emit_u32_add(byte_off, spv.get_const_u32(static_cast<uint32_t>(off64)));
        uint32_t fv = emit_mem_load(base, foff, st->getElementType(i), scalar_budget, depth + 1);
        if (!fv) return 0;
        fields.push_back(fv);
      }
      uint32_t sty = spv.map_llvm_type(ty);
      if (!sty) return 0;
      uint32_t res = spv.alloc_id();
      std::vector<uint32_t> args = {sty, res};
      args.insert(args.end(), fields.begin(), fields.end());
      spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
      return spv.error.empty() ? res : 0u;
    }
    if (auto *at = llvm::dyn_cast<llvm::ArrayType>(ty)) {
      uint64_t n = at->getNumElements();
      llvm::Type *ety = at->getElementType();
      uint64_t esz64 = 0;
      if (scalar_budget <= 0 || n == 0 || n > static_cast<uint64_t>(scalar_budget) ||
          !try_type_alloc_bytes(dl, ety, esz64) || esz64 > UINT32_MAX) {
        spv.fail("array load size invalid or exceeds scalar op budget");
        return 0;
      }
      std::vector<uint32_t> elems;
      elems.reserve(static_cast<size_t>(n));
      for (uint64_t i = 0; i < n; i++) {
        uint64_t off64 = i * esz64;
        if (off64 > UINT32_MAX) {
          spv.fail("array load offset exceeds 32-bit range");
          return 0;
        }
        uint32_t eoff = emit_u32_add(byte_off, spv.get_const_u32(static_cast<uint32_t>(off64)));
        uint32_t ev = emit_mem_load(base, eoff, ety, scalar_budget, depth + 1);
        if (!ev) return 0;
        elems.push_back(ev);
      }
      uint32_t aty = spv.map_llvm_type(ty);
      if (!aty) return 0;
      uint32_t res = spv.alloc_id();
      std::vector<uint32_t> args = {aty, res};
      args.insert(args.end(), elems.begin(), elems.end());
      spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
      return spv.error.empty() ? res : 0u;
    }
    return emit_load_scalar_at(base, byte_off, ty);
  }

  bool emit_store_scalar_at(const PtrInfo &base, uint32_t byte_off, llvm::Type *ty, uint32_t val_id) {
    uint32_t var_id = 0, ptr_elem_ty = 0;
    ElemKind elem = ElemKind::U32;
    bool is_ssbo = false;
    if (!get_backing_info(base, var_id, ptr_elem_ty, elem, is_ssbo)) return false;

    uint32_t u32 = spv.get_int(32);
    uint32_t f32 = spv.get_float(32);

    if (ty->isHalfTy()) {
      if (elem == ElemKind::F16) {
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 1), val_id);
        return true;
      }
      if (elem == ElemKind::U16) {
        uint32_t u16 = spv.get_int(16);
        if (!u16) return false;
        uint32_t bits = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {u16, bits, val_id});
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 1), bits);
        return true;
      }
      if (elem == ElemKind::U8) {
        uint32_t u16 = spv.get_int(16);
        uint32_t u8 = spv.get_int(8);
        if (!u16 || !u8) return false;
        uint32_t bits = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {u16, bits, val_id});
        uint32_t b0 = spv.alloc_id(), sh = spv.alloc_id(), b1 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u8, b0, bits});
        spv.emit_op(cur_body, spv::OpShiftRightLogical, {u16, sh, bits, spv.get_const_int(16, 8)});
        spv.emit_op(cur_body, spv::OpUConvert, {u8, b1, sh});
        emit_raw_elem_store(base, byte_off, b0);
        emit_raw_elem_store(base, emit_u32_add(byte_off, spv.get_const_u32(1)), b1);
        return true;
      }
      return spv.fail("half store requires 16-bit or 8-bit storage backing");
    }

    if (ty->isFloatTy() || ty->isIntegerTy(32)) {
      if ((ty->isFloatTy() && elem == ElemKind::F32) ||
          (ty->isIntegerTy(32) && elem == ElemKind::U32)) {
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 2), val_id);
        return true;
      }
      if (elem == ElemKind::U32 || elem == ElemKind::F32) {
        uint32_t cast_ty = (elem == ElemKind::F32) ? f32 : u32;
        uint32_t bc = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {cast_ty, bc, val_id});
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 2), bc);
        return true;
      }
      if (elem == ElemKind::U16 || elem == ElemKind::F16) {
        uint32_t u16 = spv.get_int(16);
        if (!u16) return false;
        uint32_t w = val_id;
        if (ty->isFloatTy()) {
          w = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u32, w, val_id});
        }
        uint32_t lo = spv.alloc_id(), hi_sh = spv.alloc_id(), hi = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u16, lo, w});
        spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, hi_sh, w, spv.get_const_u32(16)});
        spv.emit_op(cur_body, spv::OpUConvert, {u16, hi, hi_sh});
        if (elem == ElemKind::F16) {
          uint32_t f16 = spv.get_float(16);
          if (!f16) return false;
          uint32_t lof = spv.alloc_id(), hif = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {f16, lof, lo});
          spv.emit_op(cur_body, spv::OpBitcast, {f16, hif, hi});
          lo = lof;
          hi = hif;
        }
        uint32_t idx0 = emit_u32_shr_const(byte_off, 1);
        emit_raw_elem_store(base, idx0, lo);
        emit_raw_elem_store(base, emit_u32_add(idx0, spv.get_const_u32(1)), hi);
        return true;
      }
      if (elem == ElemKind::U8) {
        uint32_t u8 = spv.get_int(8);
        if (!u8) return false;
        uint32_t w = val_id;
        if (ty->isFloatTy()) {
          w = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u32, w, val_id});
        }
        for (uint32_t b = 0; b < 4; b++) {
          uint32_t src = w;
          if (b > 0) {
            src = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, src, w, spv.get_const_u32(b * 8u)});
          }
          uint32_t bv = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpUConvert, {u8, bv, src});
          emit_raw_elem_store(base, emit_u32_add(byte_off, spv.get_const_u32(b)), bv);
        }
        return true;
      }
    }

    if (ty->isIntegerTy(16) || ty->isBFloatTy()) {
      if (elem == ElemKind::U16) {
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 1), val_id);
        return true;
      }
      if (elem == ElemKind::F16) {
        uint32_t f16 = spv.get_float(16);
        if (!f16) return false;
        uint32_t bc = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitcast, {f16, bc, val_id});
        emit_raw_elem_store(base, emit_u32_shr_const(byte_off, 1), bc);
        return true;
      }
      if (elem == ElemKind::U8) {
        uint32_t u8 = spv.get_int(8);
        uint32_t u16 = spv.get_int(16);
        if (!u8 || !u16) return false;
        uint32_t lo = spv.alloc_id(), sh = spv.alloc_id(), hi = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u8, lo, val_id});
        spv.emit_op(cur_body, spv::OpShiftRightLogical, {u16, sh, val_id, spv.get_const_int(16, 8)});
        spv.emit_op(cur_body, spv::OpUConvert, {u8, hi, sh});
        emit_raw_elem_store(base, byte_off, lo);
        emit_raw_elem_store(base, emit_u32_add(byte_off, spv.get_const_u32(1)), hi);
        return true;
      }
      return spv.fail("16-bit store requires 16-bit or 8-bit storage backing");
    }

    if (ty->isIntegerTy(8) || ty->isIntegerTy(1)) {
      if (elem != ElemKind::U8) return spv.fail("8-bit store requires 8-bit storage backing");
      uint32_t u8 = spv.get_int(8);
      if (!u8) return false;
      uint32_t bv = val_id;
      if (ty->isIntegerTy(1)) {
        bv = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect,
                    {u8, bv, val_id, spv.get_const_int(8, 1), spv.get_const_int(8, 0)});
      }
      emit_raw_elem_store(base, byte_off, bv);
      return true;
    }

    if (ty->isIntegerTy(64)) {
      uint32_t u64 = spv.get_int(64);
      if (!u64) return false;
      uint32_t lo = spv.alloc_id(), hi_sh = spv.alloc_id(), hi = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpUConvert, {u32, lo, val_id});
      spv.emit_op(cur_body, spv::OpShiftRightLogical, {u64, hi_sh, val_id, spv.get_const_int(64, 32)});
      spv.emit_op(cur_body, spv::OpUConvert, {u32, hi, hi_sh});
      llvm::Type *i32_ty = llvm::Type::getInt32Ty(mod.getContext());
      return emit_store_scalar_at(base, byte_off, i32_ty, lo) &&
             emit_store_scalar_at(base, emit_u32_add(byte_off, spv.get_const_u32(4)), i32_ty, hi);
    }

    return spv.fail("unsupported scalar store type/backing combination");
  }

  bool emit_mem_store(const PtrInfo &base, uint32_t byte_off, llvm::Type *ty, uint32_t val_id,
                      int &scalar_budget, int depth) {
    if (!spv.error.empty()) return false;
    if (!ty || depth > 16) return spv.fail("aggregate store nesting too deep");
    if (--scalar_budget < 0) return spv.fail("aggregate store exceeds scalar op budget");
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ty)) {
      if (!spv.map_llvm_type(ty)) return false;
      unsigned n = vt->getNumElements();
      llvm::Type *ety = vt->getElementType();
      uint64_t esz64 = 0;
      if (!try_type_alloc_bytes(dl, ety, esz64) || esz64 > UINT32_MAX) {
        return spv.fail("invalid vector element size in store");
      }
      uint32_t esz = static_cast<uint32_t>(esz64);
      uint32_t spv_ety = spv.map_llvm_type(ety);
      if (!spv_ety) return false;
      for (unsigned i = 0; i < n; i++) {
        if (--scalar_budget < 0) return spv.fail("aggregate store exceeds scalar op budget");
        uint32_t e = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpCompositeExtract, {spv_ety, e, val_id, i});
        uint32_t off_i = i == 0 ? byte_off : emit_u32_add(byte_off, spv.get_const_u32(i * esz));
        if (!emit_store_scalar_at(base, off_i, ety, e)) return false;
      }
      return spv.error.empty();
    }
    if (auto *st = llvm::dyn_cast<llvm::StructType>(ty)) {
      if (st->isOpaque() || st->getNumElements() > kMaxStructMembers) {
        return spv.fail("unsupported struct type in store");
      }
      const llvm::StructLayout *sl = dl.getStructLayout(st);
      for (unsigned i = 0; i < st->getNumElements(); i++) {
        uint64_t off64 = sl->getElementOffset(i);
        if (off64 > UINT32_MAX) return spv.fail("struct field offset exceeds 32-bit range");
        uint32_t fty = spv.map_llvm_type(st->getElementType(i));
        if (!fty) return false;
        uint32_t fv = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpCompositeExtract, {fty, fv, val_id, i});
        uint32_t foff = emit_u32_add(byte_off, spv.get_const_u32(static_cast<uint32_t>(off64)));
        if (!emit_mem_store(base, foff, st->getElementType(i), fv, scalar_budget, depth + 1)) {
          return false;
        }
      }
      return spv.error.empty();
    }
    if (auto *at = llvm::dyn_cast<llvm::ArrayType>(ty)) {
      uint64_t n = at->getNumElements();
      llvm::Type *ety = at->getElementType();
      uint64_t esz64 = 0;
      if (scalar_budget <= 0 || n == 0 || n > static_cast<uint64_t>(scalar_budget) ||
          !try_type_alloc_bytes(dl, ety, esz64) || esz64 > UINT32_MAX) {
        return spv.fail("array store size invalid or exceeds scalar op budget");
      }
      uint32_t spv_ety = spv.map_llvm_type(ety);
      if (!spv_ety) return false;
      for (uint64_t i = 0; i < n; i++) {
        uint64_t off64 = i * esz64;
        if (off64 > UINT32_MAX) return spv.fail("array store offset exceeds 32-bit range");
        uint32_t ev = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpCompositeExtract, {spv_ety, ev, val_id, static_cast<uint32_t>(i)});
        uint32_t eoff = emit_u32_add(byte_off, spv.get_const_u32(static_cast<uint32_t>(off64)));
        if (!emit_mem_store(base, eoff, ety, ev, scalar_budget, depth + 1)) return false;
      }
      return spv.error.empty();
    }
    return emit_store_scalar_at(base, byte_off, ty, val_id);
  }

  uint32_t splat_const_int(uint32_t bits, uint64_t val, uint32_t lanes) {
    uint32_t c = spv.get_const_int(bits, val);
    if (!c || lanes <= 1) return c;
    uint32_t vty = spv.get_vec(spv.get_int(bits), lanes);
    if (!vty) return 0;
    std::vector<uint32_t> elems(lanes, c);
    auto key = std::make_pair(vty, elems);
    auto it = spv.const_comp.find(key);
    if (it != spv.const_comp.end()) return it->second;
    uint32_t id = spv.alloc_id();
    std::vector<uint32_t> args = {vty, id};
    args.insert(args.end(), elems.begin(), elems.end());
    spv.emit_op_vec(spv.types_consts_vars, spv::OpConstantComposite, args);
    spv.const_comp[key] = id;
    return id;
  }

  uint32_t emit_bf16_to_f32(uint32_t src_id, llvm::Type *bf_ty) {
    uint32_t lanes = 1u;
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(bf_ty)) lanes = vt->getNumElements();
    uint32_t u32 = spv.get_int(32);
    uint32_t f32 = spv.get_float(32);
    uint32_t u32_ty = lanes > 1 ? spv.get_vec(u32, lanes) : u32;
    uint32_t f32_ty = lanes > 1 ? spv.get_vec(f32, lanes) : f32;
    uint32_t sh16 = splat_const_int(32, 16u, lanes);
    uint32_t ext = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpUConvert, {u32_ty, ext, src_id});
    uint32_t shifted = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u32_ty, shifted, ext, sh16});
    uint32_t res = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpBitcast, {f32_ty, res, shifted});
    return spv.error.empty() ? res : 0u;
  }

  uint32_t emit_f32_to_bf16(uint32_t src_id, llvm::Type *f32_llvm_ty) {
    uint32_t lanes = 1u;
    if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(f32_llvm_ty)) lanes = vt->getNumElements();
    uint32_t u16 = spv.get_int(16);
    uint32_t u32 = spv.get_int(32);
    uint32_t bty = spv.get_bool();
    if (!u16 || !u32 || !bty) return 0;
    uint32_t u16_ty = lanes > 1 ? spv.get_vec(u16, lanes) : u16;
    uint32_t u32_ty = lanes > 1 ? spv.get_vec(u32, lanes) : u32;
    uint32_t bool_ty = lanes > 1 ? spv.get_vec(bty, lanes) : bty;

    uint32_t u = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpBitcast, {u32_ty, u, src_id});
    uint32_t abs_u = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32_ty, abs_u, u, splat_const_int(32, 0x7FFFFFFFu, lanes)});
    uint32_t is_nan = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpUGreaterThan, {bool_ty, is_nan, abs_u, splat_const_int(32, 0x7F800000u, lanes)});
    uint32_t hi16 = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32_ty, hi16, u, splat_const_int(32, 16u, lanes)});
    uint32_t lsb = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32_ty, lsb, hi16, splat_const_int(32, 1u, lanes)});
    uint32_t bias = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpIAdd, {u32_ty, bias, splat_const_int(32, 0x7FFFu, lanes), lsb});
    uint32_t sum = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpIAdd, {u32_ty, sum, u, bias});
    uint32_t rounded = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32_ty, rounded, sum, splat_const_int(32, 16u, lanes)});
    uint32_t nan_bf = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpBitwiseOr, {u32_ty, nan_bf, hi16, splat_const_int(32, 0x0040u, lanes)});
    uint32_t sel = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpSelect, {u32_ty, sel, is_nan, nan_bf, rounded});
    uint32_t res = spv.alloc_id();
    spv.emit_op(cur_body, spv::OpUConvert, {u16_ty, res, sel});
    return spv.error.empty() ? res : 0u;
  }

  PtrInfo shift_ptr_bytes(const PtrInfo &p, uint32_t delta_bytes) {
    uint32_t p_off = materialize_byte_off(p.primary_arm());
    if (delta_bytes != 0) p_off = emit_u32_add(p_off, spv.get_const_u32(delta_bytes));
    if (!p.is_dual()) {
      return PtrInfo{p.kind, p.base_id, p_off, 0, 0};
    }
    uint32_t a_off = materialize_byte_off(p.alt_arm());
    if (delta_bytes != 0) a_off = emit_u32_add(a_off, spv.get_const_u32(delta_bytes));
    return PtrInfo{p.kind, p.base_id, p_off, 0, 0, p.sel_cond_id, p.alt_kind, p.alt_base_id, a_off};
  }

  bool is_ptr_readonly(const PtrInfo &p) const {
    if (p.kind == PtrKind::Buffer && (p.base_id >= kMaxBufferSlots || buffers[p.base_id].readonly)) {
      return true;
    }
    if (p.is_dual() && p.alt_kind == PtrKind::Buffer &&
        (p.alt_base_id >= kMaxBufferSlots || buffers[p.alt_base_id].readonly)) {
      return true;
    }
    return false;
  }

  uint32_t get_val(llvm::Value *v) {
    if (auto it = val_map.find(v); it != val_map.end()) return it->second;
    if (auto *c = llvm::dyn_cast<llvm::Constant>(v)) return spv.emit_constant(c);
    spv.fail("reference to undefined SSA value in AIR");
    return 0;
  }

  uint32_t emit_glsl_ext(uint32_t res_ty, uint32_t ext_op, std::initializer_list<uint32_t> ops) {
    uint32_t id = spv.alloc_id();
    std::vector<uint32_t> args = {res_ty, id, spv.glsl_ext_id, ext_op};
    for (uint32_t a : ops) args.push_back(a);
    spv.emit_op_vec(cur_body, spv::OpExtInst, args);
    return id;
  }

  bool emit_call(llvm::CallInst *ci) {
    llvm::Function *callee = ci->getCalledFunction();
    if (!callee) return spv.fail("indirect function call not supported in AIR");
    llvm::StringRef name = callee->getName();

    /* Debug / lifetime / assume / invariant intrinsics -> no-op. */
    if (name.starts_with("llvm.lifetime.") || name.starts_with("llvm.dbg.") ||
        name.starts_with("llvm.assume") || name.starts_with("llvm.invariant.") ||
        name.starts_with("llvm.experimental.noalias")) {
      return true;
    }

    /* Threadgroup / SIMD barriers. */
    if (name == "air.wg.barrier" || name.starts_with("air.wg.barrier.")) {
      uint32_t mem_flags = 2u; /* default mem_threadgroup */
      if (ci->arg_size() >= 1) {
        if (auto *c = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(0))) {
          if (!try_get_u32(c, mem_flags)) return spv.fail("invalid barrier flags");
        }
      }
      uint32_t sem = static_cast<uint32_t>(spv::MemorySemanticsAcquireReleaseMask);
      if (mem_flags & 1u) sem |= static_cast<uint32_t>(spv::MemorySemanticsUniformMemoryMask);
      if (mem_flags & 2u) sem |= static_cast<uint32_t>(spv::MemorySemanticsWorkgroupMemoryMask);
      if (mem_flags == 0u) sem = 0u;
      spv.emit_op(cur_body, spv::OpControlBarrier,
                  {spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(sem)});
      return spv.error.empty();
    }
    if (name == "air.simdgroup.barrier" || name.starts_with("air.simdgroup.barrier.")) {
      if ((spv.hw_caps.subgroup_ops & AIR2SPV_SUBGROUP_BASIC) == 0) {
        return spv.fail("simdgroup barrier requires subgroup basic capability unsupported by GPU");
      }
      uint32_t scope = (spv.hw_caps.subgroup_size == 16u)
                           ? static_cast<uint32_t>(spv::ScopeWorkgroup)
                           : static_cast<uint32_t>(spv::ScopeSubgroup);
      uint32_t sem = static_cast<uint32_t>(spv::MemorySemanticsAcquireReleaseMask) |
                     static_cast<uint32_t>(spv::MemorySemanticsWorkgroupMemoryMask) |
                     static_cast<uint32_t>(spv::MemorySemanticsUniformMemoryMask);
      spv.emit_op(cur_body, spv::OpControlBarrier,
                  {spv.get_const_u32(scope), spv.get_const_u32(scope), spv.get_const_u32(sem)});
      return spv.error.empty();
    }

    if (name.starts_with("llvm.memset.")) {
      if (ci->arg_size() < 3) return spv.fail("malformed llvm.memset");
      PtrInfo dst = resolve_ptr(ci->getArgOperand(0));
      if (dst.kind == PtrKind::Invalid || dst.kind == PtrKind::FuncConst) {
        return spv.fail("invalid llvm.memset destination");
      }
      if (is_ptr_readonly(dst)) {
        return spv.fail("llvm.memset to readonly buffer");
      }
      auto *len_c = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(2));
      uint64_t nbytes = 0;
      if (!len_c || !try_get_u64(len_c, nbytes) || nbytes > 1024u) {
        return spv.fail("llvm.memset requires constant size <= 1024");
      }
      llvm::Type *i32_ty = llvm::Type::getInt32Ty(mod.getContext());
      llvm::Type *i8_ty = llvm::Type::getInt8Ty(mod.getContext());
      uint64_t off = 0;
      if (auto *vc = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(1))) {
        uint32_t b = static_cast<uint32_t>(vc->getZExtValue() & 0xFFu);
        uint32_t w32 = b * 0x01010101u;
        uint32_t c32 = spv.get_const_u32(w32);
        while (off + 4u <= nbytes) {
          if (!emit_mem_store_ptr(shift_ptr_bytes(dst, static_cast<uint32_t>(off)), i32_ty, c32)) {
            return false;
          }
          off += 4u;
        }
      }
      if (off < nbytes) {
        uint32_t vb = get_val(ci->getArgOperand(1));
        if (!vb) return false;
        while (off < nbytes) {
          if (!emit_mem_store_ptr(shift_ptr_bytes(dst, static_cast<uint32_t>(off)), i8_ty, vb)) {
            return false;
          }
          off += 1u;
        }
      }
      return spv.error.empty();
    }
    if (name.starts_with("llvm.memcpy.") || name.starts_with("llvm.memmove.")) {
      if (ci->arg_size() < 3) return spv.fail("malformed llvm.memcpy");
      PtrInfo dst = resolve_ptr(ci->getArgOperand(0));
      PtrInfo src = resolve_ptr(ci->getArgOperand(1));
      if (dst.kind == PtrKind::Invalid || dst.kind == PtrKind::FuncConst ||
          src.kind == PtrKind::Invalid || src.kind == PtrKind::FuncConst) {
        return spv.fail("invalid llvm.memcpy pointer");
      }
      if (is_ptr_readonly(dst)) {
        return spv.fail("llvm.memcpy to readonly buffer");
      }
      auto *len_c = llvm::dyn_cast<llvm::ConstantInt>(ci->getArgOperand(2));
      uint64_t nbytes = 0;
      if (!len_c || !try_get_u64(len_c, nbytes) || nbytes > 1024u) {
        return spv.fail("llvm.memcpy requires constant size <= 1024");
      }
      llvm::Type *i32_ty = llvm::Type::getInt32Ty(mod.getContext());
      llvm::Type *i8_ty = llvm::Type::getInt8Ty(mod.getContext());
      uint64_t off = 0;
      while (off + 4u <= nbytes) {
        uint32_t v = emit_mem_load_ptr(shift_ptr_bytes(src, static_cast<uint32_t>(off)), i32_ty);
        if (!v) return false;
        if (!emit_mem_store_ptr(shift_ptr_bytes(dst, static_cast<uint32_t>(off)), i32_ty, v)) {
          return false;
        }
        off += 4u;
      }
      while (off < nbytes) {
        uint32_t v = emit_mem_load_ptr(shift_ptr_bytes(src, static_cast<uint32_t>(off)), i8_ty);
        if (!v) return false;
        if (!emit_mem_store_ptr(shift_ptr_bytes(dst, static_cast<uint32_t>(off)), i8_ty, v)) {
          return false;
        }
        off += 1u;
      }
      return spv.error.empty();
    }

    llvm::Type *rty = ci->getType();
    if (rty->isVoidTy()) {
      return spv.fail("unsupported void AIR/LLVM intrinsic call: " + name.str());
    }
    uint32_t spv_rty = spv.map_llvm_type(rty);
    if (!spv_rty) return false;

    auto is_nonbool_int = [](llvm::Type *t) -> bool {
      return t->isIntOrIntVectorTy() && !t->getScalarType()->isIntegerTy(1);
    };
    auto vec_lanes = [](llvm::Type *t) -> uint32_t {
      if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(t)) return vt->getNumElements();
      return 1u;
    };

    /* Helper to strip air.fast_ / air. / llvm. prefix cleanly. */
    auto match_math = [&](llvm::StringRef base) -> bool {
      return name == base || name.starts_with((base + ".").str());
    };
    llvm::StringRef stem = name;
    if (stem.starts_with("air.fast_")) stem = stem.drop_front(9);
    else if (stem.starts_with("air.")) stem = stem.drop_front(4);
    else if (stem.starts_with("llvm.")) stem = stem.drop_front(5);

    auto make_f32_llvm_ty = [&](llvm::Type *t) -> llvm::Type * {
      uint32_t lanes = vec_lanes(t);
      llvm::Type *f32_t = llvm::Type::getFloatTy(mod.getContext());
      return lanes > 1 ? static_cast<llvm::Type *>(llvm::FixedVectorType::get(f32_t, lanes)) : f32_t;
    };

    /* GLSL.std.450 math ops with strict operand/result type checking (software BF16 via f32). */
    auto unary_glsl = [&](uint32_t glsl_op, bool expect_fp) -> bool {
      if (ci->arg_size() < 1) return spv.fail("missing math arg");
      if (expect_fp ? !rty->isFPOrFPVectorTy() : !is_nonbool_int(rty)) {
        return spv.fail("math intrinsic result type mismatch");
      }
      if (ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("math intrinsic operand type mismatch");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      if (!a) return false;
      if (expect_fp && rty->getScalarType()->isBFloatTy()) {
        llvm::Type *f32_llvm = make_f32_llvm_ty(rty);
        uint32_t f32_spv = spv.map_llvm_type(f32_llvm);
        uint32_t af = emit_bf16_to_f32(a, rty);
        if (!f32_spv || !af) return false;
        uint32_t rf = emit_glsl_ext(f32_spv, glsl_op, {af});
        uint32_t rb = emit_f32_to_bf16(rf, f32_llvm);
        if (!rb) return false;
        val_map[ci] = rb;
        return spv.error.empty();
      }
      val_map[ci] = emit_glsl_ext(spv_rty, glsl_op, {a});
      return spv.error.empty();
    };
    auto binary_glsl = [&](uint32_t glsl_op, bool expect_fp) -> bool {
      if (ci->arg_size() < 2) return spv.fail("missing math args");
      if (expect_fp ? !rty->isFPOrFPVectorTy() : !is_nonbool_int(rty)) {
        return spv.fail("math intrinsic result type mismatch");
      }
      if (ci->getArgOperand(0)->getType() != rty || ci->getArgOperand(1)->getType() != rty) {
        return spv.fail("math intrinsic operand type mismatch");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      uint32_t b = get_val(ci->getArgOperand(1));
      if (!a || !b) return false;
      if (expect_fp && rty->getScalarType()->isBFloatTy()) {
        llvm::Type *f32_llvm = make_f32_llvm_ty(rty);
        uint32_t f32_spv = spv.map_llvm_type(f32_llvm);
        uint32_t af = emit_bf16_to_f32(a, rty);
        uint32_t bf = emit_bf16_to_f32(b, rty);
        if (!f32_spv || !af || !bf) return false;
        uint32_t rf = emit_glsl_ext(f32_spv, glsl_op, {af, bf});
        uint32_t rb = emit_f32_to_bf16(rf, f32_llvm);
        if (!rb) return false;
        val_map[ci] = rb;
        return spv.error.empty();
      }
      val_map[ci] = emit_glsl_ext(spv_rty, glsl_op, {a, b});
      return spv.error.empty();
    };
    auto ternary_glsl = [&](uint32_t glsl_op, bool expect_fp) -> bool {
      if (ci->arg_size() < 3) return spv.fail("missing math args");
      if (expect_fp ? !rty->isFPOrFPVectorTy() : !is_nonbool_int(rty)) {
        return spv.fail("math intrinsic result type mismatch");
      }
      if (ci->getArgOperand(0)->getType() != rty || ci->getArgOperand(1)->getType() != rty ||
          ci->getArgOperand(2)->getType() != rty) {
        return spv.fail("math intrinsic operand type mismatch");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      uint32_t b = get_val(ci->getArgOperand(1));
      uint32_t c = get_val(ci->getArgOperand(2));
      if (!a || !b || !c) return false;
      if (expect_fp && rty->getScalarType()->isBFloatTy()) {
        llvm::Type *f32_llvm = make_f32_llvm_ty(rty);
        uint32_t f32_spv = spv.map_llvm_type(f32_llvm);
        uint32_t af = emit_bf16_to_f32(a, rty);
        uint32_t bf = emit_bf16_to_f32(b, rty);
        uint32_t cf = emit_bf16_to_f32(c, rty);
        if (!f32_spv || !af || !bf || !cf) return false;
        uint32_t rf = emit_glsl_ext(f32_spv, glsl_op, {af, bf, cf});
        uint32_t rb = emit_f32_to_bf16(rf, f32_llvm);
        if (!rb) return false;
        val_map[ci] = rb;
        return spv.error.empty();
      }
      val_map[ci] = emit_glsl_ext(spv_rty, glsl_op, {a, b, c});
      return spv.error.empty();
    };

    if (stem == "fma" || stem.starts_with("fma.") || stem == "fmuladd" || stem.starts_with("fmuladd."))
      return ternary_glsl(GLSLstd450Fma, true);
    if (stem == "rsqrt" || stem.starts_with("rsqrt.")) return unary_glsl(GLSLstd450InverseSqrt, true);
    if (stem == "sqrt" || stem.starts_with("sqrt.")) return unary_glsl(GLSLstd450Sqrt, true);
    if (stem == "exp" || stem.starts_with("exp.")) return unary_glsl(GLSLstd450Exp, true);
    if (stem == "exp2" || stem.starts_with("exp2.")) return unary_glsl(GLSLstd450Exp2, true);
    if (stem == "log" || stem.starts_with("log.")) return unary_glsl(GLSLstd450Log, true);
    if (stem == "log2" || stem.starts_with("log2.")) return unary_glsl(GLSLstd450Log2, true);
    if (stem == "sin" || stem.starts_with("sin.")) return unary_glsl(GLSLstd450Sin, true);
    if (stem == "cos" || stem.starts_with("cos.")) return unary_glsl(GLSLstd450Cos, true);
    if (stem == "tan" || stem.starts_with("tan.")) return unary_glsl(GLSLstd450Tan, true);
    if (stem == "tanh" || stem.starts_with("tanh.")) return unary_glsl(GLSLstd450Tanh, true);
    if (stem == "asin" || stem.starts_with("asin.")) return unary_glsl(GLSLstd450Asin, true);
    if (stem == "acos" || stem.starts_with("acos.")) return unary_glsl(GLSLstd450Acos, true);
    if (stem == "atan" || stem.starts_with("atan.")) return unary_glsl(GLSLstd450Atan, true);
    if (stem == "atan2" || stem.starts_with("atan2.")) return binary_glsl(GLSLstd450Atan2, true);
    if (stem == "pow" || stem.starts_with("pow.") || stem == "powr" || stem.starts_with("powr."))
      return binary_glsl(GLSLstd450Pow, true);
    if (stem == "fabs" || stem.starts_with("fabs.")) return unary_glsl(GLSLstd450FAbs, true);
    if (stem == "abs" || stem.starts_with("abs.") || stem == "sabs" || stem.starts_with("sabs.")) {
      return rty->isFPOrFPVectorTy() ? unary_glsl(GLSLstd450FAbs, true)
                                     : unary_glsl(GLSLstd450SAbs, false);
    }
    if (stem == "floor" || stem.starts_with("floor.")) return unary_glsl(GLSLstd450Floor, true);
    if (stem == "ceil" || stem.starts_with("ceil.")) return unary_glsl(GLSLstd450Ceil, true);
    if (stem == "trunc" || stem.starts_with("trunc.")) return unary_glsl(GLSLstd450Trunc, true);
    if (stem == "rint" || stem.starts_with("rint.") || stem == "round" || stem.starts_with("round.") ||
        stem == "roundeven" || stem.starts_with("roundeven.")) {
      return unary_glsl(GLSLstd450RoundEven, true);
    }
    if (stem == "fract" || stem.starts_with("fract.")) return unary_glsl(GLSLstd450Fract, true);
    if (stem == "sign" || stem.starts_with("sign.")) return unary_glsl(GLSLstd450FSign, true);
    if (stem == "fmin" || stem.starts_with("fmin.") || stem == "minnum" || stem.starts_with("minnum.") ||
        stem == "minimum" || stem.starts_with("minimum.")) {
      return binary_glsl(GLSLstd450FMin, true);
    }
    if (stem == "fmax" || stem.starts_with("fmax.") || stem == "maxnum" || stem.starts_with("maxnum.") ||
        stem == "maximum" || stem.starts_with("maximum.")) {
      return binary_glsl(GLSLstd450FMax, true);
    }
    if (stem == "smin" || stem.starts_with("smin.") || stem == "min.s" || stem.starts_with("min.s.")) {
      return binary_glsl(GLSLstd450SMin, false);
    }
    if (stem == "umin" || stem.starts_with("umin.") || stem == "min.u" || stem.starts_with("min.u.")) {
      return binary_glsl(GLSLstd450UMin, false);
    }
    if (stem == "smax" || stem.starts_with("smax.") || stem == "max.s" || stem.starts_with("max.s.")) {
      return binary_glsl(GLSLstd450SMax, false);
    }
    if (stem == "umax" || stem.starts_with("umax.") || stem == "max.u" || stem.starts_with("max.u.")) {
      return binary_glsl(GLSLstd450UMax, false);
    }
    if (stem == "fclamp" || stem.starts_with("fclamp.") ||
        (stem.starts_with("clamp") && rty->isFPOrFPVectorTy())) {
      return ternary_glsl(GLSLstd450FClamp, true);
    }
    if (stem == "sclamp" || stem.starts_with("sclamp.")) return ternary_glsl(GLSLstd450SClamp, false);
    if (stem == "uclamp" || stem.starts_with("uclamp.")) return ternary_glsl(GLSLstd450UClamp, false);

    if (stem == "dot" || stem.starts_with("dot.")) {
      if (ci->arg_size() < 2) return spv.fail("missing dot args");
      auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ci->getArgOperand(0)->getType());
      if (!rty->isFloatingPointTy() || !vt || ci->getArgOperand(1)->getType() != vt ||
          vt->getElementType() != rty) {
        return spv.fail("dot intrinsic requires matching floating-point vector operands");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      uint32_t b = get_val(ci->getArgOperand(1));
      if (!a || !b) return false;
      if (rty->isBFloatTy()) {
        uint32_t af = emit_bf16_to_f32(a, vt);
        uint32_t bf = emit_bf16_to_f32(b, vt);
        if (!af || !bf) return false;
        uint32_t f32 = spv.get_float(32);
        uint32_t df = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpDot, {f32, df, af, bf});
        uint32_t db = emit_f32_to_bf16(df, llvm::Type::getFloatTy(mod.getContext()));
        if (!db) return false;
        val_map[ci] = db;
        return spv.error.empty();
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpDot, {spv_rty, id, a, b});
      val_map[ci] = id;
      return spv.error.empty();
    }

    if (stem == "ctpop" || stem.starts_with("ctpop.")) {
      if (ci->arg_size() < 1) return spv.fail("missing ctpop arg");
      if (!is_nonbool_int(rty) || ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("ctpop requires matching integer operand and result");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      if (!a) return false;
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitCount, {spv_rty, id, a});
      val_map[ci] = id;
      return spv.error.empty();
    }
    if (stem == "bitreverse" || stem.starts_with("bitreverse.")) {
      if (ci->arg_size() < 1) return spv.fail("missing bitreverse arg");
      if (!is_nonbool_int(rty) || ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("bitreverse requires matching integer operand and result");
      }
      uint32_t a = get_val(ci->getArgOperand(0));
      if (!a) return false;
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitReverse, {spv_rty, id, a});
      val_map[ci] = id;
      return spv.error.empty();
    }

    /* Subgroup (SIMD-group) intrinsics gated against hardware supportedOperations. */
    uint32_t max_wg_budget = spv.hw_caps.max_workgroup_memory
                                 ? std::min(spv.hw_caps.max_workgroup_memory, kMaxWorkgroupBytes)
                                 : kMaxWorkgroupBytes;
    auto emit_wg_total_threads = [&]() -> uint32_t {
      uint32_t u32 = spv.get_int(32);
      uint32_t xy = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpIMul, {u32, xy, spv.spec_lx, spv.spec_ly});
      uint32_t tot = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpIMul, {u32, tot, xy, spv.spec_lz});
      return tot;
    };
    auto emit_subgroup_arith = [&](spv::Op fp_op, spv::Op int_op, spv::GroupOperation gop) -> bool {
      if (ci->arg_size() < 1) return spv.fail("missing subgroup arith arg");
      uint32_t req_ops = AIR2SPV_SUBGROUP_BASIC | AIR2SPV_SUBGROUP_ARITHMETIC;
      if ((spv.hw_caps.subgroup_ops & req_ops) != req_ops) {
        return spv.fail("subgroup arithmetic capability unsupported by GPU");
      }
      if ((!rty->isFPOrFPVectorTy() && !is_nonbool_int(rty)) ||
          ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("subgroup arithmetic operand type mismatch");
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      spv.caps.insert(spv::CapabilityGroupNonUniformArithmetic);
      uint32_t a = get_val(ci->getArgOperand(0));
      if (!a) return false;

      bool is_bf16 = rty->getScalarType()->isBFloatTy();
      bool is_half_emul = (spv.hw_caps.subgroup_size == 16u && rty->isHalfTy());
      if (spv.hw_caps.subgroup_size == 16u &&
          !(rty->isFloatTy() || rty->isIntegerTy(32) || rty->isHalfTy() || rty->isBFloatTy())) {
        return spv.fail("unsupported type for 32-lane simdgroup arithmetic emulation on 16-lane GPU");
      }

      uint32_t work_a = a;
      uint32_t work_spv_ty = spv_rty;
      bool work_is_fp = rty->isFPOrFPVectorTy();
      if (is_bf16) {
        llvm::Type *f32_llvm = make_f32_llvm_ty(rty);
        work_spv_ty = spv.map_llvm_type(f32_llvm);
        work_a = emit_bf16_to_f32(a, rty);
        if (!work_spv_ty || !work_a) return false;
      } else if (is_half_emul) {
        work_spv_ty = spv.get_float(32);
        uint32_t af = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFConvert, {work_spv_ty, af, a});
        work_a = af;
      }

      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, work_is_fp ? fp_op : int_op,
                  {work_spv_ty, id, spv.get_const_u32(static_cast<uint32_t>(spv::ScopeSubgroup)),
                   static_cast<uint32_t>(gop), work_a});

      if (spv.hw_caps.subgroup_size == 16u) {
        /* Pair adjacent 16-lane halves via 256B workgroup u32 scratch (atomic store avoids races). */
        uint32_t u32 = spv.get_int(32);
        uint32_t bty = spv.get_bool();
        uint32_t lvar = get_or_create_builtin_var(BuiltinKind::LocalInvocationIndex);
        if (!lvar) return false;
        uint32_t raw_lid = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpLoad, {u32, raw_lid, lvar});
        uint32_t hw_sg = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, hw_sg, raw_lid, spv.get_const_u32(4u)});
        uint32_t sg_idx = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, sg_idx, hw_sg, spv.get_const_u32(63u)});

        uint32_t wg_tot = emit_wg_total_threads();
        uint32_t plus15 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, plus15, wg_tot, spv.get_const_u32(15u)});
        uint32_t hw_nsg = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpShiftRightLogical, {u32, hw_nsg, plus15, spv.get_const_u32(4u)});

        uint32_t ptr_ty = 0;
        uint32_t scratch_var = spv.get_sg32_scratch_u32(wg_bytes_used, max_wg_budget, &ptr_ty);
        if (!scratch_var) return false;

        uint32_t pub_val = id;
        if (gop != spv::GroupOperationReduce) {
          pub_val = spv.alloc_id();
          spv.emit_op(cur_body, work_is_fp ? fp_op : int_op,
                      {work_spv_ty, pub_val,
                       spv.get_const_u32(static_cast<uint32_t>(spv::ScopeSubgroup)),
                       static_cast<uint32_t>(spv::GroupOperationReduce), work_a});
        }
        uint32_t pub_u32 = pub_val;
        if (work_is_fp) {
          pub_u32 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {u32, pub_u32, pub_val});
        }
        uint32_t st_ptr = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpAccessChain, {ptr_ty, st_ptr, scratch_var, sg_idx});
        spv.emit_op(cur_body, spv::OpAtomicStore,
                    {st_ptr, spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                     spv.get_const_u32(0u), pub_u32});
        uint32_t sem = static_cast<uint32_t>(spv::MemorySemanticsAcquireReleaseMask) |
                       static_cast<uint32_t>(spv::MemorySemanticsWorkgroupMemoryMask);
        spv.emit_op(cur_body, spv::OpControlBarrier,
                    {spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                     spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                     spv.get_const_u32(sem)});

        if (gop == spv::GroupOperationReduce) {
          uint32_t partner = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseXor, {u32, partner, hw_sg, spv.get_const_u32(1u)});
          uint32_t has_partner = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpULessThan, {bty, has_partner, partner, hw_nsg});
          uint32_t partner_idx = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, partner_idx, partner, spv.get_const_u32(63u)});
          uint32_t safe_partner = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpSelect, {u32, safe_partner, has_partner, partner_idx, sg_idx});
          uint32_t ld_ptr = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpAccessChain, {ptr_ty, ld_ptr, scratch_var, safe_partner});
          uint32_t pval_u32 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpLoad, {u32, pval_u32, ld_ptr});
          uint32_t pval = pval_u32;
          if (work_is_fp) {
            pval = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpBitcast, {work_spv_ty, pval, pval_u32});
          }
          uint32_t comb = 0;
          if (work_is_fp) {
            if (fp_op == spv::OpGroupNonUniformFAdd) {
              comb = spv.alloc_id();
              spv.emit_op(cur_body, spv::OpFAdd, {work_spv_ty, comb, id, pval});
            } else if (fp_op == spv::OpGroupNonUniformFMax) {
              comb = emit_glsl_ext(work_spv_ty, GLSLstd450FMax, {id, pval});
            } else {
              comb = emit_glsl_ext(work_spv_ty, GLSLstd450FMin, {id, pval});
            }
          } else {
            if (int_op == spv::OpGroupNonUniformIAdd) {
              comb = spv.alloc_id();
              spv.emit_op(cur_body, spv::OpIAdd, {work_spv_ty, comb, id, pval});
            } else if (int_op == spv::OpGroupNonUniformSMax) {
              comb = emit_glsl_ext(work_spv_ty, GLSLstd450SMax, {id, pval});
            } else {
              comb = emit_glsl_ext(work_spv_ty, GLSLstd450SMin, {id, pval});
            }
          }
          uint32_t res = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpSelect, {work_spv_ty, res, has_partner, comb, id});
          id = res;
        } else {
          /* 32-lane ExclusiveScan / InclusiveScan: odd half adds even half's 16-lane total. */
          uint32_t even_idx = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, even_idx, sg_idx, spv.get_const_u32(~1u & 63u)});
          uint32_t odd_bit = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, odd_bit, sg_idx, spv.get_const_u32(1u)});
          uint32_t is_odd = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpINotEqual, {bty, is_odd, odd_bit, spv.get_const_u32(0u)});
          uint32_t ld_ptr = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpAccessChain, {ptr_ty, ld_ptr, scratch_var, even_idx});
          uint32_t eval_u32 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpLoad, {u32, eval_u32, ld_ptr});
          uint32_t eval = eval_u32;
          if (work_is_fp) {
            eval = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpBitcast, {work_spv_ty, eval, eval_u32});
          }
          uint32_t shifted = spv.alloc_id();
          spv.emit_op(cur_body, work_is_fp ? spv::OpFAdd : spv::OpIAdd,
                      {work_spv_ty, shifted, id, eval});
          uint32_t res = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpSelect, {work_spv_ty, res, is_odd, shifted, id});
          id = res;
        }
        spv.emit_op(cur_body, spv::OpControlBarrier,
                    {spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                     spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                     spv.get_const_u32(sem)});
      }

      if (is_bf16) {
        id = emit_f32_to_bf16(id, make_f32_llvm_ty(rty));
        if (!id) return false;
      } else if (is_half_emul) {
        uint32_t h = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFConvert, {spv_rty, h, id});
        id = h;
      }
      val_map[ci] = id;
      return spv.error.empty();
    };
    if (match_math("air.simd_sum"))
      return emit_subgroup_arith(spv::OpGroupNonUniformFAdd, spv::OpGroupNonUniformIAdd,
                                 spv::GroupOperationReduce);
    if (match_math("air.simd_prefix_exclusive_sum"))
      return emit_subgroup_arith(spv::OpGroupNonUniformFAdd, spv::OpGroupNonUniformIAdd,
                                 spv::GroupOperationExclusiveScan);
    if (match_math("air.simd_prefix_inclusive_sum"))
      return emit_subgroup_arith(spv::OpGroupNonUniformFAdd, spv::OpGroupNonUniformIAdd,
                                 spv::GroupOperationInclusiveScan);
    if (match_math("air.simd_max"))
      return emit_subgroup_arith(spv::OpGroupNonUniformFMax, spv::OpGroupNonUniformSMax,
                                 spv::GroupOperationReduce);
    if (match_math("air.simd_min"))
      return emit_subgroup_arith(spv::OpGroupNonUniformFMin, spv::OpGroupNonUniformSMin,
                                 spv::GroupOperationReduce);
    auto emit_sg32_shuffle_emu = [&](uint32_t val, uint32_t arg, int mode) -> bool {
      /* mode: 0=down, 1=up, 2=xor, 3=index/broadcast */
      if (!(rty->isFloatTy() || rty->isIntegerTy(32) || rty->isHalfTy() || rty->isBFloatTy() ||
            rty->isIntegerTy(16))) {
        return spv.fail("unsupported type for 32-lane simdgroup shuffle emulation on 16-lane GPU");
      }
      bool is_fp = rty->isFloatTy() || rty->isHalfTy();
      uint32_t u32 = spv.get_int(32);
      uint32_t f32 = spv.get_float(32);
      uint32_t bty = spv.get_bool();
      uint32_t work_ty = is_fp ? f32 : u32;
      uint32_t work_val = val;
      if (rty->isHalfTy()) {
        work_val = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFConvert, {f32, work_val, val});
      } else if (rty->isBFloatTy() || rty->isIntegerTy(16)) {
        work_val = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {u32, work_val, val});
      }

      uint32_t lvar = get_or_create_builtin_var(BuiltinKind::LocalInvocationIndex);
      if (!lvar) return false;
      uint32_t raw_lid = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {u32, raw_lid, lvar});
      uint32_t lid = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, lid, raw_lid, spv.get_const_u32(1023u)});
      uint32_t lane = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, lane, lid, spv.get_const_u32(31u)});
      uint32_t base = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, base, lid, spv.get_const_u32(~31u & 1023u)});

      uint32_t ptr_ty = 0;
      uint32_t scratch = spv.get_sg32_shuf_scratch(is_fp, wg_bytes_used, max_wg_budget, &ptr_ty);
      if (!scratch) return false;
      uint32_t st_ptr = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpAccessChain, {ptr_ty, st_ptr, scratch, lid});
      spv.emit_op(cur_body, spv::OpStore, {st_ptr, work_val});
      uint32_t sem = static_cast<uint32_t>(spv::MemorySemanticsAcquireReleaseMask) |
                     static_cast<uint32_t>(spv::MemorySemanticsWorkgroupMemoryMask);
      spv.emit_op(cur_body, spv::OpControlBarrier,
                  {spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(sem)});

      uint32_t target_idx = lid;
      if (mode == 0) {
        uint32_t arg_ok = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpULessThan, {bty, arg_ok, arg, spv.get_const_u32(32u)});
        uint32_t tlane = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, tlane, lane, arg});
        uint32_t tlane_ok = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpULessThan, {bty, tlane_ok, tlane, spv.get_const_u32(32u)});
        uint32_t ok = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpLogicalAnd, {bty, ok, arg_ok, tlane_ok});
        uint32_t cand = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, cand, base, tlane});
        target_idx = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {u32, target_idx, ok, cand, lid});
      } else if (mode == 1) {
        uint32_t ok = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUGreaterThanEqual, {bty, ok, lane, arg});
        uint32_t tlane = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpISub, {u32, tlane, lane, arg});
        uint32_t cand = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, cand, base, tlane});
        target_idx = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {u32, target_idx, ok, cand, lid});
      } else if (mode == 2) {
        uint32_t x = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseXor, {u32, x, lane, arg});
        uint32_t tlane = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, tlane, x, spv.get_const_u32(31u)});
        target_idx = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, target_idx, base, tlane});
      } else {
        uint32_t tlane = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, tlane, arg, spv.get_const_u32(31u)});
        target_idx = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpIAdd, {u32, target_idx, base, tlane});
      }
      /* Clamp against actual workgroup size so non-multiple-of-32 groups never read uninitialized slots. */
      uint32_t wg_tot = emit_wg_total_threads();
      uint32_t in_wg = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpULessThan, {bty, in_wg, target_idx, wg_tot});
      uint32_t safe_idx = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelect, {u32, safe_idx, in_wg, target_idx, lid});

      uint32_t ld_ptr = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpAccessChain, {ptr_ty, ld_ptr, scratch, safe_idx});
      uint32_t res = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpLoad, {work_ty, res, ld_ptr});
      spv.emit_op(cur_body, spv::OpControlBarrier,
                  {spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(static_cast<uint32_t>(spv::ScopeWorkgroup)),
                   spv.get_const_u32(sem)});
      if (rty->isHalfTy()) {
        uint32_t h = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFConvert, {spv_rty, h, res});
        res = h;
      } else if (rty->isBFloatTy() || rty->isIntegerTy(16)) {
        uint32_t u16v = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {spv_rty, u16v, res});
        res = u16v;
      }
      val_map[ci] = res;
      return spv.error.empty();
    };
    if (match_math("air.simd_shuffle_down") || match_math("air.simd_shuffle_up") ||
        match_math("air.simd_shuffle_xor") || match_math("air.simd_shuffle")) {
      if (ci->arg_size() < 2) return spv.fail("missing subgroup shuffle args");
      if ((!rty->isFPOrFPVectorTy() && !rty->isIntOrIntVectorTy()) ||
          ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("subgroup shuffle operand type mismatch");
      }
      uint32_t val = get_val(ci->getArgOperand(0));
      uint32_t delta = get_index_u32(ci->getArgOperand(1));
      if (!val || !delta) return false;
      if (spv.hw_caps.subgroup_size == 16u) {
        int mode = 3;
        if (match_math("air.simd_shuffle_down")) mode = 0;
        else if (match_math("air.simd_shuffle_up")) mode = 1;
        else if (match_math("air.simd_shuffle_xor")) mode = 2;
        return emit_sg32_shuffle_emu(val, delta, mode);
      }
      bool is_rel = match_math("air.simd_shuffle_down") || match_math("air.simd_shuffle_up");
      uint32_t req_ops = AIR2SPV_SUBGROUP_BASIC |
                         (is_rel ? AIR2SPV_SUBGROUP_SHUFFLE_RELATIVE : AIR2SPV_SUBGROUP_SHUFFLE);
      if ((spv.hw_caps.subgroup_ops & req_ops) != req_ops) {
        return spv.fail("subgroup shuffle capability unsupported by GPU");
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      spv.caps.insert(is_rel ? spv::CapabilityGroupNonUniformShuffleRelative
                             : spv::CapabilityGroupNonUniformShuffle);
      spv::Op op = spv::OpGroupNonUniformShuffle;
      if (match_math("air.simd_shuffle_down")) op = spv::OpGroupNonUniformShuffleDown;
      else if (match_math("air.simd_shuffle_up")) op = spv::OpGroupNonUniformShuffleUp;
      else if (match_math("air.simd_shuffle_xor")) op = spv::OpGroupNonUniformShuffleXor;
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op,
                  {spv_rty, id, spv.get_const_u32(static_cast<uint32_t>(spv::ScopeSubgroup)),
                   val, delta});
      val_map[ci] = id;
      return spv.error.empty();
    }
    if (match_math("air.simd_broadcast")) {
      if (ci->arg_size() < 2) return spv.fail("missing subgroup broadcast args");
      if ((!rty->isFPOrFPVectorTy() && !rty->isIntOrIntVectorTy()) ||
          ci->getArgOperand(0)->getType() != rty) {
        return spv.fail("subgroup broadcast operand type mismatch");
      }
      uint32_t val = get_val(ci->getArgOperand(0));
      uint32_t lane = get_index_u32(ci->getArgOperand(1));
      if (!val || !lane) return false;
      if (spv.hw_caps.subgroup_size == 16u) {
        return emit_sg32_shuffle_emu(val, lane, 3);
      }
      uint32_t req_ops = AIR2SPV_SUBGROUP_BASIC | AIR2SPV_SUBGROUP_BALLOT;
      if ((spv.hw_caps.subgroup_ops & req_ops) != req_ops) {
        return spv.fail("subgroup broadcast capability unsupported by GPU");
      }
      spv.caps.insert(spv::CapabilityGroupNonUniform);
      spv.caps.insert(spv::CapabilityGroupNonUniformBallot);
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpGroupNonUniformBroadcast,
                  {spv_rty, id, spv.get_const_u32(static_cast<uint32_t>(spv::ScopeSubgroup)),
                   val, lane});
      val_map[ci] = id;
      return spv.error.empty();
    }

    /* Apple AIR type conversion intrinsics: air.convert.* */
    if (name.starts_with("air.convert.")) {
      if (ci->arg_size() < 1) return spv.fail("missing air.convert arg");
      llvm::Value *src_v = ci->getArgOperand(0);
      llvm::Type *sty = src_v->getType();
      bool src_is_bool = sty->getScalarType()->isIntegerTy(1);
      bool src_ok = sty->isFPOrFPVectorTy() || is_nonbool_int(sty) || src_is_bool;
      bool dst_ok = rty->isFPOrFPVectorTy() || is_nonbool_int(rty);
      if (!src_ok || !dst_ok || vec_lanes(sty) != vec_lanes(rty)) {
        return spv.fail("unsupported air.convert operand or result type");
      }
      uint32_t src_id = get_val(src_v);
      if (!src_id) return false;
      if (sty == rty) {
        val_map[ci] = src_id;
        return true;
      }
      if (src_is_bool) {
        bool is_signed = name.contains(".s.");
        llvm::Type *r_scalar = rty->getScalarType();
        uint32_t c_zero = 0, c_one = 0;
        if (r_scalar->isFloatTy()) {
          c_zero = spv.get_const_f32(0.0f);
          c_one = spv.get_const_f32(is_signed ? -1.0f : 1.0f);
        } else if (r_scalar->isHalfTy()) {
          c_zero = spv.get_const_fp_bits(16, 0x0000u);
          c_one = spv.get_const_fp_bits(16, is_signed ? 0xBC00u : 0x3C00u);
        } else if (r_scalar->isBFloatTy()) {
          c_zero = spv.get_const_int(16, 0x0000u);
          c_one = spv.get_const_int(16, is_signed ? 0xBF80u : 0x3F80u);
        } else if (auto *it = llvm::dyn_cast<llvm::IntegerType>(r_scalar)) {
          uint32_t bw = it->getBitWidth();
          c_zero = spv.get_const_int(bw, 0);
          c_one = spv.get_const_int(bw, is_signed ? ~0ULL : 1ULL);
        } else {
          return spv.fail("unsupported air.convert bool destination type");
        }
        if (!c_zero || !c_one) return false;
        uint32_t lanes = vec_lanes(rty);
        if (lanes > 1) {
          std::vector<uint32_t> z_ops = {spv_rty, 0};
          std::vector<uint32_t> o_ops = {spv_rty, 0};
          uint32_t vz = spv.alloc_id();
          uint32_t vo = spv.alloc_id();
          z_ops[1] = vz;
          o_ops[1] = vo;
          for (uint32_t l = 0; l < lanes; l++) {
            z_ops.push_back(c_zero);
            o_ops.push_back(c_one);
          }
          spv.emit_op_vec(spv.types_consts_vars, spv::OpConstantComposite, z_ops);
          spv.emit_op_vec(spv.types_consts_vars, spv::OpConstantComposite, o_ops);
          c_zero = vz;
          c_one = vo;
        }
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {spv_rty, id, src_id, c_one, c_zero});
        val_map[ci] = id;
        return spv.error.empty();
      }
      bool src_is_bf16 = sty->getScalarType()->isBFloatTy();
      bool dst_is_bf16 = rty->getScalarType()->isBFloatTy();
      if (src_is_bf16 || dst_is_bf16) {
        uint32_t lanes = vec_lanes(rty);
        llvm::Type *f32_llvm = lanes > 1 ? static_cast<llvm::Type *>(
                                               llvm::FixedVectorType::get(llvm::Type::getFloatTy(mod.getContext()), lanes))
                                         : static_cast<llvm::Type *>(llvm::Type::getFloatTy(mod.getContext()));
        uint32_t f32_spv = spv.map_llvm_type(f32_llvm);
        if (!f32_spv) return false;
        uint32_t cur = src_id;
        if (src_is_bf16) {
          cur = emit_bf16_to_f32(cur, sty);
          if (!cur) return false;
          if (rty->getScalarType()->isFloatTy()) {
            val_map[ci] = cur;
            return spv.error.empty();
          }
          uint32_t id = spv.alloc_id();
          spv::Op op = rty->isFPOrFPVectorTy() ? spv::OpFConvert
                                               : (name.contains(".s.") ? spv::OpConvertFToS : spv::OpConvertFToU);
          spv.emit_op(cur_body, op, {spv_rty, id, cur});
          val_map[ci] = id;
          return spv.error.empty();
        }
        if (!sty->getScalarType()->isFloatTy()) {
          uint32_t tmp = spv.alloc_id();
          spv::Op op = sty->isFPOrFPVectorTy() ? spv::OpFConvert
                                               : (name.contains(".s.") ? spv::OpConvertSToF : spv::OpConvertUToF);
          spv.emit_op(cur_body, op, {f32_spv, tmp, cur});
          cur = tmp;
        }
        uint32_t bf = emit_f32_to_bf16(cur, f32_llvm);
        if (!bf) return false;
        val_map[ci] = bf;
        return spv.error.empty();
      }
      uint32_t id = spv.alloc_id();
      spv::Op op = spv::OpNop;
      if (sty->isFPOrFPVectorTy() && rty->isFPOrFPVectorTy()) {
        op = spv::OpFConvert;
      } else if (sty->isFPOrFPVectorTy() && is_nonbool_int(rty)) {
        op = name.contains(".s.") ? spv::OpConvertFToS : spv::OpConvertFToU;
      } else if (is_nonbool_int(sty) && rty->isFPOrFPVectorTy()) {
        op = name.contains(".s.") ? spv::OpConvertSToF : spv::OpConvertUToF;
      } else {
        op = name.contains(".s.") ? spv::OpSConvert : spv::OpUConvert;
      }
      spv.emit_op(cur_body, op, {spv_rty, id, src_id});
      val_map[ci] = id;
      return spv.error.empty();
    }

    return spv.fail("unsupported AIR/LLVM intrinsic call: " + name.str());
  }

  bool emit_inst(llvm::Instruction &inst) {
    if (!spv.error.empty()) return false;
    if (llvm::isa<llvm::PHINode>(&inst) || llvm::isa<llvm::AllocaInst>(&inst) ||
        inst.isTerminator()) {
      return true;
    }
    if (inst.getType()->isPointerTy()) {
      /* Pointer-producing ops are tracked symbolically by resolve_ptr(). */
      if (resolve_ptr(&inst).kind == PtrKind::Invalid) return false;
      return spv.error.empty();
    }
    if (is_i64_index_only(&inst)) {
      /* Materialize in-place as u32 once per instruction (O(1), dominance-safe, zero Int64). */
      uint32_t id = materialize_narrowed_i64(&inst);
      if (!id) return false;
      narrowed_i64_vals[&inst] = id;
      return spv.error.empty();
    }

    if (auto *li = llvm::dyn_cast<llvm::LoadInst>(&inst)) {
      PtrInfo ptr = resolve_ptr(li->getPointerOperand());
      if (ptr.kind == PtrKind::Invalid) return false;
      if (ptr.kind == PtrKind::FuncConst) {
        if (auto *gv = llvm::dyn_cast<llvm::GlobalVariable>(li->getPointerOperand()->stripPointerCasts())) {
          auto it = fc_map.find(gv);
          if (it != fc_map.end()) {
            if (it->second.is_air_bool) {
              if (li->getType()->isIntegerTy(8)) {
                uint32_t u8 = spv.get_int(8);
                uint32_t c1 = spv.get_const_int(8, 1);
                uint32_t c0 = spv.get_const_int(8, 0);
                if (!u8 || !c1 || !c0) return false;
                uint32_t sel = spv.alloc_id();
                spv.emit_op(cur_body, spv::OpSelect, {u8, sel, it->second.spv_id, c1, c0});
                val_map[li] = sel;
                return spv.error.empty();
              }
              if (li->getType()->isIntegerTy(1)) {
                val_map[li] = it->second.spv_id;
                return true;
              }
              return spv.fail("boolean function constant load type mismatch");
            }
            if (li->getType() != it->second.type) {
              return spv.fail("function constant load type mismatch");
            }
            val_map[li] = it->second.spv_id;
            return true;
          }
        }
        return spv.fail("invalid function constant load");
      }
      uint32_t v = emit_mem_load_ptr(ptr, li->getType());
      if (!v) return false;
      val_map[li] = v;
      return spv.error.empty();
    }

    if (auto *si = llvm::dyn_cast<llvm::StoreInst>(&inst)) {
      PtrInfo ptr = resolve_ptr(si->getPointerOperand());
      if (ptr.kind == PtrKind::Invalid || ptr.kind == PtrKind::FuncConst) {
        return spv.fail("invalid store destination");
      }
      if (is_ptr_readonly(ptr)) {
        return spv.fail("store to readonly buffer binding");
      }
      if (si->getValueOperand()->getType()->isPointerTy()) {
        if (ptr.is_dual() || ptr.kind != PtrKind::Function || !ptr_alloca_roots.count(ptr.base_id)) {
          return spv.fail("pointer store only supported to single-root function alloca");
        }
        llvm::Value *sv = si->getValueOperand();
        uint32_t off = 0;
        if (llvm::isa<llvm::ConstantPointerNull>(sv) || llvm::isa<llvm::UndefValue>(sv) ||
            llvm::isa<llvm::PoisonValue>(sv)) {
          off = spv.get_const_u32(0);
        } else {
          PtrInfo vp = resolve_ptr(sv);
          if (vp.kind == PtrKind::Invalid || vp.is_dual()) return spv.fail("dual-root pointer cannot be stored to alloca");
          auto root = ptr_alloca_roots[ptr.base_id];
          if (vp.kind != root.first || vp.base_id != root.second) {
            return spv.fail("pointer store root mismatch in function alloca");
          }
          off = materialize_byte_off(vp);
        }
        return emit_mem_store_ptr(ptr, llvm::Type::getInt32Ty(mod.getContext()), off);
      }
      if (!spv.hw_caps.has_int64 && si->getValueOperand()->getType()->isIntegerTy(64)) {
        if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(si->getValueOperand())) {
          uint64_t val = ci->getZExtValue();
          llvm::Type *u32_ty = llvm::Type::getInt32Ty(mod.getContext());
          if (!emit_mem_store_ptr(ptr, u32_ty, spv.get_const_u32(static_cast<uint32_t>(val)))) {
            return false;
          }
          PtrInfo hi = shift_ptr_bytes(ptr, 4u);
          return emit_mem_store_ptr(hi, u32_ty,
                                    spv.get_const_u32(static_cast<uint32_t>(val >> 32)));
        }
      }
      uint32_t v = get_val(si->getValueOperand());
      if (!v) return false;
      return emit_mem_store_ptr(ptr, si->getValueOperand()->getType(), v);
    }

    if (auto *rmw = llvm::dyn_cast<llvm::AtomicRMWInst>(&inst)) {
      PtrInfo ptr = resolve_ptr(rmw->getPointerOperand());
      if (ptr.is_dual() || (ptr.kind != PtrKind::Buffer && ptr.kind != PtrKind::Workgroup)) {
        return spv.fail("atomicrmw requires single-root buffer or workgroup memory");
      }
      if (is_ptr_readonly(ptr)) {
        return spv.fail("atomicrmw on readonly buffer binding");
      }
      if (!rmw->getType()->isIntegerTy(32)) return spv.fail("atomicrmw currently supports i32");
      uint32_t ep = emit_elem_ptr(ptr, materialize_elem_idx(ptr, 2));
      if (!ep) return false;
      uint32_t val = get_val(rmw->getValOperand());
      if (!val) return false;
      uint32_t scope = spv.get_const_u32(
          ptr.kind == PtrKind::Workgroup ? static_cast<uint32_t>(spv::ScopeWorkgroup)
                                         : static_cast<uint32_t>(spv::ScopeDevice));
      uint32_t sem = spv.get_const_u32(0u);
      spv::Op op = spv::OpNop;
      switch (rmw->getOperation()) {
      case llvm::AtomicRMWInst::Xchg: op = spv::OpAtomicExchange; break;
      case llvm::AtomicRMWInst::Add: op = spv::OpAtomicIAdd; break;
      case llvm::AtomicRMWInst::Sub: op = spv::OpAtomicISub; break;
      case llvm::AtomicRMWInst::And: op = spv::OpAtomicAnd; break;
      case llvm::AtomicRMWInst::Or: op = spv::OpAtomicOr; break;
      case llvm::AtomicRMWInst::Xor: op = spv::OpAtomicXor; break;
      case llvm::AtomicRMWInst::Max: op = spv::OpAtomicSMax; break;
      case llvm::AtomicRMWInst::Min: op = spv::OpAtomicSMin; break;
      case llvm::AtomicRMWInst::UMax: op = spv::OpAtomicUMax; break;
      case llvm::AtomicRMWInst::UMin: op = spv::OpAtomicUMin; break;
      default: return spv.fail("unsupported atomicrmw operation");
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op, {spv.get_int(32), id, ep, scope, sem, val});
      val_map[rmw] = id;
      return true;
    }

    if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&inst)) {
      return emit_call(ci);
    }

    if (auto *fi = llvm::dyn_cast<llvm::FreezeInst>(&inst)) {
      uint32_t v = get_val(fi->getOperand(0));
      if (!v) return false;
      val_map[fi] = v;
      return true;
    }

    uint32_t rty = spv.map_llvm_type(inst.getType());
    if (!rty) return false;

    if (auto *uo = llvm::dyn_cast<llvm::UnaryOperator>(&inst)) {
      if (uo->getOpcode() == llvm::Instruction::FNeg) {
        uint32_t a = get_val(uo->getOperand(0));
        if (!a) return false;
        if (uo->getType()->getScalarType()->isBFloatTy()) {
          uint32_t lanes = 1u;
          if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(uo->getType())) lanes = vt->getNumElements();
          uint32_t id = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseXor, {rty, id, a, splat_const_int(16, 0x8000u, lanes)});
          val_map[uo] = id;
          return spv.error.empty();
        }
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpFNegate, {rty, id, a});
        val_map[uo] = id;
        return true;
      }
      return spv.fail("unsupported unary operator");
    }

    if (auto *bo = llvm::dyn_cast<llvm::BinaryOperator>(&inst)) {
      uint32_t a = get_val(bo->getOperand(0));
      uint32_t b = get_val(bo->getOperand(1));
      if (!a || !b) return false;
      if (bo->getType()->getScalarType()->isBFloatTy()) {
        spv::Op fop = spv::OpNop;
        switch (bo->getOpcode()) {
        case llvm::Instruction::FAdd: fop = spv::OpFAdd; break;
        case llvm::Instruction::FSub: fop = spv::OpFSub; break;
        case llvm::Instruction::FMul: fop = spv::OpFMul; break;
        case llvm::Instruction::FDiv: fop = spv::OpFDiv; break;
        case llvm::Instruction::FRem: fop = spv::OpFRem; break;
        default: return spv.fail("unsupported bfloat binary operator");
        }
        uint32_t af = emit_bf16_to_f32(a, bo->getType());
        uint32_t bf = emit_bf16_to_f32(b, bo->getType());
        if (!af || !bf) return false;
        uint32_t lanes = 1u;
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(bo->getType())) lanes = vt->getNumElements();
        llvm::Type *f32_llvm = lanes > 1 ? static_cast<llvm::Type *>(
                                               llvm::FixedVectorType::get(llvm::Type::getFloatTy(mod.getContext()), lanes))
                                         : static_cast<llvm::Type *>(llvm::Type::getFloatTy(mod.getContext()));
        uint32_t f32_ty = spv.map_llvm_type(f32_llvm);
        uint32_t rf = spv.alloc_id();
        spv.emit_op(cur_body, fop, {f32_ty, rf, af, bf});
        uint32_t res = emit_f32_to_bf16(rf, f32_llvm);
        if (!res) return false;
        val_map[bo] = res;
        return spv.error.empty();
      }
      bool is_bool = bo->getType()->getScalarType()->isIntegerTy(1);
      if (is_bool && bo->getOpcode() != llvm::Instruction::And &&
          bo->getOpcode() != llvm::Instruction::Or &&
          bo->getOpcode() != llvm::Instruction::Xor) {
        return spv.fail("unsupported boolean arithmetic operator in AIR");
      }
      spv::Op op = spv::OpNop;
      switch (bo->getOpcode()) {
      case llvm::Instruction::Add: op = spv::OpIAdd; break;
      case llvm::Instruction::Sub: op = spv::OpISub; break;
      case llvm::Instruction::Mul: op = spv::OpIMul; break;
      case llvm::Instruction::UDiv: op = spv::OpUDiv; break;
      case llvm::Instruction::SDiv: op = spv::OpSDiv; break;
      case llvm::Instruction::URem: op = spv::OpUMod; break;
      case llvm::Instruction::SRem: op = spv::OpSRem; break;
      case llvm::Instruction::FAdd: op = spv::OpFAdd; break;
      case llvm::Instruction::FSub: op = spv::OpFSub; break;
      case llvm::Instruction::FMul: op = spv::OpFMul; break;
      case llvm::Instruction::FDiv: op = spv::OpFDiv; break;
      case llvm::Instruction::FRem: op = spv::OpFRem; break;
      case llvm::Instruction::Shl: op = spv::OpShiftLeftLogical; break;
      case llvm::Instruction::LShr: op = spv::OpShiftRightLogical; break;
      case llvm::Instruction::AShr: op = spv::OpShiftRightArithmetic; break;
      case llvm::Instruction::And: op = is_bool ? spv::OpLogicalAnd : spv::OpBitwiseAnd; break;
      case llvm::Instruction::Or: op = is_bool ? spv::OpLogicalOr : spv::OpBitwiseOr; break;
      case llvm::Instruction::Xor: op = is_bool ? spv::OpLogicalNotEqual : spv::OpBitwiseXor; break;
      default: return spv.fail("unsupported binary operator");
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op, {rty, id, a, b});
      val_map[bo] = id;
      return spv.error.empty();
    }

    if (auto *ic = llvm::dyn_cast<llvm::ICmpInst>(&inst)) {
      llvm::Type *op_ty = ic->getOperand(0)->getType();
      if (op_ty->isPointerTy()) {
        auto pred = ic->getPredicate();
        if (pred != llvm::ICmpInst::ICMP_EQ && pred != llvm::ICmpInst::ICMP_NE) {
          return spv.fail("unsupported pointer icmp predicate");
        }
        llvm::Value *op0 = ic->getOperand(0);
        llvm::Value *op1 = ic->getOperand(1);
        uint32_t bty = spv.get_bool();
        uint32_t eq_id = 0;
        bool n0 = llvm::isa<llvm::ConstantPointerNull>(op0);
        bool n1 = llvm::isa<llvm::ConstantPointerNull>(op1);
        if (n0 && n1) {
          eq_id = spv.get_const_bool(true);
        } else if (n0) {
          eq_id = get_ptr_is_null(op1);
        } else if (n1) {
          eq_id = get_ptr_is_null(op0);
        } else {
          PtrInfo pa = resolve_ptr(op0);
          PtrInfo pb = resolve_ptr(op1);
          if (pa.kind == PtrKind::Invalid || pb.kind == PtrKind::Invalid) return false;
          if (pa.is_dual() || pb.is_dual()) {
            return spv.fail("pointer comparison on dual-root selected pointer not supported");
          }
          uint32_t same_addr = 0;
          if (pa.kind == pb.kind && pa.base_id == pb.base_id) {
            uint32_t oa = materialize_byte_off(pa);
            uint32_t ob = materialize_byte_off(pb);
            same_addr = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpIEqual, {bty, same_addr, oa, ob});
          } else if (pa.kind == PtrKind::Buffer && pb.kind == PtrKind::Buffer &&
                     pa.base_id < 16u && pb.base_id < 16u) {
            uint32_t ida = emit_pc_load(0u, pa.base_id);
            uint32_t idb = emit_pc_load(0u, pb.base_id);
            uint32_t same_id = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpIEqual, {bty, same_id, ida, idb});
            uint32_t off_a = emit_u32_add(emit_buf_pc_base_off(pa.base_id), materialize_byte_off(pa));
            uint32_t off_b = emit_u32_add(emit_buf_pc_base_off(pb.base_id), materialize_byte_off(pb));
            uint32_t same_off = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpIEqual, {bty, same_off, off_a, off_b});
            same_addr = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalAnd, {bty, same_addr, same_id, same_off});
          } else {
            same_addr = spv.get_const_bool(false);
          }
          uint32_t na = get_ptr_is_null(op0);
          uint32_t nb = get_ptr_is_null(op1);
          uint32_t cf = spv.get_const_bool(false);
          if (na == cf && nb == cf) {
            eq_id = same_addr;
          } else {
            uint32_t both_null = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalAnd, {bty, both_null, na, nb});
            uint32_t any_null = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalOr, {bty, any_null, na, nb});
            uint32_t neither_null = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalNot, {bty, neither_null, any_null});
            uint32_t nonnull_eq = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalAnd, {bty, nonnull_eq, neither_null, same_addr});
            eq_id = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpLogicalOr, {bty, eq_id, both_null, nonnull_eq});
          }
        }
        if (pred == llvm::ICmpInst::ICMP_NE) {
          uint32_t neq = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpLogicalNot, {bty, neq, eq_id});
          val_map[ic] = neq;
        } else {
          val_map[ic] = eq_id;
        }
        return spv.error.empty();
      }
      uint32_t a = 0, b = 0;
      if (op_ty->isIntegerTy(64) &&
          (is_i64_index_only(ic->getOperand(0)) || is_i64_index_only(ic->getOperand(1)))) {
        a = get_index_u32(ic->getOperand(0));
        b = get_index_u32(ic->getOperand(1));
      } else {
        a = get_val(ic->getOperand(0));
        b = get_val(ic->getOperand(1));
      }
      if (!a || !b) return false;
      bool is_bool = op_ty->getScalarType()->isIntegerTy(1);
      auto pred = ic->getPredicate();
      spv::Op op = spv::OpNop;
      switch (pred) {
      case llvm::ICmpInst::ICMP_EQ: op = is_bool ? spv::OpLogicalEqual : spv::OpIEqual; break;
      case llvm::ICmpInst::ICMP_NE: op = is_bool ? spv::OpLogicalNotEqual : spv::OpINotEqual; break;
      case llvm::ICmpInst::ICMP_UGT: op = spv::OpUGreaterThan; break;
      case llvm::ICmpInst::ICMP_UGE: op = spv::OpUGreaterThanEqual; break;
      case llvm::ICmpInst::ICMP_ULT: op = spv::OpULessThan; break;
      case llvm::ICmpInst::ICMP_ULE: op = spv::OpULessThanEqual; break;
      case llvm::ICmpInst::ICMP_SGT: op = spv::OpSGreaterThan; break;
      case llvm::ICmpInst::ICMP_SGE: op = spv::OpSGreaterThanEqual; break;
      case llvm::ICmpInst::ICMP_SLT: op = spv::OpSLessThan; break;
      case llvm::ICmpInst::ICMP_SLE: op = spv::OpSLessThanEqual; break;
      default: return spv.fail("unsupported icmp predicate");
      }
      if (is_bool && pred != llvm::ICmpInst::ICMP_EQ && pred != llvm::ICmpInst::ICMP_NE) {
        bool is_signed = (pred == llvm::ICmpInst::ICMP_SGT || pred == llvm::ICmpInst::ICMP_SGE ||
                          pred == llvm::ICmpInst::ICMP_SLT || pred == llvm::ICmpInst::ICMP_SLE);
        uint32_t u32 = spv.get_int(32);
        uint32_t cmp_ty = u32;
        uint32_t c_true = spv.get_const_u32(is_signed ? 0xFFFFFFFFu : 1u);
        uint32_t c_false = spv.get_const_u32(0u);
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(op_ty)) {
          unsigned n = vt->getNumElements();
          cmp_ty = spv.get_vec(u32, n);
          uint32_t v1 = spv.alloc_id(), v0 = spv.alloc_id();
          std::vector<uint32_t> a1 = {cmp_ty, v1}, a0 = {cmp_ty, v0};
          for (unsigned i = 0; i < n; i++) {
            a1.push_back(c_true);
            a0.push_back(c_false);
          }
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a1);
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a0);
          c_true = v1;
          c_false = v0;
        }
        uint32_t a32 = spv.alloc_id(), b32 = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {cmp_ty, a32, a, c_true, c_false});
        spv.emit_op(cur_body, spv::OpSelect, {cmp_ty, b32, b, c_true, c_false});
        a = a32;
        b = b32;
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op, {rty, id, a, b});
      val_map[ic] = id;
      return spv.error.empty();
    }

    if (auto *fc = llvm::dyn_cast<llvm::FCmpInst>(&inst)) {
      uint32_t a = get_val(fc->getOperand(0));
      uint32_t b = get_val(fc->getOperand(1));
      if (!a || !b) return false;
      if (fc->getOperand(0)->getType()->getScalarType()->isBFloatTy()) {
        a = emit_bf16_to_f32(a, fc->getOperand(0)->getType());
        b = emit_bf16_to_f32(b, fc->getOperand(1)->getType());
        if (!a || !b) return false;
      }
      spv::Op op = spv::OpNop;
      switch (fc->getPredicate()) {
      case llvm::FCmpInst::FCMP_FALSE:
        val_map[fc] = spv.get_null(rty);
        return true;
      case llvm::FCmpInst::FCMP_TRUE: {
        if (fc->getType()->isIntegerTy(1)) {
          val_map[fc] = spv.get_const_bool(true);
          return true;
        }
        auto *vt = llvm::cast<llvm::FixedVectorType>(fc->getType());
        std::vector<uint32_t> ones(vt->getNumElements(), spv.get_const_bool(true));
        uint32_t id = spv.alloc_id();
        std::vector<uint32_t> args = {rty, id};
        args.insert(args.end(), ones.begin(), ones.end());
        spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
        val_map[fc] = id;
        return true;
      }
      case llvm::FCmpInst::FCMP_OEQ: op = spv::OpFOrdEqual; break;
      case llvm::FCmpInst::FCMP_OGT: op = spv::OpFOrdGreaterThan; break;
      case llvm::FCmpInst::FCMP_OGE: op = spv::OpFOrdGreaterThanEqual; break;
      case llvm::FCmpInst::FCMP_OLT: op = spv::OpFOrdLessThan; break;
      case llvm::FCmpInst::FCMP_OLE: op = spv::OpFOrdLessThanEqual; break;
      case llvm::FCmpInst::FCMP_ONE: op = spv::OpFOrdNotEqual; break;
      case llvm::FCmpInst::FCMP_ORD: op = spv::OpOrdered; break;
      case llvm::FCmpInst::FCMP_UNO: op = spv::OpUnordered; break;
      case llvm::FCmpInst::FCMP_UEQ: op = spv::OpFUnordEqual; break;
      case llvm::FCmpInst::FCMP_UGT: op = spv::OpFUnordGreaterThan; break;
      case llvm::FCmpInst::FCMP_UGE: op = spv::OpFUnordGreaterThanEqual; break;
      case llvm::FCmpInst::FCMP_ULT: op = spv::OpFUnordLessThan; break;
      case llvm::FCmpInst::FCMP_ULE: op = spv::OpFUnordLessThanEqual; break;
      case llvm::FCmpInst::FCMP_UNE: op = spv::OpFUnordNotEqual; break;
      default: return spv.fail("unsupported fcmp predicate");
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op, {rty, id, a, b});
      val_map[fc] = id;
      return true;
    }

    if (auto *sel = llvm::dyn_cast<llvm::SelectInst>(&inst)) {
      uint32_t cond = get_val(sel->getCondition());
      uint32_t tv = get_val(sel->getTrueValue());
      uint32_t fv = get_val(sel->getFalseValue());
      if (!cond || !tv || !fv) return false;
      /* Scalar condition selecting vector -> splat cond to vector bool for SPIR-V 1.6 legality. */
      if (sel->getCondition()->getType()->isIntegerTy(1) && sel->getType()->isVectorTy()) {
        unsigned n = llvm::cast<llvm::FixedVectorType>(sel->getType())->getNumElements();
        uint32_t vb = spv.get_vec(spv.get_bool(), n);
        uint32_t splat = spv.alloc_id();
        std::vector<uint32_t> args = {vb, splat};
        for (unsigned i = 0; i < n; i++) args.push_back(cond);
        spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, args);
        cond = splat;
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, spv::OpSelect, {rty, id, cond, tv, fv});
      val_map[sel] = id;
      return true;
    }

    if (auto *cast = llvm::dyn_cast<llvm::CastInst>(&inst)) {
      llvm::Value *src_v = cast->getOperand(0);
      llvm::Type *sty = src_v->getType();
      llvm::Type *dty = cast->getType();
      if (cast->getOpcode() == llvm::Instruction::PtrToInt) {
        if (!dty->isIntegerTy()) return spv.fail("unsupported ptrtoint destination type");
        uint32_t dbits = dty->getIntegerBitWidth();
        if (dbits != 8u && dbits != 16u && dbits != 32u && dbits != 64u) {
          return spv.fail("unsupported ptrtoint integer width");
        }
        uint32_t off32 = 0, hi32 = 0;
        if (!emit_ptr_to_u32_pair(src_v, off32, hi32)) return false;
        if (dbits == 64u) {
          uint32_t u64 = spv.get_int(64);
          if (!u64) return false;
          uint32_t lo64 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpUConvert, {u64, lo64, off32});
          uint32_t hi64 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpUConvert, {u64, hi64, hi32});
          uint32_t sh = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpShiftLeftLogical, {u64, sh, hi64, spv.get_const_int(64, 32)});
          uint32_t addr64 = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseOr, {u64, addr64, sh, lo64});
          val_map[cast] = addr64;
          return spv.error.empty();
        }
        if (dbits == 32u) {
          val_map[cast] = off32;
          return spv.error.empty();
        }
        uint32_t res = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {rty, res, off32});
        val_map[cast] = res;
        return spv.error.empty();
      }
      if (cast->getOpcode() == llvm::Instruction::Trunc && is_i64_index_only(src_v)) {
        uint32_t u32_src = get_index_u32(src_v);
        if (!u32_src) return false;
        if (dty->isIntegerTy(32)) {
          val_map[cast] = u32_src;
          return true;
        }
        if (dty->isIntegerTy(1)) {
          uint32_t u32 = spv.get_int(32);
          uint32_t masked = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, masked, u32_src, spv.get_const_u32(1u)});
          uint32_t id = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpINotEqual, {rty, id, masked, spv.get_const_u32(0u)});
          val_map[cast] = id;
          return spv.error.empty();
        }
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpUConvert, {rty, id, u32_src});
        val_map[cast] = id;
        return spv.error.empty();
      }
      uint32_t src = get_val(src_v);
      if (!src) return false;

      /* Handle bool (i1) <-> int conversions without invalid OpUConvert/OpSConvert on OpTypeBool. */
      if (sty->getScalarType()->isIntegerTy(1) &&
          (cast->getOpcode() == llvm::Instruction::ZExt ||
           cast->getOpcode() == llvm::Instruction::SExt)) {
        uint32_t bw = dty->getScalarType()->getIntegerBitWidth();
        uint64_t one_bits = (cast->getOpcode() == llvm::Instruction::ZExt) ? 1ULL : ~0ULL;
        uint32_t c_one = spv.get_const_int(bw, one_bits);
        uint32_t c_zero = spv.get_const_int(bw, 0);
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(dty)) {
          unsigned n = vt->getNumElements();
          uint32_t v1 = spv.alloc_id(), v0 = spv.alloc_id();
          std::vector<uint32_t> a1 = {rty, v1}, a0 = {rty, v0};
          for (unsigned i = 0; i < n; i++) {
            a1.push_back(c_one);
            a0.push_back(c_zero);
          }
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a1);
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a0);
          c_one = v1;
          c_zero = v0;
        }
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {rty, id, src, c_one, c_zero});
        val_map[cast] = id;
        return true;
      }
      if (dty->getScalarType()->isIntegerTy(1) && cast->getOpcode() == llvm::Instruction::Trunc) {
        uint32_t bw = sty->getScalarType()->getIntegerBitWidth();
        uint32_t sty_spv = spv.map_llvm_type(sty);
        uint32_t c_one = spv.get_const_int(bw, 1);
        uint32_t c_zero = spv.get_const_int(bw, 0);
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(sty)) {
          unsigned n = vt->getNumElements();
          uint32_t v1 = spv.alloc_id(), v0 = spv.alloc_id();
          std::vector<uint32_t> a1 = {sty_spv, v1}, a0 = {sty_spv, v0};
          for (unsigned i = 0; i < n; i++) {
            a1.push_back(c_one);
            a0.push_back(c_zero);
          }
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a1);
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a0);
          c_one = v1;
          c_zero = v0;
        }
        uint32_t masked = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {sty_spv, masked, src, c_one});
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpINotEqual, {rty, id, masked, c_zero});
        val_map[cast] = id;
        return true;
      }

      /* Handle bool (i1) <-> float conversions without invalid OpConvert* on OpTypeBool. */
      if (sty->getScalarType()->isIntegerTy(1) &&
          (cast->getOpcode() == llvm::Instruction::UIToFP ||
           cast->getOpcode() == llvm::Instruction::SIToFP)) {
        llvm::Type *dsc = dty->getScalarType();
        if (!dsc->isHalfTy() && !dsc->isFloatTy() && !dsc->isBFloatTy()) {
          return spv.fail("unsupported bool-to-fp cast");
        }
        bool is_signed = (cast->getOpcode() == llvm::Instruction::SIToFP);
        uint32_t c_one = 0;
        uint32_t c_zero = 0;
        if (dsc->isBFloatTy()) {
          c_one = spv.get_const_int(16, is_signed ? 0xBF80ULL : 0x3F80ULL);
          c_zero = spv.get_const_int(16, 0u);
        } else {
          uint32_t bits = dsc->isHalfTy() ? 16u : 32u;
          uint64_t one_bits =
              bits == 16u ? (is_signed ? 0xBC00ULL : 0x3C00ULL)
                          : (is_signed ? 0xBF800000ULL : 0x3F800000ULL);
          c_one = spv.get_const_fp_bits(bits, one_bits);
          c_zero = spv.get_const_fp_bits(bits, 0u);
        }
        if (!c_one || !c_zero) return false;
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(dty)) {
          unsigned n = vt->getNumElements();
          uint32_t v1 = spv.alloc_id(), v0 = spv.alloc_id();
          std::vector<uint32_t> a1 = {rty, v1}, a0 = {rty, v0};
          for (unsigned i = 0; i < n; i++) {
            a1.push_back(c_one);
            a0.push_back(c_zero);
          }
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a1);
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a0);
          c_one = v1;
          c_zero = v0;
        }
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpSelect, {rty, id, src, c_one, c_zero});
        val_map[cast] = id;
        return spv.error.empty();
      }
      if (dty->getScalarType()->isIntegerTy(1) &&
          (cast->getOpcode() == llvm::Instruction::FPToUI ||
           cast->getOpcode() == llvm::Instruction::FPToSI)) {
        uint32_t fp_src = sty->getScalarType()->isBFloatTy() ? emit_bf16_to_f32(src, sty) : src;
        if (!fp_src) return false;
        uint32_t u32 = spv.get_int(32);
        uint32_t mid_ty = u32;
        uint32_t c_one = spv.get_const_u32(1u);
        uint32_t c_zero = spv.get_const_u32(0u);
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(dty)) {
          unsigned n = vt->getNumElements();
          mid_ty = spv.get_vec(u32, n);
          uint32_t v1 = spv.alloc_id(), v0 = spv.alloc_id();
          std::vector<uint32_t> a1 = {mid_ty, v1}, a0 = {mid_ty, v0};
          for (unsigned i = 0; i < n; i++) {
            a1.push_back(c_one);
            a0.push_back(c_zero);
          }
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a1);
          spv.emit_op_vec(cur_body, spv::OpCompositeConstruct, a0);
          c_one = v1;
          c_zero = v0;
        }
        uint32_t iv = spv.alloc_id();
        spv.emit_op(cur_body,
                    cast->getOpcode() == llvm::Instruction::FPToSI ? spv::OpConvertFToS
                                                                   : spv::OpConvertFToU,
                    {mid_ty, iv, fp_src});
        uint32_t masked = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpBitwiseAnd, {mid_ty, masked, iv, c_one});
        uint32_t id = spv.alloc_id();
        spv.emit_op(cur_body, spv::OpINotEqual, {rty, id, masked, c_zero});
        val_map[cast] = id;
        return spv.error.empty();
      }

      if (sty == dty && (cast->getOpcode() == llvm::Instruction::Trunc ||
                         cast->getOpcode() == llvm::Instruction::ZExt ||
                         cast->getOpcode() == llvm::Instruction::SExt ||
                         cast->getOpcode() == llvm::Instruction::BitCast)) {
        val_map[cast] = src;
        return true;
      }

      /* Software IEEE-754 bfloat16 cast lowering (bfloat is represented as u16 in SPIR-V). */
      bool src_is_bf16 = sty->getScalarType()->isBFloatTy();
      bool dst_is_bf16 = dty->getScalarType()->isBFloatTy();
      if (src_is_bf16 || dst_is_bf16) {
        if (cast->getOpcode() == llvm::Instruction::BitCast) {
          uint32_t sty_spv = spv.map_llvm_type(sty);
          if (!sty_spv) return false;
          if (sty_spv == rty) {
            val_map[cast] = src;
            return true;
          }
          uint32_t id = spv.alloc_id();
          spv.emit_op(cur_body, spv::OpBitcast, {rty, id, src});
          val_map[cast] = id;
          return spv.error.empty();
        }
        uint32_t lanes = 1u;
        if (auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(dty)) lanes = vt->getNumElements();
        llvm::Type *f32_llvm = lanes > 1 ? static_cast<llvm::Type *>(
                                               llvm::FixedVectorType::get(llvm::Type::getFloatTy(mod.getContext()), lanes))
                                         : static_cast<llvm::Type *>(llvm::Type::getFloatTy(mod.getContext()));
        uint32_t f32_spv = spv.map_llvm_type(f32_llvm);
        if (!f32_spv) return false;
        if (src_is_bf16) {
          uint32_t f32_val = emit_bf16_to_f32(src, sty);
          if (!f32_val) return false;
          if (cast->getOpcode() == llvm::Instruction::FPExt && dty->getScalarType()->isFloatTy()) {
            val_map[cast] = f32_val;
            return spv.error.empty();
          }
          spv::Op op = spv::OpNop;
          switch (cast->getOpcode()) {
          case llvm::Instruction::FPExt:
          case llvm::Instruction::FPTrunc: op = spv::OpFConvert; break;
          case llvm::Instruction::FPToUI: op = spv::OpConvertFToU; break;
          case llvm::Instruction::FPToSI: op = spv::OpConvertFToS; break;
          default: return spv.fail("unsupported bfloat source cast in AIR");
          }
          uint32_t id = spv.alloc_id();
          spv.emit_op(cur_body, op, {rty, id, f32_val});
          val_map[cast] = id;
          return spv.error.empty();
        }
        /* Destination is bfloat. */
        uint32_t f32_val = src;
        if (!sty->getScalarType()->isFloatTy()) {
          spv::Op op = spv::OpNop;
          switch (cast->getOpcode()) {
          case llvm::Instruction::FPExt:
          case llvm::Instruction::FPTrunc: op = spv::OpFConvert; break;
          case llvm::Instruction::UIToFP: op = spv::OpConvertUToF; break;
          case llvm::Instruction::SIToFP: op = spv::OpConvertSToF; break;
          default: return spv.fail("unsupported bfloat destination cast in AIR");
          }
          f32_val = spv.alloc_id();
          spv.emit_op(cur_body, op, {f32_spv, f32_val, src});
        }
        uint32_t bf = emit_f32_to_bf16(f32_val, f32_llvm);
        if (!bf) return false;
        val_map[cast] = bf;
        return spv.error.empty();
      }

      spv::Op op = spv::OpNop;
      switch (cast->getOpcode()) {
      case llvm::Instruction::Trunc:
      case llvm::Instruction::ZExt: op = spv::OpUConvert; break;
      case llvm::Instruction::SExt: op = spv::OpSConvert; break;
      case llvm::Instruction::FPTrunc:
      case llvm::Instruction::FPExt: op = spv::OpFConvert; break;
      case llvm::Instruction::FPToUI: op = spv::OpConvertFToU; break;
      case llvm::Instruction::FPToSI: op = spv::OpConvertFToS; break;
      case llvm::Instruction::UIToFP: op = spv::OpConvertUToF; break;
      case llvm::Instruction::SIToFP: op = spv::OpConvertSToF; break;
      case llvm::Instruction::BitCast:
        if (sty->getScalarType()->isIntegerTy(1) || dty->getScalarType()->isIntegerTy(1)) {
          return spv.fail("bitcast involving i1 is not valid in SPIR-V");
        }
        op = spv::OpBitcast;
        break;
      default: return spv.fail("unsupported cast instruction in AIR");
      }
      uint32_t id = spv.alloc_id();
      spv.emit_op(cur_body, op, {rty, id, src});
      val_map[cast] = id;
      return spv.error.empty();
    }

    if (auto *ee = llvm::dyn_cast<llvm::ExtractElementInst>(&inst)) {
      auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ee->getVectorOperand()->getType());
      if (!vt) return spv.fail("extractelement on non-fixed vector");
      uint32_t vec = get_val(ee->getVectorOperand());
      if (!vec) return false;
      uint32_t id = spv.alloc_id();
      if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(ee->getIndexOperand())) {
        uint32_t idx = 0;
        if (!try_get_u32(ci, idx) || idx >= vt->getNumElements()) {
          return spv.fail("extractelement constant index out of bounds");
        }
        spv.emit_op(cur_body, spv::OpCompositeExtract, {rty, id, vec, idx});
      } else {
        uint32_t idx = get_index_u32(ee->getIndexOperand());
        if (!idx) return false;
        spv.emit_op(cur_body, spv::OpVectorExtractDynamic, {rty, id, vec, idx});
      }
      val_map[ee] = id;
      return spv.error.empty();
    }

    if (auto *ie = llvm::dyn_cast<llvm::InsertElementInst>(&inst)) {
      auto *vt = llvm::dyn_cast<llvm::FixedVectorType>(ie->getOperand(0)->getType());
      if (!vt) return spv.fail("insertelement on non-fixed vector");
      uint32_t vec = get_val(ie->getOperand(0));
      uint32_t elem = get_val(ie->getOperand(1));
      if (!vec || !elem) return false;
      uint32_t id = spv.alloc_id();
      if (auto *ci = llvm::dyn_cast<llvm::ConstantInt>(ie->getOperand(2))) {
        uint32_t idx = 0;
        if (!try_get_u32(ci, idx) || idx >= vt->getNumElements()) {
          return spv.fail("insertelement constant index out of bounds");
        }
        spv.emit_op(cur_body, spv::OpCompositeInsert, {rty, id, elem, vec, idx});
      } else {
        uint32_t idx = get_index_u32(ie->getOperand(2));
        if (!idx) return false;
        spv.emit_op(cur_body, spv::OpVectorInsertDynamic, {rty, id, vec, elem, idx});
      }
      val_map[ie] = id;
      return spv.error.empty();
    }

    if (auto *sv = llvm::dyn_cast<llvm::ShuffleVectorInst>(&inst)) {
      uint32_t v1 = get_val(sv->getOperand(0));
      uint32_t v2 = get_val(sv->getOperand(1));
      if (!v1 || !v2) return false;
      uint32_t id = spv.alloc_id();
      std::vector<uint32_t> args = {rty, id, v1, v2};
      for (int m : sv->getShuffleMask()) {
        args.push_back(m < 0 ? 0xFFFFFFFFu : static_cast<uint32_t>(m));
      }
      spv.emit_op_vec(cur_body, spv::OpVectorShuffle, args);
      val_map[sv] = id;
      return true;
    }

    if (auto *ev = llvm::dyn_cast<llvm::ExtractValueInst>(&inst)) {
      uint32_t agg = get_val(ev->getAggregateOperand());
      if (!agg) return false;
      uint32_t id = spv.alloc_id();
      std::vector<uint32_t> args = {rty, id, agg};
      for (unsigned idx : ev->indices()) args.push_back(idx);
      spv.emit_op_vec(cur_body, spv::OpCompositeExtract, args);
      val_map[ev] = id;
      return true;
    }

    if (auto *iv = llvm::dyn_cast<llvm::InsertValueInst>(&inst)) {
      uint32_t agg = get_val(iv->getAggregateOperand());
      uint32_t elem = get_val(iv->getInsertedValueOperand());
      if (!agg || !elem) return false;
      uint32_t id = spv.alloc_id();
      std::vector<uint32_t> args = {rty, id, elem, agg};
      for (unsigned idx : iv->indices()) args.push_back(idx);
      spv.emit_op_vec(cur_body, spv::OpCompositeInsert, args);
      val_map[iv] = id;
      return true;
    }

    return spv.fail(std::string("unsupported LLVM instruction: ") + inst.getOpcodeName());
  }

  /* Guarantee SPIR-V 1.6 structured CFG invariants via targeted IR block splits. */
  bool canonicalize_cfg_for_spirv() {
    /* 1. Compute LoopInfo and normalize every loop (unique dedicated merge + dedicated continue). */
    llvm::DominatorTree DT(fn);
    llvm::LoopInfo LI(DT);

    auto loops = LI.getLoopsInPreorder();
    std::reverse(loops.begin(), loops.end()); /* innermost first */

    std::unordered_set<llvm::BasicBlock *> claimed_targets;
    std::unordered_map<llvm::BasicBlock *, llvm::BasicBlock *> loop_merge_map;
    std::unordered_map<llvm::BasicBlock *, llvm::BasicBlock *> loop_cont_map;

    for (llvm::Loop *L : loops) {
      llvm::BasicBlock *H = L->getHeader();
      llvm::BasicBlock *Latch = L->getLoopLatch();
      if (!H || !Latch) return spv.fail("irreducible or multi-latch loop in AIR");

      llvm::SmallVector<llvm::BasicBlock *, 4> exits;
      L->getUniqueExitBlocks(exits);
      if (exits.empty()) return spv.fail("infinite loop without exit block");
      if (exits.size() > 1) return spv.fail("multi-exit loop after StructurizeCFG");
      llvm::BasicBlock *Exit = exits[0];

      /* Give this loop an exclusive merge block if Exit is shared, already claimed, or an enclosing loop's latch. */
      bool is_parent_latch = false;
      for (llvm::Loop *PL = L->getParentLoop(); PL; PL = PL->getParentLoop()) {
        if (Exit == PL->getLoopLatch()) {
          is_parent_latch = true;
          break;
        }
      }
      llvm::SmallVector<llvm::BasicBlock *, 8> in_preds;
      bool has_ext_pred = false;
      for (llvm::BasicBlock *P : llvm::predecessors(Exit)) {
        if (L->contains(P)) in_preds.push_back(P);
        else has_ext_pred = true;
      }
      if (has_ext_pred || is_parent_latch || claimed_targets.count(Exit) || Exit == H || Exit == Latch) {
        Exit = llvm::SplitBlockPredecessors(Exit, in_preds, ".spv.loop.merge", &DT, &LI);
      }
      claimed_targets.insert(Exit);

      /* Self-loop (Header == Latch): split backedge into dedicated continue block. */
      if (Latch == H) {
        auto *bi = llvm::dyn_cast<llvm::BranchInst>(H->getTerminator());
        if (!bi) return spv.fail("unsupported terminator in self-loop header");
        llvm::BasicBlock *Cont =
            llvm::BasicBlock::Create(mod.getContext(), ".spv.loop.cont", &fn, Exit);
        llvm::BranchInst::Create(H, Cont);
        for (unsigned s = 0; s < bi->getNumSuccessors(); s++) {
          if (bi->getSuccessor(s) == H) bi->setSuccessor(s, Cont);
        }
        for (llvm::PHINode &phi : H->phis()) {
          for (unsigned k = 0; k < phi.getNumIncomingValues(); k++) {
            if (phi.getIncomingBlock(k) == H) phi.setIncomingBlock(k, Cont);
          }
        }
        L->addBasicBlockToLoop(Cont, LI);
        Latch = Cont;
      }

      /* If Header's conditional branch keeps BOTH distinct arms inside L (inner if at header),
         or Header contains a multi-root selected pointer memory op (which emits an inline
         OpSelectionMerge diamond), split Header after PHIs so Header emits OpLoopMerge + OpBranch Body cleanly. */
      bool need_header_split = false;
      if (auto *hbi = llvm::dyn_cast<llvm::BranchInst>(H->getTerminator())) {
        if (hbi->isConditional() && hbi->getSuccessor(0) != hbi->getSuccessor(1) &&
            L->contains(hbi->getSuccessor(0)) && L->contains(hbi->getSuccessor(1))) {
          need_header_split = true;
        }
      }
      if (!need_header_split) {
        std::vector<std::pair<PtrKind, uint32_t>> hroots;
        for (llvm::Instruction &hi : *H) {
          const llvm::Value *hptr = nullptr;
          if (auto *li = llvm::dyn_cast<llvm::LoadInst>(&hi)) {
            hptr = li->getPointerOperand();
          } else if (auto *si = llvm::dyn_cast<llvm::StoreInst>(&hi)) {
            hptr = si->getPointerOperand();
          } else if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&hi)) {
            if (llvm::Function *cf = ci->getCalledFunction()) {
              llvm::StringRef cn = cf->getName();
              if ((cn.starts_with("llvm.memset.") || cn.starts_with("llvm.memcpy.") ||
                   cn.starts_with("llvm.memmove.")) && ci->arg_size() >= 3) {
                if (collect_ptr_roots(ci->getArgOperand(0), hroots) && hroots.size() > 1) {
                  need_header_split = true;
                  break;
                }
                if (!cn.starts_with("llvm.memset.") &&
                    collect_ptr_roots(ci->getArgOperand(1), hroots) && hroots.size() > 1) {
                  need_header_split = true;
                  break;
                }
              }
            }
          }
          if (hptr && collect_ptr_roots(hptr, hroots) && hroots.size() > 1) {
            need_header_split = true;
            break;
          }
        }
      }
      if (need_header_split) {
        llvm::Instruction *split_pt = H->getFirstNonPHI();
        llvm::BasicBlock *Body = H->splitBasicBlock(split_pt, ".spv.loop.body");
        L->addBasicBlockToLoop(Body, LI);
      }

      claimed_targets.insert(Latch);
      loop_merge_map[H] = Exit;
      loop_cont_map[H] = Latch;
    }

    /* Recompute fresh DT, LI, PDT after loop splits. */
    DT.recalculate(fn);
    LI.releaseMemory();
    LI.analyze(DT);
    llvm::PostDominatorTree PDT(fn);

    /* Record loop merges and continue latches. */
    for (auto &[H, Merge] : loop_merge_map) {
      llvm::BasicBlock *Cont = loop_cont_map[H];
      merges[H] = MergeInfo{true, Merge, Cont};
      if (auto *cbi = llvm::dyn_cast<llvm::BranchInst>(Cont->getTerminator())) {
        if (cbi->isConditional() &&
            ((cbi->getSuccessor(0) == H && cbi->getSuccessor(1) == Merge) ||
             (cbi->getSuccessor(1) == H && cbi->getSuccessor(0) == Merge))) {
          continue_latches.insert(Cont);
        }
      }
    }

    /* 2. Assign exclusive OpSelectionMerge targets in reverse postorder (dominators/outer first),
          so inner selections sharing a post-dominator split strictly inside outer constructs. */
    std::vector<llvm::BasicBlock *> work_bbs;
    {
      std::unordered_set<llvm::BasicBlock *> vis;
      std::vector<std::pair<llvm::BasicBlock *, bool>> stk;
      stk.push_back({&fn.getEntryBlock(), false});
      while (!stk.empty()) {
        auto [bb, expanded] = stk.back();
        stk.pop_back();
        if (expanded) {
          work_bbs.push_back(bb);
          continue;
        }
        if (!bb || !vis.insert(bb).second) continue;
        stk.push_back({bb, true});
        llvm::Instruction *term = bb->getTerminator();
        for (unsigned i = term->getNumSuccessors(); i > 0; i--) {
          llvm::BasicBlock *succ = term->getSuccessor(i - 1);
          if (!vis.count(succ)) stk.push_back({succ, false});
        }
      }
      std::reverse(work_bbs.begin(), work_bbs.end());
    }

    for (llvm::BasicBlock *B : work_bbs) {
      if (merges.count(B) || continue_latches.count(B)) continue;
      auto *bi = llvm::dyn_cast<llvm::BranchInst>(B->getTerminator());
      if (!bi || !bi->isConditional() || bi->getSuccessor(0) == bi->getSuccessor(1)) continue;

      llvm::Loop *L = LI.getLoopFor(B);
      llvm::BasicBlock *loop_merge = L ? loop_merge_map[L->getHeader()] : nullptr;
      llvm::BasicBlock *loop_cont = L ? loop_cont_map[L->getHeader()] : nullptr;

      llvm::BasicBlock *M = nullptr;
      if (auto *pnode = PDT.getNode(B)) {
        if (auto *idom = pnode->getIDom()) M = idom->getBlock();
      }
      /* If one branch breaks/continues out of L, the in-loop successor is the selection merge. */
      if (!M || (L && LI.getLoopFor(M) != L) || M == loop_merge || M == loop_cont) {
        llvm::BasicBlock *s0 = bi->getSuccessor(0);
        llvm::BasicBlock *s1 = bi->getSuccessor(1);
        bool s0_out = L && (!L->contains(s0) || s0 == loop_merge || s0 == loop_cont);
        bool s1_out = L && (!L->contains(s1) || s1 == loop_merge || s1 == loop_cont);
        if (s0_out && !s1_out) {
          M = s1;
        } else if (s1_out && !s0_out) {
          M = s0;
        }
      }
      if (!M) return spv.fail("unable to determine structured selection merge block");

      /* Ensure M is exclusive (not shared with another merge or loop continue/merge) and stays inside L. */
      if (claimed_targets.count(M) || M == B || M == loop_merge || M == loop_cont ||
          (L && !L->contains(M))) {
        llvm::SmallVector<llvm::BasicBlock *, 8> dom_preds;
        for (llvm::BasicBlock *P : llvm::predecessors(M)) {
          if (DT.dominates(B, P)) dom_preds.push_back(P);
        }
        if (dom_preds.empty()) {
          return spv.fail("cannot split exclusive selection merge block");
        }
        M = llvm::SplitBlockPredecessors(M, dom_preds, ".spv.sel.merge", &DT, &LI);
        PDT.recalculate(fn);
        if (!M || claimed_targets.count(M) || M == B || M == loop_merge || M == loop_cont) {
          return spv.fail("failed to create exclusive selection merge block");
        }
      }
      claimed_targets.insert(M);
      merges[B] = MergeInfo{false, M, nullptr};
    }

    if (fn.size() > kMaxBlocks) return spv.fail("CFG block count exceeds budget");
    return true;
  }

  /* Compute SPIR-V valid RPO iteratively (dominators before dominated; loop body before exit). */
  void compute_rpo(std::vector<llvm::BasicBlock *> &order) {
    std::unordered_set<llvm::BasicBlock *> vis;
    std::vector<llvm::BasicBlock *> post;
    std::vector<std::pair<llvm::BasicBlock *, bool>> stk;
    stk.push_back({&fn.getEntryBlock(), false});
    while (!stk.empty()) {
      auto [bb, expanded] = stk.back();
      stk.pop_back();
      if (expanded) {
        post.push_back(bb);
        continue;
      }
      if (!bb || !vis.insert(bb).second) continue;
      stk.push_back({bb, true});

      /* Push successors in reverse order so merge/exit blocks are visited FIRST in DFS
         (and therefore appear AFTER loop/selection bodies in Reverse Post-Order). */
      std::vector<llvm::BasicBlock *> children;
      if (auto it = merges.find(bb); it != merges.end()) {
        if (it->second.merge_bb) children.push_back(it->second.merge_bb);
        if (it->second.is_loop && it->second.continue_bb) children.push_back(it->second.continue_bb);
      }
      llvm::Instruction *term = bb->getTerminator();
      for (unsigned i = 0; i < term->getNumSuccessors(); i++) {
        children.push_back(term->getSuccessor(i));
      }
      for (auto rit = children.rbegin(); rit != children.rend(); ++rit) {
        if (*rit && !vis.count(*rit)) stk.push_back({*rit, false});
      }
    }
    order.assign(post.rbegin(), post.rend());
  }

  bool compile(air2spv_result_t *out) {
    if (!setup_memory_objects()) return false;
    if (!canonicalize_cfg_for_spirv()) return false;

    std::vector<llvm::BasicBlock *> order;
    compute_rpo(order);
    if (order.empty()) return spv.fail("empty function CFG");
    compute_narrowed_i64_set(order);

    /* Assign SPIR-V block labels & pre-allocate PHI result IDs (including pointer PHIs). */
    for (llvm::BasicBlock *bb : order) {
      bb_label[bb] = spv.alloc_id();
    }
    if (!order[0]->phis().empty()) {
      return spv.fail("PHI node in entry block is not valid in SPIR-V");
    }
    TraceCache phi_trace_cache;
    for (llvm::BasicBlock *bb : order) {
      for (llvm::PHINode &phi : bb->phis()) {
        if (phi.getType()->isPointerTy()) {
          PtrKind pk = PtrKind::Invalid;
          uint32_t base = 0;
          PtrInfo info{};
          uint32_t off_id = spv.alloc_id();
          if (trace_ptr_root(&phi, pk, base, phi_trace_cache)) {
            info = PtrInfo{pk, base, off_id, 0, 0};
          } else {
            std::vector<std::pair<PtrKind, uint32_t>> roots;
            if (!collect_ptr_roots(&phi, roots) || roots.size() != 2) {
              return spv.fail("pointer PHI merges incompatible memory objects");
            }
            auto root_key = [](const std::pair<PtrKind, uint32_t> &r) {
              return std::make_pair(static_cast<uint8_t>(r.first), r.second);
            };
            if (root_key(roots[1]) < root_key(roots[0])) {
              std::swap(roots[0], roots[1]);
            }
            uint32_t alt_off_id = spv.alloc_id();
            uint32_t sel_id = spv.alloc_id();
            info = PtrInfo{roots[0].first, roots[0].second, off_id, 0, 0,
                           sel_id, roots[1].first, roots[1].second, alt_off_id};
          }
          ptr_phi_info[&phi] = info;
          ptr_map[&phi] = info;
          phi_ids[&phi] = off_id;
          uint32_t null_phi_id = spv.alloc_id();
          ptr_phi_null_ids[&phi] = null_phi_id;
          ptr_null_map[&phi] = null_phi_id;
        } else if (phi.getType()->isIntegerTy(64) && is_i64_index_only(&phi)) {
          uint32_t id = spv.alloc_id();
          narrowed_i64_phis.insert(&phi);
          narrowed_i64_vals[&phi] = id;
          phi_ids[&phi] = id;
        } else {
          uint32_t ty = spv.map_llvm_type(phi.getType());
          if (!ty) return false;
          uint32_t id = spv.alloc_id();
          val_map[&phi] = id;
          phi_ids[&phi] = id;
        }
      }
    }

    /* Emit per-block instruction bodies in RPO order. */
    std::unordered_map<llvm::BasicBlock *, std::vector<uint32_t>> bb_bodies;
    std::unordered_map<llvm::BasicBlock *, std::vector<uint32_t>> bb_phis;

    for (size_t bi = 0; bi < order.size(); bi++) {
      llvm::BasicBlock *bb = order[bi];
      cur_bb = bb;
      cur_exit_label = bb_label[bb];
      cur_body.clear();

      /* Entry block: load builtin kernel arguments and sub-aligned SSBO base offsets. */
      if (bi == 0) {
        uint32_t salign = spv.hw_caps.ssbo_offset_align;
        if (salign > 1u && (salign & (salign - 1u)) == 0) {
          uint32_t mask = spv.get_const_u32(salign - 1u);
          uint32_t u32 = spv.get_int(32);
          for (uint32_t s = 0; s < 16u; s++) {
            if (!buffers[s].used) continue;
            uint32_t raw_off = emit_pc_load(1u, s);
            uint32_t sub_off = spv.alloc_id();
            spv.emit_op(cur_body, spv::OpBitwiseAnd, {u32, sub_off, raw_off, mask});
            buf_sub_off_ids[s] = sub_off;
          }
        }
        for (unsigned a = 0; a < fn.arg_size(); a++) {
          const KernelArgMeta &am = arg_meta[a];
          if (am.builtin != BuiltinKind::None) {
            uint32_t v = load_builtin_for_arg(am.builtin, fn.getArg(a)->getType());
            if (!v) return false;
            val_map[fn.getArg(a)] = v;
          }
        }
      }

      for (llvm::Instruction &inst : *bb) {
        if (!emit_inst(inst)) return false;
        if (spv.types_consts_vars.size() + cur_body.size() > kMaxSpirvWords) {
          return spv.fail("SPIR-V output exceeds word budget");
        }
      }

      /* Emit OpLoopMerge / OpSelectionMerge + terminator. */
      if (auto mit = merges.find(bb); mit != merges.end()) {
        if (mit->second.is_loop) {
          spv.emit_op(cur_body, spv::OpLoopMerge,
                      {bb_label[mit->second.merge_bb], bb_label[mit->second.continue_bb], 0u});
        } else {
          spv.emit_op(cur_body, spv::OpSelectionMerge, {bb_label[mit->second.merge_bb], 0u});
        }
      }

      llvm::Instruction *term = bb->getTerminator();
      if (auto *br = llvm::dyn_cast<llvm::BranchInst>(term)) {
        if (br->isUnconditional() || br->getSuccessor(0) == br->getSuccessor(1)) {
          spv.emit_op(cur_body, spv::OpBranch, {bb_label[br->getSuccessor(0)]});
        } else {
          uint32_t cond = get_val(br->getCondition());
          if (!cond) return false;
          spv.emit_op(cur_body, spv::OpBranchConditional,
                      {cond, bb_label[br->getSuccessor(0)], bb_label[br->getSuccessor(1)]});
        }
      } else if (llvm::isa<llvm::ReturnInst>(term)) {
        spv.emit_op(cur_body, spv::OpReturn, {});
      } else if (llvm::isa<llvm::UnreachableInst>(term)) {
        spv.emit_op(cur_body, spv::OpUnreachable, {});
      } else {
        return spv.fail("unsupported terminator instruction");
      }

      bb_exit_label[bb] = cur_exit_label;
      bb_bodies[bb] = std::move(cur_body);
    }
    cur_bb = nullptr;

    /* Populate OpPhi instructions now that all block instructions (including backedges) exist. */
    for (llvm::BasicBlock *bb : order) {
      std::vector<uint32_t> phi_words;
      for (llvm::PHINode &phi : bb->phis()) {
        uint32_t id = phi_ids[&phi];
        bool is_narrow_i64 = narrowed_i64_phis.count(&phi) != 0;
        auto pit = phi.getType()->isPointerTy() ? ptr_phi_info.find(&phi) : ptr_phi_info.end();
        if (pit != ptr_phi_info.end() && pit->second.is_dual()) {
          const PtrInfo &dinfo = pit->second;
          uint32_t u32 = spv.get_int(32);
          uint32_t bty = spv.get_bool();
          std::vector<uint32_t> o0_args = {u32, dinfo.byte_off_id};
          std::vector<uint32_t> o1_args = {u32, dinfo.alt_byte_off_id};
          std::vector<uint32_t> sel_args = {bty, dinfo.sel_cond_id};
          struct DualInc { uint32_t o0, o1, sel; };
          std::unordered_map<uint32_t, DualInc> seen_pred;
          for (unsigned k = 0; k < phi.getNumIncomingValues(); k++) {
            llvm::BasicBlock *pred = phi.getIncomingBlock(k);
            auto lit = bb_exit_label.find(pred);
            if (lit == bb_exit_label.end()) continue;
            llvm::Value *inc = phi.getIncomingValue(k);
            DualInc di{0, 0, 0};
            if (llvm::isa<llvm::UndefValue>(inc) || llvm::isa<llvm::PoisonValue>(inc)) {
              di = {spv.get_undef(u32), spv.get_undef(u32), spv.get_undef(bty)};
            } else if (llvm::isa<llvm::ConstantPointerNull>(inc)) {
              di = {spv.get_const_u32(0), spv.get_const_u32(0), spv.get_const_bool(true)};
            } else {
              cur_body.clear();
              PtrInfo p = resolve_ptr(inc);
              if (p.kind == PtrKind::Invalid) return false;
              if (!p.is_dual()) {
                uint32_t off_v = materialize_byte_off(p);
                if (!cur_body.empty()) {
                  return spv.fail("non-materialized constant GEP in dual pointer PHI");
                }
                if (p.kind == dinfo.kind && p.base_id == dinfo.base_id) {
                  di = {off_v, spv.get_const_u32(0), spv.get_const_bool(true)};
                } else if (p.kind == dinfo.alt_kind && p.base_id == dinfo.alt_base_id) {
                  di = {spv.get_const_u32(0), off_v, spv.get_const_bool(false)};
                } else {
                  return spv.fail("pointer PHI merges more than two memory objects");
                }
              } else {
                if (p.kind == dinfo.kind && p.base_id == dinfo.base_id &&
                    p.alt_kind == dinfo.alt_kind && p.alt_base_id == dinfo.alt_base_id) {
                  uint32_t o0 = materialize_byte_off(p.primary_arm());
                  uint32_t o1 = materialize_byte_off(p.alt_arm());
                  if (!cur_body.empty()) {
                    return spv.fail("non-materialized constant GEP in dual pointer PHI");
                  }
                  di = {o0, o1, p.sel_cond_id};
                } else {
                  return spv.fail("incompatible dual pointer incoming value in pointer PHI");
                }
              }
            }
            auto sit = seen_pred.find(lit->second);
            if (sit != seen_pred.end()) {
              if (sit->second.o0 != di.o0 || sit->second.o1 != di.o1 || sit->second.sel != di.sel) {
                return spv.fail("conflicting PHI values for duplicate predecessor");
              }
              continue;
            }
            seen_pred[lit->second] = di;
            o0_args.push_back(di.o0);
            o0_args.push_back(lit->second);
            o1_args.push_back(di.o1);
            o1_args.push_back(lit->second);
            sel_args.push_back(di.sel);
            sel_args.push_back(lit->second);
          }
          if (seen_pred.empty()) {
            return spv.fail("PHI node has no reachable predecessors");
          }
          spv.emit_op_vec(phi_words, spv::OpPhi, o0_args);
          spv.emit_op_vec(phi_words, spv::OpPhi, o1_args);
          spv.emit_op_vec(phi_words, spv::OpPhi, sel_args);
        } else {
        uint32_t ty = (phi.getType()->isPointerTy() || is_narrow_i64)
                          ? spv.get_int(32)
                          : spv.map_llvm_type(phi.getType());
        std::vector<uint32_t> args = {ty, id};
        std::unordered_map<uint32_t, uint32_t> seen_pred;
        for (unsigned k = 0; k < phi.getNumIncomingValues(); k++) {
          llvm::BasicBlock *pred = phi.getIncomingBlock(k);
          auto lit = bb_exit_label.find(pred);
          if (lit == bb_exit_label.end()) continue; /* skip unreachable pred */
          llvm::Value *inc = phi.getIncomingValue(k);
          uint32_t inc_id = 0;
          if (phi.getType()->isPointerTy()) {
            if (llvm::isa<llvm::UndefValue>(inc) || llvm::isa<llvm::PoisonValue>(inc)) {
              inc_id = spv.get_undef(spv.get_int(32));
            } else if (llvm::isa<llvm::ConstantPointerNull>(inc)) {
              inc_id = spv.get_const_u32(0);
            } else {
              cur_body.clear();
              PtrInfo p = resolve_ptr(inc);
              if (p.kind == PtrKind::Invalid || p.is_dual()) return false;
              inc_id = materialize_byte_off(p);
              if (!cur_body.empty()) {
                return spv.fail("non-materialized constant GEP in pointer PHI");
              }
            }
          } else if (is_narrow_i64) {
            cur_body.clear();
            inc_id = get_index_u32(inc);
            if (!inc_id || !cur_body.empty()) {
              return spv.fail("non-materialized expression in narrowed i64 PHI");
            }
          } else {
            inc_id = get_val(inc);
            if (!inc_id) return false;
          }
          auto sit = seen_pred.find(lit->second);
          if (sit != seen_pred.end()) {
            if (sit->second != inc_id) {
              return spv.fail("conflicting PHI values for duplicate predecessor");
            }
            continue;
          }
          seen_pred[lit->second] = inc_id;
          args.push_back(inc_id);
          args.push_back(lit->second);
        }
        if (seen_pred.empty()) {
          return spv.fail("PHI node has no reachable predecessors");
        }
        spv.emit_op_vec(phi_words, spv::OpPhi, args);
        }
        if (phi.getType()->isPointerTy()) {
          auto nit = ptr_phi_null_ids.find(&phi);
          if (nit != ptr_phi_null_ids.end()) {
            std::vector<uint32_t> nargs = {spv.get_bool(), nit->second};
            std::unordered_map<uint32_t, uint32_t> nseen;
            for (unsigned k = 0; k < phi.getNumIncomingValues(); k++) {
              llvm::BasicBlock *pred = phi.getIncomingBlock(k);
              auto lit = bb_exit_label.find(pred);
              if (lit == bb_exit_label.end()) continue;
              uint32_t nid = get_ptr_is_null(phi.getIncomingValue(k));
              if (nseen.count(lit->second)) continue;
              nseen[lit->second] = nid;
              nargs.push_back(nid);
              nargs.push_back(lit->second);
            }
            spv.emit_op_vec(phi_words, spv::OpPhi, nargs);
          }
        }
        if (!spv.error.empty()) return false;
      }
      bb_phis[bb] = std::move(phi_words);
    }

    if (!spv.error.empty()) return false;
    if (spv.annotations.size() > kMaxSpirvWords ||
        spv.types_consts_vars.size() > kMaxSpirvWords - spv.annotations.size()) {
      return spv.fail("SPIR-V global section exceeds size cap");
    }

    /* Assemble final SPIR-V 1.6 binary in exact spec section order. */
    uint32_t void_ty = spv.get_void();
    uint32_t fn_ty = spv.get_fn_void();
    std::vector<uint32_t> words;
    words.reserve(2048);

    /* 1. Header */
    words.push_back(spv::MagicNumber);
    words.push_back(kSpvVersion16);
    words.push_back(0u);
    words.push_back(spv.next_id);
    words.push_back(0u);

    /* 2. Capabilities */
    for (spv::Capability c : spv.caps) {
      spv.emit_op(words, spv::OpCapability, {static_cast<uint32_t>(c)});
    }

    /* 3. ExtInstImport ("GLSL.std.450") */
    {
      std::vector<uint32_t> args = {spv.glsl_ext_id};
      SpvEmitter::append_string(args, "GLSL.std.450");
      spv.emit_op_vec(words, spv::OpExtInstImport, args);
    }

    /* 4. MemoryModel */
    spv.emit_op(words, spv::OpMemoryModel,
                {static_cast<uint32_t>(spv::AddressingModelLogical),
                 static_cast<uint32_t>(spv::MemoryModelGLSL450)});

    /* 5. EntryPoint (lists all static interface global variables for SPIR-V 1.6 legality) */
    {
      std::vector<uint32_t> args = {static_cast<uint32_t>(spv::ExecutionModelGLCompute),
                                    spv.main_fn_id};
      SpvEmitter::append_string(args, "main");
      for (uint32_t iv : spv.interface_vars) args.push_back(iv);
      spv.emit_op_vec(words, spv::OpEntryPoint, args);
    }

    /* 6. ExecutionModeId LocalSizeId */
    spv.emit_op(words, spv::OpExecutionModeId,
                {spv.main_fn_id, static_cast<uint32_t>(spv::ExecutionModeLocalSizeId),
                 spv.spec_lx, spv.spec_ly, spv.spec_lz});

    /* 7. Annotations */
    if (words.size() > kMaxSpirvWords - spv.annotations.size()) {
      return spv.fail("SPIR-V binary exceeds size cap");
    }
    words.insert(words.end(), spv.annotations.begin(), spv.annotations.end());

    /* 8. Types, Constants, Global Variables */
    if (words.size() > kMaxSpirvWords - spv.types_consts_vars.size()) {
      return spv.fail("SPIR-V binary exceeds size cap");
    }
    words.insert(words.end(), spv.types_consts_vars.begin(), spv.types_consts_vars.end());

    /* 9. Function body */
    spv.emit_op(words, spv::OpFunction,
                {void_ty, spv.main_fn_id, static_cast<uint32_t>(spv::FunctionControlMaskNone),
                 fn_ty});
    for (size_t bi = 0; bi < order.size(); bi++) {
      llvm::BasicBlock *bb = order[bi];
      spv.emit_op(words, spv::OpLabel, {bb_label[bb]});
      if (bi == 0 && !spv.entry_vars.empty()) {
        if (words.size() > kMaxSpirvWords - spv.entry_vars.size()) {
          return spv.fail("SPIR-V binary exceeds size cap");
        }
        words.insert(words.end(), spv.entry_vars.begin(), spv.entry_vars.end());
      }
      const auto &pw = bb_phis[bb];
      if (!pw.empty()) {
        if (words.size() > kMaxSpirvWords - pw.size()) {
          return spv.fail("SPIR-V binary exceeds size cap");
        }
        words.insert(words.end(), pw.begin(), pw.end());
      }
      const auto &bw = bb_bodies[bb];
      if (!bw.empty()) {
        if (words.size() > kMaxSpirvWords - bw.size()) {
          return spv.fail("SPIR-V binary exceeds size cap");
        }
        words.insert(words.end(), bw.begin(), bw.end());
      }
      if (!spv.error.empty()) return false;
    }
    spv.emit_op(words, spv::OpFunctionEnd, {});
    if (!spv.error.empty()) return false;

    /* Patch final ID bound in header word 3. */
    words[3] = spv.next_id;

    size_t nbytes = words.size() * sizeof(uint32_t);
    uint32_t *buf = static_cast<uint32_t *>(std::malloc(nbytes));
    if (!buf) return spv.fail("host OOM allocating SPIR-V output");
    std::memcpy(buf, words.data(), nbytes);

    uint32_t bmask = 0;
    uint32_t rmask = 0;
    for (uint32_t s = 0; s < kMaxBufferSlots; s++) {
      if (buffers[s].used) {
        bmask |= (1u << s);
        if (buffers[s].readonly) rmask |= (1u << s);
      }
    }
    out->spirv = buf;
    out->spirv_bytes = nbytes;
    out->binding_mask = bmask;
    out->readonly_mask = rmask;
    return true;
  }
};

bool validate_function_tree_prepass(llvm::Function &root, std::string &err) {
  std::unordered_map<const llvm::Function *, uint8_t> state; /* 1 = active, 2 = done */
  size_t total_switch_cases = 0;
  size_t visited_fns = 0;

  auto scan = [&](auto &&self, llvm::Function *f) -> bool {
    auto it = state.find(f);
    if (it != state.end()) {
      if (it->second == 1) {
        err = "recursive call graph not supported in AIR";
        return false;
      }
      return true;
    }
    if (++visited_fns > kMaxReachableHelpers) {
      err = "AIR helper function count exceeds limit";
      return false;
    }
    if (f->size() > kMaxBlocks || f->getInstructionCount() > kMaxInsts) {
      err = "AIR function exceeds block or instruction budget";
      return false;
    }
    state[f] = 1;
    for (llvm::BasicBlock &bb : *f) {
      const llvm::Instruction *term = bb.getTerminator();
      if (!term) {
        err = "AIR basic block missing terminator";
        return false;
      }
      if (!llvm::isa<llvm::BranchInst>(term) && !llvm::isa<llvm::SwitchInst>(term) &&
          !llvm::isa<llvm::ReturnInst>(term) && !llvm::isa<llvm::UnreachableInst>(term)) {
        err = "unsupported terminator instruction in AIR";
        return false;
      }
      if (auto *sw = llvm::dyn_cast<llvm::SwitchInst>(term)) {
        size_t nc = sw->getNumCases();
        if (nc > kMaxSwitchCasesSingle || nc > kMaxSwitchCasesTotal - total_switch_cases) {
          err = "switch case count exceeds budget";
          return false;
        }
        total_switch_cases += nc;
      }
      for (llvm::Instruction &inst : bb) {
        if (inst.isEHPad()) {
          err = "exception handling instructions not supported in AIR";
          return false;
        }
        if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&inst)) {
          llvm::Function *callee = ci->getCalledFunction();
          if (callee && !callee->isDeclaration()) {
            if (!self(self, callee)) return false;
          }
        }
      }
    }
    state[f] = 2;
    return true;
  };
  return scan(scan, &root);
}

bool inline_helpers(llvm::Function &fn, std::string &err) {
  for (int depth = 0; depth < kMaxInlineDepth; depth++) {
    std::vector<llvm::CallInst *> to_inline;
    size_t est_insts = 0;
    size_t est_blocks = fn.size();
    if (est_blocks > kMaxBlocks) {
      err = "AIR function exceeds block budget";
      return false;
    }
    for (llvm::BasicBlock &bb : fn) {
      for (llvm::Instruction &inst : bb) {
        if (++est_insts > kMaxInsts) {
          err = "AIR function exceeds instruction budget";
          return false;
        }
        if (auto *ci = llvm::dyn_cast<llvm::CallInst>(&inst)) {
          llvm::Function *callee = ci->getCalledFunction();
          if (callee && !callee->isDeclaration()) {
            if (callee == &fn) {
              err = "recursive kernel call not supported";
              return false;
            }
            to_inline.push_back(ci);
          }
        }
      }
    }
    if (to_inline.empty()) return true;
    for (llvm::CallInst *ci : to_inline) {
      llvm::Function *callee = ci->getCalledFunction();
      if (!callee || callee->isDeclaration()) continue;
      size_t c_insts = callee->getInstructionCount();
      size_t c_blocks = callee->size();
      if (c_insts > kMaxInsts || est_insts > kMaxInsts - c_insts) {
        err = "AIR helper inlining exceeds instruction budget";
        return false;
      }
      if (c_blocks > kMaxBlocks || est_blocks > kMaxBlocks - c_blocks) {
        err = "AIR helper inlining exceeds block budget";
        return false;
      }
      est_insts += c_insts;
      est_blocks += c_blocks;
      llvm::InlineFunctionInfo ifi;
      if (!llvm::InlineFunction(*ci, ifi).isSuccess()) {
        err = "failed to inline internal AIR helper function";
        return false;
      }
    }
  }
  err = "AIR helper inline depth exceeded limit";
  return false;
}

bool is_cfg_cleanly_structured(llvm::Function &fn) {
  llvm::DominatorTree DT(fn);
  llvm::LoopInfo LI(DT);

  /* 1. Verify reducibility iteratively: every DFS back-edge must target a dominator. */
  std::unordered_set<const llvm::BasicBlock *> visited;
  std::unordered_set<const llvm::BasicBlock *> in_stack;
  struct Frame {
    const llvm::BasicBlock *bb;
    unsigned succ_idx;
  };
  std::vector<Frame> stk;
  const llvm::BasicBlock *entry = &fn.getEntryBlock();
  visited.insert(entry);
  in_stack.insert(entry);
  stk.push_back({entry, 0});
  while (!stk.empty()) {
    Frame &fr = stk.back();
    const llvm::Instruction *term = fr.bb->getTerminator();
    if (fr.succ_idx >= term->getNumSuccessors()) {
      in_stack.erase(fr.bb);
      stk.pop_back();
      continue;
    }
    const llvm::BasicBlock *succ = term->getSuccessor(fr.succ_idx++);
    if (in_stack.count(succ)) {
      if (!DT.dominates(succ, fr.bb)) return false;
    } else if (visited.insert(succ).second) {
      in_stack.insert(succ);
      stk.push_back({succ, 0});
    }
  }

  /* 2. Verify every loop has a single latch and a single unique exit block. */
  std::unordered_map<const llvm::BasicBlock *, const llvm::BasicBlock *> loop_exit_map;
  std::unordered_map<const llvm::BasicBlock *, const llvm::BasicBlock *> loop_latch_map;
  for (llvm::Loop *L : LI.getLoopsInPreorder()) {
    const llvm::BasicBlock *H = L->getHeader();
    const llvm::BasicBlock *Latch = L->getLoopLatch();
    if (!H || !Latch) return false;
    llvm::SmallVector<llvm::BasicBlock *, 4> exits;
    L->getUniqueExitBlocks(exits);
    if (exits.size() != 1) return false;
    loop_exit_map[H] = exits[0];
    loop_latch_map[H] = Latch;
  }

  /* 3. Verify every non-loop conditional branch forms a single-entry selection construct
        (all blocks reachable before its post-dominator/loop targets are dominated by B). */
  llvm::PostDominatorTree PDT(fn);
  for (llvm::BasicBlock &bb : fn) {
    if (!visited.count(&bb)) continue;
    auto *bi = llvm::dyn_cast<llvm::BranchInst>(bb.getTerminator());
    if (!bi || !bi->isConditional() || bi->getSuccessor(0) == bi->getSuccessor(1)) continue;
    if (loop_exit_map.count(&bb)) continue;

    llvm::Loop *L = LI.getLoopFor(&bb);
    const llvm::BasicBlock *l_exit = L ? loop_exit_map[L->getHeader()] : nullptr;
    const llvm::BasicBlock *l_latch = L ? loop_latch_map[L->getHeader()] : nullptr;
    if (L && &bb == l_latch &&
        ((bi->getSuccessor(0) == L->getHeader() && bi->getSuccessor(1) == l_exit) ||
         (bi->getSuccessor(1) == L->getHeader() && bi->getSuccessor(0) == l_exit))) {
      continue;
    }

    const llvm::BasicBlock *M = nullptr;
    if (auto *pnode = PDT.getNode(&bb)) {
      if (auto *idom = pnode->getIDom()) M = idom->getBlock();
    }
    if (!M || (L && LI.getLoopFor(M) != L) || M == l_exit || M == l_latch) {
      const llvm::BasicBlock *s0 = bi->getSuccessor(0);
      const llvm::BasicBlock *s1 = bi->getSuccessor(1);
      if (L && (s0 == l_exit || s0 == l_latch) && s1 != l_exit && s1 != l_latch) M = s1;
      else if (L && (s1 == l_exit || s1 == l_latch) && s0 != l_exit && s0 != l_latch) M = s0;
    }
    if (!M) return false;

    std::unordered_set<const llvm::BasicBlock *> seen;
    std::vector<const llvm::BasicBlock *> q;
    seen.insert(&bb);
    for (unsigned s = 0; s < 2; s++) {
      const llvm::BasicBlock *succ = bi->getSuccessor(s);
      if (succ != M && succ != l_exit && succ != l_latch && seen.insert(succ).second) {
        q.push_back(succ);
      }
    }
    while (!q.empty()) {
      const llvm::BasicBlock *cur = q.back();
      q.pop_back();
      if (!DT.dominates(&bb, cur)) return false;
      const llvm::Instruction *cterm = cur->getTerminator();
      for (unsigned s = 0; s < cterm->getNumSuccessors(); s++) {
        const llvm::BasicBlock *nx = cterm->getSuccessor(s);
        if (nx == M || nx == l_exit || nx == l_latch) continue;
        if (seen.insert(nx).second) q.push_back(nx);
      }
    }
  }
  return true;
}

bool prepare_llvm_function(llvm::Module &mod, llvm::Function &fn, std::string &err) {
  if (fn.isDeclaration() || fn.empty()) {
    err = "AIR kernel has no body";
    return false;
  }
  if (!fn.getReturnType()->isVoidTy()) {
    err = "AIR compute kernel must return void";
    return false;
  }
  if (!validate_function_tree_prepass(fn, err)) return false;
  if (!inline_helpers(fn, err)) return false;

  /* Re-check post-inline switch counts and IR integrity before running passes. */
  size_t post_switches = 0;
  for (llvm::BasicBlock &bb : fn) {
    if (auto *sw = llvm::dyn_cast<llvm::SwitchInst>(bb.getTerminator())) {
      size_t nc = sw->getNumCases();
      if (nc > kMaxSwitchCasesSingle || nc > kMaxSwitchCasesTotal - post_switches) {
        err = "post-inline switch case count exceeds budget";
        return false;
      }
      post_switches += nc;
    }
  }

  std::string ver_str;
  llvm::raw_string_ostream ver_os(ver_str);
  if (llvm::verifyFunction(fn, &ver_os)) {
    err = "malformed AIR bitcode: " + ver_str.substr(0, 256);
    return false;
  }

  /* Run New Pass Manager: LowerSwitch -> UnifyExit -> Mem2Reg -> LoopSimplify.
     Only run StructurizeCFG when the CFG actually has irreducible/multi-exit loops,
     preserving tight 2-block native loops for GPU drivers. */
  llvm::LoopAnalysisManager LAM;
  llvm::FunctionAnalysisManager FAM;
  llvm::CGSCCAnalysisManager CGAM;
  llvm::ModuleAnalysisManager MAM;
  llvm::PassBuilder PB;
  PB.registerModuleAnalyses(MAM);
  PB.registerCGSCCAnalyses(CGAM);
  PB.registerFunctionAnalyses(FAM);
  PB.registerLoopAnalyses(LAM);
  PB.crossRegisterProxies(LAM, FAM, CGAM, MAM);

  llvm::FunctionPassManager FPM;
  FPM.addPass(llvm::LowerSwitchPass());
  FPM.addPass(llvm::UnifyFunctionExitNodesPass());
  FPM.addPass(llvm::PromotePass());
  FPM.addPass(llvm::LoopSimplifyPass());
  FPM.run(fn, FAM);

  if (!is_cfg_cleanly_structured(fn)) {
    FAM.clear(fn, fn.getName());
    llvm::FunctionPassManager SFPM;
    SFPM.addPass(llvm::StructurizeCFGPass());
    SFPM.addPass(llvm::LoopSimplifyPass());
    SFPM.run(fn, FAM);
  }

  size_t insts = 0;
  if (fn.size() > kMaxBlocks) {
    err = "AIR kernel exceeded block budget after structurization";
    return false;
  }
  for (llvm::BasicBlock &bb : fn) {
    insts += bb.size();
    if (insts > kMaxInsts) {
      err = "AIR kernel exceeded instruction budget";
      return false;
    }
  }
  (void)mod;
  return true;
}

llvm::MDNode *find_kernel_metadata(llvm::Module &mod,
                                   const char *kernel_name,
                                   llvm::Function **out_fn) {
  auto *nmd = mod.getNamedMetadata("air.kernel");
  if (!nmd) return nullptr;
  for (unsigned i = 0; i < nmd->getNumOperands(); i++) {
    auto *node = nmd->getOperand(i);
    if (!node || node->getNumOperands() < 1) continue;
    auto *fn = llvm::mdconst::dyn_extract_or_null<llvm::Function>(node->getOperand(0).get());
    if (!fn) continue;
    if (!kernel_name || kernel_name[0] == '\0' || fn->getName() == kernel_name) {
      *out_fn = fn;
      return node;
    }
  }
  return nullptr;
}

} // namespace

extern "C" bool air2spv_compile_bitcode(const uint8_t *bc,
                                        size_t bc_len,
                                        const char *kernel_name,
                                        const air2spv_caps_t *caps,
                                        air2spv_result_t *out,
                                        char *err_buf,
                                        size_t err_cap) {
  if (!out) return false;
  std::memset(out, 0, sizeof(*out));
  if (!bc || bc_len < 4 || bc_len > METALLIB_MAX_BITCODE_BYTES) {
    set_err(err_buf, err_cap, "AIR bitcode buffer invalid or oversized");
    return false;
  }
  if (!kernel_name || kernel_name[0] == '\0') {
    set_err(err_buf, err_cap, "missing kernel name");
    return false;
  }

  air2spv_caps_t eff_caps{};
  if (caps) {
    eff_caps = *caps;
  } else {
    eff_caps.has_float16 = true;
    eff_caps.has_int16 = true;
    eff_caps.has_int8 = true;
    eff_caps.has_storage_16bit = true;
    eff_caps.has_storage_8bit = true;
    eff_caps.has_int64 = true;
    eff_caps.max_workgroup_memory = 32768u;
    eff_caps.subgroup_ops = AIR2SPV_SUBGROUP_BASIC | AIR2SPV_SUBGROUP_VOTE |
                            AIR2SPV_SUBGROUP_ARITHMETIC | AIR2SPV_SUBGROUP_BALLOT |
                            AIR2SPV_SUBGROUP_SHUFFLE | AIR2SPV_SUBGROUP_SHUFFLE_RELATIVE;
  }

  llvm::LLVMContext ctx;
  auto mbuf = llvm::MemoryBuffer::getMemBuffer(
      llvm::StringRef(reinterpret_cast<const char *>(bc), bc_len), "air.bc", false);
  auto mod_exp = llvm::parseBitcodeFile(mbuf->getMemBufferRef(), ctx);
  if (!mod_exp) {
    std::string msg = llvm::toString(mod_exp.takeError());
    set_err(err_buf, err_cap, "LLVM bitcode parse failed: " + msg);
    return false;
  }
  std::unique_ptr<llvm::Module> mod = std::move(*mod_exp);

  /* Verify entire module before touching any function or running inlining/passes. */
  std::string ver_str;
  llvm::raw_string_ostream ver_os(ver_str);
  if (llvm::verifyModule(*mod, &ver_os)) {
    set_err(err_buf, err_cap, "malformed AIR module: " + ver_str.substr(0, 256));
    return false;
  }

  llvm::Function *fn = nullptr;
  llvm::MDNode *kmd = find_kernel_metadata(*mod, kernel_name, &fn);
  if (!kmd || !fn) {
    set_err(err_buf, err_cap, std::string("compute kernel '") + kernel_name + "' not found in !air.kernel");
    return false;
  }

  std::string prep_err;
  if (!prepare_llvm_function(*mod, *fn, prep_err)) {
    set_err(err_buf, err_cap, prep_err);
    return false;
  }

  Compiler comp(*mod, *fn, eff_caps);
  if (!comp.parse_air_metadata(kmd)) {
    set_err(err_buf, err_cap, comp.spv.error);
    return false;
  }
  if (!comp.compile(out)) {
    set_err(err_buf, err_cap, comp.spv.error);
    return false;
  }
  return true;
}

extern "C" bool air2spv_compile_metallib(const uint8_t *mlib,
                                         size_t mlib_len,
                                         const char *kernel_name,
                                         const air2spv_caps_t *caps,
                                         air2spv_result_t *out,
                                         char *err_buf,
                                         size_t err_cap) {
  if (!out) return false;
  std::memset(out, 0, sizeof(*out));
  metallib_index_t idx{};
  if (!metallib_parse(mlib, mlib_len, &idx, err_buf, err_cap)) {
    return false;
  }
  const metallib_func_t *fn = metallib_find_compute(&idx, kernel_name);
  if (!fn) {
    metallib_index_free(&idx);
    set_err(err_buf, err_cap, std::string("compute kernel '") + (kernel_name ? kernel_name : "") +
                                  "' not found in metallib");
    return false;
  }
  bool ok = air2spv_compile_bitcode(fn->bitcode, fn->bitcode_len, fn->name, caps, out, err_buf,
                                    err_cap);
  metallib_index_free(&idx);
  return ok;
}
