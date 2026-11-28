/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#include "msl2glsl.h"

#include <ctype.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
  char *data;
  size_t len;
  size_t cap;
  bool oom;
} strbuf_t;

static void sb_init(strbuf_t *sb) {
  memset(sb, 0, sizeof(*sb));
}

static void sb_free(strbuf_t *sb) {
  free(sb->data);
  memset(sb, 0, sizeof(*sb));
}

static bool sb_reserve(strbuf_t *sb, size_t extra) {
  if (sb->oom) return false;
  if (extra > SIZE_MAX - sb->len - 1) {
    sb->oom = true;
    return false;
  }
  size_t need = sb->len + extra + 1;
  if (need <= sb->cap) return true;
  size_t cap = sb->cap ? sb->cap : 1024;
  while (cap < need) {
    if (cap > SIZE_MAX / 2) {
      cap = need;
      break;
    }
    cap *= 2;
  }
  char *p = realloc(sb->data, cap);
  if (!p) {
    sb->oom = true;
    return false;
  }
  sb->data = p;
  sb->cap = cap;
  return true;
}

static void sb_append_n(strbuf_t *sb, const char *s, size_t n) {
  if (n == 0 || !sb_reserve(sb, n)) return;
  memcpy(sb->data + sb->len, s, n);
  sb->len += n;
  sb->data[sb->len] = '\0';
}

static void sb_append(strbuf_t *sb, const char *s) {
  sb_append_n(sb, s, strlen(s));
}

static void sb_append_c(strbuf_t *sb, char c) {
  sb_append_n(sb, &c, 1);
}

static void sb_printf(strbuf_t *sb, const char *fmt, ...) {
  char tmp[1024];
  va_list ap;
  va_start(ap, fmt);
  int n = vsnprintf(tmp, sizeof(tmp), fmt, ap);
  va_end(ap);
  if (n > 0) {
    if ((size_t)n < sizeof(tmp)) {
      sb_append_n(sb, tmp, (size_t)n);
    } else if (sb_reserve(sb, (size_t)n)) {
      va_list ap2;
      va_start(ap2, fmt);
      vsnprintf(sb->data + sb->len, (size_t)n + 1, fmt, ap2);
      va_end(ap2);
      sb->len += (size_t)n;
    }
  }
}

static bool is_ident_start(char c) {
  return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_';
}

static bool is_ident_char(char c) {
  return is_ident_start(c) || (c >= '0' && c <= '9');
}

static void skip_ws_and_comments(const char *s, size_t len, size_t *pos) {
  size_t i = *pos;
  while (i < len) {
    if (isspace((unsigned char)s[i])) {
      i++;
    } else if (i + 1 < len && s[i] == '/' && s[i + 1] == '/') {
      i += 2;
      while (i < len && s[i] != '\n') i++;
    } else if (i + 1 < len && s[i] == '/' && s[i + 1] == '*') {
      i += 2;
      while (i + 1 < len && !(s[i] == '*' && s[i + 1] == '/')) i++;
      i = (i + 1 < len) ? (i + 2) : len;
    } else {
      break;
    }
  }
  *pos = i;
}

/*
 * Normalize line endings (\r\n and bare \r -> \n), reject unsafe control bytes
 * (\0, \v, \f, etc.), iteratively splice all backslash-newline continuations,
 * and strip C/C++ comments. Always NUL-terminates out->data.
 */
static bool strip_comments(const char *src, size_t len, strbuf_t *out) {
  sb_init(out);
  if (!sb_reserve(out, len + 1)) return false;
  out->data[0] = '\0';

  /* Pass 1: reject control bytes, normalize all CR/CRLF to LF, iteratively splice \\\n. */
  strbuf_t spliced;
  sb_init(&spliced);
  if (!sb_reserve(&spliced, len + 1)) return false;
  spliced.data[0] = '\0';
  for (size_t i = 0; i < len;) {
    unsigned char uc = (unsigned char)src[i];
    if ((uc < 0x20u && uc != '\t' && uc != '\n' && uc != '\r') || uc == 0x7fu) {
      sb_free(&spliced);
      return false;
    }
    char c;
    if (uc == '\r') {
      if (i + 1 < len && src[i + 1] == '\n') i += 2;
      else i += 1;
      c = '\n';
    } else {
      c = src[i++];
    }
    if (c == '\n' && spliced.len > 0 && spliced.data[spliced.len - 1] == '\\') {
      spliced.len--;
      spliced.data[spliced.len] = '\0';
      continue;
    }
    sb_append_c(&spliced, c);
  }
  if (spliced.oom) {
    sb_free(&spliced);
    return false;
  }

  /* Pass 2: strip comments while preserving string/char literals. */
  const char *s = spliced.data;
  size_t slen = spliced.len;
  size_t i = 0;
  while (i < slen) {
    if (s[i] == '"' || s[i] == '\'') {
      char q = s[i];
      sb_append_c(out, s[i++]);
      while (i < slen) {
        char c = s[i++];
        sb_append_c(out, c);
        if (c == '\\' && i < slen) {
          sb_append_c(out, s[i++]);
        } else if (c == q) {
          break;
        }
      }
    } else if (i + 1 < slen && s[i] == '/' && s[i + 1] == '/') {
      i += 2;
      while (i < slen && s[i] != '\n') i++;
    } else if (i + 1 < slen && s[i] == '/' && s[i + 1] == '*') {
      i += 2;
      while (i + 1 < slen && !(s[i] == '*' && s[i + 1] == '/')) {
        if (s[i] == '\n') sb_append_c(out, '\n');
        i++;
      }
      i = (i + 1 < slen) ? (i + 2) : slen;
      sb_append_c(out, ' ');
    } else {
      sb_append_c(out, s[i++]);
    }
  }
  sb_free(&spliced);
  return !out->oom;
}

