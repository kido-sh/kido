/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "metallib.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>

namespace {

constexpr size_t kMtlbHeaderSize = 88;
constexpr size_t kFatMaxArchs = 32;
constexpr uint32_t kBitcodeWrapperMagic = 0x0B17C0DEu;

void set_err(char *err_buf, size_t err_cap, const char *msg) {
  if (err_buf && err_cap > 0) {
    std::snprintf(err_buf, err_cap, "%s", msg ? msg : "metallib parse error");
  }
}

uint16_t read_le16(const uint8_t *p) {
  return static_cast<uint16_t>(static_cast<uint16_t>(p[0]) | (static_cast<uint16_t>(p[1]) << 8));
}

uint32_t read_le32(const uint8_t *p) {
  return static_cast<uint32_t>(p[0]) | (static_cast<uint32_t>(p[1]) << 8) |
         (static_cast<uint32_t>(p[2]) << 16) | (static_cast<uint32_t>(p[3]) << 24);
}

uint64_t read_le64(const uint8_t *p) {
  return static_cast<uint64_t>(read_le32(p)) | (static_cast<uint64_t>(read_le32(p + 4)) << 32);
}

uint32_t read_be32(const uint8_t *p) {
  return (static_cast<uint32_t>(p[0]) << 24) | (static_cast<uint32_t>(p[1]) << 16) |
         (static_cast<uint32_t>(p[2]) << 8) | static_cast<uint32_t>(p[3]);
}

uint64_t read_be64(const uint8_t *p) {
  return (static_cast<uint64_t>(read_be32(p)) << 32) | static_cast<uint64_t>(read_be32(p + 4));
}

bool is_raw_bitcode_magic(const uint8_t *p, size_t len) {
  return len >= 4 && p[0] == 0x42u && p[1] == 0x43u && p[2] == 0xC0u && p[3] == 0xDEu;
}

bool unwrap_bitcode_slice(const uint8_t *raw,
                          size_t raw_len,
                          const uint8_t **out_bc,
                          size_t *out_len) {
  if (!raw || raw_len < 4 || raw_len > METALLIB_MAX_BITCODE_BYTES) return false;
  if (is_raw_bitcode_magic(raw, raw_len)) {
    *out_bc = raw;
    *out_len = raw_len;
    return true;
  }
  if (raw_len >= 20 && read_le32(raw) == kBitcodeWrapperMagic) {
    uint32_t inner_off = read_le32(raw + 8);
    uint32_t inner_sz = read_le32(raw + 12);
    if (inner_off < 20u || inner_sz < 4u || inner_sz > METALLIB_MAX_BITCODE_BYTES) return false;
    if (inner_off > raw_len || inner_sz > raw_len - inner_off) return false;
    const uint8_t *inner = raw + inner_off;
    if (!is_raw_bitcode_magic(inner, inner_sz)) return false;
    *out_bc = inner;
    *out_len = inner_sz;
    return true;
  }
  return false;
}

bool locate_mtlb_slice(const uint8_t *data,
                       size_t len,
                       const uint8_t **out_slice,
                       size_t *out_len,
                       char *err_buf,
                       size_t err_cap) {
  if (len >= 4 && std::memcmp(data, "MTLB", 4) == 0) {
    *out_slice = data;
    *out_len = len;
    return true;
  }
  if (len < 8) {
    set_err(err_buf, err_cap, "metallib data truncated");
    return false;
  }
  uint32_t magic_be = read_be32(data);
  bool is_fat32 = (magic_be == 0xCAFEBABEu || magic_be == 0xBEBAFECAu);
  bool is_fat64 = (magic_be == 0xCAFEBABFu || magic_be == 0xBFBAFECAu);
  if (!is_fat32 && !is_fat64) {
    set_err(err_buf, err_cap, "invalid metallib magic (expected MTLB or Mach-O FAT)");
    return false;
  }
  bool big_endian = (magic_be == 0xCAFEBABEu || magic_be == 0xCAFEBABFu);
  uint32_t nfat = big_endian ? read_be32(data + 4) : read_le32(data + 4);
  if (nfat == 0 || nfat > kFatMaxArchs) {
    set_err(err_buf, err_cap, "invalid Mach-O FAT slice count in metallib");
    return false;
  }
  size_t entry_sz = is_fat64 ? 32u : 20u;
  if (static_cast<size_t>(nfat) > (len - 8u) / entry_sz) {
    set_err(err_buf, err_cap, "truncated Mach-O FAT table in metallib");
    return false;
  }
  for (uint32_t i = 0; i < nfat; i++) {
    const uint8_t *e = data + 8u + static_cast<size_t>(i) * entry_sz;
    uint64_t off = 0;
    uint64_t sz = 0;
    if (is_fat64) {
      off = big_endian ? read_be64(e + 8) : read_le64(e + 8);
      sz = big_endian ? read_be64(e + 16) : read_le64(e + 16);
    } else {
      off = big_endian ? read_be32(e + 8) : read_le32(e + 8);
      sz = big_endian ? read_be32(e + 12) : read_le32(e + 12);
    }
    if (off > len || sz < kMtlbHeaderSize || sz > len - off) continue;
    if (std::memcmp(data + off, "MTLB", 4) == 0) {
      *out_slice = data + off;
      *out_len = static_cast<size_t>(sz);
      return true;
    }
  }
  set_err(err_buf, err_cap, "Mach-O FAT archive does not contain an MTLB slice");
  return false;
}

bool valid_func_name(const char *s, size_t n) {
  if (n == 0 || n > METALLIB_MAX_NAME_LEN) return false;
  for (size_t i = 0; i < n; i++) {
    unsigned char c = static_cast<unsigned char>(s[i]);
    if (c < 0x20u || c == 0x7Fu) return false;
  }
  return true;
}

} // namespace

