# Contributing to Ichnora

Use [GitHub Issues](https://github.com/timfewi/ichnora/issues) for bugs and feature
requests. For bugs, include the Ichnora version, the affected client or adapter,
and steps to reproduce the behavior with synthetic data. Use the
[security reporting process](SECURITY.md) for potential vulnerabilities.

Start with the [README](README.md), the [operations guide](docs/operations.md),
and the [project policy](AGENTS.md) for development conventions. From the root
of your checkout, run:

```sh
nix develop .
just fmt
just check
```

CI audits workflow files with `zizmor` and checks Go code against the Go
vulnerability database using `govulncheck`. Both tools are in the development
shell.

Keep pull requests focused. Describe the user-visible change, report which
checks you ran, and update the affected documentation when behavior changes.
Use synthetic fixtures in tests, examples, and reports. Keep real session
transcripts, runtime databases, and credentials out of GitHub.