static bool match_word_at(const char *s, size_t len, size_t pos, const char *word) {
  size_t wl = strlen(word);
  if (pos + wl > len) return false;
  if (pos > 0 && is_ident_char(s[pos - 1])) return false;
  if (pos + wl < len && is_ident_char(s[pos + wl])) return false;
  return memcmp(s + pos, word, wl) == 0;
}

static bool has_forbidden_include_ext(const char *s, size_t len) {
  if (!s || len == 0) return false;
  return strstr(s, "GL_GOOGLE_") != NULL || strstr(s, "GL_ARB_shading_language_include") != NULL;
}

static bool is_allowed_user_directive(const char *s, size_t le, size_t d) {
  return match_word_at(s, le, d, "define") || match_word_at(s, le, d, "undef") ||
         match_word_at(s, le, d, "if") || match_word_at(s, le, d, "ifdef") ||
         match_word_at(s, le, d, "ifndef") || match_word_at(s, le, d, "elif") ||
         match_word_at(s, le, d, "else") || match_word_at(s, le, d, "endif");
}

/*
 * Sanitize preprocessor directives across the entire comment-stripped MSL source.
 * Blocks GL_GOOGLE_include_directive, GL_ARB_shading_language_include, arbitrary
 * #include, #extension, #version, #line, and strips benign Metal headers and #pragma.
 */
static bool sanitize_directives(const char *s, size_t len, strbuf_t *out, char *err_buf, size_t err_cap) {
  sb_init(out);
  if (!sb_reserve(out, len + 1)) {
    snprintf(err_buf, err_cap, "out of memory sanitizing MSL directives");
    return false;
  }
  out->data[0] = '\0';

  if (has_forbidden_include_ext(s, len)) {
    snprintf(err_buf, err_cap, "forbidden GLSL extension token in MSL source");
    sb_free(out);
    return false;
  }

  size_t i = 0;
  while (i < len) {
    size_t ls = i;
    while (i < len && s[i] != '\n') i++;
    size_t le = i;
    if (i < len && s[i] == '\n') i++;

    size_t p = ls;
    while (p < le && isspace((unsigned char)s[p])) p++;
    if (p < le && s[p] == '#') {
      size_t d = p + 1;
      while (d < le && isspace((unsigned char)s[d])) d++;
      if (match_word_at(s, le, d, "include")) {
        size_t a = d + 7;
        while (a < le && isspace((unsigned char)s[a])) a++;
        /* Only allow and strip standard Metal system headers like <metal_stdlib>. */
        if (a < le && s[a] == '<' && (le - a) > 7 && memcmp(s + a + 1, "metal_", 6) == 0) {
          sb_append_c(out, '\n');
          continue;
        }
        snprintf(err_buf, err_cap, "arbitrary #include directives are not permitted");
        sb_free(out);
        return false;
      }
      if (match_word_at(s, le, d, "pragma")) {
        sb_append_c(out, '\n');
        continue;
      }
      if (is_allowed_user_directive(s, le, d)) {
        sb_append_n(out, s + ls, i - ls);
        continue;
      }
      snprintf(err_buf, err_cap, "unsupported preprocessor directive in MSL source");
      sb_free(out);
      return false;
    }
    sb_append_n(out, s + ls, i - ls);
  }
  if (out->oom) {
    snprintf(err_buf, err_cap, "out of memory sanitizing MSL directives");
    sb_free(out);
    return false;
  }
  return true;
}

/* Defense-in-depth verification on user-derived GLSL output after the fixed preamble. */
static bool verify_emitted_user_glsl(const char *s, size_t len, char *err_buf, size_t err_cap) {
  if (has_forbidden_include_ext(s, len)) {
    snprintf(err_buf, err_cap, "forbidden GLSL extension token in translated output");
    return false;
  }
  for (size_t i = 0; i < len; i++) {
    unsigned char uc = (unsigned char)s[i];
    if ((uc < 0x20u && uc != '\t' && uc != '\n') || uc == 0x7fu) {
      snprintf(err_buf, err_cap, "forbidden control character in translated output");
      return false;
    }
    if (uc == '\\' && i + 1 < len && s[i + 1] == '\n') {
      snprintf(err_buf, err_cap, "forbidden line continuation in translated output");
      return false;
    }
  }
  size_t i = 0;
  while (i < len) {
    size_t ls = i;
    while (i < len && s[i] != '\n') i++;
    size_t le = i;
    if (i < len && s[i] == '\n') i++;
    size_t p = ls;
    while (p < le && isspace((unsigned char)s[p])) p++;
    if (p < le && s[p] == '#') {
      size_t d = p + 1;
      while (d < le && isspace((unsigned char)s[d])) d++;
      if (!is_allowed_user_directive(s, le, d)) {
        snprintf(err_buf, err_cap, "forbidden preprocessor directive in translated output");
        return false;
      }
    }
  }
  return true;
}

static bool validate_macros(const msl_macro_t *macros, size_t num_macros, char *err_buf, size_t err_cap) {
  for (size_t i = 0; i < num_macros; i++) {
    const msl_macro_t *m = &macros[i];
    if (!m->key || m->key_len == 0 || m->key_len > 128 || !is_ident_start(m->key[0])) {
      snprintf(err_buf, err_cap, "invalid preprocessor macro key");
      return false;
    }
    for (size_t k = 0; k < m->key_len; k++) {
      if (!is_ident_char(m->key[k])) {
        snprintf(err_buf, err_cap, "invalid character in preprocessor macro key");
        return false;
      }
    }
    if (has_forbidden_include_ext(m->key, m->key_len)) {
      snprintf(err_buf, err_cap, "forbidden macro key");
      return false;
    }
    if (m->val && m->val_len > 0) {
      for (size_t k = 0; k < m->val_len; k++) {
        unsigned char uc = (unsigned char)m->val[k];
        if ((uc < 0x20u && uc != '\t') || uc == 0x7fu) {
          snprintf(err_buf, err_cap, "invalid control byte in macro value");
          return false;
        }
      }
      if (m->val[m->val_len - 1] == '\\') {
        snprintf(err_buf, err_cap, "trailing backslash in macro value");
        return false;
      }
      if (has_forbidden_include_ext(m->val, m->val_len)) {
        snprintf(err_buf, err_cap, "forbidden macro value");
        return false;
      }
    }
  }
  return true;
}

