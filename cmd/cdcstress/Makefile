GO ?= go
OUTPUT_DIR ?= build

BINARY_NAME := cdcstress
CONFIG_FILE := cdcstress.example.json

.DEFAULT_GOAL := all

.PHONY: all linux macos linux-amd64 linux-arm64 macos-amd64 macos-arm64 clean

all: linux macos

linux: linux-amd64 linux-arm64

macos: macos-amd64 macos-arm64

define build_target
$(1):
	@echo "Building $(2)/$(3)..."
	@mkdir -p "$(OUTPUT_DIR)/$(2)/$(3)"
	@CGO_ENABLED=0 GOOS=$(2) GOARCH=$(3) "$(GO)" build -trimpath \
		-o "$(OUTPUT_DIR)/$(2)/$(3)/$(BINARY_NAME)" .
	@cp "$(CONFIG_FILE)" "$(OUTPUT_DIR)/$(2)/$(3)/$(CONFIG_FILE)"
	@tar -C "$(OUTPUT_DIR)/$(2)/$(3)" -czf \
		"$(OUTPUT_DIR)/$(2)/$(3)/$(BINARY_NAME)-$(2)-$(3).tar.gz" \
		"$(BINARY_NAME)" "$(CONFIG_FILE)"
	@echo "Created $(OUTPUT_DIR)/$(2)/$(3)/$(BINARY_NAME)-$(2)-$(3).tar.gz"
endef

$(eval $(call build_target,linux-amd64,linux,amd64))
$(eval $(call build_target,linux-arm64,linux,arm64))
$(eval $(call build_target,macos-amd64,darwin,amd64))
$(eval $(call build_target,macos-arm64,darwin,arm64))

clean:
	@if [ -z "$(OUTPUT_DIR)" ] || [ "$(OUTPUT_DIR)" = "/" ] || \
		[ "$(OUTPUT_DIR)" = "." ] || [ "$(OUTPUT_DIR)" = ".." ]; then \
		echo "Refusing to clean unsafe OUTPUT_DIR: $(OUTPUT_DIR)" >&2; \
		exit 1; \
	fi
	@rm -rf -- "$(OUTPUT_DIR)"
	@echo "Removed $(OUTPUT_DIR)"
