# 便捷入口。每一步都能单独跑：集群侧 target 声明了它依赖的上一步，
# 单独 make 任意一步会自动补齐前置（kubectl apply 幂等）。
SHELL := /bin/bash

.PHONY: all base certs control router istio reaper example-user \
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

example-user:
	kubectl apply --dry-run=server -f 14-example-user.yaml

image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	@test -n "$$DSH_VERSION" || { echo "DSH_VERSION 未设置，例如 0.2.1-alpha.4"; exit 1; }
	docker build -t $$DSH_REGISTRY/dsh:$$DSH_VERSION ./image
	docker push $$DSH_REGISTRY/dsh:$$DSH_VERSION

router-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker build -t $$DSH_REGISTRY/dsh-router:0.1.0 ./router
	docker push $$DSH_REGISTRY/dsh-router:0.1.0

control-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker build -t $$DSH_REGISTRY/dsh-control:0.1.0 ./control
	docker push $$DSH_REGISTRY/dsh-control:0.1.0

reaper-image:
	@test -n "$$DSH_REGISTRY" || { echo "DSH_REGISTRY 未设置，例如 registry.corp"; exit 1; }
	docker build -t $$DSH_REGISTRY/dsh-reaper:0.1.0 ./reaper
	docker push $$DSH_REGISTRY/dsh-reaper:0.1.0

user:
	@test "$(origin USER)" = "command line" || { echo "用法: make user USER=alice@corp HOST=alice.dsh.corp [TIER=T2]"; exit 1; }
	@test -n "$(HOST)" || { echo "HOST 未设置，例如 HOST=alice.dsh.corp"; exit 1; }
	./provision.py "$(USER)" --host "$(HOST)" --tier "$(or $(TIER),T1)"

verify:
	./verify.sh
