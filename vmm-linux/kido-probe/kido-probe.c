/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 *
 * kido-probe: standalone zero-dependency ARM64 Linux hardware probe.
 * Inspects CPU topology, /dev/kvm & VGIC support, Vulkan compute/limits/features,
 * zero-copy memory tiers (udmabuf / VK_EXT_external_memory_host), and live
 * CPU<->GPU DMA coherency. Outputs either a concise terminal summary or a
 * schema-compatible board JSON record for kido.sh documentation.
 */
#define _GNU_SOURCE
#define VK_NO_PROTOTYPES

#include <ctype.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <linux/kvm.h>
#include <linux/memfd.h>
#include <linux/udmabuf.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <unistd.h>
#include <vulkan/vulkan.h>

#ifndef KVM_CAP_ARM_VM_IPA_SIZE
#define KVM_CAP_ARM_VM_IPA_SIZE 165
#endif
#ifndef KVM_CAP_ARM_PMU_V3
#define KVM_CAP_ARM_PMU_V3 126
#endif
#ifndef KVM_DEV_TYPE_ARM_VGIC_V2
#define KVM_DEV_TYPE_ARM_VGIC_V2 5
#endif
#ifndef KVM_DEV_TYPE_ARM_VGIC_V3
#define KVM_DEV_TYPE_ARM_VGIC_V3 7
#endif
#ifndef KVM_CREATE_DEVICE_TEST
#define KVM_CREATE_DEVICE_TEST 1
#endif
#ifndef VK_SUBGROUP_FEATURE_ROTATE_BIT
#define VK_SUBGROUP_FEATURE_ROTATE_BIT 0x00000200u
#endif
#ifndef VK_SUBGROUP_FEATURE_ROTATE_CLUSTERED_BIT
#define VK_SUBGROUP_FEATURE_ROTATE_CLUSTERED_BIT 0x00000400u
#endif

typedef struct {
  uint32_t impl;
  uint32_t part;
  uint32_t tier;
  uint32_t capacity;
  uint32_t max_freq_khz;
  uint32_t count;
  char name[64];
} cpu_cluster_t;

typedef struct {
  char board_model[256];
  char vendor[128];
  char kernel_release[128];
  char os_name[256];
  char machine[128];
  uint32_t host_page_size;
  uint64_t total_ram_mb;

  uint32_t n_cpus;
  cpu_cluster_t clusters[16];
  uint32_t n_clusters;
  bool heterogeneous;
  char cpu_summary[256];

  bool kvm_present;
  bool kvm_accessible;
  int kvm_errno;
  int kvm_api_version;
  int kvm_ipa_bits;
  bool kvm_pmu_v3;
  bool kvm_gicv2;
  bool kvm_gicv3;
  char arch_summary[128];

  bool vk_lib_loaded;
  bool gpu_found;
  bool gpu_is_cpu;
  char gpu_name[256];
  uint32_t vendor_id;
  uint32_t device_id;
  uint32_t vk_api_ver;
  char driver_name[256];
  char driver_info[256];
  char conformance[64];

  uint32_t max_tg_invocations;
  uint32_t max_tg_size[3];
  uint32_t max_tg_mem_bytes;
  uint32_t max_ssbo_per_stage;
  uint32_t max_ssbo_update_after_bind;
  uint32_t max_image_2d;
  uint32_t max_viewports;
  uint32_t max_msaa;
  uint64_t max_storage_range;
  uint64_t sparse_va_bytes;
  uint32_t subgroup_size;
  uint32_t subgroup_ops;
  bool has_timestamps;
  double timestamp_period_ns;

  bool has_fp16;
  bool has_storage_16bit;
  bool has_int8;
  bool has_storage_8bit;
  bool has_int64;
  bool has_dot4x8;
  bool has_bda;
  bool has_update_after_bind;
  bool has_etc2;
  bool has_astc_ldr;
  bool has_astc_hdr;
  bool has_bc;
  bool has_tessellation;
  bool has_mesh_shader;
  bool has_ray_query;
  bool has_coop_matrix;
  bool has_bfloat16;
  bool has_float_atomics;
  bool has_float_atomic_add;
  bool has_int64_buf_atomics;
  bool has_int64_shared_atomics;
  bool has_int64_img_atomics;
  bool has_vk_mem_model;
  bool has_sync2;
  bool has_sampler_minmax;
  bool has_depth_bounds;
  bool has_fp32_denorm;

  bool has_dmabuf_ext;
  bool has_host_ptr_ext;
  uint64_t host_ptr_align;
  uint64_t buf_extra_pad;
  bool udmabuf_accessible;
  int udmabuf_limit_mb;
  char zero_copy_tier[32];

  bool fw_dma_coherent;
  bool coherency_tested;
  bool coherency_passed;
  bool dma_coherent;
  bool has_pci_bus_info;
  uint32_t pci_domain, pci_bus, pci_device, pci_function;
  bool has_drm_props;
  int64_t drm_primary_major, drm_primary_minor;
  int64_t drm_render_major, drm_render_minor;
} probe_info_t;

static bool read_text_file(const char *path, char *buf, size_t cap) {
  if (cap < 2) return false;
  int fd = open(path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) return false;
  ssize_t n = read(fd, buf, cap - 1);
  close(fd);
  if (n <= 0) return false;
  buf[n] = '\0';
  while (n > 0 && (buf[n - 1] == '\n' || buf[n - 1] == '\r' || buf[n - 1] == ' ' || buf[n - 1] == '\0')) {
    buf[--n] = '\0';
  }
  return n > 0;
}

static uint64_t parse_u64_str(const char *s) {
  if (!s) return 0;
  while (*s == ' ' || *s == '\t' || *s == '\n' || *s == '\r') s++;
  uint64_t base = 10;
  if (s[0] == '0' && (s[1] == 'x' || s[1] == 'X')) {
    base = 16;
    s += 2;
  }
  uint64_t acc = 0;
  for (; *s; s++) {
    unsigned char c = (unsigned char)*s;
    uint64_t d;
    if (c >= '0' && c <= '9') d = (uint64_t)(c - '0');
    else if (base == 16 && c >= 'a' && c <= 'f') d = (uint64_t)(c - 'a' + 10);
    else if (base == 16 && c >= 'A' && c <= 'F') d = (uint64_t)(c - 'A' + 10);
    else break;
    if (acc > (UINT64_MAX - d) / base) return UINT64_MAX;
    acc = acc * base + d;
  }
  return acc;
}

static uint32_t read_u32_file(const char *path) {
  char txt[64];
  if (!read_text_file(path, txt, sizeof(txt))) return 0;
  uint64_t v = parse_u64_str(txt);
  return (v > UINT32_MAX) ? UINT32_MAX : (uint32_t)v;
}

static bool path_exists(const char *p) {
  struct stat st;
  return stat(p, &st) == 0;
}

static uint64_t align_up(uint64_t v, uint64_t a) {
  if (a <= 1) return v;
  uint64_t r = v % a;
  if (r == 0) return v;
  if (v > UINT64_MAX - (a - r)) return 0;
  return v + (a - r);
}

static void make_slug(const char *src, char *dst, size_t cap) {
  size_t out = 0;
  bool prev_dash = true;
  for (size_t i = 0; src[i] != '\0' && out + 1 < cap; i++) {
    unsigned char c = (unsigned char)src[i];
    if (isalnum(c)) {
      dst[out++] = (char)tolower(c);
      prev_dash = false;
    } else if (!prev_dash && out + 1 < cap) {
      dst[out++] = '-';
      prev_dash = true;
    }
  }
  while (out > 0 && dst[out - 1] == '-') out--;
  if (out == 0) snprintf(dst, cap, "arm64-linux-board");
  else dst[out] = '\0';
}

static const char *arm_cpu_part_name(uint32_t impl, uint32_t part) {
  if (impl == 0x41) { /* Arm Limited */
    switch (part) {
    case 0xd03: return "Cortex-A53";
    case 0xd04: return "Cortex-A35";
    case 0xd05: return "Cortex-A55";
    case 0xd07: return "Cortex-A57";
    case 0xd08: return "Cortex-A72";
    case 0xd09: return "Cortex-A73";
    case 0xd0a: return "Cortex-A75";
    case 0xd0b: return "Cortex-A76";
    case 0xd0c: return "Neoverse-N1";
    case 0xd0d: return "Cortex-A77";
    case 0xd40: return "Neoverse-V1";
    case 0xd41: return "Cortex-A78";
    case 0xd42: return "Cortex-A78AE";
    case 0xd44: return "Cortex-X1";
    case 0xd46: return "Cortex-A510";
    case 0xd47: return "Cortex-A710";
    case 0xd48: return "Cortex-X2";
    case 0xd49: return "Neoverse-N2";
    case 0xd4a: return "Neoverse-E1";
    case 0xd4b: return "Cortex-A78C";
    case 0xd4d: return "Cortex-A715";
    case 0xd4e: return "Cortex-X3";
    case 0xd4f: return "Neoverse-V2";
    case 0xd80: return "Cortex-A520";
    case 0xd81: return "Cortex-A720";
    case 0xd82: return "Cortex-X4";
    case 0xd83: return "Neoverse-V3AE";
    case 0xd84: return "Neoverse-V3";
    case 0xd85: return "Cortex-X925";
    case 0xd87: return "Cortex-A725";
    case 0xd8e: return "Neoverse-N3";
    default: break;
    }
  } else if (impl == 0x61) {
    return "Apple Silicon";
  } else if (impl == 0xc0) {
    if (part == 0xac3) return "Ampere-1";
    if (part == 0xac4) return "Ampere-1A";
    return "Ampere";
  } else if (impl == 0x51) {
    return "Qualcomm Kryo/Oryon";
  } else if (impl == 0x4e) {
    return "NVIDIA Carmel/Denver";
  }
  return NULL;
}

/* Matches vmm-linux/src/affinity.rs and internal/runtime/kvm_topology.go. */
static uint32_t arm_part_tier(uint32_t impl, uint32_t part) {
  if (impl != 0x41) return 0;
  switch (part) {
  case 0xd03: case 0xd04: case 0xd05: case 0xd46: case 0xd4a: case 0xd80:
    return 1;
  case 0xd07: case 0xd08: case 0xd09: case 0xd0a: case 0xd0b: case 0xd0c:
  case 0xd0d: case 0xd41: case 0xd42: case 0xd47: case 0xd49: case 0xd4b:
  case 0xd4d: case 0xd81: case 0xd87: case 0xd8e:
    return 2;
  case 0xd40: case 0xd44: case 0xd48: case 0xd4e: case 0xd4f: case 0xd82:
  case 0xd83: case 0xd84: case 0xd85:
    return 3;
  default:
    return 0;
  }
}

static bool cluster_ranks_ahead(const cpu_cluster_t *a, const cpu_cluster_t *b) {
  if (a->capacity != b->capacity) return a->capacity > b->capacity;
  if (a->max_freq_khz != b->max_freq_khz) return a->max_freq_khz > b->max_freq_khz;
  if (a->tier != b->tier) return a->tier > b->tier;
  if (a->count != b->count) return a->count > b->count;
  return a->part > b->part;
}

static void add_cpu_core(probe_info_t *p, uint32_t impl, uint32_t part, uint32_t cap, uint32_t freq_khz) {
  p->n_cpus++;
  for (uint32_t i = 0; i < p->n_clusters; i++) {
    if (p->clusters[i].impl == impl && p->clusters[i].part == part) {
      p->clusters[i].count++;
      if (cap > p->clusters[i].capacity) p->clusters[i].capacity = cap;
      if (freq_khz > p->clusters[i].max_freq_khz) p->clusters[i].max_freq_khz = freq_khz;
      return;
    }
  }
  if (p->n_clusters >= 16) return;
  cpu_cluster_t *c = &p->clusters[p->n_clusters++];
  c->impl = impl;
  c->part = part;
  c->tier = arm_part_tier(impl, part);
  c->capacity = cap;
  c->max_freq_khz = freq_khz;
  c->count = 1;
  const char *nm = arm_cpu_part_name(impl, part);
  if (nm) snprintf(c->name, sizeof(c->name), "%s", nm);
  else snprintf(c->name, sizeof(c->name), "ARM64 (0x%02x:0x%03x)", impl, part);
}

