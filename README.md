# grpc-gateway-x402

Payment middleware for grpc-gateway that implements the [x402 protocol](https://x402.org).

[![Go Reference](https://pkg.go.dev/badge/github.com/becomeliminal/grpc-gateway-x402.svg)](https://pkg.go.dev/github.com/becomeliminal/grpc-gateway-x402)
[![Go Report Card](https://goreportcard.com/badge/github.com/becomeliminal/grpc-gateway-x402)](https://goreportcard.com/report/github.com/becomeliminal/grpc-gateway-x402)

## Features

- Multi-currency: Accept any ERC-20 token (USDC, DAI, USDT, custom tokens)
- Multi-chain: Ethereum, Base, Polygon, Arbitrum, Optimism, etc.
- Per-endpoint pricing with wildcard pattern matching
- Payment context propagates to gRPC handlers
- Custom HTML paywall support with User-Agent detection
- Pluggable verification: Use Coinbase's facilitator, your own, or implement custom logic
- Output schema support for API documentation

## Quick Start

### Installation

```bash
go get github.com/becomeliminal/grpc-gateway-x402
```

### Basic Usage

```go
package main

import (
    x402 "github.com/becomeliminal/grpc-gateway-x402"
    "github.com/becomeliminal/grpc-gateway-x402/evm"
    "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

func main() {
    // Create EVM verifier (supports all EVM chains + ERC-20 tokens)
    verifier, _ := evm.NewEVMVerifier("https://facilitator.x402.org")

    // Configure payment requirements
    x402Config := x402.Config{
        Verifier: verifier,
        EndpointPricing: map[string]x402.PricingRule{
            "/v1/premium/*": {
                Amount: "1.00",  // $1.00
                AcceptedTokens: []x402.TokenRequirement{
                    {
                        Network:       "base-mainnet",
                        AssetContract: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", // USDC
                        Symbol:        "USDC",
                        Recipient:     "0xYourAddress",
                    },
                },
            },
        },
    }

    // Create grpc-gateway mux with payment metadata propagation
    mux := runtime.NewServeMux(x402.WithPaymentMetadata())

    // Wrap with payment middleware
    handler := x402.PaymentMiddleware(x402Config)(mux)

    // Start server
    http.ListenAndServe(":8080", handler)
}
```

### Access Payment Details in gRPC Handlers

```go
func (s *server) GetPremiumContent(ctx context.Context, req *pb.Request) (*pb.Response, error) {
    // Extract payment info from gRPC context
    payment, ok := x402.GetPaymentFromGRPCContext(ctx)
    if !ok {
        return nil, status.Error(codes.Unauthenticated, "payment required")
    }

    log.Printf("Received payment: %s %s from %s",
        payment.Amount, payment.TokenSymbol, payment.PayerAddress)

    // Return premium content
    return &pb.Response{Data: "premium content"}, nil
}
```

## Configuration

### Multi-Currency

```go
x402Config := x402.Config{
    Verifier: verifier,
    EndpointPricing: map[string]x402.PricingRule{
        "/v1/api/*": {
            Amount: "0.10",
            AcceptedTokens: []x402.TokenRequirement{
                {
                    Network:       "base-mainnet",
                    AssetContract: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
                    Symbol:        "USDC",
                    Recipient:     "0xYourAddress",
                    TokenDecimals: 6,
                },
                {
                    Network:       "ethereum-mainnet",
                    AssetContract: "0x6B175474E89094C44Da98b954EedeAC495271d0F",
                    Symbol:        "DAI",
                    Recipient:     "0xYourAddress",
                    TokenDecimals: 18,
                },
                {
                    Network:       "polygon-mainnet",
                    AssetContract: "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
                    Symbol:        "USDC",
                    Recipient:     "0xYourAddress",
                    TokenDecimals: 6,
                },
            },
        },
    },
}
```

### Path-Based Pricing

```go
EndpointPricing: map[string]x402.PricingRule{
    "/v1/premium/*":     {Amount: "1.00", ...},  // $1.00 for premium
    "/v1/basic/*":       {Amount: "0.10", ...},  // $0.10 for basic
    "/v1/micro/*":       {Amount: "0.01", ...},  // $0.01 for micro
    "/v1/specific-path": {Amount: "0.50", ...},  // Exact match
}
```

### Skip Free Endpoints

```go
Config{
    SkipPaths: []string{
        "/health",
        "/metrics",
        "/v1/public/*",
    },
}
```

### Default Pricing

```go
Config{
    DefaultPricing: &x402.PricingRule{
        Amount: "0.05",  // Default for unmatched paths
        AcceptedTokens: []x402.TokenRequirement{...},
    },
}
```

### Custom HTML Paywall

```go
Config{
    CustomPaywallHTML: `
        <html>
        <head><title>Payment Required</title></head>
        <body>
            <h1>💳 Payment Required</h1>
            <p>This content requires payment. Please connect your wallet to continue.</p>
            <button onclick="connectWallet()">Connect Wallet</button>
        </body>
        </html>
    `,
}
```

Browsers get HTML, API clients get JSON.

### Output Schema

```go
PricingRule{
    Amount: "1.00",
    OutputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "joke": map[string]interface{}{
                "type": "string",
                "description": "A programming joke",
            },
            "paid": map[string]interface{}{
                "type": "boolean",
            },
        },
    },
    AcceptedTokens: []x402.TokenRequirement{...},
}
```

## Custom Facilitators

The library supports any x402-compatible facilitator or custom verification logic.

### Use Any Facilitator

```go
// Coinbase's facilitator
verifier, _ := evm.NewEVMVerifier("https://facilitator.x402.org")

// Your own facilitator
verifier, _ := evm.NewEVMVerifier("https://my-facilitator.com")

// Local development
verifier, _ := evm.NewEVMVerifier("http://localhost:3000")
```

### Implement Custom Verification

Skip facilitators entirely by implementing the `ChainVerifier` interface:

```go
type CustomVerifier struct {
    ethClient *ethclient.Client
}

func (v *CustomVerifier) Verify(ctx context.Context, payment *x402.Payment) (*x402.VerificationResult, error) {
    // Your verification logic
    // - Check on-chain directly
    // - Query your own database
    // - Implement custom rules
    return &x402.VerificationResult{Valid: true}, nil
}

func (v *CustomVerifier) Settle(ctx context.Context, payment *x402.Payment) (*x402.SettlementResult, error) {
    // Your settlement logic
    // - Submit transaction yourself
    // - Update internal ledger
    // - Call payment processor
    return &x402.SettlementResult{TransactionHash: "0x..."}, nil
}

func (v *CustomVerifier) SupportedNetworks() []x402.NetworkInfo {
    return []x402.NetworkInfo{{Network: "ethereum-mainnet"}}
}

// Use it
config := x402.Config{
    Verifier: &CustomVerifier{ethClient: client},
}
```

## How It Works

1. Client requests resource
2. Server returns 402 with payment requirements
3. Client signs payment (EIP-3009)
4. Client retries with X-PAYMENT header
5. Middleware verifies and settles payment
6. Server returns resource with X-PAYMENT-RESPONSE

## Protocol Flow

```
Client                    Gateway                    Facilitator              Blockchain
  |                          |                            |                        |
  |-- GET /v1/premium/data ->|                            |                        |
  |                          |                            |                        |
  |<- 402 Payment Required --|                            |                        |
  |   (with payment options) |                            |                        |
  |                          |                            |                        |
  |-- GET /v1/premium/data ->|                            |                        |
  |   X-PAYMENT: <sig>       |                            |                        |
  |                          |-- POST /verify ----------->|                        |
  |                          |<- {valid: true} -----------|                        |
  |                          |                            |                        |
  |                          |-- POST /settle ----------->|                        |
  |                          |                            |-- transfer() --------->|
  |                          |                            |<- tx hash -------------|
  |                          |<- {txHash: "0x..."} -------|                        |
  |                          |                            |                        |
  |<- 200 OK ----------------|                            |                        |
  |   X-PAYMENT-RESPONSE     |                            |                        |
  |   Premium Data           |                            |                        |
```

## Supported Networks

EVM chains via Coinbase facilitator: Ethereum, Base, Polygon, Arbitrum, Optimism (mainnet/testnet)

## Token Addresses

### USDC

- **Base Mainnet**: `0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913`
- **Base Sepolia**: `0x036CbD53842c5426634e7929541eC2318f3dCF7e`
- **Ethereum Mainnet**: `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48`
- **Polygon Mainnet**: `0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359`

### DAI

- **Ethereum Mainnet**: `0x6B175474E89094C44Da98b954EedeAC495271d0F`
- **Base Mainnet**: `0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb`

### USDT

- **Ethereum Mainnet**: `0xdAC17F958D2ee523a2206206994597C13D831ec7`
- **Polygon Mainnet**: `0xc2132D05D31c914a87C6611C10748AEb04B58e8F`

## Examples

```bash
cd examples/basic
export RECIPIENT_ADDRESS="0xYourAddress"
go run main.go
```

See [`examples/`](./examples) for more:
- `basic/` - Single token integration
- `multi-token/` - Multiple currencies and networks, tiered pricing
- `jokes-api/` - Fun demo (pay $0.0001 for programming jokes)

## API Reference

### Types

#### `Config`

Main configuration for payment middleware.

```go
type Config struct {
    Verifier         ChainVerifier          // Payment verification backend
    EndpointPricing  map[string]PricingRule // Path patterns to pricing
    DefaultPricing   *PricingRule           // Fallback pricing (optional)
    ValidityDuration time.Duration          // Payment validity (default: 5 min)
    SkipPaths        []string               // Paths to skip payment checks
}
```

#### `PricingRule`

Defines payment requirements for an endpoint.

```go
type PricingRule struct {
    Amount         string              // Payment amount (e.g., "1.00")
    AcceptedTokens []TokenRequirement  // Accepted payment options
    Description    string              // Human-readable description
    MimeType       string              // Resource MIME type (optional)
}
```

#### `TokenRequirement`

Specifies a payment option (network + token).

```go
type TokenRequirement struct {
    Network       string // Blockchain network
    AssetContract string // Token contract address
    Symbol        string // Token symbol (e.g., "USDC")
    Recipient     string // Payment recipient address
    TokenName     string // Human-readable token name
    TokenDecimals int    // Token decimals (optional)
}
```

#### `PaymentContext`

Payment information accessible in handlers.

```go
type PaymentContext struct {
    Verified        bool
    PayerAddress    string
    Amount          string
    TokenSymbol     string
    Network         string
    TransactionHash string
    SettledAt       time.Time
}
```

### Functions

- `PaymentMiddleware(cfg Config)` - Creates HTTP middleware
- `GetPaymentFromContext(ctx)` - Extract payment from HTTP context
- `GetPaymentFromGRPCContext(ctx)` - Extract payment from gRPC metadata
- `WithPaymentMetadata()` - Propagate payment to gRPC context
- `evm.NewEVMVerifier(url)` - Create EVM verifier

## Architecture

The library is designed with extensibility in mind:

```
┌─────────────────────────────────────────────────────┐
│                  HTTP Request                       │
└────────────────────┬────────────────────────────────┘
                     │
                     ▼
         ┌───────────────────────┐
         │ PaymentMiddleware     │  Standard http.Handler
         │ - Check path rules    │
         │ - Parse X-PAYMENT     │
         │ - Inject context      │
         └───────────┬───────────┘
                     │
                     ▼
         ┌───────────────────────┐
         │   ChainVerifier       │  Interface (pluggable!)
         │   - Verify payment    │
         │   - Settle on-chain   │
         └───────────┬───────────┘
                     │
         ┌───────────┼───────────┐
         │           │           │
         ▼           ▼           ▼
    ┌────────┐  ┌────────┐  ┌────────┐
    │  EVM   │  │ Solana │  │Future  │  Future: Easy to add new chains!
    │Verifier│  │(TODO)  │  │chains  │
    └────────┘  └────────┘  └────────┘
```

## Testing

```bash
go test ./...
go test -cover ./...
```

## License

MIT
