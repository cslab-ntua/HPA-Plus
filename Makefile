REGISTRY = dimitrisde
NAME = hpa-plus-operator
VERSION = 0.14.0-hpaplus
GOBIN = $(shell go env GOBIN)
GOPATH = $(shell go env GOPATH)
GO_BIN_DIR = $(if $(GOBIN),$(GOBIN),$(GOPATH)/bin)
CONTROLLER_GEN = $(GO_BIN_DIR)/controller-gen
CONTROLLER_GEN_VERSION = v0.16.4
UI_PORT ?= 8080
UI_HOST ?= hpaplus.localtest.me
UI_NAMESPACE ?=

.PHONY: build worker collector py_dependencies test gotest pytest docs docker push generate view_coverage get_controller-gen ui

build: worker collector
	go build -trimpath -o bin/manager .

worker:
	go build -trimpath -o bin/worker-server ./cmd/worker-server

collector:
	go build -trimpath -o bin/cgroup-collector ./cmd/cgroup-collector

py_dependencies:
	python -m pip install --require-hashes -r algorithms/requirements.txt
	python -m pip install --require-hashes -r docs/requirements.txt
	python -m pip install -r requirements-dev.txt

test: gotest pytest

gotest:
	export GOCOVERDIR='.' && go test ./... -cover -coverprofile unit_cover.out

pytest:
	pytest --import-mode=importlib algorithms/ --cov-report term --cov-report=xml:algorithm_coverage.out --cov-report=html:.algorithm_coverage --cov=algorithms/

docs:
	mkdocs build --strict

docker:
	docker build . -t $(REGISTRY)/$(NAME):$(VERSION)

push:
	docker push $(REGISTRY)/$(NAME):$(VERSION)

generate: get_controller-gen
	@echo "=============Generating Golang and YAML============="
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	$(CONTROLLER_GEN) rbac:roleName=hpa-plus-operator crd:allowDangerousTypes=true \
		paths="./api/...;./internal/...;./cmd/worker-server;." \
		output:crd:artifacts:config=helm/templates/crd \
		output:rbac:artifacts:config=helm/templates/cluster

view_coverage:
	@echo "=============Loading coverage HTML============="
	go tool cover -html=unit_cover.out
	python -m webbrowser file://$(shell pwd)/.algorithm_coverage/index.html

get_controller-gen:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

ui:
	go run ./cmd/hpaplus-ui --listen 127.0.0.1:$(UI_PORT) --host $(UI_HOST) --namespace "$(UI_NAMESPACE)"
