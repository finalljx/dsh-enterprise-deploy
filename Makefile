# 便捷入口。每一步都能单独跑：集群侧 target 声明了它依赖的上一步，
# 单独 make 任意一步会自动补齐前置（kubectl apply 幂等）。
SHELL := /bin/bash

.PHONY: all base certs control router istio reaper example-user render \
        image router-image control-image reaper-image user verify

all: istio

base:
	kubectl apply -f 01-namespaces.yaml
	kubectl apply -f 02-runtimeclass.yaml
	kubectl apply -f 03-storageclass.yaml
	kubectl apply -f 04-rbac.yaml
	kubectl apply -f 05-networkpolicy.yaml
	kubectl apply -f 06-admission-policy.yaml

certs: base
	kubectl apply -f 07-issuer.yaml
	kubectl apply -f 08-edge-cert.yaml
	kubectl apply -f 09-postgres.yaml

control: certs
	kubectl apply -f 10-control.yaml

router: control
	kubectl apply -f 11-router.yaml

istio: router
	kubectl apply -f 12-istio.yaml

reaper: control
	kubectl apply -f 13-reaper.yaml

# 只跑 up.sh 的渲染步骤（不碰集群），产物在 deploy/cloud/render/。
# 需要 deploy/cloud/env 里的 DSH_REGISTRY / DSH_DOMAIN。
render:
	RENDER_ONLY=1 ./deploy/cloud/up.sh

# 校验示例用户清单。必须用渲染后的副本：源文件里的 registry.corp / dsh-rbd
# 都是占位值，直接 apply 到云上会拉不到镜像。
example-user: render
	kubectl apply --dry-run=server -f deploy/cloud/render/14-example-user.yaml

# 注意 build 和 push 必须用同一个 tag 表达式。曾经 push 写死 DSH_VERSION，
# 于是设置了 IMAGE_TAG 时构建出 a-r2、却推了 a，集群按 a-r2 去拉必然 404。
image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	@test -n "$$DSH_VERSION" || { echo "DSH_VERSION 未设置，例如 0.2.1-alpha.4"; exit 1; }
	@hack/check-tag-unused.sh "$$DSH_REGISTRY/dsh:$${IMAGE_TAG:-$$DSH_VERSION}"
	docker build --platform linux/amd64 -f image/Dockerfile \
	  --build-arg NPM_REGISTRY=$${NPM_REGISTRY:-https://registry.npmmirror.com} \
	  --build-arg DSH_VERSION=$$DSH_VERSION \
	  -t $$DSH_REGISTRY/dsh:$${IMAGE_TAG:-$$DSH_VERSION} .
	docker push $$DSH_REGISTRY/dsh:$${IMAGE_TAG:-$$DSH_VERSION}

router-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker build --platform linux/amd64 -t $$DSH_REGISTRY/dsh-router:0.1.0 ./router
	docker push $$DSH_REGISTRY/dsh-router:0.1.0

control-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker  build --platform linux/amd64 -t $$DSH_REGISTRY/dsh-control:0.1.0 ./control
	docker push $$DSH_REGISTRY/dsh-control:0.1.0

reaper-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker build --platform linux/amd64 -t $$DSH_REGISTRY/dsh-reaper:0.1.0 ./reaper
	docker push $$DSH_REGISTRY/dsh-reaper:0.1.0

# host 由控制面按 slug + DSH_BASE_DOMAIN 生成，不能从命令行指定
user:
	@test "$(origin USER)" = "command line" || { echo "用法: make user USER=alice@corp [TIER=T2]"; exit 1; }
	./provision.py "$(USER)" --tier "$(or $(TIER),T1)"

verify:
	./verify.sh
