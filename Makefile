REGISTRY     ?= local
TAG          ?= dev
KIND_CLUSTER ?= ai-pr-review-cloud
NAMESPACE    ?= ai-pr-review-cloud
GO_CMDS      := gateway controller fetch
IMAGES       := $(addprefix $(REGISTRY)/ai-pr-review-cloud-,$(GO_CMDS) runner)
CHART        := deploy/helm/ai-pr-review-cloud

.PHONY: test lint images $(addprefix image-,$(GO_CMDS)) image-runner kind-up kind-load deploy

test:
	cd go && go test ./...
	cd runner && npm test

lint:
	cd go && test -z "$$(gofmt -l .)" && go vet ./...
	cd runner && npm run -s typecheck
	helm lint $(CHART) --set github.webhookSecret=lint

images: $(addprefix image-,$(GO_CMDS)) image-runner

$(addprefix image-,$(GO_CMDS)): image-%:
	docker build -f go/Dockerfile --build-arg CMD=$* -t $(REGISTRY)/ai-pr-review-cloud-$*:$(TAG) go

image-runner:
	docker build -t $(REGISTRY)/ai-pr-review-cloud-runner:$(TAG) runner

kind-up:
	hack/kind-up.sh $(KIND_CLUSTER)

kind-load: images
	kind load docker-image --name $(KIND_CLUSTER) $(addsuffix :$(TAG),$(IMAGES))

# GITHUB_WEBHOOK_SECRET defaults to a throwaway value so the stack starts without a GitHub App.
deploy: kind-load
	helm upgrade --install ai-pr-review-cloud $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.registry=$(REGISTRY) --set image.tag=$(TAG) \
		--set github.webhookSecret=$${GITHUB_WEBHOOK_SECRET:-local-dev-secret} \
		--wait
