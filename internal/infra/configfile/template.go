package configfile

// strictTemplate is the minimal config.toml awa init writes for the strict
// profile. The default profile writes no config at all — an absent file means
// "use the product defaults" — so the only thing a generated config needs to
// carry is the one override that defines the profile. The full field reference
// lives in the binary (`awa help config`), not in every initialized project.
const strictTemplate = `# awa project configuration (strict profile).
# Absent or empty config means product defaults; this file carries only overrides.
# Run 'awa help config' for the full configuration reference.

[hashing]
trust_mode = "strict"
`

// StrictTOML returns the config.toml bytes for the strict profile, which carries
// only the trust_mode override on top of the product defaults.
func StrictTOML() []byte {
	return []byte(strictTemplate)
}
