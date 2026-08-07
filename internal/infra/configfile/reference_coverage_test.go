package configfile

import (
	"reflect"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/configref"
)

// wireTags reflects the decoder's wire struct into a section -> {key: true} map. It
// is the authoritative list of implemented config keys, so the reference must match
// it exactly. Every field here is a real key: the wire struct carries no
// decode-only sentinels, so a key awa accepts and a key awa documents are the same
// set by construction. overlayWire's section fields are pointers
// (nil = absent), so it dereferences each to reach the key struct.
func wireTags(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	top := reflect.TypeOf(overlayWire{})
	for i := 0; i < top.NumField(); i++ {
		section := top.Field(i)
		sectionTag := section.Tag.Get("toml")
		if sectionTag == "" {
			t.Fatalf("overlayWire.%s has no toml tag", section.Name)
		}
		st := section.Type
		if st.Kind() == reflect.Pointer {
			st = st.Elem()
		}
		keys := map[string]bool{}
		for j := 0; j < st.NumField(); j++ {
			keyTag := st.Field(j).Tag.Get("toml")
			if keyTag == "" {
				continue
			}
			keys[keyTag] = true
		}
		out[sectionTag] = keys
	}
	return out
}

// TestReferenceCoversEveryImplementedKey is the synchronization guarantee: every
// key the decoder accepts appears exactly once in configref, and every reference
// key maps to a real decoder key. A field implemented but undocumented, or
// documented but unimplemented, fails here.
func TestReferenceCoversEveryImplementedKey(t *testing.T) {
	wire := wireTags(t)

	// Reference -> wire: every documented section/key must be a real decoder key,
	// and no key may be listed twice.
	refSections := map[string]bool{}
	for _, s := range configref.Sections() {
		if refSections[s.Name] {
			t.Errorf("reference lists section [%s] more than once", s.Name)
		}
		refSections[s.Name] = true
		keys, ok := wire[s.Name]
		if !ok {
			t.Errorf("reference documents unknown section [%s]", s.Name)
			continue
		}
		seen := map[string]bool{}
		for _, f := range s.Fields {
			if seen[f.Key] {
				t.Errorf("reference lists [%s].%s more than once", s.Name, f.Key)
			}
			seen[f.Key] = true
			if !keys[f.Key] {
				t.Errorf("reference documents [%s].%s, which the decoder does not accept", s.Name, f.Key)
			}
			if f.Default == "" {
				t.Errorf("reference [%s].%s has an empty default", s.Name, f.Key)
			}
			if f.Type == "" || f.Summary == "" || f.WhenToChange == "" {
				t.Errorf("reference [%s].%s is missing type/summary/when-to-change", s.Name, f.Key)
			}
		}
	}

	// Wire -> reference: every implemented section/key must be documented.
	for section, keys := range wire {
		refKeys := map[string]bool{}
		for _, s := range configref.Sections() {
			if s.Name == section {
				for _, f := range s.Fields {
					refKeys[f.Key] = true
				}
			}
		}
		if !refSections[section] {
			t.Errorf("implemented section [%s] is missing from the reference", section)
			continue
		}
		for key := range keys {
			if !refKeys[key] {
				t.Errorf("implemented key [%s].%s is missing from the reference", section, key)
			}
		}
	}
}

// TestConfigKeysMatchWireTags pins the domain-owned closed key set (config.Keys,
// which the resolved-config constructor validates provenance against) to the
// decoder's wire tags. A key the decoder accepts but config.Keys omits — or a
// config.Keys entry the decoder does not accept — fails here, so the two namespaces
// cannot drift. config.Keys must also list each key exactly once.
func TestConfigKeysMatchWireTags(t *testing.T) {
	wire := map[string]bool{}
	for section, keys := range wireTags(t) {
		for key := range keys {
			wire[section+"."+key] = true
		}
	}

	seen := map[string]bool{}
	for _, k := range config.Keys() {
		if seen[k] {
			t.Errorf("config.Keys lists %q more than once", k)
		}
		seen[k] = true
		if !wire[k] {
			t.Errorf("config.Keys lists %q, which the decoder does not accept", k)
		}
	}
	for k := range wire {
		if !seen[k] {
			t.Errorf("decoder accepts %q, which config.Keys omits", k)
		}
	}
}

// TestTemplateAndHelpMentionEveryKey guards that both rendered surfaces list every
// reference key, so neither the template nor the canonical Markdown reference —
// the bytes `awa help config` prints and `awa docs export` publishes — can
// silently omit a documented setting.
func TestTemplateAndHelpMentionEveryKey(t *testing.T) {
	tmpl := configref.Template()
	help := configref.MarkdownReference()
	for _, s := range configref.Sections() {
		if !strings.Contains(tmpl, "["+s.Name+"]") {
			t.Errorf("template omits section [%s]", s.Name)
		}
		if !strings.Contains(help, "["+s.Name+"]") {
			t.Errorf("help config omits section [%s]", s.Name)
		}
		for _, f := range s.Fields {
			if !strings.Contains(tmpl, f.Key) {
				t.Errorf("template omits key [%s].%s", s.Name, f.Key)
			}
			if !strings.Contains(help, f.Key) {
				t.Errorf("help config omits key [%s].%s", s.Name, f.Key)
			}
		}
	}
}
