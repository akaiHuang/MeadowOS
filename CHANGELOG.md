# Changelog

## 0.1.0-alpha.1 — 2026-10-25

First standalone source release of the native MeadowOS container desktop.

- Glass desktop bar/Dock and shared native window/folder resources.
- Addable/closable desktop tabs with window preservation.
- Compact floating chat composer, durable conversations and background work records.
- Native Agent, external provider integration and secure Inception Mercury3 onboarding.
- Dedicated non-root Docker recipe with persistent home and loopback-only viewing.
- Fresh-home/restart native smoke checks, source provenance and redacted history scanning.

Known release boundaries: no bootable ISO, Wayland acceptance, GPU performance
claim or complete Linux speech integration. Docker hosts can block WebKit's
nested sandbox, making GNOME Web / optional Wails Desktop unavailable; the
native GTK chat and API setup remain independent. Mercury 3 is text/tools-only;
image recognition is explicitly unavailable in the default configuration.
