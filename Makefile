.PHONY: test lint demo

test:
	test -z "$$(gofmt -l plugin examples tests)"
	go vet ./...
	go test -race ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# Exit 1 means it found a server that closes first, which the ALB example
# has on purpose. The nginx example is fine and must exit 0. The binary is
# built first because go run turns every failing exit code into 1.
demo:
	go build -o idlegap ./plugin/scripts
	./idlegap --k8s examples/alb/deploy ./examples/alb/api || test $$? -eq 1
	./idlegap --k8s examples/nginx/deploy ./examples/nginx/web
