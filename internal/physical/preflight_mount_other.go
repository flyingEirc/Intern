//go:build !linux

package physical

func checkReadonlyDataMount(string) error {
	return preflightFailure("mount", "platform_unsupported", "Physical source mount admission requires Linux.")
}
