# Copyright (c) 2026 The kido authors
# SPDX-License-Identifier: Apache-2.0

CC ?= cc
CFLAGS ?= -O2 -std=c11 -Wall -Wextra -Wno-unused-parameter
LDFLAGS ?= -ldl
OUT ?= kido-probe

.PHONY: all clean test

all: $(OUT)

$(OUT): kido-probe.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

test: $(OUT)
	./$(OUT)
	./$(OUT) --json >/dev/null

clean:
	rm -f kido-probe
