package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// validateScheduleRemoval uses the effective selector and live eligibility label.
// False and omitted flags permit the existing schedule configurations unchanged.
func (h *Handler) validateScheduleRemoval(ctx context.Context, req apitypes.ScheduleRequest, selector string) (int, string) {
	if req.RemoveLabelOnDispatch == nil || !*req.RemoveLabelOnDispatch {
		return 0, ""
	}
	if selector == "" {
		selector = schedtmpl.SelectorLabel
	}
	if req.Target != "sweep" || req.Timing != "recurring" || selector != schedtmpl.SelectorLabel {
		return http.StatusBadRequest, "remove_label_on_dispatch requires a recurring label-selected sweep"
	}
	labels := make([]string, 0, len(req.Labels))
	for _, label := range req.Labels {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) != 1 {
		return http.StatusBadRequest, "remove_label_on_dispatch requires exactly one selector label"
	}
	uzi := settings.DefaultUziLabel
	if h.settings != nil {
		live, _ := h.settings.UziLabel(ctx)
		if strings.TrimSpace(live) != "" {
			uzi = live
		}
	}
	if labels[0] == uzi {
		return http.StatusBadRequest, "remove_label_on_dispatch cannot remove the configured uzi label"
	}
	return 0, ""
}
