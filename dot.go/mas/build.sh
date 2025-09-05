(
set -xeuo pipefail &&
rm -vf masuds &&
export GO111MODULE=auto &&
export GOPATH=/usr/share/gocode &&
export SECRET=$(openssl rand -hex 16) &&
echo $SECRET &&
go build -ldflags "-s -w -X 'main.buildSecretHex=$SECRET' -X 'main.buildID=dev-uds'" -o masuds masuds.go &&
cp masuds /home/dev/test/
)
