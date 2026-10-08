# Contributing

Thanks for helping out. Issues and pull requests are welcome.

## Before you open a pull request

- Open an issue first for anything larger than a small fix, so we can agree on the approach.
- Run the checks CI runs (no hardware needed):

  ```bash
  go build ./... && go vet ./... && go test ./...
  ```

- If you have a BenchPod, try the commands you changed against it, and say how in the pull request.
- Keep commits small and focused, and describe what changed and why.

## Releases

Maintainers release by tag; see "Releasing" in [README.md](README.md).

## Security

Report vulnerabilities privately, see [SECURITY.md](SECURITY.md).
