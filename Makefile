# https://go.dev/wiki/MinimumRequirements

PWD := $(shell pwd)
SRC_DIR := $(PWD)/src
BIN_DIR := $(PWD)/binary
VERSION := $(shell awk -F'"' '/const\s+AppVersion\s*=/{print $$2}' $(SRC_DIR)/main.go)

CPU_FLAGS := $(shell grep -m1 '^flags' /proc/cpuinfo)
GOAMD64_LEVEL := v1

ifneq (,$(findstring avx512f,$(CPU_FLAGS)))
    ifneq (,$(findstring avx512bw,$(CPU_FLAGS)))
        ifneq (,$(findstring avx512cd,$(CPU_FLAGS)))
            ifneq (,$(findstring avx512dq,$(CPU_FLAGS)))
                ifneq (,$(findstring avx512vl,$(CPU_FLAGS)))
                    GOAMD64_LEVEL := v4
                endif
            endif
        endif
    endif
else ifneq (,$(findstring avx2,$(CPU_FLAGS)))
    ifneq (,$(findstring fma,$(CPU_FLAGS)))
        ifneq (,$(findstring bmi2,$(CPU_FLAGS)))
            GOAMD64_LEVEL := v3
        endif
    endif
else ifneq (,$(findstring sse4_2,$(CPU_FLAGS)))
    ifneq (,$(findstring popcnt,$(CPU_FLAGS)))
        GOAMD64_LEVEL := v2
    endif
endif

LDFLAGS := -s -w -extldflags "-static" -X main.AppVersion=$(VERSION)

.PHONY: all linux windows clean info

all: linux windows

info:
	@echo "CPU Flags: $(CPU_FLAGS)"
	@echo "GOAMD64 Level: $(GOAMD64_LEVEL)"

linux:
	cd $(SRC_DIR) && CGO_ENABLED=0 GOAMD64=$(GOAMD64_LEVEL) go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/goget

windows:
	cd $(SRC_DIR) && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=$(GOAMD64_LEVEL) go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/goget.exe

clean:
	rm -f $(BIN_DIR)/*