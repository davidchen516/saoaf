# Identity and authorization mock

This local-only stack provides a real OIDC provider and an AuthZEN-compatible contract mock. It contains no users, passwords, client secrets, or production configuration.

1. Export `KC_BOOTSTRAP_ADMIN_USERNAME` and `KC_BOOTSTRAP_ADMIN_PASSWORD` from a local secret source.
2. Run `docker compose -f mocks/identity/compose.yaml up` from the repository root.
3. Open `http://127.0.0.1:8080`, select realm `saoaf-dev`, and create temporary test users and role mappings.
4. Use discovery at `http://127.0.0.1:8080/realms/saoaf-dev/.well-known/openid-configuration`.
5. Send AuthZEN evaluations to `http://127.0.0.1:4010/access/v1/evaluation` with a UUID `X-Request-ID`. Use `Prefer: example=deny` to select the deny example when supported by the Prism client path.

The Keycloak client `saoaf-local-pkce` accepts only `http://127.0.0.1:3000/callback`, requires Authorization Code + PKCE `S256`, and disables implicit and password grants. Production browser access uses a BFF and secure session cookie as defined in the design document.

Prism validates and returns contract examples; it does not authenticate the PDP caller. Production must authenticate that channel and fail closed when the PDP is unavailable or returns an invalid response.
