// Package resourcevault provides a path-free container for proprietary prompt,
// policy, and capability resources.
//
// A vault index is signed with Ed25519. Each payload is encrypted independently
// with AES-256-GCM, and its authenticated data binds it to the signed bundle,
// bundle version, resource version, resource type, capability set, key id, and
// declared plaintext size. The wrapped AES data key is released only through a
// caller-supplied KeyUnwrapper. Production code must obtain that unwrap service
// from a platform keystore, isolated helper, entitlement service, or equivalent
// trust boundary; this package contains no private key or decryption key.
//
// The runtime API accepts readers rather than paths and never writes decrypted
// bytes to disk. Plaintext is held in a bounded, mutable buffer exposed only for
// the duration of Plaintext.Use. Callers must close the buffer promptly and must
// not retain the borrowed slice or convert it to an immutable string. Close and
// finalization perform best-effort zeroing, but Go cannot guarantee removal of
// copies made by callers, the runtime, crash reporters, or swapped memory.
package resourcevault
