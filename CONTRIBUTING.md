# Contributing to Wiser Dashboard

Thanks for helping. Every change goes in through a **pull request**: nobody pushes to `main` directly, the
maintainer included. Pull requests are checked automatically on Linux, macOS and Windows and reviewed before
they're merged.

This project was *clauded*: written with Claude (Anthropic) in Claude Code, directed and tested by its author.
AI-assisted contributions are welcome on the same terms as any other: you're responsible for what you submit,
you understand it, and you've tested it.

## The workflow

1. **Start with an issue** for anything bigger than a small fix: describe the problem or idea, so we can agree on
   the approach before you spend time on it. Small fixes can go straight to a pull request.
2. **Make a branch** from an up-to-date `main` (in your fork if you don't have write access):
   ```sh
   git switch main && git pull
   git switch -c fix/hot-water-boost-length    # fix/…, feature/…, docs/… or ci/…
   ```
3. **Make one focused change.** A pull request should do one thing; split unrelated changes into separate ones.
   Match the style of the code around you: comments, naming and idioms.
4. **Run the checks locally** (see below) and **test it for real** where you can.
5. **Commit** with a clear message: a short summary line in the imperative ("Add a 1-hour hot water boost
   switch"), a blank line, then *why* the change is needed and anything a reviewer should know.
6. **Open a pull request** against `main` and fill in the template: what changed, why, and how you tested it.
   Link the issue (`Fixes #12`).
7. **CI must be green.** Every check has to pass; if one fails, push a fix to the same branch.
8. **Review.** The maintainer reviews every pull request. Answer comments with changes or replies, and push
   follow-up commits to the same branch; don't open a new pull request.
9. **Merge.** The maintainer merges with **squash and merge**, so `main` gets one clean commit per pull request,
   and the branch is deleted.

## Checks to run before opening a pull request

```sh
gofmt -l .                       # must print nothing
go vet ./... && go vet -tags matter ./...
go test -race ./...              # MCP server tests (no -race on Windows)
go run ./tools/build             # builds this machine's version, with the Matter bridge if Node.js is installed
```

If you changed the Matter bridge (`matter/`) or anything it relies on:

```sh
cd matter
npm run typecheck
node test/e2e.mjs dist/wiser-matter-$(go env GOOS)-$(go env GOARCH)   # the bridge, with a real Matter controller
node test/system.mjs ../wiser-dashboard                              # the whole app (not on Windows)
```

These pair a real Matter controller with the bridge against a mock hub; mDNS must work on your machine (on macOS,
allow Local Network access when asked).

## Testing against a real hub

The automated tests use mock hubs, so they can't catch everything the real hardware does. If your change affects
how the dashboard talks to the hub, also try it on a real Wiser hub and say so in the pull request. Be careful:
it's real heating. Don't leave test overrides in place, and put back any setting you change.

The hub handles one request at a time, about five seconds apart, and stalls on responses larger than ~2.7 KB.
Every request must go through the hub queue (`Hub.Do`), never around it, and nothing may fetch
`/data/domain/` whole from the hub.

## Never commit secrets or personal data

- No hub secrets, MCP tokens, passwords, `config.json`, `history.jsonl`, `health.jsonl` or Matter state.
  `.gitignore` covers the usual files; check `git status` and the diff before every commit anyway.
- No real IP addresses, network names or serial numbers in code, tests or docs; use examples like
  `192.168.1.50`.
- No built binaries (`wiser-dashboard`, `dist/`, `matterbin/`).

If you find a security problem, don't open a public issue: contact the maintainer privately on GitHub
([@shenhab](https://github.com/shenhab)).

## What's especially welcome

The [limitations table in the README](README.md#limitations-and-what-could-be-done) lists known gaps and what
could be done about them. Reports and tests with **other Wiser hubs and devices** (OpenTherm boilers, smart
plugs, radiator valves, second-generation hubs) are particularly valuable, because the project has only been
tested on one setup.

## Releases

Releases are made by the maintainer by pushing a version tag on `main` (`v1.4.0`); CI then builds, tests,
packages, **signs** (`tools/sign`, with the `RELEASE_SIGNING_KEY` repository secret) and publishes every platform.
Contributors don't need to do anything for a release. The public keys in `internal/releasekeys` must not be
changed without the maintainer: installs only accept updates signed by them.

## Licence

The project is under the [MIT licence](LICENSE). By opening a pull request you agree that your contribution is
licensed under it too.
