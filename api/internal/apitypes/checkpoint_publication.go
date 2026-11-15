package apitypes

// ReasonCheckpointPublicationWithheld is the fixed, server-controlled health reason for a running
// run whose worker reported that checkpoint publication is withheld by a secret-scan restriction
// (issue #4257). It names no finding, path, rule or secret content.
const ReasonCheckpointPublicationWithheld = "checkpoint publication is withheld by a secret-scan restriction; new work stays on the worker until it is resolved"

// HintCheckpointPublicationWithheld is the fixed, content-free operator hint that accompanies
// ReasonCheckpointPublicationWithheld on surfaces that render a health hint (issue #4257).
const HintCheckpointPublicationWithheld = "an eligible finding reaches the lead at its next work-turn boundary, subject to the existing safety checks and the shared attempt cap; the done-point fallback remains subject to its safety checks and the same cap; `uzi run follow-up` can give the agent owner guidance"
