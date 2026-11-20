#!/usr/bin/env bash
# 推送前的「tag 是否已被占用」检查。
#
# 为什么需要：用户 Deployment 没有写 imagePullPolicy，而 tag 又不是 latest，
# 所以节点默认用 IfNotPresent。同名 tag 覆盖推送后，节点认为本地已有该 tag，
# 不会重新拉取 —— 于是「构建成功、推送成功、部署成功」，pod 里还是旧镜像，
# 而且任何一层都不报错。只能在升级后对比 digest 才会发现。
#
# 用法：
#   hack/check-tag-unused.sh <image-ref>
#   ALLOW_TAG_OVERWRITE=1 hack/check-tag-unused.sh <image-ref>   # 明确要覆盖
#
# 判定策略（不能查就放行，避免把构建流程卡死在网络/登录问题上）：
#   查到 tag       → 报错退出，提示换 IMAGE_TAG
#   明确不存在     → 放行
#   查不了         → 告警后放行（未登录 / 网络不通 / 仓库不支持 manifest）
#
# 退出码：0 放行，1 已存在，2 用法错误。
set -uo pipefail

ref="${1:-}"
if [ -z "$ref" ]; then
  echo "用法: $0 <image-ref>" >&2
  exit 2
fi

if [ "${ALLOW_TAG_OVERWRITE:-0}" = "1" ]; then
  echo "  ALLOW_TAG_OVERWRITE=1，跳过 tag 占用检查（${ref}）"
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "  WARN: 没有 docker，跳过 tag 占用检查（${ref}）" >&2
  exit 0
fi

out=$(docker manifest inspect "$ref" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
  cat >&2 <<EOF
错误: ${ref} 已经存在于仓库，不能再用同名 tag 推送。

  用户 Deployment 默认 imagePullPolicy=IfNotPresent：同名 tag 覆盖后节点
  不会重新拉取，结果是「推送成功、部署成功」但 pod 还在跑旧镜像，且不报错。

  正确做法：换一个新 tag 再推
    IMAGE_TAG=<新值，如 0.2.1-alpha.4-r2> ./deploy/cloud/push-images.sh
    然后重跑 ./deploy/cloud/up.sh（会顺带对齐存量用户）

  确认要覆盖已有 tag（例如刚才那次推送失败、重推）：
    ALLOW_TAG_OVERWRITE=1 ./deploy/cloud/push-images.sh
EOF
  exit 1
fi

case "$out" in
  *"no such manifest"*|*"manifest unknown"*|*"unknown manifest"*|*"not found"*|*"Name unknown"*)
    exit 0
    ;;
  *)
    echo "  WARN: 无法确认 ${ref} 是否已存在，跳过检查" >&2
    echo "        （${out}）" >&2
    exit 0
    ;;
esac
