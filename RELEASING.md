# Releasing agent-vm

`release.sh` checks a commit, tags it, builds a tarball per platform from the tag, publishes them
on GitHub with their `SHA256SUMS`, and prints the Homebrew lines. It runs on a Mac: the macOS
binaries need its SDK and `codesign`; Linux and Windows are cross-built there. It bumps and
commits nothing.

Needs: git, make, Go, the Xcode command line tools, `gh` logged in, and for signed Windows
binaries `osslsigncode` (`brew install osslsigncode`).

## A release

1. In an ordinary commit on the branch: `NEXT_VERSION := X.Y.Z` in the Makefile, and CHANGELOG.md's
   `## Unreleased` renamed `## X.Y.Z`. Push it, and wait for CI (the `go` workflow) to pass on it.
2. On the Mac, in a clean clone of that commit:

   ```bash
   ./release.sh X.Y.Z --dry-run   # every check and the builds, nothing changed
   ./release.sh X.Y.Z
   ```

   - Windows binaries, signed: `AGENT_VM_WINDOWS_PFX=<certificate and key, .pfx>` and
     `AGENT_VM_WINDOWS_PFX_PASS=<its password>`. Without, they go out unsigned, with a warning.
   - Homebrew: `AGENT_VM_TAP=<clone of the tap>` updates `Formula/agent-vm.rb` there (each
     platform's url and sha256) and prints the commands to commit it.
   - A run that tagged and then failed is resumed by running it again.
3. In a commit after it: `NEXT_VERSION` to the next version, and a new `## Unreleased` section.

## After it

- Check: `gh release view vX.Y.Z` shows it, and on a test machine
  `curl -fsSL https://www.agent-vm.org/install.sh | sh -s -- --version X.Y.Z` gives X.Y.Z.
- The Homebrew formula has one url and sha256 per platform (`on_macos`/`on_linux`,
  `on_arm`/`on_intel`), installing the tarball's `agent-vm`: that is the shape `AGENT_VM_TAP` updates.
  Then, in the tap: `brew audit --strict sylvinus/tap/agent-vm`, `brew reinstall` and `brew test`
  before pushing.
- `--not-latest` publishes a release without making it GitHub's latest: the installer's default
  stays the previous release, and the Homebrew formula is left alone, `AGENT_VM_TAP` or not.
