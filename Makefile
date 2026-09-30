APP     = gitlab-ci-bootstrap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
OUTDIR  = bin

.PHONY: linux

linux:
	@mkdir -p $(OUTDIR)
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" -o $(OUTDIR)/$(APP)-linux-amd64 ./cmd/gitlab-ci-bootstrap

clean:
	rm -rf $(OUTDIR)
