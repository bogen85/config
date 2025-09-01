
# Build in GOPATH mode; import path is local/goscripter.
# We add PWD to GOPATH so src/local/goscripter is discoverable.
BINARY := goscripter

.PHONY: default build clean checksums verify fmt install

default:
	@echo $(BINARY)

build:
	GO111MODULE=auto GOPATH="$(PWD):/usr/share/gocode" \
		go build -trimpath -ldflags="-s -w" -o $(BINARY) .

clean:
	rm -f $(BINARY)

# Generate sha256 checksums for all project files (excluding the checksum file itself)
checksums:
	@echo "Generating checksums.sha256..."
	@rm -f checksums.sha256
	@{ \
		find . -type f ! -path "./checksums.sha256" ! -path "./$(BINARY)" -print0 \
		| sort -z \
		| xargs -0 sha256sum ; \
	} > checksums.sha256
	@echo "Wrote checksums.sha256"

verify:
	sha256sum -c checksums.sha256

fmt:
	@for i in $$(find . -name '*.go'); do echo [$$i]; go fmt $$i; done

install: build
	cp -v $(BINARY) $(HOME)/bin/$(BINARY)
