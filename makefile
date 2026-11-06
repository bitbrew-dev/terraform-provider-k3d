BINARY := terraform-provider-k3d
DIST   := dist

.PHONY: build test testacc vet tidy

build:
	go build -o $(DIST)/$(BINARY) .

test:
	go test -race ./...

testacc:
	TF_ACC=1 go test -count=1 -timeout 60m ./...

vet:
	go vet ./...

tidy:
	go mod tidy
