FROM --platform=$BUILDPLATFORM alpine AS build
ARG TARGETPLATFORM \
    TARGETARCH \
    TARGETOS \
    TARGETVARIANT \
    WITH_GVISOR=1 \
    WITH_NAIVE=1 \
    WITH_UPX=1 \
    NAIVE_VERSION=""

# сборка mihomo с исправлениями загрузки битых ссылок в публичных подписках
WORKDIR /src

# инструменты для сборки
RUN apk add --no-cache git go bash curl jq gzip tar xz unzip ca-certificates make upx

ENV GOCACHE=/var/cache/go-build \
    GOTMPDIR=/var/tmp/go-tmp \
    GOFLAGS="-modcacherw"
RUN mkdir -p /var/cache/go-build /var/tmp/go-tmp

# Переключаемся на нужный тэг
# тег последнего релиза: API GitHub (лимит 60 req/h без токена) -> запасной путь через git ls-remote
RUN TAG="$(curl -fsSL https://api.github.com/repos/MetaCubeX/mihomo/releases/latest 2>/dev/null | jq -r '.tag_name // empty')" ; \
    if [ -z "$TAG" ]; then \
        TAG="$(git ls-remote --tags https://github.com/MetaCubeX/mihomo.git | grep -o 'refs/tags/v[0-9]*\.[0-9]*\.[0-9]*$' | sed 's@refs/tags/@@' | sort -V | tail -1)"; \
    fi && \
    [ -n "$TAG" ] && echo "mihomo tag: $TAG" && \
    git clone --depth=1 --branch "$TAG" --single-branch https://github.com/MetaCubeX/mihomo.git /src && \
    git switch --detach "$TAG" && \
    BUILDTIME=$(date -u '+%a %b %d %H:%M:%S UTC %Y') && \
    echo "Updating version.go with TAG=${TAG} and BUILDTIME=${BUILDTIME}" && \
    sed -i "s|Version\s*=.*|Version = \"${TAG}\"|" constant/version.go && \
    sed -i "s|BuildTime\s*=.*|BuildTime = \"${BUILDTIME}\"|" constant/version.go

RUN sed -i '/^import (/a\    "github.com/metacubex/mihomo/log"' \
    adapter/provider/provider.go && \
    sed -i 's@return nil, fmt.Errorf("proxy %d error: %w", idx, err)@name, _ := mapping["name"].(string)\n                log.Warnln("[Provider %s] skip invalid proxy (idx=%d, name=%q): %v", pdName, idx, name, err)\n                continue@g' \
    adapter/provider/provider.go

RUN curl -fsSL https://github.com/MetaCubeX/mihomo/pull/3326.patch | git apply && \
    grep -q 'BuildAddedX25519MLKEM768HandshakeState' component/tls/utls.go || \
    { echo "patch did not apply: PR #3326" >&2; exit 1; }

RUN sed -i \
    -e 's/hello\.SessionId\[0\] = 1/hello.SessionId[0] = 26/' \
    -e 's/hello\.SessionId\[1\] = 8/hello.SessionId[1] = 3/' \
    -e 's/hello\.SessionId\[2\] = 2/hello.SessionId[2] = 27/' \
    component/tls/reality.go && \
    grep -Fq 'hello.SessionId[0] = 26' component/tls/reality.go && \
    grep -Fq 'hello.SessionId[1] = 3' component/tls/reality.go && \
    grep -Fq 'hello.SessionId[2] = 27' component/tls/reality.go || \
    { echo "patch did not apply: reality minClientVer (component/tls/reality.go)" >&2; exit 1; }

# sing-tun: в iptables-ветке auto-redirect (RouterOS без nf_tables) цикл exclude-uid передаёт
# ranges.Range в форматтер -> panic "unknown value". Цепочка там висит на PREROUTING, где owner-match
# всё равно не работает; uid исключается через auto-route (ip rule uidrange). Цикл вырезаем.
RUN D="$(go mod download -json github.com/metacubex/sing-tun | jq -r '.Dir // empty')" && \
    [ -n "$D" ] && rm -rf /src/.sing-tun && cp -r "$D" /src/.sing-tun && chmod -R u+w /src/.sing-tun && \
    T="$(printf '\t')" && \
    sed -i "/for _, uid := range r.tunOptions.ExcludeUID {/,/^${T}}\$/d" /src/.sing-tun/redirect_iptables.go && \
    ! grep -q 'range r.tunOptions.ExcludeUID' /src/.sing-tun/redirect_iptables.go && \
    grep -q 'tableNamePreRouteing, "-m addrtype --dst-type LOCAL -j RETURN")' /src/.sing-tun/redirect_iptables.go && \
    go mod edit -replace github.com/metacubex/sing-tun=./.sing-tun || \
    { echo "patch did not apply: sing-tun exclude-uid (redirect_iptables.go)" >&2; exit 1; }
  
