# 用户 pod 镜像
#
# 构建（上下文必须是仓库根目录，因为要打包 plugin/）：
#   docker build -f image/Dockerfile -t registry.corp/dsh:0.2.1-alpha.4 .
# 国内网络默认走 npmmirror，可用 --build-arg NPM_REGISTRY=... 覆盖。
FROM node:22-bookworm-slim

ARG NPM_REGISTRY=https://registry.npmmirror.com
# DSH 版本：与 deploy/cloud/env 的 DSH_VERSION 保持一致（构建脚本会自动传进来）
ARG DSH_VERSION=0.2.1-alpha.4

RUN apt-get update \
 && apt-get install -y --no-install-recommends jq curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# 锁死完整版本，禁止 range。
# pnpm 是 `dsh plugin add` 的安装器，node 镜像里没有，缺了插件会静默装不上。
RUN npm i -g --registry="${NPM_REGISTRY}" \
      @deepseek-ai/dsh@${DSH_VERSION} \
      pnpm@10 \
 && npm cache clean --force

# node:22 里已有 uid 1000 的 node 用户，useradd -u 1000 会报 UID 冲突；
# manifest 要求 runAsUser: 1000，所以复用该用户、改名并把家目录挪到 /home/dsh
RUN usermod -l dsh -d /home/dsh -m -s /bin/bash node

ENV HOME=/home/dsh \
    NODE_ENV=production \
    DSH_HOME=/data/dsh

COPY image/init-home.sh image/entrypoint.sh /opt/dsh/
COPY image/profile/cordis.patch.yml /opt/dsh/profile/cordis.patch.yml
RUN chmod 0755 /opt/dsh/*.sh && chmod 0644 /opt/dsh/profile/cordis.patch.yml

# 活动上报插件：从仓库本地目录安装，运行期不依赖 registry。
# 装不上就让构建失败——缺这个插件 DSH 会拒绝启动（1 required plugin did not activate）。
COPY plugin /opt/plugin
RUN mkdir -p /opt/dsh/profile-seed \
 && DSH_HOME=/opt/dsh/profile-seed dsh plugin --profile web add /opt/plugin \
 && test -d /opt/dsh/profile-seed/profiles/web/node_modules/@corp/dsh-plugin-activity

USER 1000
EXPOSE 8443
ENTRYPOINT ["/opt/dsh/entrypoint.sh"]
