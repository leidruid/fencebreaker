#
# Copyright (C) 2026 The fencebreaker contributors
#
# This is free software, licensed under the Apache License, Version 2.0 .
#

include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-fencebreaker
PKG_VERSION:=0.1.0
PKG_RELEASE:=17
PKG_LICENSE:=Apache-2.0
PKG_MAINTAINER:=The fencebreaker contributors

LUCI_TITLE:=fencebreaker - multi-tenant policy routing
LUCI_DEPENDS:=+fencebreaker +fencebreaker-coredns

LUCI_PKGARCH:=all

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
