(
	set -euxo pipefail; export GO111MODULE=off; export GOPATH=/usr/share/gocode;
	go fmt *.go
	go build output-tool.go
	cp -v output-tool output-tool.exe
	cp -v output-tool output-tool.elf
)
