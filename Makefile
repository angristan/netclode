CONTEXT ?= netclode
NAMESPACE ?= netclode
TAG ?= dev-$(shell date +%Y%m%d-%H%M%S)
DEV_CONTROL_PLANE_IMAGE ?= netclode-control-plane:$(TAG)
DEV_AGENT_IMAGE ?= netclode-agent:$(TAG)
ANSIBLE_EXTRA_ARGS ?=
ANSIBLE_USER ?=

TEAM_ID_AUTO ?= $(shell \
	team=$$(security find-certificate -a -c "Apple Development" -p "$$HOME/Library/Keychains/login.keychain-db" 2>/dev/null | \
		openssl x509 -noout -subject 2>/dev/null | sed -n 's/.*OU=\([^,]*\).*/\1/p' | head -n 1); \
	if [ -z "$$team" ]; then \
		profile=$$(ls -1 "$$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"/*.mobileprovision 2>/dev/null | head -n 1); \
		if [ -n "$$profile" ]; then \
			team=$$(security cms -D -i "$$profile" 2>/dev/null | \
				awk 'found && /<string>/{gsub(/.*<string>|<\/string>.*/, ""); print; exit} /<key>TeamIdentifier<\/key>/{found=1}'); \
		fi; \
	fi; \
	printf "%s" "$$team" \
)
TEAM_ID ?= $(TEAM_ID_AUTO)

XCODE_SIGN_ARGS := -allowProvisioningUpdates
ifneq ($(strip $(TEAM_ID)),)
XCODE_SIGN_ARGS += DEVELOPMENT_TEAM=$(TEAM_ID)
endif

.PHONY: rollout rollout-control-plane rollout-agent drain-warmpool deploy test-ios run-macos run-ios run-device print-ios-team-id proto proto-lint proto-breaking proto-setup dev-install-builder dev-build-remote dev-build-remote-control-plane dev-build-remote-agent dev-deploy-images dev-verify dev-loop-remote dev-loop-ansible dev-loop-ansible-build dev-loop-ansible-deploy dev-loop-ansible-verify

# Proto generation
proto: proto-setup ## Generate code from proto files
	@mkdir -p services/control-plane/gen
	@mkdir -p services/agent/gen
	@mkdir -p clients/ios/Netclode/Generated
	cd proto && buf generate

proto-lint: proto-setup ## Lint proto files
	cd proto && buf lint

proto-breaking: proto-setup ## Check for breaking changes against main
	cd proto && buf breaking --against '.git#branch=main'

proto-setup: ## Install buf if not present
	@which buf > /dev/null || (echo "Installing buf..." && brew install bufbuild/buf/buf)

rollout: ## Rollout a deployment: make rollout target=control-plane
ifndef target
	$(error target is required. Usage: make rollout target=control-plane)
endif
	kubectl --context $(CONTEXT) -n $(NAMESPACE) rollout restart deployment/$(target)

rollout-control-plane: ## Rollout control-plane
	kubectl --context $(CONTEXT) -n $(NAMESPACE) rollout restart deployment/control-plane

rollout-agent: ## Rollout agent (drains warm pool to pick up new image)
	@echo "Scaling warm pool to 0..."
	kubectl --context $(CONTEXT) -n $(NAMESPACE) patch sandboxwarmpool netclode-agent-pool -p '{"spec":{"replicas":0}}' --type=merge
	@echo "Waiting for warm pods to terminate..."
	@sleep 5
	@echo "Scaling warm pool back to 1..."
	kubectl --context $(CONTEXT) -n $(NAMESPACE) patch sandboxwarmpool netclode-agent-pool -p '{"spec":{"replicas":1}}' --type=merge
	@echo "Warm pool refreshed with new agent image"

drain-warmpool: ## Drain warm pool to pick up new agent image
	@echo "Scaling warm pool to 0..."
	kubectl --context $(CONTEXT) -n $(NAMESPACE) patch sandboxwarmpool netclode-agent-pool -p '{"spec":{"replicas":0}}' --type=merge
	@echo "Waiting for warm pods to terminate..."
	@sleep 5
	@echo "Scaling warm pool back to 1..."
	kubectl --context $(CONTEXT) -n $(NAMESPACE) patch sandboxwarmpool netclode-agent-pool -p '{"spec":{"replicas":1}}' --type=merge
	@echo "Warm pool refreshed"

deploy: ## Wait for CI then rollout control-plane
	gh run watch $$(gh run list --limit 1 --json databaseId --jq '.[0].databaseId') --exit-status
	$(MAKE) rollout-control-plane

dev-build-remote: ## Build control-plane + agent images on DEPLOY_HOST and import into k3s containerd
	@TAG=$(TAG) CONTROL_PLANE_IMAGE=$(DEV_CONTROL_PLANE_IMAGE) AGENT_IMAGE=$(DEV_AGENT_IMAGE) scripts/dev/build-on-remote.sh

dev-build-remote-control-plane: ## Build only control-plane image on DEPLOY_HOST and import into k3s containerd
	@TAG=$(TAG) BUILD_AGENT=0 CONTROL_PLANE_IMAGE=$(DEV_CONTROL_PLANE_IMAGE) scripts/dev/build-on-remote.sh

dev-build-remote-agent: ## Build only agent image on DEPLOY_HOST and import into k3s containerd
	@TAG=$(TAG) BUILD_CONTROL_PLANE=0 AGENT_IMAGE=$(DEV_AGENT_IMAGE) scripts/dev/build-on-remote.sh

