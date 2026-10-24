# Security and private configuration

Please do not include API keys, private files, conversation histories or runtime
profiles in public issues, pull requests, screenshots or diagnostic logs. Use
synthetic reproducers and redact account-specific values.

The quick start does not ship a maintainer key. Users enter their own Inception
key at a hidden terminal prompt. Native credential storage is outside the source
checkout, in the persistent home volume. Build context rules exclude Git metadata,
credentials and runtime state. Stopping containers retains the named volume;
`docker compose down -v` deliberately removes it.

The standalone release starts with fresh Git history and an explicit source
closure. It does not import the private development repository's history or
runtime profiles. CI scans the entire public history for secrets with redacted
output. Please keep the same boundary when contributing changes or screenshots.

This early desktop grants the Agent full tool access inside its container. The
default Compose file mounts neither your host home nor the Docker socket and
publishes the viewer/companion on host loopback only. Keep those boundaries when
customizing deployment. Public remote access/authenticated multi-user hosting is
not provided by this local container recipe.

For a suspected vulnerability, use GitHub's private vulnerability reporting when
available on this repository. Public reports should contain only non-sensitive
reproduction details. This experimental release has no formal support SLA.