static bool find_matching_delim(const char *s, size_t len, size_t open_pos, char open_c, char close_c, size_t *out_close) {
  if (open_pos >= len || s[open_pos] != open_c) return false;
  int depth = 0;
  for (size_t i = open_pos; i < len; i++) {
    if (s[i] == '"' || s[i] == '\'') {
      char q = s[i++];
      while (i < len) {
        if (s[i] == '\\' && i + 1 < len) {
          i += 2;
          continue;
        }
        if (s[i] == q) break;
        i++;
      }
      continue;
    }
    if (s[i] == open_c) depth++;
    else if (s[i] == close_c) {
      depth--;
      if (depth == 0) {
        *out_close = i;
        return true;
      }
    }
  }
  return false;
}

typedef struct {
  char name[128];
  size_t start;
  size_t params_open;
  size_t params_close;
  size_t body_open;
  size_t body_close;
} kernel_span_t;

static bool find_kernels(const char *s, size_t len, kernel_span_t **out_spans, size_t *out_count) {
  *out_spans = NULL;
  *out_count = 0;
  size_t cap = 0;
  size_t count = 0;
  kernel_span_t *arr = NULL;

  size_t i = 0;
  int brace_depth = 0;
  while (i < len) {
    if (s[i] == '{') {
      brace_depth++;
      i++;
      continue;
    }
    if (s[i] == '}') {
      if (brace_depth > 0) brace_depth--;
      i++;
      continue;
    }
    if (brace_depth == 0 && match_word_at(s, len, i, "kernel")) {
      size_t kstart = i;
      size_t p = i + 6;
      skip_ws_and_comments(s, len, &p);
      if (!match_word_at(s, len, p, "void")) {
        i += 6;
        continue;
      }
      p += 4;
      skip_ws_and_comments(s, len, &p);
      if (p >= len || !is_ident_start(s[p])) {
        i = p;
        continue;
      }
      size_t nstart = p;
      while (p < len && is_ident_char(s[p])) p++;
      size_t nlen = p - nstart;
      skip_ws_and_comments(s, len, &p);
      if (p >= len || s[p] != '(') {
        i = p;
        continue;
      }
      size_t pclose = 0;
      if (!find_matching_delim(s, len, p, '(', ')', &pclose)) {
        i = p + 1;
        continue;
      }
      size_t bopen = pclose + 1;
      skip_ws_and_comments(s, len, &bopen);
      if (bopen >= len || s[bopen] != '{') {
        i = bopen;
        continue;
      }
      size_t bclose = 0;
      if (!find_matching_delim(s, len, bopen, '{', '}', &bclose)) {
        i = bopen + 1;
        continue;
      }
      if (nlen > 0 && nlen < sizeof(arr[0].name)) {
        if (count == cap) {
          size_t ncap = cap ? cap * 2 : 16;
          if (ncap > 4096) {
            free(arr);
            return false;
          }
          kernel_span_t *narr = realloc(arr, ncap * sizeof(*narr));
          if (!narr) {
            free(arr);
            return false;
          }
          arr = narr;
          cap = ncap;
        }
        memcpy(arr[count].name, s + nstart, nlen);
        arr[count].name[nlen] = '\0';
        arr[count].start = kstart;
        arr[count].params_open = p;
        arr[count].params_close = pclose;
        arr[count].body_open = bopen;
        arr[count].body_close = bclose;
        count++;
      }
      i = bclose + 1;
      continue;
    }
    i++;
  }
  *out_spans = arr;
  *out_count = count;
  return true;
}

size_t msl_extract_kernel_names(const char *src, size_t src_len, char *out_buf, size_t out_cap) {
  if (!src || src_len == 0 || !out_buf || out_cap == 0) return 0;
  strbuf_t clean;
  if (!strip_comments(src, src_len, &clean)) {
    sb_free(&clean);
    return 0;
  }
  kernel_span_t *spans = NULL;
  size_t n = 0;
  if (!find_kernels(clean.data, clean.len, &spans, &n)) {
    sb_free(&clean);
    return 0;
  }
  size_t pos = 0;
  for (size_t i = 0; i < n; i++) {
    size_t nl = strlen(spans[i].name);
    if (nl + 1 > out_cap - pos) break;
    memcpy(out_buf + pos, spans[i].name, nl + 1);
    pos += nl + 1;
  }
  free(spans);
  sb_free(&clean);
  return pos;
}

typedef struct {
  bool uses_fp16;
  bool uses_int16;
  bool uses_int8;
  bool uses_int64;
  bool uses_subgroup;
} ext_flags_t;

typedef struct {
  char from[64];
  char to[96];
} ident_remap_t;