static void probe_system_and_cpu(probe_info_t *p) {
  struct utsname uts;
  memset(&uts, 0, sizeof(uts));
  if (uname(&uts) == 0) {
    snprintf(p->kernel_release, sizeof(p->kernel_release), "%s", uts.release);
    snprintf(p->machine, sizeof(p->machine), "%s", uts.machine);
  }
  p->host_page_size = (uint32_t)getpagesize();
  long pages = sysconf(_SC_PHYS_PAGES);
  if (pages > 0) {
    p->total_ram_mb = ((uint64_t)pages * (uint64_t)p->host_page_size) >> 20;
  }

  /* Board model & vendor */
  if (!read_text_file("/proc/device-tree/model", p->board_model, sizeof(p->board_model))) {
    if (!read_text_file("/sys/class/dmi/id/product_name", p->board_model, sizeof(p->board_model))) {
      snprintf(p->board_model, sizeof(p->board_model), "Linux %s", p->machine[0] ? p->machine : "arm64");
    }
  }
  if (!read_text_file("/sys/class/dmi/id/sys_vendor", p->vendor, sizeof(p->vendor))) {
    if (strcasestr(p->board_model, "Raspberry Pi")) snprintf(p->vendor, sizeof(p->vendor), "Raspberry Pi / Broadcom");
    else if (strcasestr(p->board_model, "RK3588") || strcasestr(p->board_model, "Rock") ||
             strcasestr(p->board_model, "Orange Pi 5") || strcasestr(p->board_model, "NanoPC"))
      snprintf(p->vendor, sizeof(p->vendor), "Rockchip");
    else if (strcasestr(p->board_model, "Radxa") || strcasestr(p->board_model, "Orion"))
      snprintf(p->vendor, sizeof(p->vendor), "Radxa / CIX");
    else if (strcasestr(p->board_model, "NVIDIA") || strcasestr(p->board_model, "Jetson") ||
             strcasestr(p->board_model, "Spark"))
      snprintf(p->vendor, sizeof(p->vendor), "NVIDIA");
    else if (strcasestr(p->board_model, "Apple"))
      snprintf(p->vendor, sizeof(p->vendor), "Apple");
    else
      snprintf(p->vendor, sizeof(p->vendor), "ARM64 Linux");
  }

  /* OS pretty name */
  FILE *osfp = fopen("/etc/os-release", "r");
  if (osfp) {
    char line[256];
    while (fgets(line, sizeof(line), osfp)) {
      if (strncmp(line, "PRETTY_NAME=", 12) == 0) {
        char *s = line + 12;
        if (*s == '"') s++;
        char *end = s + strlen(s);
        while (end > s && (end[-1] == '\n' || end[-1] == '\r' || end[-1] == '"')) *--end = '\0';
        snprintf(p->os_name, sizeof(p->os_name), "%s", s);
        break;
      }
    }
    fclose(osfp);
  }
  if (!p->os_name[0]) snprintf(p->os_name, sizeof(p->os_name), "Linux");

  /* CPU clusters via sysfs midr_el1 + capacity + max_freq, fallback to /proc/cpuinfo */
  long nconf = sysconf(_SC_NPROCESSORS_CONF);
  if (nconf < 1) nconf = 1;
  if (nconf > 256) nconf = 256;
  bool any_sysfs = false;
  for (long i = 0; i < nconf; i++) {
    char path[128], txt[64];
    snprintf(path, sizeof(path), "/sys/devices/system/cpu/cpu%ld/regs/identification/midr_el1", i);
    if (read_text_file(path, txt, sizeof(txt))) {
      uint64_t midr = parse_u64_str(txt);
      uint32_t impl = (uint32_t)((midr >> 24) & 0xffu);
      uint32_t part = (uint32_t)((midr >> 4) & 0xfffu);
      snprintf(path, sizeof(path), "/sys/devices/system/cpu/cpu%ld/cpu_capacity", i);
      uint32_t cap = read_u32_file(path);
      snprintf(path, sizeof(path), "/sys/devices/system/cpu/cpu%ld/cpufreq/cpuinfo_max_freq", i);
      uint32_t freq = read_u32_file(path);
      add_cpu_core(p, impl, part, cap, freq);
      any_sysfs = true;
    }
  }
  if (!any_sysfs) {
    FILE *cfp = fopen("/proc/cpuinfo", "r");
    if (cfp) {
      char line[256];
      uint32_t cur_impl = 0, cur_part = 0;
      bool have_impl = false, have_part = false;
      while (fgets(line, sizeof(line), cfp)) {
        if (strncmp(line, "CPU implementer", 15) == 0) {
          char *col = strchr(line, ':');
          if (col) { cur_impl = (uint32_t)parse_u64_str(col + 1); have_impl = true; }
        } else if (strncmp(line, "CPU part", 8) == 0) {
          char *col = strchr(line, ':');
          if (col) { cur_part = (uint32_t)parse_u64_str(col + 1); have_part = true; }
        }
        if (have_impl && have_part) {
          add_cpu_core(p, cur_impl, cur_part, 0, 0);
          have_impl = have_part = false;
        }
      }
      fclose(cfp);
    }
  }
  if (p->n_cpus == 0) {
    long onl = sysconf(_SC_NPROCESSORS_ONLN);
    p->n_cpus = (onl > 0) ? (uint32_t)onl : 1;
    snprintf(p->cpu_summary, sizeof(p->cpu_summary), "%ux %s", p->n_cpus, p->machine[0] ? p->machine : "arm64");
  } else {
    for (uint32_t i = 0; i < p->n_clusters; i++) {
      for (uint32_t j = i + 1; j < p->n_clusters; j++) {
        if (cluster_ranks_ahead(&p->clusters[j], &p->clusters[i])) {
          cpu_cluster_t tmp = p->clusters[i];
          p->clusters[i] = p->clusters[j];
          p->clusters[j] = tmp;
        }
      }
    }
    p->heterogeneous = (p->n_clusters > 1);
    size_t pos = 0;
    for (uint32_t i = 0; i < p->n_clusters; i++) {
      int w = snprintf(p->cpu_summary + pos, sizeof(p->cpu_summary) - pos, "%s%ux %s",
                       i ? " + " : "", p->clusters[i].count, p->clusters[i].name);
      if (w < 0 || (size_t)w >= sizeof(p->cpu_summary) - pos) break;
      pos += (size_t)w;
    }
  }
}

static void probe_kvm(probe_info_t *p) {
  p->kvm_present = path_exists("/dev/kvm");
  int fd = open("/dev/kvm", O_RDWR | O_CLOEXEC);
  if (fd < 0) {
    p->kvm_errno = errno;
    snprintf(p->arch_summary, sizeof(p->arch_summary), "ARM64 (KVM unavailable)");
    return;
  }
  p->kvm_accessible = true;
  p->kvm_api_version = ioctl(fd, KVM_GET_API_VERSION, 0);
  int ipa = ioctl(fd, KVM_CHECK_EXTENSION, KVM_CAP_ARM_VM_IPA_SIZE);
  p->kvm_ipa_bits = (ipa > 0) ? ipa : 40;
  p->kvm_pmu_v3 = ioctl(fd, KVM_CHECK_EXTENSION, KVM_CAP_ARM_PMU_V3) > 0;

  int vm_fd = ioctl(fd, KVM_CREATE_VM, (unsigned long)(ipa > 0 ? ipa : 0));
  if (vm_fd < 0) vm_fd = ioctl(fd, KVM_CREATE_VM, 0UL);
  if (vm_fd >= 0) {
    struct kvm_create_device d3 = {
        .type = KVM_DEV_TYPE_ARM_VGIC_V3,
        .flags = KVM_CREATE_DEVICE_TEST,
    };
    p->kvm_gicv3 = (ioctl(vm_fd, KVM_CREATE_DEVICE, &d3) == 0);
    struct kvm_create_device d2 = {
        .type = KVM_DEV_TYPE_ARM_VGIC_V2,
        .flags = KVM_CREATE_DEVICE_TEST,
    };
    p->kvm_gicv2 = (ioctl(vm_fd, KVM_CREATE_DEVICE, &d2) == 0);
    close(vm_fd);
  }
  close(fd);

  const char *gic = (p->kvm_gicv3 && p->kvm_gicv2) ? "GICv3/GICv2"
                    : p->kvm_gicv3                 ? "GICv3"
                    : p->kvm_gicv2                 ? "GICv2"
                                                   : "GIC unknown";
  snprintf(p->arch_summary, sizeof(p->arch_summary), "ARM64 (%s, %d-bit IPA)", gic, p->kvm_ipa_bits);
}

static bool check_of_node_dma_coherent(const char *dev_path, bool *out_found_of_node) {
  *out_found_of_node = false;
  char sys_cur[PATH_MAX], of_link[PATH_MAX], cur[PATH_MAX];
  if (!realpath(dev_path, sys_cur)) return false;
  bool resolved = false;
  for (int up = 0; up < 8; up++) {
    if (snprintf(of_link, sizeof(of_link), "%s/of_node", sys_cur) < (int)sizeof(of_link) &&
        realpath(of_link, cur) != NULL) {
      resolved = true;
      break;
    }
    if (strcmp(sys_cur, "/sys/devices") == 0 || strcmp(sys_cur, "/sys") == 0 || strcmp(sys_cur, "/") == 0) break;
    char *slash = strrchr(sys_cur, '/');
    if (!slash || slash == sys_cur) break;
    *slash = '\0';
  }
  if (!resolved) return false;
  *out_found_of_node = true;
  for (int depth = 0; depth < 16; depth++) {
    char prop[PATH_MAX];
    if (snprintf(prop, sizeof(prop), "%s/dma-noncoherent", cur) < (int)sizeof(prop) && path_exists(prop)) {
      return false;
    }
    if (snprintf(prop, sizeof(prop), "%s/dma-coherent", cur) < (int)sizeof(prop) && path_exists(prop)) {
      return true;
    }
    if (strcmp(cur, "/sys/firmware/devicetree/base") == 0 || strcmp(cur, "/") == 0) break;
    char *slash = strrchr(cur, '/');
    if (!slash || slash == cur) break;
    *slash = '\0';
  }
  return false;
}

static bool detect_fw_dma_coherent(const probe_info_t *p) {
#if defined(__aarch64__)
  char dev_path[PATH_MAX];
  dev_path[0] = '\0';
  if (p->has_pci_bus_info) {
    snprintf(dev_path, sizeof(dev_path), "/sys/bus/pci/devices/%04x:%02x:%02x.%u",
             p->pci_domain, p->pci_bus, p->pci_device, p->pci_function);
  } else if (p->has_drm_props && p->drm_render_major >= 0) {
    snprintf(dev_path, sizeof(dev_path), "/sys/dev/char/%lld:%lld/device",
             (long long)p->drm_render_major, (long long)p->drm_render_minor);
  } else if (p->has_drm_props && p->drm_primary_major >= 0) {
    snprintf(dev_path, sizeof(dev_path), "/sys/dev/char/%lld:%lld/device",
             (long long)p->drm_primary_major, (long long)p->drm_primary_minor);
  }
  if (dev_path[0] != '\0') {
    bool found_of = false;
    bool dt_coh = check_of_node_dma_coherent(dev_path, &found_of);
    if (found_of) return dt_coh;
  }
  if (!path_exists("/sys/firmware/devicetree/base") && path_exists("/sys/firmware/acpi")) {
    return true;
  }
  return false;
#else
  (void)p;
  return true;
#endif
}

