# Database migrations (I02 scope: directory convention only)

Schema ownership: **I04** establishes the PostgreSQL schema, `goose` migration
framework, and expand/migrate/contract discipline. Do not add migration files
here before I04 lands its framework choice; this README reserves the path so
the scaffold layout matches the approved development plan (§3:

```
sovereign-control-plane/
├── modules/...
└── migrations/
```

Note: the repository root *is* the control-plane monorepo, so `migrations/`
lives at the root next to `cmd/` and `internal/`.