static const char *map_msl_ident(const char *tok, size_t len, ext_flags_t *ext) {
  static const struct {
    const char *msl;
    const char *glsl;
    int flag;
  } table[] = {
      {"half", "float16_t", 1},
      {"half2", "f16vec2", 1},
      {"half3", "f16vec3", 1},
      {"half4", "f16vec4", 1},
      {"float2", "vec2", 0},
      {"float3", "vec3", 0},
      {"float4", "vec4", 0},
      {"int2", "ivec2", 0},
      {"int3", "ivec3", 0},
      {"int4", "ivec4", 0},
      {"uint2", "uvec2", 0},
      {"uint3", "uvec3", 0},
      {"uint4", "uvec4", 0},
      {"bool2", "bvec2", 0},
      {"bool3", "bvec3", 0},
      {"bool4", "bvec4", 0},
      {"ushort", "uint16_t", 2},
      {"ushort2", "u16vec2", 2},
      {"ushort3", "u16vec3", 2},
      {"ushort4", "u16vec4", 2},
      {"short", "int16_t", 2},
      {"short2", "i16vec2", 2},
      {"short3", "i16vec3", 2},
      {"short4", "i16vec4", 2},
      {"uchar", "uint8_t", 3},
      {"uchar2", "u8vec2", 3},
      {"uchar3", "u8vec3", 3},
      {"uchar4", "u8vec4", 3},
      {"char", "int8_t", 3},
      {"char2", "i8vec2", 3},
      {"char3", "i8vec3", 3},
      {"char4", "i8vec4", 3},
      {"ulong", "uint64_t", 4},
      {"long", "int64_t", 4},
      {"rsqrt", "inversesqrt", 0},
      {"simd_sum", "subgroupAdd", 5},
      {"simd_max", "subgroupMax", 5},
      {"simd_min", "subgroupMin", 5},
      {"simd_broadcast", "subgroupBroadcast", 5},
      {"simd_shuffle", "subgroupShuffle", 5},
      {"constexpr", "const", 0},
      /* Escape GLSL-reserved keywords that are ordinary identifiers in Metal/C++ */
      {"flat", "_msl_flat", 0},
      {"smooth", "_msl_smooth", 0},
      {"centroid", "_msl_centroid", 0},
      {"sample", "_msl_sample", 0},
      {"patch", "_msl_patch", 0},
      {"input", "_msl_input", 0},
      {"output", "_msl_output", 0},
      {"filter", "_msl_filter", 0},
      {"active", "_msl_active", 0},
      {"common", "_msl_common", 0},
      {"partition", "_msl_partition", 0},
      {"resource", "_msl_resource", 0},
      {"uniform", "_msl_uniform", 0},
      {"shared", "_msl_shared", 0},
      {"layout", "_msl_layout", 0},
      {"buffer", "_msl_buffer", 0},
      {"coherent", "_msl_coherent", 0},
      {"readonly", "_msl_readonly", 0},
      {"writeonly", "_msl_writeonly", 0},
      {"precise", "_msl_precise", 0},
      {"invariant", "_msl_invariant", 0},
      {"in", "_msl_in", 0},
      {"out", "_msl_out", 0},
      {"inout", "_msl_inout", 0},
  };
  for (size_t i = 0; i < sizeof(table) / sizeof(table[0]); i++) {
    if (strlen(table[i].msl) == len && memcmp(tok, table[i].msl, len) == 0) {
      if (ext) {
        if (table[i].flag == 1) ext->uses_fp16 = true;
        else if (table[i].flag == 2) ext->uses_int16 = true;
        else if (table[i].flag == 3) ext->uses_int8 = true;
        else if (table[i].flag == 4) ext->uses_int64 = true;
        else if (table[i].flag == 5) ext->uses_subgroup = true;
      }
      return table[i].glsl;
    }
  }
  return NULL;
}

static void translate_type_str(const char *in, size_t in_len, char *out, size_t out_cap, ext_flags_t *ext) {
  strbuf_t sb;
  sb_init(&sb);
  size_t i = 0;
  while (i < in_len) {
    if (is_ident_start(in[i])) {
      size_t st = i;
      while (i < in_len && is_ident_char(in[i])) i++;
      size_t tl = i - st;
      if ((tl == 6 && memcmp(in + st, "device", 6) == 0) ||
          (tl == 8 && memcmp(in + st, "constant", 8) == 0) ||
          (tl == 5 && memcmp(in + st, "const", 5) == 0) ||
          (tl == 6 && memcmp(in + st, "thread", 6) == 0) ||
          (tl == 11 && memcmp(in + st, "threadgroup", 11) == 0)) {
        continue;
      }
      const char *mapped = map_msl_ident(in + st, tl, ext);
      if (sb.len > 0 && sb.data[sb.len - 1] != ' ') sb_append_c(&sb, ' ');
      if (mapped) sb_append(&sb, mapped);
      else sb_append_n(&sb, in + st, tl);
    } else {
      i++;
    }
  }
  if (sb.data && sb.len < out_cap) {
    memcpy(out, sb.data, sb.len + 1);
  } else if (out_cap > 0) {
    out[0] = '\0';
  }
  sb_free(&sb);
}

static bool parse_uint_in_parens(const char *s, unsigned long *out_val) {
  while (*s == ' ' || *s == '\t') s++;
  if (!isdigit((unsigned char)*s)) return false;
  char *endp = NULL;
  unsigned long v = strtoul(s, &endp, 10);
  if (endp == s) return false;
  while (*endp == ' ' || *endp == '\t') endp++;
  if (*endp != ')') return false;
  *out_val = v;
  return true;
}

