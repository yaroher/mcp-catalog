.PHONY: build rebuild install uninstall run clean

BINARY=mcp-manager
BUILD_DIR=./build
USER_BIN_DIR=$(HOME)/.local/bin
USER_SYSTEMD_DIR=$(HOME)/.config/systemd/user
USER_CONFIG_DIR=$(HOME)/.config/mcp-manager
USER_SERVICE=$(USER_SYSTEMD_DIR)/mcp-manager.service

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-manager

rebuild: clean
	mkdir -p $(BUILD_DIR)
	go build -a -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-manager

run: build
	$(BUILD_DIR)/$(BINARY) --port 9847

# Install or reinstall for current user (no sudo required)
install: rebuild
	mkdir -p $(USER_BIN_DIR)
	install -m 0755 $(BUILD_DIR)/$(BINARY) $(USER_BIN_DIR)/$(BINARY)
	mkdir -p $(USER_SYSTEMD_DIR)
	install -m 0644 mcp-manager.service $(USER_SERVICE)
	rm -f $(HOME)/.config/systemd/user/multi-user.target.wants/mcp-manager.service
	mkdir -p $(USER_CONFIG_DIR)
	test -f $(USER_CONFIG_DIR)/config.json || printf '{\n  "mcpServers": {}\n}\n' > $(USER_CONFIG_DIR)/config.json
	systemctl --user daemon-reload
	systemctl --user enable --now mcp-manager
	systemctl --user restart mcp-manager
	@echo ""
	@echo "Installed/Reinstalled for user!"
	@echo "Service:"
	@echo "  systemctl --user status mcp-manager"
	@echo ""
	@echo "UI available at: http://localhost:9847"

uninstall:
	-systemctl --user stop mcp-manager
	-systemctl --user disable mcp-manager
	-rm -f $(USER_BIN_DIR)/$(BINARY)
	-rm -f $(USER_SERVICE)
	-systemctl --user daemon-reload

clean:
	rm -rf $(BUILD_DIR)
