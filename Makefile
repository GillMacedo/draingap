.PHONY: test lint demo

test:
	test -z "$$(gofmt -l plugin examples tests)"
	go vet ./...
	go vet ./tests/testdata/progs/...
	go test -race ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# The tight and billing examples have problems on purpose, so they run
# with --fail-on never.
demo:
	go run ./plugin/scripts --pkg ./examples/orders examples/orders/deploy.yaml
	go run ./plugin/scripts --fail-on never --pkg ./examples/orders examples/orders/deploy-tight.yaml
	go run ./plugin/scripts --fail-on never --pkg ./examples/billing examples/billing/deploy.yaml