static void rewrite_code_tokens(const char *s,
                                size_t len,
                                const ident_remap_t *remaps,
                                size_t num_remaps,
                                ext_flags_t *ext,
                                strbuf_t *out) {
  size_t i = 0;
  char prev_nonws = '\0';
  while (i < len) {
    if (i + 7 <= len && memcmp(s + i, "metal::", 7) == 0 &&
        (i == 0 || !is_ident_char(s[i - 1]))) {
      i += 7;
      continue;
    }
    if (s[i] == '(') {
      size_t p = i + 1;
      skip_ws_and_comments(s, len, &p);
      if (match_word_at(s, len, p, "void")) {
        p += 4;
        skip_ws_and_comments(s, len, &p);
        if (p < len && s[p] == ')') {
          p++;
          skip_ws_and_comments(s, len, &p);
          if (p < len && is_ident_start(s[p])) {
            while (p < len && is_ident_char(s[p])) p++;
            skip_ws_and_comments(s, len, &p);
            if (p < len && s[p] == ';') {
              i = p + 1;
              continue;
            }
          }
        }
      }
    }
    if (match_word_at(s, len, i, "threadgroup_barrier")) {
      size_t p = i + 19;
      skip_ws_and_comments(s, len, &p);
      size_t close_p = 0;
      if (p < len && s[p] == '(' && find_matching_delim(s, len, p, '(', ')', &close_p)) {
        sb_append(out, "barrier()");
        i = close_p + 1;
        prev_nonws = ')';
        continue;
      }
    }
    if (match_word_at(s, len, i, "static_cast")) {
      size_t p = i + 11;
      skip_ws_and_comments(s, len, &p);
      size_t close_a = 0;
      if (p < len && s[p] == '<' && find_matching_delim(s, len, p, '<', '>', &close_a)) {
        char tbuf[128];
        translate_type_str(s + p + 1, close_a - p - 1, tbuf, sizeof(tbuf), ext);
        sb_append(out, tbuf);
        i = close_a + 1;
        prev_nonws = 'x';
        continue;
      }
    }
    if (match_word_at(s, len, i, "thread") && prev_nonws != '.') {
      i += 6;
      continue;
    }
    if ((s[i] >= '0' && s[i] <= '9') && (i == 0 || !is_ident_char(s[i - 1]))) {
      size_t st = i;
      bool has_dot_or_exp = false;
      if (s[i] == '0' && i + 1 < len && (s[i + 1] == 'x' || s[i + 1] == 'X')) {
        i += 2;
        while (i < len && isxdigit((unsigned char)s[i])) i++;
      } else {
        while (i < len && (isdigit((unsigned char)s[i]) || s[i] == '.')) {
          if (s[i] == '.') has_dot_or_exp = true;
          i++;
        }
        if (i < len && (s[i] == 'e' || s[i] == 'E')) {
          has_dot_or_exp = true;
          i++;
          if (i < len && (s[i] == '+' || s[i] == '-')) i++;
          while (i < len && isdigit((unsigned char)s[i])) i++;
        }
      }
      if (has_dot_or_exp && i < len && (s[i] == 'h' || s[i] == 'H') &&
          (i + 1 >= len || !is_ident_char(s[i + 1]))) {
        ext->uses_fp16 = true;
        sb_append(out, "float16_t(");
        sb_append_n(out, s + st, i - st);
        sb_append_c(out, ')');
        i++;
        prev_nonws = ')';
        continue;
      }
      while (i < len && is_ident_char(s[i])) i++;
      sb_append_n(out, s + st, i - st);
      prev_nonws = s[i - 1];
      continue;
    }
    if (is_ident_start(s[i])) {
      size_t st = i;
      while (i < len && is_ident_char(s[i])) i++;
      size_t tl = i - st;
      if (prev_nonws != '.') {
        bool replaced = false;
        for (size_t r = 0; r < num_remaps; r++) {
          if (strlen(remaps[r].from) == tl && memcmp(s + st, remaps[r].from, tl) == 0) {
            sb_append(out, remaps[r].to);
            replaced = true;
            break;
          }
        }
        if (!replaced) {
          const char *mapped = map_msl_ident(s + st, tl, ext);
          if (mapped) sb_append(out, mapped);
          else sb_append_n(out, s + st, tl);
        }
      } else {
        sb_append_n(out, s + st, tl);
      }
      prev_nonws = 'x';
      continue;
    }
    char c = s[i++];
    sb_append_c(out, c);
    if (!isspace((unsigned char)c)) prev_nonws = c;
  }
}

static const char *default_zero_for_type(const char *t) {
  if (strcmp(t, "bool") == 0) return "false";
  if (strcmp(t, "float") == 0) return "0.0";
  if (strcmp(t, "float16_t") == 0) return "float16_t(0.0)";
  if (strcmp(t, "uint") == 0) return "0u";
  return "0";
}

static bool translate_top_level(const char *s,
                                size_t len,
                                ext_flags_t *ext,
                                strbuf_t *out,
                                char *err_buf,
                                size_t err_cap) {
  size_t i = 0;
  while (i < len) {
    if (s[i] == '#') {
      size_t ls = i;
      while (i < len && s[i] != '\n') i++;
      if (i < len) i++;
      sb_append_n(out, s + ls, i - ls);
      continue;
    }
    if (match_word_at(s, len, i, "using")) {
      size_t p = i + 5;
      while (p < len && s[p] != ';') p++;
      if (p < len) p++;
      i = p;
      continue;
    }
    if (match_word_at(s, len, i, "constant")) {
      size_t semi = i;
      while (semi < len && s[semi] != ';' && s[semi] != '{') semi++;
      if (semi < len && s[semi] == ';') {
        const char *fc = strstr(s + i, "function_constant");
        if (fc && fc < s + semi) {
          const char *lpar = strchr(fc, '(');
          unsigned long cid = 0;
          if (!lpar || lpar >= s + semi || !parse_uint_in_parens(lpar + 1, &cid)) {
            snprintf(err_buf, err_cap, "malformed [[function_constant(...)]] index");
            return false;
          }
          if (cid > 65535) {
            snprintf(err_buf, err_cap, "function_constant index %lu out of allowed range", cid);
            return false;
          }
          const char *br = strstr(s + i, "[[");
          if (!br || br >= s + semi) {
            snprintf(err_buf, err_cap, "malformed function_constant declaration");
            return false;
          }
          const char *p = s + i + 8;
          while (p < br && isspace((unsigned char)*p)) p++;
          const char *end = br;
          while (end > p && isspace((unsigned char)end[-1])) end--;
          const char *name_s = end;
          while (name_s > p && is_ident_char(name_s[-1])) name_s--;
          char name[64] = {0};
          size_t nl = (size_t)(end - name_s);
          if (nl == 0 || nl >= sizeof(name)) {
            snprintf(err_buf, err_cap, "invalid function_constant name");
            return false;
          }
          memcpy(name, name_s, nl);
          const char *esc_name = map_msl_ident(name, nl, NULL);
          char tbuf[64] = {0};
          translate_type_str(p, (size_t)(name_s - p), tbuf, sizeof(tbuf), ext);
          sb_printf(out, "layout(constant_id = %lu) const %s %s = %s;\n",
                    cid, tbuf, esc_name ? esc_name : name, default_zero_for_type(tbuf));
          i = semi + 1;
          continue;
        }
        sb_append(out, "const");
        i += 8;
        continue;
      }
    }
    size_t st = i;
    while (i < len && s[i] != '\n') i++;
    if (i < len) i++;
    rewrite_code_tokens(s + st, i - st, NULL, 0, ext, out);
  }
  return true;
}

