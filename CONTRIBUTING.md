# Contributing

1. Create a feature branch from `main` (`feat/...`, `fix/...`).
2. Run `make ci` before pushing. It runs tidy, vet, golangci-lint, govulncheck, tests with the coverage gate and the Helm checks.
3. Use [Conventional Commits](https://www.conventionalcommits.org/) and sign your commits.
4. Open a pull request against `main`.

Use `make demo` to try UI changes without a UniFi controller.
