REGISTRY = dimitrisde
NAME = hpa-plus-operator
VERSION = 0.13.3
GOBIN = $(shell go env GOBIN)
GOPATH = $(shell go env GOPATH)
GO_BIN_DIR = $(if $(GOBIN),$(GOBIN),$(GOPATH)/bin)
CONTROLLER_GEN = $(GO_BIN_DIR)/controller-gen

py_dependencies:
	python -m pip install -r requirements-dev.txt

test: gotest pytest

gotest:
	export GOCOVERDIR='.' && go test ./... -cover -coverprofile unit_cover.out

pytest:
	pytest algorithms/ --cov-report term --cov-report=xml:algorithm_coverage.out --cov-report=html:.algorithm_coverage --cov=algorithms/

docker:
	docker build . -t $(REGISTRY)/$(NAME):$(VERSION)

push:
	docker push $(REGISTRY)/$(NAME):$(VERSION)

generate: get_controller-gen
	@echo "=============Generating Golang and YAML============="
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	$(CONTROLLER_GEN) rbac:roleName=hpa-plus-operator webhook crd:allowDangerousTypes=true \
		paths="./..." \
		output:crd:artifacts:config=helm/templates/crd \
		output:rbac:artifacts:config=helm/templates/cluster \
		output:webhook:artifacts:config=helm/templates/cluster

view_coverage:
	@echo "=============Loading coverage HTML============="
	go tool cover -html=unit_cover.out
	python -m webbrowser file://$(shell pwd)/.algorithm_coverage/index.html

get_controller-gen:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.4
