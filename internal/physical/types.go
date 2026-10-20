// Package physical runs the native physical-backup tools independently of the CLI.
package physical

// Connection describes a local MySQL instance. Password must never be logged.
type Connection struct {
	Socket   string
	DataDir  string
	Username string
	Password string
}

// Identity is the identity queried from the live server.
type Identity struct {
	UUID           string
	Version        string
	VersionComment string
	DataDir        string
	PageSize       uint64
}

// ToolVersion preserves the exact version-probe output.
type ToolVersion struct {
	Version string
	Raw     string
}

// Checkpoints contains the native backup state and LSN interval.
type Checkpoints struct {
	BackupType string
	FromLSN    uint64
	ToLSN      uint64
	LastLSN    uint64
}

// Info contains the identity, versions and interval in xtrabackup_info.
// Fields preserves every recognized field without interpreting optional formats.
type Info struct {
	UUID          string
	ToolName      string
	ToolVersion   string
	ServerVersion string
	FromLSN       uint64
	ToLSN         uint64
	Incremental   bool
	Fields        map[string]string
}

// Metadata retains exact native bytes alongside their validated interpretation.
type Metadata struct {
	CheckpointsRaw []byte
	InfoRaw        []byte
	Checkpoints    Checkpoints
	Info           Info
}
