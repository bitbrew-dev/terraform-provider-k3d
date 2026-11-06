BINARY := terraform-provider-k3d
DIST   := dist

GORELEASER := go run github.com/goreleaser/goreleaser/v2@v2.20.0

.PHONY: build test testacc vet tidy validate-examples docs release-check

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

validate-examples:
	./scripts/validate-examples.sh

docs:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@v0.25.0 generate --provider-name k3d --rendered-provider-name k3d

release-check:
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean --skip=sign
