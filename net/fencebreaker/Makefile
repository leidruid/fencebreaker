include $(TOPDIR)/rules.mk

PKG_NAME:=fencebreaker
PKG_VERSION:=0.1.0
PKG_RELEASE:=9

PKG_LICENSE:=Apache-2.0
PKG_MAINTAINER:=fencebreaker project

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/leidruid/fencebreaker/daemon
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/fencebreakerd
GO_PKG_LDFLAGS:=-s -w
GO_PKG_LDFLAGS_X:=$(GO_PKG)/cmd/fencebreakerd.Version=$(PKG_VERSION)

# Sources live in this repository (daemon/ next to the feed root), so no
# PKG_SOURCE: Build/Prepare copies them in.
PKG_SOURCE:=

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) ./../../daemon/go.mod ./../../daemon/go.sum $(PKG_BUILD_DIR)/
	$(CP) ./../../daemon/cmd $(PKG_BUILD_DIR)/
	$(CP) ./../../daemon/internal $(PKG_BUILD_DIR)/
endef

define Package/fencebreaker
  TITLE:=fencebreaker - multi-tenant policy routing
  SECTION:=net
  CATEGORY:=Network
  URL:=https://github.com/leidruid/fencebreaker
  # sing-box is optional: required only for VLESS tenants (install manually
  # on space-constrained systems); the singbox init script skips starting
  # when the binary is absent.
  DEPENDS:=$(GO_ARCH_DEPENDS) +nftables +kmod-nft-nat +ca-bundle +ca-certificates \
	+fencebreaker-coredns
  MENU:=1
endef

define Package/fencebreaker/description
 Multi-tenant policy routing for OpenWrt (fw4/nftables): route selected
 networks/hosts to selected resources through per-tenant egress channels
 (interface or VLESS via sing-box), with per-tenant DNS (CoreDNS fbtenant)
 that learns resolved addresses into nftables sets.
endef

define Package/fencebreaker/conffiles
/etc/config/fencebreaker
endef

define Package/fencebreaker/install
	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/fencebreakerd $(1)/usr/sbin/fencebreakerd
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/fencebreaker.init $(1)/etc/init.d/fencebreaker
	$(INSTALL_BIN) ./files/fencebreaker-coredns.init $(1)/etc/init.d/fencebreaker-coredns
	$(INSTALL_BIN) ./files/fencebreaker-singbox.init $(1)/etc/init.d/fencebreaker-singbox
	$(INSTALL_DIR) $(1)/etc/uci-defaults
	$(INSTALL_BIN) ./files/uci-defaults-fencebreaker $(1)/etc/uci-defaults/99-fencebreaker
	$(INSTALL_DIR) $(1)/usr/share/fencebreaker
	$(INSTALL_DATA) ./files/fencebreaker.conf $(1)/usr/share/fencebreaker/fencebreaker.conf
	$(INSTALL_DIR) $(1)/etc/fencebreaker/lists
endef

# A package upgrade replaces the daemon binary; a running fencebreakerd would
# keep the old code (and old state expectations) until manually restarted.
define Package/fencebreaker/postinst
#!/bin/sh
[ -x /etc/init.d/fencebreaker ] && \
	/etc/init.d/fencebreaker running && \
	/etc/init.d/fencebreaker restart >/dev/null 2>&1
exit 0
endef

$(eval $(call BuildPackage,fencebreaker))
