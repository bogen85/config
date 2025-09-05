(
set -xeuo pipefail &&
app=file-digest
rm -vf $app &&
export GO111MODULE=auto &&
export GOPATH=/usr/share/gocode &&
go build -o $app $app.go
)
