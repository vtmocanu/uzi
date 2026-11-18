package apitypes

import "testing"

func TestRunEnvDTOTags(t *testing.T) {
	assertTags(t, "RunEnvVarDTO", RunEnvVarDTO{},
		"var_id", "name", "secret", "description", "value", "value_withheld", "state", "rev", "disabled_at", "created_at", "updated_at")
	assertTags(t, "RunEnvCountsDTO", RunEnvCountsDTO{}, "total", "disabled", "pending_deletion", "limit")
	assertTags(t, "RunEnvListDTO", RunEnvListDTO{}, "variables", "counts", "vault_locked")
	assertTags(t, "RunEnvWriteDTO", RunEnvWriteDTO{}, "variable")
	assertTags(t, "RunEnvDeleteDTO", RunEnvDeleteDTO{}, "state")
}
