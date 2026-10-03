all: lint test build

build:
	go build -o bin/ ./cmd/...

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	golangci-lint run ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

docker:
	docker build -t floodgate:dev .

demo: docker
	docker compose up -d --wait
	./scripts/demo-shared-limits.sh

infra-check:
	cd deploy/terraform/aws && terraform fmt -check && terraform init -backend=false -input=false && terraform validate
	kubectl kustomize deploy/k8s/base | kubeconform -strict -summary -
	kubectl kustomize deploy/k8s/overlays/local | kubeconform -strict -summary -
	helm lint deploy/helm/floodgate

.PHONY: all build test lint bench docker demo infra-check
