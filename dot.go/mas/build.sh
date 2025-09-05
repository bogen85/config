(
set -xeuo pipefail &&
rm -vf masuds &&
export GO111MODULE=auto &&
export GOPATH=/usr/share/gocode &&
export CGO_ENABLED=0
export SECRET=$(openssl rand -hex 16) &&
echo $SECRET &&
go build -tags 'osusergo,netgo' -ldflags "-s -w -X 'main.buildSecretHex=$SECRET' -X 'main.buildID=dev-uds'" -o masuds masuds.go &&
cp masuds /home/dev/test/
strip -v /home/dev/test/masuds
)
