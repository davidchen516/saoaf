# Multi-Model Router mock

Run `docker compose -f mocks/mmr/compose.yaml up` and call `http://127.0.0.1:4020`.

The contract exposes only the logical profile `reasoning-high-v1`. `GET /control/v1/profile-snapshots/current` returns its ARR-facing Snapshot. The runtime API requires `X-Resource-Plan-ID`, `X-Resource-Plan-Item-ID`, `X-Tenant-Ref`, and W3C `traceparent`, then returns a `model_route_decision_id`. Prism can select named error examples through its standard example-selection behavior.

This mock validates the SAOAF/MMR boundary. It does not run vLLM Semantic Router, choose a physical model, stream tokens, enforce quotas, or measure routing quality. Those behaviors remain owned by the existing MMR.
