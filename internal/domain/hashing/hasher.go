package hashing

import "io"

// Hasher is the port the scanner uses to compute identities. It is defined here,
// on the consumer side, and implemented by an infrastructure adapter so the
// concrete BLAKE3 type never enters the domain. There is one local primitive, so
// the port exposes no algorithm choice and no way to report one.
//
// HashReader streams its input: a file is hashed through an io.Reader and must
// never be buffered whole, so a multi-gigabyte file costs constant memory.
// HashBytes digests an in-memory buffer and is used to fold a tree's canonical
// encoding into a TreeHash.
type Hasher interface {
	// HashReader streams r to completion and returns its content hash. It does
	// not close r.
	HashReader(r io.Reader) (ContentHash, error)
	// HashBytes returns the tree hash of b. It is total: a fixed hasher cannot
	// fail on an in-memory buffer.
	HashBytes(b []byte) TreeHash
}
