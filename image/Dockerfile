# 用户 pod 镜像
#
# 构建：
#   docker build -t registry.corp/dsh:0.2.1-alpha.4 ./image
#   docker push registry.corp/dsh:0.2.1-alpha.4
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends jq curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# 锁死完整版本，禁止 range
RUN npm i -g @deepseek-ai/dsh@0.2.1-alpha.4 \
 && npm cache clean --force

RUN useradd -u 1000 -m -s /bin/bash dsh

ENV HOME=/home/dsh \
    NODE_ENV=production \
    DSH_HOME=/data/dsh

COPY init-home.sh entrypoint.sh /opt/dsh/
COPY profile/cordis.patch.yml /opt/dsh/profile/cordis.patch.yml
RUN chmod 0755 /opt/dsh/*.sh && chmod 0644 /opt/dsh/profile/cordis.patch.yml

# 预置 web profile 与活动上报插件。构建期装好，运行期不依赖 registry。
# 插件发布到私有 registry 后这里才有意义；未发布时用下面的 fallback 注释掉本段。
ARG ACTIVITY_PLUGIN=@corp/dsh-plugin-activity@1.0.0
RUN mkdir -p /opt/dsh/profile-seed \
 && DSH_HOME=/opt/dsh/profile-seed dsh plugin --profile web add "${ACTIVITY_PLUGIN}"

USER 1000
EXPOSE 8443
ENTRYPOINT ["/opt/dsh/entrypoint.sh"]
