# Transport: gRPC

## Summary

The gRPC transport carries the same three x402 v2 JSON objects as the HTTP
transport. The bytes in metadata are the base64 strings an HTTP client already
knows. Native RPCs use this transport directly. A grpc-gateway deployment uses
the HTTP transport on the public port and this transport only for internal RPCs
that are paid on their own.

## Metadata

| Role | Key | When |
| --- | --- | --- |
| Challenge | `payment-required` | Trailing metadata on `RESOURCE_EXHAUSTED` |
| Payment | `payment-signature` | Incoming metadata |
| Receipt | `payment-response` | Trailing metadata on success and on settlement failure |

V1 keys remain accepted: `x402-payment`, `x402-payment-response`, and
`x402-payment-requirements`.

The status message is a short reason (`payment_required`, or the facilitator's
`invalidReason`). The object itself is in the metadata, not in the message
string.

A challenge also attaches `google.rpc.ErrorInfo` with reason `PAYMENT_REQUIRED`
and domain `x402.org`.

## Objects

`payment-required` is a base64 `PaymentRequired` object: `x402Version`, `error`,
`resource` (with an absolute `url`), and `accepts`. `resource.url` for a native
method is `grpc://localhost` plus the full method name, unless the route sets
its own URL. `accepts[].network` is CAIP-2. `accepts[].amount` is a decimal
string of atomic units. `maxTimeoutSeconds` is present on every accept and is
the minimum of the route validity and the time left on the RPC deadline.

`payment-signature` is a base64 `PaymentPayload`. `accepted` must equal one
advertised option: scheme, network, amount, asset, payTo, maxTimeoutSeconds,
and the server's `extra` as a subset. The scheme payload is opaque to the
resource server.

`payment-response` is a base64 settlement receipt. `transaction` and `network`
are always present. `transaction` may be empty. `success: false` is how a
settlement failure, including `settlement_pending`, is reported. A pending
settlement keeps the transaction hash.

## Status mapping

| Condition | Code | Metadata |
| --- | --- | --- |
| No payment, verification failed, or settlement failed | `RESOURCE_EXHAUSTED` | `payment-required`, or `payment-response` when a receipt exists |
| Malformed payload | `INVALID_ARGUMENT` | none |
| Facilitator transport failure | `INTERNAL` | none |
| Handler success after settlement | `OK` | `payment-response` |

## Payment flow

The default flow is `authorization`:

1. Verify the payload with the facilitator.
2. Run the handler. The handler sees the verified payer. The transaction hash
   is empty, because funds have not moved.
3. Settle only if the handler returns nil.
4. Return the handler result with `payment-response`.

`upfront` settles before the handler. The handler sees the transaction hash.
A failed handler still carries `payment-response`, because the client was
charged. `escrow` settles once before the handler and once after a successful
handler.

Unary calls withhold the handler result until settlement returns. That is the
same guarantee as an HTTP server that buffers a 2xx body.

Streams verify before the first message and settle only if the handler returns
nil. Bytes already sent cannot be recalled. A route that must not leak unpaid
bytes sets `upfront`.

## Idempotency

When `extensions["payment-identifier"]` carries an id, a settled success is
remembered for the route validity window. A retry with the same id returns the
stored receipt and does not call `/settle` again.

## References

- [Core x402 Specification](https://github.com/x402-foundation/x402/blob/main/specs/x402-specification-v2.md)
- [HTTP transport](https://github.com/x402-foundation/x402/blob/main/specs/transports-v2/http.md)
