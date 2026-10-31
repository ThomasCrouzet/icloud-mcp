## Summary

Describe the change and the problem that it solves.

## Scope

State the affected domain and capability gate. Name any protocol change.

## Checks

- [ ] `make test`
- [ ] `make vet`
- [ ] `make lint`
- [ ] `make check-registry` when the change touches release metadata
- [ ] `make protocol-evidence EVIDENCE_DIR=/absolute/path/outside/repository`
      when the change touches a protocol path. Keep the evidence local.
- [ ] `CHANGELOG.md` and the applicable document under `docs/` are updated.

## Safety

- [ ] No secret, app-specific password, token, certificate, or live account data is included.
- [ ] The change keeps the fixed egress allowlists and the per-domain client boundaries.
- [ ] The change keeps global read-only precedence and independent capability gates.
- [ ] The change adds no `os/exec`, telemetry, disk write, or private Apple API.