static uint8_t *mmap_aligned_shm(int fd, uint64_t bytes, uint64_t align) {
  if (align > 4096u && (align & (align - 1u)) == 0 && bytes <= SIZE_MAX - align) {
    size_t reserve = (size_t)bytes + (size_t)align;
    void *raw = mmap(NULL, reserve, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (raw != MAP_FAILED) {
      uintptr_t base = ((uintptr_t)raw + (uintptr_t)(align - 1u)) & ~(uintptr_t)(align - 1u);
      void *mapped = mmap((void *)base, (size_t)bytes, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_FIXED, fd, 0);
      if (mapped != MAP_FAILED) {
        size_t head = (size_t)(base - (uintptr_t)raw);
        if (head > 0) munmap(raw, head);
        size_t tail = reserve - head - (size_t)bytes;
        if (tail > 0) munmap((uint8_t *)mapped + bytes, tail);
        return (uint8_t *)mapped;
      }
      munmap(raw, reserve);
    }
  }
  void *m = mmap(NULL, (size_t)bytes, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  return (m == MAP_FAILED) ? NULL : (uint8_t *)m;
}

static int find_mem_type(const VkPhysicalDeviceMemoryProperties *mp, uint32_t bits, VkMemoryPropertyFlags req) {
  uint32_t cnt = mp->memoryTypeCount;
  if (cnt > VK_MAX_MEMORY_TYPES) cnt = VK_MAX_MEMORY_TYPES;
  for (uint32_t i = 0; i < cnt; i++) {
    if ((bits & (1u << i)) && (mp->memoryTypes[i].propertyFlags & req) == req) return (int)i;
  }
  return -1;
}

static uint32_t max_sample_count(VkSampleCountFlags flags) {
  if (flags & VK_SAMPLE_COUNT_16_BIT) return 16;
  if (flags & VK_SAMPLE_COUNT_8_BIT) return 8;
  if (flags & VK_SAMPLE_COUNT_4_BIT) return 4;
  if (flags & VK_SAMPLE_COUNT_2_BIT) return 2;
  return 1;
}

static void probe_vulkan(probe_info_t *p, bool run_coh_test, bool force_coh_test) {
  snprintf(p->zero_copy_tier, sizeof(p->zero_copy_tier), "staging");
  p->drm_primary_major = p->drm_primary_minor = -1;
  p->drm_render_major = p->drm_render_minor = -1;

  char lim_txt[32];
  if (read_text_file("/sys/module/udmabuf/parameters/size_limit_mb", lim_txt, sizeof(lim_txt))) {
    uint64_t lim = parse_u64_str(lim_txt);
    p->udmabuf_limit_mb = (lim > INT_MAX) ? INT_MAX : (int)lim;
  }
  int ufd = open("/dev/udmabuf", O_RDWR | O_CLOEXEC);
  if (ufd >= 0) {
    p->udmabuf_accessible = true;
    close(ufd);
  }

  void *vklib = dlopen("libvulkan.so.1", RTLD_NOW | RTLD_LOCAL);
  if (!vklib) vklib = dlopen("libvulkan.so", RTLD_NOW | RTLD_LOCAL);
  if (!vklib) return;
  p->vk_lib_loaded = true;

  PFN_vkGetInstanceProcAddr gipa = (PFN_vkGetInstanceProcAddr)dlsym(vklib, "vkGetInstanceProcAddr");
  if (!gipa) { dlclose(vklib); return; }

  #define LOAD_GLOBAL(fn) PFN_##fn fn = (PFN_##fn)gipa(VK_NULL_HANDLE, #fn)
  LOAD_GLOBAL(vkCreateInstance);
  if (!vkCreateInstance) { dlclose(vklib); return; }

  uint32_t inst_api_ver = VK_API_VERSION_1_3;
  VkApplicationInfo app = {
      .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
      .pApplicationName = "kido-probe",
      .apiVersion = inst_api_ver,
  };
  VkInstanceCreateInfo ici = {
      .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
      .pApplicationInfo = &app,
  };
  VkInstance inst = VK_NULL_HANDLE;
  if (vkCreateInstance(&ici, NULL, &inst) != VK_SUCCESS) {
    inst_api_ver = VK_API_VERSION_1_1;
    app.apiVersion = inst_api_ver;
    if (vkCreateInstance(&ici, NULL, &inst) != VK_SUCCESS) {
      inst_api_ver = VK_API_VERSION_1_0;
      app.apiVersion = inst_api_ver;
      if (vkCreateInstance(&ici, NULL, &inst) != VK_SUCCESS) {
        dlclose(vklib);
        return;
      }
    }
  }

  #define LOAD_INST(fn) PFN_##fn fn = (PFN_##fn)gipa(inst, #fn)
  LOAD_INST(vkDestroyInstance);
  LOAD_INST(vkEnumeratePhysicalDevices);
  LOAD_INST(vkGetPhysicalDeviceProperties);
  LOAD_INST(vkGetPhysicalDeviceFeatures);
  LOAD_INST(vkGetPhysicalDeviceProperties2);
  LOAD_INST(vkGetPhysicalDeviceFeatures2);
  LOAD_INST(vkGetPhysicalDeviceMemoryProperties);
  LOAD_INST(vkGetPhysicalDeviceQueueFamilyProperties);
  LOAD_INST(vkEnumerateDeviceExtensionProperties);
  LOAD_INST(vkCreateDevice);
  LOAD_INST(vkGetDeviceProcAddr);
  if (!vkDestroyInstance || !vkEnumeratePhysicalDevices || !vkGetPhysicalDeviceProperties ||
      !vkGetPhysicalDeviceFeatures || !vkGetPhysicalDeviceMemoryProperties ||
      !vkGetPhysicalDeviceQueueFamilyProperties || !vkEnumerateDeviceExtensionProperties ||
      !vkCreateDevice || !vkGetDeviceProcAddr) {
    if (vkDestroyInstance) vkDestroyInstance(inst, NULL);
    dlclose(vklib);
    return;
  }

  uint32_t dcount = 0;
  VkResult er = vkEnumeratePhysicalDevices(inst, &dcount, NULL);
  if ((er != VK_SUCCESS && er != VK_INCOMPLETE) || dcount == 0) {
    vkDestroyInstance(inst, NULL);
    dlclose(vklib);
    return;
  }
  VkPhysicalDevice *devs = calloc(dcount, sizeof(*devs));
  if (!devs) { vkDestroyInstance(inst, NULL); dlclose(vklib); return; }
  er = vkEnumeratePhysicalDevices(inst, &dcount, devs);
  if ((er != VK_SUCCESS && er != VK_INCOMPLETE) || dcount == 0) {
    free(devs);
    vkDestroyInstance(inst, NULL);
    dlclose(vklib);
    return;
  }

  int best_idx = -1, best_score = -1;
  uint32_t best_qf = 0, best_ts_bits = 0;
  for (uint32_t i = 0; i < dcount; i++) {
    if (devs[i] == VK_NULL_HANDLE) continue;
    VkPhysicalDeviceProperties dp;
    memset(&dp, 0, sizeof(dp));
    vkGetPhysicalDeviceProperties(devs[i], &dp);
    uint32_t qcount = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, NULL);
    if (qcount == 0) continue;
    VkQueueFamilyProperties *qp = calloc(qcount, sizeof(*qp));
    if (!qp) continue;
    vkGetPhysicalDeviceQueueFamilyProperties(devs[i], &qcount, qp);
    int qf = -1;
    uint32_t ts_bits = 0;
    for (uint32_t q = 0; q < qcount; q++) {
      if (qp[q].queueFlags & VK_QUEUE_COMPUTE_BIT) {
        qf = (int)q;
        ts_bits = qp[q].timestampValidBits;
        break;
      }
    }
    free(qp);
    if (qf < 0) continue;
    int score = 1;
    if (dp.deviceType == VK_PHYSICAL_DEVICE_TYPE_DISCRETE_GPU) score = 100;
    else if (dp.deviceType == VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU) score = 80;
    else if (dp.deviceType == VK_PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU) score = 40;
    else if (dp.deviceType == VK_PHYSICAL_DEVICE_TYPE_CPU) score = 5;
    if (score > best_score) {
      best_score = score;
      best_idx = (int)i;
      best_qf = (uint32_t)qf;
      best_ts_bits = ts_bits;
    }
  }
  if (best_idx < 0) {
    free(devs);
    vkDestroyInstance(inst, NULL);
    dlclose(vklib);
    return;
  }

  VkPhysicalDevice pd = devs[best_idx];
  free(devs);
  p->gpu_found = true;

  VkPhysicalDeviceProperties base_props;
  memset(&base_props, 0, sizeof(base_props));
  vkGetPhysicalDeviceProperties(pd, &base_props);
  VkPhysicalDeviceMemoryProperties mem_props;
  memset(&mem_props, 0, sizeof(mem_props));
  vkGetPhysicalDeviceMemoryProperties(pd, &mem_props);

  p->gpu_is_cpu = (base_props.deviceType == VK_PHYSICAL_DEVICE_TYPE_CPU);
  snprintf(p->gpu_name, sizeof(p->gpu_name), "%s", base_props.deviceName);
  p->vendor_id = base_props.vendorID;
  p->device_id = base_props.deviceID;
  p->vk_api_ver = base_props.apiVersion;
  p->max_tg_invocations = base_props.limits.maxComputeWorkGroupInvocations;
  p->max_tg_size[0] = base_props.limits.maxComputeWorkGroupSize[0];
  p->max_tg_size[1] = base_props.limits.maxComputeWorkGroupSize[1];
  p->max_tg_size[2] = base_props.limits.maxComputeWorkGroupSize[2];
  p->max_tg_mem_bytes = base_props.limits.maxComputeSharedMemorySize;
  p->max_ssbo_per_stage = base_props.limits.maxPerStageDescriptorStorageBuffers;
  p->max_image_2d = base_props.limits.maxImageDimension2D;
  p->max_viewports = base_props.limits.maxViewports;
  p->max_msaa = max_sample_count(base_props.limits.framebufferColorSampleCounts);
  p->max_storage_range = base_props.limits.maxStorageBufferRange;
  p->sparse_va_bytes = base_props.limits.sparseAddressSpaceSize;
  if (best_ts_bits > 0 && best_ts_bits <= 64 &&
      base_props.limits.timestampPeriod > 0.0f && base_props.limits.timestampPeriod < 1e9f) {
    p->has_timestamps = true;
    p->timestamp_period_ns = (double)base_props.limits.timestampPeriod;
  }

  /* Enumerate device extensions */
  uint32_t ecount = 0;
  VkResult eer = vkEnumerateDeviceExtensionProperties(pd, NULL, &ecount, NULL);
  VkExtensionProperties *exts = (eer == VK_SUCCESS || eer == VK_INCOMPLETE) && ecount > 0
                                    ? calloc(ecount, sizeof(*exts))
                                    : NULL;
  bool ext_mem_fd = false, ext_dmabuf = false, ext_host_ptr = false;
  bool ext_pci = false, ext_drm = false, ext_driver = false;
  bool ext_atomic_float = false, ext_img_int64 = false;
  bool ext_coop_khr = false, ext_coop_nv = false;
  bool ext_mesh = false, ext_ray = false, ext_sync2 = false;
  bool ext_bf16_khr = false, ext_fp16_int8 = false, ext_8bit_storage = false;
  bool ext_bda = false, ext_desc_idx = false, ext_atomic_int64 = false;
  bool ext_vk_mem_model = false, ext_sampler_minmax = false, ext_float_ctrl = false;
  bool ext_dot_prod = false, ext_astc_hdr = false;
  if (exts && ecount > 0) {
    eer = vkEnumerateDeviceExtensionProperties(pd, NULL, &ecount, exts);
    if (eer == VK_SUCCESS || eer == VK_INCOMPLETE) {
      for (uint32_t i = 0; i < ecount; i++) {
        const char *en = exts[i].extensionName;
        if (strcmp(en, VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME) == 0) ext_mem_fd = true;
        else if (strcmp(en, VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME) == 0) ext_dmabuf = true;
        else if (strcmp(en, VK_EXT_EXTERNAL_MEMORY_HOST_EXTENSION_NAME) == 0) ext_host_ptr = true;
        else if (strcmp(en, VK_EXT_PCI_BUS_INFO_EXTENSION_NAME) == 0) ext_pci = true;
        else if (strcmp(en, VK_EXT_PHYSICAL_DEVICE_DRM_EXTENSION_NAME) == 0) ext_drm = true;
        else if (strcmp(en, VK_KHR_DRIVER_PROPERTIES_EXTENSION_NAME) == 0) ext_driver = true;
        else if (strcmp(en, "VK_EXT_shader_atomic_float") == 0) ext_atomic_float = true;
        else if (strcmp(en, "VK_EXT_shader_image_atomic_int64") == 0) ext_img_int64 = true;
        else if (strcmp(en, "VK_KHR_cooperative_matrix") == 0) ext_coop_khr = true;
        else if (strcmp(en, "VK_NV_cooperative_matrix") == 0) ext_coop_nv = true;
        else if (strcmp(en, "VK_EXT_mesh_shader") == 0) ext_mesh = true;
        else if (strcmp(en, "VK_KHR_ray_query") == 0) ext_ray = true;
        else if (strcmp(en, "VK_KHR_synchronization2") == 0) ext_sync2 = true;
        else if (strcmp(en, "VK_KHR_shader_bfloat16") == 0) ext_bf16_khr = true;
        else if (strcmp(en, "VK_KHR_shader_float16_int8") == 0) ext_fp16_int8 = true;
        else if (strcmp(en, "VK_KHR_8bit_storage") == 0) ext_8bit_storage = true;
        else if (strcmp(en, "VK_KHR_buffer_device_address") == 0) ext_bda = true;
        else if (strcmp(en, "VK_EXT_descriptor_indexing") == 0) ext_desc_idx = true;
        else if (strcmp(en, "VK_KHR_shader_atomic_int64") == 0) ext_atomic_int64 = true;
        else if (strcmp(en, "VK_KHR_vulkan_memory_model") == 0) ext_vk_mem_model = true;
        else if (strcmp(en, "VK_EXT_sampler_filter_minmax") == 0) ext_sampler_minmax = true;
        else if (strcmp(en, "VK_KHR_shader_float_controls") == 0) ext_float_ctrl = true;
        else if (strcmp(en, "VK_KHR_shader_integer_dot_product") == 0) ext_dot_prod = true;
        else if (strcmp(en, "VK_EXT_texture_compression_astc_hdr") == 0) ext_astc_hdr = true;
      }
    }
  }
  free(exts);
  p->has_dmabuf_ext = ext_mem_fd && ext_dmabuf;
  p->has_host_ptr_ext = ext_host_ptr;

  bool can_query2 = (inst_api_ver >= VK_API_VERSION_1_1 && base_props.apiVersion >= VK_API_VERSION_1_1 &&
                     vkGetPhysicalDeviceProperties2 && vkGetPhysicalDeviceFeatures2);

  if (!can_query2) {
    /* Vulkan 1.0 loader or ICD fallback: use core 1.0 features only. */
    VkPhysicalDeviceFeatures base_feats;
    memset(&base_feats, 0, sizeof(base_feats));
    vkGetPhysicalDeviceFeatures(pd, &base_feats);
    p->has_int64 = (base_feats.shaderInt64 == VK_TRUE);
    p->has_etc2 = (base_feats.textureCompressionETC2 == VK_TRUE);
    p->has_astc_ldr = (base_feats.textureCompressionASTC_LDR == VK_TRUE);
    p->has_bc = (base_feats.textureCompressionBC == VK_TRUE);
    p->has_tessellation = (base_feats.tessellationShader == VK_TRUE);
    p->has_depth_bounds = (base_feats.depthBounds == VK_TRUE);
  } else {
    /* Properties2 chain (Vulkan 1.1+ loader and ICD) */
    VkPhysicalDeviceSubgroupProperties sg_props = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SUBGROUP_PROPERTIES };
    VkPhysicalDeviceDriverProperties drv_props = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRIVER_PROPERTIES };
    VkPhysicalDeviceVulkan12Properties vk12_props = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_PROPERTIES };
    VkPhysicalDeviceVulkan13Properties vk13_props = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_PROPERTIES };
    VkPhysicalDeviceExternalMemoryHostPropertiesEXT hp_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_MEMORY_HOST_PROPERTIES_EXT,
    };
    VkPhysicalDevicePCIBusInfoPropertiesEXT pci_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PCI_BUS_INFO_PROPERTIES_EXT,
    };
    VkPhysicalDeviceDrmPropertiesEXT drm_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRM_PROPERTIES_EXT,
    };
#ifdef VK_EXT_descriptor_indexing
    VkPhysicalDeviceDescriptorIndexingPropertiesEXT di_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DESCRIPTOR_INDEXING_PROPERTIES_EXT,
    };
#endif
#ifdef VK_KHR_shader_float_controls
    VkPhysicalDeviceFloatControlsPropertiesKHR fc_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FLOAT_CONTROLS_PROPERTIES_KHR,
    };
#endif
#ifdef VK_KHR_shader_integer_dot_product
    VkPhysicalDeviceShaderIntegerDotProductPropertiesKHR dp_props = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_INTEGER_DOT_PRODUCT_PROPERTIES_KHR,
    };
