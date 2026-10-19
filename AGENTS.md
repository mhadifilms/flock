# Flock repository instructions

Flock is the public `mhadifilms/flock` fork of croc. Preserve the MIT license and
upstream attribution. Read README.md and UPSTREAM.md before changing transport
or command behavior.

## Scope and verification

Keep changes focused. Build the installed Flock command with `go build .` (or `./cmd/flock`). Run the relevant Go tests and
`go fix ./...` before publishing. For transport changes, exercise actual UDP
sockets and receiver verification. Do not claim a speedup without a comparable
measurement; distinguish LAN, overlays, simulation, and geographic WAN results.

Stdout from Flock must stay JSON Lines and commands must not prompt. Preserve
explicit failure when required QUIC is unavailable. Treat invitation capabilities
as secrets and keep private host details out of published fixtures and reports.

## Git and publication

Use conventional commits. Commit completed changes, push the feature branch to
`mhadifilms/flock`, and open a pull request explicitly targeting that fork's
`main` branch. Use Git for commits/pushes and `gh` for the pull request. Always
specify the fork repository when creating the PR so it is not opened upstream.
Do not merge unless requested.

The inherited upstream instructions referenced the original maintainer's local
`~/dotfiles/git-pr` helper, which is not provided by this fork. Flock uses the
normal Git/GitHub CLI workflow above.

## Releases

Flock releases use `packaging/flock_release.py`, which builds archives and
checksums without publishing. Keep `internal/flockcli.Release`, the release tag,
and Homebrew formula version aligned. Include LICENSE and dependency notices in
binary archives. Publish only a tested commit and verify downloaded assets and
package-manager installation before claiming a release is available.

The user's request for public package-manager installation authorizes the first
Flock release and the necessary PR merges. For future releases, obtain release
authorization. Never use the inherited upstream release/deployment scripts for
Flock; they have not been adapted for this project.
