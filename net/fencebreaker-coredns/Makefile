include $(TOPDIR)/rules.mk

PKG_NAME:=fencebreaker-coredns
PKG_VERSION:=1.15.0
PKG_RELEASE:=7

PKG_SOURCE:=coredns-$(PKG_VERSION).tar.gz
PKG_SOURCE_URL:=https://codeload.github.com/coredns/coredns/tar.gz/v$(PKG_VERSION)?
PKG_HASH:=skip
# the source tarball unpacks into coredns-<ver>/, not <pkgname>-<ver>/
PKG_BUILD_DIR:=$(BUILD_DIR)/coredns-$(PKG_VERSION)

PKG_LICENSE:=Apache-2.0
PKG_MAINTAINER:=fencebreaker project

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/coredns/coredns
GO_PKG_BUILD_PKG:=github.com/coredns/coredns
GO_PKG_LDFLAGS:=-s -w

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

# After unpack the standard patch set (patches/001-add-fbtenant-plugin.patch)
# wires the plugin into plugin.cfg and the generated zplugin/zdirectives; this
# hook only brings in the plugin module and its go.mod replace directive.
define Build/Prepare
	$(call Build/Prepare/Default)
	$(CP) ./../../coredns-plugin/fbtenant $(PKG_BUILD_DIR)/plugin/fbtenant
	printf '\nrequire github.com/leidruid/fencebreaker/coredns-plugin/fbtenant v0.0.0-00010101000000-000000000000\nreplace github.com/leidruid/fencebreaker/coredns-plugin/fbtenant => ./plugin/fbtenant\nrequire github.com/ameshkov/dnscrypt v1.1.0\nrequire github.com/ameshkov/dnsstamps v1.0.1\nrequire github.com/aead/poly1305 v0.0.0-20180717145839-3fee0db0b635\nrequire github.com/aead/chacha20 v0.0.0-20180709150244-8b13a72661da\n' >> $(PKG_BUILD_DIR)/go.mod
	# the build runs with a frozen go.sum: pull in hashes of the modules the
	# plugin adds to the module graph (ameshkov/dnscrypt, dnsstamps, ...)
	cat ./files/go.sum.add >> $(PKG_BUILD_DIR)/go.sum
endef

define Package/fencebreaker-coredns
  TITLE:=CoreDNS with the fbtenant plugin (fencebreaker DNS engine)
  SECTION:=net
  CATEGORY:=Network
  URL:=https://github.com/leidruid/fencebreaker
  DEPENDS:=$(GO_ARCH_DEPENDS) +ca-bundle
endef

define Package/fencebreaker-coredns/description
 CoreDNS built with the external fbtenant plugin: one instance serves all
 fencebreaker tenants, resolves via per-tenant upstreams and learns A records
 into nftables timeout sets without re-adding live elements.
 Installed as /usr/bin/fb-coredns to avoid clashing with other coredns
 packages.
endef

define Package/fencebreaker-coredns/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/coredns $(1)/usr/bin/fb-coredns
endef

# A running instance would keep serving with the replaced binary until the
# next Corefile change: restart it as soon as the package lands.
define Package/fencebreaker-coredns/postinst
#!/bin/sh
[ -x /etc/init.d/fencebreaker-coredns ] && \
	[ -f /var/run/fencebreaker/Corefile ] && \
	/etc/init.d/fencebreaker-coredns restart >/dev/null 2>&1
exit 0
endef

$(eval $(call BuildPackage,fencebreaker-coredns))
