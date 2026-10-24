# ASCII onboarding and two app themes — design QA

## Login revision, 2026-09-07 20:15

Local frontend checks passed; native installation and real Google OAuth remain blocked/unverified.
User's 20:10 revision supersedes the illustration-dominant login: logo and large
left headline restored, illustration secondary, right side simplified to welcome,
Google/Email choices and serial entry. Initial idle status and repeated explanation removed.
Reference and rendered light screenshot inspected together; different outer viewport
sizes mean composition comparison, not pixel equivalence. Original left green panel
is intentionally replaced by the approved welcome cream background and ASCII scene.

- Typography: welcome sans/mono hierarchy; fixed awkward left headline wrapping
  with a deliberate break after the comma; enlarged body and button labels.
- Spacing: preserved left brand/right entry grouping, stable bottom theme/pause strip.
  Welcome is 1116x612 at 1280x720, exactly 90% of the old 1240x680, top54px.
- Colors: dark neutral/green and cream/chartreuse/pink; light kicker now dark for contrast.
- Imagery: generated garden entry sampled into actual glyph animation. Pause,
  reduced motion, theme identity and destroy are tested, no moving raster substitute.
- Copy and behavior: concise Chinese, no initial fake progress; existing device
  authorization protocol preserved. Both Google/Email entries open the same server
  verification URL, then the user selects the provider on the existing membership
  website. This is NOT direct Google provider routing or a verified OAuth login.

24/24 focused tests passed before final font-size/line-break-only adjustment;
git diff --check passed after it. Two broader commercial capability contract tests
remain unresolved; no claim that the entire auth suite passes.
Evidence: docs/desktop/design/login-ascii-20260907/evidence/login-{dark,light}.png.
These captures precede the final font-size/line-break-only adjustment.
Diagnostic build20260907.2010 completed but was deliberately not installed after
the user's intervening design revision. Installed Desktop remains20260907.1935.
Latest revision is the local preview/source, not the2010 bundle.

2026-09-07. final result: passed (local frontend scope)

## Target and evidence

User approved `docs/desktop/design/onboarding-ascii-20260907/01-welcome-approved.png`
and `03-preview-proposal.png`, supplied the laptop-to-file image, then explicitly
requested actual ASCII animation and dark/light versions of all four pages plus
an app-wide light theme. Fourth page subject was replaced per latest instruction:
`assets/ascii-onboarding/local-dark-v2.png`, with models/files inside the laptop.
The reference PNG is sampled once; visible canvas uses only fillText, not drawImage.
The user's explicit ASCII request overrides the generic image-to-code glyph prohibition.

Compared reference and rendered frames together in image inspection inputs, not
claimed as a stitched side-by-side. Welcome source 1487x1058, implementation1488x1056
CSS pixels at DPR1: negligible outer-size difference noted; compare matching content
regions. Fourth standalone source is square and fitted within the selected left art
column, not compared as a full UI screenshot. Light is a user-requested palette
variant, so dark-to-cream change is intentional, not pixel-matching failure.
Evidence: `docs/desktop/design/onboarding-ascii-20260907/evidence/` (repo-root relative).
Eight themed frames, shell, settings, login and model-picker images.
Main agent also used IAB live tour and full shell at ports18117/18099; console errors/warnings empty.

## Findings and fixes

- P2 fixed: earlier glyph grid stretched artwork and underexposed device outlines.
  Square132x132 grid, isotropic fit and stronger glyph opacity; post-fix captures
  retain recognizable laptop/window silhouettes and the selected61/39 composition.
- P2 fixed: preview reduced-motion froze in the dim initial scan state. Paused
  rendering now exposes the full content; moving scan minimum opacity is .65.
  Recaptured preview-dark/light and reran three lifecycle/theme checks after fix.
- P2 fixed: portrait733px browser panel had overly small side-by-side art. At
  <=850px portrait (or <=660px) art and copy stack, keeping bottom navigation visible.
- P2 fixed: settings/account/model submenu/search had literal dark colors bypassing
  root tokens. Scoped light overrides cover these; existing/new xterm instances get
  their actual light palette through the theme event, not CSS inversion.
- Review fix: keep empty Send neutral, armed Send lime, and explicit Guide warning
  colored. Stop remains a separate control with its original destructive semantics.

## Required fidelity surfaces

- Typography: real DOM Chinese copy, system/PingFang fallback, monospace eyebrow;
  desktop headline wraps as the approved two-line composition, body text readable.
  Native font optical weight differs slightly from generated text (P3, expected).
- Spacing: selected two-column frame, fixed footer and interactive controls retained.
  Added36px pause/theme utility strip is intentional accessible product chrome.
- Colors: all four default dark; all four cream in light mode. Chartreuse CTA,
  pink accents, charcoal text. Dark olive replaces bright lime for small readable
  app text. Tests check actual computed shell/settings luminance and contrast.
- Imagery: original subjects preserved for1–3, regenerated local-work subject for4.
  Character contours necessarily differ from raster reference; no moving full PNG.
  Media and embedded user webpages are deliberately not recolored.
- Copy: page4 says local models/files and explicitly discloses network needs for
  login, licensing and web search. No fabricated VERIFIED or capability guarantee.

## Verification and boundaries

- Final focused browser suite24/24 PASS in20.8s after all source edits
  (onboarding, ASCII/theme, settings).
- Latest paused-preview fix:3/3 targeted PASS, refreshed all eight theme captures.
- Real xterm constructor/palette tested with PTY endpoints intercepted; no user
  terminals or model inference started. Theme persistence, all-four same Canvas,
  no duplicate active RAF, pause/reduce-motion/visibility/destroy tested.
- `go test ./desktop -run '^$' -count=1` passes compile/link. Existing linker warnings
  about newer macOS objects and duplicate objc remain; this is not installed QA.
- `git diff --check` passes. No backend/auth/model behavior changes.
- Installed app has NOT been replaced. Native Wails display remains a release gate.
  NSWindow's initial pre-HTML background is still dark; HTML app-owned chrome follows
  the selected theme. OS-owned dialogs follow macOS appearance, not this preference.

## Follow-up polish

P3: raster-to-character conversion is less dense at very narrow widths; evaluate
native Retina appearance and startup background when packaging the approved build.
