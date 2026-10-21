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
// UUID is @@server_uuid, the value preflight compares with auto.cnf.
// It is not the backup UUID recorded in xtrabackup_info.
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

// Info contains the versions and LSN interval recorded in xtrabackup_info.
// UUID is that backup's own identifier, produced by the tool with SELECT UUID().
// It is not @@server_uuid and must not be used to identify a server or volume.
// Fields preserves every recognized field, including the informational lock_time.
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

// Metadata retains the exact --extra-lsndir bytes beside their validated fields.
// CheckpointsRaw is xtrabackup_checkpoints. XtraBackup writes the streamed copy
// and this copy from the same xtrabackup_print_metadata values, so those bytes match.
// InfoRaw is xtrabackup_info from a later get_xtrabackup_info call. That call
// samples backup_size again from the stdout leaf after the streamed info chunk
// has been counted, so InfoRaw is authoritative and is not the streamed file's bytes.
type Metadata struct {
	CheckpointsRaw []byte
	InfoRaw        []byte
	Checkpoints    Checkpoints
	Info           Info
}
