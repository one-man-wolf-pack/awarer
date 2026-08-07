package runstore

// MetaSchemaVersionForTest exposes the current run metadata schema version to the
// package's external tests, so a version assertion reads the constant the writer
// uses instead of restating the number and drifting from it.
const MetaSchemaVersionForTest = metaSchemaVersion
