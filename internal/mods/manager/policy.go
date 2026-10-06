package manager

// InstallCancelPolicy stops cancellation before file placement begins.
func InstallCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case CheckpointBackedUp, CheckpointManifestWritten, CheckpointApplied:
		return false, "placing files into the server directory"
	default:
		return true, ""
	}
}

// UninstallCancelPolicy protects a partially removed package.
func UninstallCancelPolicy(string) (cancellable bool, phase string) {
	return false, "removing files from the server directory"
}

// ToggleCancelPolicy protects the package's file moves.
func ToggleCancelPolicy(string) (cancellable bool, phase string) {
	return false, "moving the mod's files"
}
