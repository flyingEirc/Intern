package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"regexp"
)

var artifactID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func sourceKey(project string, info sourceInfo, db string) string {
	return project + ":" + info.UUID + ":" + db
}
func requestID(source, kind, request string) string {
	if request == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		return hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(source + "\x00" + kind + "\x00" + request))
	return hex.EncodeToString(sum[:16])
}
func result(m Manifest, path string) Result {
	return Result{ID: m.ID, Kind: m.Kind, Directory: path, Parent: m.Parent, CreatedAt: m.CreatedAt, End: m.End}
}
func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, errors.New("invalid backup manifest")
	}
	if err := validateManifestIdentity(m); err != nil {
		return m, err
	}
	if !databaseName.MatchString(m.Charset) || !databaseName.MatchString(m.Collation) {
		return m, errors.New("missing or invalid database charset/collation")
	}
	if !validCoordinate(m.Start) || !validCoordinate(m.End) {
		return m, errors.New("invalid backup binlog coordinates")
	}
	return m, validateManifestKind(m)
}
func validateManifestIdentity(m Manifest) error {
	if m.Version != 1 || !artifactID.MatchString(m.ID) || m.Project == "" || m.SourceUUID == "" || !databaseName.MatchString(m.Database) || len(m.Files) == 0 {
		return errors.New("unsupported or incomplete backup manifest")
	}
	return nil
}
func validCoordinate(c Coordinate) bool { return logName.MatchString(c.File) && c.Position >= 4 }
func validateManifestKind(m Manifest) error {
	switch m.Kind {
	case "logical-full":
		return validateFullManifest(m)
	case "binlog":
		if !artifactID.MatchString(m.Parent) {
			return errors.New("incremental backup has no valid parent")
		}
	default:
		return errors.New("unsupported or incomplete backup manifest")
	}
	return nil
}
func validateFullManifest(m Manifest) error {
	if m.Parent != "" || len(m.Files) != 1 || m.Files[0].Name != "dump.sql" || m.Start != m.End {
		return errors.New("invalid logical baseline manifest")
	}
	return nil
}
