set shell := ["bash", ".just-shell"]
set positional-arguments

# Show available recipes.
default:
    @just --list

# Run declared fast checks; accepts project-check options such as --json.
lint *args:
    @project-check fast "$@"

# Run the declared full gate; accepts project-check options such as --json.
verify *args:
    @project-check full "$@"

# Format supported repository files using pinned tools.
fmt:
    @python3 .project-format.py write default

# Check formatting without changing files.
fmt-check:
    @python3 .project-format.py check default

# Alias for the declared fast gate.
check *args:
    @project-check fast "$@"

# Run the service with a configuration file (default: $ICHNORA_CONFIG).
start *args:
    @go run -tags libsqlite3 ./cmd/ichnora serve "$@"

# Run the Go tests against the linked Nix SQLite.
test *args:
    @go test -tags libsqlite3 -count=1 "$@" ./...

# Compile all packages without writing artifacts.
build *args:
    @go build -tags libsqlite3 "$@" ./...
