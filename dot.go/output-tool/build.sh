(
	set -euxo pipefail; export GO111MODULE=off; export GOPATH=/usr/share/gocode;
	go fmt *.go
	go build output-tool.go
	ln -fv output-tool output-tool.exe
	ln -fv output-tool output-tool.elf
	ln -fv output-tool ot
)
