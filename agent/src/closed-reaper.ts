// Authority and the fixed pipeline live together: this module exposes no request
// builder, generic factory, validation callback, or caller-controlled spawn lane.
export {
  reaperCaptureScope,
  closedReaperDeadline,
  isClosedReaperRequest,
  assertClosedReaperRequest,
} from "./run-quiescence.js";
