IMAGE ?= ghcr.io/example/asg-ami-rotator:latest

.PHONY: tidy build vet test run docker deploy

tidy:
	go mod tidy

# Build binary
build:
	CGO_ENABLED=0 go build -o bin/controller ./cmd/controller

vet:
	go vet ./...

# Comment for now no tests
# test:
# 	go test ./...

# Locally run binary
run:
	go run ./cmd/controller --asg-names=$(ASG_NAMES) --region=$(AWS_REGION) --leader-elect=false

docker:
	docker build -t $(IMAGE) .

deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/deployment.yaml
