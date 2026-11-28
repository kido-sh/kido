# Copyright (c) 2026 The kido authors
# SPDX-License-Identifier: Apache-2.0

CC ?= gcc
CXX ?= g++
CFLAGS ?= -O2 -std=c11 -Wall -Wextra -Wno-unused-parameter
CXXFLAGS ?= -O2 -std=c++17 -fno-exceptions -Wall -Wextra -Wno-unused-parameter
LDFLAGS ?= -lvulkan -pthread

SPIRV_INCDIR ?= /usr/include
LLVM_CONFIG ?= $(shell command -v llvm-config-19 || command -v llvm-config || true)
HAS_SPIRV_HDR := $(shell test -f $(SPIRV_INCDIR)/spirv/unified1/spirv.hpp && echo 1 || echo 0)
ENABLE_AIR2SPV ?= $(if $(and $(LLVM_CONFIG),$(filter 1,$(HAS_SPIRV_HDR))),1,0)

ifeq ($(ENABLE_AIR2SPV),1)
LLVM_CXXFLAGS := $(shell $(LLVM_CONFIG) --cxxflags) -I$(SPIRV_INCDIR)
LLVM_LDFLAGS := $(shell $(LLVM_CONFIG) --ldflags)
LLVM_LIBS := $(shell $(LLVM_CONFIG) --libs core bitreader scalaropts transformutils analysis passes support)
LLVM_TEST_LIBS := $(shell $(LLVM_CONFIG) --libs asmparser bitwriter core bitreader scalaropts transformutils analysis passes support)
AIR_CFLAGS := -DKIDO_HAVE_AIR2SPV
AIR_OBJS := metallib.o air2spv.o
LINK_CMD := $(CXX)
LINK_EXTRA := $(LLVM_LDFLAGS) $(LLVM_LIBS)
else
AIR_CFLAGS :=
AIR_OBJS :=
LINK_CMD := $(CC)
LINK_EXTRA :=
endif

.PHONY: all test clean

all: kido-vk-worker

kido-vk-worker.o: kido-vk-worker.c msl2glsl.h ../../vmm/kmtl_shm.h ../../vmm/kmtl_proto.h $(if $(filter 1,$(ENABLE_AIR2SPV)),air2spv.h metallib.h)
	$(CC) $(CFLAGS) $(AIR_CFLAGS) -c -o $@ kido-vk-worker.c

msl2glsl.o: msl2glsl.c msl2glsl.h
	$(CC) $(CFLAGS) -c -o $@ msl2glsl.c

kmtl_shm.o: ../../vmm/kmtl_shm.c ../../vmm/kmtl_shm.h
	$(CC) $(CFLAGS) -c -o $@ ../../vmm/kmtl_shm.c

metallib.o: metallib.cpp metallib.h
	$(CXX) $(CXXFLAGS) -c -o $@ metallib.cpp

air2spv.o: air2spv.cpp air2spv.h metallib.h
	$(CXX) $(CXXFLAGS) $(LLVM_CXXFLAGS) -c -o $@ air2spv.cpp

kido-vk-worker: kido-vk-worker.o msl2glsl.o kmtl_shm.o $(AIR_OBJS)
	$(LINK_CMD) -o $@ kido-vk-worker.o msl2glsl.o kmtl_shm.o $(AIR_OBJS) $(LDFLAGS) $(LINK_EXTRA)

test: msl2glsl_test $(if $(filter 1,$(ENABLE_AIR2SPV)),air2spv_test)
	./msl2glsl_test
ifeq ($(ENABLE_AIR2SPV),1)
	./air2spv_test
endif

msl2glsl_test: msl2glsl_test.c msl2glsl.c msl2glsl.h
	$(CC) -O1 -g -std=c11 -Wall -Wextra -fsanitize=address,undefined -o $@ msl2glsl_test.c msl2glsl.c

air2spv_asan.o: air2spv.cpp air2spv.h metallib.h
	$(CXX) -O1 -g -std=c++17 -fno-exceptions -Wall -Wextra -Wno-unused-parameter -fsanitize=address,undefined $(LLVM_CXXFLAGS) -c -o $@ air2spv.cpp

metallib_asan.o: metallib.cpp metallib.h
	$(CXX) -O1 -g -std=c++17 -fno-exceptions -Wall -Wextra -Wno-unused-parameter -fsanitize=address,undefined -c -o $@ metallib.cpp

air2spv_test: air2spv_test.cpp air2spv_asan.o metallib_asan.o air2spv.h metallib.h
	$(CXX) -O1 -g -std=c++17 -fno-exceptions -Wall -Wextra -Wno-unused-parameter -fsanitize=address,undefined $(LLVM_CXXFLAGS) -o $@ air2spv_test.cpp air2spv_asan.o metallib_asan.o $(LLVM_LDFLAGS) $(LLVM_TEST_LIBS)

clean:
	rm -f kido-vk-worker msl2glsl_test air2spv_test *.o