#endif

    void *pchain = &sg_props;
    if (base_props.apiVersion >= VK_API_VERSION_1_2) {
      vk12_props.pNext = pchain; pchain = &vk12_props;
    } else {
      if (ext_driver) { drv_props.pNext = pchain; pchain = &drv_props; }
#ifdef VK_EXT_descriptor_indexing
      if (ext_desc_idx) { di_props.pNext = pchain; pchain = &di_props; }
#endif
#ifdef VK_KHR_shader_float_controls
      if (ext_float_ctrl) { fc_props.pNext = pchain; pchain = &fc_props; }
#endif
    }
    if (base_props.apiVersion >= VK_API_VERSION_1_3) {
      vk13_props.pNext = pchain; pchain = &vk13_props;
    } else {
#ifdef VK_KHR_shader_integer_dot_product
      if (ext_dot_prod) { dp_props.pNext = pchain; pchain = &dp_props; }
#endif
    }
    if (ext_host_ptr) { hp_props.pNext = pchain; pchain = &hp_props; }
    if (ext_pci) { pci_props.pNext = pchain; pchain = &pci_props; }
    if (ext_drm) { drm_props.pNext = pchain; pchain = &drm_props; }

    VkPhysicalDeviceProperties2 props2 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
        .pNext = pchain,
    };
    vkGetPhysicalDeviceProperties2(pd, &props2);

    p->subgroup_size = sg_props.subgroupSize;
    if (sg_props.supportedStages & VK_SHADER_STAGE_COMPUTE_BIT) {
      p->subgroup_ops = sg_props.supportedOperations;
    }
    if (base_props.apiVersion >= VK_API_VERSION_1_2) {
      snprintf(p->driver_name, sizeof(p->driver_name), "%s", vk12_props.driverName);
      snprintf(p->driver_info, sizeof(p->driver_info), "%s", vk12_props.driverInfo);
      if (vk12_props.conformanceVersion.major || vk12_props.conformanceVersion.minor ||
          vk12_props.conformanceVersion.subminor || vk12_props.conformanceVersion.patch) {
        snprintf(p->conformance, sizeof(p->conformance), "%u.%u.%u.%u",
                 vk12_props.conformanceVersion.major, vk12_props.conformanceVersion.minor,
                 vk12_props.conformanceVersion.subminor, vk12_props.conformanceVersion.patch);
      }
      p->max_ssbo_update_after_bind = vk12_props.maxPerStageDescriptorUpdateAfterBindStorageBuffers;
      p->has_fp32_denorm = (vk12_props.shaderDenormPreserveFloat32 == VK_TRUE);
    } else {
      if (ext_driver) {
        snprintf(p->driver_name, sizeof(p->driver_name), "%s", drv_props.driverName);
        snprintf(p->driver_info, sizeof(p->driver_info), "%s", drv_props.driverInfo);
        if (drv_props.conformanceVersion.major || drv_props.conformanceVersion.minor ||
            drv_props.conformanceVersion.subminor || drv_props.conformanceVersion.patch) {
          snprintf(p->conformance, sizeof(p->conformance), "%u.%u.%u.%u",
                   drv_props.conformanceVersion.major, drv_props.conformanceVersion.minor,
                   drv_props.conformanceVersion.subminor, drv_props.conformanceVersion.patch);
        }
      }
#ifdef VK_EXT_descriptor_indexing
      if (ext_desc_idx) p->max_ssbo_update_after_bind = di_props.maxPerStageDescriptorUpdateAfterBindStorageBuffers;
#endif
#ifdef VK_KHR_shader_float_controls
      if (ext_float_ctrl) p->has_fp32_denorm = (fc_props.shaderDenormPreserveFloat32 == VK_TRUE);
#endif
    }
    if (base_props.apiVersion >= VK_API_VERSION_1_3) {
      p->has_dot4x8 = (vk13_props.integerDotProduct4x8BitPackedSignedAccelerated == VK_TRUE ||
                       vk13_props.integerDotProduct4x8BitPackedUnsignedAccelerated == VK_TRUE);
    } else {
#ifdef VK_KHR_shader_integer_dot_product
      if (ext_dot_prod) {
        p->has_dot4x8 = (dp_props.integerDotProduct4x8BitPackedSignedAccelerated == VK_TRUE ||
                         dp_props.integerDotProduct4x8BitPackedUnsignedAccelerated == VK_TRUE);
      }
#endif
    }
    if (ext_host_ptr) p->host_ptr_align = hp_props.minImportedHostPointerAlignment;
    if (ext_pci) {
      p->has_pci_bus_info = true;
      p->pci_domain = pci_props.pciDomain;
      p->pci_bus = pci_props.pciBus;
      p->pci_device = pci_props.pciDevice;
      p->pci_function = pci_props.pciFunction;
    }
    if (ext_drm) {
      p->has_drm_props = true;
      if (drm_props.hasPrimary) { p->drm_primary_major = drm_props.primaryMajor; p->drm_primary_minor = drm_props.primaryMinor; }
      if (drm_props.hasRender) { p->drm_render_major = drm_props.renderMajor; p->drm_render_minor = drm_props.renderMinor; }
    }

    /* Features2 chain */
    VkPhysicalDevice16BitStorageFeatures f_16bit = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_16BIT_STORAGE_FEATURES };
    VkPhysicalDeviceVulkan11Features f11 = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES };
    VkPhysicalDeviceVulkan12Features f12 = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES };
    VkPhysicalDeviceVulkan13Features f13 = { .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES };
#ifdef VK_KHR_shader_float16_int8
    VkPhysicalDeviceShaderFloat16Int8FeaturesKHR f_fp16i8 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_FLOAT16_INT8_FEATURES_KHR,
    };
#endif
#ifdef VK_KHR_8bit_storage
    VkPhysicalDevice8BitStorageFeaturesKHR f_8bit = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_8BIT_STORAGE_FEATURES_KHR,
    };
#endif
#ifdef VK_KHR_buffer_device_address
    VkPhysicalDeviceBufferDeviceAddressFeaturesKHR f_bda = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_BUFFER_DEVICE_ADDRESS_FEATURES_KHR,
    };
#endif
#ifdef VK_EXT_descriptor_indexing
    VkPhysicalDeviceDescriptorIndexingFeaturesEXT f_di = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DESCRIPTOR_INDEXING_FEATURES_EXT,
    };
#endif
#ifdef VK_KHR_shader_atomic_int64
    VkPhysicalDeviceShaderAtomicInt64FeaturesKHR f_i64 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_ATOMIC_INT64_FEATURES_KHR,
    };
#endif
#ifdef VK_KHR_vulkan_memory_model
    VkPhysicalDeviceVulkanMemoryModelFeaturesKHR f_vmm = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_MEMORY_MODEL_FEATURES_KHR,
    };
#endif
#ifdef VK_EXT_texture_compression_astc_hdr
    VkPhysicalDeviceTextureCompressionASTCHDRFeaturesEXT f_astc_hdr = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_TEXTURE_COMPRESSION_ASTC_HDR_FEATURES_EXT,
    };
#endif
#ifdef VK_KHR_synchronization2
    VkPhysicalDeviceSynchronization2FeaturesKHR f_sync2 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SYNCHRONIZATION_2_FEATURES_KHR,
    };
#endif
#ifdef VK_EXT_shader_atomic_float
    VkPhysicalDeviceShaderAtomicFloatFeaturesEXT f_af = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_ATOMIC_FLOAT_FEATURES_EXT,
    };
#endif
#ifdef VK_EXT_shader_image_atomic_int64
    VkPhysicalDeviceShaderImageAtomicInt64FeaturesEXT f_i64img = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_IMAGE_ATOMIC_INT64_FEATURES_EXT,
    };
#endif
#ifdef VK_KHR_cooperative_matrix
    VkPhysicalDeviceCooperativeMatrixFeaturesKHR f_cm_khr = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_COOPERATIVE_MATRIX_FEATURES_KHR,
    };
#endif
#ifdef VK_NV_cooperative_matrix
    VkPhysicalDeviceCooperativeMatrixFeaturesNV f_cm_nv = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_COOPERATIVE_MATRIX_FEATURES_NV,
    };
#endif
#ifdef VK_KHR_shader_bfloat16
    VkPhysicalDeviceShaderBfloat16FeaturesKHR f_bf16 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_SHADER_BFLOAT16_FEATURES_KHR,
    };
#endif
#ifdef VK_EXT_mesh_shader
    VkPhysicalDeviceMeshShaderFeaturesEXT f_mesh = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MESH_SHADER_FEATURES_EXT,
    };
#endif
#ifdef VK_KHR_ray_query
    VkPhysicalDeviceRayQueryFeaturesKHR f_rq = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_RAY_QUERY_FEATURES_KHR,
    };
#endif

    void *fchain = NULL;
    if (base_props.apiVersion >= VK_API_VERSION_1_2) {
      f11.pNext = fchain; fchain = &f11;
      f12.pNext = fchain; fchain = &f12;
    } else {
      f_16bit.pNext = fchain; fchain = &f_16bit;
#ifdef VK_KHR_shader_float16_int8
      if (ext_fp16_int8) { f_fp16i8.pNext = fchain; fchain = &f_fp16i8; }
#endif
#ifdef VK_KHR_8bit_storage
      if (ext_8bit_storage) { f_8bit.pNext = fchain; fchain = &f_8bit; }
#endif
#ifdef VK_KHR_buffer_device_address
      if (ext_bda) { f_bda.pNext = fchain; fchain = &f_bda; }
#endif
#ifdef VK_EXT_descriptor_indexing
      if (ext_desc_idx) { f_di.pNext = fchain; fchain = &f_di; }
#endif
#ifdef VK_KHR_shader_atomic_int64
      if (ext_atomic_int64) { f_i64.pNext = fchain; fchain = &f_i64; }
#endif
#ifdef VK_KHR_vulkan_memory_model
      if (ext_vk_mem_model) { f_vmm.pNext = fchain; fchain = &f_vmm; }
#endif
    }
    if (base_props.apiVersion >= VK_API_VERSION_1_3) {
      f13.pNext = fchain; fchain = &f13;
    } else {
#ifdef VK_EXT_texture_compression_astc_hdr
      if (ext_astc_hdr) { f_astc_hdr.pNext = fchain; fchain = &f_astc_hdr; }
#endif
#ifdef VK_KHR_synchronization2
      if (ext_sync2) { f_sync2.pNext = fchain; fchain = &f_sync2; }
#endif
    }
#ifdef VK_EXT_shader_atomic_float
    if (ext_atomic_float) { f_af.pNext = fchain; fchain = &f_af; }
#endif
#ifdef VK_EXT_shader_image_atomic_int64
    if (ext_img_int64) { f_i64img.pNext = fchain; fchain = &f_i64img; }
#endif
#ifdef VK_KHR_cooperative_matrix
    if (ext_coop_khr) { f_cm_khr.pNext = fchain; fchain = &f_cm_khr; }
#endif
#ifdef VK_NV_cooperative_matrix
    if (ext_coop_nv && !ext_coop_khr) { f_cm_nv.pNext = fchain; fchain = &f_cm_nv; }
#endif
#ifdef VK_KHR_shader_bfloat16
    if (ext_bf16_khr) { f_bf16.pNext = fchain; fchain = &f_bf16; }
#endif
#ifdef VK_EXT_mesh_shader
    if (ext_mesh) { f_mesh.pNext = fchain; fchain = &f_mesh; }
#endif
#ifdef VK_KHR_ray_query
    if (ext_ray) { f_rq.pNext = fchain; fchain = &f_rq; }
#endif

    VkPhysicalDeviceFeatures2 feat2 = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2,
        .pNext = fchain,
    };
    vkGetPhysicalDeviceFeatures2(pd, &feat2);

    p->has_int64 = (feat2.features.shaderInt64 == VK_TRUE);
    p->has_etc2 = (feat2.features.textureCompressionETC2 == VK_TRUE);
    p->has_astc_ldr = (feat2.features.textureCompressionASTC_LDR == VK_TRUE);
    p->has_bc = (feat2.features.textureCompressionBC == VK_TRUE);
    p->has_tessellation = (feat2.features.tessellationShader == VK_TRUE);
    p->has_depth_bounds = (feat2.features.depthBounds == VK_TRUE);
    if (base_props.apiVersion >= VK_API_VERSION_1_2) {
      p->has_storage_16bit = (f11.storageBuffer16BitAccess == VK_TRUE);
      p->has_fp16 = (f12.shaderFloat16 == VK_TRUE && p->has_storage_16bit);
      p->has_int8 = (f12.shaderInt8 == VK_TRUE);
      p->has_storage_8bit = (f12.storageBuffer8BitAccess == VK_TRUE);
      p->has_bda = (f12.bufferDeviceAddress == VK_TRUE);
      p->has_update_after_bind = (f12.descriptorBindingStorageBufferUpdateAfterBind == VK_TRUE);
      p->has_int64_buf_atomics = (f12.shaderBufferInt64Atomics == VK_TRUE);
      p->has_int64_shared_atomics = (f12.shaderSharedInt64Atomics == VK_TRUE);
      p->has_vk_mem_model = (f12.vulkanMemoryModel == VK_TRUE && f12.vulkanMemoryModelDeviceScope == VK_TRUE);
      p->has_sampler_minmax = (f12.samplerFilterMinmax == VK_TRUE);
    } else {
      p->has_storage_16bit = (f_16bit.storageBuffer16BitAccess == VK_TRUE);
#ifdef VK_KHR_shader_float16_int8
      if (ext_fp16_int8) {
        p->has_fp16 = (f_fp16i8.shaderFloat16 == VK_TRUE && p->has_storage_16bit);
        p->has_int8 = (f_fp16i8.shaderInt8 == VK_TRUE);
      }
#endif
#ifdef VK_KHR_8bit_storage
      if (ext_8bit_storage) p->has_storage_8bit = (f_8bit.storageBuffer8BitAccess == VK_TRUE);
#endif
#ifdef VK_KHR_buffer_device_address
      if (ext_bda) p->has_bda = (f_bda.bufferDeviceAddress == VK_TRUE);
#endif
#ifdef VK_EXT_descriptor_indexing
      if (ext_desc_idx) p->has_update_after_bind = (f_di.descriptorBindingStorageBufferUpdateAfterBind == VK_TRUE);
#endif
#ifdef VK_KHR_shader_atomic_int64
      if (ext_atomic_int64) {
        p->has_int64_buf_atomics = (f_i64.shaderBufferInt64Atomics == VK_TRUE);
        p->has_int64_shared_atomics = (f_i64.shaderSharedInt64Atomics == VK_TRUE);
      }
#endif
#ifdef VK_KHR_vulkan_memory_model
      if (ext_vk_mem_model) {
        p->has_vk_mem_model = (f_vmm.vulkanMemoryModel == VK_TRUE && f_vmm.vulkanMemoryModelDeviceScope == VK_TRUE);
      }
#endif
      p->has_sampler_minmax = ext_sampler_minmax;
    }
    if (base_props.apiVersion >= VK_API_VERSION_1_3) {
      p->has_astc_hdr = (f13.textureCompressionASTC_HDR == VK_TRUE);
      p->has_sync2 = (f13.synchronization2 == VK_TRUE);
    } else {
#ifdef VK_EXT_texture_compression_astc_hdr
      if (ext_astc_hdr) p->has_astc_hdr = (f_astc_hdr.textureCompressionASTC_HDR == VK_TRUE);
#endif
#ifdef VK_KHR_synchronization2
      if (ext_sync2) p->has_sync2 = (f_sync2.synchronization2 == VK_TRUE);
#else
      p->has_sync2 = ext_sync2;
#endif
    }
