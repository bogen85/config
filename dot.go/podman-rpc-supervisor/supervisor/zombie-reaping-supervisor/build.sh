(export GO111MODULE=auto; export GOPATH=/usr/share/gocode:$(pwd); CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o supervisor supervisor.go )
