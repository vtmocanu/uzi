Synthetic plan (reconstructed for the replay corpus; not the historically approved plan)

1. Add a name function that returns the current name when it fits in 255 bytes and a sha256-based name otherwise.
2. Record the raw origin in the new clone config when the hashed name is used.
3. Verify the recorded origin before reusing a hashed-name bare in clone setup, recovery discovery and the cross-check code stage.
4. Resolve recorded recovery basenames by hashing them back to the name.
5. Keep existing in-limit names byte-for-byte unchanged.
6. Add tests for long and multibyte origins under a long TMPDIR and for the identity checks.
7. Add a CHANGELOG entry under Fixed.
