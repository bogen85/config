(set -x; GO111MODULE=auto GOPATH=/usr/share/gocode go build -trimpath -ldflags="-s -w"   goscripter.go)
