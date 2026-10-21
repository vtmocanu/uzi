//go:build !go1.27

package workersvc

// Retain the module's Go floor without weakening semantic validation on an older
// local toolchain. Release and gate evidence uses the selected patched toolchain.
func decodeSemanticCompletionContract(raw []byte, dst *completionContract) error {
	return strictSemanticJSON(raw, dst)
}
