## What and why

<!-- What does this change, and why is it needed? Link the issue: "Fixes #12". -->

## How it was tested

<!-- Tick what applies and add details. -->

- [ ] `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass
- [ ] `go run ./tools/build` builds
- [ ] Matter bridge changes: `npm run typecheck`, `test/e2e.mjs` and `test/system.mjs` pass
- [ ] Tried on a real Wiser hub (model / firmware: …)
- [ ] Docs (README, CONTRIBUTING) updated if behaviour or setup changed

## Checklist

- [ ] One focused change
- [ ] No secrets, tokens, real IPs, config or history files, or built binaries in the diff
