(
set -xeuo pipefail &&
app=file-digest
rm -vf $app &&
export GO111MODULE=off &&
export GOPATH=/usr/share/gocode &&
go build -o $app $app.go
)
