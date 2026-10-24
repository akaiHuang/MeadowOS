# Answer presentation dependencies

These ESM browser bundles are copied unchanged from Meadow Search's
`public/vendor` so the phone and native frontend use the same Markdown parser
and sanitizer without a CDN or runtime package installation.

- Marked 18.0.14: MIT; see `marked-LICENSE`.
- DOMPurify 3.4.16: Apache-2.0 OR MPL-2.0; both license texts are included as
  `purify-LICENSE` and `purify-LICENSE-MPL`.

Copyright/license banners are retained in both bundles. No OpenCC/text
conversion dependency is included: model text and copied code keep their
original language. Both bundles expose standard ES modules for the existing
module-based native/phone frontend. DOMPurify requires a browser DOM; the
renderer falls back to literal text if sanitization is unavailable.

SHA-256 checksums (unmodified source bundles):

```
528a1b88bef88fc27277e06036ce7f4afb6a220b18110ba57c09e292b24a7ce0  marked.js
c44274a7959cfdd4da871fa78a5d5fbbef55db68d118c5c0833bc4b5cf9633ad  purify.js
8e3a3f82f59a60958f56ca08f445647c32a4733dc7ca6c2c46f6eb898471ab9c  marked-LICENSE
cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30  purify-LICENSE
fab3dd6bdab226f1c08630b1dd917e11fcb4ec5e1e020e2c16f83a0a13863e85  purify-LICENSE-MPL
```
