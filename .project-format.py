"""Format repository files with argv-only commands and a read-only check mode."""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path


def commands(language: str, check: bool, files: list[str]) -> list[list[str]]:
    groups: dict[str, list[str]] = {
        name: [] for name in ("nix", "shell", "just", "python", "biome", "prettier", "go")
    }
    for name in files:
        path = Path(name)
        suffix = path.suffix.lower()
        if suffix == ".nix":
            group = "nix"
        elif suffix == ".go":
            group = "go"
        elif suffix in {".sh", ".bash"} or path.name in {".envrc", ".just-shell"}:
            group = "shell"
        elif path.name in {"Justfile", "justfile", ".justfile"}:
            group = "just"
        elif language == "python" and suffix in {".py", ".pyi"}:
            group = "python"
        elif language == "web" and suffix in {
            ".js",
            ".jsx",
            ".ts",
            ".tsx",
            ".mjs",
            ".cjs",
            ".mts",
            ".cts",
            ".css",
            ".json",
            ".jsonc",
        }:
            group = "biome"
        elif suffix in {".md", ".mdx", ".yaml", ".yml", ".json", ".jsonc"} or (
            language == "web" and suffix in {".astro", ".html", ".scss", ".less"}
        ):
            group = "prettier"
        else:
            continue
        # Prefix paths so names beginning with '-' cannot become formatter options.
        groups[group].append("./" + name)

    prefixes = {
        "nix": ["nixfmt", *(["--check"] if check else [])],
        "shell": ["shfmt", "-ln", "bash", "-i", "2", "-d" if check else "-w"],
        # gofmt -l exits 0 even for unformatted files; run() fails on its output.
        "go": ["gofmt", "-l" if check else "-w"],
        "python": ["ruff", "format", "--no-cache", *(["--check"] if check else [])],
        "biome": ["bun", "run", "biome", "format", *([] if check else ["--write"])],
        "prettier": [
            *(["bun", "run", "prettier"] if language == "web" else ["prettier"]),
            "--check" if check else "--write",
        ],
    }
    result = []
    for group, paths in groups.items():
        if group == "just":
            result.extend(
                [
                    "just",
                    "--unstable",
                    "--fmt",
                    *(["--check"] if check else []),
                    "--justfile",
                    path,
                ]
                for path in paths
            )
        else:
            # Bound argv size without shell interpolation, including unusual filenames.
            result.extend(
                prefixes[group] + paths[i : i + 100] for i in range(0, len(paths), 100)
            )
    if language == "rust" and "Cargo.toml" in files:
        result.append(["cargo", "fmt", "--all", *(["--", "--check"] if check else [])])
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("write", "check"))
    parser.add_argument("language", choices=("default", "python", "rust", "web"))
    args = parser.parse_args()
    try:
        # rg respects repository ignore rules, includes untracked scaffold files,
        # and does not follow symlinks. Only search this recipe's repository root.
        scan = subprocess.run(
            [
                "rg",
                "--files",
                "--hidden",
                "--no-require-git",
                "--null",
                "--glob",
                "!.git",
                ".",
            ],
            capture_output=True,
            check=False,
        )
        if scan.returncode not in (0, 1):
            sys.stderr.buffer.write(scan.stderr)
            return scan.returncode
        files = sorted(
            name.removeprefix("./") for name in scan.stdout.decode().split("\0") if name
        )
        plan = commands(args.language, args.mode == "check", files)
        for command in plan:
            if shutil.which(command[0]) is None:
                print(
                    f"format: blocked: missing {command[0]}; enter nix develop .",
                    file=sys.stderr,
                )
                return 127
        for command in plan:
            if command[:2] == ["gofmt", "-l"]:
                listed = subprocess.run(command, capture_output=True, check=False)
                if listed.returncode or listed.stdout:
                    sys.stdout.buffer.write(listed.stdout)
                    sys.stderr.buffer.write(listed.stderr)
                    return listed.returncode or 1
                continue
            result = subprocess.run(command, check=False)
            if result.returncode:
                return result.returncode
    except FileNotFoundError as error:
        print(
            f"format: blocked: missing {error.filename}; enter nix develop .",
            file=sys.stderr,
        )
        return 127
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
