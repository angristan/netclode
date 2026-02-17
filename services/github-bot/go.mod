module github.com/angristan/netclode/services/github-bot

go 1.25.5

require (
	github.com/angristan/netclode/services/control-plane v0.0.0
	github.com/bradleyfalzon/ghinstallation/v2 v2.14.0
	github.com/google/go-github/v68 v68.0.0
	github.com/redis/go-redis/v9 v9.17.3
	golang.org/x/net v0.49.0
)

require (
	connectrpc.com/connect v1.19.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/google/go-github/v69 v69.0.0 // indirect
	github.com/google/go-querystring v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.39.0 // indirect
	go.opentelemetry.io/otel/trace v1.39.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251222181119-0a764e51fe1b // indirect
	google.golang.org/grpc v1.78.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/angristan/netclode/services/control-plane => ../control-plane
