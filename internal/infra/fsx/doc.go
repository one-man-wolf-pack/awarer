// Package fsx holds small filesystem primitives shared across infrastructure
// adapters. Its no-follow opens refuse a symlink at the path so a store-owned
// regular file — a checkpoint, an alias, a blob, or a scanned input — cannot be read
// through a symlink swapped in between a shape check and the open, closing the race
// a separate check-then-open leaves.
package fsx
