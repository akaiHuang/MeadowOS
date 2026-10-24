# Third-party notices

MeadowOS source is Apache-2.0 except where an accompanying notice states otherwise.
The source export retains original notices rather than replacing upstream authorship.

| Component | License / notice location |
| --- | --- |
| Selected Meadow Code / MeadowOS source | Root `LICENSE` and per-file headers |
| Raven embedded prompt provenance | `third_party/raven-code/README.md`, `LICENSE`, `NOTICES.md`, `LICENSES/` |
| Marked | MIT; `go/desktop/frontend/vendor/marked-LICENSE` |
| DOMPurify | Apache-2.0 OR MPL-2.0; `purify-LICENSE`, `purify-LICENSE-MPL` |
| Leaflet | BSD-2-Clause; `leaflet-LICENSE` |
| Lottie Web | MIT; `lottie-LICENSE` |
| xterm.js | MIT; `xterm-LICENSE` |
| Lucide / Feather artwork | `go/desktop/frontend/icons/nearby-lucide-LICENSE.txt` |
| Mapbox attribution asset | `go/desktop/frontend/vendor/nearby-maps-NOTICE.md` (trademark notice, separate from Leaflet license) |
| noVNC | System package MPL notices retained when the runtime client is prepared |
| Go module dependencies | Versions/checksums in `go/go.mod`, `go/go.sum`; upstream modules retain their licenses |
| GTK, Xfce, Thunar, WebKitGTK, fonts and Linux packages | Installed distribution packages retain their respective notices |

The Docker image copies project/Raven/frontend notices to
`/usr/local/share/meadowos-licenses/`. A downstream binary/image distributor should
retain notices for its resolved distribution and Go dependencies as well.
