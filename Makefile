REGISTRY     ?= local
TAG          ?= dev
KIND_CLUSTER ?= ai-pr-review-cloud
NAMESPACE    ?= ai-pr-review-cloud
GO_CMDS      := gateway controller fetch
IMAGES       := $(addprefix $(REGISTRY)/ai-pr-review-cloud-,$(GO_CMDS) runner)
CHART        := deploy/helm/ai-pr-review-cloud
DEPLOYMENTS  := deployment/ai-pr-review-cloud-gateway deployment/ai-pr-review-cloud-controller

.PHONY: test lint images $(addprefix image-,$(GO_CMDS)) image-runner kind-up kind-load deploy webhook-forward

test:
	cd go && go test -count=1 ./...
	cd runner && npm test
	node --test demo/console.test.mjs

lint:
	cd go && test -z "$$(gofmt -l .)" && go vet ./...
	cd runner && npm run -s typecheck
	helm lint $(CHART) --set github.webhookSecret=lint
	helm lint $(CHART) --set github.webhookSecret=lint --set preview.enabled=true --set 'preview.apiServerCIDRs[0]=192.0.2.1/32' --set 'preview.imageRepositories.test/repo=registry.example/test'
	helm lint demo/preview/chart

images: $(addprefix image-,$(GO_CMDS)) image-runner

$(addprefix image-,$(GO_CMDS)): image-%:
	docker build -f go/Dockerfile --build-arg CMD=$* $(if $(filter fetch,$*),--target fetch) -t $(REGISTRY)/ai-pr-review-cloud-$*:$(TAG) go

image-runner:
	docker build -t $(REGISTRY)/ai-pr-review-cloud-runner:$(TAG) runner

kind-up:
	hack/kind-up.sh $(KIND_CLUSTER)

kind-load: images
	kind load docker-image --name $(KIND_CLUSTER) $(addsuffix :$(TAG),$(IMAGES))

# Secrets come from the environment only. GITHUB_WEBHOOK_SECRET defaults to a
# throwaway value, and without GITHUB_APP_ID the controller leaves runs queued,
# so the stack starts without a GitHub App. GITHUB_APP_PRIVATE_KEY_FILE is the
# App's .pem; LLM_API_KEY is the real model key, which only the LiteLLM proxy
# sees (runners get per-run virtual keys). The first deploy pulls LiteLLM's
# image (about 1.2 GB), hence the longer timeout.
deploy: kind-load
	helm upgrade --install ai-pr-review-cloud $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.registry=$(REGISTRY) --set image.tag=$(TAG) \
		--set github.webhookSecret=$${GITHUB_WEBHOOK_SECRET:-local-dev-secret} \
		--set-string github.appId=$${GITHUB_APP_ID:-} \
		$(if $(GITHUB_APP_PRIVATE_KEY_FILE),--set-file github.privateKey=$(GITHUB_APP_PRIVATE_KEY_FILE)) \
		--set llm.apiKey=$${LLM_API_KEY:-} \
		--wait --timeout 10m
	@# Images keep the same tag across builds; restart so freshly loaded ones run.
	@# The dev PostgreSQL is left alone: restarting it would drop its data.
	kubectl -n $(NAMESPACE) rollout restart $(DEPLOYMENTS)
	for d in $(DEPLOYMENTS); do kubectl -n $(NAMESPACE) rollout status $$d || exit 1; done

# Forwards GitHub App webhooks from a smee.io channel to the gateway in Kind.
webhook-forward:
	hack/webhook-forward.sh $(SMEE_URL)