#ifdef VK_EXT_shader_atomic_float
    if (ext_atomic_float) {
      p->has_float_atomics = (f_af.shaderBufferFloat32Atomics == VK_TRUE);
      p->has_float_atomic_add = (f_af.shaderBufferFloat32AtomicAdd == VK_TRUE);
    }
#else
    (void)ext_atomic_float;
#endif
#ifdef VK_EXT_shader_image_atomic_int64
    if (ext_img_int64) p->has_int64_img_atomics = (f_i64img.shaderImageInt64Atomics == VK_TRUE);
#else
    (void)ext_img_int64;
#endif
#ifdef VK_KHR_cooperative_matrix
    if (ext_coop_khr) p->has_coop_matrix = (f_cm_khr.cooperativeMatrix == VK_TRUE);
#else
    if (ext_coop_khr) p->has_coop_matrix = true;
#endif
#ifdef VK_NV_cooperative_matrix
    if (!p->has_coop_matrix && ext_coop_nv && !ext_coop_khr) p->has_coop_matrix = (f_cm_nv.cooperativeMatrix == VK_TRUE);
#else
    if (!p->has_coop_matrix && ext_coop_nv) p->has_coop_matrix = true;
#endif
#ifdef VK_KHR_shader_bfloat16
    if (ext_bf16_khr) p->has_bfloat16 = (f_bf16.shaderBFloat16Type == VK_TRUE);
#else
    p->has_bfloat16 = ext_bf16_khr;
#endif
#ifdef VK_EXT_mesh_shader
    if (ext_mesh) p->has_mesh_shader = (f_mesh.meshShader == VK_TRUE);
#else
    p->has_mesh_shader = ext_mesh;
#endif
#ifdef VK_KHR_ray_query
    if (ext_ray) p->has_ray_query = (f_rq.rayQuery == VK_TRUE);
#else
    p->has_ray_query = ext_ray;
#endif
  }

  (void)ext_fp16_int8; (void)ext_8bit_storage; (void)ext_bda; (void)ext_desc_idx;
  (void)ext_atomic_int64; (void)ext_vk_mem_model; (void)ext_sampler_minmax;
  (void)ext_float_ctrl; (void)ext_dot_prod; (void)ext_astc_hdr;

  p->fw_dma_coherent = detect_fw_dma_coherent(p);

  /* Create temporary logical device to test zero-copy tier & live hardware DMA coherency */
  const char *en_exts[4];
  uint32_t en_cnt = 0;
  if (p->has_dmabuf_ext) {
    en_exts[en_cnt++] = VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME;
    en_exts[en_cnt++] = VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME;
  }
  if (p->has_host_ptr_ext) {
    en_exts[en_cnt++] = VK_EXT_EXTERNAL_MEMORY_HOST_EXTENSION_NAME;
  }
  float prio = 1.0f;
  VkDeviceQueueCreateInfo qci = {
      .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
      .queueFamilyIndex = best_qf,
      .queueCount = 1,
      .pQueuePriorities = &prio,
  };
  VkDeviceCreateInfo dci = {
      .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
      .queueCreateInfoCount = 1,
      .pQueueCreateInfos = &qci,
      .enabledExtensionCount = en_cnt,
      .ppEnabledExtensionNames = en_exts,
  };
  VkDevice dev = VK_NULL_HANDLE;
  if (vkCreateDevice(pd, &dci, NULL, &dev) != VK_SUCCESS) {
    vkDestroyInstance(inst, NULL);
    dlclose(vklib);
    return;
  }

  #define LOAD_DEV(fn) PFN_##fn fn = (PFN_##fn)vkGetDeviceProcAddr(dev, #fn)
  LOAD_DEV(vkDestroyDevice);
  LOAD_DEV(vkGetDeviceQueue);
  LOAD_DEV(vkCreateBuffer);
  LOAD_DEV(vkDestroyBuffer);
  LOAD_DEV(vkGetBufferMemoryRequirements);
  LOAD_DEV(vkAllocateMemory);
  LOAD_DEV(vkFreeMemory);
  LOAD_DEV(vkBindBufferMemory);
  LOAD_DEV(vkCreateCommandPool);
  LOAD_DEV(vkDestroyCommandPool);
  LOAD_DEV(vkAllocateCommandBuffers);
  LOAD_DEV(vkResetCommandBuffer);
  LOAD_DEV(vkBeginCommandBuffer);
  LOAD_DEV(vkEndCommandBuffer);
  LOAD_DEV(vkCmdCopyBuffer);
  LOAD_DEV(vkCreateFence);
  LOAD_DEV(vkDestroyFence);
  LOAD_DEV(vkResetFences);
  LOAD_DEV(vkWaitForFences);
  LOAD_DEV(vkQueueSubmit);
  LOAD_DEV(vkDeviceWaitIdle);
  PFN_vkGetMemoryFdPropertiesKHR pfn_fd_props =
      p->has_dmabuf_ext ? (PFN_vkGetMemoryFdPropertiesKHR)vkGetDeviceProcAddr(dev, "vkGetMemoryFdPropertiesKHR") : NULL;
  PFN_vkGetMemoryHostPointerPropertiesEXT pfn_hp_props =
      p->has_host_ptr_ext ? (PFN_vkGetMemoryHostPointerPropertiesEXT)vkGetDeviceProcAddr(dev, "vkGetMemoryHostPointerPropertiesEXT") : NULL;

  bool have_buf_ops = (vkCreateBuffer && vkDestroyBuffer && vkGetBufferMemoryRequirements &&
                       vkAllocateMemory && vkFreeMemory && vkBindBufferMemory);

  const VkBufferUsageFlags buf_usage =
      VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT |
      VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT;

  /* Probe extra external buffer padding (e.g. V3D 256B guard) */
  uint64_t pg = p->host_page_size < 16384u ? 16384u : p->host_page_size;
  if (have_buf_ops) {
    VkExternalMemoryHandleTypeFlags types[2];
    size_t nt = 0;
    if (p->has_dmabuf_ext) types[nt++] = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT;
    if (p->has_host_ptr_ext) types[nt++] = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT;
    for (size_t t = 0; t < nt; t++) {
      VkExternalMemoryBufferCreateInfo eb = {
          .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
          .handleTypes = types[t],
      };
      VkBufferCreateInfo bci = {
          .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
          .pNext = &eb,
          .size = pg,
          .usage = buf_usage,
          .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
      };
      VkBuffer tb = VK_NULL_HANDLE;
      if (vkCreateBuffer(dev, &bci, NULL, &tb) == VK_SUCCESS) {
        VkMemoryRequirements mr;
        vkGetBufferMemoryRequirements(dev, tb, &mr);
        if (mr.size > pg && mr.size - pg > p->buf_extra_pad) p->buf_extra_pad = mr.size - pg;
        vkDestroyBuffer(dev, tb, NULL);
      }
    }
  }

  /* Test zero-copy import on a 256 KiB memfd */
  uint64_t shm_bytes = 256u * 1024u;
  int shm_fd = memfd_create("kido-probe-shm", MFD_CLOEXEC | MFD_ALLOW_SEALING);
  uint8_t *shm_base = NULL;
  if (shm_fd >= 0 && ftruncate(shm_fd, (off_t)shm_bytes) == 0) {
    fcntl(shm_fd, F_ADD_SEALS, F_SEAL_SHRINK);
    shm_base = mmap_aligned_shm(shm_fd, shm_bytes, 65536u);
  }

  VkBuffer zbuf = VK_NULL_HANDLE;
  VkDeviceMemory zmem = VK_NULL_HANDLE;
  int dfd = -1;
  bool zc_ok = false;

  if (have_buf_ops && shm_base && p->udmabuf_accessible && p->has_dmabuf_ext && pfn_fd_props &&
      p->buf_extra_pad <= shm_bytes - 65536u) {
    uint64_t pshm = align_up(65536u + p->buf_extra_pad, pg);
    if (pshm > 0 && pshm <= shm_bytes) {
      int ud = open("/dev/udmabuf", O_RDWR | O_CLOEXEC);
      if (ud >= 0) {
        struct udmabuf_create uc = { .memfd = (uint32_t)shm_fd, .flags = UDMABUF_FLAGS_CLOEXEC, .offset = 0, .size = pshm };
        dfd = ioctl(ud, UDMABUF_CREATE, &uc);
        close(ud);
      }
    }
    if (dfd >= 0) {
      VkExternalMemoryBufferCreateInfo eb = {
          .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
          .handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
      };
      VkBufferCreateInfo bci = {
          .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
          .pNext = &eb,
          .size = 65536u,
          .usage = buf_usage,
          .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
      };
      if (vkCreateBuffer(dev, &bci, NULL, &zbuf) == VK_SUCCESS) {
        VkMemoryRequirements mr;
        vkGetBufferMemoryRequirements(dev, zbuf, &mr);
        VkMemoryFdPropertiesKHR fp = { .sType = VK_STRUCTURE_TYPE_MEMORY_FD_PROPERTIES_KHR };
        if (mr.size <= pshm &&
            pfn_fd_props(dev, VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT, dfd, &fp) == VK_SUCCESS) {
          int mt = find_mem_type(&mem_props, mr.memoryTypeBits & fp.memoryTypeBits, VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT);
          if (mt < 0) mt = find_mem_type(&mem_props, mr.memoryTypeBits & fp.memoryTypeBits, 0);
          int dupfd = fcntl(dfd, F_DUPFD_CLOEXEC, 0);
          if (mt >= 0 && dupfd >= 0) {
            VkMemoryDedicatedAllocateInfo ded = {
                .sType = VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,
                .buffer = zbuf,
            };
            VkImportMemoryFdInfoKHR imp = {
                .sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR,
                .pNext = &ded,
                .handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
                .fd = dupfd,
            };
            VkMemoryAllocateInfo mai = {
                .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
                .pNext = &imp,
                .allocationSize = pshm,
                .memoryTypeIndex = (uint32_t)mt,
            };
            VkResult ar = vkAllocateMemory(dev, &mai, NULL, &zmem);
            if (ar == VK_SUCCESS) {
              /* Vulkan took ownership of dupfd on VK_SUCCESS. */
              if (vkBindBufferMemory(dev, zbuf, zmem, 0) == VK_SUCCESS) {
                zc_ok = true;
                snprintf(p->zero_copy_tier, sizeof(p->zero_copy_tier), "dmabuf");
              } else {
                vkFreeMemory(dev, zmem, NULL);
                zmem = VK_NULL_HANDLE;
              }
            } else {
              close(dupfd);
            }
          } else if (dupfd >= 0) {
            close(dupfd);
          }
        }
        if (!zc_ok) { vkDestroyBuffer(dev, zbuf, NULL); zbuf = VK_NULL_HANDLE; }
      }
      if (!zc_ok) { close(dfd); dfd = -1; }
    }
  }

  if (!zc_ok && have_buf_ops && shm_base && p->has_host_ptr_ext && pfn_hp_props &&
      p->buf_extra_pad <= shm_bytes - 65536u) {
    uint64_t ha = p->host_ptr_align > pg ? p->host_ptr_align : pg;
    if ((ha & (ha - 1u)) == 0 && ha <= 65536u && (((uintptr_t)shm_base) % ha) == 0) {
      uint64_t pshm = align_up(65536u + p->buf_extra_pad, ha);
      if (pshm > 0 && pshm <= shm_bytes) {
        VkExternalMemoryBufferCreateInfo eb = {
            .sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
            .handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT,
        };
        VkBufferCreateInfo bci = {
            .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
            .pNext = &eb,
            .size = 65536u,
            .usage = buf_usage,
            .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
        };
        if (vkCreateBuffer(dev, &bci, NULL, &zbuf) == VK_SUCCESS) {
          VkMemoryRequirements mr;
          vkGetBufferMemoryRequirements(dev, zbuf, &mr);
          VkMemoryHostPointerPropertiesEXT hp = { .sType = VK_STRUCTURE_TYPE_MEMORY_HOST_POINTER_PROPERTIES_EXT };
          if (mr.size <= pshm &&
              pfn_hp_props(dev, VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT, shm_base, &hp) == VK_SUCCESS) {
            int mt = find_mem_type(&mem_props, mr.memoryTypeBits & hp.memoryTypeBits,
                                   VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
            if (mt < 0) mt = find_mem_type(&mem_props, mr.memoryTypeBits & hp.memoryTypeBits, VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
            if (mt >= 0) {
              VkImportMemoryHostPointerInfoEXT imp = {
                  .sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT,
                  .handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT,
                  .pHostPointer = shm_base,
              };
              VkMemoryAllocateInfo mai = {
                  .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
                  .pNext = &imp,
                  .allocationSize = pshm,
                  .memoryTypeIndex = (uint32_t)mt,
              };
              if (vkAllocateMemory(dev, &mai, NULL, &zmem) == VK_SUCCESS) {
                if (vkBindBufferMemory(dev, zbuf, zmem, 0) == VK_SUCCESS) {
                  zc_ok = true;
                  snprintf(p->zero_copy_tier, sizeof(p->zero_copy_tier), "host_ptr");
                } else {
                  vkFreeMemory(dev, zmem, NULL);
                  zmem = VK_NULL_HANDLE;
                }
              }
            }
          }
          if (!zc_ok) { vkDestroyBuffer(dev, zbuf, NULL); zbuf = VK_NULL_HANDLE; }
        }
      }
    }
  }

  /* Live 4-round L1-hot GPU DMA coherency test when firmware indicates coherent or forced */
  if (zc_ok && run_coh_test && (p->fw_dma_coherent || force_coh_test) &&
      vkGetDeviceQueue && vkCreateCommandPool && vkDestroyCommandPool &&
      vkAllocateCommandBuffers && vkResetCommandBuffer && vkBeginCommandBuffer &&
      vkEndCommandBuffer && vkCmdCopyBuffer && vkCreateFence && vkDestroyFence &&
      vkResetFences && vkWaitForFences && vkQueueSubmit) {
    VkQueue queue = VK_NULL_HANDLE;
    vkGetDeviceQueue(dev, best_qf, 0, &queue);
    VkCommandPoolCreateInfo cpci = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
        .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
        .queueFamilyIndex = best_qf,
    };
    VkCommandPool cpool = VK_NULL_HANDLE;
    VkCommandBuffer cb = VK_NULL_HANDLE;
    VkFence fence = VK_NULL_HANDLE;
    VkFenceCreateInfo fci = { .sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO };
    if (vkCreateCommandPool(dev, &cpci, NULL, &cpool) == VK_SUCCESS &&
        vkCreateFence(dev, &fci, NULL, &fence) == VK_SUCCESS) {
      VkCommandBufferAllocateInfo cbai = {
          .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
          .commandPool = cpool,
          .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
          .commandBufferCount = 1,
      };
      if (vkAllocateCommandBuffers(dev, &cbai, &cb) == VK_SUCCESS) {
        p->coherency_tested = true;
        bool all_ok = true;
        bool wedged = false;
        volatile uint32_t *src = (volatile uint32_t *)shm_base;
        volatile uint32_t *dst = (volatile uint32_t *)(shm_base + 32768u);
        const size_t nwords = 32768u / sizeof(uint32_t);
        for (uint32_t r = 0; r < 4 && all_ok; r++) {
          uint32_t sentinel = 0xDEADBEE0u ^ (r * 0x11111111u);
          uint32_t seed = 0xA5A55A50u ^ (r * 0x01010101u);
          for (size_t i = 0; i < nwords; i++) {
            dst[i] = sentinel;
            src[i] = seed ^ ((uint32_t)i * 0x9E3779B1u);
          }
#if defined(__aarch64__)
          __asm__ volatile("dmb osh" ::: "memory");
#else
          __atomic_thread_fence(__ATOMIC_SEQ_CST);
#endif
          vkResetCommandBuffer(cb, 0);
          VkCommandBufferBeginInfo cbbi = {
              .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
              .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
          };
          if (vkBeginCommandBuffer(cb, &cbbi) != VK_SUCCESS) { all_ok = false; break; }
          VkBufferCopy reg = { .srcOffset = 0, .dstOffset = 32768u, .size = 32768u };
          vkCmdCopyBuffer(cb, zbuf, zbuf, 1, &reg);
          if (vkEndCommandBuffer(cb) != VK_SUCCESS) { all_ok = false; break; }
          vkResetFences(dev, 1, &fence);
          VkSubmitInfo si = {
              .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
              .commandBufferCount = 1,
              .pCommandBuffers = &cb,
          };
          if (vkQueueSubmit(queue, 1, &si, fence) != VK_SUCCESS) { all_ok = false; break; }
          if (vkWaitForFences(dev, 1, &fence, VK_TRUE, 5000000000ULL) != VK_SUCCESS) {
            if (!vkDeviceWaitIdle || vkDeviceWaitIdle(dev) != VK_SUCCESS) wedged = true;
            all_ok = false;
            break;
          }
#if defined(__aarch64__)
          __asm__ volatile("dmb osh" ::: "memory");
#else
          __atomic_thread_fence(__ATOMIC_SEQ_CST);
#endif
          for (size_t i = 0; i < nwords; i++) {
            if (dst[i] != (seed ^ ((uint32_t)i * 0x9E3779B1u))) {
              all_ok = false;
              break;
            }
          }
        }
        if (wedged) {
          /* Do not unmap shm_base or destroy in-use Vulkan resources while DMA may be in-flight. */
          if (dfd >= 0) close(dfd);
          if (shm_fd >= 0) close(shm_fd);
          return;
        }
        p->coherency_passed = all_ok;
        /* Match kido-vk-worker.c: auto-enable hardware coherency only when both firmware and live probe agree. */
        p->dma_coherent = (p->fw_dma_coherent && p->coherency_passed);
      }
    }
    if (fence) vkDestroyFence(dev, fence, NULL);
    if (cpool) vkDestroyCommandPool(dev, cpool, NULL);
  }

  if (zbuf && vkDestroyBuffer) vkDestroyBuffer(dev, zbuf, NULL);
  if (zmem && vkFreeMemory) vkFreeMemory(dev, zmem, NULL);
  if (dfd >= 0) close(dfd);
  if (shm_base) munmap(shm_base, (size_t)shm_bytes);
  if (shm_fd >= 0) close(shm_fd);
  if (vkDestroyDevice) vkDestroyDevice(dev, NULL);
  vkDestroyInstance(inst, NULL);
  dlclose(vklib);
}

