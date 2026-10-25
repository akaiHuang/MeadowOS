# Security

## Report a vulnerability

Please use [private vulnerability reporting](https://github.com/akaiHuang/MeadowOS/security/advisories/new)
for security issues. Include steps to reproduce, the affected version and the
expected behavior. Keep API keys, personal files and private conversations out
of reports and screenshots.

## Local desktop access

Meadow can edit files and run programs in its desktop environment. The default
Docker setup runs as a non-root user and makes the desktop available only on
local ports. It does not mount your host home or Docker socket.

Review access carefully before adding host mounts or enabling remote access.
The Alpha Docker setup is intended for local use, not multi-user hosting.