# Сборка mihomo для целевой архитектуры
RUN echo "Building for $TARGETARCH $TARGETVARIANT" && \
    BUILD_TAGS="" && [ "$WITH_GVISOR" = "1" ] && BUILD_TAGS="with_gvisor" && \
    echo "Build tags: $BUILD_TAGS" && \
    if [ "$TARGETARCH" = "arm" ]; then \
        export GOARCH=arm; \
        if [ "$TARGETVARIANT" = "v5" ]; then export GOARM=5; \
        elif [ "$TARGETVARIANT" = "v7" ]; then export GOARM=7; fi; \
    else \
        export GOARCH=$TARGETARCH; \
    fi && \
    export GOOS=$TARGETOS && CGO_ENABLED=0 go build -tags "$BUILD_TAGS" -trimpath -ldflags "-w -s -buildid=" -o /src/mihomo .

WORKDIR /tmp

#make work structure
RUN mkdir -p build/usr/bin build/etc && \
    mv /src/mihomo build/usr/bin/mihomo && \
    chmod -R +x .

# веб-панель mihomo-web (Go, только stdlib)
COPY web-panel /web-panel
RUN cd /web-panel && \
    CGO_ENABLED=0 go test ./... && \
    if [ "$TARGETARCH" = "arm" ]; then \
        export GOARCH=arm; \
        if [ "$TARGETVARIANT" = "v5" ]; then export GOARM=5; else export GOARM=7; fi; \
    else \
        export GOARCH=$TARGETARCH; \
    fi && \
    export GOOS=$TARGETOS && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-w -s -buildid= -X main.version=$(date -u +%Y%m%d.%H%M)" \
        -o /tmp/build/usr/bin/mihomo-web . && \
    chmod +x /tmp/build/usr/bin/mihomo-web && \
    printf '#!/bin/sh\n# консольное меню серверов: vpn (меню) | vpn status | vpn list | vpn use N ...\n[ $# -eq 0 ] && set -- menu\nexec /usr/bin/mihomo-web "$@"\n' > /tmp/build/usr/bin/vpn && \
    chmod +x /tmp/build/usr/bin/vpn

# naiveproxy (клиент). Для armv7 — static musl, для arm64 — static, для amd64 — musl (нужен libgcc)
RUN if [ "$WITH_NAIVE" != "1" ]; then echo "naive disabled"; exit 0; fi; \
    case "$TARGETARCH$TARGETVARIANT" in \
        amd64)  NAIVE_ASSET="openwrt-x86_64" ;; \
        arm64)  NAIVE_ASSET="openwrt-aarch64_generic-static" ;; \
        armv7)  NAIVE_ASSET="openwrt-arm_cortex-a7_neon-vfpv4-static" ;; \
        *)      echo "naive: no build for $TARGETARCH$TARGETVARIANT, skipping"; exit 0 ;; \
    esac; \
    TAG="$NAIVE_VERSION"; \
    if [ -z "$TAG" ]; then \
        TAG="$(curl -fsSI https://github.com/klzgrad/naiveproxy/releases/latest | tr -d '\r' | awk -F'/tag/' 'tolower($0) ~ /^location:/ {print $2}')"; \
    fi; \
    [ -n "$TAG" ] || { echo "naive: cannot resolve release tag" >&2; exit 1; }; \
    echo "naive $TAG ($NAIVE_ASSET)"; \
    curl -fsSL "https://github.com/klzgrad/naiveproxy/releases/download/${TAG}/naiveproxy-${TAG}-${NAIVE_ASSET}.tar.xz" -o /tmp/naive.tar.xz && \
    mkdir -p /tmp/naive && tar -xJf /tmp/naive.tar.xz -C /tmp/naive --strip-components=1 && \
    install -m 0755 /tmp/naive/naive /tmp/build/usr/bin/naive && \
    rm -rf /tmp/naive /tmp/naive.tar.xz

# UPX: под внутреннюю NAND 128 МБ (MikroTik L009 и т.п.) — mihomo ~56->13 МБ, naive ~10.5->3.8 МБ.
# naive для amd64 динамический (musl) — не трогаем.
RUN if [ "$WITH_UPX" = "1" ]; then \
        for b in /tmp/build/usr/bin/mihomo /tmp/build/usr/bin/mihomo-web; do \
            upx --lzma -q "$b" >/dev/null && upx -t -q "$b" || echo "upx: skip $b"; \
        done; \
        if [ -f /tmp/build/usr/bin/naive ] && [ "$TARGETARCH" != "amd64" ]; then \
            upx --lzma -q /tmp/build/usr/bin/naive >/dev/null && upx -t -q /tmp/build/usr/bin/naive || echo "upx: skip naive"; \
        fi; \
    fi; \
    ls -la /tmp/build/usr/bin

COPY entrypoint.sh build/
COPY mihomo-fs build/

