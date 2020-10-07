#
# Makefile with some common workflow for dev, build and test
#
export GOPROXY?=direct

protoc-setup:
	cd meshes
	wget https://raw.githubusercontent.com/layer5io/meshery/master/meshes/meshops.proto

proto:	
	protoc -I meshes/ meshes/meshops.proto --go_out=plugins=grpc:./meshes/

docker:
	docker build -t layer5/meshery-linkerd .

docker-run:
	(docker rm -f meshery-linkerd) || true
	docker run --name meshery-linkerd -d \
	-p 10001:10001 \
	-e DEBUG=true \
	layer5/meshery-linkerd

run:
	DEBUG=true go run main.go

.PHONY: local-check
local-check: tidy
local-check: golang-ci

.PHONY: tidy
tidy:
	@echo "Executing go mod tidy"
	go mod tidy

.PHONY: golang-ci
golangci-lint: $(GOLANGLINT)
	@echo "Golang-ci checking"
	$(GOPATH)/bin/golangci-lint run

$(GOLANGLINT):
	(cd /; GO111MODULE=on GOSUMDB=off go get github.com/golangci/golangci-lint/cmd/golangci-lint@v1.30.0)
	# aisuko local
	#(cd /; GO111MODULE=on GOPROXY="https://goproxy.cn,direct" GOSUMDB=off go get github.com/golangci/golangci-lint/cmd/golangci-lint@v1.30.0)