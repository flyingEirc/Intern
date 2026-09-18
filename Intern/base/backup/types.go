// Package backup implements the shared MySQL logical backup and restore use cases.
package backup

import "time"

// Connection credentials never enter backup metadata or logs.
type Connection struct {
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	Socket      string `mapstructure:"socket"`
	Username    string `mapstructure:"username"`
	Password    string `mapstructure:"password" json:"-"`
	PasswordEnv string `mapstructure:"password_env"`
	DB          string `mapstructure:"db"`
	TLSMode     string `mapstructure:"tls_mode"`
	TLSCA       string `mapstructure:"tls_ca"`
}

type Options struct {
	Connection      Connection
	Directory       string
	StateDirectory  string
	Project         string
	Compress        bool
	Encrypt         bool
	KeyFile         string
	MaxRestoreBytes int64
}

type Coordinate struct {
	File     string `json:"file"`
	Position int64  `json:"position"`
}

type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version       int        `json:"version"`
	ID            string     `json:"id"`
	Project       string     `json:"project"`
	Kind          string     `json:"kind"`
	Parent        string     `json:"parent"`
	SourceUUID    string     `json:"source_uuid"`
	Database      string     `json:"database"`
	Charset       string     `json:"charset"`
	Collation     string     `json:"collation"`
	ServerVersion string     `json:"server_version"`
	ToolVersion   string     `json:"tool_version"`
	GTIDMode      string     `json:"gtid_mode"`
	ObservedGTID  string     `json:"observed_gtid"`
	Encrypted     bool       `json:"encrypted"`
	KeyID         string     `json:"key_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	Start         Coordinate `json:"start"`
	End           Coordinate `json:"end"`
	Files         []File     `json:"files"`
	SchemaObjects bool       `json:"schema_objects"`
}

type Result struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Directory string     `json:"directory"`
	Parent    string     `json:"parent"`
	CreatedAt time.Time  `json:"created_at"`
	End       Coordinate `json:"end"`
}

type RestoreRequest struct {
	BackupID string
	Target   Connection
	Execute  bool
}

type RestorePlan struct {
	BackupID       string     `json:"backup_id"`
	SourceUUID     string     `json:"source_uuid"`
	SourceDatabase string     `json:"source_database"`
	TargetUUID     string     `json:"target_uuid"`
	TargetDatabase string     `json:"target_database"`
	Artifacts      []string   `json:"artifacts"`
	Stop           Coordinate `json:"stop"`
	ExpandedBytes  int64      `json:"expanded_bytes"`
	Executed       bool       `json:"executed"`
	Warning        string     `json:"warning"`
}