# Базовые образы для каждой архитектуры
FROM --platform=linux/amd64 wiktorbgu/alpine-mikrotik:ip-nf-tables AS linux-amd64
FROM --platform=linux/arm/v7 wiktorbgu/alpine-mikrotik:ip-nf-tables AS linux-armv7
FROM --platform=linux/arm64 wiktorbgu/alpine-mikrotik:ip-nf-tables AS linux-arm64
FROM --platform=linux/arm/v5 debian:trixie-slim AS linux-armv5

# FINAL IMAGE
FROM ${TARGETOS}-${TARGETARCH}${TARGETVARIANT}
ARG TARGETPLATFORM \
    TARGETARCH \
    TARGETOS \
    TARGETVARIANT

COPY --from=build /tmp/build/ /

# DISABLE_NFTABLES нужна именно для михомо
ENV DISABLE_NFTABLES=1 \
    CONFIG="default_config.yaml" \
    WORKDIR="/etc/mihomo" \
    HEALTH_CHECK_ENABLE="true" \
    HEALTH_CHECK_URL="https://www.gstatic.com/generate_204" \
    HEALTH_CHECK_INTERVAL=300 \
    HEALTH_CHECK_TIMEOUT=5000 \
    HEALTH_CHECK_LAZY="true" \
    HEALTH_CHECK_EXPECTED_STATUS=204 \
    TOLERANCE=10 \
    MIXED_PORT=1080 \
    UI_PORT=9090 \
    EXTERNAL_CONTROLLER_ADDRESS="0.0.0.0" \
    TUN_STACK="mips" \
    TUN_INET4_ADDRESS="198.19.0.1/30" \
    TUN_INET6_ADDRESS="fdfe:dcba:9876::1/126" \
    TUN_AUTO_REDIRECT="true" \
    TUN_AUTO_DETECT_INTERFACE="true" \
    TUN_AUTO_ROUTE="true" \
    TUN_DISABLE_ICMP_FORWARDING="true" \
    EXTERNAL_UI_PATH="ui" \
    IPV6="true" \
    IP_VERSION="dual" \
    PROVIDER_INTERVAL=3600 \
    DNS_ENABLE="true" \
    DNS_USE_SYSTEM_HOSTS="true" \
    DNS_CACHE_ALGORITHM="arc" \
    DNS_PREFER_H3="false" \
    DNS_LISTEN="0.0.0.0:53" \
    DNS_ENHANCED_MODE="fake-ip" \
    DNS_FAKE_IP_RANGE="198.18.0.0/15" \
    DNS_FAKE_IP_TTL=1 \
    TCP_CONCURRENT="true" \
    KEEP_ALIVE_IDLE=60 \
    KEEP_ALIVE_INTERVAL=30 \
    FIND_PROCESS_MODE="off" \
    STORE_SELECTED="true" \
    UNIFIED_DELAY="true" \
    LOADBALANCE_STRATEGY="consistent-hashing" \
    WEB_ENABLE="true" \
    WEB_PORT=8080 \
    WEB_USER="admin" \
    NAIVE_UID=2080 \
    NAIVE_BASE_PORT=20800

RUN case "$TARGETPLATFORM" in \
        "linux/arm/v5") \
            apt update && \
            apt install -y bash iptables tzdata gettext-base iputils-ping traceroute procps ca-certificates kmod tini iproute2 && \
            apt autoremove -y && \
            apt clean -y && \
            rm -rf /var/cache/apt/archives /var/lib/apt/lists/* && \
            sed -i '1s|^#!/bin/sh|#!/bin/bash|' /entrypoint.sh && \
            # IPv4
            rm /usr/sbin/iptables /usr/sbin/iptables-save /usr/sbin/iptables-restore && \
            ln -s /usr/sbin/iptables-legacy /usr/sbin/iptables && \
            ln -s /usr/sbin/iptables-legacy-save /usr/sbin/iptables-save && \
            ln -s /usr/sbin/iptables-legacy-restore /usr/sbin/iptables-restore && \
            # IPv6
            rm /usr/sbin/ip6tables /usr/sbin/ip6tables-save /usr/sbin/ip6tables-restore && \
            ln -s /usr/sbin/ip6tables-legacy /usr/sbin/ip6tables && \
            ln -s /usr/sbin/ip6tables-legacy-save /usr/sbin/ip6tables-save && \
            ln -s /usr/sbin/ip6tables-legacy-restore /usr/sbin/ip6tables-restore;; \
        linux/arm/v7) \
            apk add --no-cache envsubst ca-certificates && \
            rm -vrf /var/cache/apk/* ;; \
        linux/amd64) \
            apk add --no-cache envsubst ca-certificates libgcc && \
            rm -vrf /var/cache/apk/* ;; \
        linux/arm64) \
            apk add --no-cache envsubst ca-certificates && \
            rm -vrf /var/cache/apk/* ;; \
        *) echo "Unsupported platform: $TARGETPLATFORM" && exit 1 ;; \
    esac && \
    chmod +x /entrypoint.sh

EXPOSE 8080 9090 1080

ENTRYPOINT ["tini", "--", "/entrypoint.sh"]