extern "C" bool metallib_parse(const uint8_t *data,
                               size_t len,
                               metallib_index_t *out,
                               char *err_buf,
                               size_t err_cap) {
  if (!out) return false;
  out->funcs = nullptr;
  out->count = 0;

  if (!data || len < kMtlbHeaderSize || len > METALLIB_MAX_BYTES) {
    set_err(err_buf, err_cap, "metallib size out of bounds");
    return false;
  }

  const uint8_t *slice = nullptr;
  size_t slice_len = 0;
  if (!locate_mtlb_slice(data, len, &slice, &slice_len, err_buf, err_cap)) {
    return false;
  }
  if (slice_len < kMtlbHeaderSize || std::memcmp(slice, "MTLB", 4) != 0) {
    set_err(err_buf, err_cap, "truncated MTLB header");
    return false;
  }

  uint64_t declared_sz = read_le64(slice + 16);
  if (declared_sz < kMtlbHeaderSize || declared_sz > slice_len) {
    set_err(err_buf, err_cap, "MTLB declared file size exceeds buffer");
    return false;
  }

  uint64_t fn_off = read_le64(slice + 24);
  uint64_t fn_sz = read_le64(slice + 32);
  uint64_t bc_off = read_le64(slice + 72);
  uint64_t bc_sz = read_le64(slice + 80);

  if (fn_off < kMtlbHeaderSize || fn_off > declared_sz || declared_sz - fn_off < 4u ||
      fn_sz > declared_sz - fn_off - 4u) {
    set_err(err_buf, err_cap, "MTLB function list bounds invalid");
    return false;
  }
  if (bc_off < kMtlbHeaderSize || bc_off > declared_sz || bc_sz > declared_sz - bc_off) {
    set_err(err_buf, err_cap, "MTLB bitcode section bounds invalid");
    return false;
  }

  uint32_t fn_count = read_le32(slice + fn_off);
  const uint8_t *fn_entries = slice + fn_off + 4u;
  if (fn_count > METALLIB_MAX_FUNCS) {
    set_err(err_buf, err_cap, "MTLB function count exceeds limit");
    return false;
  }
  if (fn_count == 0) {
    return true;
  }

  metallib_func_t *funcs =
      static_cast<metallib_func_t *>(std::calloc(fn_count, sizeof(metallib_func_t)));
  if (!funcs) {
    set_err(err_buf, err_cap, "host OOM allocating metallib index");
    return false;
  }

  size_t pos = 0;
  size_t parsed = 0;
  for (uint32_t i = 0; i < fn_count; i++) {
    if (fn_sz - pos < 4u) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB function entry truncated");
      return false;
    }
    uint32_t entry_sz = read_le32(fn_entries + pos);
    if (entry_sz < 4u || entry_sz > fn_sz - pos) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB function entry size invalid");
      return false;
    }
    size_t entry_end = pos + entry_sz;
    size_t cur = pos + 4u;

    bool has_name = false;
    bool has_type = false;
    bool has_offt = false;
    bool has_mdsz = false;
    char name_buf[128] = {0};
    uint8_t fn_type = 0xFFu;
    uint64_t bc_rel_off = 0;
    uint64_t mdsz = 0;

    while (cur + 4u <= entry_end) {
      const uint8_t *tag = fn_entries + cur;
      cur += 4u;
      if (std::memcmp(tag, "ENDT", 4) == 0) {
        break;
      }
      if (entry_end - cur < 2u) {
        std::free(funcs);
        set_err(err_buf, err_cap, "MTLB tag length truncated");
        return false;
      }
      uint16_t tlen = read_le16(fn_entries + cur);
      cur += 2u;
      if (tlen > entry_end - cur) {
        std::free(funcs);
        set_err(err_buf, err_cap, "MTLB tag value exceeds entry bounds");
        return false;
      }
      const uint8_t *val = fn_entries + cur;
      cur += tlen;

      if (std::memcmp(tag, "NAME", 4) == 0) {
        size_t nlen = tlen;
        while (nlen > 0 && val[nlen - 1] == '\0') nlen--;
        if (nlen == 0 || nlen > METALLIB_MAX_NAME_LEN || std::memchr(val, '\0', nlen) != nullptr) {
          std::free(funcs);
          set_err(err_buf, err_cap, "MTLB function name invalid");
          return false;
        }
        std::memset(name_buf, 0, sizeof(name_buf));
        std::memcpy(name_buf, val, nlen);
        name_buf[nlen] = '\0';
        if (!valid_func_name(name_buf, nlen)) {
          std::free(funcs);
          set_err(err_buf, err_cap, "MTLB function name contains control characters");
          return false;
        }
        has_name = true;
      } else if (std::memcmp(tag, "TYPE", 4) == 0) {
        if (tlen < 1u) {
          std::free(funcs);
          set_err(err_buf, err_cap, "MTLB TYPE tag empty");
          return false;
        }
        fn_type = val[0];
        has_type = true;
      } else if (std::memcmp(tag, "OFFT", 4) == 0) {
        if (tlen != 24u) {
          std::free(funcs);
          set_err(err_buf, err_cap, "MTLB OFFT tag size invalid");
          return false;
        }
        bc_rel_off = read_le64(val + 16);
        has_offt = true;
      } else if (std::memcmp(tag, "MDSZ", 4) == 0) {
        if (tlen != 8u) {
          std::free(funcs);
          set_err(err_buf, err_cap, "MTLB MDSZ tag size invalid");
          return false;
        }
        mdsz = read_le64(val);
        has_mdsz = true;
      }
    }

    pos = entry_end;

    if (!has_name || !has_type || !has_offt) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB function entry missing required tags");
      return false;
    }
    if (bc_rel_off > bc_sz) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB bitcode offset out of range");
      return false;
    }
    uint64_t avail = bc_sz - bc_rel_off;
    uint64_t raw_len = has_mdsz ? mdsz : avail;
    if (raw_len == 0 || raw_len > avail || raw_len > METALLIB_MAX_BITCODE_BYTES) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB bitcode slice length out of range");
      return false;
    }

    const uint8_t *bc_ptr = nullptr;
    size_t bc_len = 0;
    if (!unwrap_bitcode_slice(slice + bc_off + bc_rel_off, static_cast<size_t>(raw_len), &bc_ptr,
                              &bc_len)) {
      std::free(funcs);
      set_err(err_buf, err_cap, "MTLB function bitcode header invalid");
      return false;
    }

    std::memcpy(funcs[parsed].name, name_buf, sizeof(funcs[parsed].name));
    funcs[parsed].fn_type = fn_type;
    funcs[parsed].bitcode = bc_ptr;
    funcs[parsed].bitcode_len = bc_len;
    parsed++;
  }

  out->funcs = funcs;
  out->count = parsed;
  return true;
}

