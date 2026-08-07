package hashing

// ConfigHash is the identity of an effective configuration: the hex digest of the
// config's canonical encoding. It is a distinct value type so a scan's recorded
// config hash cannot be assigned a tree or content hash by mistake, and so it can
// never be constructed from an arbitrary string — only from a real digest or by
// parsing a validated "blake3:hex" form.
type ConfigHash struct {
	hex string
}

// ConfigHashFromTree adapts a digest produced by Hasher.HashBytes (a TreeHash, the
// port's general byte-hashing result) into a ConfigHash. Both share the validated
// "blake3:hex" representation; this keeps the config hash a first-class value
// without adding a third hashing method to the port.
func ConfigHashFromTree(th TreeHash) ConfigHash {
	return ConfigHash(th)
}

// ParseConfigHash parses a persisted "blake3:hex" config hash.
func ParseConfigHash(s string) (ConfigHash, error) {
	hex, err := parseDigest("config hash", s)
	if err != nil {
		return ConfigHash{}, err
	}
	return ConfigHash{hex: hex}, nil
}

// IsZero reports whether c is the zero value — no config hash recorded.
func (c ConfigHash) IsZero() bool { return c.hex == "" }

// String renders the canonical "blake3:hex" form; the zero value renders as the
// empty string.
func (c ConfigHash) String() string {
	if c.IsZero() {
		return ""
	}
	return formatDigest(c.hex)
}
