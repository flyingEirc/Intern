package physical

import "golang.org/x/sys/unix"

func checkReadonlyDataMount(directory string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(directory, &stat); err != nil {
		return preflightFailure("mount", "datadir_mount_unknown", "Cannot inspect datadir mount flags; verify the shared data volume.")
	}
	if stat.Flags&unix.ST_RDONLY == 0 {
		return preflightFailure("mount", "datadir_mount_writable", "Mount the source data volume read-only before enabling physical backups.")
	}
	return nil
}
