package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const maintenancePageSize = 256
const maintenanceMaxPages = 16

// maintenancePages scans at most 4096 objects, including foreign objects. A read
// failure stops this observation; the caller's reconcile loop handles siblings.
func maintenancePages(ctx context.Context, resource string, read func(metav1.ListOptions) (int, string, error)) error {
	token := ""
	seen := map[string]bool{}
	for page := 0; page < maintenanceMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, next, err := read(metav1.ListOptions{Limit: maintenancePageSize, Continue: token})
		if err != nil {
			return fmt.Errorf("maintenance %s: %w", resource, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if count > maintenancePageSize {
			return fmt.Errorf("maintenance %s oversized page", resource)
		}
		if next == "" {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("maintenance %s repeated continuation", resource)
		}
		seen[next] = true
		token = next
	}
	return fmt.Errorf("maintenance %s list incomplete at page limit", resource)
}