extern "C" void metallib_index_free(metallib_index_t *idx) {
  if (!idx) return;
  std::free(idx->funcs);
  idx->funcs = nullptr;
  idx->count = 0;
}

extern "C" size_t metallib_encode_compute_names(const metallib_index_t *idx,
                                                char *out_buf,
                                                size_t out_cap) {
  if (!idx || !out_buf || out_cap == 0) return 0;
  size_t pos = 0;
  for (size_t i = 0; i < idx->count; i++) {
    if (idx->funcs[i].fn_type != METALLIB_FN_COMPUTE) continue;
    size_t nlen = std::strlen(idx->funcs[i].name);
    if (nlen == 0 || nlen + 1u > out_cap - pos) break;
    std::memcpy(out_buf + pos, idx->funcs[i].name, nlen + 1u);
    pos += nlen + 1u;
  }
  return pos;
}

extern "C" const metallib_func_t *metallib_find_compute(const metallib_index_t *idx,
                                                        const char *name) {
  if (!idx || !name || name[0] == '\0') return nullptr;
  for (size_t i = 0; i < idx->count; i++) {
    if (idx->funcs[i].fn_type == METALLIB_FN_COMPUTE &&
        std::strcmp(idx->funcs[i].name, name) == 0) {
      return &idx->funcs[i];
    }
  }
  return nullptr;
}
