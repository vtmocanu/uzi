package main

import "github.com/vtmocanu/uzi/api/internal/apitypes"

func workerRollFailure(status string) bool {
	return status == "upgrade_failed" || status == "provisioning_failed" || status == "capacity_blocked"
}

func workerRollLabel(status string) string {
	switch status {
	case "capacity_blocked":
		return "capacity blocked"
	case "provisioning_failed":
		return "provisioning failed"
	case "provisioning":
		return "provisioning"
	default:
		return "upgrade failed"
	}
}

func workerCapacityRemedy(w apitypes.WorkerDTO) string {
	if w.UpgradeStatus != "capacity_blocked" {
		return ""
	}
	return "wait for available capacity, choose a smaller worker, or ask an operator to add capacity"
}
