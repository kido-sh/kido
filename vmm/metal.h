/*
 * Copyright (c) 2026 The kido authors
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once

#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>

NS_ASSUME_NONNULL_BEGIN

void kido_vmm_logf(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

#if defined(__MAC_27_0)

// Configures and owns one VirtIO-Metal custom device (macOS 27+) attached to a VM.
// Returns nil on error and populates *errorOut.
API_AVAILABLE(macos(27.0))
@interface KidoMetalDevice : NSObject <VZCustomVirtioDeviceConfigurationDelegate, VZCustomVirtioDeviceDelegate>

- (nullable instancetype)initWithMemoryLimitBytes:(uint64_t)limitBytes error:(NSError **)errorOut;
- (VZCustomVirtioDeviceConfiguration *)makeConfiguration;
- (void)shutdown;

@end

#endif

NS_ASSUME_NONNULL_END