dev-deploy-images: ## Fast dev deploy via kubectl (no Ansible): update images + AGENT_IMAGE env + refresh warm pool
	@CONTROL_PLANE_IMAGE=$(DEV_CONTROL_PLANE_IMAGE) AGENT_IMAGE=$(DEV_AGENT_IMAGE) CONTEXT=$(CONTEXT) NAMESPACE=$(NAMESPACE) scripts/dev/deploy-dev-images.sh

dev-verify: ## Verify control-plane + sandbox template image wiring and logs
	@CONTEXT=$(CONTEXT) NAMESPACE=$(NAMESPACE) scripts/dev/verify-dev-loop.sh

dev-loop-remote: ## Build on DEPLOY_HOST, deploy with kubectl patch, then verify (TAG=dev-...)
	@$(MAKE) dev-build-remote TAG=$(TAG)
	@$(MAKE) dev-deploy-images TAG=$(TAG)
	@$(MAKE) dev-verify

dev-install-builder: ## Install Docker + Buildx on DEPLOY_HOST for remote dev builds
	@set -a; [ -f .env ] && . ./.env || true; set +a; \
	cd infra/ansible && ansible-playbook playbooks/dev-builder.yaml \
		$(if $(strip $(ANSIBLE_USER)),-e ansible_user=$(ANSIBLE_USER),) \
		$(ANSIBLE_EXTRA_ARGS)

dev-loop-ansible: ## Fast dev loop via Ansible (build + deploy + verify)
	@set -a; [ -f .env ] && . ./.env || true; set +a; \
	cd infra/ansible && ansible-playbook playbooks/dev-loop.yaml \
		$(if $(strip $(ANSIBLE_USER)),-e ansible_user=$(ANSIBLE_USER),) \
		-e k8s_namespace=$(NAMESPACE) \
		-e tag=$(TAG) \
		-e control_plane_image=$(DEV_CONTROL_PLANE_IMAGE) \
		-e agent_image=$(DEV_AGENT_IMAGE) \
		$(ANSIBLE_EXTRA_ARGS)

dev-loop-ansible-build: ## Ansible dev loop: build/import only
	@set -a; [ -f .env ] && . ./.env || true; set +a; \
	cd infra/ansible && ansible-playbook playbooks/dev-loop.yaml --tags dev-build \
		$(if $(strip $(ANSIBLE_USER)),-e ansible_user=$(ANSIBLE_USER),) \
		-e k8s_namespace=$(NAMESPACE) \
		-e tag=$(TAG) \
		-e control_plane_image=$(DEV_CONTROL_PLANE_IMAGE) \
		-e agent_image=$(DEV_AGENT_IMAGE) \
		$(ANSIBLE_EXTRA_ARGS)

dev-loop-ansible-deploy: ## Ansible dev loop: deploy/rollout only
	@set -a; [ -f .env ] && . ./.env || true; set +a; \
	cd infra/ansible && ansible-playbook playbooks/dev-loop.yaml --tags dev-deploy \
		$(if $(strip $(ANSIBLE_USER)),-e ansible_user=$(ANSIBLE_USER),) \
		-e k8s_namespace=$(NAMESPACE) \
		-e control_plane_image=$(DEV_CONTROL_PLANE_IMAGE) \
		-e agent_image=$(DEV_AGENT_IMAGE) \
		$(ANSIBLE_EXTRA_ARGS)

dev-loop-ansible-verify: ## Ansible dev loop: verification only
	@set -a; [ -f .env ] && . ./.env || true; set +a; \
	cd infra/ansible && ansible-playbook playbooks/dev-loop.yaml --tags dev-verify \
		$(if $(strip $(ANSIBLE_USER)),-e ansible_user=$(ANSIBLE_USER),) \
		-e k8s_namespace=$(NAMESPACE) \
		$(ANSIBLE_EXTRA_ARGS)

test-ios: ## Run iOS unit tests
	cd clients/ios && xcodebuild test -scheme NetclodeTests -destination 'platform=macOS' -quiet

run-macos: ## Build and run macOS (Catalyst) app
	cd clients/ios && xcodebuild -scheme Netclode -destination 'platform=macOS,variant=Mac Catalyst' -derivedDataPath .build $(XCODE_SIGN_ARGS) build
	open clients/ios/.build/Build/Products/Debug-maccatalyst/Netclode.app

SIMULATOR ?= iPhone 16 Pro
run-ios: ## Build and run iOS simulator app (SIMULATOR="iPhone 16 Pro")
	xcrun simctl boot "$(SIMULATOR)" 2>/dev/null || true
	cd clients/ios && xcodebuild -scheme Netclode -destination 'platform=iOS Simulator,name=$(SIMULATOR)' -derivedDataPath .build $(XCODE_SIGN_ARGS) build
	xcrun simctl install "$(SIMULATOR)" clients/ios/.build/Build/Products/Debug-iphonesimulator/Netclode.app
	xcrun simctl launch "$(SIMULATOR)" com.netclode.ios

run-device: ## Build and run on connected iPhone
	cd clients/ios && xcodebuild -scheme Netclode -destination 'generic/platform=iOS' -derivedDataPath .build $(XCODE_SIGN_ARGS) -allowProvisioningDeviceRegistration build
	xcrun devicectl device install app --device "$(shell xcrun devicectl list devices 2>/dev/null | grep iPhone | grep -oE '[0-9A-F-]{36}' | head -1)" clients/ios/.build/Build/Products/Debug-iphoneos/Netclode.app
	xcrun devicectl device process launch --device "$(shell xcrun devicectl list devices 2>/dev/null | grep iPhone | grep -oE '[0-9A-F-]{36}' | head -1)" com.netclode.ios

print-ios-team-id: ## Print detected iOS signing Team ID (override with TEAM_ID=...)
	@echo $(TEAM_ID)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