static size_t valid_utf8_len(const unsigned char *s) {
  unsigned char b0 = s[0];
  if (b0 < 0x80) return 1;
  if ((b0 & 0xE0) == 0xC0) {
    unsigned char b1 = s[1];
    if ((b1 & 0xC0) != 0x80) return 0;
    uint32_t cp = ((uint32_t)(b0 & 0x1F) << 6) | (uint32_t)(b1 & 0x3F);
    return (cp >= 0x80) ? 2 : 0;
  }
  if ((b0 & 0xF0) == 0xE0) {
    unsigned char b1 = s[1];
    if ((b1 & 0xC0) != 0x80) return 0;
    unsigned char b2 = s[2];
    if ((b2 & 0xC0) != 0x80) return 0;
    uint32_t cp = ((uint32_t)(b0 & 0x0F) << 12) | ((uint32_t)(b1 & 0x3F) << 6) | (uint32_t)(b2 & 0x3F);
    return (cp >= 0x800 && !(cp >= 0xD800 && cp <= 0xDFFF)) ? 3 : 0;
  }
  if ((b0 & 0xF8) == 0xF0) {
    unsigned char b1 = s[1];
    if ((b1 & 0xC0) != 0x80) return 0;
    unsigned char b2 = s[2];
    if ((b2 & 0xC0) != 0x80) return 0;
    unsigned char b3 = s[3];
    if ((b3 & 0xC0) != 0x80) return 0;
    uint32_t cp = ((uint32_t)(b0 & 0x07) << 18) | ((uint32_t)(b1 & 0x3F) << 12) |
                  ((uint32_t)(b2 & 0x3F) << 6) | (uint32_t)(b3 & 0x3F);
    return (cp >= 0x10000 && cp <= 0x10FFFF) ? 4 : 0;
  }
  return 0;
}

static void json_str(const char *s) {
  putchar('"');
  const unsigned char *p = (const unsigned char *)(s ? s : "");
  while (*p) {
    unsigned char c = *p;
    if (c < 0x80) {
      switch (c) {
      case '"': fputs("\\\"", stdout); break;
      case '\\': fputs("\\\\", stdout); break;
      case '\b': fputs("\\b", stdout); break;
      case '\f': fputs("\\f", stdout); break;
      case '\n': fputs("\\n", stdout); break;
      case '\r': fputs("\\r", stdout); break;
      case '\t': fputs("\\t", stdout); break;
      default:
        if (c < 0x20) printf("\\u%04x", c);
        else putchar((int)c);
        break;
      }
      p++;
    } else {
      size_t n = valid_utf8_len(p);
      if (n > 0) {
        fwrite(p, 1, n, stdout);
        p += n;
      } else {
        fputs("\\ufffd", stdout);
        p++;
      }
    }
  }
  putchar('"');
}

static void json_cap(const char *key, const char *status, const char *val, const char *detail, bool comma) {
  printf("    \"%s\": {\n      \"status\": ", key);
  json_str(status);
  printf(",\n      \"value\": ");
  json_str(val);
  printf(",\n      \"detail\": ");
  json_str(detail);
  printf("\n    }%s\n", comma ? "," : "");
}

static void json_limit_cap(const char *key,
                           const char *status,
                           const char *pass_st,
                           const char *fail_st,
                           uint64_t numeric,
                           const char *val,
                           const char *detail) {
  printf("    \"%s\": {\n      \"status\": ", key);
  json_str(status);
  printf(",\n      \"pass_status\": ");
  json_str(pass_st);
  printf(",\n      \"fail_status\": ");
  json_str(fail_st);
  printf(",\n      \"numeric\": %llu,\n      \"value\": ", (unsigned long long)numeric);
  json_str(val);
  printf(",\n      \"detail\": ");
  json_str(detail);
  printf("\n    },\n");
}