/* Linear-time hoisting of `threadgroup <T> <decl>;` statements out of kernel body. */
static void hoist_threadgroup_vars(const char *body,
                                   size_t len,
                                   const ident_remap_t *remaps,
                                   size_t num_remaps,
                                   ext_flags_t *ext,
                                   strbuf_t *shared_out,
                                   strbuf_t *body_out) {
  size_t i = 0;
  size_t seg_start = 0;
  while (i < len) {
    if (match_word_at(body, len, i, "threadgroup")) {
      size_t semi = i + 11;
      while (semi < len && body[semi] != ';') semi++;
      if (semi >= len) break;
      if (i > seg_start) {
        rewrite_code_tokens(body + seg_start, i - seg_start, remaps, num_remaps, ext, body_out);
      }
      sb_append(shared_out, "shared ");
      rewrite_code_tokens(body + i + 11, semi - (i + 11), remaps, num_remaps, ext, shared_out);
      sb_append(shared_out, ";\n");
      i = semi + 1;
      seg_start = i;
      continue;
    }
    i++;
  }
  if (len > seg_start) {
    rewrite_code_tokens(body + seg_start, len - seg_start, remaps, num_remaps, ext, body_out);
  }
}

static bool parse_kernel_params(const char *params,
                                size_t len,
                                ext_flags_t *ext,
                                uint32_t *binding_mask,
                                uint32_t *readonly_mask,
                                strbuf_t *bindings_out,
                                strbuf_t *shared_out,
                                strbuf_t *prologue_out,
                                ident_remap_t *remaps,
                                size_t *num_remaps,
                                size_t max_remaps,
                                char *err_buf,
                                size_t err_cap) {
  size_t i = 0;
  while (i < len) {
    size_t st = i;
    int depth = 0;
    while (i < len) {
      if (params[i] == '(' || params[i] == '[' || params[i] == '<') depth++;
      else if (params[i] == ')' || params[i] == ']' || params[i] == '>') depth--;
      else if (params[i] == ',' && depth == 0) break;
      i++;
    }
    size_t plen = i - st;
    if (i < len && params[i] == ',') i++;

    const char *p = params + st;
    while (plen > 0 && isspace((unsigned char)*p)) { p++; plen--; }
    while (plen > 0 && isspace((unsigned char)p[plen - 1])) plen--;
    if (plen == 0) continue;

    const char *attr_open = NULL;
    const char *attr_close = NULL;
    for (size_t k = 0; k + 1 < plen; k++) {
      if (p[k] == '[' && p[k + 1] == '[') {
        attr_open = p + k + 2;
        for (size_t m = k + 2; m + 1 < plen; m++) {
          if (p[m] == ']' && p[m + 1] == ']') {
            attr_close = p + m;
            break;
          }
        }
        break;
      }
    }
    if (!attr_open || !attr_close) {
      snprintf(err_buf, err_cap, "kernel parameter missing [[...]] attribute");
      return false;
    }

    char attr[128] = {0};
    size_t alen = (size_t)(attr_close - attr_open);
    if (alen >= sizeof(attr)) alen = sizeof(attr) - 1;
    memcpy(attr, attr_open, alen);

    const char *decl_end = attr_open - 2;
    while (decl_end > p && isspace((unsigned char)decl_end[-1])) decl_end--;
    const char *name_s = decl_end;
    while (name_s > p && is_ident_char(name_s[-1])) name_s--;
    if (name_s == decl_end) {
      snprintf(err_buf, err_cap, "kernel parameter missing identifier name");
      return false;
    }
    char pname[64] = {0};
    size_t pnl = (size_t)(decl_end - name_s);
    if (pnl >= sizeof(pname)) pnl = sizeof(pname) - 1;
    memcpy(pname, name_s, pnl);
    const char *esc_pname = map_msl_ident(pname, pnl, NULL);
    const char *glsl_pname = esc_pname ? esc_pname : pname;

    const char *type_end = name_s;
    bool is_ptr = false;
    bool is_ref = false;
    bool is_const = false;
    for (const char *q = p; q < type_end; q++) {
      if (*q == '*') is_ptr = true;
      if (*q == '&') is_ref = true;
    }
    for (size_t k = 0; k < (size_t)(type_end - p); k++) {
      if (match_word_at(p, (size_t)(type_end - p), k, "const") ||
          match_word_at(p, (size_t)(type_end - p), k, "constant")) {
        is_const = true;
        break;
      }
    }

    char tbuf[128] = {0};
    translate_type_str(p, (size_t)(type_end - p), tbuf, sizeof(tbuf), ext);

    if (strncmp(attr, "buffer(", 7) == 0) {
      unsigned long slot = 0;
      if (!parse_uint_in_parens(attr + 7, &slot) || slot >= 31) {
        snprintf(err_buf, err_cap, "invalid or out-of-range buffer attribute [[%s]]", attr);
        return false;
      }
      *binding_mask |= (1u << slot);
      if (is_ref && !is_ptr) {
        *readonly_mask |= (1u << slot);
        sb_printf(bindings_out,
                  "layout(std430, set = 0, binding = %lu) readonly buffer _KmtlUBuf%lu { %s val; } _kmtl_ubuf%lu;\n",
                  slot, slot, tbuf, slot);
        if (*num_remaps < max_remaps) {
          snprintf(remaps[*num_remaps].from, sizeof(remaps[*num_remaps].from), "%s", pname);
          snprintf(remaps[*num_remaps].to, sizeof(remaps[*num_remaps].to), "_kmtl_ubuf%lu.val", slot);
          (*num_remaps)++;
        }
      } else {
        if (is_const) *readonly_mask |= (1u << slot);
        sb_printf(bindings_out,
                  "layout(std430, set = 0, binding = %lu) %sbuffer _KmtlBuf%lu { %s data[]; } _kmtl_buf%lu;\n",
                  slot, is_const ? "readonly " : "", slot, tbuf, slot);
        if (*num_remaps < max_remaps) {
          snprintf(remaps[*num_remaps].from, sizeof(remaps[*num_remaps].from), "%s", pname);
          snprintf(remaps[*num_remaps].to, sizeof(remaps[*num_remaps].to), "_kmtl_buf%lu.data", slot);
          (*num_remaps)++;
        }
      }
    } else if (strncmp(attr, "threadgroup(", 12) == 0) {
      unsigned long idx = 0;
      if (!parse_uint_in_parens(attr + 12, &idx) || idx >= 31) {
        snprintf(err_buf, err_cap, "invalid threadgroup attribute [[%s]]", attr);
        return false;
      }
      sb_printf(shared_out, "shared %s _kmtl_tg%lu[4096];\n", tbuf, idx);
      if (*num_remaps < max_remaps) {
        snprintf(remaps[*num_remaps].from, sizeof(remaps[*num_remaps].from), "%s", pname);
        snprintf(remaps[*num_remaps].to, sizeof(remaps[*num_remaps].to), "_kmtl_tg%lu", idx);
        (*num_remaps)++;
      }
    } else if (strcmp(attr, "thread_position_in_grid") == 0) {
      const char *sw = strcmp(tbuf, "uvec3") == 0 ? "" : (strcmp(tbuf, "uvec2") == 0 ? ".xy" : ".x");
      sb_printf(prologue_out, "  %s %s = gl_GlobalInvocationID%s;\n", tbuf, glsl_pname, sw);
    } else if (strcmp(attr, "thread_position_in_threadgroup") == 0) {
      const char *sw = strcmp(tbuf, "uvec3") == 0 ? "" : (strcmp(tbuf, "uvec2") == 0 ? ".xy" : ".x");
      sb_printf(prologue_out, "  %s %s = gl_LocalInvocationID%s;\n", tbuf, glsl_pname, sw);
    } else if (strcmp(attr, "threadgroup_position_in_grid") == 0) {
      const char *sw = strcmp(tbuf, "uvec3") == 0 ? "" : (strcmp(tbuf, "uvec2") == 0 ? ".xy" : ".x");
      sb_printf(prologue_out, "  %s %s = gl_WorkGroupID%s;\n", tbuf, glsl_pname, sw);
    } else if (strcmp(attr, "thread_index_in_threadgroup") == 0) {
      sb_printf(prologue_out, "  %s %s = gl_LocalInvocationIndex;\n", tbuf, glsl_pname);
    } else if (strcmp(attr, "threads_per_threadgroup") == 0) {
      const char *sw = strcmp(tbuf, "uvec3") == 0 ? "" : (strcmp(tbuf, "uvec2") == 0 ? ".xy" : ".x");
      sb_printf(prologue_out, "  %s %s = gl_WorkGroupSize%s;\n", tbuf, glsl_pname, sw);
    } else if (strcmp(attr, "threads_per_grid") == 0) {
      const char *sw = strcmp(tbuf, "uvec3") == 0 ? "" : (strcmp(tbuf, "uvec2") == 0 ? ".xy" : ".x");
      sb_printf(prologue_out, "  %s %s = (gl_NumWorkGroups * gl_WorkGroupSize)%s;\n", tbuf, glsl_pname, sw);
    } else if (strcmp(attr, "thread_index_in_simdgroup") == 0) {
      ext->uses_subgroup = true;
      sb_printf(prologue_out, "  %s %s = gl_SubgroupInvocationID;\n", tbuf, glsl_pname);
    } else if (strcmp(attr, "simdgroup_index_in_threadgroup") == 0) {
      ext->uses_subgroup = true;
      sb_printf(prologue_out, "  %s %s = gl_SubgroupID;\n", tbuf, glsl_pname);
    } else if (strcmp(attr, "threads_per_simdgroup") == 0) {
      ext->uses_subgroup = true;
      sb_printf(prologue_out, "  %s %s = gl_SubgroupSize;\n", tbuf, glsl_pname);
    } else if (strcmp(attr, "simdgroups_per_threadgroup") == 0) {
      ext->uses_subgroup = true;
      sb_printf(prologue_out, "  %s %s = gl_NumSubgroups;\n", tbuf, glsl_pname);
    } else {
      snprintf(err_buf, err_cap, "unsupported Metal kernel attribute [[%s]]", attr);
      return false;
    }
  }
  return true;
}

