.PHONY: test terminal-bench vet vulncheck verify verify-sdk-purity

GRATEFUL_LIVE_TESTS ?= skip

test:
	GRATEFUL_LIVE_TESTS=$(GRATEFUL_LIVE_TESTS) go test ./...

terminal-bench:
	./scripts/run-terminal-bench.sh

vet:
	go vet ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

verify: test vet vulncheck verify-sdk-purity

verify-sdk-purity:
	@forbidden='(github.com/gratefulagents/gratefulagents/|sigs.k8s.io/controller-runtime|k8s.io/client-go|github.com/jackc/pgx/v5/pgxpool|github.com/aws/aws-sdk-go|github.com/aws/aws-sdk-go-v2|github.com/google/go-github)'; \
	hits=$$(go list -deps ./... | grep -E "$$forbidden" || true); \
	if [ -n "$$hits" ]; then \
		echo "SDK forbidden dependencies detected:"; \
		echo "$$hits"; \
		exit 1; \
	fi
