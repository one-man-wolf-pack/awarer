package docbundle

import "testing"

// A bundle's content hashes are an EXTERNAL interoperability contract: a consumer
// verifies a published document against the manifest with an ordinary sha256sum,
// never by re-deriving it through this package. Every other test here compares a
// document's SHA256 against another value this same code produced, so all of them
// would still pass if the content hash silently became something else. These
// vectors are the independent oracle — they come from the SHA-256 reference suite,
// not from this code.
func TestDocumentSHA256KnownAnswers(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq", "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"},
	}
	for _, c := range cases {
		d := mustDoc(t, "known", "topics/known.md", KindTopic, nil, c.body)
		if got := d.SHA256(); got != c.want {
			t.Errorf("SHA256 of %q = %q, want %q", c.body, got, c.want)
		}
	}
}
