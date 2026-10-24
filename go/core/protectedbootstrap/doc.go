// Package protectedbootstrap assembles Meadow's existing protection
// boundaries into one fail-closed production startup lifecycle.
//
// It accepts readers and opaque identifiers, never filesystem paths or raw
// keys. Production preparation has no development or public fallback.
package protectedbootstrap
