# Copyright (c) 2026 The kido authors
# SPDX-License-Identifier: Apache-2.0

CC ?= gcc
CFLAGS ?= -O2 -std=c11 -Wall -Wextra -Wno-unused-parameter
LDFLAGS ?= -lvulkan

.PHONY: all test clean

all: kido-vk-worker

kido-vk-worker: kido-vk-worker.c msl2glsl.c msl2glsl.h ../../vmm/kmtl_shm.c ../../vmm/kmtl_shm.h ../../vmm/kmtl_proto.h
	$(CC) $(CFLAGS) -o $@ kido-vk-worker.c msl2glsl.c ../../vmm/kmtl_shm.c $(LDFLAGS)

test: msl2glsl_test
	./msl2glsl_test

msl2glsl_test: msl2glsl_test.c msl2glsl.c msl2glsl.h
	$(CC) -O1 -g -std=c11 -Wall -Wextra -fsanitize=address,undefined -o $@ msl2glsl_test.c msl2glsl.c

clean:
	rm -f kido-vk-worker msl2glsl_test