static void print_json(const probe_info_t *p) {
  char slug[128], drv_short[320], api_str[48], buf_val[64], buf_det[160];
  char tg_val[32], tg_det[128], shmem_val[32], shmem_det[128], tex_val[32], tex_det[128];
  char vp_val[32], vp_det[128], msaa_val[32], msaa_det[128], ts_det[128];

  char slug_src[528];
  snprintf(slug_src, sizeof(slug_src), "%s-%s", p->board_model, p->gpu_found ? p->gpu_name : "cpu");
  make_slug(slug_src, slug, sizeof(slug));

  uint32_t v_maj = VK_API_VERSION_MAJOR(p->vk_api_ver);
  uint32_t v_min = VK_API_VERSION_MINOR(p->vk_api_ver);
  uint32_t v_pat = VK_API_VERSION_PATCH(p->vk_api_ver);
  snprintf(api_str, sizeof(api_str), "Vulkan %u.%u.%u", v_maj, v_min, v_pat);
  if (p->driver_name[0]) {
    snprintf(drv_short, sizeof(drv_short), "%s, Vulkan %u.%u%s",
             p->driver_name, v_maj, v_min, p->gpu_is_cpu ? " (CPU)" : "");
  } else if (p->gpu_found) {
    snprintf(drv_short, sizeof(drv_short), "Vulkan %u.%u%s", v_maj, v_min, p->gpu_is_cpu ? " (CPU)" : "");
  } else {
    snprintf(drv_short, sizeof(drv_short), "No Vulkan GPU detected");
  }

  const char *hw_ok = p->gpu_is_cpu ? "software" : "hw_ready";
  bool has_quad = (p->subgroup_ops & VK_SUBGROUP_FEATURE_QUAD_BIT) != 0;
  bool has_ballot = (p->subgroup_ops & VK_SUBGROUP_FEATURE_BALLOT_BIT) != 0;
  bool has_shuffle = (p->subgroup_ops & VK_SUBGROUP_FEATURE_SHUFFLE_BIT) != 0;
  bool has_arith = (p->subgroup_ops & VK_SUBGROUP_FEATURE_ARITHMETIC_BIT) != 0;
  bool has_vote = (p->subgroup_ops & VK_SUBGROUP_FEATURE_VOTE_BIT) != 0;
  bool has_rotate = (p->subgroup_ops & VK_SUBGROUP_FEATURE_ROTATE_BIT) != 0;
  bool full_simd = has_quad && has_ballot && has_shuffle && has_arith && has_vote;
  bool full_metal3_limits = p->gpu_found && !p->gpu_is_cpu &&
                            p->max_tg_invocations >= 1024 && p->max_tg_mem_bytes >= 32768 &&
                            p->max_ssbo_per_stage >= 31 && p->has_int64 &&
                            p->has_fp16 && p->has_int8 && p->has_storage_8bit && full_simd;

  const char *comp_fam = !p->gpu_found ? "No GPU"
                         : p->gpu_is_cpu ? "CPU software renderer"
                         : (full_metal3_limits && p->has_coop_matrix && p->has_bfloat16) ? "Apple9/Apple10 (Compute)"
                         : (full_metal3_limits && p->has_coop_matrix) ? "Apple7/Apple8 (Compute)"
                         : full_metal3_limits ? "Apple6/Apple7 (Compute)"
                         : full_simd ? "Apple2/Apple4 limits, full SIMD"
                         : "Apple2 limits, partial SIMD";

  printf("{\n");
  printf("  \"id\": "); json_str(slug); printf(",\n");
  printf("  \"order\": 100,\n");
  printf("  \"name\": "); json_str(p->board_model); printf(",\n");
  printf("  \"subtitle\": "); json_str(p->gpu_found ? p->gpu_name : "No Vulkan GPU"); printf(",\n");
  printf("  \"vendor\": "); json_str(p->vendor); printf(",\n");
  printf("  \"gpu\": "); json_str(p->gpu_found ? p->gpu_name : "None"); printf(",\n");
  printf("  \"cpu\": "); json_str(p->cpu_summary); printf(",\n");
  printf("  \"arch\": "); json_str(p->arch_summary); printf(",\n");
  char drv_full[384];
  if (p->driver_info[0]) snprintf(drv_full, sizeof(drv_full), "%s (%s)", p->driver_info, api_str);
  else snprintf(drv_full, sizeof(drv_full), "%s", drv_short);
  printf("  \"driver\": "); json_str(drv_full); printf(",\n");
  char kern_full[192];
  snprintf(kern_full, sizeof(kern_full), "Linux %s%s", p->kernel_release, p->kvm_accessible ? " (KVM)" : " (no KVM)");
  printf("  \"kernel\": "); json_str(kern_full); printf(",\n");
  printf("  \"verification\": {\n    \"status\": \"Probed\"\n  },\n");
  printf("  \"trademarks\": [],\n");
  printf("  \"capabilities\": {\n");

  json_cap("verification_status", p->gpu_found ? hw_ok : "hw_limit", "Probed",
           "Hardware capabilities probed by kido-probe; live Darwin micro-VM run not yet verified", true);
  json_cap("metal_api_level", p->gpu_found ? hw_ok : "hw_limit",
           !p->gpu_found ? "None"
           : p->gpu_is_cpu ? "Metal 3 Compute (CPU SW)"
           : (full_metal3_limits ? "Metal 3 Compute" : "Metal 3 Compute (Subset)"),
           p->gpu_found ? "Probed from host Vulkan compute capabilities" : "No Vulkan compute device detected", true);
  json_cap("effective_gpu_family", p->gpu_found ? hw_ok : "hw_limit", comp_fam,
           "Estimated compute family equivalence from probed Vulkan limits and shader features", true);
  json_cap("vulkan_driver", p->gpu_found ? hw_ok : "hw_limit", drv_short,
           p->conformance[0] ? p->conformance : (p->driver_info[0] ? p->driver_info : "Host Vulkan ICD"), true);

  snprintf(tg_val, sizeof(tg_val), "%u", p->max_tg_invocations);
  snprintf(tg_det, sizeof(tg_det), "%u x %u x %u compute workgroup", p->max_tg_size[0], p->max_tg_size[1], p->max_tg_size[2]);
  json_limit_cap("max_threads_per_tg", (p->gpu_found && p->max_tg_invocations >= 1024) ? hw_ok : "hw_limit",
                 hw_ok, "hw_limit", p->max_tg_invocations, tg_val, tg_det);

  uint32_t shmem_kb = p->max_tg_mem_bytes / 1024u;
  snprintf(shmem_val, sizeof(shmem_val), "%u KB", shmem_kb);
  snprintf(shmem_det, sizeof(shmem_det), "%u bytes maxComputeSharedMemorySize", p->max_tg_mem_bytes);
  json_limit_cap("max_tg_memory", (p->gpu_found && shmem_kb >= 32) ? hw_ok : "hw_limit",
                 hw_ok, "hw_limit", shmem_kb, shmem_val, shmem_det);

  if (p->max_ssbo_per_stage >= 31) {
    snprintf(buf_val, sizeof(buf_val), "31 (HW: %u)", p->max_ssbo_per_stage);
    snprintf(buf_det, sizeof(buf_det), "Meets Apple2+ direct slot limit");
  } else {
    snprintf(buf_val, sizeof(buf_val), "%u SSBOs / stage", p->max_ssbo_per_stage);
    snprintf(buf_det, sizeof(buf_det), "Below Apple2 minimum of 31");
  }
  const char *buf_fail = (p->gpu_found && p->has_bda) ? "emulatable" : "hw_limit";
  json_limit_cap("buffer_arg_slots", (p->gpu_found && p->max_ssbo_per_stage >= 31) ? hw_ok : buf_fail,
                 hw_ok, buf_fail, p->max_ssbo_per_stage, buf_val, buf_det);

  snprintf(tex_val, sizeof(tex_val), "%u px", p->max_image_2d);
  snprintf(tex_det, sizeof(tex_det), "Hardware maxImageDimension2D");
  json_limit_cap("max_texture_2d", (p->gpu_found && p->max_image_2d >= 16384) ? hw_ok : "hw_limit",
                 hw_ok, "hw_limit", p->max_image_2d, tex_val, tex_det);

  snprintf(vp_val, sizeof(vp_val), "%u", p->max_viewports);
  snprintf(vp_det, sizeof(vp_det), "Hardware maxViewports");
  json_limit_cap("max_viewports", (p->gpu_found && p->max_viewports >= 16) ? hw_ok : "hw_limit",
                 hw_ok, "hw_limit", p->max_viewports, vp_val, vp_det);

  snprintf(msaa_val, sizeof(msaa_val), "%ux", p->max_msaa);
  snprintf(msaa_det, sizeof(msaa_det), "Hardware framebufferColorSampleCounts");
  json_limit_cap("max_msaa_samples", (p->gpu_found && p->max_msaa >= 4) ? hw_ok : "hw_limit",
                 hw_ok, "hw_limit", p->max_msaa, msaa_val, msaa_det);

  json_cap("function_specialization", p->gpu_found ? hw_ok : "hw_limit",
           p->gpu_found ? "Supported" : "Unsupported", "Vulkan specialization constants", true);
  json_cap("fp16_compute", !p->gpu_found ? "hw_limit" : (p->has_fp16 ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_fp16 ? "Native FP16" : "No HW FP16"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_fp16 ? "shaderFloat16 and storageBuffer16BitAccess" : "Emulatable via FP32 lowering"), true);
  json_cap("int8_storage", !p->gpu_found ? "hw_limit" : ((p->has_int8 && p->has_storage_8bit) ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported"
           : ((p->has_int8 && p->has_storage_8bit) ? (p->has_dot4x8 ? "Native Int8, Dot4x8" : "Native Int8") : "No HW Int8 storage"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : ((p->has_int8 && p->has_storage_8bit) ? "shaderInt8 and storageBuffer8BitAccess" : "Emulatable via 32-bit packing"), true);
  json_cap("etc2_astc", (p->gpu_found && p->has_etc2 && p->has_astc_ldr) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_etc2 && p->has_astc_ldr) ? (p->has_astc_hdr ? "ETC2, ASTC LDR/HDR" : "ETC2, ASTC LDR") : "Partial or absent",
           "Probed from Vulkan physical device texture compression features", true);
  json_cap("pvrtc_formats", "hw_limit", "Unsupported", "Apple/PowerVR proprietary format", true);
  json_cap("int64_math", !p->gpu_found ? "hw_limit" : (p->has_int64 ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_int64 ? "Native Int64" : "No HW Int64"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_int64 ? "shaderInt64 = true" : "shaderInt64 = false; emulatable as 2x32-bit pairs"), true);
  json_cap("indirect_dispatch", p->gpu_found ? hw_ok : "hw_limit",
           p->gpu_found ? "Supported" : "Unsupported", "vkCmdDispatchIndirect", true);
  json_cap("binary_archives", p->gpu_found ? "software" : "hw_limit",
           p->gpu_found ? "Sandboxed AIR to SPIR-V" : "Unsupported", "kido LLVM AIR translator", true);
  json_cap("tessellation", (p->gpu_found && p->has_tessellation) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_tessellation) ? "Supported in HW" : "Unsupported",
           (p->gpu_found && p->has_tessellation) ? "tessellationShader = true" : "tessellationShader = false", true);
  json_cap("quad_permute", (p->gpu_found && has_quad) ? hw_ok : "hw_limit",
           (p->gpu_found && has_quad) ? "QUAD supported" : "Unsupported",
           (p->gpu_found && has_quad) ? "Hardware quad subgroup operations" : "SUBGROUP_FEATURE_QUAD_BIT absent", true);
  json_cap("nonuniform_tg", p->gpu_found ? hw_ok : "hw_limit",
           p->gpu_found ? "Supported" : "Unsupported", "Bounds-clamped dispatchThreads", true);
  json_cap("apple_tbdr", "hw_limit", "Unsupported", "Apple-proprietary TBDR tile memory and raster order groups", true);

  json_cap("argument_buffers_t2", !p->gpu_found ? "hw_limit"
                                  : ((p->has_bda && p->has_update_after_bind && p->max_ssbo_update_after_bind >= 1024) ? hw_ok
                                  : (p->has_bda ? "emulatable" : "hw_limit")),
           !p->gpu_found ? "Unsupported"
           : ((p->has_bda && p->has_update_after_bind && p->max_ssbo_update_after_bind >= 1024) ? "BDA and Bindless"
           : (p->has_bda ? "BDA only" : "Unsupported")),
           (p->gpu_found && p->has_bda) ? "bufferDeviceAddress = true" : "bufferDeviceAddress = false", true);
  json_cap("simd_permute_ballot", (p->gpu_found && has_shuffle && has_ballot) ? hw_ok : "hw_limit",
           (p->gpu_found && has_shuffle && has_ballot) ? "SHUFFLE and BALLOT" : "Partial or absent",
           "Probed from VkPhysicalDeviceSubgroupProperties", true);
  json_cap("bfloat16", !p->gpu_found ? "hw_limit" : (p->has_bfloat16 ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_bfloat16 ? "Native BFloat16" : "Software lowering"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_bfloat16 ? "shaderBFloat16Type = true" : "No HW bfloat16; emulatable via FP32"), true);
  json_cap("sparse_textures", (p->gpu_found && p->sparse_va_bytes > 0) ? hw_ok : "hw_limit",
           (p->gpu_found && p->sparse_va_bytes > 0) ? "Sparse VA present" : "Unsupported",
           (p->gpu_found && p->sparse_va_bytes > 0) ? "Hardware sparseAddressSpaceSize > 0" : "sparseAddressSpaceSize = 0", true);
  json_cap("ray_tracing", (p->gpu_found && p->has_ray_query) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_ray_query) ? "Ray query in HW" : "Unsupported",
           (p->gpu_found && p->has_ray_query) ? "VK_KHR_ray_query supported" : "No hardware ray-query units", true);

  json_cap("simd_reduction", (p->gpu_found && has_arith && has_vote) ? hw_ok : "hw_limit",
           (p->gpu_found && has_arith && has_vote) ? "ARITHMETIC and VOTE" : "Partial or absent",
           "Probed from VkPhysicalDeviceSubgroupProperties", true);
  json_cap("simdgroup_matrix", !p->gpu_found ? "hw_limit" : (p->has_coop_matrix ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_coop_matrix ? "Cooperative Matrix" : "Software lowering"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_coop_matrix ? "Hardware cooperative matrix extension present" : "No HW cooperativeMatrix; emulatable via subgroup math"), true);
  json_cap("float_atomics", !p->gpu_found ? "hw_limit" : (p->has_float_atomic_add ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported"
           : (p->has_float_atomic_add ? "Native float atomics" : (p->has_float_atomics ? "Load/store HW, add CAS" : "32-bit CAS emulation")),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_float_atomic_add ? "shaderBufferFloat32AtomicAdd = true" : "Emulatable via 32-bit CAS loop"), true);
  json_cap("acq_rel_atomics", (p->gpu_found && p->has_vk_mem_model) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_vk_mem_model) ? "Vulkan Memory Model" : "Unsupported",
           (p->gpu_found && p->has_vk_mem_model) ? "vulkanMemoryModel and deviceScope = true" : "vulkanMemoryModel unavailable", true);
  json_cap("mesh_shading", (p->gpu_found && p->has_mesh_shader) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_mesh_shader) ? "Supported in HW" : "Unsupported",
           (p->gpu_found && p->has_mesh_shader) ? "VK_EXT_mesh_shader supported" : "No hardware mesh shader pipeline", true);

  json_cap("m4_argument_tables", !p->gpu_found ? "hw_limit"
                                 : ((p->has_bda && p->has_update_after_bind) ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported"
           : ((p->has_bda && p->has_update_after_bind) ? "Bindless descriptor table"
           : (p->has_bda ? "BDA buffer table" : "Descriptor set table")),
           !p->gpu_found ? "No Vulkan GPU detected"
           : ((p->has_bda && p->has_update_after_bind) ? "Mappable via descriptor indexing and bufferDeviceAddress"
           : (p->has_bda ? "Mappable via bufferDeviceAddress" : "Emulatable via descriptor bindings without BDA")), true);
  json_cap("m4_command_allocators", p->gpu_found ? "emulatable" : "hw_limit",
           p->gpu_found ? "Host allocator mapping" : "Unsupported",
           p->gpu_found ? "Implementable over VkCommandPool" : "No Vulkan GPU detected", true);
  json_cap("m4_command_barriers", !p->gpu_found ? "hw_limit" : (p->has_sync2 ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_sync2 ? "Vulkan sync2 barriers" : "Vulkan 1.1 barriers"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_sync2 ? "Mappable to vkCmdPipelineBarrier2" : "Mappable to vkCmdPipelineBarrier"), true);
  json_cap("m4_tensors_ml", !p->gpu_found ? "hw_limit" : (p->has_coop_matrix ? hw_ok : "emulatable"),
           !p->gpu_found ? "Unsupported" : (p->has_coop_matrix ? "Cooperative matrix kernels" : "Compute shader lowering"),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (p->has_coop_matrix ? "Backed by hardware matrix units" : "Lowered to compute kernels"), true);
  json_cap("m4_counter_heaps", (p->gpu_found && p->has_timestamps) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_timestamps) ? "VkQueryPool timestamps" : "Unsupported",
           (p->gpu_found && p->has_timestamps) ? "64-bit hardware timestamp queries present" : "No compute timestamp queries", true);

  json_cap("simd_rotate", !p->gpu_found ? "hw_limit"
                          : (has_rotate ? hw_ok : (has_shuffle ? "emulatable" : "hw_limit")),
           !p->gpu_found ? "Unsupported"
           : (has_rotate ? "ROTATE supported" : (has_shuffle ? "Emulatable via shuffle" : "Unsupported")),
           !p->gpu_found ? "No Vulkan GPU detected"
           : (has_rotate ? "SUBGROUP_FEATURE_ROTATE_BIT present"
           : (has_shuffle ? "Emulatable with subgroup shuffle" : "Neither ROTATE nor SHUFFLE subgroup ops present")), true);
  json_cap("m4_placement_sparse", (p->gpu_found && p->sparse_va_bytes > 0) ? hw_ok : "hw_limit",
           (p->gpu_found && p->sparse_va_bytes > 0) ? "Sparse VA present" : "Unsupported",
           (p->gpu_found && p->sparse_va_bytes > 0) ? "Hardware sparse binding present" : "No hardware sparse memory binding", true);
  json_cap("bc_compression", (p->gpu_found && p->has_bc) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_bc) ? "BC1-BC7 in HW" : "Unsupported",
           (p->gpu_found && p->has_bc) ? "textureCompressionBC = true" : "textureCompressionBC = false", true);

  const char *a64_st = (p->gpu_found && p->has_int64_buf_atomics) ? hw_ok : "hw_limit";
  const char *a64_val = !p->gpu_found || !p->has_int64_buf_atomics ? "Unsupported"
                        : (p->has_int64_shared_atomics && p->has_int64_img_atomics) ? "Full Int64 atomics"
                        : p->has_int64_shared_atomics ? "Buffer/shared Int64 atomics"
                        : "Buffer Int64 atomics";
  const char *a64_det = !p->gpu_found || !p->has_int64_buf_atomics ? "No hardware 64-bit integer atomics"
                        : (p->has_int64_shared_atomics && p->has_int64_img_atomics) ? "Buffer, shared, and image 64-bit integer atomics"
                        : p->has_int64_shared_atomics ? "shaderBufferInt64Atomics and shaderSharedInt64Atomics"
                        : "shaderBufferInt64Atomics = true";
  json_cap("atomics_64bit", a64_st, a64_val, a64_det, true);

  json_cap("sampler_minmax", (p->gpu_found && p->has_sampler_minmax) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_sampler_minmax) ? "Supported in HW" : "Unsupported",
           (p->gpu_found && p->has_sampler_minmax) ? "samplerFilterMinmax = true" : "samplerFilterMinmax = false", true);
  json_cap("depth_bounds", (p->gpu_found && p->has_depth_bounds) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_depth_bounds) ? "Supported in HW" : "Unsupported",
           (p->gpu_found && p->has_depth_bounds) ? "depthBounds = true" : "depthBounds = false", true);
  json_cap("fp32_denormals", (p->gpu_found && p->has_fp32_denorm) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_fp32_denorm) ? "Preserved in HW" : "Not guaranteed",
           (p->gpu_found && p->has_fp32_denorm) ? "shaderDenormPreserveFloat32 = true" : "shaderDenormPreserveFloat32 = false", true);

  char zc_val[64], zc_det[160];
  const char *zc_st = "hw_limit";
  if (!p->gpu_found) {
    snprintf(zc_val, sizeof(zc_val), "Unsupported");
    snprintf(zc_det, sizeof(zc_det), "No Vulkan GPU detected");
  } else if (strcmp(p->zero_copy_tier, "dmabuf") == 0) {
    zc_st = hw_ok;
    snprintf(zc_val, sizeof(zc_val), "udmabuf (Tier 1)");
    if (p->udmabuf_limit_mb > 0) {
      snprintf(zc_det, sizeof(zc_det), "Zero-copy BAR4 import (pad %llu B, udmabuf limit %d MiB)",
               (unsigned long long)p->buf_extra_pad, p->udmabuf_limit_mb);
    } else {
      snprintf(zc_det, sizeof(zc_det), "Zero-copy BAR4 import (pad %llu B)",
               (unsigned long long)p->buf_extra_pad);
    }
  } else if (strcmp(p->zero_copy_tier, "host_ptr") == 0) {
    zc_st = hw_ok;
    snprintf(zc_val, sizeof(zc_val), "external_memory_host (Tier 2)");
    snprintf(zc_det, sizeof(zc_det), "Zero-copy host pointer import (align %llu B)", (unsigned long long)p->host_ptr_align);
  } else {
    zc_st = "software";
    snprintf(zc_val, sizeof(zc_val), "Staging memcpy (Tier 3)");
    snprintf(zc_det, sizeof(zc_det), "Host-visible Vulkan staging fallback");
  }
  json_cap("zero_copy_tier", zc_st, zc_val, zc_det, true);

  const char *coh_st = "software";
  const char *coh_val = "KVM dirty-log + PoC sync";
  const char *coh_det = "Non-coherent DMA; kido uses KVM dirty-log and parallel ARM64 dc civac";
  if (!p->gpu_found) {
    coh_st = "hw_limit";
    coh_val = "Unsupported";
    coh_det = "No Vulkan GPU detected";
  } else if (p->dma_coherent) {
    coh_st = hw_ok;
    coh_val = "Hardware DMA coherent";
    coh_det = "Firmware and live 4-round L1-hot GPU copy probe verified coherent (zero dc civac)";
  } else if (strcmp(p->zero_copy_tier, "staging") == 0) {
    coh_val = "Host staging copy";
    coh_det = "Staging memcpy path (zero-copy BAR4 import unavailable)";
  } else if (!p->coherency_tested && p->fw_dma_coherent) {
    coh_val = "Untested (probe skipped)";
    coh_det = "Firmware indicates coherent DMA, but live copy probe was skipped";
  } else if (!p->fw_dma_coherent && p->coherency_tested && p->coherency_passed) {
    coh_val = "KVM dirty-log + PoC sync";
    coh_det = "Forced live probe passed, but firmware declares non-coherent; kido defaults to PoC sync";
  }
  json_cap("dma_coherency", coh_st, coh_val, coh_det, true);

  json_cap("async_pipeline", p->gpu_found ? hw_ok : "hw_limit",
           p->gpu_found ? "4-slot async ring" : "Unsupported",
           p->gpu_found ? "Up to 4 in-flight Vulkan command buffers" : "No Vulkan GPU detected", true);

  if (p->gpu_found && p->has_timestamps) {
    snprintf(ts_det, sizeof(ts_det), "%.2f ns period via VkQueryPool", p->timestamp_period_ns);
  } else {
    snprintf(ts_det, sizeof(ts_det), "No compute timestamp queries");
  }
  json_cap("hw_timestamps", (p->gpu_found && p->has_timestamps) ? hw_ok : "hw_limit",
           (p->gpu_found && p->has_timestamps) ? "64-bit HW timestamps" : "Unsupported", ts_det, true);
  json_cap("measured_utilization", p->gpu_found ? hw_ok : "hw_limit",
           p->gpu_found ? "Not benchmarked" : "Unsupported",
           p->gpu_found ? "Probed hardware only; run metal_pipeline_bench inside a Darwin guest to verify"
                        : "No Vulkan GPU detected", false);

  printf("  }\n}\n");
}

