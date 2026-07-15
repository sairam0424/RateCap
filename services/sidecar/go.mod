module github.com/ratecap/sidecar

go 1.26.2

require (
	github.com/ratecap/proto v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.82.0
)

require (
	github.com/ratecap/core v0.0.0
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/ratecap/proto => ../../proto

replace github.com/ratecap/core => ../core