bool msl_translate_kernel(const char *src,
                          size_t src_len,
                          const msl_macro_t *macros,
                          size_t num_macros,
                          const char *kernel_name,
                          msl_translate_result_t *out,
                          char *err_buf,
                          size_t err_cap) {
  memset(out, 0, sizeof(*out));
  if (!src || src_len == 0 || !kernel_name || !kernel_name[0]) {
    snprintf(err_buf, err_cap, "empty MSL source or kernel name");
    return false;
  }
  if (!validate_macros(macros, num_macros, err_buf, err_cap)) {
    return false;
  }

  strbuf_t stripped;
  if (!strip_comments(src, src_len, &stripped)) {
    sb_free(&stripped);
    snprintf(err_buf, err_cap, "out of memory stripping comments");
    return false;
  }
  strbuf_t clean;
  if (!sanitize_directives(stripped.data, stripped.len, &clean, err_buf, err_cap)) {
    sb_free(&stripped);
    return false;
  }
  sb_free(&stripped);

  kernel_span_t *spans = NULL;
  size_t n_kernels = 0;
  if (!find_kernels(clean.data, clean.len, &spans, &n_kernels)) {
    sb_free(&clean);
    snprintf(err_buf, err_cap, "failed scanning MSL kernels");
    return false;
  }
  size_t target_idx = n_kernels;
  for (size_t i = 0; i < n_kernels; i++) {
    if (strcmp(spans[i].name, kernel_name) == 0) {
      target_idx = i;
      break;
    }
  }
  if (target_idx == n_kernels) {
    free(spans);
    sb_free(&clean);
    snprintf(err_buf, err_cap, "compute kernel '%s' not found in MSL library", kernel_name);
    return false;
  }

  ext_flags_t ext;
  memset(&ext, 0, sizeof(ext));

  strbuf_t top_level, bindings, shared_decls, prologue, body_glsl;
  sb_init(&top_level);
  sb_init(&bindings);
  sb_init(&shared_decls);
  sb_init(&prologue);
  sb_init(&body_glsl);

  bool ok = true;
  size_t cur = 0;
  for (size_t i = 0; i < n_kernels && ok; i++) {
    if (spans[i].start > cur) {
      ok = translate_top_level(clean.data + cur, spans[i].start - cur, &ext, &top_level, err_buf, err_cap);
    }
    cur = spans[i].body_close + 1;
  }
  if (ok && cur < clean.len) {
    ok = translate_top_level(clean.data + cur, clean.len - cur, &ext, &top_level, err_buf, err_cap);
  }

  kernel_span_t *k = &spans[target_idx];
  ident_remap_t remaps[48];
  size_t num_remaps = 0;
  uint32_t binding_mask = 0;
  uint32_t readonly_mask = 0;

  if (ok) {
    ok = parse_kernel_params(clean.data + k->params_open + 1,
                             k->params_close - k->params_open - 1,
                             &ext,
                             &binding_mask,
                             &readonly_mask,
                             &bindings,
                             &shared_decls,
                             &prologue,
                             remaps,
                             &num_remaps,
                             48,
                             err_buf,
                             err_cap);
  }
  if (ok) {
    hoist_threadgroup_vars(clean.data + k->body_open + 1,
                           k->body_close - k->body_open - 1,
                           remaps,
                           num_remaps,
                           &ext,
                           &shared_decls,
                           &body_glsl);
  }

  strbuf_t final_src;
  sb_init(&final_src);
  if (ok) {
    sb_append(&final_src, "#version 450\n");
    if (ext.uses_fp16) {
      sb_append(&final_src, "#extension GL_EXT_shader_explicit_arithmetic_types_float16 : enable\n");
      sb_append(&final_src, "#extension GL_EXT_shader_16bit_storage : enable\n");
    }
    if (ext.uses_int16) {
      sb_append(&final_src, "#extension GL_EXT_shader_explicit_arithmetic_types_int16 : enable\n");
      if (!ext.uses_fp16) sb_append(&final_src, "#extension GL_EXT_shader_16bit_storage : enable\n");
    }
    if (ext.uses_int8) {
      sb_append(&final_src, "#extension GL_EXT_shader_explicit_arithmetic_types_int8 : enable\n");
      sb_append(&final_src, "#extension GL_EXT_shader_8bit_storage : enable\n");
    }
    if (ext.uses_int64) {
      sb_append(&final_src, "#extension GL_EXT_shader_explicit_arithmetic_types_int64 : enable\n");
    }
    if (ext.uses_subgroup) {
      sb_append(&final_src, "#extension GL_KHR_shader_subgroup_basic : enable\n");
      sb_append(&final_src, "#extension GL_KHR_shader_subgroup_arithmetic : enable\n");
      sb_append(&final_src, "#extension GL_KHR_shader_subgroup_ballot : enable\n");
      sb_append(&final_src, "#extension GL_KHR_shader_subgroup_shuffle : enable\n");
    }
    sb_append(&final_src, "layout(local_size_x_id = 65536, local_size_y_id = 65537, local_size_z_id = 65538) in;\n");
    size_t preamble_end = final_src.len;
    for (size_t m = 0; m < num_macros; m++) {
      sb_append(&final_src, "#define ");
      sb_append_n(&final_src, macros[m].key, macros[m].key_len);
      if (macros[m].val && macros[m].val_len > 0) {
        sb_append_c(&final_src, ' ');
        sb_append_n(&final_src, macros[m].val, macros[m].val_len);
      }
      sb_append_c(&final_src, '\n');
    }
    if (top_level.len > 0) sb_append_n(&final_src, top_level.data, top_level.len);
    sb_append_c(&final_src, '\n');
    if (bindings.len > 0) sb_append_n(&final_src, bindings.data, bindings.len);
    if (shared_decls.len > 0) sb_append_n(&final_src, shared_decls.data, shared_decls.len);
    sb_append(&final_src, "void main() {\n");
    if (prologue.len > 0) sb_append_n(&final_src, prologue.data, prologue.len);
    if (body_glsl.len > 0) sb_append_n(&final_src, body_glsl.data, body_glsl.len);
    sb_append(&final_src, "\n}\n");
    if (!final_src.oom && preamble_end <= final_src.len) {
      ok = verify_emitted_user_glsl(final_src.data + preamble_end, final_src.len - preamble_end, err_buf, err_cap);
    }
  }

  if (ok && (top_level.oom || bindings.oom || shared_decls.oom || prologue.oom || body_glsl.oom || final_src.oom)) {
    snprintf(err_buf, err_cap, "out of memory translating MSL shader");
    ok = false;
  }

  free(spans);
  sb_free(&clean);
  sb_free(&top_level);
  sb_free(&bindings);
  sb_free(&shared_decls);
  sb_free(&prologue);
  sb_free(&body_glsl);

  if (!ok) {
    sb_free(&final_src);
    return false;
  }
  out->glsl = final_src.data;
  out->glsl_len = final_src.len;
  out->binding_mask = binding_mask;
  out->readonly_mask = readonly_mask;
  return true;
}
