# SAOAF Phase 0 mocks

The mock suite is split by replaceable integration boundary:

| Directory | Port | Capability |
|---|---:|---|
| `identity` | 8080, 4010, 4011 | OIDC/MFA, AuthZEN PDP, approval contract, ephemeral workload certificate |
| `mmr` | 4020 | OpenAI-compatible Multi-Model Router contract |
| `events` | 4222, 8222 | NATS JetStream event transport |

Start each Compose project independently from the repository root. Set the Keycloak bootstrap username and password from a local secret source before starting `identity`. No production credential or private key belongs in this repository.

These mocks prove contracts and failure handling. They do not prove production HA, real model-routing quality, enterprise identity lifecycle, approval segregation, SPIRE attestation, or WORM compliance.

The published Prism 5.15.10 image is `linux/amd64`; Apple Silicon runs it through Docker emulation. Compose disables Prism multiprocess mode to avoid its Node.js 24 container startup defect. This affects local Mock performance only.