static void print_human(const probe_info_t *p) {
  printf("kido-probe: ARM64 Linux hardware report\n");
  printf("=======================================\n\n");

  printf("Host\n");
  printf("  Board:         %s\n", p->board_model);
  printf("  OS:            %s\n", p->os_name);
  printf("  Kernel:        Linux %s (%s)\n", p->kernel_release, p->machine);
  printf("  Memory:        %llu MiB (page size %u B)\n\n",
         (unsigned long long)p->total_ram_mb, p->host_page_size);

  printf("CPU and Virtualization\n");
  printf("  Topology:      %s (%u cores%s)\n", p->cpu_summary, p->n_cpus,
         p->heterogeneous ? ", heterogeneous big.LITTLE" : "");
  if (p->heterogeneous) {
    printf("  KVM pinning:   auto-pins vCPUs to %ux %s cluster\n",
           p->clusters[0].count, p->clusters[0].name);
  }
  if (p->kvm_accessible) {
    printf("  KVM:           available (API v%d, %s)\n", p->kvm_api_version, p->arch_summary);
    printf("  GIC:           GICv3=%s, GICv2=%s, PMUv3=%s\n",
           p->kvm_gicv3 ? "yes" : "no", p->kvm_gicv2 ? "yes" : "no", p->kvm_pmu_v3 ? "yes" : "no");
    printf("  CPU verdict:   READY for native-speed KidoDarwin micro-VMs via kido-vmm\n\n");
  } else {
    const char *why = (p->kvm_errno == EACCES || p->kvm_errno == EPERM)
                          ? "permission denied (add user to kvm group)"
                          : (p->kvm_present ? strerror(p->kvm_errno) : "/dev/kvm not present");
    printf("  KVM:           UNAVAILABLE (%s)\n", why);
    printf("  CPU verdict:   Native KVM unavailable. Software CPU emulation (KIDO_ACCEL=tcg via\n");
    printf("                 QEMU) can still boot Darwin guests at lower CPU performance.\n\n");
  }

  printf("GPU and Vulkan\n");
  if (!p->vk_lib_loaded) {
    printf("  Vulkan:        libvulkan.so.1 not found\n");
    printf("  GPU verdict:   CPU-only mode (install Mesa/vendor Vulkan ICD for Metal compute)\n\n");
    return;
  }
  if (!p->gpu_found) {
    printf("  Vulkan:        loaded, no compute-capable GPU found\n\n");
    return;
  }
  printf("  Device:        %s (vendor=0x%04x, device=0x%04x%s)\n",
         p->gpu_name, p->vendor_id, p->device_id, p->gpu_is_cpu ? ", CPU software renderer" : "");
  printf("  Driver:        %s (%s)\n",
         p->driver_name[0] ? p->driver_name : "unknown",
         p->driver_info[0] ? p->driver_info : "unknown");
  printf("  Vulkan API:    %u.%u.%u (conformance %s)\n",
         VK_API_VERSION_MAJOR(p->vk_api_ver), VK_API_VERSION_MINOR(p->vk_api_ver),
         VK_API_VERSION_PATCH(p->vk_api_ver), p->conformance[0] ? p->conformance : "n/a");
  printf("  Zero-copy:     %s (extra pad=%llu B, udmabuf=%s",
         p->zero_copy_tier, (unsigned long long)p->buf_extra_pad,
         p->udmabuf_accessible ? "accessible" : "unavailable");
  if (p->udmabuf_limit_mb > 0) {
    printf(", limit %d MiB", p->udmabuf_limit_mb);
  }
  printf(")\n");
  const char *coh_label = p->dma_coherent ? "HARDWARE COHERENT (zero dc civac)"
                          : (!p->fw_dma_coherent && p->coherency_tested && p->coherency_passed)
                              ? "non-coherent by default (forced live probe passed; set KIDO_VK_DMA_COHERENT=1 to override)"
                              : "non-coherent (KVM dirty-log + PoC sync)";
  printf("  Coherency:     %s (firmware=%s, live_probe=%s)\n",
         coh_label,
         p->fw_dma_coherent ? "candidate" : "non-coherent",
         p->coherency_tested ? (p->coherency_passed ? "passed" : "failed") : "skipped");
  printf("  Timestamps:    %s", p->has_timestamps ? "64-bit HW" : "unavailable");
  if (p->has_timestamps) printf(" (%.2f ns/tick)", p->timestamp_period_ns);
  printf("\n\n");

  printf("Apple Metal Spec Limits and Key Features\n");
  printf("  Threads / TG:  %u (Apple2: 512, Apple4+: 1024)\n", p->max_tg_invocations);
  printf("  Shared Memory: %u KB (Apple2: 16 KB, Apple4+: 32 KB)\n", p->max_tg_mem_bytes / 1024u);
  printf("  Buffer Slots:  %u / stage (Apple2+: 31)\n", p->max_ssbo_per_stage);
  printf("  Max 2D Tex:    %u px (Apple2: 8192, Apple3-9: 16384, Apple10+: 32768)\n", p->max_image_2d);
  printf("  Viewports:     %u (Apple5+: 16) | MSAA: %ux (Apple10+: 8x)\n", p->max_viewports, p->max_msaa);
  printf("  Types:         FP16=%s, Int8=%s, Int64=%s, BFloat16=%s\n",
         p->has_fp16 ? "hw" : "emu", (p->has_int8 && p->has_storage_8bit) ? "hw" : "emu",
         p->has_int64 ? "hw" : "emu", p->has_bfloat16 ? "hw" : "emu");
  printf("  SIMD:          subgroup=%u, quad=%s, shuffle=%s, reduce=%s, rotate=%s, matrix=%s\n",
         p->subgroup_size,
         (p->subgroup_ops & VK_SUBGROUP_FEATURE_QUAD_BIT) ? "hw" : "no",
         (p->subgroup_ops & VK_SUBGROUP_FEATURE_SHUFFLE_BIT) ? "hw" : "no",
         (p->subgroup_ops & VK_SUBGROUP_FEATURE_ARITHMETIC_BIT) ? "hw" : "no",
         (p->subgroup_ops & VK_SUBGROUP_FEATURE_ROTATE_BIT) ? "hw" : "emu",
         p->has_coop_matrix ? "hw" : "emu");
  printf("  Advanced:      BDA=%s, Bindless=%s, Sparse=%s, Int64Atomic=%s, Mesh=%s, RayQuery=%s\n\n",
         p->has_bda ? "yes" : "no", p->has_update_after_bind ? "yes" : "no",
         p->sparse_va_bytes > 0 ? "yes" : "no", p->has_int64_buf_atomics ? "yes" : "no",
         p->has_mesh_shader ? "yes" : "no", p->has_ray_query ? "yes" : "no");

  printf("Run with '--json' to export a board capability JSON record.\n");
}

static void usage(void) {
  fprintf(stderr,
          "Usage: kido-probe [OPTIONS]\n\n"
          "Probe ARM64 Linux CPU, KVM, Vulkan GPU, and Apple Metal capability alignment.\n\n"
          "Options:\n"
          "  --json                 Emit board capability JSON matching kido.sh docs schema\n"
          "  --no-coherency-test    Skip the live GPU DMA coherency copy probe\n"
          "  --force-coherency-test Run the live GPU DMA coherency probe even on DT non-coherent boards\n"
          "  -h, --help             Show this help message\n");
}

int main(int argc, char **argv) {
  bool as_json = false;
  bool no_coh = false;
  bool force_coh = false;
  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "--json") == 0) as_json = true;
    else if (strcmp(argv[i], "--no-coherency-test") == 0) no_coh = true;
    else if (strcmp(argv[i], "--force-coherency-test") == 0) force_coh = true;
    else if (strcmp(argv[i], "-h") == 0 || strcmp(argv[i], "--help") == 0) { usage(); return 0; }
    else {
      fprintf(stderr, "kido-probe: unknown option: %s\n", argv[i]);
      usage();
      return 2;
    }
  }
  if (no_coh && force_coh) {
    fprintf(stderr, "kido-probe: --no-coherency-test and --force-coherency-test are mutually exclusive\n");
    return 2;
  }

  probe_info_t info;
  memset(&info, 0, sizeof(info));
  probe_system_and_cpu(&info);
  probe_kvm(&info);
  probe_vulkan(&info, !no_coh, force_coh);

  if (as_json) print_json(&info);
  else print_human(&info);
  return 0;
}
