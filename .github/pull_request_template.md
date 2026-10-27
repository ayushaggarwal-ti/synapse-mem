## What and why

<!-- What does this change, and what problem does it solve? Link the issue: Fixes #123 -->

## How it was tested

<!-- The test that covers it, and anything you ran by hand. -->

## Checklist

- [ ] `gofmt -w .` and `make test` pass
- [ ] Tests cover the change (a bug fix has a test that fails without it)
- [ ] Event payload changes only add fields; old logs still replay
- [ ] New settings have defaults and are documented in `docs/CONFIG.md`
- [ ] Docs updated if behavior changed (README, DESIGN, CONFIG)
