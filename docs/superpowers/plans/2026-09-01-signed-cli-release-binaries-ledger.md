# Signed CLI Release Binaries SDD Ledger

Task 1: APPROVED — Add cross-platform ratecapctl binary build to publish-release.yml — commit beda94e
Task 2: APPROVED — Sign and attest the release binaries (cosign sign-blob --bundle on checksums.txt + attest-build-provenance over binaries + checksums.txt) — commit b4ecef6
Task 3: APPROVED — Documentation + version bump (README "Downloading a release"/"Verifying a release", CHANGELOG entry, VERSION 2.11.0 -> 2.12.0) — commit 2f0fe4e
