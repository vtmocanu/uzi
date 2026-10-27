# Checkpoint publication reads successful response bodies without a byte limit

publishCheckpoint reads every successful acknowledgment from the server with an unbounded res.text(), so an oversized or endless body is buffered whole.

Read the acknowledgment through the existing bounded reader with a 64 KiB cap that rejects on overflow, using the combined caller and request abort signal. An oversized body, a read failure or an abort must surface to the caller as an ambiguous best-effort publish failure. No part of the response body may be logged or echoed. Add regression tests for the cap boundary, multibyte text and aborts.
