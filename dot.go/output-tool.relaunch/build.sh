(
	function : () {
		echo '++' $@
		$@ 2>&1
	}
	set -xeuo pipefail; export GO111MODULE=off; export GOPATH=/usr/share/gocode;
	set +x
	: go fmt *.go
	: go build output-tool.go
	: ln -fv output-tool output-tool.exe
	: ln -fv output-tool output-tool.elf
	: ln -fv output-tool ot
)
