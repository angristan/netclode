module github.com/angristan/netclode/clients/cli

go 1.25.5

require (
	connectrpc.com/connect v1.19.1
	github.com/angristan/netclode/services/control-plane v0.0.0
	github.com/fatih/color v1.18.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/net v0.49.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251222181119-0a764e51fe1b // indirect
	google.golang.org/grpc v1.78.0 // indirect
)

replace github.com/angristan/netclode/services/control-plane => ../../services/control-plane
