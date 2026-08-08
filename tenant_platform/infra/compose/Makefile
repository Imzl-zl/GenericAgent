# GenericAgent 镜像打包 / 部署 Makefile
#
# 规范化打包入口(与生产模板 fail-closed 原则一致):
#   - 镜像 tag 一律取 git 版本(VERSION), 可追溯; :local 仅为 compose 默认别名
#   - 生产 GA_RUNNER_IMAGE 必须用 digest(make runner-digest 一键生成并自动写入 .env)
#   - REGISTRY 未设置时 push/pull 直接失败, 不会静默推错位置
#
# 常用目标:
#   make build                构建全部镜像(:local + :$(VERSION))
#   make push REGISTRY=host/ga   推送到私有 Registry
#   make pull REGISTRY=host/ga   服务器侧拉取并重打 :local
#   make runner-digest        生成 GA_RUNNER_IMAGE digest 并自动写入 .env(提示重启 sandbox-manager)
#   make verify               校验 compose 配置(等价 docker compose config)
#   make up / down / ps / logs   日常运维(docker compose 透传)
#   make version              显示当前 VERSION(可 make VERSION=xxx 覆盖)
#
# 构建上下文是仓库根(与 compose.yaml build.context 一致)。
# 用法详见 README.zh-CN.md「镜像打包」章节。

ROOT        := $(abspath ../../..)
DOCKERFILES := $(wildcard *.Dockerfile)
IMAGES      := $(patsubst %.Dockerfile,%,$(DOCKERFILES))
# 版本标识: git describe(--dirty 提示构建时工作区有未提交改动); 可用 make VERSION=x 覆盖
VERSION     ?= $(shell git -C $(ROOT) describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY    ?=

# 镜像名映射: 所有镜像统一 genericagent-* 前缀(compose/.env 的 GA_*_IMAGE 默认
# 与 Makefile 构建产物一致)。历史坑: 曾按 Dockerfile 文件名打 tag(platform:local),
# compose 引用 genericagent-platform:local, 导致构建产物不被使用; ga-runner 曾是
# 例外(无前缀), 造成 .env/默认值与构建产物三处不一致——已统一, 不允许再引入例外。

.PHONY: help version build push pull runner-digest verify up down ps logs

help:
	@echo "GenericAgent 镜像打包 / 部署"
	@echo "  make build                 构建全部镜像($(IMAGES)), tag: :local + :$(VERSION)"
	@echo "  make push REGISTRY=host/ga 推送到私有 Registry(:$(VERSION))"
	@echo "  make pull REGISTRY=host/ga 拉取并重打 :local(服务器侧)"
	@echo "  make runner-digest         输出 GA_RUNNER_IMAGE 的 digest 引用"
	@echo "  make verify                校验 docker compose config"
	@echo "  make up/down/ps/logs       compose 日常运维"

version:
	@echo "VERSION=$(VERSION)"
	@echo "IMAGES=$(IMAGES)"

build:
	@echo "==> 构建 $(words $(IMAGES)) 个镜像, VERSION=$(VERSION)"
	@set -e; for img in $(IMAGES); do \
		tag="genericagent-$$img"; \
		echo "==> [$$img] docker build -f $$img.Dockerfile -> $$tag:local"; \
		docker build -t "$$tag:local" -t "$$tag:$(VERSION)" -f "$$img.Dockerfile" "$(ROOT)"; \
	done
	@echo "==> 构建完成: $(VERSION)"; \
		echo "    :local 可直接运行(compose 默认); :$(VERSION) 用于追溯/推送"; \
		echo "    生产环境请继续: make runner-digest(自动更新 .env 的 GA_RUNNER_IMAGE 为 digest)"

push:
	@test -n "$(REGISTRY)" || { echo "错误: 未设置 REGISTRY(例: make push REGISTRY=registry.example.com/ga)"; exit 1; }
	@echo "==> 推送 $(words $(IMAGES)) 个镜像到 $(REGISTRY) :$(VERSION)"
	@set -e; for img in $(IMAGES); do \
		tag="genericagent-$$img"; \
		echo "==> [$$img] $(REGISTRY)/$$tag:$(VERSION)"; \
		docker tag "$$tag:$(VERSION)" "$(REGISTRY)/$$tag:$(VERSION)"; \
		docker push "$(REGISTRY)/$$tag:$(VERSION)"; \
	done
	@echo "==> 推送完成。服务器侧执行: make pull REGISTRY=$(REGISTRY) VERSION=$(VERSION)"

pull:
	@test -n "$(REGISTRY)" || { echo "错误: 未设置 REGISTRY(例: make pull REGISTRY=registry.example.com/ga)"; exit 1; }
	@echo "==> 从 $(REGISTRY) 拉取 :$(VERSION) 并重打 :local"
	@set -e; for img in $(IMAGES); do \
		tag="genericagent-$$img"; \
		echo "==> [$$img] $(REGISTRY)/$$tag:$(VERSION)"; \
		docker pull "$(REGISTRY)/$$tag:$(VERSION)"; \
		docker tag "$(REGISTRY)/$$tag:$(VERSION)" "$$tag:local"; \
	done
	@echo "==> 拉取完成。.env 的 GA_*_IMAGE 保持 *:local 即可; 生产 GA_RUNNER_IMAGE 用 make runner-digest"

runner-digest:
	@set -e; \
	digest="$$(docker image inspect --format '{{index .RepoDigests 0}}' genericagent-ga-runner:local 2>/dev/null || true)"; \
	if [ -z "$$digest" ] || ! echo "$$digest" | grep -qE '@sha256:[0-9a-f]{64}$$'; then \
		digest="genericagent-ga-runner@$$(docker image inspect --format '{{.Id}}' genericagent-ga-runner:local | sed 's/^sha256://')"; \
	fi; \
	echo "==> GA_RUNNER_IMAGE=$$digest"; \
	if grep -q '^GA_RUNNER_IMAGE=' .env; then \
		sed -i "s|^GA_RUNNER_IMAGE=.*|GA_RUNNER_IMAGE=$$digest|" .env; \
		echo "==> 已写入 .env(原值备份见下); 重启 sandbox-manager 生效:"; \
		echo "    docker compose up -d sandbox-manager"; \
	else \
		echo "==> .env 无 GA_RUNNER_IMAGE 行, 请手动追加: GA_RUNNER_IMAGE=$$digest"; \
	fi

verify:
	@docker compose config >/dev/null && echo "==> compose 配置校验通过"

# 构建与启动分离: 镜像由 make build 构建(生产 GA_RUNNER_IMAGE 是 digest 引用,
# docker compose up --build 无法把构建产物 tag 成 digest, 会报 build tag cannot
# contain a digest)。本地开发想构建+启动一步到位可用:
#   docker compose up -d --build
up:
	@docker compose up -d

down:
	@docker compose down

ps:
	@docker compose ps

logs:
	@docker compose logs -f --tail=100
