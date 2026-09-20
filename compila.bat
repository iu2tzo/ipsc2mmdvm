set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
set GOARM=
go build -trimpath -o ipsc2mmdvm_linux_amd64 .

set GOARCH=arm64
go build -trimpath -o ipsc2mmdvm_linux_arm64 .

set GOARCH=arm
set GOARM=6
go build -trimpath -o ipsc2mmdvm_linux_armv6 .