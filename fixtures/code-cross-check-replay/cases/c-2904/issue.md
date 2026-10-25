# Bare-cache filenames exceed NAME_MAX for long local origins

The worker derives a bare-cache directory name from the clone origin. For a very long local origin path the name exceeds the filesystem NAME_MAX of 255 bytes and cloning fails.

Bound the name: when the derived name is longer than 255 UTF-8 bytes, fall back to a stable hash-based name, record the raw origin in the clone config, and reuse a hashed-name bare only when the recorded origin matches. Names within the limit must stay unchanged, and recovery discovery and the cross-check code stage must use the same rule.
