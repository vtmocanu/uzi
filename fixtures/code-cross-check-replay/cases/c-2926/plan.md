Synthetic plan (reconstructed for the replay corpus; not the historically approved plan)

1. Add a named 64 KiB cap constant for the checkpoint publication acknowledgment.
2. Build one combined abort signal (caller boundary plus request timeout) before the request and reuse it for the body read.
3. Replace the unbounded text read in publishCheckpoint with the bounded reader, rejecting on overflow.
4. Keep the empty-2xx and non-2xx handling unchanged.
5. Update the doc comment to describe the new throwing behavior.
6. Add tests: exact-cap acceptance, one byte over, multibyte decoding across chunks, mid-read stream error, boundary abort.
7. Extend the runner retry test so an oversized acknowledgment is recorded as ambiguous.
8. Add a CHANGELOG entry under Fixed.
